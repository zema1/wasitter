package wasitter

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
)

// ReadFunc is the callback shape used by Tree-sitter's Go bindings.  The
// stable wasitter API accepts this function directly through ParseInput;
// naming it here makes callback declarations self-documenting and eases
// migration from packages that expose the alias.
type ReadFunc func(offset uint32, point Point) []byte

// InputEncoding identifies the source encoding requested by a callback-based
// parser.  The WASM bridge normalizes all input to UTF-8 before entering the
// guest, but retaining these values keeps adapters source-compatible with
// Tree-sitter callers and allows future zero-copy encodings to be added.
type InputEncoding uint8

// Values stay in lock-step with Tree-sitter's TSInputEncoding enum; callers
// may exchange them with a custom ABI adapter.
const (
	// InputEncodingUTF8 selects UTF-8 callback input.
	InputEncodingUTF8 InputEncoding = 0
	// InputEncodingUTF16LE selects little-endian UTF-16 callback input.
	InputEncodingUTF16LE InputEncoding = 1
	// InputEncodingUTF16BE selects big-endian UTF-16 callback input.
	InputEncodingUTF16BE InputEncoding = 2
	// InputEncodingCustom is reserved for a caller-supplied Decoder.  The
	// descriptor form has no decoder field, so ParseInputSpec reports
	// ErrUnsupported for this value rather than silently treating bytes as
	// UTF-8.
	InputEncodingCustom InputEncoding = 3
	// InputEncodingUTF16 was used by an early wasitter prototype before the
	// byte-order variants were aligned with the C API.  Keep it as a distinct
	// value for source compatibility, but reject it in ParseInputSpec because a
	// byte callback cannot convey the host's UTF-16 byte order safely.
	InputEncodingUTF16 InputEncoding = 4
)

// Input describes callback-provided parser input.  ParseInput remains the
// concise function-taking API; ParseInputSpec accepts this descriptor when an
// application wants to carry the encoding alongside its callback.
type Input struct {
	Read     ReadFunc
	Encoding InputEncoding
}

// Decoder is the small host-side contract used by ParseCustomEncoding. Decode
// returns the Unicode code point and the number of source bytes consumed. A
// negative code point indicates malformed input.
type Decoder interface {
	Decode(data []byte) (codePoint int32, bytesRead uint32)
}

// CustomDecoderFunc adapts a plain function to Decoder.
type CustomDecoderFunc func(data []byte) (codePoint int32, bytesRead uint32)

// Decode calls f, or reports malformed input when f is nil.
func (f CustomDecoderFunc) Decode(data []byte) (int32, uint32) {
	if f == nil {
		return -1, 0
	}
	return f(data)
}

// This file contains small spelling/ergonomics bridges.  The core API keeps
// explicit errors and value-shaped nodes; these helpers make migration from
// the upstream Go binding and from early wasitter prototypes less noisy
// without changing the behavior of the primary methods.

// NewPoint constructs a source position.  Tree-sitter positions are bounded
// by the wasm32 ABI, so values wider than uint32 are rejected by truncation in
// the same way as the other wire-facing value types.
func NewPoint(row, column uint) Point {
	return Point{Row: uint32(row), Column: uint32(column)}
}

// ParseBytes is an explicit byte-oriented alias for Parse.
func (p *Parser) ParseBytes(input []byte, oldTrees ...*Tree) (*Tree, error) {
	return p.Parse(input, oldTrees...)
}

// ParseText parses a UTF-8 string and is an alias for ParseString.
func (p *Parser) ParseText(input string, oldTrees ...*Tree) (*Tree, error) {
	return p.ParseString(input, oldTrees...)
}

// ParseUTF16 is the historical convenience spelling.  The returned tree uses
// UTF-8 byte offsets; see ParseUTF16LE for the encoding caveat.
func (p *Parser) ParseUTF16(input []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.ParseUTF16LE(input, oldTrees...)
}

// ParseWith materializes chunks returned by a callback before parsing.  The
// callback receives a byte offset and point, matching Tree-sitter's UTF-8
// callback convention.  ParseInput is the uint32-offset variant used by the
// main API.
func (p *Parser) ParseWith(read func(offset int, point Point) []byte, oldTrees ...*Tree) (*Tree, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	oldTree, err := oneOldTree(oldTrees)
	if err != nil {
		return nil, err
	}
	return p.parseInputRead(func(offset uint32, point Point) []byte {
		return read(int(offset), point)
	}, oldTree)
}

