package wasitter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// Parser is a stateful Tree-sitter parser running in a WASM module.
type Parser struct {
	runtime *Runtime
	// runtimeMu protects the lazily bound runtime pointer. Most parser
	// operations also take mu for state serialization, but lifecycle checks and
	// callback adapters may need to inspect the owner before mu can be acquired.
	// Keeping this small gate separate avoids both data races and recursive mu
	// locking in ensureOpen.
	runtimeMu sync.RWMutex
	// handle is atomic because Handle and lifecycle transitions are commonly
	// observed by a different goroutine than the one performing a parse.  The
	// parser mutex still serializes stateful Tree-sitter operations; the atomic
	// only makes the opaque token itself safe to inspect while Close runs.
	handle atomic.Uint32

	mu       sync.Mutex
	closed   atomic.Bool
	language *Language
	logger   Logger
	timeout  atomic.Uint64
	// compatCancellationFlag and externalCancellationFlag back the optional
	// upstream-style CancellationFlag/SetCancellationFlag helpers.  A native
	// Tree-sitter parser exposes a pointer that callers may set asynchronously;
	// the WASM ABI cannot expose a Go pointer directly, so ParseWithOptions
	// mirrors the value into a short-lived guest cell while a parse is running.
	compatCancellationFlag    uintptr
	compatCancellationEnabled atomic.Bool
	externalCancellationFlag  atomic.Pointer[uintptr]
	// includedRanges is retained as a local fallback for bridge modules that
	// predate the parser range accessors. It is guarded by mu.
	includedRanges []Range
}

func (p *Parser) runtimePtr() *Runtime {
	if p == nil {
		return nil
	}
	p.runtimeMu.RLock()
	r := p.runtime
	p.runtimeMu.RUnlock()
	return r
}

func (p *Parser) setRuntime(r *Runtime) {
	if p == nil {
		return
	}
	p.runtimeMu.Lock()
	p.runtime = r
	p.runtimeMu.Unlock()
}

// NewParser creates a parser. A runtime may be supplied as an optional
// argument; omitting it creates an unbound parser that is lazily attached to
// the Runtime of the first language passed to SetLanguage. This variadic form
// preserves the familiar upstream NewParser() call while allowing
// NewParser(rt) in small programs.
func NewParser(runtimes ...*Runtime) *Parser {
	p := &Parser{}
	if len(runtimes) != 0 {
		p.setRuntime(runtimes[0])
		if p.runtimePtr() != nil {
			// Constructors cannot return an error for compatibility. A failed
			// guest allocation is reported by ParseWithError.
			if h, err := p.newGuestParser(); err == nil {
				p.handle.Store(h)
			}
		}
	}
	if p.handle.Load() != 0 {
		goruntime.SetFinalizer(p, func(parser *Parser) { _ = parser.Close() })
	}
	return p
}

// NewParserWithRuntime creates a parser and reports guest construction errors.
func NewParserWithRuntime(rt *Runtime) (*Parser, error) {
	if rt == nil {
		return nil, ErrNoRuntime
	}
	// Check the runtime before allocating the guest parser.  Without this
	// preflight a parser built against a closed (or externally closed) module
	// reaches the export call and returns a low-level wazero "module closed"
	// ABIError.  Constructors elsewhere in the package consistently report
	// the lifecycle sentinel, and callers should be able to use errors.Is
	// here just as they can after a parser/runtime has been closed.
	if err := rt.ensureOpen(); err != nil {
		return nil, err
	}
	p := &Parser{}
	p.setRuntime(rt)
	h, err := p.newGuestParser()
	if err != nil {
		return nil, err
	}
	p.handle.Store(h)
	goruntime.SetFinalizer(p, func(parser *Parser) { _ = parser.Close() })
	return p, nil
}

func (p *Parser) newGuestParser() (uint32, error) {
	r := p.runtimePtr()
	if p == nil || r == nil {
		return 0, ErrNoRuntime
	}
	r.mu.Lock()
	fn, name, lookupErr := lookupIntegerFunctionLocked(r, []string{
		"tsw_parser_new",
		"wasitter_parser_new",
		"ts_parser_new",
		"parser_new",
	}, 0, 1, true, true)
	if lookupErr != nil {
		r.mu.Unlock()
		return 0, lookupErr
	}
	result, callErr := fn.Call(r.Context())
	r.mu.Unlock()
	if callErr != nil {
		return 0, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		return 0, &ABIError{Function: name, Message: "returned a null parser handle"}
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return 0, &ABIError{Function: name, Message: "returned a non-wasm32 parser handle"}
	}
	if handle == 0 {
		return 0, &ABIError{Function: name, Message: "returned a null parser handle"}
	}
	return handle, nil
}

func (p *Parser) ensureOpen() error {
	if p == nil || p.closed.Load() {
		return ErrClosed
	}
	r := p.runtimePtr()
	if r == nil {
		return ErrNoRuntime
	}
	// Check the owning runtime before validating the parser handle.  A parser
	// constructed against an already-closed (or externally closed) module may
	// legitimately have a zero handle; reporting ErrClosed is more useful and
	// deterministic than leaking the implementation detail as ErrInvalidHandle.
	if err := r.ensureOpen(); err != nil {
		return err
	}
	if p.handle.Load() == 0 {
		return ErrInvalidHandle
	}
	return nil
}

// Handle returns the opaque guest parser handle. It returns zero once either
// the parser or its owning runtime has begun closing, so callers cannot pass a
// stale wasm32 pointer to an ABI extension.
func (p *Parser) Handle() uint32 {
	if p == nil || p.closed.Load() {
		return 0
	}
	r := p.runtimePtr()
	if r == nil || r.closedState() {
		return 0
	}
	handle := p.handle.Load()
	// Close publishes the lifecycle transition before clearing the handle.
	// Recheck after the load so a concurrent close cannot expose the old guest
	// address through this safety-oriented accessor.
	if p.closed.Load() || r.closedState() {
		return 0
	}
	return handle
}

