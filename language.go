package wasitter

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
)

// LANGUAGE_VERSION is the newest Tree-sitter language ABI understood by the
// bundled runtime.  It mirrors TREE_SITTER_LANGUAGE_VERSION from the C API
// and is intentionally an untyped constant so it can be compared with either
// uint32 ABI values or integer literals.
const LANGUAGE_VERSION = 15

// MIN_COMPATIBLE_LANGUAGE_VERSION is the oldest Tree-sitter language ABI that
// can be loaded by the bundled runtime.
const MIN_COMPATIBLE_LANGUAGE_VERSION = 13

// Go-style aliases for callers that prefer mixed-case identifiers.
const (
	LanguageVersion              = LANGUAGE_VERSION
	MinCompatibleLanguageVersion = MIN_COMPATIBLE_LANGUAGE_VERSION
)

// SymbolType mirrors Tree-sitter's grammar symbol classification.
type SymbolType uint8

const (
	// SymbolTypeRegular identifies a visible, named grammar symbol.
	SymbolTypeRegular SymbolType = iota
	// SymbolTypeAnonymous identifies a visible, anonymous grammar symbol.
	SymbolTypeAnonymous
	// SymbolTypeSupertype identifies an invisible grammar supertype.
	SymbolTypeSupertype
	// SymbolTypeAuxiliary identifies an invisible auxiliary symbol.
	SymbolTypeAuxiliary
)

// LanguageMetadata contains optional semantic-version metadata emitted by a
// grammar generator. Older shim modules may not expose it and return nil.
type LanguageMetadata struct {
	MajorVersion uint8
	MinorVersion uint8
	PatchVersion uint8
}

// Language identifies a grammar exported by a Runtime. A language is usually
// statically embedded in the guest module, so closing it does not free a guest
// allocation; Runtime.Close releases the module itself.
type Language struct {
	runtime *Runtime
	handle  uint32
	export  string
	closed  atomic.Bool
}

// clone returns an independent Go wrapper for the same guest grammar.  A
// TSLanguage is immutable and owned by Runtime; consequently wrappers must
// not share lifecycle state.  In particular, closing a temporary Language
// value obtained from Parser.Language (or the value passed to SetLanguage)
// must not invalidate a parser, query, or tree that merely references the
// same grammar.
func (l *Language) clone() *Language {
	if l == nil {
		return nil
	}
	return &Language{runtime: l.runtime, handle: l.handle, export: l.export}
}

// NewLanguage resolves a language export in rt. With no export name it tries
// the canonical tsw_language/wasitter_language exports. A grammar-specific
// name such as "json" is expanded to tree_sitter_json when appropriate.
func NewLanguage(rt *Runtime, export ...string) (*Language, error) {
	if rt == nil {
		return nil, ErrNoRuntime
	}
	if len(export) > 1 {
		return nil, fmt.Errorf("wasitter: expected at most one language export, got %d", len(export))
	}
	name := ""
	if len(export) != 0 {
		name = export[0]
	}
	return rt.LoadLanguage(name)
}

