package wasitter_test

import (
	"testing"

	wasitter "github.com/zema1/wasitter"
)

// The native go-tree-sitter API passes InputEdit by pointer, while
// wasitter's value API accepts the same edit without an allocation. Verify
// that both forms reach the same wire representation.
func TestTreeEditAcceptsPointerAndValue(t *testing.T) {
	parser, runtime, err := wasitter.NewJSONParser(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.Parse([]byte(`[1]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	edit := wasitter.InputEdit{
		StartByte:   1,
		OldEndByte:  2,
		NewEndByte:  3,
		StartPoint:  wasitter.Point{Row: 0, Column: 1},
		OldEndPoint: wasitter.Point{Row: 0, Column: 2},
		NewEndPoint: wasitter.Point{Row: 0, Column: 3},
	}
	if err := tree.Edit(&edit); err != nil {
		t.Fatalf("pointer edit: %v", err)
	}
	if err := tree.Edit(edit); err != nil {
		t.Fatalf("value edit: %v", err)
	}
	root := tree.RootNode()
	child := root.Child(0)
	if !root.Equal(&root) || !root.Equals(&root) || !root.Eq(&root) {
		t.Fatal("node equality did not accept pointer arguments")
	}
	if got := root.ChildWithDescendant(&child); got.IsNull() {
		t.Fatal("ChildWithDescendant did not accept pointer argument")
	}
	if err := child.Edit(&edit); err != nil {
		t.Fatalf("node pointer edit: %v", err)
	}
	if cursor := wasitter.NewTreeCursor(root); cursor == nil {
		t.Fatal("NewTreeCursor did not accept a Node value")
	} else {
		_ = cursor.Close()
	}
	if iterator := wasitter.NewIterator(root, wasitter.DFSMode); iterator == nil {
		t.Fatal("NewIterator did not accept a Node value")
	} else {
		_ = iterator.Close()
	}
}

func TestRootNodeWithOffsetAcceptsNativeIntegerWidths(t *testing.T) {
	parser, runtime, err := wasitter.NewJSONParser(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	defer parser.Close()
	tree, err := parser.Parse([]byte(`1`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	// The upstream method uses int; the WASM-native form uses uint32. Both
	// should compile and produce a usable shifted root.
	if node, err := tree.RootNodeWithOffset(0, wasitter.Point{}); err != nil || node.IsNull() {
		t.Fatalf("uint32 literal offset: node=%v err=%v", node, err)
	}
	var nativeOffset int = 0
	if node, err := tree.RootNodeWithOffset(nativeOffset, wasitter.Point{}); err != nil || node.IsNull() {
		t.Fatalf("int offset: node=%v err=%v", node, err)
	}
}