// SetLanguage assigns a grammar to the parser.
func (p *Parser) SetLanguage(language *Language) error {
	// Keep the upstream construction pattern usable: callers commonly create a
	// parser first and attach a language later (`NewParser(); p.SetLanguage(l)`).
	// Such a parser has no runtime yet, so ensureOpen cannot be called until the
	// language has supplied one.  The parser lock below serializes this lazy
	// attachment with another SetLanguage or Close call.
	if p == nil || p.closed.Load() {
		return ErrClosed
	}
	if language == nil {
		// Preserve the zero-value parser's historical diagnostic.  A parser
		// without an owning Runtime cannot accept any language (including nil),
		// and callers commonly use this check to detect an unbound value.
		if p.runtimePtr() == nil {
			return ErrNoRuntime
		}
		return ErrNoLanguage
	}
	if err := language.ensureOpen(); err != nil {
		return err
	}
	// Acquire the parser lock before consulting the language. The actual
	// parser call below necessarily takes Runtime.mu, and all other parser
	// operations use the p.mu -> Runtime.mu order. Doing the ABI preflight
	// first would take Runtime.mu -> p.mu and could deadlock with a concurrent
	// Parse/Reset/SetIncludedRanges call.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return ErrClosed
	}
	if language.runtime == nil {
		return ErrNoRuntime
	}
	parserRuntime := p.runtimePtr()
	if parserRuntime == nil {
		// Attach the language's owning Runtime exactly once.  NewParser's
		// constructor intentionally cannot return an allocation error for
		// compatibility, so the first mutating operation is the point at which
		// we report a failed guest parser allocation.
		p.setRuntime(language.runtime)
		h, err := p.newGuestParser()
		if err != nil {
			p.setRuntime(nil)
			return err
		}
		p.handle.Store(h)
		if h != 0 {
			goruntime.SetFinalizer(p, func(parser *Parser) { _ = parser.Close() })
		}
	}
	parserRuntime = p.runtimePtr()
	if parserRuntime == nil {
		return ErrNoRuntime
	}
	if err := parserRuntime.ensureOpen(); err != nil {
		return err
	}
	if language.runtime != parserRuntime {
		return fmt.Errorf("wasitter: language belongs to a different runtime")
	}
	if p.handle.Load() == 0 {
		// NewParser(rt) deliberately keeps construction error-free.  If its
		// initial allocation failed, retry now so SetLanguage has deterministic
		// error reporting rather than returning ErrInvalidHandle forever.
		h, err := p.newGuestParser()
		if err != nil {
			return err
		}
		p.handle.Store(h)
		goruntime.SetFinalizer(p, func(parser *Parser) { _ = parser.Close() })
	}
	// Check the advertised grammar ABI before entering the parser. The guest
	// performs the same validation, but this yields a concrete
	// LanguageError for callers that need to distinguish version mismatches.
	// Metadata is optional on older/custom bridges, so unavailable/zero values
	// are left to the guest validator.
	if version, versionErr := language.ABIVersionE(); versionErr == nil &&
		version != 0 && (version < MIN_COMPATIBLE_LANGUAGE_VERSION || version > LANGUAGE_VERSION) {
		return &LanguageError{Version: version}
	}
	// Close sets the lifecycle bit before waiting for this mutex.  Recheck
	// after acquiring it so a caller that raced Close never invokes the guest
	// with a handle that has already been invalidated.
	if p.closed.Load() || p.handle.Load() == 0 {
		return ErrClosed
	}
	parserRuntime.mu.Lock()
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(parserRuntime, []string{
		"tsw_parser_set_language",
		"wasitter_parser_set_language",
		"ts_parser_set_language",
		"parser_set_language",
	}, []int{2}, []int{0, 1}, true, true)
	if lookupErr != nil {
		parserRuntime.mu.Unlock()
		return lookupErr
	}
	result, callErr := fn.Call(parserRuntime.Context(), uint64(p.handle.Load()), uint64(language.handle))
	parserRuntime.mu.Unlock()
	if callErr != nil {
		return &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) != 0 {
		// Canonical shims return 1 on success. A negative value is always an
		// error; for legacy C-style wrappers, zero is also treated as failure.
		status := int32(result[0])
		if status < 0 || status == 0 {
			return &ABIError{Function: name, Message: "language rejected (ABI/version mismatch)"}
		}
	}
	// A runtime may be closed concurrently while the guest call is in flight.
	// Runtime.Close waits for the call lock, but marks the runtime closed before
	// waiting; do not publish a language association after that transition.
	if parserRuntime.closedState() {
		return ErrClosed
	}
	// Keep an independent wrapper.  Language values are immutable views owned
	// by Runtime, while Close only affects the particular Go wrapper; retaining
	// the caller's pointer would let a later language.Close unexpectedly disable
	// this parser.
	p.language = language.clone()
	return nil
}

// SetIncludedRanges limits parsing to the supplied source ranges. Ranges use
// UTF-8 byte offsets and zero-based row/column points, just like Tree-sitter's
// TSRange. The slice is copied, so callers may reuse their backing array.
func (p *Parser) SetIncludedRanges(ranges []Range) error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	// Tree-sitter requires ranges to be ordered and non-overlapping. Validate
	// this before entering the guest so callers get the same typed error as the
	// native binding, and so a malformed list cannot partially replace the
	// parser's previous configuration. Empty ranges are intentionally valid and
	// reset the parser to Tree-sitter's implicit whole-document range.
	var previousEnd uint32
	for i, current := range ranges {
		if current.EndByte < current.StartByte || (i > 0 && current.StartByte < previousEnd) {
			return &IncludedRangesError{Index: uint32(i)}
		}
		previousEnd = current.EndByte
	}
	encoded, err := encodeRanges(ranges)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || p.handle.Load() == 0 {
		return ErrClosed
	}
	r := p.runtimePtr()
	if r == nil {
		return ErrNoRuntime
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_parser_set_included_ranges",
		"wasitter_parser_set_included_ranges",
		"ts_parser_set_included_ranges",
		"parser_set_included_ranges",
	}, []int{3}, []int{0, 1}, true, true)
	if lookupErr != nil {
		if isUnsupported(lookupErr) {
			if r.closedState() {
				return ErrClosed
			}
			if len(ranges) == 0 {
				p.includedRanges = defaultIncludedRanges()
			} else {
				p.includedRanges = append([]Range(nil), ranges...)
			}
			return nil
		}
		return lookupErr
	}
	// A zero-length list has a special meaning in the C API (restore the
	// default full-document range). Pass a null pointer rather than allocating a
	// dummy byte; this also matches native callers and strict custom bridges.
	var ptr uint32
	if len(encoded) != 0 {
		var allocErr error
		ptr, allocErr = r.allocLocked(uint32(len(encoded)))
		if allocErr != nil {
			return allocErr
		}
		defer r.freeLocked(ptr)
		if mem := r.mod.Memory(); mem == nil || !mem.Write(ptr, encoded) {
			return io.ErrShortWrite
		}
	}
	result, callErr := fn.Call(r.Context(), uint64(p.handle.Load()), uint64(ptr), uint64(len(ranges)))
	if callErr != nil {
		return &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) != 0 && result[0] == 0 {
		// The bundled bridge returns zero only for malformed ranges. The list has
		// already passed host-side validation, so preserve a useful ABI error for
		// custom modules that reject it for another reason.
		return &ABIError{Function: name, Message: "included ranges rejected"}
	}
	if r.closedState() {
		return ErrClosed
	}
	if len(ranges) == 0 {
		p.includedRanges = defaultIncludedRanges()
	} else {
		p.includedRanges = append([]Range(nil), ranges...)
	}
	return nil
}

// SetIncludedRangesE is an explicit error-suffixed alias for
// SetIncludedRanges.
func (p *Parser) SetIncludedRangesE(ranges []Range) error { return p.SetIncludedRanges(ranges) }

// IncludedRanges returns a copy of the ranges currently configured on the
// parser. Bridges without the optional guest accessor use the locally retained
// list, initialized to Tree-sitter's implicit whole-document range.
func (p *Parser) IncludedRanges() []Range {
	ranges, _ := p.IncludedRangesE()
	return ranges
}

// GetIncludedRanges is a compatibility alias for IncludedRanges.
func (p *Parser) GetIncludedRanges() []Range { return p.IncludedRanges() }