// ParseCallback is a descriptive alias for ParseWith.
func (p *Parser) ParseCallback(read func(offset int, point Point) []byte, oldTrees ...*Tree) (*Tree, error) {
	return p.ParseWith(read, oldTrees...)
}

// ParseInputSpec is the descriptor-oriented counterpart of ParseInput.  The
// current bridge accepts UTF-8 callbacks directly; UTF-16 callbacks should use
// ParseUTF16LEWith/ParseUTF16BEWith so code units are decoded without an
// ambiguous InputEncoding value.
func (p *Parser) ParseInputSpec(input Input, oldTrees ...*Tree) (*Tree, error) {
	if input.Read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	if input.Encoding == InputEncodingUTF16 {
		// Input.Read is byte-oriented by design, so it cannot safely represent
		// UTF-16 code units. Return a typed unsupported error rather than silently
		// interpreting two-byte units as UTF-8.
		return nil, fmt.Errorf("%w: InputEncodingUTF16 requires ParseUTF16* callbacks", ErrUnsupported)
	}
	data, err := collectInputEncoding(input)
	if err != nil {
		return nil, err
	}
	return p.Parse(data, oldTrees...)
}

// utf16BytesToUTF8 decodes an even-length byte sequence into UTF-8.  A nil
// result indicates malformed byte alignment; callers should validate the
// alignment before invoking this helper when they need a diagnostic.
func utf16BytesToUTF8(raw []byte, order binary.ByteOrder) ([]byte, error) {
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("wasitter: UTF-16 input has odd byte length %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = order.Uint16(raw[i*2:])
	}
	return []byte(string(utf16.Decode(units))), nil
}

// ParseCallbackWithOptions combines ParseWith's callback form with parser
// progress options.  It is provided under a distinct name because the main
// ParseWithOptions method intentionally reserves its first argument for a
// context and a concrete byte slice.
func (p *Parser) ParseCallbackWithOptions(
	read func(offset int, point Point) []byte,
	oldTree *Tree,
	options *ParseOptions,
) (*Tree, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	all, err := collectUTF8Input(func(offset uint32, point Point) []byte {
		return read(int(offset), point)
	})
	if err != nil {
		return nil, err
	}
	return p.ParseWithOptions(p.runtimeContext(), all, oldTree, options)
}

// ParseCustomEncoding parses callback-provided text using a host decoder. The
// guest ABI is UTF-8-only, so the callback bytes are materialized and decoded
// to UTF-8 before parsing. Consequently node byte/column offsets refer to the
// decoded UTF-8 buffer; callers that need offsets in the original encoding
// should retain their own mapping. A Decoder (or CustomDecoderFunc) is
// accepted; a nil decoder treats the callback bytes as UTF-8.
//
// The method returns an error, unlike older cgo bindings whose compatibility
// method returned only *Tree. Go callers may still invoke it as a statement
// when they intentionally ignore the result.
func (p *Parser) ParseCustomEncoding(
	read func(offset int, point Point) []byte,
	oldTree *Tree,
	options *ParseOptions,
	decoder any,
) (*Tree, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	raw, err := collectUTF8Input(func(offset uint32, point Point) []byte {
		return read(int(offset), point)
	})
	if err != nil {
		return nil, err
	}
	decoded, err := decodeCustomInput(raw, decoder)
	if err != nil {
		return nil, err
	}
	return p.ParseWithOptions(p.runtimeContext(), decoded, oldTree, options)
}

