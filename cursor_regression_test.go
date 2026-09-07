package wasitter_test

import (
	"testing"
	"time"

	wasitter "github.com/zema1/wasitter"
)

// ResetTo used to recursively lock the source cursor (self-reset) and could
// deadlock when two goroutines reset opposite cursors at the same time.  Keep
// these calls behind a timeout so a regression fails the test suite promptly.
func TestTreeCursorResetToDoesNotDeadlock(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2, 3]`)
	a, b := tree.Walk(), tree.Walk()
	if a == nil || b == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	selfDone := make(chan struct{})
	go func() {
		a.ResetTo(a)
		close(selfDone)
	}()
	select {
	case <-selfDone:
	case <-time.After(2 * time.Second):
		t.Fatal("self ResetTo deadlocked")
	}

	mutualDone := make(chan struct{}, 2)
	go func() {
		a.ResetTo(b)
		mutualDone <- struct{}{}
	}()
	go func() {
		b.ResetTo(a)
		mutualDone <- struct{}{}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-mutualDone:
		case <-time.After(2 * time.Second):
			t.Fatal("opposite-order ResetTo deadlocked")
		}
	}
	if a.CurrentNode().IsNull() || b.CurrentNode().IsNull() {
		t.Fatal("ResetTo left a cursor at a null node")
	}
}

func TestTreeCursorNativeMovementRefreshesNode(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	root := tree.RootNode()

	if !cursor.GoToFirstChildForByte(1) {
		t.Fatal("GoToFirstChildForByte did not move")
	}
	if got := cursor.CurrentNode().Type(); got != "array" {
		t.Fatalf("byte movement node = %q, want array", got)
	}

	cursor.Reset(root)
	if !cursor.GoToFirstChildForPoint(wasitter.Point{Row: 0, Column: 1}) {
		t.Fatal("GoToFirstChildForPoint did not move")
	}
	if got := cursor.CurrentNode().Type(); got != "array" {
		t.Fatalf("point movement node = %q, want array", got)
	}

	cursor.Reset(root)
	cursor.GotoDescendant(2)
	if got := cursor.CurrentNode().Type(); got == "document" || got == "" {
		t.Fatalf("GotoDescendant did not refresh node, got %q", got)
	}

	// Verify both the upstream-sized and fixed-width index spellings compile
	// and report the same child index.
	cursor.Reset(root.NamedChild(0))
	byteIndex := cursor.GotoFirstChildForByte(0)
	cursor.Reset(root.NamedChild(0))
	byteIndex32 := cursor.GotoFirstChildForByte32(0)
	if byteIndex == nil || byteIndex32 == nil || uint32(*byteIndex) != *byteIndex32 {
		t.Fatalf("byte child indexes disagree: %v/%v", byteIndex, byteIndex32)
	}
}

// ts_tree_cursor_goto_descendant is a void-returning C API.  Keep the native
// cursor attached after invoking it; treating its empty result vector as an
// ABI failure silently switched to the approximate Go fallback and changed
// Tree-sitter's structural cursor semantics.
func TestTreeCursorNativeVoidGotoDescendantKeepsHandle(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2, 3]`)
	cursor := tree.RootNode().Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	if before := cursor.Handle(); before == 0 {
		t.Fatal("bundled cursor unexpectedly used the compatibility fallback")
	}
	cursor.GotoDescendant(3)
	if got := cursor.CurrentNode().Type(); got != "number" {
		t.Fatalf("GotoDescendant(3) node = %q, want number", got)
	}
	if after := cursor.Handle(); after == 0 {
		t.Fatal("void GotoDescendant detached the native cursor")
	}
}

