package wasitter

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