// IncludedRangesE is the error-returning form of IncludedRanges.
func (p *Parser) IncludedRangesE() ([]Range, error) {
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || p.handle.Load() == 0 {
		return nil, ErrClosed
	}
	r := p.runtimePtr()
	if r == nil {
		return nil, ErrNoRuntime
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_parser_included_ranges_into",
		"wasitter_parser_included_ranges_into",
		"ts_parser_included_ranges_into",
		"parser_included_ranges_into",
	}, []int{3}, []int{1}, true, true)
	if lookupErr != nil {
		if isUnsupported(lookupErr) {
			if r.closedState() {
				return nil, ErrClosed
			}
			return p.cachedIncludedRanges(), nil
		}
		return nil, lookupErr
	}
	params := len(fn.Definition().ParamTypes())
	if params != 3 {
		if r.closedState() {
			return nil, ErrClosed
		}
		return p.cachedIncludedRanges(), nil
	}
	countResult, callErr := fn.Call(r.Context(), uint64(p.handle.Load()), 0, 0)
	if callErr != nil {
		return nil, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(countResult) == 0 {
		return nil, &ABIError{Function: name, Message: "returned no range count"}
	}
	count, countOK := checkedU32(countResult[0])
	if !countOK {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 range count"}
	}
	if count == 0 {
		if r.closedState() {
			return nil, ErrClosed
		}
		return p.cachedIncludedRanges(), nil
	}
	if uint64(count) > uint64(^uint32(0))/24 {
		return nil, fmt.Errorf("wasitter: included range count overflows wasm32 memory")
	}
	ptr, allocErr := r.allocLocked(count * 24)
	if allocErr != nil {
		return nil, allocErr
	}
	defer r.freeLocked(ptr)
	copyResult, copyErr := fn.Call(r.Context(), uint64(p.handle.Load()), uint64(ptr), uint64(count))
	if copyErr != nil {
		return nil, &ABIError{Function: name, Message: copyErr.Error(), Cause: copyErr}
	}
	if len(copyResult) == 0 {
		return nil, &ABIError{Function: name, Message: "included-range output was truncated"}
	}
	copyCount, copyOK := checkedU32(copyResult[0])
	if !copyOK || copyCount < count {
		return nil, &ABIError{Function: name, Message: "included-range output was truncated"}
	}
	mem := r.mod.Memory()
	if mem == nil {
		return nil, fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
	}
	b, ok := mem.Read(ptr, count*24)
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	if r.closedState() {
		return nil, ErrClosed
	}
	return decodeRanges(b, count), nil
}

// cachedIncludedRanges returns the parser's locally retained range list for a
// bridge that does not expose the optional guest accessor. Tree-sitter always
// has one implicit whole-document range, even before the first parse; represent
// that range with the same UINT32_MAX sentinels used by the C runtime.
func (p *Parser) cachedIncludedRanges() []Range {
	if p == nil || len(p.includedRanges) == 0 {
		return defaultIncludedRanges()
	}
	return append([]Range(nil), p.includedRanges...)
}

func defaultIncludedRanges() []Range {
	max := ^uint32(0)
	return []Range{{
		StartByte:  0,
		EndByte:    max,
		StartPoint: Point{},
		EndPoint:   Point{Row: max, Column: max},
	}}
}

// Language returns the parser's currently assigned language, if known.
func (p *Parser) Language() *Language {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return nil
	}
	return p.language.clone()
}

// Parse parses UTF-8 source text. The old tree may be supplied after applying
// Tree.Edit to enable incremental parsing. Parse returns an error for malformed
// ABI calls, cancellation, or a null guest tree.
func (p *Parser) Parse(input []byte, oldTrees ...*Tree) (*Tree, error) {
	oldTree, err := oneOldTree(oldTrees)
	if err != nil {
		return nil, err
	}
	return p.ParseWithOptions(p.runtimeContext(), input, oldTree, nil)
}

// ParseWithError is an explicit spelling useful when migrating from bindings
// whose Parse method had a different signature.
func (p *Parser) ParseWithError(input []byte, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse(input, oldTrees...)
}

// ParseString parses a UTF-8 string.
func (p *Parser) ParseString(input string, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse([]byte(input), oldTrees...)
}

// ParseUTF8 is an explicit spelling for Parse.
func (p *Parser) ParseUTF8(input []byte, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse(input, oldTrees...)
}

// ParseCompat mirrors the upstream binding's no-error convenience. It returns
// nil when parsing fails; callers that need diagnostics should use Parse.
func (p *Parser) ParseCompat(input []byte, oldTrees ...*Tree) *Tree {
	t, _ := p.Parse(input, oldTrees...)
	return t
}

// MustParse parses input and panics on an ABI/runtime error.
func (p *Parser) MustParse(input []byte, oldTrees ...*Tree) *Tree {
	t, err := p.Parse(input, oldTrees...)
	if err != nil {
		panic(err)
	}
	return t
}

// ParseContext parses with a caller-supplied context. Cancellation is passed
// directly to wazero and is honored by runtimes configured with
// WithCloseOnContextDone.
func (p *Parser) ParseContext(ctx context.Context, input []byte, oldTrees ...*Tree) (*Tree, error) {
	oldTree, err := oneOldTree(oldTrees)
	if err != nil {
		return nil, err
	}
	return p.ParseWithOptions(ctx, input, oldTree, nil)
}

// ParseCtx parses with a caller-supplied context.
//
// The modern tree-sitter Go binding spells this as ParseCtx(ctx, input,
// oldTree), which is also wasitter's preferred ordering. For compatibility
// with early experimental adapters, the old-tree-first ordering is accepted
// as well. Go does not support overloaded methods, so this entry point uses
// strict runtime type validation. The strongly typed ParseContext method
// remains available for code that prefers compile-time argument checking.
func (p *Parser) ParseCtx(ctx context.Context, args ...any) (*Tree, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, fmt.Errorf("wasitter: ParseCtx expects (input, oldTree) or (oldTree, input), got %d arguments", len(args))
	}
	var input []byte
	var oldTree *Tree
	// A byte slice in the first position identifies the canonical ordering. A
	// nil first argument is ambiguous: it can be a nil input, or the alternate
	// old-tree-first argument. Resolve the useful alternate form
	// (`ParseCtx(ctx, nil, input)`) by looking at the second argument before
	// falling back to the canonical interpretation. This is
	// especially important because an untyped nil passed through an `any`
	// parameter loses whether the caller intended a tree or a byte slice.
	if first, ok := args[0].([]byte); ok || args[0] == nil {
		if args[0] == nil && len(args) == 2 {
			if modernInput, inputOK := args[1].([]byte); inputOK {
				return p.ParseContext(ctx, modernInput, nil)
			}
			// A non-byte second argument is handled by the historical
			// branch below so it receives the precise old-tree diagnostic.
		}
		if ok {
			input = first
		}
		if len(args) == 2 {
			var treeOK bool
			oldTree, treeOK = parseParseOptionsTreeArg(args[1])
			if !treeOK {
				return nil, fmt.Errorf("wasitter: ParseCtx old tree must be *Tree or nil, got %T", args[1])
			}
		}
		return p.ParseContext(ctx, input, oldTree)
	}
	// Otherwise the first argument must be the alternate old-tree position and
	// the second argument must contain the source bytes.
	var treeOK bool
	oldTree, treeOK = parseParseOptionsTreeArg(args[0])
	if !treeOK {
		return nil, fmt.Errorf("wasitter: ParseCtx first argument must be []byte, *Tree, or nil, got %T", args[0])
	}
	if len(args) != 2 {
		return nil, fmt.Errorf("wasitter: ParseCtx old-tree form requires input []byte")
	}
	var inputOK bool
	input, inputOK = args[1].([]byte)
	if !inputOK && args[1] != nil {
		return nil, fmt.Errorf("wasitter: ParseCtx input must be []byte or nil, got %T", args[1])
	}
	return p.ParseContext(ctx, input, oldTree)
}

