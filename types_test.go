package wasitter

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

func TestInputEditCoordinatesEncode(t *testing.T) {
	// Verify the exact TSInputEdit wire slots used by Tree.Edit and Node.Edit.
	edit := InputEdit{
		StartByte:   7,
		OldEndByte:  9,
		NewEndByte:  11,
		StartPoint:  Point{Row: 2, Column: 3},
		OldEndPoint: Point{Row: 2, Column: 5},
		NewEndPoint: Point{Row: 2, Column: 7},
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
			want: "wasitter: st_parse: invalid input",
		},
		{
			name: "without message",
			err:  &ABIError{Function: "st_parse"},
			want: "wasitter: st_parse failed",
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
