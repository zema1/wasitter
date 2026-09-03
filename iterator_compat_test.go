package sitterwasm_test

import (
	"io"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// Iterators should remain usable with legacy bridges that expose child
// navigation but do not export the optional tsw_node_is_null predicate.
func TestIteratorWorksWithoutNodeIsNullExport(t *testing.T) {
	wasm := hideWASMExport(t, sitterwasm.BuiltinJSONWASM(), "tsw_node_is_null", "old_node_is_null")
	// Hide the native cursor constructor as well so the fixture exercises the
	// value-style traversal path used by old bridges.
	wasm = hideWASMExport(t, wasm, "tsw_cursor_new", "old_cursor_new")
	wasm = hideWASMExport(t, wasm, "tsw_tree_cursor_new", "old_tree_cursor_new")
	tree := parseWithWASM(t, wasm)
	it := sitterwasm.NewIterator(tree.RootNode(), sitterwasm.DFSMode)
	if it == nil {
		t.Fatal("NewIterator returned nil")
	}
	var got []string
	for {
		node, err := it.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("iterator Next: %v", err)
		}
		got = append(got, node.Type())
	}
	if len(got) == 0 || got[0] != "document" {
		t.Fatalf("iterator yielded %v, want document root", got)
	}
}
