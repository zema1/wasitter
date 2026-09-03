package sitterwasm

import "fmt"

// Point identifies a position in a source document. Row and Column are zero
// based. Column is measured in UTF-8 bytes, matching Tree-sitter's API.
type Point struct {
	Row    uint32
	Column uint32
}

// Symbol is the numeric grammar-symbol identifier used by Tree-sitter.
// Tree-sitter's C ABI defines this as a 16-bit value.  Keeping a named Go
// type makes APIs that exchange symbol ids self-documenting while remaining
// assignment-compatible with the uint16 values returned by this package.
type Symbol = uint16

// Range describes a source range in both byte and point coordinates.
type Range struct {
	StartByte  uint32
	EndByte    uint32
	StartPoint Point
	EndPoint   Point
}

// InputEdit describes a change to a source document. It is the Go equivalent
// of Tree-sitter's TSInputEdit.
type InputEdit struct {
	StartByte   uint32
	OldEndByte  uint32
	NewEndByte  uint32
	StartPoint  Point
	OldEndPoint Point
	NewEndPoint Point
	// The upstream go-tree-sitter binding calls the three position fields
	// StartPosition/OldEndPosition/NewEndPosition.  Keep those spellings as
	// compatibility aliases while retaining the shorter Point names used by
	// sitterwasm.  When both forms are populated, the Point field wins; the
	// position alias is used only when its corresponding Point is zero.  This
	// permits keyed struct literals written for either API to pass through the
	// same wire encoder without changing the wasm32 byte/point representation.
	StartPosition  Point
	OldEndPosition Point
	NewEndPosition Point
	// StartIndex/OldEndIndex/NewEndIndex are the spellings used by the older
	// smacker/go-tree-sitter binding.  They are aliases of the byte offsets
	// above and are accepted by the wire encoder when the corresponding
	// *Byte field is left at its zero value.  A zero byte offset is valid, so
	// callers that need to distinguish an explicitly populated zero from an
	// omitted field should use the canonical *Byte spelling.
	StartIndex  uint32
	OldEndIndex uint32
	NewEndIndex uint32
}

// inputEditValue normalizes the value- and pointer-shaped edit forms exposed
// by the native Go bindings.  The primary sitterwasm API uses a value so an
// edit can be passed without an allocation, while upstream callers commonly
// keep an *InputEdit and pass its address to Tree.Edit/Node.Edit.  Keeping the
// normalization in one place prevents the two entry points from drifting.
func inputEditValue(value any) (InputEdit, error) {
	switch edit := value.(type) {
	case InputEdit:
		return edit, nil
	case *InputEdit:
		if edit == nil {
			return InputEdit{}, ErrInvalidHandle
		}
		return *edit, nil
	default:
		return InputEdit{}, fmt.Errorf("sitterwasm: edit must be InputEdit or *InputEdit, got %T", value)
	}
}

// uint32Arg converts the integer forms commonly used by Tree-sitter bindings
// to the fixed-width wasm32 representation.  Negative and overflowing values
// are rejected instead of wrapping to a valid-looking offset.
func uint32Arg(value any) (uint32, bool) {
	switch v := value.(type) {
	case uint32:
		return v, true
	case uint:
		if uint64(v) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(v), true
	case uint64:
		if v > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(v), true
	case uint16:
		return uint32(v), true
	case uint8:
		return uint32(v), true
	case int:
		if v < 0 || uint64(v) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(v), true
	case int64:
		if v < 0 || uint64(v) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(v), true
	case int32:
		if v < 0 {
			return 0, false
		}
		return uint32(v), true
	case int16:
		if v < 0 {
			return 0, false
		}
		return uint32(v), true
	case int8:
		if v < 0 {
			return 0, false
		}
		return uint32(v), true
	default:
		return 0, false
	}
}

// EditInput is the historical name used by smacker/go-tree-sitter.  It is an
// alias rather than a second representation so edits can be passed directly
// to Tree.Edit/Node.Edit without conversion.
type EditInput = InputEdit

// Edit is kept as a short, idiomatic alias for InputEdit.
type Edit = InputEdit

// canonicalPoints returns the wire-facing point triplet for an edit.  The
// shorter *Point fields are the native sitterwasm spelling; the
// *Position fields are compatibility aliases for go-tree-sitter callers.  A
// zero Point is a valid source position, so this helper can only distinguish
// the common keyed-literal case (where one spelling is left at its zero value).
// If both spellings are non-zero, the explicit Point value is authoritative.
func (e InputEdit) canonicalPoints() (start, oldEnd, newEnd Point) {
	start, oldEnd, newEnd = e.StartPoint, e.OldEndPoint, e.NewEndPoint
	if start == (Point{}) && e.StartPosition != (Point{}) {
		start = e.StartPosition
	}
	if oldEnd == (Point{}) && e.OldEndPosition != (Point{}) {
		oldEnd = e.OldEndPosition
	}
	if newEnd == (Point{}) && e.NewEndPosition != (Point{}) {
		newEnd = e.NewEndPosition
	}
	return start, oldEnd, newEnd
}

// canonicalBytes returns the wire-facing byte offsets for an edit.  The
// modern *Byte fields are authoritative when non-zero; compatibility
// *Index fields fill in omitted values for keyed literals written against the
// older binding.
func (e InputEdit) canonicalBytes() (start, oldEnd, newEnd uint32) {
	start, oldEnd, newEnd = e.StartByte, e.OldEndByte, e.NewEndByte
	if start == 0 && e.StartIndex != 0 {
		start = e.StartIndex
	}
	if oldEnd == 0 && e.OldEndIndex != 0 {
		oldEnd = e.OldEndIndex
	}
	if newEnd == 0 && e.NewEndIndex != 0 {
		newEnd = e.NewEndIndex
	}
	return start, oldEnd, newEnd
}

// Logger receives parser diagnostics. Type is the historical string spelling
// ("parse" or "lex") retained for source compatibility with early
// sitterwasm releases.
type Logger func(typ, message string)

// TypedLogger is the Tree-sitter-compatible logger shape. SetLogger accepts
// both Logger and TypedLogger (as well as their unnamed function equivalents)
// so code can migrate without an adapter; Logger remains available because it
// was part of the initial public API.
type TypedLogger func(typ LogType, message string)

// LogType identifies the source of a parser log message.
type LogType uint8

const (
	// LogTypeParse identifies parser-state diagnostics.
	LogTypeParse LogType = iota
	// LogTypeLex identifies lexer diagnostics.
	LogTypeLex
)

// ParseState is supplied to a progress callback, when supported by the shim.
type ParseState struct {
	CurrentByteOffset uint32
	HasError          bool
}

// ParseOptions controls optional parsing behavior.
type ParseOptions struct {
	ProgressCallback func(ParseState) bool
}