// Reset accepts both the value-shaped wasitter node and the pointer-shaped
// node used by the upstream Go binding.
func TestTreeCursorResetUsesNodeValue(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2]`)
	cursor := tree.RootNode().Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	root := tree.RootNode()
	child := root.NamedChild(0)
	cursor.Reset(child)
	if got := cursor.CurrentNode(); got.IsNull() || !got.Equal(child) {
		t.Fatalf("Reset(node) positioned at %q, want %q", got.Type(), child.Type())
	}
	cursor.Reset(wasitter.Node{})
	if got := cursor.CurrentNode(); got.IsNull() {
		// An invalid reset is intentionally a no-op, so the prior node remains.
		t.Fatal("Reset(null node) unexpectedly invalidated cursor")
	}
}

func TestTreeCursorGoFallbackMatchesNativeTraversal(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	// Disable only the cursor constructor aliases.  The Go cursor then keeps a
	// value-style shadow and exercises the compatibility traversal while all
	// node accessors still come from the same upstream runtime.
	for _, name := range []string{"tsw_cursor_new", "tsw_tree_cursor_new"} {
		replacement := "old_" + name[4:]
		if len(replacement) != len(name) {
			t.Fatalf("replacement length mismatch for %q", name)
		}
		wasm = hideWASMExport(t, wasm, name, replacement)
	}
	tree := parseWithWASM(t, wasm)
	root := tree.RootNode()
	cursor := root.Walk()
	if cursor == nil {
		t.Fatal("fallback Node.Walk returned nil")
	}
	defer cursor.Close()
	if cursor.Handle() != 0 {
		t.Fatalf("fallback cursor handle = %#x, want zero", cursor.Handle())
	}
	if !cursor.GoToFirstChild() || cursor.CurrentNode().Type() != "array" {
		t.Fatalf("fallback first child = %q", cursor.CurrentNode().Type())
	}
	if !cursor.GoToFirstChild() || cursor.CurrentNode().Type() != "[" {
		t.Fatalf("fallback nested first child = %q", cursor.CurrentNode().Type())
	}
	if !cursor.GoToNextSibling() || cursor.CurrentNode().Type() != "number" {
		t.Fatalf("fallback next sibling = %q", cursor.CurrentNode().Type())
	}
	if !cursor.GoToNextNamedSibling() || cursor.CurrentNode().Type() != "number" {
		t.Fatalf("fallback next named sibling = %q", cursor.CurrentNode().Type())
	}
	if !cursor.GoToParent() || cursor.CurrentNode().Type() != "array" {
		t.Fatalf("fallback parent = %q", cursor.CurrentNode().Type())
	}
	if got := cursor.GotoFirstChildForByte(0); got == nil || *got != 0 || cursor.CurrentNode().Type() != "[" {
		t.Fatalf("fallback byte child = %v/%q", got, cursor.CurrentNode().Type())
	}
	cursor.Reset(root)
	cursor.GotoDescendant(3)
	if got := cursor.CurrentNode().Type(); got != "number" {
		t.Fatalf("fallback descendant[3] = %q, want number", got)
	}
	if got := cursor.CurrentDepth(); got != 2 {
		t.Fatalf("fallback descendant depth = %d, want 2", got)
	}
}

// A cursor rooted at a non-root node must not escape through that node's
// containing-tree siblings.  The native TSTreeCursor keeps this boundary in
// its internal stack; the value-style compatibility path has to enforce it
// explicitly because Node.NextSibling naturally sees the containing tree.
func TestTreeCursorGoFallbackHonorsSubtreeRootBoundary(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{"tsw_cursor_new", "tsw_tree_cursor_new"} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	tree := parseWithWASM(t, wasm)
	root := tree.RootNode()
	subtree := root.NamedChild(0)
	if subtree.IsNull() {
		t.Fatal("missing named subtree")
	}
	cursor := subtree.Walk()
	if cursor == nil {
		t.Fatal("subtree Walk returned nil")
	}
	defer cursor.Close()
	if cursor.CurrentFieldName() != "" || cursor.CurrentFieldID() != 0 {
		t.Fatalf("subtree root field = %q/%d, want empty/0", cursor.CurrentFieldName(), cursor.CurrentFieldID())
	}
	if cursor.GoToNextSibling() || cursor.GoToPreviousSibling() {
		t.Fatal("subtree-root cursor escaped through a sibling")
	}
}

// The compatibility cursor exposes visible pre-order descendant indexes. A
// previous-sibling move must update that index (and the shadow path used by a
// subsequent next-sibling move), even when the underlying grammar stores the
// siblings in a hidden repetition node.
func TestTreeCursorGoFallbackPreviousSiblingKeepsDescendantIndex(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{"tsw_cursor_new", "tsw_tree_cursor_new"} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	rt, err := wasitter.NewRuntime(nil, wasm)
	if err != nil {
		t.Fatal(err)
	}
	lang, err := rt.LoadLanguage("json")
	if err != nil {
		rt.Close()
		t.Fatal(err)
	}
	p, err := wasitter.NewParserWithRuntime(rt)
	if err != nil {
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	if err := p.SetLanguage(lang); err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	tree, err := p.Parse([]byte(`[1,{"a":[true,null]},3,{"b":4}]`), nil)
	if err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tree.Close()
		p.Close()
		lang.Close()
		rt.Close()
	})
	c := tree.RootNode().Walk()
	if c == nil {
		t.Fatal("fallback cursor is nil")
	}
	defer c.Close()
	// The inner array's visible sequence is [, true, ,, null, ].
	inner := tree.RootNode().NamedChild(0).NamedChild(1).NamedChild(0).NamedChild(1)
	c.Reset(inner)
	if !c.GoToLastChild() || c.CurrentNode().Type() != "]" {
		t.Fatalf("last child = %q", c.CurrentNode().Type())
	}
	if !c.GoToPrevSibling() || c.CurrentNode().Type() != "null" {
		t.Fatalf("previous sibling = %q", c.CurrentNode().Type())
	}
	if got := c.DescendantIndex(); got != 4 {
		t.Fatalf("previous sibling descendant index = %d, want 4", got)
	}
	if !c.GoToNextSibling() || c.CurrentNode().Type() != "]" {
		t.Fatalf("next sibling after previous = %q", c.CurrentNode().Type())
	}
	if got := c.DescendantIndex(); got != 5 {
		t.Fatalf("next sibling descendant index = %d, want 5", got)
	}
}

// Native Tree-sitter preserves its structural descendant index when moving
// backwards. Hidden repetition nodes can make that value differ from the
// visible pre-order index used by the Go-only fallback (the upstream C API
// intentionally leaves the reverse iterator's index at its structural value).
// Keep the native path attached so callers observe the same value as C.
func TestTreeCursorNativePreviousSiblingPreservesStructuralIndex(t *testing.T) {
	_, _, tree := parseJSON(t, `[1,{"a":[true,null]},3,{"b":4}]`)
	cursor := tree.RootNode().NamedChild(0).NamedChild(1).NamedChild(0).NamedChild(1).Walk()
	if cursor == nil {
		t.Fatal("inner cursor is nil")
	}
	defer cursor.Close()
	if !cursor.GoToLastChild() || cursor.CurrentNode().Type() != "]" {
		t.Fatalf("last child = %q", cursor.CurrentNode().Type())
	}
	if !cursor.GoToPreviousSibling() || cursor.CurrentNode().Type() != "null" {
		t.Fatalf("previous sibling = %q", cursor.CurrentNode().Type())
	}
	// Tree-sitter 0.25's reverse iterator reports the structural index (1)
	// here; the visible fallback index is 4 and is tested separately above.
	if got := cursor.DescendantIndex(); got != 1 {
		t.Fatalf("native previous-sibling descendant index = %d, want 1", got)
	}
}

func TestNodeRangeFallbackUsesTreeSitterBoundaryRules(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{
		"tsw_node_descendant_for_byte_range",
		"tsw_node_named_descendant_for_byte_range",
		"tsw_node_descendant_for_point_range",
		"tsw_node_named_descendant_for_point_range",
	} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	tree := parseWithWASM(t, wasm)
	root := tree.RootNode()
	// At byte zero the opening bracket is the smallest node spanning the empty
	// point.  A range outside the receiver follows the native runtime's contract
	// and returns the receiver itself when no child can span it.
	if got := root.DescendantForByteRange(0, 0).Type(); got != "[" {
		t.Fatalf("fallback point-at-start node = %q, want [", got)
	}
	if got := root.DescendantForByteRange(100, 101).Type(); got != "document" {
		t.Fatalf("fallback out-of-range node = %q, want document", got)
	}
	if got := root.NamedDescendantForByteRange(0, 0).Type(); got != "array" {
		t.Fatalf("fallback named point-at-start node = %q, want array", got)
	}
	if got := root.DescendantForPointRange(wasitter.Point{Row: 0, Column: 0}, wasitter.Point{Row: 0, Column: 0}).Type(); got != "[" {
		t.Fatalf("fallback point-range node = %q, want [", got)
	}
}

// Exercise every small byte/point interval against the native bridge and the
// compatibility traversal. This catches boundary mistakes that are easy to
// miss with only a point-at-start example (especially empty nodes and ranges
// extending outside the receiver).
func TestNodeRangeFallbackMatchesNativeIntervals(t *testing.T) {
	source := []byte(`[1,{"a":[true,null]},3]`)
	_, _, nativeTree := parseJSON(t, string(source))
	nativeRoot := nativeTree.RootNode()
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{
		"tsw_node_descendant_for_byte_range",
		"tsw_node_named_descendant_for_byte_range",
		"tsw_node_descendant_for_point_range",
		"tsw_node_named_descendant_for_point_range",
	} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	rt, err := wasitter.NewRuntime(nil, wasm)
	if err != nil {
		t.Fatal(err)
	}
	lang, err := rt.LoadLanguage("json")
	if err != nil {
		rt.Close()
		t.Fatal(err)
	}
	p, err := wasitter.NewParserWithRuntime(rt)
	if err != nil {
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	if err := p.SetLanguage(lang); err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	fallbackTree, err := p.Parse(source, nil)
	if err != nil {
		p.Close()
		lang.Close()
		rt.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nativeTree.Close()
		fallbackTree.Close()
		p.Close()
		lang.Close()
		rt.Close()
	})
	fallbackRoot := fallbackTree.RootNode()
	check := func(label string, a, b wasitter.Node) {
		t.Helper()
		if a.IsNull() != b.IsNull() || a.Type() != b.Type() || a.StartByte() != b.StartByte() || a.EndByte() != b.EndByte() {
			t.Fatalf("%s native=%q[%d,%d] fallback=%q[%d,%d]", label, a.Type(), a.StartByte(), a.EndByte(), b.Type(), b.StartByte(), b.EndByte())
		}
	}
	for start := uint32(0); start <= uint32(len(source)+2); start++ {
		for end := start; end <= uint32(len(source)+2); end++ {
			check("byte", nativeRoot.DescendantForByteRange(start, end), fallbackRoot.DescendantForByteRange(start, end))
			check("named byte", nativeRoot.NamedDescendantForByteRange(start, end), fallbackRoot.NamedDescendantForByteRange(start, end))
		}
	}
	// Include a few points outside the document and every same-row interval.
	for start := uint32(0); start <= uint32(len(source)+2); start++ {
		for end := start; end <= uint32(len(source)+2); end++ {
			a := wasitter.Point{Column: start}
			b := wasitter.Point{Column: end}
			check("point", nativeRoot.DescendantForPointRange(a, b), fallbackRoot.DescendantForPointRange(a, b))
			check("named point", nativeRoot.NamedDescendantForPointRange(a, b), fallbackRoot.NamedDescendantForPointRange(a, b))
		}
	}
}

func TestNodeSExpressionFallbackQuotesAnonymousAndFields(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{"tsw_node_to_sexp", "tsw_node_string"} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	tree := parseWithWASM(t, wasm)
	if got, want := tree.ToSExpression(), `(document (array "[" (number) "," (number) "]"))`; got != want {
		t.Fatalf("fallback S-expression = %q, want %q", got, want)
	}
}

func TestTreeCursorGotoDescendantOutOfRangeReturnsToRoot(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, {"x": 2}]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	if !cursor.GoToFirstChild() {
		t.Fatal("failed to descend to array before out-of-range check")
	}
	if !cursor.GoToFirstChild() {
		t.Fatal("failed to descend before out-of-range check")
	}
	if cursor.CurrentNode().Type() == "document" {
		t.Fatal("setup did not descend")
	}
	cursor.GotoDescendant(^uint32(0))
	if got := cursor.CurrentNode().Type(); got != "document" {
		t.Fatalf("out-of-range GotoDescendant node = %q, want document root", got)
	}
	if got := cursor.CurrentDepth(); got != 0 {
		t.Fatalf("out-of-range GotoDescendant depth = %d, want 0", got)
	}
}

func TestTreeCursorCloseAfterTreeCloseReleasesSafely(t *testing.T) {
	_, _, tree := parseJSON(t, `[1]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	if err := tree.Close(); err != nil {
		t.Fatalf("Tree.Close: %v", err)
	}
	if got := tree.Handle(); got != 0 {
		t.Fatalf("tree.Handle after close = %#x, want zero", got)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Cursor.Close after Tree.Close: %v", err)
	}
}

