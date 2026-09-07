package wasitter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
)

// ReadFunc returns source bytes beginning at offset. Return an empty slice at
// end of input. Offsets count bytes; point columns count UTF-8 bytes for UTF-8
// input or UTF-16 code units for UTF-16 input. The parser copies each chunk.
type ReadFunc func(offset uint32, point Point) []byte

// InputEncoding identifies the byte encoding supplied by [Input.Read].
// Input is decoded to UTF-8 before entering the guest.
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
	// descriptor form has no decoder field, so ParseInput reports
	// ErrUnsupported for this value rather than silently treating bytes as
	// UTF-8.
	InputEncodingCustom InputEncoding = 3
)

// Input describes callback-provided parser input. The zero encoding is UTF-8.
// Use [Parser.ParseInputContext] to parse it with an explicit context.
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

// NewPoint constructs a zero-based position. Row counts lines and column
// counts UTF-8 bytes within the line.
func NewPoint(row, column uint32) Point {
	return Point{Row: uint32(row), Column: uint32(column)}
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

// ParseCallbackWithOptions collects UTF-8 callback input and parses it
// with progress options using the runtime's context. Offsets are byte offsets.
// For an explicit context or byte-oriented UTF-16 input, use
// [Parser.ParseInputWithOptionsContext].
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
	return p.ParseWithOptionsContext(p.runtimeContext(), all, oldTree, options)
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
	decoder Decoder,
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
	return p.ParseWithOptionsContext(p.runtimeContext(), decoded, oldTree, options)
}

func decodeCustomInput(raw []byte, decoder Decoder) ([]byte, error) {
	if decoder == nil {
		return append([]byte(nil), raw...), nil
	}
	if isNilDecoder(decoder) {
		return append([]byte(nil), raw...), nil
	}
	decode := decoder.Decode
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

// CancellationFlag returns a host-side flag compatible with the pointer
// exposed by the native Go binding.  Callers that use atomic.StoreUintptr on
// the returned pointer can cancel a parse in progress; the value is mirrored
// into the guest cancellation cell by ParseWithOptionsContext.  A nil parser returns
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

// ParseUTF16With collects UTF-16 code units from read before parsing.
// Callback offsets and columns count code units. Resulting tree offsets and
// columns refer to the decoded UTF-8 source. Go uint16 values have no byte order.
func (p *Parser) ParseUTF16With(read func(offset int, point Point) []uint16, oldTrees ...*Tree) (*Tree, error) {
	return p.parseUTF16With(read, oldTrees...)
}

// ParseUTF16WithOptions collects UTF-16 code units and parses them using
// the runtime's context and progress options. See [Parser.ParseUTF16With] for
// coordinate units.
func (p *Parser) ParseUTF16WithOptions(
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
	return p.ParseWithOptionsContext(p.runtimeContext(), utf8BytesFromUTF16(units), oldTree, options)
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
	return p.ParseUTF16(units, oldTrees...)
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
