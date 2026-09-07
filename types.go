package wasitter

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
}

// canonicalPoints returns the edit's source coordinates.
func (e InputEdit) canonicalPoints() (Point, Point, Point) {
	return e.StartPoint, e.OldEndPoint, e.NewEndPoint
}

// canonicalBytes returns the edit's source offsets.
func (e InputEdit) canonicalBytes() (uint32, uint32, uint32) {
	return e.StartByte, e.OldEndByte, e.NewEndByte
}

// Logger receives parser diagnostics. See [Parser.SetLogger] for bridge support.
type Logger func(typ LogType, message string)

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