// Tree.Close owns the backing TSTree, while cursors are intentionally
// independent allocations.  A cursor that outlives its tree must therefore
// become a harmless zero-value view rather than exposing a stale guest
// pointer or retaining its old depth/position shadow.
func TestTreeCursorAfterTreeCloseIsInvalid(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	if !cursor.GoToFirstChild() {
		t.Fatal("cursor did not descend during setup")
	}
	if err := tree.Close(); err != nil {
		t.Fatalf("Tree.Close: %v", err)
	}
	if got := cursor.Handle(); got != 0 {
		t.Fatalf("cursor.Handle after tree close = %#x, want zero", got)
	}
	if got := cursor.CurrentNode(); !got.IsNull() {
		t.Fatalf("cursor.CurrentNode after tree close = %q, want null", got.Type())
	}
	if got := cursor.CurrentDepth(); got != 0 {
		t.Fatalf("cursor.CurrentDepth after tree close = %d, want zero", got)
	}
	if got := cursor.DescendantIndex(); got != 0 {
		t.Fatalf("cursor.DescendantIndex after tree close = %d, want zero", got)
	}
	if copy := cursor.Copy(); copy != nil {
		_ = copy.Close()
		t.Fatal("cursor.Copy after tree close returned an invalid cursor")
	}
	if cursor.GoToFirstChild() || cursor.GoToNextSibling() || cursor.GoToParent() {
		t.Fatal("closed-tree cursor unexpectedly moved")
	}
	if got := cursor.GotoFirstChildForByte(0); got != nil {
		t.Fatalf("GotoFirstChildForByte after tree close = %v, want nil", *got)
	}
	if got := cursor.GotoFirstChildForPoint(wasitter.Point{}); got != nil {
		t.Fatalf("GotoFirstChildForPoint after tree close = %v, want nil", *got)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Cursor.Close after tree close: %v", err)
	}
}

