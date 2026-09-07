package wasitter_test

import (
	"testing"

	wasitter "github.com/zema1/wasitter"
)

func TestTreeEditAndValueNavigation(t *testing.T) {
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
	if err := tree.Edit(edit); err != nil {
		t.Fatalf("value edit: %v", err)
	}
	root := tree.RootNode()
	child := root.Child(0)
	if !root.Equal(root) {
		t.Fatal("node equality is not reflexive")
	}
	if got := root.ChildWithDescendant(child); got.IsNull() {
		t.Fatal("ChildWithDescendant did not find direct child")
	}
	if err := child.Edit(edit); err != nil {
		t.Fatalf("node edit: %v", err)
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

func TestRootNodeWithOffsetShiftsCoordinates(t *testing.T) {
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

	node, err := tree.RootNodeWithOffset(8, wasitter.Point{Row: 2, Column: 3})
	if err != nil {
		t.Fatal(err)
	}
	if node.StartByte() != 8 || node.EndByte() != 9 || node.StartPoint() != (wasitter.Point{Row: 2, Column: 3}) {
		t.Fatalf("shifted root = %d..%d, %v", node.StartByte(), node.EndByte(), node.StartPoint())
	}
}