func decodeCustomInput(raw []byte, decoder any) ([]byte, error) {
	if decoder == nil {
		return append([]byte(nil), raw...), nil
	}
	var decode func([]byte) (int32, uint32)
	switch value := decoder.(type) {
	case Decoder:
		// An interface containing a typed nil (for example
		// `var d CustomDecoderFunc; decoder = d`) is not itself equal to nil.
		// Avoid invoking a nil method value, and treat it the same as an
		// omitted decoder.  This also covers nil pointer receivers supplied by
		// adapters that implement Decoder on a pointer type.
		if isNilDecoder(value) {
			return append([]byte(nil), raw...), nil
		}
		decode = value.Decode
	case func([]byte) (int32, uint32):
		if value == nil {
			return append([]byte(nil), raw...), nil
		}
		decode = value
	case func([]byte) (rune, int):
		if value == nil {
			return append([]byte(nil), raw...), nil
		}
		decode = func(data []byte) (int32, uint32) {
			r, n := value(data)
			return int32(r), uint32(n)
		}
	default:
		return nil, fmt.Errorf("wasitter: decoder must implement Decoder, got %T", decoder)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	// A custom decoder is user code. Bound the number of iterations by the
	// input length so a decoder that repeatedly reports zero bytes cannot spin.
	for offset := 0; offset < len(raw); {
		codePoint, consumed := decode(raw[offset:])
		if codePoint < 0 {
			return nil, fmt.Errorf("wasitter: custom decoder rejected input at byte %d", offset)
		}
		if consumed == 0 || uint64(consumed) > uint64(len(raw)-offset) {
			return nil, fmt.Errorf("wasitter: custom decoder made invalid progress at byte %d", offset)
		}
		runeValue := rune(codePoint)
		if !utf8.ValidRune(runeValue) {
			return nil, fmt.Errorf("wasitter: custom decoder returned invalid code point U+%X at byte %d", codePoint, offset)
		}
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], runeValue)
		_, _ = out.Write(encoded[:n])
		offset += int(consumed)
		if uint64(out.Len()) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("wasitter: decoded input exceeds uint32 byte offset")
		}
	}
	return out.Bytes(), nil
}

// isNilDecoder reports whether an interface value contains a nil function,
// pointer, map, slice, channel, or interface.  Decoder is intentionally a
// small interface and callers frequently pass a named function type through
// it; a plain `value == nil` check cannot detect that typed-nil form.
func isNilDecoder(value Decoder) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// PrintDotGraphs is retained for source compatibility with go-tree-sitter.
// The compact WASM ABI does not expose Tree-sitter's FILE*/file-descriptor
// graph callback, so graph output cannot be enabled by this package.  The
// method returns ErrUnsupported instead of silently claiming that a graph was
// configured; callers that only need source compatibility may ignore the
// returned error.
func (p *Parser) PrintDotGraphs(file *os.File) error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("wasitter: nil dot-graph file")
	}
	return ErrUnsupported
}

// Debug enables parser diagnostics when the loaded bridge provides a logger
// hook. The bundled ABI intentionally has no callback trampoline, so it
// reports ErrUnsupported after validating the parser lifecycle. Callers that
// only need the upstream method for source compatibility may ignore the
// returned error.
func (p *Parser) Debug() error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	return ErrUnsupported
}

// StopPrintingDotGraphs disables parser graph output.  It is a no-op for the
// current ABI, which never enables graph output, but still validates the
// parser lifecycle like the other parser mutators.
func (p *Parser) StopPrintingDotGraphs() error {
	if err := p.ensureOpen(); err != nil {
		return err
	}
	return nil
}

// OperationLimit returns the configured parser timeout in microseconds. It is
// the naming used by older go-tree-sitter releases; TimeoutMicros is the
// preferred spelling in this package.
func (p *Parser) OperationLimit() int {
	if p == nil {
		return 0
	}
	value := p.TimeoutMicros()
	if value > uint64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(value)
}

// SetOperationLimit is the compatibility spelling of SetTimeoutMicros.
func (p *Parser) SetOperationLimit(limit int) error {
	if limit < 0 {
		limit = 0
	}
	return p.SetTimeoutMicros(uint64(limit))
}

// CancellationFlag returns a host-side flag compatible with the pointer
// exposed by the native Go binding.  Callers that use atomic.StoreUintptr on
// the returned pointer can cancel a parse in progress; the value is mirrored
// into the guest cancellation cell by ParseWithOptions.  A nil parser returns
// nil.  The pointer remains owned by Parser and must not be retained after the
// parser is discarded.
func (p *Parser) CancellationFlag() *uintptr {
	if p == nil {
		return nil
	}
	p.compatCancellationEnabled.Store(true)
	return &p.compatCancellationFlag
}

// SetCancellationFlag selects an optional caller-owned cancellation flag.  A
// non-nil pointer is sampled atomically while parsing, matching the native
// API's asynchronous cancellation contract.  Passing nil clears the external
// pointer; the Parser-owned flag returned by CancellationFlag remains usable.
// The caller must keep flag alive until the next parse completes.
func (p *Parser) SetCancellationFlag(flag *uintptr) {
	if p == nil {
		return
	}
	p.externalCancellationFlag.Store(flag)
	p.compatCancellationEnabled.Store(true)
}