func TestTreeCursorResetToAcrossTrees(t *testing.T) {
	p, rt := newJSONParser(t)
	treeA, err := p.Parse([]byte(`[0]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	treeB, err := p.Parse([]byte(`{"key": 1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = treeA.Close()
		_ = treeB.Close()
		_ = p.Close()
		_ = rt.Close()
	})
	a, b := treeA.Walk(), treeB.Walk()
	if a == nil || b == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer a.Close()
	defer b.Close()
	if !b.GoToFirstChild() {
		t.Fatal("source cursor did not move")
	}
	a.ResetTo(b)
	if got := a.CurrentNode().Type(); got != "object" {
		t.Fatalf("cross-tree ResetTo node = %q, want object", got)
	}
	// The destination now owns the source tree's native cursor state.  Closing
	// the old tree must not make the reset cursor unusable.
	if err := treeA.Close(); err != nil {
		t.Fatal(err)
	}
	if got := a.CurrentNode().Type(); got != "object" {
		t.Fatalf("cursor after old-tree close = %q, want object", got)
	}
}

func TestTreeCursorResetToAcrossRuntimesFallsBackSafely(t *testing.T) {
	p1, rt1 := newJSONParser(t)
	p2, rt2 := newJSONParser(t)
	tree1, err := p1.Parse([]byte(`[0]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree2, err := p2.Parse([]byte(`[1, 2]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tree1.Close()
		_ = tree2.Close()
		_ = p1.Close()
		_ = p2.Close()
		_ = rt1.Close()
		_ = rt2.Close()
	})
	a, b := tree1.Walk(), tree2.Walk()
	if a == nil || b == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer a.Close()
	defer b.Close()
	if !b.GoToFirstChild() {
		t.Fatal("source cursor did not move")
	}
	a.ResetTo(b)
	if got := a.CurrentNode().Type(); got != "array" {
		t.Fatalf("cross-runtime ResetTo node = %q, want array", got)
	}
	if got := a.Handle(); got != 0 {
		t.Fatalf("cross-runtime fallback retained foreign native handle %#x", got)
	}
	if !a.GoToFirstChild() || a.CurrentNode().Type() != "[" {
		t.Fatalf("fallback cursor navigation after ResetTo = %q", a.CurrentNode().Type())
	}
}

// ChildWithDescendant must return a null node when asked about the receiver
// itself. The native bridge returns null for this case; the compatibility
// fallback used to recurse through Parent/Equal while holding nodePairMu and
// deadlock instead.
func TestNodeChildWithDescendantSelfIsNull(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, {"x": null}]`)
	value := tree.RootNode().NamedChild(0).NamedChild(1)
	if got := value.ChildWithDescendant(value); !got.IsNull() {
		t.Fatalf("self ChildWithDescendant = %q, want null", got.Type())
	}
}

// Id identifies the underlying Tree-sitter node, rather than the transient
// WASM wrapper allocated for each accessor call.  Re-fetching a node through
// the tree must therefore preserve its identity even though Handle values
// (the wrapper pointers) are intentionally different.
func TestNodeIDStableAcrossWrappers(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2]`)
	first := tree.RootNode().NamedChild(0).NamedChild(0)
	second := tree.RootNode().NamedChild(0).NamedChild(0)
	if first.Handle() == 0 || second.Handle() == 0 {
		t.Fatal("node wrappers have zero handles")
	}
	if first.Handle() == second.Handle() {
		t.Fatal("test expected distinct transient wrapper handles")
	}
	if first.ID() == 0 || first.ID() != second.ID() {
		t.Fatalf("node ids = %#x/%#x, want stable non-zero identity", first.ID(), second.ID())
	}
}