// ParseInput parses data supplied lazily by a callback or an Input descriptor.
//
// The historical wasitter form is ParseInput(read, oldTree), where read is
// a function receiving a uint32 byte offset. The smacker/go-tree-sitter form
// is ParseInput(oldTree, Input). Accepting both forms keeps migration
// straightforward while ParseInputCtx remains the strongly typed,
// context-aware descriptor API.
func (p *Parser) ParseInput(args ...any) (*Tree, error) {
	if len(args) == 0 || len(args) > 2 {
		return nil, fmt.Errorf("wasitter: ParseInput expects (read, oldTree) or (oldTree, Input), got %d arguments", len(args))
	}
	// In the upstream form the old tree is commonly written as an untyped nil:
	// `ParseInput(nil, Input{Read: ...})`.  Since both the callback and old-tree
	// positions are represented as `any`, inspect the second argument first so
	// this valid call is not mistaken for a nil callback.  A typed nil *Tree is
	// handled by the regular descriptor branch below.
	if args[0] == nil && len(args) == 2 {
		if descriptor, ok := args[1].(Input); ok {
			return p.ParseInputCtx(p.runtimeContext(), nil, descriptor)
		}
	}
	if read, ok := normalizeUTF8ReadCallback(args[0]); ok {
		if read == nil {
			return nil, fmt.Errorf("wasitter: nil input callback")
		}
		var oldTree *Tree
		if len(args) == 2 {
			var treeOK bool
			oldTree, treeOK = parseParseOptionsTreeArg(args[1])
			if !treeOK {
				return nil, fmt.Errorf("wasitter: ParseInput old tree must be *Tree or nil, got %T", args[1])
			}
		}
		return p.parseInputRead(read, oldTree)
	}
	// Descriptor form: (oldTree, Input). A single Input is also accepted as a
	// convenient no-old-tree shorthand.
	if descriptor, ok := args[0].(Input); ok {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasitter: ParseInput descriptor form accepts one Input argument")
		}
		return p.ParseInputCtx(p.runtimeContext(), nil, descriptor)
	}
	if len(args) != 2 {
		return nil, fmt.Errorf("wasitter: ParseInput first argument must be a callback or *Tree, got %T", args[0])
	}
	oldTree, treeOK := parseParseOptionsTreeArg(args[0])
	if !treeOK {
		return nil, fmt.Errorf("wasitter: ParseInput old tree must be *Tree or nil, got %T", args[0])
	}
	descriptor, descriptorOK := args[1].(Input)
	if !descriptorOK {
		return nil, fmt.Errorf("wasitter: ParseInput input must be Input, got %T", args[1])
	}
	return p.ParseInputCtx(p.runtimeContext(), oldTree, descriptor)
}

// parseInputRead is the typed implementation shared by ParseInput and the
// callback-oriented compatibility aliases.
func (p *Parser) parseInputRead(read ReadFunc, oldTree *Tree) (*Tree, error) {
	if read == nil {
		return nil, fmt.Errorf("wasitter: nil input callback")
	}
	// Validate ownership before invoking user code. Apart from avoiding
	// surprising callback side effects after Parser.Close, this keeps callback
	// parsing consistent with ParseWithOptions' lifecycle behavior.
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	all, err := collectUTF8Input(read)
	if err != nil {
		return nil, err
	}
	return p.Parse(all, oldTree)
}

// collectUTF8Input materializes a Tree-sitter-style input callback.  The
// callback is called with the byte offset immediately after the bytes already
// returned and must return bytes beginning at that offset (usually the
// remaining suffix, although returning a bounded chunk is also valid).  It is
// tempting to remove an apparent copy of the already-collected prefix here,
// but that is not sound: a perfectly valid source can contain repeated bytes
// (for example, a callback returning `aa`, then `aa` for `aaaa`).  Treating the
// second chunk as a duplicate would silently truncate the input.  Appending
// every returned chunk follows the upstream contract and supports both suffix
// and bounded-chunk callbacks without guessing from contents.
func collectUTF8Input(read func(offset uint32, point Point) []byte) ([]byte, error) {
	var all []byte
	point := Point{}
	// A callback is user code, so a finite guard is preferable to allowing a
	// buggy callback to spin forever.  This is deliberately generous enough for
	// very small chunks while still making the failure deterministic.
	const maxCalls = 1 << 20
	for calls := 0; calls < maxCalls; calls++ {
		chunk := read(uint32(len(all)), point)
		if len(chunk) == 0 {
			return all, nil
		}
		// Check the wasm32 limit before growing the host slice.  Apart from
		// avoiding an overflow in the callback offset on the next iteration,
		// this keeps a malicious or accidental callback from forcing an
		// allocation larger than the ABI can represent only to reject it after
		// append has already consumed the memory.
		if uint64(len(all))+uint64(len(chunk)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("wasitter: input exceeds uint32 byte offset")
		}
		all = append(all, chunk...)
		for _, b := range chunk {
			if b == '\n' {
				point.Row++
				point.Column = 0
			} else {
				point.Column++
			}
		}
	}
	return nil, io.ErrNoProgress
}

// ParseUTF16LE parses UTF-16 code units. The WASM shim consumes UTF-8, so the
// code units are decoded before parsing; source offsets in the returned tree
// therefore refer to the UTF-8 representation. The old tree may be nil.
func (p *Parser) ParseUTF16LE(input []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse(utf8BytesFromUTF16(input), oldTrees...)
}

// ParseUTF16LEText is the non-incremental one-argument convenience form.
func (p *Parser) ParseUTF16LEText(input []uint16) (*Tree, error) {
	return p.ParseUTF16LE(input)
}

// ParseUTF16BE is equivalent to ParseUTF16LE for Go uint16 code units. A Go
// uint16 slice has no byte order; callers should decode external big-endian
// bytes into uint16 values first.
func (p *Parser) ParseUTF16BE(input []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse(utf8BytesFromUTF16(input), oldTrees...)
}

// ParseUTF16BEText is the non-incremental one-argument convenience form.
func (p *Parser) ParseUTF16BEText(input []uint16) (*Tree, error) {
	return p.ParseUTF16BE(input)
}

func utf8BytesFromUTF16(input []uint16) []byte {
	return []byte(string(utf16.Decode(input)))
}

