package sitterwasm

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestPointAndRangeUseUTF8ByteCoordinates(t *testing.T) {
	// Tree-sitter columns are byte columns, rather than rune counts.  Keeping
	// this as a package-level contract prevents an accidental conversion in a
	// future convenience API.
	if got := len([]byte("猫")); got != 3 {
		t.Fatalf("test precondition: UTF-8 width = %d, want 3", got)
	}

	r := Range{
		StartByte:  0,
		EndByte:    5,
		StartPoint: Point{Row: 0, Column: 0},
		EndPoint:   Point{Row: 1, Column: 3},
	}
	if r.StartByte != 0 || r.EndByte != 5 {
		t.Fatalf("unexpected byte range: %#v", r)
	}
	if r.EndPoint.Row != 1 || r.EndPoint.Column != 3 {
		t.Fatalf("unexpected point range: %#v", r)
	}
}

func TestEditIsInputEditAlias(t *testing.T) {
	var edit Edit = InputEdit{
		StartByte:   2,
		OldEndByte:  4,
		NewEndByte:  6,
		StartPoint:  Point{Row: 1, Column: 2},
		OldEndPoint: Point{Row: 1, Column: 4},
		NewEndPoint: Point{Row: 1, Column: 6},
	}
	var input InputEdit = edit
	if input.StartByte != 2 || input.OldEndByte != 4 || input.NewEndByte != 6 {
		t.Fatalf("Edit and InputEdit do not preserve fields: %#v", input)
	}
}

func TestInputEditPositionAliasesEncode(t *testing.T) {
	// Upstream callers use the *Position field names.  Verify that the aliases
	// reach the exact TSInputEdit wire slots used by Tree.Edit/Node.Edit.
	edit := InputEdit{
		StartByte:      7,
		OldEndByte:     9,
		NewEndByte:     11,
		StartPosition:  Point{Row: 2, Column: 3},
		OldEndPosition: Point{Row: 2, Column: 5},
		NewEndPosition: Point{Row: 2, Column: 7},
	}
	encoded := encodeInputEdit(edit)
	if len(encoded) != 36 {
		t.Fatalf("encoded edit length = %d, want 36", len(encoded))
	}
	want := []uint32{7, 9, 11, 2, 3, 2, 5, 2, 7}
	for i, value := range want {
		if got := binary.LittleEndian.Uint32(encoded[i*4:]); got != value {
			t.Errorf("wire word %d = %d, want %d", i, got, value)
		}
	}

	// Explicit Point fields remain authoritative when both spellings are
	// supplied, which makes conflicting compatibility literals deterministic.
	canonical := edit
	canonical.StartPoint = Point{Row: 8, Column: 9}
	canonical.OldEndPoint = Point{Row: 8, Column: 10}
	canonical.NewEndPoint = Point{Row: 8, Column: 12}
	encoded = encodeInputEdit(canonical)
	if got := binary.LittleEndian.Uint32(encoded[12:]); got != 8 {
		t.Errorf("canonical start row = %d, want 8", got)
	}
	if got := binary.LittleEndian.Uint32(encoded[16:]); got != 9 {
		t.Errorf("canonical start column = %d, want 9", got)
	}
}

func TestLogTypeValuesAreStable(t *testing.T) {
	if LogTypeParse != 0 || LogTypeLex != 1 {
		t.Fatalf("unexpected log type values: parse=%d lex=%d", LogTypeParse, LogTypeLex)
	}
}

func TestSentinelErrors(t *testing.T) {
	for name, err := range map[string]error{
		"closed":         ErrClosed,
		"no runtime":     ErrNoRuntime,
		"no language":    ErrNoLanguage,
		"unsupported":    ErrUnsupported,
		"invalid handle": ErrInvalidHandle,
	} {
		if err == nil {
			t.Errorf("%s sentinel is nil", name)
		}
	}
	if !errors.Is(ErrClosed, ErrClosed) {
		t.Fatal("errors.Is must recognize sentinel identity")
	}
}

func TestABIErrorFormatting(t *testing.T) {
	tests := []struct {
		name string
		err  *ABIError
		want string
	}{
		{
			name: "with message",
			err:  &ABIError{Function: "st_parse", Message: "invalid input"},
			want: "sitterwasm: st_parse: invalid input",
		},
		{
			name: "without message",
			err:  &ABIError{Function: "st_parse"},
			want: "sitterwasm: st_parse failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