// Reusing a cursor that cannot be reset to a null node must not leak its old
// position into Node.Children.
func TestNodeChildrenRejectsInvalidResetTarget(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, 2]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	if got := tree.RootNode().Children(cursor); len(got) == 0 {
		t.Fatal("expected root children")
	}
	if got := (wasitter.Node{}).Children(cursor); got != nil {
		t.Fatalf("children of null node = %#v, want nil", got)
	}
}

// A partially implemented bridge may provide native movement and current-node
// access but omit the optional descendant-index accessor.  The cursor must
// reconstruct its visible pre-order shadow immediately; otherwise the first
// DescendantIndex call after a child/byte move reports the index from before
// the move.
func TestTreeCursorPartialIndexAccessorRebuildsShadow(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index"} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	cursor := tree.RootNode().Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	defer cursor.Close()
	if !cursor.GoToFirstChild() || cursor.CurrentNode().Type() != "array" {
		t.Fatalf("first child = %q", cursor.CurrentNode().Type())
	}
	if got := cursor.DescendantIndex(); got != 1 {
		t.Fatalf("partial-index child descendant index = %d, want 1", got)
	}
	if got := cursor.GotoFirstChildForByte(1); got == nil {
		t.Fatal("GotoFirstChildForByte did not find a child")
	}
	if got := cursor.DescendantIndex(); got != 3 {
		t.Fatalf("partial-index byte descendant index = %d, want 3", got)
	}
}

