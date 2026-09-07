package wasitter

// Quantifier is the name used by the older smacker/go-tree-sitter binding for
// a query capture quantifier.  Keep it as an alias so values can be passed to
// APIs using either spelling without conversions.
type Quantifier = CaptureQuantifier

const (
	// QuantifierZero indicates that a capture does not occur.
	QuantifierZero = CaptureQuantifierZero
	// QuantifierZeroOrOne indicates an optional capture.
	QuantifierZeroOrOne = CaptureQuantifierZeroOrOne
	// QuantifierZeroOrMore indicates a capture repeated any number of times.
	QuantifierZeroOrMore = CaptureQuantifierZeroOrMore
	// QuantifierOne indicates exactly one capture.
	QuantifierOne = CaptureQuantifierOne
	// QuantifierOneOrMore indicates a capture repeated at least once.
	QuantifierOneOrMore = CaptureQuantifierOneOrMore
)

// String implements fmt.Stringer for symbol classifications.  The textual
// names follow Tree-sitter's C enum names and are useful in diagnostics and
// grammar-inspection tools.
func (s SymbolType) String() string {
	switch s {
	case SymbolTypeRegular:
		return "Regular"
	case SymbolTypeAnonymous:
		return "Anonymous"
	case SymbolTypeSupertype:
		return "Supertype"
	case SymbolTypeAuxiliary:
		return "Auxiliary"
	default:
		return "Unknown"
	}
}
