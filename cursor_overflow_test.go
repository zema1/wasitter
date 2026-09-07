package wasitter

import "testing"

func TestSaturatingU32Add(t *testing.T) {
	const max = ^uint32(0)
	for _, tc := range []struct {
		name string
		a, b uint32
		want uint32
	}{
		{name: "ordinary", a: 7, b: 9, want: 16},
		{name: "exact maximum", a: max - 1, b: 1, want: max},
		{name: "overflow", a: max, b: 1, want: max},
		{name: "large overflow", a: max - 10, b: 100, want: max},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := saturatingU32Add(tc.a, tc.b); got != tc.want {
				t.Fatalf("saturatingU32Add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