// ParseWithOptions parses with optional progress instrumentation. The current
// shim ABI does not expose a callback trampoline; the callback is invoked once
// before and once after the guest call so callers still get deterministic
// cancellation behavior.
// ParseWithOptions parses input with optional progress instrumentation.
//
// The package originally exposed a WASM-oriented form:
//
//	ParseWithOptions(ctx, input, oldTree, options)
//
// whereas the upstream tree-sitter Go binding uses a callback-oriented form:
//
//	ParseWithOptions(read, oldTree, options)
//
// Go has no method overloading, so this entry point accepts both forms and
// performs strict argument validation before dispatching to the typed
// implementation below.  The callback form is materialized into one UTF-8
// buffer before entering the guest, just like ParseInput.  Returning an error
// is intentional: it preserves the idiomatic/error-aware API of wasitter
// while still allowing source-compatible calls to the upstream shape.
func (p *Parser) ParseWithOptions(args ...any) (*Tree, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("wasitter: ParseWithOptions requires arguments")
	}

	// WASM-oriented form: (context.Context, []byte, *Tree, *ParseOptions).
	// Accept a three-argument variant with a nil/default options value as a
	// convenience; the historical four-argument form remains unchanged.
	if ctx, isContext := args[0].(context.Context); isContext || args[0] == nil {
		if len(args) < 2 || len(args) > 4 {
			return nil, fmt.Errorf("wasitter: ParseWithOptions context form expects 2 to 4 arguments, got %d", len(args))
		}
		var input []byte
		if args[1] != nil {
			var ok bool
			input, ok = args[1].([]byte)
			if !ok {
				return nil, fmt.Errorf("wasitter: ParseWithOptions input must be []byte, got %T", args[1])
			}
		}
		var oldTree *Tree
		if len(args) >= 3 {
			var ok bool
			oldTree, ok = parseParseOptionsTreeArg(args[2])
			if !ok {
				return nil, fmt.Errorf("wasitter: ParseWithOptions old tree must be *Tree or nil, got %T", args[2])
			}
		}
		var options *ParseOptions
		if len(args) >= 4 {
			var ok bool
			options, ok = parseParseOptionsArg(args[3])
			if !ok {
				return nil, fmt.Errorf("wasitter: ParseWithOptions options must be *ParseOptions, ParseOptions, or nil, got %T", args[3])
			}
		}
		return p.parseWithOptionsContext(ctx, input, oldTree, options)
	}

	// Upstream callback form: (func(int, Point) []byte, *Tree,
	// *ParseOptions). A two-argument variant is accepted as the natural alias
	// of ParseWith and uses no options.
	if len(args) < 1 || len(args) > 3 {
		return nil, fmt.Errorf("wasitter: ParseWithOptions callback form expects 1 to 3 arguments, got %d", len(args))
	}
	read, ok := normalizeUTF8ReadCallback(args[0])
	if !ok || read == nil {
		return nil, fmt.Errorf("wasitter: ParseWithOptions callback must be func(int, Point) []byte or ReadFunc, got %T", args[0])
	}
	// Validate ownership before invoking user code. Apart from producing a
	// deterministic lifecycle error for a closed/unbound parser, this avoids
	// surprising callback side effects when the parse cannot possibly start.
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	var oldTree *Tree
	if len(args) >= 2 {
		var treeOK bool
		oldTree, treeOK = parseParseOptionsTreeArg(args[1])
		if !treeOK {
			return nil, fmt.Errorf("wasitter: ParseWithOptions old tree must be *Tree or nil, got %T", args[1])
		}
	}
	var options *ParseOptions
	if len(args) >= 3 {
		var optionsOK bool
		options, optionsOK = parseParseOptionsArg(args[2])
		if !optionsOK {
			return nil, fmt.Errorf("wasitter: ParseWithOptions options must be *ParseOptions, ParseOptions, or nil, got %T", args[2])
		}
	}
	input, err := collectUTF8Input(read)
	if err != nil {
		return nil, err
	}
	return p.parseWithOptionsContext(p.runtimeContext(), input, oldTree, options)
}

// ParseWithOptionsContext is the strongly typed spelling of the
// context-plus-byte-slice ParseWithOptions form. It is useful for callers who
// prefer compile-time argument checking while ParseWithOptions itself remains
// source-compatible with the upstream callback form.
func (p *Parser) ParseWithOptionsContext(ctx context.Context, input []byte, oldTree *Tree, options *ParseOptions) (*Tree, error) {
	return p.parseWithOptionsContext(ctx, input, oldTree, options)
}

// parseParseOptionsTreeArg decodes the optional old-tree argument used by the
// variadic ParseWithOptions compatibility dispatcher. A nil interface and a
// typed nil *Tree both represent no old tree.
func parseParseOptionsTreeArg(value any) (*Tree, bool) {
	if value == nil {
		return nil, true
	}
	tree, ok := value.(*Tree)
	return tree, ok
}

// parseParseOptionsArg decodes ParseOptions in pointer or value form. The
// upstream API uses *ParseOptions; accepting a value is harmless and avoids a
// surprising failure when callers use the value-shaped Go API elsewhere.
func parseParseOptionsArg(value any) (*ParseOptions, bool) {
	if value == nil {
		return nil, true
	}
	switch options := value.(type) {
	case *ParseOptions:
		return options, true
	case ParseOptions:
		copy := options
		return &copy, true
	default:
		return nil, false
	}
}

