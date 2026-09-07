package wasitter

// NewTreeCursor creates a cursor rooted at node. It returns nil for a null
// node. Close the cursor before closing the node's tree. See also [Node.Walk].
func NewTreeCursor(node Node) *TreeCursor {
	if node.IsNull() {
		return nil
	}
	return newTreeCursor(node)
}