// cancellationRequested reports either compatibility cancellation source.
// Callers that set a flag concurrently should use sync/atomic.StoreUintptr,
// exactly as they would with the native binding's pointer.
func (p *Parser) cancellationRequested() bool {
	if p == nil {
		return false
	}
	if atomic.LoadUintptr(&p.compatCancellationFlag) != 0 {
		return true
	}
	if flag := p.externalCancellationFlag.Load(); flag != nil {
		return atomic.LoadUintptr(flag) != 0
	}
	return false
}

// NewRuntimeFromBytes is a descriptive alias for NewRuntime.
func NewRuntimeFromBytes(ctx context.Context, wasm []byte) (*Runtime, error) {
	return NewRuntime(ctx, wasm)
}

// ParseUTF16LEWith and ParseUTF16BEWith accept chunked UTF-16 callbacks.  The
// callback offset is measured in uint16 code units, while the resulting tree
// is parsed from the decoded UTF-8 text.
func (p *Parser) ParseUTF16LEWith(read func(offset int, point Point) []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.parseUTF16With(read, oldTrees...)
}

// ParseUTF16BEWith is the big-endian-named counterpart of ParseUTF16LEWith.
// Go uint16 values have no byte order, so both methods decode the same code
// units; callers must decode external big-endian bytes before invoking it.
func (p *Parser) ParseUTF16BEWith(read func(offset int, point Point) []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.parseUTF16With(read, oldTrees...)
}

// ParseUTF16LEWithOptions is the error-returning WASM counterpart of
// go-tree-sitter's callback-plus-options API.  The callback is materialized
// before entering the guest, then the regular ParseWithOptions path applies
// progress and context handling.
func (p *Parser) ParseUTF16LEWithOptions(
	read func(offset int, point Point) []uint16,
	oldTree *Tree,
	options *ParseOptions,
) (*Tree, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	units, err := collectUTF16Input(read)
	if err != nil {
		return nil, err
	}
	return p.ParseWithOptions(p.runtimeContext(), utf8BytesFromUTF16(units), oldTree, options)
}

// ParseUTF16BEWithOptions has the same semantics as ParseUTF16LEWithOptions.
// Go uint16 values are decoded as code units; byte order must be handled by
// the caller when constructing the slice, as with ParseUTF16BE.
func (p *Parser) ParseUTF16BEWithOptions(
	read func(offset int, point Point) []uint16,
	oldTree *Tree,
	options *ParseOptions,
) (*Tree, error) {
	return p.ParseUTF16LEWithOptions(read, oldTree, options)
}

// ParseUTF16WithOptions is a concise compatibility alias for the little
// endian form.
func (p *Parser) ParseUTF16WithOptions(
	read func(offset int, point Point) []uint16,
	oldTree *Tree,
	options *ParseOptions,
) (*Tree, error) {
	return p.ParseUTF16LEWithOptions(read, oldTree, options)
}

// ParseUTF16With is the historical little-endian callback spelling. It is
// equivalent to ParseUTF16LEWith and is retained for callers migrating from
// the upstream binding.
func (p *Parser) ParseUTF16With(read func(offset int, point Point) []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.ParseUTF16LEWith(read, oldTrees...)
}

func (p *Parser) parseUTF16With(read func(offset int, point Point) []uint16, oldTrees ...*Tree) (*Tree, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	units, err := collectUTF16Input(read)
	if err != nil {
		return nil, err
	}
	return p.ParseUTF16LE(units, oldTrees...)
}

// collectUTF16Input is the UTF-16 counterpart of collectUTF8Input.  The
// callback is called with the code-unit offset immediately after the data
// already returned and must return code units beginning at that offset.  Do
// not infer whether a chunk is a duplicate from its contents: repeated UTF-16
// data is valid (for example, `aa`, then `aa` in `aaaa`) and prefix stripping
// would silently lose it.
func collectUTF16Input(read func(offset int, point Point) []uint16) ([]uint16, error) {
	var all []uint16
	point := Point{}
	const maxCalls = 1 << 20
	for calls := 0; calls < maxCalls; calls++ {
		chunk := read(len(all), point)
		if len(chunk) == 0 {
			return all, nil
		}
		// Reject an unrepresentable code-unit offset before growing the host
		// slice. This mirrors collectUTF8Input and prevents a malformed callback
		// from consuming memory only to fail after the append.
		if uint64(len(all))+uint64(len(chunk)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("wasitter: input exceeds uint32 code-unit offset")
		}
		all = append(all, chunk...)
		for _, unit := range chunk {
			if unit == '\n' {
				point.Row++
				point.Column = 0
			} else {
				point.Column++
			}
		}
	}
	return nil, fmt.Errorf("wasitter: input callback exceeded %d calls: %w", maxCalls, io.ErrNoProgress)
}

