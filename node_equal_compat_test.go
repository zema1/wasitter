package sitterwasm_test

import (
	"bytes"
	"context"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// hideWASMExport renames an export in-place while preserving the string
// length.  This lets compatibility tests exercise the Go fallback against the
// real bundled runtime without maintaining a second fixture module.
func hideWASMExport(t *testing.T, wasm []byte, name, replacement string) []byte {
	t.Helper()
	if len(name) != len(replacement) {
		t.Fatalf("replacement %q has length %d, want %d", replacement, len(replacement), len(name))
	}
	// A WebAssembly binary may contain the export name both in the export
	// section and in the optional linker/name section.  Replace one occurrence
	// per call so callers can deliberately hide both copies when they need to
	// exercise export discovery against a minimally stripped fixture.
	if got := bytes.Count(wasm, []byte(name)); got == 0 {
		t.Fatalf("export %q does not occur in fixture", name)
	}
	return bytes.Replace(wasm, []byte(name), []byte(replacement), 1)
}

func parseWithWASM(t *testing.T, wasm []byte) *sitterwasm.Tree {
	t.Helper()
	rt, err := sitterwasm.NewRuntime(context.Background(), wasm)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	lang, err := rt.LoadLanguage("json")
	if err != nil {
		rt.Close()
		t.Fatalf("LoadLanguage: %v", err)
	}
	p, err := sitterwasm.NewParserWithRuntime(rt)
	if err != nil {
		lang.Close()
		rt.Close()
		t.Fatalf("NewParserWithRuntime: %v", err)
	}
	if err := p.SetLanguage(lang); err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatalf("SetLanguage: %v", err)
	}
	tree, err := p.Parse([]byte(`[1, 1]`), nil)
	if err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatalf("Parse: %v", err)
	}
	t.Cleanup(func() {
		tree.Close()
		p.Close()
		lang.Close()
		rt.Close()
	})
	return tree
}

func TestNodeEqualUsesStableIDWithoutEqualityExport(t *testing.T) {
	wasm := hideWASMExport(t, sitterwasm.BuiltinJSONWASM(), "tsw_node_eq", "old_node_eq")
	tree := parseWithWASM(t, wasm)
	array := tree.RootNode().NamedChild(0)
	first := array.NamedChild(0)
	secondWrapper := tree.RootNode().NamedChild(0).NamedChild(0)
	if first.Handle() == secondWrapper.Handle() {
		t.Fatal("fixture unexpectedly reused a node wrapper handle")
	}
	if !first.Equal(secondWrapper) {
		t.Fatal("Equal returned false for identical nodes when eq export is absent")
	}
	if first.Equal(array.NamedChild(1)) {
		t.Fatal("Equal returned true for distinct nodes with the same type")
	}
}

func TestNodeEqualUsesRangeMetadataWithoutIDOrEqualityExports(t *testing.T) {
	wasm := sitterwasm.BuiltinJSONWASM()
	wasm = hideWASMExport(t, wasm, "tsw_node_eq", "old_node_eq")
	wasm = hideWASMExport(t, wasm, "tsw_node_id", "old_node_id")
	tree := parseWithWASM(t, wasm)
	array := tree.RootNode().NamedChild(0)
	first := array.NamedChild(0)
	secondWrapper := tree.RootNode().NamedChild(0).NamedChild(0)
	if !first.Equal(secondWrapper) {
		t.Fatal("Equal returned false for identical nodes with legacy metadata fallback")
	}
	if first.Equal(array.NamedChild(1)) {
		t.Fatal("metadata fallback conflated distinct same-type nodes")
	}
}

// Equal must not pass a wrapper reclaimed by a concurrently/previously closed
// foreign tree into the guest. The operation is value-shaped, so a node from a
// closed tree simply compares unequal to a live node.
func TestNodeEqualClosedForeignTreeIsFalse(t *testing.T) {
	p, rt := newJSONParser(t)
	first, err := p.Parse([]byte(`[1]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Parse([]byte(`[1]`), nil)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	left := first.RootNode()
	right := second.RootNode()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if left.Equal(right) || right.Equal(left) {
		t.Fatal("Equal reported equality with a node from a closed foreign tree")
	}
	second.Close()
	p.Close()
	rt.Close()
}
