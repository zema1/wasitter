package comparison

import (
	"fmt"
	"reflect"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	wasm "github.com/zema1/wasitter"
)

// QueryMatches/QueryCaptures in the upstream binding are lazy views over a
// native cursor. The public wasitter values retain slice semantics, so the
// range setters replay a private shadow cursor when a caller has already
// consumed part of the materialized view. Keep this small parity matrix here
// because rooted patterns expose the in-progress-state behavior that a simple
// capture-only filter misses.
func TestQueryIteratorRangeMutationParity(t *testing.T) {
	// Keep this fixture multi-line so the point-range cases exercise the same
	// row/column filtering logic as native Tree-sitter. A single-line source
	// would make the old Row=1..4 range vacuously empty on both sides.
	source := []byte("{\n  \"items\": [1, 2, 3, {\"a\": 4, \"b\": 5}, [6, 7], 8, 9],\n  \"tail\": true\n}\n")
	queries := []string{`(number) @n`, `(array (_) @x)`}
	type rangeCase struct {
		name       string
		byteStart  uint32
		byteEnd    uint32
		pointStart wasm.Point
		pointEnd   wasm.Point
		point      bool
	}
	ranges := []rangeCase{
		{name: "byte", byteStart: 1, byteEnd: 4},
		{name: "point", point: true, pointStart: wasm.Point{Row: 1, Column: 0}, pointEnd: wasm.Point{Row: 3, Column: 0}},
	}
	for _, querySource := range queries {
		for _, mode := range []string{"matches", "captures"} {
			for _, r := range ranges {
				for _, consumed := range []int{0, 2, 100} {
					name := fmt.Sprintf("%s/%s/%s/n%d", querySource, mode, r.name, consumed)
					t.Run(name, func(t *testing.T) {
						wasmParser, _ := newWASM(t)
						nativeParser, nativeLanguage := newNative(t)
						wasmTree, err := wasmParser.Parse(source, nil)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = wasmTree.Close() })
						nativeTree := nativeParser.Parse(source, nil)
						if nativeTree == nil {
							t.Fatal("native parse returned nil")
						}
						t.Cleanup(nativeTree.Close)
						wasmQuery, wasmErr := wasm.NewQuery(wasmTree.Language(), querySource)
						nativeQuery, nativeErr := native.NewQuery(nativeLanguage, querySource)
						if (wasmErr == nil) != (nativeErr == nil) {
							t.Fatalf("compile mismatch wasm=%v native=%v", wasmErr, nativeErr)
						}
						if wasmErr != nil {
							return
						}
						t.Cleanup(func() { _ = wasmQuery.Close() })
						t.Cleanup(nativeQuery.Close)
						wasmCursor, nativeCursor := wasm.NewQueryCursor(), native.NewQueryCursor()
						t.Cleanup(func() { _ = wasmCursor.Close() })
						t.Cleanup(nativeCursor.Close)

						if mode == "matches" {
							wasmMatches := wasmCursor.Matches(wasmQuery, wasmTree.RootNode(), source)
							nativeMatches := nativeCursor.Matches(nativeQuery, nativeTree.RootNode(), source)
							for i := 0; i < consumed; i++ {
								if wasmMatches.Next() == nil || nativeMatches.Next() == nil {
									break
								}
							}
							if r.point {
								wasmMatches.SetPointRange(r.pointStart, r.pointEnd)
								nativeMatches.SetPointRange(native.Point{Row: uint(r.pointStart.Row), Column: uint(r.pointStart.Column)}, native.Point{Row: uint(r.pointEnd.Row), Column: uint(r.pointEnd.Column)})
							} else {
								wasmMatches.SetByteRange(r.byteStart, r.byteEnd)
								nativeMatches.SetByteRange(uint(r.byteStart), uint(r.byteEnd))
							}
							got := wasmMatchViews(wasmMatches, source)
							want := nativeMatchViews(nativeMatches, source)
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("matches differ: wasm=%#v native=%#v", got, want)
							}
							return
						}

						wasmCaptures := wasmCursor.Captures(wasmQuery, wasmTree.RootNode(), source)
						nativeCaptures := nativeCursor.Captures(nativeQuery, nativeTree.RootNode(), source)
						for i := 0; i < consumed; i++ {
							_, _ = wasmCaptures.Next()
							_, _ = nativeCaptures.Next()
						}
						if r.point {
							wasmCaptures.SetPointRange(r.pointStart, r.pointEnd)
							nativeCaptures.SetPointRange(native.Point{Row: uint(r.pointStart.Row), Column: uint(r.pointStart.Column)}, native.Point{Row: uint(r.pointEnd.Row), Column: uint(r.pointEnd.Column)})
						} else {
							wasmCaptures.SetByteRange(r.byteStart, r.byteEnd)
							nativeCaptures.SetByteRange(uint(r.byteStart), uint(r.byteEnd))
						}
						got := flattenWasmCaptures(wasmCaptures, source)
						want := flattenNativeCaptures(nativeCaptures, source)
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("captures differ: wasm=%#v native=%#v", got, want)
						}
					})
				}
			}
		}
	}
}

func flattenWasmCaptures(captures wasm.QueryCaptures, source []byte) []captureView {
	var out []captureView
	for match, ordinal := captures.Next(); match != nil; match, ordinal = captures.Next() {
		if ordinal >= uint(len(match.Captures)) {
			continue
		}
		capture := match.Captures[ordinal]
		out = append(out, captureView{Index: capture.Index, Type: capture.Node.Type(), Start: capture.Node.StartByte(), End: capture.Node.EndByte(), Text: capture.Node.Utf8Text(source)})
	}
	return out
}

func flattenNativeCaptures(captures native.QueryCaptures, source []byte) []captureView {
	var out []captureView
	for match, ordinal := captures.Next(); match != nil; match, ordinal = captures.Next() {
		if ordinal >= uint(len(match.Captures)) {
			continue
		}
		capture := match.Captures[ordinal]
		out = append(out, captureView{Index: capture.Index, Type: capture.Node.Kind(), Start: uint32(capture.Node.StartByte()), End: uint32(capture.Node.EndByte()), Text: capture.Node.Utf8Text(source)})
	}
	return out
}