// LoadLanguage resolves an exported TSLanguage. The name may be an exact WASM
// export (for example tree_sitter_json) or a grammar name (json).
func (r *Runtime) LoadLanguage(name string) (*Language, error) {
	if err := r.ensureOpen(); err != nil {
		return nil, err
	}
	aliases := languageAliases(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Resolve aliases by signature.  Generated grammars commonly expose a
	// zero-argument constructor returning one wasm pointer, while compatibility
	// modules may also carry helper exports with a colliding name.  Selecting a
	// colliding function and invoking it with the wrong arity would produce a
	// wasm trap and, more importantly, prevent a later valid alias from being
	// considered.
	fn, resolved, err := lookupIntegerFunctionLocked(r, aliases, 0, 1, true, true)
	if err != nil {
		// Generated grammars commonly export only their canonical
		// `tree_sitter_<name>` constructor and do not include the generic
		// wasitter bridge alias.  When the caller omitted a name, discover one
		// such constructor from the module's export table.  Keep this fallback
		// deterministic (sorted names), and validate the signature before calling
		// so unrelated exports cannot trigger a WASM arity/type trap.
		if strings.TrimSpace(name) == "" && isUnsupported(err) {
			if discoveredFn, discoveredName, discoveredErr := discoverLanguageExportLocked(r); discoveredErr == nil {
				fn, resolved = discoveredFn, discoveredName
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	// lookupIntegerFunctionLocked has already checked the arity and integer
	// result shape. Keep the explicit guard for defensive clarity if this code
	// is later changed to accept another lookup path.
	if params := len(fn.Definition().ParamTypes()); params != 0 {
		return nil, &ABIError{Function: resolved, Message: fmt.Sprintf("language export has unsupported signature (%d parameters)", params)}
	}
	result, callErr := fn.Call(r.ctx)
	if callErr != nil {
		return nil, &ABIError{Function: resolved, Message: callErr.Error()}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("wasitter: language export %q returned a null handle", resolved)
	}
	handle, ok := checkedU32(result[0])
	if !ok || handle == 0 {
		message := "returned a null language handle"
		if !ok {
			message = "returned a non-wasm32 language handle"
		}
		return nil, &ABIError{Function: resolved, Message: message}
	}
	return &Language{runtime: r, handle: handle, export: resolved}, nil
}

// discoverLanguageExportLocked finds a canonical generated-grammar export.
// The caller must hold r.mu.  Only zero-argument functions returning one
// wasm integer are considered; this keeps discovery side-effect free for the
// usual scanner/helper exports that happen to share the tree_sitter_ prefix.
func discoverLanguageExportLocked(r *Runtime) (api.Function, string, error) {
	if r == nil || r.mod == nil {
		return nil, "", ErrNoRuntime
	}
	if r.closedState() {
		return nil, "", ErrClosed
	}
	definitions := r.mod.ExportedFunctionDefinitions()
	names := make([]string, 0, len(definitions))
	for name, definition := range definitions {
		if !strings.HasPrefix(name, "tree_sitter_") || name == "tree_sitter_language" {
			continue
		}
		if len(definition.ParamTypes()) != 0 || len(definition.ResultTypes()) != 1 {
			continue
		}
		resultType := definition.ResultTypes()[0]
		if resultType != api.ValueTypeI32 && resultType != api.ValueTypeI64 {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn := r.mod.ExportedFunction(name)
		if fn == nil {
			continue
		}
		r.funcs[name] = fn
		return fn, name, nil
	}
	return nil, "", fmt.Errorf("%w: no zero-argument tree_sitter_* language export", ErrUnsupported)
}

// Language is a convenient alias for LoadLanguage. With no name it resolves
// the module's default grammar.
func (r *Runtime) Language(name ...string) (*Language, error) {
	if len(name) > 1 {
		return nil, fmt.Errorf("wasitter: expected at most one language name, got %d", len(name))
	}
	if len(name) == 0 {
		return r.LoadLanguage("")
	}
	return r.LoadLanguage(name[0])
}

func languageAliases(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return []string{"tsw_language", "sw_language", "st_language", "wasitter_language", "language", "tree_sitter_language"}
	}
	seen := make(map[string]struct{}, 8)
	result := make([]string, 0, 6)
	add := func(s string) {
		if s != "" {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				result = append(result, s)
			}
		}
	}
	add(name)
	// Prefer a grammar-specific constructor when the caller supplied a friendly
	// name. A module can legitimately contain more than one generated grammar
	// (for example tree_sitter_json and tree_sitter_javascript) in addition to
	// a generic bridge alias. Resolving the generic alias first would silently
	// return whichever grammar the bridge chose as its default, making
	// LoadLanguage("json") depend on export ordering rather than the requested
	// name.
	if !strings.HasPrefix(name, "tree_sitter_") {
		add("tree_sitter_" + name)
	}
	if !strings.HasPrefix(name, "tsw_") {
		add("tsw_" + name)
	}
	if !strings.HasPrefix(name, "sw_") {
		add("sw_" + name)
	}
	if !strings.HasPrefix(name, "st_") {
		add("st_" + name)
	}
	if !strings.HasPrefix(name, "wasitter_") {
		add("wasitter_" + name)
	}
	// A module produced by the bundled build script has one statically linked
	// grammar and exposes the generic tsw_language entry point. Keep these
	// aliases after the name-specific forms so they remain an ergonomic
	// fallback without shadowing an exact constructor.
	add("tsw_language")
	add("sw_language")
	add("st_language")
	add("wasitter_language")
	// A module normally contains one statically linked grammar and exposes it
	// through tsw_language regardless of the human-readable name requested by
	// the caller.
	add("tsw_language")
	add("wasitter_language")
	add("language")
	return result
}

func (l *Language) ensureOpen() error {
	if l == nil || l.closed.Load() || l.runtime == nil {
		return ErrClosed
	}
	return l.runtime.ensureOpen()
}

// Handle returns the opaque guest handle. It is intended for ABI extensions
// and returns zero once this wrapper or its owning runtime has closed.
func (l *Language) Handle() uint32 {
	if l == nil || l.closed.Load() || l.runtime == nil || l.runtime.closedState() {
		return 0
	}
	handle := l.handle
	if l.closed.Load() || l.runtime.closedState() {
		return 0
	}
	return handle
}

// ExportName returns the export used to resolve this language.
func (l *Language) ExportName() string {
	if l == nil {
		return ""
	}
	return l.export
}

// Valid reports whether this language handle is still usable.
func (l *Language) Valid() bool { return l != nil && l.handle != 0 && l.ensureOpen() == nil }

// IsValid is an alias for Valid.
func (l *Language) IsValid() bool { return l.Valid() }

// Equal compares two language handles. Language values obtained from a tree,
// parser, and runtime may be distinct Go wrappers while referring to the same
// guest grammar, so comparison is based on runtime identity and handle.
func (l *Language) Equal(other *Language) bool {
	if l == nil || other == nil || l.handle == 0 || other.handle == 0 {
		return false
	}
	return l.runtime == other.runtime && l.handle == other.handle && l.Valid() && other.Valid()
}

// Equals is an alias for Equal.
func (l *Language) Equals(other *Language) bool { return l.Equal(other) }

// String implements fmt.Stringer and returns the grammar name when available.
func (l *Language) String() string { return l.Name() }

// Close marks a language wrapper closed. The underlying grammar is owned by
// the WASM module and is released when Runtime.Close is called.
func (l *Language) Close() error {
	if l != nil {
		l.closed.Store(true)
	}
	return nil
}

// Name returns the grammar name, or an empty string when the module does not
// expose language metadata.
func (l *Language) Name() string {
	name, _ := l.NameE()
	return name
}

// NameE is the error-returning form of Name.
func (l *Language) NameE() (string, error) {
	if err := l.ensureOpen(); err != nil {
		return "", err
	}
	// The canonical shim takes a language handle.  A few early modules expose
	// a module-wide, zero-argument name pointer instead; inspect the function
	// arity before calling so both forms remain safe.
	return l.callLanguageString([]string{
		"tsw_language_name",
		"wasitter_language_name",
		"language_name",
		"ts_language_name",
		"tsw_language_name_ptr",
		"wasitter_language_name_ptr",
		"language_name_ptr",
	})
}

// ABIVersion returns the grammar ABI version. A zero value means metadata is
// unavailable.
func (l *Language) ABIVersion() uint32 {
	v, _ := l.ABIVersionE()
	return v
}

// AbiVersion is an upstream-compatible spelling.
func (l *Language) AbiVersion() uint32 { return l.ABIVersion() }

// ABIVersionE is the error-returning form of ABIVersion.
func (l *Language) ABIVersionE() (uint32, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	r := l.runtime
	// Canonical tsw_language_abi_version is zero-argument; some early shims
	// accepted a language handle. Inspect the export signature before calling.
	r.mu.Lock()
	fn, name, err := lookupIntegerFunctionAllowedLocked(r, []string{
		"tsw_language_abi_version_for",
		"wasitter_language_abi_version_for",
		"language_abi_version_for",
		"ts_language_abi_version_for",
		"tsw_language_abi_version",
		"wasitter_language_abi_version",
		"language_abi_version",
		"ts_language_abi_version",
	}, []int{0, 1}, 1, true, true)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	params := len(fn.Definition().ParamTypes())
	args := []uint64(nil)
	if params != 0 {
		args = []uint64{uint64(l.handle)}
	}
	result, callErr := fn.Call(r.Context(), args...)
	r.mu.Unlock()
	if callErr != nil {
		err = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU32(result[0])
	if !ok {
		return 0, &ABIError{Function: name, Message: "returned a non-wasm32 ABI version"}
	}
	return value, nil
}

// Version is the deprecated Tree-sitter spelling for the grammar ABI version.
// It falls back to ABIVersion when a guest only exposes the newer name.
func (l *Language) Version() uint32 {
	v, _ := l.VersionE()
	return v
}

// VersionE is the error-returning form of Version.  Newer shims expose both
// ts_language_version and ts_language_abi_version; older shims only expose
// the latter, so the ABI version is used as a compatible fallback.
func (l *Language) VersionE() (uint32, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	return l.callLanguageUint([]string{
		"tsw_language_version_for",
		"wasitter_language_version_for",
		"language_version_for",
		"ts_language_version_for",
		"tsw_language_version",
		"wasitter_language_version",
		"language_version",
		"ts_language_version",
		"tsw_language_abi_version_for",
		"tsw_language_abi_version",
		"wasitter_language_abi_version",
		"language_abi_version",
		"ts_language_abi_version",
	})
}

// MetadataE retrieves semantic-version metadata when the guest module exports
// it.  The stable bundled ABI predates this optional operation and therefore
// returns (nil, nil) for modules that do not provide it.
//
// Several small bridges have existed in the wild, so this method accepts all
// of the following representations:
//   - three scalar exports (..._major, ..._minor, ..._patch);
//   - a metadata function returning three result values;
//   - a pointer to the three-byte TSLanguageMetadata structure; and
//   - a packed 0x00PPMMVV/0x00MMPPPP integer.
func (l *Language) MetadataE() (*LanguageMetadata, error) {
	if err := l.ensureOpen(); err != nil {
		return nil, err
	}

	// Prefer explicit scalar exports.  They are unambiguous and do not depend
	// on a particular C struct layout.
	major, majorErr := l.callLanguageUint([]string{
		"tsw_language_metadata_major",
		"wasitter_language_metadata_major",
		"language_metadata_major",
		"ts_language_metadata_major",
	})
	if majorErr == nil {
		minor, err := l.callLanguageUint([]string{
			"tsw_language_metadata_minor",
			"wasitter_language_metadata_minor",
			"language_metadata_minor",
			"ts_language_metadata_minor",
		})
		if err != nil {
			return nil, err
		}
		patch, err := l.callLanguageUint([]string{
			"tsw_language_metadata_patch",
			"wasitter_language_metadata_patch",
			"language_metadata_patch",
			"ts_language_metadata_patch",
		})
		if err != nil {
			return nil, err
		}
		if major > uint32(^uint8(0)) || minor > uint32(^uint8(0)) || patch > uint32(^uint8(0)) {
			return nil, &ABIError{Function: "language_metadata", Message: "metadata scalar is outside uint8 range"}
		}
		return &LanguageMetadata{MajorVersion: uint8(major), MinorVersion: uint8(minor), PatchVersion: uint8(patch)}, nil
	}
	if !isUnsupported(majorErr) {
		return nil, majorErr
	}

	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_language_metadata_version",
		"wasitter_language_metadata_version",
		"language_metadata_version",
		"ts_language_metadata_version",
		"tsw_language_metadata_packed",
		"wasitter_language_metadata_packed",
		"language_metadata_packed",
		"ts_language_metadata_packed",
		"tsw_language_metadata_for",
		"wasitter_language_metadata_for",
		"language_metadata_for",
		"ts_language_metadata_for",
		"tsw_language_metadata",
		"wasitter_language_metadata",
		"language_metadata",
		"ts_language_metadata",
	}, []int{0, 1}, []int{1, 2, 3}, true, true)
	if err != nil {
		if isUnsupported(err) {
			return nil, nil
		}
		return nil, err
	}
	params := len(fn.Definition().ParamTypes())
	var args []uint64
	if params == 1 {
		args = []uint64{uint64(l.handle)}
	} else if params != 0 {
		return nil, &ABIError{Function: name, Message: "unsupported metadata function signature"}
	}
	result, callErr := fn.Call(r.Context(), args...)
	if callErr != nil {
		return nil, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		// A null metadata pointer is the documented way to indicate that a
		// grammar did not provide semantic-version metadata.
		return nil, nil
	}
	if len(result) >= 3 {
		if result[0] > uint64(^uint8(0)) || result[1] > uint64(^uint8(0)) || result[2] > uint64(^uint8(0)) {
			return nil, &ABIError{Function: name, Message: "metadata scalar is outside uint8 range"}
		}
		return &LanguageMetadata{MajorVersion: uint8(result[0]), MinorVersion: uint8(result[1]), PatchVersion: uint8(result[2])}, nil
	}
	if result[0] == 0 {
		// Pointer/packed representations use a zero first result as their
		// absence sentinel. Scalar triple representations were handled above,
		// where a legitimate major version of zero must be preserved.
		return nil, nil
	}

	value, valueOK := checkedU32(result[0])
	if !valueOK {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 metadata value"}
	}
	// A pointer representation is preferred when the value points inside the
	// guest memory and contains three plausible bytes.  The optional length
	// export lets a bridge use a four-byte/aligned structure without ambiguity.
	packedResult := strings.Contains(name, "metadata_version") || strings.Contains(name, "metadata_packed")
	if !packedResult {
		if mem := r.mod.Memory(); mem != nil && value < mem.Size() {
			length := uint32(3)
			if len(result) >= 2 {
				if result[1] > uint64(^uint32(0)) {
					return nil, &ABIError{Function: name, Message: "metadata length is not a wasm32 value"}
				}
				if result[1] != 0 {
					length = uint32(result[1])
				}
			}
			if length > 0 && length <= 16 {
				if b, ok := mem.Read(value, length); ok && len(b) >= 3 {
					return &LanguageMetadata{MajorVersion: b[0], MinorVersion: b[1], PatchVersion: b[2]}, nil
				}
			}
		}
	}
	// Packed forms are accepted for tiny hand-written bridges.  The canonical
	// representation is major in bits 16..23, minor in bits 8..15, patch in
	// bits 0..7; when the high byte is non-zero, interpret it as major instead.
	if value > 0xFFFFFF {
		return &LanguageMetadata{MajorVersion: uint8(value >> 24), MinorVersion: uint8(value >> 16), PatchVersion: uint8(value >> 8)}, nil
	}
	return &LanguageMetadata{MajorVersion: uint8(value >> 16), MinorVersion: uint8(value >> 8), PatchVersion: uint8(value)}, nil
}

// Metadata returns optional semantic-version metadata. It intentionally
// preserves the upstream nil-on-unavailable behavior while MetadataE exposes
// diagnostics for callers that need to distinguish an absent export from a
// malformed one.
func (l *Language) Metadata() *LanguageMetadata {
	m, _ := l.MetadataE()
	return m
}

// SymbolCount returns the number of symbols in the grammar.
func (l *Language) SymbolCount() uint32 {
	v, _ := l.count([]string{"tsw_language_symbol_count", "wasitter_language_symbol_count", "language_symbol_count", "ts_language_symbol_count"})
	return v
}

// NodeKindCount is the upstream spelling of SymbolCount.
func (l *Language) NodeKindCount() uint32 { return l.SymbolCount() }

// StateCount returns the number of parser states in the grammar.
func (l *Language) StateCount() uint32 {
	v, _ := l.count([]string{"tsw_language_state_count", "wasitter_language_state_count", "language_state_count", "ts_language_state_count"})
	return v
}

// ParseStateCount is the upstream spelling of StateCount.
func (l *Language) ParseStateCount() uint32 { return l.StateCount() }

// FieldCount returns the number of named fields in the grammar.
func (l *Language) FieldCount() uint32 {
	v, _ := l.count([]string{"tsw_language_field_count", "wasitter_language_field_count", "language_field_count", "ts_language_field_count"})
	return v
}

func (l *Language) count(names []string) (uint32, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	result, _, err := l.callLanguage(names)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU32(result[0])
	if !ok {
		return 0, &ABIError{Function: "language accessor", Message: "returned a non-wasm32 value"}
	}
	return value, nil
}

// SymbolName returns a symbol's display name.
func (l *Language) SymbolName(id uint16) string {
	s, _ := l.SymbolNameE(id)
	return s
}

// SymbolNameE returns a symbol's display name and any ABI or lifecycle error.
func (l *Language) SymbolNameE(id uint16) (string, error) {
	// The handle is evaluated at the call site, before callStringWithArgs can
	// validate its receiver. Preflight explicitly so nil/zero Language values
	// return a lifecycle error instead of panicking.
	if err := l.ensureOpen(); err != nil {
		return "", err
	}
	return l.callStringWithArgs([]string{
		"tsw_language_symbol_name",
		"wasitter_language_symbol_name",
		"language_symbol_name",
		"ts_language_symbol_name",
	}, uint64(l.handle), uint64(id))
}

// FieldNameForID returns a field's display name.
func (l *Language) FieldNameForID(id uint16) string {
	s, _ := l.FieldNameForIDE(id)
	return s
}

// FieldNameForId is the upstream spelling of FieldNameForID.
func (l *Language) FieldNameForId(id uint16) string { return l.FieldNameForID(id) }

// FieldNameForIDE returns a field's display name and any ABI or lifecycle
// error.
func (l *Language) FieldNameForIDE(id uint16) (string, error) {
	if err := l.ensureOpen(); err != nil {
		return "", err
	}
	return l.callStringWithArgs([]string{
		"tsw_language_field_name",
		"tsw_language_field_name_for_id",
		"wasitter_language_field_name",
		"wasitter_language_field_name_for_id",
		"language_field_name",
		"language_field_name_for_id",
		"ts_language_field_name",
		"ts_language_field_name_for_id",
	}, uint64(l.handle), uint64(id))
}

// FieldName is a concise alias used by older Go bindings.
func (l *Language) FieldName(id uint16) string { return l.FieldNameForID(id) }

// FieldNameE is the error-returning form of FieldName.
func (l *Language) FieldNameE(id uint16) (string, error) { return l.FieldNameForIDE(id) }

// SymbolForName resolves a symbol by its UTF-8 name.
func (l *Language) SymbolForName(name string) uint16 {
	v, _ := l.SymbolForNameE(name)
	return uint16(v)
}

// NodeKindForId returns the node-kind name for a numerical id.
func (l *Language) NodeKindForId(id uint16) string { return l.SymbolName(id) }

// NodeKindForID is an initialism-friendly alias for NodeKindForId.
func (l *Language) NodeKindForID(id uint16) string { return l.NodeKindForId(id) }

// IdForNodeKind resolves a node-kind name. The named selector is forwarded to
// the optional four-argument ABI when available; older bridges retain the
// historical named=true behavior.
func (l *Language) IdForNodeKind(kind string, named bool) uint16 {
	v, _ := l.SymbolForNameNamedE(kind, named)
	return v
}

// IDForNodeKind is an initialism-friendly alias for IdForNodeKind.
func (l *Language) IDForNodeKind(kind string, named bool) uint16 {
	return l.IdForNodeKind(kind, named)
}

// FieldIdForName resolves a field name to its numerical id. A native export is
// used when present; otherwise the small field table is searched locally.
func (l *Language) FieldIdForName(name string) uint16 {
	if v, err := l.fieldIDForName(name); err == nil {
		return v
	}
	count := l.FieldCount()
	// Iterate in a wider type so a grammar exposing the full uint16 field-id
	// space (count == 65535) cannot wrap id back to zero and loop forever.
	for rawID := uint32(1); rawID <= count && rawID <= uint32(^uint16(0)); rawID++ {
		id := uint16(rawID)
		if l.FieldNameForID(id) == name {
			return id
		}
	}
	return 0
}

// FieldIDForName is an initialism-friendly alias for FieldIdForName.
func (l *Language) FieldIDForName(name string) uint16 { return l.FieldIdForName(name) }

func (l *Language) fieldIDForName(name string) (uint16, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	result, _, err := l.runtime.callWithInput(context.Background(), []string{
		"tsw_language_field_id_for_name",
		"wasitter_language_field_id_for_name",
		"language_field_id_for_name",
		"ts_language_field_id_for_name",
	}, []uint64{uint64(l.handle)}, []byte(name))
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU16(result[0])
	if !ok {
		return 0, &ABIError{Function: "tsw_language_field_id_for_name", Message: "returned a value outside uint16 range"}
	}
	return value, nil
}

// NodeKindIsNamed reports whether a symbol is named. If the optional symbol
// type export is unavailable, it conservatively treats known symbols as named.
func (l *Language) NodeKindIsNamed(id uint16) bool {
	if l == nil || l.ensureOpen() != nil {
		return false
	}
	if typ, err := l.symbolType(id); err == nil {
		return typ == SymbolTypeRegular
	}
	return l.SymbolName(id) != ""
}

// NodeKindIsVisible reports whether a symbol is visible in the public tree.
func (l *Language) NodeKindIsVisible(id uint16) bool {
	if l == nil || l.ensureOpen() != nil {
		return false
	}
	if typ, err := l.symbolType(id); err == nil {
		return typ <= SymbolTypeAnonymous
	}
	return l.SymbolName(id) != ""
}

// NodeKindIsSupertype reports whether a symbol is a grammar supertype.
func (l *Language) NodeKindIsSupertype(id uint16) bool {
	if typ, err := l.symbolType(id); err == nil {
		return typ == SymbolTypeSupertype
	}
	return false
}

func (l *Language) symbolType(id uint16) (SymbolType, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := lookupIntegerFunctionLocked(r, []string{
		"tsw_language_symbol_type",
		"wasitter_language_symbol_type",
		"language_symbol_type",
		"ts_language_symbol_type",
	}, 2, 1, true, true)
	if err != nil {
		return 0, err
	}
	result, callErr := fn.Call(context.Background(), uint64(l.handle), uint64(id))
	if callErr != nil {
		return 0, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	if result[0] > uint64(^uint8(0)) {
		return 0, &ABIError{Function: name, Message: "returned a value outside uint8 range"}
	}
	return SymbolType(result[0]), nil
}

// NextState returns the parser state reached after consuming a symbol.
func (l *Language) NextState(state, id uint16) uint16 {
	if err := l.ensureOpen(); err != nil {
		return 0
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, _, err := lookupIntegerFunctionLocked(r, []string{
		"tsw_language_next_state",
		"wasitter_language_next_state",
		"language_next_state",
		"ts_language_next_state",
	}, 3, 1, true, true)
	if err != nil {
		return 0
	}
	result, callErr := fn.Call(context.Background(), uint64(l.handle), uint64(state), uint64(id))
	if callErr != nil || len(result) == 0 {
		return 0
	}
	value, ok := checkedU16(result[0])
	if !ok {
		return 0
	}
	return value
}

// SymbolForNameE resolves a named grammar symbol and returns any ABI or
// lifecycle error.
func (l *Language) SymbolForNameE(name string) (uint16, error) {
	return l.SymbolForNameNamedE(name, true)
}

// SymbolForNameNamed resolves a node-kind name while selecting named or
// anonymous grammar symbols. It is useful when a grammar has both a visible
// named rule and an anonymous token with the same textual spelling.
func (l *Language) SymbolForNameNamed(name string, named bool) uint16 {
	v, _ := l.SymbolForNameNamedE(name, named)
	return v
}

// SymbolForNameNamedE is the error-returning form of SymbolForNameNamed.
func (l *Language) SymbolForNameNamedE(name string, named bool) (uint16, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, fnName, err := lookupIntegerFunctionAllowedLocked(r, []string{
		"tsw_language_symbol_for_name_named",
		"wasitter_language_symbol_for_name_named",
		"language_symbol_for_name_named",
		"ts_language_symbol_for_name_named",
		"tsw_language_symbol_for_name",
		"wasitter_language_symbol_for_name",
		"language_symbol_for_name",
		"ts_language_symbol_for_name",
	}, []int{3, 4}, 1, true, true)
	if err != nil {
		return 0, err
	}
	ptr, allocErr := r.allocLocked(uint32(len(name)))
	if allocErr != nil {
		return 0, allocErr
	}
	defer r.freeLocked(ptr)
	if mem := r.mod.Memory(); mem == nil || !mem.Write(ptr, []byte(name)) {
		return 0, fmt.Errorf("wasitter: cannot write language symbol name")
	}
	params := len(fn.Definition().ParamTypes())
	args := []uint64{uint64(l.handle), uint64(ptr), uint64(len(name))}
	if params == 4 {
		if named {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	result, callErr := fn.Call(r.Context(), args...)
	if callErr != nil {
		return 0, &ABIError{Function: fnName, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU16(result[0])
	if !ok {
		return 0, &ABIError{Function: fnName, Message: "returned a value outside uint16 range"}
	}
	return value, nil
}

// SymbolType returns the grammar classification for a symbol. A zero value is
// returned when the optional export is unavailable, matching the historical
// convenience methods in this package.
func (l *Language) SymbolType(id uint16) SymbolType {
	if err := l.ensureOpen(); err != nil {
		return SymbolTypeRegular
	}
	result, _, err := l.runtime.call(context.Background(), []string{
		"tsw_language_symbol_type",
		"wasitter_language_symbol_type",
		"language_symbol_type",
		"ts_language_symbol_type",
	}, uint64(l.handle), uint64(id))
	if err != nil || len(result) == 0 {
		return SymbolTypeRegular
	}
	if result[0] > uint64(^uint8(0)) {
		return SymbolTypeRegular
	}
	return SymbolType(result[0])
}

// SymbolTypeE is the error-returning form of SymbolType.
func (l *Language) SymbolTypeE(id uint16) (SymbolType, error) {
	if err := l.ensureOpen(); err != nil {
		return 0, err
	}
	result, _, err := l.runtime.call(context.Background(), []string{
		"tsw_language_symbol_type",
		"wasitter_language_symbol_type",
		"language_symbol_type",
		"ts_language_symbol_type",
	}, uint64(l.handle), uint64(id))
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	if result[0] > uint64(^uint8(0)) {
		return 0, &ABIError{Function: "language_symbol_type", Message: "returned a value outside uint8 range"}
	}
	return SymbolType(result[0]), nil
}

// callLanguage invokes a language accessor while adapting between the
// canonical handle-taking form and module-wide zero-argument forms.
func (l *Language) callLanguage(names []string) ([]uint64, string, error) {
	if err := l.ensureOpen(); err != nil {
		return nil, "", err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := lookupIntegerFunctionAllowedLocked(r, names, []int{0, 1}, 1, true, true)
	if err != nil {
		return nil, name, err
	}
	params := len(fn.Definition().ParamTypes())
	var args []uint64
	switch params {
	case 0:
		args = nil
	case 1:
		args = []uint64{uint64(l.handle)}
	default:
		return nil, name, &ABIError{Function: name, Message: "unsupported language accessor signature"}
	}
	result, callErr := fn.Call(r.Context(), args...)
	if callErr != nil {
		return nil, name, &ABIError{Function: name, Message: callErr.Error()}
	}
	return result, name, nil
}

func (l *Language) callLanguageUint(names []string) (uint32, error) {
	result, name, err := l.callLanguage(names)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU32(result[0])
	if !ok {
		return 0, &ABIError{Function: name, Message: "returned a non-wasm32 value"}
	}
	return value, nil
}

// callLanguageString is the language equivalent of callStringWithArgs, with
// arity adaptation for no-argument name-pointer exports.
func (l *Language) callLanguageString(names []string) (string, error) {
	if err := l.ensureOpen(); err != nil {
		return "", err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := lookupIntegerFunctionShapesLocked(r, names, []int{0, 1}, []int{1, 2}, true, true)
	if err != nil {
		return "", err
	}
	params := len(fn.Definition().ParamTypes())
	var args []uint64
	switch params {
	case 0:
		args = nil
	case 1:
		args = []uint64{uint64(l.handle)}
	default:
		return "", &ABIError{Function: name, Message: "unsupported language name signature"}
	}
	result, callErr := fn.Call(r.Context(), args...)
	if callErr != nil {
		return "", &ABIError{Function: name, Message: callErr.Error()}
	}
	if len(result) == 0 {
		return "", nil
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: name, Message: "returned a non-wasm32 language-name pointer"}
	}
	if ptr == 0 {
		return "", nil
	}
	if len(result) >= 2 {
		length, lengthOK := checkedU32(result[1])
		if !lengthOK {
			return "", &ABIError{Function: name, Message: "language name length is not a wasm32 value"}
		}
		if length != 0 {
			if mem := r.mod.Memory(); mem != nil {
				if b, ok := mem.Read(ptr, length); ok {
					return string(b), nil
				}
			}
			return "", &ABIError{Function: name, Message: "language name points outside guest memory"}
		}
	}
	s, readErr := r.readCString(ptr)
	if readErr != nil {
		return "", &ABIError{Function: name, Message: readErr.Error()}
	}
	return s, nil
}

func (l *Language) callStringWithArgs(names []string, args ...uint64) (string, error) {
	if err := l.ensureOpen(); err != nil {
		return "", err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, resolved, lookupErr := lookupIntegerFunctionShapesLocked(r, names, []int{len(args)}, []int{1, 2}, true, true)
	if lookupErr != nil {
		return "", lookupErr
	}
	result, callErr := fn.Call(context.Background(), args...)
	if callErr != nil {
		return "", &ABIError{Function: resolved, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		return "", ErrInvalidHandle
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: resolved, Message: "returned a non-wasm32 string pointer"}
	}
	if ptr == 0 {
		return "", nil
	}
	if len(result) >= 2 {
		length, lengthOK := checkedU32(result[1])
		if !lengthOK {
			return "", &ABIError{Function: resolved, Message: "returned a non-wasm32 string length"}
		}
		if b, e := r.readBytes(ptr, length); e == nil {
			return string(b), nil
		}
	}
	s, e := r.readCString(ptr)
	if e != nil {
		return "", &ABIError{Function: resolved, Message: e.Error()}
	}
	return s, nil
}
