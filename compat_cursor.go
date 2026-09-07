package wasitter

// NewTreeCursor creates a cursor rooted at node.  The historical
// smacker/go-tree-sitter binding exposes this constructor, while the modern
// binding normally creates cursors through Node.Walk.  Keeping the pointer
// argument here makes the migration path convenient without changing the
// value-oriented Node API used by wasitter.
//
// A nil or null node has no meaningful cursor and returns nil, matching the
// behavior of Node.Walk for an invalid node. Both Node and *Node are accepted
// so callers can use either public representation.
func NewTreeCursor(value any) *TreeCursor {
	node, ok := nodeValueArg(value)
	if !ok || node.IsNull() {
		return nil
	}
	return newTreeCursor(node)
}

// NewCursor is a concise alias for NewTreeCursor.
func NewCursor(node any) *TreeCursor { return NewTreeCursor(node) }

// CurrentNodePtr returns the cursor's current node as a pointer.  The main
// API intentionally returns copyable value nodes; this helper is useful when
// adapting code that uses pointer-shaped nodes from the upstream bindings.
func (c *TreeCursor) CurrentNodePtr() *Node {
	n := c.CurrentNode()
	if n.IsNull() {
		return nil
	}
	return &n
}

// NodePtr is the pointer-shaped counterpart of Node.
func (c *TreeCursor) NodePtr() *Node { return c.CurrentNodePtr() }

// RootNodePtr is the pointer-shaped counterpart of Tree.RootNode.  It
// returns nil for a closed tree or a guest that cannot provide a root node.
func (t *Tree) RootNodePtr() *Node {
	if t == nil {
		return nil
	}
	n := t.RootNode()
	if n.IsNull() {
		return nil
	}
	return &n
}

// WalkPtr creates a cursor from a pointer-shaped root node.  It is equivalent
// to node.Walk when node is non-nil.
func WalkPtr(node any) *TreeCursor { return NewTreeCursor(node) }
