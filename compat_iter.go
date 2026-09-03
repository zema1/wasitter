package sitterwasm

import (
	"fmt"
	"io"
	"sync"
)

// IterMode selects the traversal order used by Iterator.
type IterMode uint8

const (
	// DFSMode visits a node before recursively visiting its children.
	DFSMode IterMode = iota
	// BFSMode visits nodes level by level.
	BFSMode
)

// Iterator walks a node and all of its descendants.  It mirrors the small
// iterator helper shipped by smacker/go-tree-sitter while retaining
// sitterwasm's value-backed nodes internally.  Returned pointers refer to
// independent Node values and remain usable while their owning tree is open.
type Iterator struct {
	mu      sync.Mutex
	named   bool
	mode    IterMode
	toVisit []Node
	invalid error
}

// NewIterator creates a depth-first or breadth-first iterator rooted at node.
// Both Node and *Node are accepted to accommodate the value-oriented
// sitterwasm API and the pointer-oriented native bindings.
func NewIterator(value any, mode IterMode) *Iterator {
	it := &Iterator{mode: mode}
	node, ok := nodeValueArg(value)
	// Do not rely on the optional tsw_node_is_null export here.  A number of
	// older bridge modules expose navigation but omit that convenience
	// predicate; a non-zero handle paired with a live tree is enough to seed an
	// iterator.  Check the owning tree directly so a closed node still produces
	// a terminal iterator rather than one that repeatedly returns ABI errors.
	if !ok || node.handle == 0 || node.tree == nil || node.tree.ensureOpen() != nil {
		it.invalid = io.EOF
		return it
	}
	if mode != DFSMode && mode != BFSMode {
		it.invalid = fmt.Errorf("sitterwasm: unsupported iterator mode %d", mode)
		return it
	}
	it.toVisit = []Node{node}
	return it
}

// NewNamedIterator is NewIterator restricted to named descendants.
func NewNamedIterator(node any, mode IterMode) *Iterator {
	it := NewIterator(node, mode)
	if it != nil {
		it.named = true
	}
	return it
}

// Next returns the next node, or io.EOF after traversal is exhausted.
func (it *Iterator) Next() (*Node, error) {
	if it == nil {
		return nil, io.EOF
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.invalid != nil {
		err := it.invalid
		// Preserve io.EOF for subsequent calls; malformed mode errors are also
		// terminal and should not repeatedly mutate iterator state.
		if err == io.EOF {
			return nil, err
		}
		return nil, err
	}
	if len(it.toVisit) == 0 {
		return nil, io.EOF
	}
	node := it.toVisit[0]
	it.toVisit = it.toVisit[1:]
	var children []Node
	if it.named {
		count := node.NamedChildCount()
		children = make([]Node, 0, count)
		for i := 0; i < count; i++ {
			child := node.NamedChild(i)
			if child.handle != 0 && child.tree != nil {
				children = append(children, child)
			}
		}
	} else {
		count := node.ChildCount()
		children = make([]Node, 0, count)
		for i := 0; i < count; i++ {
			child := node.Child(i)
			if child.handle != 0 && child.tree != nil {
				children = append(children, child)
			}
		}
	}
	if it.mode == DFSMode {
		// Prepend children in source order. The first child is visited next,
		// yielding the same pre-order traversal as the upstream helper.
		it.toVisit = append(children, it.toVisit...)
	} else {
		it.toVisit = append(it.toVisit, children...)
	}
	return &node, nil
}

// ForEach visits every remaining node until the callback or traversal returns
// an error. As in smacker/go-tree-sitter, normal exhaustion is reported as
// io.EOF; callers that prefer a nil-on-success convention can use a small
// errors.Is(err, io.EOF) adapter.
func (it *Iterator) ForEach(fn func(*Node) error) error {
	if fn == nil {
		return fmt.Errorf("sitterwasm: nil iterator callback")
	}
	for {
		node, err := it.Next()
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return err
		}
		if err := fn(node); err != nil {
			return err
		}
	}
}

// Close discards any nodes still queued by the iterator. It is provided for
// symmetry with other stateful traversal helpers; nodes themselves remain
// owned by their Tree.
func (it *Iterator) Close() error {
	if it == nil {
		return nil
	}
	it.mu.Lock()
	it.toVisit = nil
	it.invalid = io.EOF
	it.mu.Unlock()
	return nil
}