// normalizeUTF8ReadCallback adapts callback spellings used by the upstream
// binding (int offsets) and by wasitter's fixed-width ReadFunc (uint32
// offsets) to collectUTF8Input's canonical uint32 callback.
func normalizeUTF8ReadCallback(value any) (ReadFunc, bool) {
	switch callback := value.(type) {
	case func(int, Point) []byte:
		if callback == nil {
			return nil, true
		}
		return func(offset uint32, point Point) []byte {
			// On a 32-bit host int can represent every uint32 value; on a wider
			// host this conversion is exact as well. Keep the explicit conversion
			// here to make the callback contract obvious.
			return callback(int(offset), point)
		}, true
	case ReadFunc:
		if callback == nil {
			return nil, true
		}
		return callback, true
	case func(uint32, Point) []byte:
		if callback == nil {
			return nil, true
		}
		return ReadFunc(callback), true
	}
	// A variadic compatibility entry point receives callbacks as `any`, which
	// means a user-defined function type (for example `type Reader func(int,
	// Point) []byte`) does not match the unnamed function cases above even
	// though it is assignment-compatible with the upstream API. Convert such
	// defined function types once at the boundary so callers do not need an
	// explicit cast. Reflection is used only during dispatch; the returned
	// closure is an ordinary typed function and incurs no per-byte reflection.
	if value == nil {
		return nil, true
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Func {
		return nil, false
	}
	if rv.IsNil() {
		return nil, true
	}
	intCallbackType := reflect.TypeOf((func(int, Point) []byte)(nil))
	if rv.Type().ConvertibleTo(intCallbackType) {
		callback := rv.Convert(intCallbackType).Interface().(func(int, Point) []byte)
		return func(offset uint32, point Point) []byte {
			return callback(int(offset), point)
		}, true
	}
	uint32CallbackType := reflect.TypeOf((func(uint32, Point) []byte)(nil))
	if rv.Type().ConvertibleTo(uint32CallbackType) {
		callback := rv.Convert(uint32CallbackType).Interface().(func(uint32, Point) []byte)
		return ReadFunc(callback), true
	}
	return nil, false
}

// parseWithOptionsContext contains the strongly typed implementation shared
// by both ParseWithOptions argument forms and all internal convenience APIs.
func (p *Parser) parseWithOptionsContext(ctx context.Context, input []byte, oldTree *Tree, options *ParseOptions) (*Tree, error) {
	if ctx == nil {
		ctx = p.runtimeContext()
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	// wazero only interrupts a running guest when the selected RuntimeConfig
	// requests it.  Checking before entering the guest gives callers
	// deterministic cancellation even with the default compiler configuration.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The optional upstream-compatible cancellation flag is sampled before
	// entering the guest as well.  This handles a flag that was set before a
	// parse started without requiring a background watcher or a guest call.
	if p.cancellationRequested() {
		return nil, context.Canceled
	}
	if uint64(len(input)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("wasitter: input exceeds uint32 byte offset")
	}
	p.mu.Lock()
	language := p.language
	p.mu.Unlock()
	if language == nil {
		return nil, ErrNoLanguage
	}
	if oldTree != nil {
		if err := oldTree.ensureOpen(); err != nil {
			return nil, err
		}
		// Use the synchronized runtime accessor.  A parser created with
		// NewParser() may still be attaching its runtime in SetLanguage while a
		// caller starts a parse; reading p.runtime directly here would race and
		// could incorrectly reject an old tree from the same runtime.
		if oldTree.runtime() != p.runtimePtr() {
			return nil, fmt.Errorf("wasitter: old tree belongs to a different runtime")
		}
		// Keep the old tree alive while its guest handle is passed to parse.
		oldTree.mu.RLock()
		defer oldTree.mu.RUnlock()
	}
	if options != nil && options.ProgressCallback != nil {
		if options.ProgressCallback(ParseState{CurrentByteOffset: 0}) {
			return nil, context.Canceled
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runtime
	// Close marks the parser closed before waiting for mu. Recheck after
	// acquiring it so a raced parse never calls the guest with an invalidated
	// parser handle.
	if p.closed.Load() || p.handle.Load() == 0 {
		return nil, ErrClosed
	}
	// Keep allocation, input copy, and parsing under one runtime lock. A
	// Runtime may host multiple parsers; releasing the lock between these steps
	// would allow another parser to overwrite the shared guest buffer.
	r.mu.Lock()
	ptr, err := r.allocLocked(uint32(len(input)))
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	defer func() {
		r.freeLocked(ptr)
		r.mu.Unlock()
	}()
	mem := r.mod.Memory()
	if mem == nil || !mem.Write(ptr, input) {
		return nil, io.ErrShortWrite
	}
	old := uint32(0)
	if oldTree != nil {
		old = oldTree.handle.Load()
		if old == 0 || oldTree.closed.Load() {
			return nil, ErrClosed
		}
	}

	// The canonical ABI orders arguments as parser, old_tree, input_ptr,
	// input_len. A small compatibility branch recognizes a three-argument
	// parser function used by early prototypes.
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_parser_parse",
		"wasitter_parser_parse",
		"ts_parser_parse",
		"parser_parse",
	}, []int{3, 4}, []int{1}, true, true)
	if lookupErr != nil {
		return nil, lookupErr
	}
	parserHandle := p.handle.Load()
	args := []uint64{uint64(parserHandle), uint64(old), uint64(ptr), uint64(len(input))}
	if len(fn.Definition().ParamTypes()) == 3 {
		args = []uint64{uint64(parserHandle), uint64(ptr), uint64(len(input))}
	}
	// Tree-sitter's cancellation flag is checked from inside the guest parser,
	// so it remains effective even when the wazero runtime was not configured
	// with WithCloseOnContextDone.  The watcher is installed after the input
	// buffer is ready and stopped before that buffer is freed (defer ordering).
	stopCancellation, cancellationErr := p.installCancellationLocked(ctx, parserHandle)
	if cancellationErr != nil {
		return nil, cancellationErr
	}
	if stopCancellation != nil {
		defer stopCancellation()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	result, callErr := fn.Call(ctx, args...)
	if callErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		// A native cancellation flag causes Tree-sitter to return a null tree
		// without a WASM call error. Prefer the caller's context error so
		// errors.Is(err, context.Canceled/DeadlineExceeded) remains reliable.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if p.cancellationRequested() {
			return nil, context.Canceled
		}
		if r.closedState() {
			return nil, ErrClosed
		}
		if p.timeout.Load() != 0 {
			return nil, ErrOperationLimit
		}
		return nil, &ABIError{Function: name, Message: "returned a null tree handle"}
	}
	treeHandle, ok := checkedU32(result[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 tree handle"}
	}
	if treeHandle == 0 {
		// A native cancellation flag causes Tree-sitter to return a null tree
		// without a WASM call error. Prefer the caller's context/flag errors
		// before reporting the generic null-handle condition.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if p.cancellationRequested() {
			return nil, context.Canceled
		}
		if r.closedState() {
			return nil, ErrClosed
		}
		if p.timeout.Load() != 0 {
			return nil, ErrOperationLimit
		}
		return nil, &ABIError{Function: name, Message: "returned a null tree handle"}
	}
	// Runtime.Close may have been requested while the guest call was in
	// flight.  Runtime.Close waits for r.mu before tearing down the module, so
	// the handle is still safe to destroy here; returning it would otherwise
	// hand the caller a tree that is already unusable as soon as Close wins the
	// race.  `r.function` intentionally rejects a closed runtime, therefore use
	// the already-resolved export (or look it up directly) for this cleanup.
	if r.closedState() {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, ErrClosed
	}
	// A non-interruptible guest can finish just after its context is canceled.
	// Do not return a tree that the caller has already asked us to abandon.
	if ctxErr := ctx.Err(); ctxErr != nil {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, ctxErr
	}
	if p.cancellationRequested() {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, context.Canceled
	}
	// Close publishes the parser lifecycle transition before waiting for p.mu.
	// A close racing the tail of a guest call must therefore not receive a tree
	// that was produced by a parser which is already being torn down.  Keep the
	// check while p.mu and Runtime.mu are still held so Close cannot pass the
	// transition unnoticed between this check and wrapper construction.
	if p.closed.Load() {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, ErrClosed
	}
	if options != nil && options.ProgressCallback != nil {
		if options.ProgressCallback(ParseState{CurrentByteOffset: uint32(len(input))}) {
			// The guest already produced a TSTree. Release it through
			// Tree-sitter's destructor; a plain free would bypass internal node
			// cleanup and can corrupt the guest allocator.
			deleteGuestTreeWhileLocked(r, treeHandle)
			return nil, context.Canceled
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, ctxErr
	}
	if p.cancellationRequested() {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, context.Canceled
	}
	if p.closed.Load() {
		deleteGuestTreeWhileLocked(r, treeHandle)
		return nil, ErrClosed
	}
	tree := &Tree{parser: p, rt: r, source: append([]byte(nil), input...), nodes: make(map[uint32]struct{})}
	tree.handle.Store(treeHandle)
	goruntime.SetFinalizer(tree, func(tree *Tree) { _ = tree.Close() })
	return tree, nil
}

func (p *Parser) runtimeContext() context.Context {
	// The runtime is attached lazily by SetLanguage.  Read it through the
	// runtime gate rather than directly: callers commonly invoke Parse (which
	// asks for the default context) concurrently with the first SetLanguage on
	// a zero-value parser.  A direct field read here would race with setRuntime
	// and could also observe a half-published pointer.
	r := p.runtimePtr()
	if r == nil {
		return context.Background()
	}
	return r.Context()
}

// installCancellationLocked installs a native Tree-sitter cancellation flag
// for one parse and returns a function that clears and frees it.  The caller
// must hold both p.mu and p.runtime.mu; the returned cleanup function is safe
// to defer while those locks remain held.  A module without the optional
// cancellation export simply returns a nil cleanup function.
func (p *Parser) installCancellationLocked(ctx context.Context, parserHandle uint32) (func(), error) {
	if p == nil || p.runtime == nil || ctx == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	externalFlag := p.externalCancellationFlag.Load()
	compatFlagEnabled := p.compatCancellationEnabled.Load()
	// A background context has no cancellation channel. Avoid installing a
	// guest cell unless the caller explicitly opted into the compatibility
	// cancellation flag, keeping ordinary parses allocation-free apart from
	// their input buffer.
	if ctx.Done() == nil && !compatFlagEnabled && externalFlag == nil {
		return nil, nil
	}
	r := p.runtime
	var setFn interface {
		Call(context.Context, ...uint64) ([]uint64, error)
	}
	var fnName string
	for _, name := range []string{
		"tsw_parser_set_cancellation_flag",
		"wasitter_parser_set_cancellation_flag",
		"ts_parser_set_cancellation_flag",
		"parser_set_cancellation_flag",
	} {
		candidate := r.mod.ExportedFunction(name)
		if candidate == nil {
			continue
		}
		if validateIntegerSignature(candidate, 2, -1, true, false) != nil {
			continue
		}
		setFn = candidate
		fnName = name
		r.funcs[name] = candidate
		break
	}
	if setFn == nil {
		return nil, nil
	}
	// wasm32's size_t is four bytes. Reserve eight bytes so a custom module
	// compiled for a wider host ABI still has a complete cell to inspect.
	const flagBytes = uint32(8)
	ptr, err := r.allocLocked(flagBytes)
	if err != nil {
		return nil, err
	}
	mem := r.mod.Memory()
	initialFlag := uint64(0)
	if p.cancellationRequested() {
		initialFlag = 1
	}
	if mem == nil || !mem.WriteUint64Le(ptr, initialFlag) {
		r.freeLocked(ptr)
		return nil, fmt.Errorf("wasitter: cannot initialize cancellation flag")
	}
	result, callErr := setFn.Call(context.Background(), uint64(parserHandle), uint64(ptr))
	if callErr != nil {
		r.freeLocked(ptr)
		return nil, &ABIError{Function: fnName, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) != 0 && result[0] == 0 {
		r.freeLocked(ptr)
		return nil, &ABIError{Function: fnName, Message: "cancellation flag rejected"}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	// Go cannot observe a caller writing through *uintptr, so compatibility
	// flags are polled while the guest parser is running. A ticker is created
	// only when the caller opted into that API; context-only cancellation keeps
	// the existing channel-only watcher and its lower overhead.
	var poll <-chan time.Time
	var ticker *time.Ticker
	if compatFlagEnabled || externalFlag != nil {
		ticker = time.NewTicker(time.Millisecond)
		poll = ticker.C
	}
	go func() {
		defer close(done)
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				// Memory writes are safe while the parse owns Runtime.mu. The
				// cleanup closure waits for this goroutine before releasing it.
				if memory := r.mod.Memory(); memory != nil {
					_ = memory.WriteUint32Le(ptr, 1)
				}
				return
			case <-stop:
				return
			case <-poll:
				if p.cancellationRequested() {
					if memory := r.mod.Memory(); memory != nil {
						_ = memory.WriteUint32Le(ptr, 1)
					}
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			// Clear the parser's pointer before freeing the cell.  Use a
			// non-cancelable context because cleanup runs precisely when the
			// caller context may already be done.
			_, _ = setFn.Call(context.Background(), uint64(parserHandle), 0)
			r.freeLocked(ptr)
		})
	}, nil
}

// encodeRanges uses the fixed little-endian wire representation shared by
// parser/tree range accessors in the C shim.
func encodeRanges(ranges []Range) ([]byte, error) {
	if uint64(len(ranges)) > uint64(^uint32(0))/24 {
		return nil, fmt.Errorf("wasitter: range count overflows wasm32 memory")
	}
	buf := make([]byte, len(ranges)*24)
	for i, r := range ranges {
		// Byte ordering is validated by SetIncludedRanges. Point coordinates are
		// deliberately serialized verbatim: the native C API validates byte
		// ranges only, and preserving caller-provided points is important for
		// parsers that use non-UTF-8 or externally mapped source coordinates.
		o := i * 24
		putU32(buf, o, r.StartByte)
		putU32(buf, o+4, r.EndByte)
		putU32(buf, o+8, r.StartPoint.Row)
		putU32(buf, o+12, r.StartPoint.Column)
		putU32(buf, o+16, r.EndPoint.Row)
		putU32(buf, o+20, r.EndPoint.Column)
	}
	return buf, nil
}

// encodeInputEdit returns Tree-sitter's little-endian TSInputEdit wire
// representation.  Both Tree.Edit and Node.Edit use this helper so their
// lifecycle and ABI behavior stay identical.
func encodeInputEdit(edit InputEdit) []byte {
	startByte, oldEndByte, newEndByte := edit.canonicalBytes()
	startPoint, oldEndPoint, newEndPoint := edit.canonicalPoints()
	buf := make([]byte, 36)
	putU32(buf, 0, startByte)
	putU32(buf, 4, oldEndByte)
	putU32(buf, 8, newEndByte)
	putU32(buf, 12, startPoint.Row)
	putU32(buf, 16, startPoint.Column)
	putU32(buf, 20, oldEndPoint.Row)
	putU32(buf, 24, oldEndPoint.Column)
	putU32(buf, 28, newEndPoint.Row)
	putU32(buf, 32, newEndPoint.Column)
	return buf
}

// ParseReader parses all bytes read from rd.
func (p *Parser) ParseReader(rd io.Reader, oldTrees ...*Tree) (*Tree, error) {
	if rd == nil {
		return nil, fmt.Errorf("wasitter: nil reader")
	}
	// Match the callback-based entry points: reject an unusable parser before
	// invoking caller-owned code. Besides making lifecycle errors predictable,
	// this avoids draining a network/file reader when parsing cannot start.
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	var data []byte
	buf := make([]byte, 32*1024)
	zeroReads := 0
	for {
		n, err := rd.Read(buf)
		// io.Reader implementations must return a count in [0, len(buf)]. Do
		// not let a broken or adversarial implementation turn that contract
		// violation into a slice-bounds panic at the public API boundary.
		if n < 0 || n > len(buf) {
			return nil, fmt.Errorf("wasitter: invalid reader count %d", n)
		}
		if n > 0 {
			data = append(data, buf[:n]...)
			zeroReads = 0
			if uint64(len(data)) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("wasitter: input exceeds uint32 byte offset")
			}
		} else {
			zeroReads++
			if zeroReads >= 100 {
				return nil, io.ErrNoProgress
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return p.Parse(data, oldTrees...)
}

// oneOldTree validates the optional tree argument used by the convenience
// parsing methods. A variadic parameter keeps both Parse(data) and the
// upstream-shaped Parse(data, oldTree) source-compatible while still
// rejecting accidental extra arguments deterministically.
func oneOldTree(oldTrees []*Tree) (*Tree, error) {
	if len(oldTrees) > 1 {
		return nil, fmt.Errorf("wasitter: expected at most one old tree, got %d", len(oldTrees))
	}
	if len(oldTrees) == 0 {
		return nil, nil
	}
	return oldTrees[0], nil
}

// SetTimeout sets a guest parse timeout in microseconds when supported.
func (p *Parser) SetTimeout(timeout time.Duration) error {
	if timeout < 0 {
		timeout = 0
	}
	return p.SetTimeoutMicros(uint64(timeout / time.Microsecond))
}

// SetTimeoutMicros configures Tree-sitter's timeout budget.
func (p *Parser) SetTimeoutMicros(micros uint64) error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || p.handle.Load() == 0 {
		return ErrClosed
	}
	// Accept both the bridge's bool-returning convenience export and
	// Tree-sitter's canonical void-returning *_set_timeout_micros symbol. The
	// latter is what generated/native modules normally expose.
	r := p.runtime
	r.mu.Lock()
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_parser_set_timeout_micros",
		"wasitter_parser_set_timeout_micros",
		"ts_parser_set_timeout_micros",
		"parser_set_timeout_micros",
		"tsw_parser_set_timeout",
		"wasitter_parser_set_timeout",
		"ts_parser_set_timeout",
		"parser_set_timeout",
	}, []int{2}, []int{0, 1}, true, true)
	if lookupErr != nil {
		r.mu.Unlock()
		if isUnsupported(lookupErr) {
			p.timeout.Store(micros)
			return nil
		}
		return lookupErr
	}
	result, callErr := fn.Call(r.Context(), uint64(p.handle.Load()), micros)
	r.mu.Unlock()
	if callErr != nil {
		return &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) > 0 {
		// Boolean/status-returning shims conventionally use 0 for failure and
		// positive values for success. A signed negative status is also always a
		// failure. The standard void export returns no values and reaches this
		// point unchanged.
		status := int32(result[0])
		if status <= 0 {
			return &ABIError{Function: name, Message: "timeout rejected"}
		}
	}
	p.timeout.Store(micros)
	return nil
}

// Timeout returns the configured parse timeout as a duration. A zero value
// means that no timeout is configured.
func (p *Parser) Timeout() time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(p.TimeoutMicros()) * time.Microsecond
}

// TimeoutMicros returns the configured timeout.
func (p *Parser) TimeoutMicros() uint64 {
	if p == nil {
		return 0
	}
	return p.timeout.Load()
}

// Reset aborts any pending parse and resets parser state.
func (p *Parser) Reset() error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || p.handle.Load() == 0 {
		return ErrClosed
	}
	r := p.runtime
	r.mu.Lock()
	fn, name, lookupErr := lookupIntegerFunctionShapesLocked(r, []string{
		"tsw_parser_reset",
		"wasitter_parser_reset",
		"ts_parser_reset",
		"parser_reset",
	}, []int{1}, []int{0, 1}, true, true)
	if lookupErr != nil {
		r.mu.Unlock()
		if !isUnsupported(lookupErr) {
			return lookupErr
		}
		return nil
	}
	result, callErr := fn.Call(r.Context(), uint64(p.handle.Load()))
	r.mu.Unlock()
	if callErr != nil {
		return &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) > 0 && int32(result[0]) < 0 {
		return &ABIError{Function: name, Message: "parser reset rejected"}
	}
	return nil
}

