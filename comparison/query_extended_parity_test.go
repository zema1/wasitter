package comparison

import (
	"fmt"
	"reflect"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	wasm "github.com/zema1/sitterwasm"
)

func TestExtendedQueryParity(t *testing.T) {
	cases := []struct {
		name    string
		source  []byte
		queries []string
	}{
		{
			name:   "patterns",
			source: []byte(`{"a":[1,2,3],"b":{"x":true,"y":"foo"},"z":null}`),
			queries: []string{
				`(number) @n`,
				`(string) @s`,
				`(pair key: (string) @k value: (_) @v)`,
				`[(number) (string)] @x`,
				`(_) @x`,
				`((number) @n (#eq? @n "2"))`,
				`((number) @n (#not-eq? @n "2"))`,
				`((number) @n (#match? @n "^[23]$"))`,
				`((number) @n (#any-of? @n "1" "3"))`,
				`((number) @n (#any-eq? @n "2"))`,
				`((number) @n (#any-not-eq? @n "2"))`,
				`((number) @n (#set! foo bar))`,
				`((number) @n (#is? foo))`,
				`(number) @n (string) @s`,
				`(number) @n (#foo? @n "x")`,
				`(number)+ @n`,
				`(number) @n+`,
				`(number) @n?`,
			},
		},
		{
			name:   "predicates",
			source: []byte(`[{"a":1,"b":1},{"a":2,"b":3},1,2,3,"1","x"]`),
			queries: []string{
				`((number) @n (#eq? @n "1"))`,
				`((number) @n (#not-eq? @n "1"))`,
				`((number) @n (#match? @n "^[13]$"))`,
				`((number) @n (#not-match? @n "^[13]$"))`,
				`((number) @n (#any-eq? @n "1"))`,
				`((number) @n (#any-not-eq? @n "1"))`,
				`((number) @n (#any-match? @n "^[13]$"))`,
				`((number) @n (#any-not-match? @n "^[13]$"))`,
				`((number) @n (#any-of? @n "1" "3"))`,
				`((number) @n (#not-any-of? @n "1" "3"))`,
				`((number) @n (#eq? @n @n))`,
				`((number) @n (#not-eq? @n @n))`,
				`((number) @n (#any-eq? @n @n))`,
				`((number) @n (#any-not-eq? @n @n))`,
				`((pair key: (string) @k value: (_) @v) (#eq? @k "a"))`,
				`((pair key: (string) @k value: (_) @v) (#match? @k "^[ab]$"))`,
				`((pair key: (string) @k value: (_) @v) (#any-of? @k "a" "b"))`,
				`((array (number) @n)+ (#eq? @n "1"))`,
				`((array (number) @n)+ (#any-eq? @n "1"))`,
				`((array (number) @n)+ (#not-eq? @n "1"))`,
				`((array (number) @n)+ (#not-any-of? @n "1"))`,
			},
		},
	}

	wasmParser, _ := newWASM(t)
	nativeParser, nativeLanguage := newNative(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wasmTree, err := wasmParser.Parse(tc.source, nil)
			if err != nil {
				t.Fatalf("WASM parse: %v", err)
			}
			t.Cleanup(func() { _ = wasmTree.Close() })
			nativeTree := nativeParser.Parse(tc.source, nil)
			if nativeTree == nil {
				t.Fatal("native parse returned nil")
			}
			t.Cleanup(nativeTree.Close)

			for i, querySource := range tc.queries {
				t.Run(fmt.Sprintf("query-%02d", i), func(t *testing.T) {
					assertQueryParity(t, wasmTree, nativeTree, nativeLanguage, tc.source, querySource)
				})
			}
		})
	}
}

func assertQueryParity(
	t *testing.T,
	wasmTree *wasm.Tree,
	nativeTree *native.Tree,
	nativeLanguage *native.Language,
	source []byte,
	querySource string,
) {
	t.Helper()
	wasmQuery, wasmErr := wasm.NewQuery(wasmTree.Language(), querySource)
	nativeQuery, nativeErr := native.NewQuery(nativeLanguage, querySource)
	if (wasmErr == nil) != (nativeErr == nil) {
		t.Fatalf("query %q compile result differs: WASM=%v native=%v", querySource, wasmErr, nativeErr)
	}
	if wasmErr != nil {
		return
	}
	t.Cleanup(func() { _ = wasmQuery.Close() })
	t.Cleanup(nativeQuery.Close)

	wasmCursor := wasm.NewQueryCursor()
	nativeCursor := native.NewQueryCursor()
	t.Cleanup(func() { _ = wasmCursor.Close() })
	t.Cleanup(nativeCursor.Close)
	got := wasmMatchViews(wasmCursor.Matches(wasmQuery, wasmTree.RootNode(), source), source)
	want := nativeMatchViews(nativeCursor.Matches(nativeQuery, nativeTree.RootNode(), source), source)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query %q matches differ:\nWASM:   %#v\nNative: %#v", querySource, got, want)
	}
}
