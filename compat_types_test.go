package wasitter

import (
	"encoding/binary"
	"testing"
)

func TestInputEditByteOffsetsEncode(t *testing.T) {
	edit := InputEdit{
		StartByte:   3,
		OldEndByte:  5,
		NewEndByte:  7,
		StartPoint:  Point{Row: 1, Column: 3},
		OldEndPoint: Point{Row: 1, Column: 5},
		NewEndPoint: Point{Row: 1, Column: 7},
	}
	encoded := encodeInputEdit(edit)
	for i, want := range []uint32{3, 5, 7} {
		if got := binary.LittleEndian.Uint32(encoded[i*4:]); got != want {
			t.Fatalf("wire byte %d = %d, want %d", i, got, want)
		}
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