// SetLogger stores a logger for clients and uses a bridge logger export when
// available. Logging callbacks are optional in the stable shim ABI. Both the
// legacy Logger (func(string, string)) and Tree-sitter's TypedLogger
// (func(LogType, string)) are accepted; unnamed functions with either shape
// are accepted as well.
func (p *Parser) SetLogger(logger any) error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed.Load() || p.handle.Load() == 0 {
		p.mu.Unlock()
		return ErrClosed
	}
	switch value := logger.(type) {
	case nil:
		p.logger = nil
	case Logger:
		if value == nil {
			p.logger = nil
		} else {
			p.logger = value
		}
	case func(string, string):
		if value == nil {
			p.logger = nil
		} else {
			p.logger = Logger(value)
		}
	case TypedLogger:
		if value == nil {
			p.logger = nil
		} else {
			p.logger = func(typ, message string) {
				value(logTypeFromString(typ), message)
			}
		}
	case func(LogType, string):
		if value == nil {
			p.logger = nil
		} else {
			p.logger = func(typ, message string) {
				value(logTypeFromString(typ), message)
			}
		}
	default:
		p.mu.Unlock()
		return fmt.Errorf("wasitter: unsupported logger type %T", logger)
	}
	p.mu.Unlock()
	return nil
}

// SetTypedLogger is an explicit spelling for callers that want the
// Tree-sitter-compatible LogType callback without relying on SetLogger's
// type-switch compatibility behavior.
func (p *Parser) SetTypedLogger(logger TypedLogger) error { return p.SetLogger(logger) }