// EditPtr is the pointer-shaped counterpart to Tree.Edit.
func (t *Tree) EditPtr(edit *InputEdit) error {
	if edit == nil {
		return ErrInvalidHandle
	}
	return t.Edit(*edit)
}

// ChangedRangesE is an explicit error-suffixed alias for ChangedRanges.
func (t *Tree) ChangedRangesE(other *Tree) ([]Range, error) {
	return t.ChangedRanges(other)
}

// RootNodeWithOffsetE mirrors the naming convention of the other error-
// returning tree accessors.
func (t *Tree) RootNodeWithOffsetE(offset any, offsetPoint Point) (Node, error) {
	return t.RootNodeWithOffset(offset, offsetPoint)
}

// PrintDotGraph is retained for source compatibility with the native binding.
// Dot graph emission requires a guest-side FILE*/fd bridge that is not part of
// the stable ABI, so the operation reports ErrUnsupported.  The descriptor is
// intentionally accepted as an int to match go-tree-sitter's API.
func (t *Tree) PrintDotGraph(file int) error {
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if file < 0 {
		return fmt.Errorf("wasitter: invalid dot-graph file descriptor %d", file)
	}
	return ErrUnsupported
}

// EditPtr is the pointer-shaped counterpart to Node.Edit.
func (n Node) EditPtr(edit *InputEdit) error {
	if edit == nil {
		return ErrInvalidHandle
	}
	return n.Edit(*edit)
}

// ChildWithDescendantPtr is the pointer-shaped counterpart of
// Node.ChildWithDescendant.  It is useful when adapting code written against
// go-tree-sitter, whose Node methods use pointers, while the primary
// wasitter API intentionally uses copyable value nodes.
func (n Node) ChildWithDescendantPtr(descendant *Node) Node {
	if descendant == nil {
		return Node{}
	}
	return n.ChildWithDescendant(*descendant)
}

// ChildForFieldName and ChildForFieldId use the terminology of the
// JavaScript/Node.js Tree-sitter binding.  They are additive aliases for the
// Go-style ChildByField* methods and are useful when sharing tree-walking
// helpers across language bindings.
func (n Node) ChildForFieldName(field string) Node { return n.ChildByFieldName(field) }

// ChildForFieldId is the mixed-case compatibility spelling of
// ChildForFieldID.
func (n Node) ChildForFieldId(fieldID uint16) Node { return n.ChildByFieldId(fieldID) }

// ChildForFieldID is the initialism-friendly spelling of ChildForFieldId.
func (n Node) ChildForFieldID(fieldID uint16) Node { return n.ChildByFieldId(fieldID) }

// FirstChildForIndex is the Node.js spelling for a byte-index lookup.  Tree
// coordinates in wasitter are UTF-8 byte offsets, so this is equivalent to
// FirstChildForByte.
func (n Node) FirstChildForIndex(index uint32) Node { return n.FirstChildForByte(index) }

// FirstNamedChildForIndex is the named-node variant of FirstChildForIndex.
func (n Node) FirstNamedChildForIndex(index uint32) Node {
	return n.FirstNamedChildForByte(index)
}

// DescendantForIndex and NamedDescendantForIndex are byte-range aliases used
// by the Node.js binding.
func (n Node) DescendantForIndex(start, end uint32) Node {
	return n.DescendantForByteRange(start, end)
}

// NamedDescendantForIndex is the named-node variant of DescendantForIndex.
func (n Node) NamedDescendantForIndex(start, end uint32) Node {
	return n.NamedDescendantForByteRange(start, end)
}

// DescendantForPosition and NamedDescendantForPosition are point-range
// aliases used by the Node.js binding.
func (n Node) DescendantForPosition(start, end Point) Node {
	return n.DescendantForPointRange(start, end)
}

// NamedDescendantForPosition is the named-node variant of
// DescendantForPosition.
func (n Node) NamedDescendantForPosition(start, end Point) Node {
	return n.NamedDescendantForPointRange(start, end)
}

// ToString is the Node.js spelling for a node's S-expression serialization.
func (n Node) ToString() string { return n.ToSExpression() }

// ResetPtr is the pointer-shaped counterpart of TreeCursor.Reset. A nil node
// is rejected and leaves the cursor unchanged, matching the value API's safe
// lifecycle behavior.
func (c *TreeCursor) ResetPtr(node *Node) {
	if node == nil {
		return
	}
	c.Reset(*node)
}
