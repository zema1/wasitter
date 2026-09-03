package sitterwasm

// The core sitterwasm API uses copyable value Nodes.  These pointer-shaped
// helpers make it straightforward to adapt traversal code written for
// bindings whose accessors return *Node, without introducing a second node
// representation or changing existing method signatures.

func nodePtr(value Node) *Node {
	if value.IsNull() {
		return nil
	}
	return &value
}

// ParentPtr is the pointer-shaped counterpart of Parent.
func (n Node) ParentPtr() *Node { return nodePtr(n.Parent()) }

// ChildPtr is the pointer-shaped counterpart of Child.
func (n Node) ChildPtr(index int) *Node { return nodePtr(n.Child(index)) }

// NamedChildPtr is the pointer-shaped counterpart of NamedChild.
func (n Node) NamedChildPtr(index int) *Node {
	return nodePtr(n.NamedChild(index))
}

// NextSiblingPtr is the pointer-shaped counterpart of NextSibling.
func (n Node) NextSiblingPtr() *Node { return nodePtr(n.NextSibling()) }

// PrevSiblingPtr is the pointer-shaped counterpart of PrevSibling.
func (n Node) PrevSiblingPtr() *Node { return nodePtr(n.PrevSibling()) }

// NextNamedSiblingPtr is the pointer-shaped counterpart of NextNamedSibling.
func (n Node) NextNamedSiblingPtr() *Node { return nodePtr(n.NextNamedSibling()) }

// PrevNamedSiblingPtr is the pointer-shaped counterpart of PrevNamedSibling.
func (n Node) PrevNamedSiblingPtr() *Node { return nodePtr(n.PrevNamedSibling()) }

// ChildByFieldNamePtr is the pointer-shaped counterpart of ChildByFieldName.
func (n Node) ChildByFieldNamePtr(field string) *Node {
	return nodePtr(n.ChildByFieldName(field))
}

// ChildByFieldIdPtr is the mixed-case compatibility spelling of
// ChildByFieldIDPtr.
func (n Node) ChildByFieldIdPtr(fieldID uint16) *Node {
	return nodePtr(n.ChildByFieldId(fieldID))
}

// ChildByFieldIDPtr is the pointer-shaped counterpart of ChildByFieldID.
func (n Node) ChildByFieldIDPtr(fieldID uint16) *Node {
	return nodePtr(n.ChildByFieldID(fieldID))
}

// ChildWithDescendantNode returns the child containing descendant as a
// pointer, or nil when no such child exists.
func (n Node) ChildWithDescendantNode(descendant *Node) *Node {
	return nodePtr(n.ChildWithDescendantPtr(descendant))
}

// EqualPtr reports whether n and other identify the same syntax node.
func (n Node) EqualPtr(other *Node) bool {
	if other == nil {
		return n.IsNull()
	}
	return n.Equal(*other)
}

// ExecPtr is the pointer-node counterpart of QueryCursor.Exec.
func (c *QueryCursor) ExecPtr(query *Query, node *Node) error {
	if node == nil {
		return ErrInvalidHandle
	}
	return c.Exec(query, *node)
}

// ExecNode is a descriptive alias for ExecPtr.
func (c *QueryCursor) ExecNode(query *Query, node *Node) error {
	return c.ExecPtr(query, node)
}

// MatchesPtr and CapturesPtr provide explicit pointer-node forms for callers
// that prefer compile-time argument checking over the variadic compatibility
// methods. The optional source buffer is used for text predicates.
func (c *QueryCursor) MatchesPtr(query *Query, node *Node, text []byte) QueryMatches {
	if node == nil {
		return nil
	}
	return c.Matches(query, node, text)
}

// CapturesPtr is the pointer-node counterpart of QueryCursor.Captures.
func (c *QueryCursor) CapturesPtr(query *Query, node *Node, text []byte) QueryCaptures {
	if node == nil {
		return nil
	}
	return c.Captures(query, node, text)
}