func logTypeFromString(value string) LogType {
	if value == "lex" {
		return LogTypeLex
	}
	return LogTypeParse
}

// Logger returns the logger currently associated with the parser.
func (p *Parser) Logger() Logger {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logger
}

// Close releases the guest parser. Trees created by this parser remain valid
// until closed (Tree-sitter trees own their backing storage).
func (p *Parser) Close() error {
	if p == nil || p.closed.Swap(true) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime == nil || p.handle.Load() == 0 {
		goruntime.SetFinalizer(p, nil)
		return nil
	}
	if p.runtime.closedState() {
		p.handle.Store(0)
		goruntime.SetFinalizer(p, nil)
		return nil
	}
	handle := p.handle.Load()
	// Destruction must not inherit the parser/runtime context: callers commonly
	// close objects precisely after that context has been canceled or timed out.
	// Runtime.call would otherwise return before invoking the guest destructor,
	// leaking the parser handle until the whole module is torn down.
	_, _, err := p.runtime.call(context.Background(), []string{"tsw_parser_delete", "wasitter_parser_delete", "ts_parser_delete", "parser_delete"}, uint64(handle))
	p.handle.Store(0)
	goruntime.SetFinalizer(p, nil)
	// A runtime configured with wazero's WithCloseOnContextDone may have
	// already closed the module as part of cancellation.  In that case the
	// parser allocation is gone with the module, so destruction is idempotent
	// and should not turn a successful cleanup into an ErrClosed report.
	if errors.Is(err, ErrClosed) {
		return nil
	}
	return err
}
