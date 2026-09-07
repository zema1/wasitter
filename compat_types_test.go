package wasitter

import (
	"encoding/binary"
	"testing"
)

func TestLegacyEditInputIndexAliasesEncode(t *testing.T) {
	edit := EditInput{
		StartIndex:     3,
		OldEndIndex:    5,
		NewEndIndex:    7,
		StartPosition:  Point{Row: 1, Column: 3},
		OldEndPosition: Point{Row: 1, Column: 5},
		NewEndPosition: Point{Row: 1, Column: 7},
	}
	encoded := encodeInputEdit(edit)
	for i, want := range []uint32{3, 5, 7} {
		if got := binary.LittleEndian.Uint32(encoded[i*4:]); got != want {
			t.Fatalf("wire byte %d = %d, want %d", i, got, want)
		}
	}
	var symbol Symbol = 42
	if symbol != 42 {
		t.Fatalf("Symbol alias = %d", symbol)
	}
	var quantifier Quantifier = QuantifierOne
	if quantifier != CaptureQuantifierOne {
		t.Fatalf("Quantifier alias = %d", quantifier)
	}
}

func TestSymbolTypeString(t *testing.T) {
	if got := SymbolTypeRegular.String(); got != "Regular" {
		t.Fatalf("regular String = %q", got)
	}
	if got := SymbolType(255).String(); got != "Unknown" {
		t.Fatalf("unknown String = %q", got)
	}
}