// When the native cursor movement exports are present but the optional
// descendant-index export is absent, the host rebuilds its fallback ancestor
// path from the returned current node.  The movement wrappers must not append
// or pop that path a second time; doing so makes Depth drift while the first
// DescendantIndex call still appears correct.
func TestTreeCursorPartialIndexAccessorKeepsDepthAndParentCoherent(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	for _, name := range []string{"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index"} {
		wasm = hideWASMExport(t, wasm, name, "old_"+name[4:])
	}
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	cursor := tree.RootNode().Walk()
	if cursor == nil {
		t.Fatal("partial-index cursor is nil")
	}
	defer cursor.Close()

	if !cursor.GoToFirstChild() || cursor.CurrentDepth() != 1 {
		t.Fatalf("first child depth = %d, want 1", cursor.CurrentDepth())
	}
	if !cursor.GoToFirstChild() || cursor.CurrentDepth() != 2 {
		t.Fatalf("nested first child depth = %d, want 2", cursor.CurrentDepth())
	}
	if !cursor.GoToParent() || cursor.CurrentNode().Type() != "array" || cursor.CurrentDepth() != 1 {
		t.Fatalf("parent after nested child = %q depth %d, want array/1", cursor.CurrentNode().Type(), cursor.CurrentDepth())
	}
	if !cursor.GoToLastChild() || cursor.CurrentDepth() != 2 {
		t.Fatalf("last child depth = %d, want 2", cursor.CurrentDepth())
	}
	if !cursor.GoToParent() || cursor.CurrentNode().Type() != "array" || cursor.CurrentDepth() != 1 {
		t.Fatalf("parent after last child = %q depth %d, want array/1", cursor.CurrentNode().Type(), cursor.CurrentDepth())
	}

	cursor.Reset(tree.RootNode())
	if got := cursor.GotoFirstChildForByte(1); got == nil || cursor.CurrentDepth() != 1 {
		t.Fatalf("byte child result/depth = %v/%d, want non-nil/1", got, cursor.CurrentDepth())
	}
	if !cursor.GoToFirstChild() || cursor.CurrentDepth() != 2 {
		t.Fatalf("child after byte move depth = %d, want 2", cursor.CurrentDepth())
	}
	if !cursor.GoToParent() || cursor.CurrentDepth() != 1 {
		t.Fatalf("parent after byte child depth = %d, want 1", cursor.CurrentDepth())
	}
}

// Cursor operations and lifecycle transitions may race in real editors. Keep
// a short stress loop here to exercise the lock ordering around native calls,
// cursor Close, and Tree.Close; the test is deliberately bounded so a future
// lock inversion fails promptly.
func TestTreeCursorConcurrentCloseAndNavigation(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, {"a": [true, null, 3]}, 4]`)
	cursor := tree.RootNode().Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil cursor")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			cursor.GoToFirstChild()
			cursor.GoToLastChild()
			cursor.GoToNextSibling()
			cursor.GoToPrevSibling()
			cursor.GoToParent()
			cursor.GotoDescendant(uint32(i % 20))
			cursor.GotoFirstChildForByte(uint32(i))
			cursor.GotoFirstChildForPoint(wasitter.Point{Column: uint32(i)})
			_ = cursor.CurrentNode()
			_ = cursor.CurrentFieldName()
			_ = cursor.CurrentFieldID()
			_ = cursor.CurrentDepth()
			_ = cursor.DescendantIndex()
		}
	}()
	// Closing the tree while navigation is in flight must not panic or leave a
	// stale native handle. Cursor.Close remains safe after the tree transition.
	_ = tree.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cursor navigation did not finish after concurrent Tree.Close")
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Cursor.Close after concurrent Tree.Close: %v", err)
	}
}
