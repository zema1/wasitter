package sitterwasm

import (
	"context"
	goruntime "runtime"
	"sync"
	"sync/atomic"
)

// TreeCursor is a mutable depth-first cursor over a syntax tree. When the
// module contains the sitterwasm cursor ABI, navigation is delegated to the
// upstream TSTreeCursor implementation. A small Go implementation is retained
// as a compatibility fallback for older/custom modules.
type TreeCursor struct {
	mu sync.Mutex

	node         Node
	root         Node
	stack        []Node
	stackIndices []uint32
	// descendantIndex mirrors the index stored by Tree-sitter for the
	// value-style fallback cursor. Native cursors keep this value in the guest
	// stack and expose it through tsw_cursor_current_descendant_index; retaining
	// it locally is important when a compatibility module has no cursor ABI (or
	// when a partially implemented native cursor is detached after an error).
	// It is guarded by mu together with node/stack.
	descendantIndex uint32
	// descendantIndexKnown is true when the shadow index was obtained from the
	// native cursor (or explicitly computed by fallback movement). It is guarded
	// by mu and lets callers distinguish a stale value after an optional ABI
	// accessor is unavailable.
	descendantIndexKnown bool

	tree   *Tree
	handle uint32
	closed atomic.Bool
}

// cursorShadow is the host-side representation of a cursor position. Native
// cursor calls can fail after mutating guest state (especially on partial ABI
// modules), so callers take a snapshot before operations that may need to fall
// back. The slices are copied by snapshotShadowLocked and restored in place to
// retain their capacity without sharing mutable backing arrays with the saved
// value.
type cursorShadow struct {
	node                 Node
	stack                []Node
	stackIndices         []uint32
	descendantIndex      uint32
	descendantIndexKnown bool
}

func (c *TreeCursor) snapshotShadowLocked() cursorShadow {
	if c == nil {
		return cursorShadow{}
	}
	return cursorShadow{
		node:                 c.node,
		stack:                append([]Node(nil), c.stack...),
		stackIndices:         append([]uint32(nil), c.stackIndices...),
		descendantIndex:      c.descendantIndex,
		descendantIndexKnown: c.descendantIndexKnown,
	}
}

func (c *TreeCursor) restoreShadowLocked(shadow cursorShadow) {
	if c == nil {
		return
	}
	c.node = shadow.node
	c.stack = append(c.stack[:0], shadow.stack...)
	c.stackIndices = append(c.stackIndices[:0], shadow.stackIndices...)
	c.descendantIndex = shadow.descendantIndex
	c.descendantIndexKnown = shadow.descendantIndexKnown
}

// fallbackVisibleSubtreeCount returns the number of visible nodes rooted at n,
// including n itself. Node.Child enumerates the same visible child sequence as
// a Tree-sitter cursor for ordinary generated grammars. Keeping this helper in
// the host lets fallback cursor movement maintain Tree-sitter's pre-order
// descendant indexes without repeatedly walking from the cursor root.
func fallbackVisibleSubtreeCount(n Node) uint32 {
	// Do not call Node.IsNull here.  That accessor is optional on old bridge
	// modules, while a non-zero SWNode handle is already sufficient to identify
	// a value that can be traversed.  Treating an otherwise valid node as null
	// would make fallback indexes collapse to zero for such modules.
	if n.handle == 0 || n.tree == nil {
		return 0
	}
	count := uint64(1)
	for i := 0; i < n.ChildCount(); i++ {
		count += uint64(fallbackVisibleSubtreeCount(n.Child(i)))
		if count > uint64(^uint32(0)) {
			return ^uint32(0)
		}
	}
	return uint32(count)
}

// fallbackChildDescendantIndex computes the pre-order index of childIndex
// relative to a cursor currently positioned at parent. The caller supplies the
// parent's already-stored index; child indexes are over visible children.
func fallbackChildDescendantIndex(parent Node, parentIndex uint32, childIndex int) uint32 {
	if childIndex < 0 {
		return parentIndex
	}
	// Descendant indexes are uint32 on the wire.  Saturate rather than wrap
	// when a malformed/very large compatibility tree would place a child past
	// the representable range; wrapping could make an out-of-range child look
	// like the root or an unrelated low index.
	value := uint64(parentIndex) + 1
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	for i := 0; i < childIndex && i < parent.ChildCount(); i++ {
		value += uint64(fallbackVisibleSubtreeCount(parent.Child(i)))
		if value > uint64(^uint32(0)) {
			return ^uint32(0)
		}
	}
	return uint32(value)
}

// pushFallbackAncestor records the node and its descendant index before a
// fallback cursor descends into one of its children.
func (c *TreeCursor) pushFallbackAncestor() {
	if c == nil {
		return
	}
	c.stack = append(c.stack, c.node)
	c.stackIndices = append(c.stackIndices, c.descendantIndex)
}

// popFallbackAncestor restores the most recently recorded visible ancestor.
// It returns false when the cursor is already at its root.
func (c *TreeCursor) popFallbackAncestor() bool {
	if c == nil || len(c.stack) == 0 {
		return false
	}
	last := len(c.stack) - 1
	c.node = c.stack[last]
	if last < len(c.stackIndices) {
		c.descendantIndex = c.stackIndices[last]
	}
	c.stack = c.stack[:last]
	if len(c.stackIndices) > last {
		c.stackIndices = c.stackIndices[:last]
	}
	return true
}

// normalizeFallbackShadowLocked rebuilds the value-style path when a native
// cursor operation could not provide a descendant-index accessor.  Older
// bridge modules may expose movement and current-node exports but omit the
// optional index export; carrying the previous index into a fallback move
// would then make every subsequent sibling/descendant index drift.  The
// caller must hold c.mu.  A failed reconstruction leaves the conservative
// shadow untouched and marks it unknown, so navigation still remains safe.
func (c *TreeCursor) normalizeFallbackShadowLocked() {
	if c == nil || c.node.handle == 0 || c.node.tree == nil || c.root.handle == 0 || c.root.tree == nil {
		return
	}
	if c.descendantIndexKnown {
		return
	}
	ancestors, indices, index, ok := fallbackPathToNode(c.root, c.node)
	if !ok {
		return
	}
	c.stack = ancestors
	c.stackIndices = indices
	c.descendantIndex = index
	c.descendantIndexKnown = true
}

// cursorPairMu serializes operations that need to inspect or mutate two
// cursors at once (currently ResetTo).  A TreeCursor is intentionally mutable,
// so taking both per-cursor locks is required for a coherent snapshot.  The
// small coordination mutex avoids lock-order inversions when two goroutines
// concurrently call a.ResetTo(b) and b.ResetTo(a), and also handles the
// self-reset case without attempting to lock the same mutex twice.
var cursorPairMu sync.Mutex

// Cursor is a concise alias for TreeCursor.
type Cursor = TreeCursor

// saturatingU32Add adds two wire-sized indexes without wrapping.  A cursor
// index is represented as uint32 by the ABI; if a compatibility tree would
// place the result beyond that domain, retaining the maximum value is safer
// than turning it into a seemingly valid low index.
func saturatingU32Add(a, b uint32) uint32 {
	if uint64(a)+uint64(b) > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return a + b
}

// Handle returns the opaque guest cursor handle.  A zero value means that the
// cursor is using the Go fallback or has been closed.  The accessor is useful
// for diagnostics and mirrors the handle helpers exposed by Runtime, Parser,
// Tree, and Query; it does not transfer ownership.
func (c *TreeCursor) Handle() uint32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// A cursor allocation only remains meaningful while its backing tree and
	// runtime are alive. Tree.Close deliberately does not need to know about
	// every cursor, so check those owners here instead of exposing a native
	// handle that now points into freed tree memory.
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return 0
	}
	return c.handle
}

// newTreeCursor creates a cursor rooted at node and prefers the native ABI.
func newTreeCursor(node Node) *TreeCursor {
	if node.handle == 0 || node.tree == nil {
		return nil
	}
	// Keep closed-tree nodes from producing a cursor that can never be used.
	// This lifecycle check is independent of the optional node-is-null export,
	// so legacy bridges without that accessor still work for live nodes.
	if node.tree.ensureOpen() != nil {
		return nil
	}
	c := &TreeCursor{node: node, root: node, tree: node.tree}
	if node.tree != nil && node.tree.rt != nil {
		tree := node.tree
		tree.mu.RLock()
		var result []uint64
		var err error
		if tree.ensureOpen() == nil {
			result, _, err = tree.rt.call(context.Background(), []string{
				"tsw_cursor_new", "tsw_tree_cursor_new", "sitterwasm_cursor_new", "sitterwasm_tree_cursor_new",
			}, uint64(node.handle))
		}
		tree.mu.RUnlock()
		if err == nil && len(result) != 0 {
			if handle, ok := checkedU32(result[0]); ok && handle != 0 {
				c.handle = handle
			}
		}
	}
	goruntime.SetFinalizer(c, func(cursor *TreeCursor) { _ = cursor.Close() })
	return c
}

// CurrentNode returns the node at the cursor's current position.
func (c *TreeCursor) CurrentNode() Node {
	if c == nil || c.closed.Load() {
		return Node{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return Node{}
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		tree := c.tree
		tree.mu.RLock()
		var result []uint64
		var err error
		if tree.ensureOpen() == nil {
			result, _, err = tree.rt.call(context.Background(), []string{
				"tsw_cursor_current_node", "tsw_tree_cursor_current_node", "sitterwasm_cursor_current_node", "sitterwasm_tree_cursor_current_node",
			}, uint64(c.handle))
		}
		tree.mu.RUnlock()
		if err == nil && len(result) != 0 {
			if handle, ok := checkedU32(result[0]); ok && handle != 0 {
				c.node = tree.registerNode(handle)
				return c.node
			}
			// A malformed/non-wasm32 handle cannot safely be retained. Detach
			// the native cursor and continue with the last coherent shadow.
			if result[0] > uint64(^uint32(0)) {
				c.invalidateNativeLocked(tree.rt)
			}
		}
		// A cursor handle without a usable current-node accessor cannot be
		// kept in sync with the value-style shadow.  Detach it immediately so
		// subsequent movement falls back from the last known node instead of
		// continuing to mutate an opaque cursor whose position we cannot see.
		c.invalidateNativeLocked(tree.rt)
	}
	// Do not expose a stale value-style node after its owning tree has begun
	// closing.  Node methods independently treat such wrappers as null, but
	// returning a null cursor position here keeps CurrentNode consistent with
	// the native API and avoids retaining a misleading handle.
	if c.tree == nil || c.tree.ensureOpen() != nil {
		return Node{}
	}
	return c.node
}

// Node is an alias for CurrentNode.
func (c *TreeCursor) Node() Node { return c.CurrentNode() }

// CurrentFieldName returns the current node's field name, if available.
func (c *TreeCursor) CurrentFieldName() string {
	if c == nil || c.closed.Load() {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ""
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		r := c.tree.rt
		tree := c.tree
		var value string
		var found bool
		var malformedHandle bool
		tree.mu.RLock()
		if tree.ensureOpen() == nil {
			r.mu.Lock()
			result, _, err := r.callLocked(context.Background(), []string{
				"tsw_cursor_current_field_name_ptr", "tsw_cursor_current_field_name",
				"tsw_tree_cursor_current_field_name_ptr", "tsw_tree_cursor_current_field_name",
				"sitterwasm_cursor_current_field_name_ptr", "sitterwasm_cursor_current_field_name",
				"sitterwasm_tree_cursor_current_field_name_ptr", "sitterwasm_tree_cursor_current_field_name",
			}, uint64(c.handle))
			if err == nil && len(result) != 0 {
				ptr, ptrOK := checkedU32(result[0])
				if !ptrOK {
					// Invalidation performs a Runtime call, so defer it until
					// after releasing r.mu to avoid recursive locking.
					malformedHandle = true
					ptr = 0
				}
				if ptr != 0 && len(result) >= 2 {
					if mem := r.mod.Memory(); mem != nil {
						length, lengthOK := checkedU32(result[1])
						if lengthOK {
							if b, ok := mem.Read(ptr, length); ok {
								value, found = string(b), true
							}
						}
					}
				}
				if ptr != 0 && !found {
					if s, readErr := r.readCString(ptr); readErr == nil {
						value, found = s, true
					}
				}
			}
			r.mu.Unlock()
		}
		tree.mu.RUnlock()
		if malformedHandle {
			c.invalidateNativeLocked(r)
		}
		if found {
			return value
		}
	}
	// Fallback for modules without a native cursor.
	// The node used to construct (or most recently Reset) the cursor is the
	// cursor root.  Tree-sitter records no field for that synthetic root even
	// when the same node happens to have a field in its containing tree.  Do
	// this check before consulting Parent so a cursor rooted at a subtree does
	// not accidentally report the field that led to that subtree.
	if len(c.stack) == 0 || (c.root.handle != 0 && c.node.Equal(c.root)) {
		return ""
	}
	node := c.node
	parent := node.Parent()
	if parent.handle == 0 || parent.tree == nil {
		return ""
	}
	for i := 0; i < parent.ChildCount(); i++ {
		if parent.Child(i).Equal(node) {
			return parent.FieldNameForChild(i)
		}
	}
	return ""
}

// FieldName is an alias for CurrentFieldName.
func (c *TreeCursor) FieldName() string { return c.CurrentFieldName() }

// CurrentFieldID returns the numerical field id of the current node.
func (c *TreeCursor) CurrentFieldID() uint16 {
	if c == nil || c.closed.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		result, err := c.nativeCallLocked([]string{
			"tsw_cursor_current_field_id", "tsw_tree_cursor_current_field_id",
			"sitterwasm_cursor_current_field_id", "sitterwasm_tree_cursor_current_field_id",
		}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			// TSFieldId is a 16-bit value.  Do not silently truncate a
			// malformed compatibility module's wider result; dropping the
			// native handle lets the deterministic node/language fallback take
			// over for subsequent calls.
			if result[0] <= uint64(^uint16(0)) {
				return uint16(result[0])
			}
			c.invalidateNativeLocked(c.tree.rt)
		} else if err != nil || len(result) == 0 {
			// An optional accessor may be absent on older bridges.  Once it
			// fails, detach the opaque cursor so later navigation cannot keep
			// retrying a handle whose native state we can no longer inspect.
			c.invalidateNativeLocked(c.tree.rt)
		}
	}
	// Compatibility cursors do not have a native field-id accessor, but the
	// node/language ABI is sufficient to derive the same value.  Do this while
	// retaining c.mu (rather than calling CurrentFieldName, which would try to
	// lock c.mu recursively).
	if c.node.handle == 0 || c.node.tree == nil || len(c.stack) == 0 || (c.root.handle != 0 && c.node.Equal(c.root)) {
		return 0
	}
	parent := c.node.Parent()
	if parent.handle == 0 || parent.tree == nil {
		return 0
	}
	for i := 0; i < parent.ChildCount(); i++ {
		if parent.Child(i).Equal(c.node) {
			name := parent.FieldNameForChild(i)
			if name == "" {
				return 0
			}
			if language := c.node.Language(); language != nil {
				return language.FieldIdForName(name)
			}
			return 0
		}
	}
	return 0
}

// FieldID is an alias for CurrentFieldID.
func (c *TreeCursor) FieldID() uint16 { return c.CurrentFieldID() }

// FieldId is the spelling used by go-tree-sitter.
func (c *TreeCursor) FieldId() uint16 { return c.CurrentFieldID() }

// CurrentFieldId is the mixed-case spelling used by a few older bindings.
// Keep it alongside CurrentFieldID so callers can migrate without an
// adapter; both methods report the same Tree-sitter field identifier.
func (c *TreeCursor) CurrentFieldId() uint16 { return c.CurrentFieldID() }

// nativeCallLocked performs a cursor ABI call while retaining the owning
// tree's read lock. Tree.Close takes the corresponding write lock before
// deleting the guest TSTree, preventing cursor/tree use-after-free races.
// The caller must hold c.mu.
func (c *TreeCursor) nativeCallLocked(names []string, args ...uint64) ([]uint64, error) {
	if c == nil || c.handle == 0 || c.tree == nil || c.tree.rt == nil {
		return nil, ErrInvalidHandle
	}
	tree := c.tree
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	if err := tree.ensureOpen(); err != nil {
		return nil, err
	}
	result, _, err := tree.rt.call(context.Background(), names, args...)
	return result, err
}

// nativeMoveArgsLocked invokes a cursor operation that takes arguments and
// refreshes the Go-side current-node shadow after a successful call.  The
// caller must hold c.mu.  Keeping the tree read lock across both the movement
// and current-node lookup prevents Tree.Close from deleting the backing tree
// between the two guest calls.  A missing current-node accessor does not make
// the movement itself fail; the native result is still returned to the caller.
// The third return value reports whether the helper had to rebuild the Go
// fallback path because the optional native descendant-index accessor was not
// available.  Callers that normally append/pop their own shadow stack entries
// must skip that adjustment when this flag is true: the rebuilt path already
// contains the exact post-move ancestors.
func (c *TreeCursor) nativeMoveArgsLocked(names []string, args ...uint64) (used bool, result []uint64, shadowRebuilt bool) {
	if c == nil || c.handle == 0 || c.tree == nil || c.tree.rt == nil {
		return false, nil, false
	}
	// Keep a complete pre-call snapshot.  A partially implemented bridge can
	// successfully mutate the guest cursor and then fail to expose its current
	// node; in that case this helper detaches the native handle and the caller
	// retries through the Go shadow.  Restoring every shadow component (including
	// the ancestor slices) is essential for that retry to start at the same
	// position rather than combining a pre-call node with a post-call index/path.
	oldNode := c.node
	oldStack := append([]Node(nil), c.stack...)
	oldStackIndices := append([]uint32(nil), c.stackIndices...)
	oldDescendantIndex := c.descendantIndex
	oldDescendantIndexKnown := c.descendantIndexKnown
	restoreShadow := func() {
		c.node = oldNode
		c.stack = append(c.stack[:0], oldStack...)
		c.stackIndices = append(c.stackIndices[:0], oldStackIndices...)
		c.descendantIndex = oldDescendantIndex
		c.descendantIndexKnown = oldDescendantIndexKnown
	}
	tree := c.tree
	tree.mu.RLock()
	if err := tree.ensureOpen(); err != nil {
		tree.mu.RUnlock()
		return false, nil, false
	}
	result, _, err := tree.rt.call(context.Background(), names, append([]uint64{uint64(c.handle)}, args...)...)
	if err != nil {
		tree.mu.RUnlock()
		// The guest may have consumed the movement before reporting a trap.
		// Restore the pre-call shadow before detaching the native cursor so a
		// compatibility retry cannot combine an old node with a new path.
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, nil, false
	}
	// The operation has definitely been consumed by the native cursor.  Try to
	// refresh the value-style Node shadow, but do not turn an optional/missing
	// accessor into a false movement failure.
	current, _, currentErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_node", "tsw_tree_cursor_current_node", "sitterwasm_cursor_current_node", "sitterwasm_tree_cursor_current_node",
	}, uint64(c.handle))
	indexKnown := false
	if indexResult, _, indexErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index",
		"sitterwasm_cursor_current_descendant_index", "sitterwasm_tree_cursor_current_descendant_index",
	}, uint64(c.handle)); indexErr == nil && len(indexResult) != 0 {
		if value, ok := checkedU32(indexResult[0]); ok {
			c.descendantIndex = value
			c.descendantIndexKnown = true
			indexKnown = true
		}
	}
	if !indexKnown {
		c.descendantIndexKnown = false
	}
	tree.mu.RUnlock()
	if currentErr != nil || len(current) == 0 {
		// The native cursor has moved, but without a current-node accessor the
		// value-style shadow cannot be kept coherent.  Drop the opaque handle and
		// let the caller's Go fallback perform the operation from the old shadow.
		// This also prevents a later native call from jumping back to an unknown
		// position after a partial/legacy ABI failure.
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, nil, false
	}
	h, ok := checkedU32(current[0])
	if !ok || h == 0 {
		// A successful movement must still leave a usable current node. If
		// the optional accessor returned a malformed/null handle, detach the
		// opaque cursor and let the caller retry through the Go shadow.
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, nil, false
	}
	c.node = tree.registerNode(h)
	if c.node.handle == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, nil, false
	}
	// Older/partial bridges may expose movement and current-node accessors but
	// omit current_descendant_index (or return a value wider than wasm32). The
	// native move has already changed position, so retaining the pre-move index
	// would make an immediate DescendantIndex call stale. Rebuild the visible
	// pre-order path while the current node is known; this also gives a coherent
	// shadow if a later optional accessor causes native fallback.
	if !indexKnown {
		if ancestors, indices, index, ok := fallbackPathToNode(c.root, c.node); ok {
			c.stack = ancestors
			c.stackIndices = indices
			c.descendantIndex = index
			c.descendantIndexKnown = true
			shadowRebuilt = true
		}
	}
	return true, result, shadowRebuilt
}

// invalidateNativeLocked detaches a cursor from its guest allocation.  The
// caller must hold c.mu and must not hold the owning Tree's mutex; deletion is
// intentionally performed after the movement helper releases its tree read
// lock.  Keeping this transition in one helper makes partial native ABI
// failures fall back atomically instead of leaving a stale handle paired with
// a Go shadow at a different position.
func (c *TreeCursor) invalidateNativeLocked(rt *Runtime) {
	if c == nil || c.handle == 0 {
		return
	}
	// The native cursor is authoritative while it is attached, including the
	// structural descendant index that Tree-sitter exposes after reverse
	// sibling traversal.  If we have to detach it (for example because an
	// optional accessor is missing), rebuild the value-style shadow first so a
	// subsequent fallback operation starts from a coherent visible path rather
	// than carrying that structural index into the Go implementation.  Callers
	// must not hold the owning tree's mutex here; fallbackPathToNode performs
	// ordinary node queries while reconstructing the path.
	if c.node.handle != 0 && c.node.tree != nil && c.root.handle != 0 && c.root.tree != nil {
		if ancestors, indices, index, ok := fallbackPathToNode(c.root, c.node); ok {
			c.stack = ancestors
			c.stackIndices = indices
			c.descendantIndex = index
			c.descendantIndexKnown = true
		}
	}
	handle := c.handle
	c.handle = 0
	if rt == nil && c.tree != nil {
		rt = c.tree.rt
	}
	_ = deleteCursorHandle(rt, handle)
}

// CurrentDepth returns the number of ancestors below the cursor root.
func (c *TreeCursor) CurrentDepth() int {
	if c == nil || c.closed.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return 0
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		result, err := c.nativeCallLocked([]string{
			"tsw_cursor_current_depth", "tsw_tree_cursor_current_depth",
			"sitterwasm_cursor_current_depth", "sitterwasm_tree_cursor_current_depth",
		}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			if value, ok := checkedU32(result[0]); ok {
				// CurrentDepth is exposed as int for compatibility with the
				// value-oriented API, while the guest result is wasm32. On a
				// 32-bit host a validly encoded uint32 can still exceed MaxInt
				// and converting directly would produce a negative depth. Treat
				// that malformed-for-host value as a native ABI failure and use
				// the bounded Go shadow instead.
				if depth, depthOK := hostInt(value); depthOK {
					return depth
				}
				c.invalidateNativeLocked(c.tree.rt)
			} else {
				c.invalidateNativeLocked(c.tree.rt)
			}
		} else if err != nil || len(result) == 0 {
			c.invalidateNativeLocked(c.tree.rt)
		}
	}
	if c.node.handle == 0 || c.node.tree == nil {
		return 0
	}
	return len(c.stack)
}

// Depth is an alias for CurrentDepth.
func (c *TreeCursor) Depth() uint32 { return uint32(c.CurrentDepth()) }

// DescendantIndex returns the current node's pre-order index relative to the
// cursor root.
func (c *TreeCursor) DescendantIndex() uint32 {
	if c == nil || c.closed.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return 0
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		result, err := c.nativeCallLocked([]string{
			"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index",
			"sitterwasm_cursor_current_descendant_index", "sitterwasm_tree_cursor_current_descendant_index",
		}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			if value, ok := checkedU32(result[0]); ok {
				return value
			}
			c.invalidateNativeLocked(c.tree.rt)
		} else if err != nil || len(result) == 0 {
			c.invalidateNativeLocked(c.tree.rt)
		}
	}
	if c.node.handle == 0 || c.node.tree == nil {
		return 0
	}
	return c.descendantIndex
}

// GoToFirstChild moves to the first child and reports whether one exists.
func (c *TreeCursor) GoToFirstChild() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	// A partially implemented native bridge may leave us with a current-node
	// shadow but no descendant-index accessor.  Normalize that shadow before
	// deriving indexes in the Go fallback path.
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	if used, moved, shadowRebuilt := c.nativeMoveLocked("tsw_cursor_goto_first_child", "tsw_tree_cursor_goto_first_child", "sitterwasm_cursor_goto_first_child", "sitterwasm_tree_cursor_goto_first_child"); used {
		if moved && !shadowRebuilt && oldNode.handle != 0 {
			// Retain the position occupied before the native move so a later
			// ABI fallback still reports depth/parent information correctly.
			c.stack = append(c.stack, oldNode)
			c.stackIndices = append(c.stackIndices, oldIndex)
		}
		return moved
	}
	child := c.node.Child(0)
	if child.handle == 0 || child.tree == nil {
		return false
	}
	parentIndex := c.descendantIndex
	c.pushFallbackAncestor()
	c.node = child
	c.descendantIndex = fallbackChildDescendantIndex(oldNode, parentIndex, 0)
	return true
}

// GotoFirstChild is the upstream spelling.
func (c *TreeCursor) GotoFirstChild() bool { return c.GoToFirstChild() }

// GoToLastChild moves to the last child and reports whether one exists.
func (c *TreeCursor) GoToLastChild() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	if used, moved, shadowRebuilt := c.nativeMoveLocked("tsw_cursor_goto_last_child", "tsw_tree_cursor_goto_last_child", "sitterwasm_cursor_goto_last_child", "sitterwasm_tree_cursor_goto_last_child"); used {
		if moved && !shadowRebuilt && oldNode.handle != 0 {
			c.stack = append(c.stack, oldNode)
			c.stackIndices = append(c.stackIndices, oldIndex)
		}
		return moved
	}
	count := c.node.ChildCount()
	if count == 0 {
		return false
	}
	child := c.node.Child(count - 1)
	if child.handle == 0 || child.tree == nil {
		return false
	}
	parentIndex := c.descendantIndex
	c.pushFallbackAncestor()
	c.node = child
	c.descendantIndex = fallbackChildDescendantIndex(oldNode, parentIndex, count-1)
	return true
}

// GotoLastChild is the upstream spelling.
func (c *TreeCursor) GotoLastChild() bool { return c.GoToLastChild() }

// GoToFirstChildForByte moves to the first child extending past offset.
func (c *TreeCursor) GoToFirstChildForByte(offset uint32) bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	var oldShadow cursorShadow
	if c.handle != 0 {
		oldShadow = c.snapshotShadowLocked()
	}
	if used, result, shadowRebuilt := c.nativeMoveArgsLocked([]string{
		"tsw_cursor_goto_first_child_for_byte", "tsw_tree_cursor_goto_first_child_for_byte", "sitterwasm_cursor_goto_first_child_for_byte", "sitterwasm_tree_cursor_goto_first_child_for_byte",
	}, uint64(offset)); used {
		if len(result) == 0 {
			// nativeMoveArgsLocked refreshes the node shadow before returning.
			// A missing result is a malformed optional ABI, not a successful
			// movement; restore the pre-call shadow before detaching the native
			// cursor so callers never observe a hidden state change on failure.
			c.restoreShadowLocked(oldShadow)
			c.invalidateNativeLocked(c.tree.rt)
			return false
		}
		index := i64(result[0])
		if index < 0 {
			c.restoreShadowLocked(oldShadow)
			return false
		}
		// The C API returns an int64 only to provide -1 as the failure
		// sentinel; successful child indexes are still uint32 values on the
		// wire. Reject a wider compatibility result instead of reporting a
		// successful move that cannot be represented by the Go API.
		if uint64(index) > uint64(^uint32(0)) {
			c.restoreShadowLocked(oldShadow)
			c.invalidateNativeLocked(c.tree.rt)
			return false
		}
		if !shadowRebuilt && oldNode.handle != 0 {
			c.stack = append(c.stack, oldNode)
			c.stackIndices = append(c.stackIndices, oldIndex)
		}
		return true
	}
	for i := 0; i < c.node.ChildCount(); i++ {
		child := c.node.Child(i)
		if child.handle != 0 && child.tree != nil && child.EndByte() > offset {
			parent := c.node
			parentIndex := c.descendantIndex
			c.pushFallbackAncestor()
			c.node = child
			c.descendantIndex = fallbackChildDescendantIndex(parent, parentIndex, i)
			return true
		}
	}
	return false
}

// GotoFirstChildForByte is the upstream index-returning spelling. A nil
// result means that no child extends past offset.
func (c *TreeCursor) GotoFirstChildForByte(offset uint32) *uint {
	if c == nil || c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return nil
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	var oldShadow cursorShadow
	if c.handle != 0 {
		oldShadow = c.snapshotShadowLocked()
	}
	if used, result, shadowRebuilt := c.nativeMoveArgsLocked([]string{
		"tsw_cursor_goto_first_child_for_byte", "tsw_tree_cursor_goto_first_child_for_byte", "sitterwasm_cursor_goto_first_child_for_byte", "sitterwasm_tree_cursor_goto_first_child_for_byte",
	}, uint64(offset)); used {
		if len(result) != 0 {
			idx := i64(result[0])
			if idx < 0 || uint64(idx) > uint64(^uint32(0)) {
				c.restoreShadowLocked(oldShadow)
				if idx >= 0 {
					// A child index is a wasm32 value.  Treat a wider
					// positive result as malformed rather than truncating it
					// into a different child on 32/64-bit hosts.
					c.invalidateNativeLocked(c.tree.rt)
				}
				return nil
			}
			if !shadowRebuilt && oldNode.handle != 0 {
				c.stack = append(c.stack, oldNode)
				c.stackIndices = append(c.stackIndices, oldIndex)
			}
			v := uint(idx)
			return &v
		}
		c.restoreShadowLocked(oldShadow)
		// These APIs are required to return an i64 child index (with -1 as
		// the failure sentinel). An empty result is therefore a malformed or
		// partial export; detach the guest cursor so its potentially consumed
		// movement cannot diverge from the restored Go shadow.
		c.invalidateNativeLocked(c.tree.rt)
		return nil
	}
	for i := 0; i < c.node.ChildCount(); i++ {
		child := c.node.Child(i)
		if child.handle != 0 && child.tree != nil && child.EndByte() > offset {
			parent := c.node
			parentIndex := c.descendantIndex
			c.pushFallbackAncestor()
			c.node = child
			c.descendantIndex = fallbackChildDescendantIndex(parent, parentIndex, i)
			v := uint(i)
			return &v
		}
	}
	return nil
}

// GotoFirstChildForByte32 is a fixed-width convenience variant for callers
// that prefer an explicitly 32-bit index on all architectures.
func (c *TreeCursor) GotoFirstChildForByte32(offset uint32) *uint32 {
	index := c.GotoFirstChildForByte(offset)
	if index == nil {
		return nil
	}
	v := uint32(*index)
	return &v
}

// GoToFirstChildForPoint is the point-coordinate variant.
func (c *TreeCursor) GoToFirstChildForPoint(point Point) bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	var oldShadow cursorShadow
	if c.handle != 0 {
		oldShadow = c.snapshotShadowLocked()
	}
	if used, result, shadowRebuilt := c.nativeMoveArgsLocked([]string{
		"tsw_cursor_goto_first_child_for_point", "tsw_tree_cursor_goto_first_child_for_point", "sitterwasm_cursor_goto_first_child_for_point", "sitterwasm_tree_cursor_goto_first_child_for_point",
	}, packPoint(point)); used {
		if len(result) == 0 {
			c.restoreShadowLocked(oldShadow)
			c.invalidateNativeLocked(c.tree.rt)
			return false
		}
		index := i64(result[0])
		if index < 0 {
			c.restoreShadowLocked(oldShadow)
			return false
		}
		if uint64(index) > uint64(^uint32(0)) {
			c.restoreShadowLocked(oldShadow)
			c.invalidateNativeLocked(c.tree.rt)
			return false
		}
		if !shadowRebuilt && oldNode.handle != 0 {
			c.stack = append(c.stack, oldNode)
			c.stackIndices = append(c.stackIndices, oldIndex)
		}
		return true
	}
	for i := 0; i < c.node.ChildCount(); i++ {
		child := c.node.Child(i)
		// The C cursor helper checks both the byte and point extents.  Its
		// point variant passes a zero byte goal, so a zero-width child at byte
		// zero is not considered a match even when its point lies after the
		// requested point.  Keep that subtle boundary rule in the fallback.
		if child.handle != 0 && child.tree != nil && child.EndByte() > 0 && comparePoint(child.EndPoint(), point) > 0 {
			parent := c.node
			parentIndex := c.descendantIndex
			c.pushFallbackAncestor()
			c.node = child
			c.descendantIndex = fallbackChildDescendantIndex(parent, parentIndex, i)
			return true
		}
	}
	return false
}

// GotoFirstChildForPoint is the upstream index-returning spelling.
func (c *TreeCursor) GotoFirstChildForPoint(point Point) *uint {
	if c == nil || c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return nil
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	var oldShadow cursorShadow
	if c.handle != 0 {
		oldShadow = c.snapshotShadowLocked()
	}
	if used, result, shadowRebuilt := c.nativeMoveArgsLocked([]string{
		"tsw_cursor_goto_first_child_for_point", "tsw_tree_cursor_goto_first_child_for_point", "sitterwasm_cursor_goto_first_child_for_point", "sitterwasm_tree_cursor_goto_first_child_for_point",
	}, packPoint(point)); used {
		if len(result) != 0 {
			idx := i64(result[0])
			if idx < 0 || uint64(idx) > uint64(^uint32(0)) {
				c.restoreShadowLocked(oldShadow)
				if idx >= 0 {
					c.invalidateNativeLocked(c.tree.rt)
				}
				return nil
			}
			if !shadowRebuilt && oldNode.handle != 0 {
				c.stack = append(c.stack, oldNode)
				c.stackIndices = append(c.stackIndices, oldIndex)
			}
			v := uint(idx)
			return &v
		}
		c.restoreShadowLocked(oldShadow)
		c.invalidateNativeLocked(c.tree.rt)
		return nil
	}
	for i := 0; i < c.node.ChildCount(); i++ {
		child := c.node.Child(i)
		if child.handle != 0 && child.tree != nil && child.EndByte() > 0 && comparePoint(child.EndPoint(), point) > 0 {
			parent := c.node
			parentIndex := c.descendantIndex
			c.pushFallbackAncestor()
			c.node = child
			c.descendantIndex = fallbackChildDescendantIndex(parent, parentIndex, i)
			v := uint(i)
			return &v
		}
	}
	return nil
}

// GotoFirstChildForPoint32 is a fixed-width convenience variant for callers
// that prefer an explicitly 32-bit index on all architectures.
func (c *TreeCursor) GotoFirstChildForPoint32(point Point) *uint32 {
	index := c.GotoFirstChildForPoint(point)
	if index == nil {
		return nil
	}
	v := uint32(*index)
	return &v
}

// GoToNextSibling moves to the next sibling.
func (c *TreeCursor) GoToNextSibling() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	oldNode := c.node
	oldIndex := c.descendantIndex
	if used, moved, shadowRebuilt := c.nativeMoveLocked("tsw_cursor_goto_next_sibling", "tsw_tree_cursor_goto_next_sibling", "sitterwasm_cursor_goto_next_sibling", "sitterwasm_tree_cursor_goto_next_sibling"); used {
		// nativeMoveLocked refreshes descendantIndex from the guest whenever
		// the accessor is available.  Keep that structural value intact: the
		// upstream cursor deliberately exposes it (and it can differ from a
		// visible pre-order index after reverse-sibling traversal through hidden
		// grammar nodes).  Only synthesize an index when the optional accessor
		// was unavailable and no path reconstruction was possible.
		if moved && !shadowRebuilt && !c.descendantIndexKnown {
			c.descendantIndex = saturatingU32Add(oldIndex, fallbackVisibleSubtreeCount(oldNode))
			c.descendantIndexKnown = true
		}
		return moved
	}
	// A cursor cannot walk outside the node it was created/reset with.  The
	// value-style fallback uses Node.NextSibling, which naturally follows the
	// containing tree and therefore needs this explicit root boundary check.
	if len(c.stack) == 0 || (c.root.handle != 0 && c.node.Equal(c.root)) {
		return false
	}
	next := c.node.NextSibling()
	if next.handle == 0 || next.tree == nil {
		return false
	}
	c.node = next
	c.descendantIndex = saturatingU32Add(oldIndex, fallbackVisibleSubtreeCount(oldNode))
	return true
}

// GotoNextSibling is the upstream spelling.
func (c *TreeCursor) GotoNextSibling() bool { return c.GoToNextSibling() }

// GoToPreviousSibling moves to the previous sibling.
func (c *TreeCursor) GoToPreviousSibling() bool { return c.GoToPrevSibling() }

// GotoPreviousSibling is the upstream spelling.
func (c *TreeCursor) GotoPreviousSibling() bool { return c.GoToPrevSibling() }

// GoToPrevSibling moves to the previous sibling.
func (c *TreeCursor) GoToPrevSibling() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	if used, moved, _ := c.nativeMoveLocked("tsw_cursor_goto_previous_sibling", "tsw_cursor_goto_prev_sibling", "tsw_tree_cursor_goto_previous_sibling", "tsw_tree_cursor_goto_prev_sibling", "sitterwasm_cursor_goto_previous_sibling", "sitterwasm_cursor_goto_prev_sibling", "sitterwasm_tree_cursor_goto_previous_sibling", "sitterwasm_tree_cursor_goto_prev_sibling"); used {
		// Keep the native cursor attached after a successful movement.  The
		// upstream C API intentionally exposes its structural descendant index,
		// which can differ from a visible pre-order index after reverse traversal
		// through hidden repetition nodes.  Detaching here would silently change
		// the documented Tree-sitter semantics.  If a later optional call forces
		// fallback, invalidateNativeLocked reconstructs the visible shadow first.
		return moved
	}
	// See GoToNextSibling: the parent tree may have siblings for a subtree
	// root, but those are outside this cursor's permitted traversal region.
	if len(c.stack) == 0 || (c.root.handle != 0 && c.node.Equal(c.root)) {
		return false
	}
	prev := c.node.PrevSibling()
	if prev.handle == 0 || prev.tree == nil {
		return false
	}
	// Reconstruct the path from the cursor root instead of assuming that the
	// reverse iterator's descendant index is zero.  Tree-sitter's native
	// cursor stores structural (including hidden/repetition) entries and its
	// historical reverse-sibling implementation can consequently expose odd
	// index values for some grammars.  The compatibility cursor only sees the
	// visible Node API, so the stable and useful contract here is the visible
	// pre-order index documented by DescendantIndex.  Rebuilding the path also
	// keeps a subsequent GoToNextSibling/GotoDescendant operation coherent.
	if ancestors, indices, index, ok := fallbackPathToNode(c.root, prev); ok {
		c.node = prev
		c.stack = ancestors
		c.stackIndices = indices
		c.descendantIndex = index
		c.descendantIndexKnown = true
	} else {
		// This should only be reachable for a malformed/legacy bridge whose
		// sibling accessor returned a node outside the cursor root.  Preserve
		// the movement while retaining the previous conservative index rather
		// than manufacturing an unrelated value.
		c.node = prev
		c.descendantIndexKnown = false
	}
	return true
}

// GotoPrevSibling is a concise alias used by a few older bindings.
func (c *TreeCursor) GotoPrevSibling() bool { return c.GoToPrevSibling() }

// GoToNextNamedSibling moves to the next named sibling.
func (c *TreeCursor) GoToNextNamedSibling() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	for c.GoToNextSibling() {
		if c.Node().IsNamed() {
			return true
		}
	}
	return false
}

// GoToPreviousNamedSibling moves to the previous named sibling.
func (c *TreeCursor) GoToPreviousNamedSibling() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	for c.GoToPrevSibling() {
		if c.Node().IsNamed() {
			return true
		}
	}
	return false
}

// GoToPrevNamedSibling is a concise alias.
func (c *TreeCursor) GoToPrevNamedSibling() bool { return c.GoToPreviousNamedSibling() }

// GotoNextNamedSibling is an upstream-style alias.
func (c *TreeCursor) GotoNextNamedSibling() bool { return c.GoToNextNamedSibling() }

// GotoPreviousNamedSibling is an upstream-style alias.
func (c *TreeCursor) GotoPreviousNamedSibling() bool { return c.GoToPreviousNamedSibling() }

// GoToParent moves to the parent, stopping at the cursor root.
func (c *TreeCursor) GoToParent() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return false
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	if used, moved, shadowRebuilt := c.nativeMoveLocked("tsw_cursor_goto_parent", "tsw_tree_cursor_goto_parent", "sitterwasm_cursor_goto_parent", "sitterwasm_tree_cursor_goto_parent"); used {
		if moved && !shadowRebuilt && len(c.stack) > 0 {
			last := len(c.stack) - 1
			c.stack = c.stack[:last]
			if len(c.stackIndices) > last {
				c.descendantIndex = c.stackIndices[last]
				c.stackIndices = c.stackIndices[:last]
			}
		}
		return moved
	}
	// The fallback stack is empty exactly at the cursor root.  Treat that as
	// the authoritative boundary as well as comparing nodes: legacy bridges
	// may omit stable node equality metadata, and a failed equality probe must
	// never let a subtree-rooted cursor escape into its containing tree.
	if len(c.stack) == 0 || c.node.Equal(c.root) {
		return false
	}
	parent := c.node.Parent()
	if parent.handle == 0 || parent.tree == nil {
		return false
	}
	// The shadow stack stores the exact ancestor node/index pair. Use it when
	// available; deriving the index from a full tree walk would lose the
	// historical reverse-sibling semantics that Tree-sitter exposes.
	if len(c.stack) > 0 {
		_ = c.popFallbackAncestor()
	} else {
		c.node = parent
		c.descendantIndex = 0
	}
	return true
}

// GotoParent is the upstream spelling.
func (c *TreeCursor) GotoParent() bool { return c.GoToParent() }

// GotoDescendant moves to the descendant at a pre-order index.
func (c *TreeCursor) GotoDescendant(index uint32) {
	if c == nil || c.closed.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.tree == nil || c.tree.closed.Load() || c.tree.rt == nil || c.tree.rt.closedState() {
		return
	}
	if c.handle == 0 {
		c.normalizeFallbackShadowLocked()
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		if used := c.nativeGotoDescendantLocked(index); used {
			if ancestors, indices, currentIndex, ok := fallbackPathToNode(c.root, c.node); ok {
				c.stack = ancestors
				c.stackIndices = indices
				// The native cursor exposes its own descendant index, which can
				// include structural entries that are not represented by the
				// value-style Node tree. Preserve it when the optional accessor
				// supplied one; use the reconstructed visible index only when the
				// bridge has no such accessor.
				if !c.descendantIndexKnown {
					c.descendantIndex = currentIndex
				}
			} else {
				c.stack = cursorAncestorStack(c.root, c.node)
				c.stackIndices = make([]uint32, len(c.stack))
			}
			return
		}
	}
	// Tree-sitter defines the index relative to the cursor's original root,
	// regardless of the current position.  When the requested index is outside
	// that subtree, the native implementation ascends to the root and leaves
	// the cursor there.  Reset the fallback shadow up front so it has the same
	// behavior (the old implementation left a deep current node untouched for
	// an out-of-range index).
	current := c.root
	c.node = current
	c.stack = c.stack[:0]
	c.stackIndices = c.stackIndices[:0]
	c.descendantIndex = 0
	if index == 0 {
		return
	}
	// Walk visible nodes in pre-order while carrying each node's index. This is
	// equivalent to the native cursor for ordinary grammars and, unlike the old
	// decrement-only implementation, does not accidentally count a subtree's
	// descendants twice when a target lies after it.
	var walk func(Node, uint32, []Node, []uint32) bool
	walk = func(n Node, nIndex uint32, ancestors []Node, ancestorIndices []uint32) bool {
		next := uint64(nIndex) + 1
		if next > uint64(^uint32(0)) {
			return false
		}
		for i := 0; i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.handle == 0 || child.tree == nil {
				continue
			}
			childIndex := uint32(next)
			if childIndex == index {
				c.node = child
				c.stack = append(c.stack[:0], ancestors...)
				c.stack = append(c.stack, n)
				c.stackIndices = append(c.stackIndices[:0], ancestorIndices...)
				c.stackIndices = append(c.stackIndices, nIndex)
				c.descendantIndex = childIndex
				return true
			}
			childCount := uint64(fallbackVisibleSubtreeCount(child))
			if uint64(index) > uint64(childIndex) && uint64(index) < uint64(childIndex)+childCount {
				path := append(append([]Node(nil), ancestors...), n)
				indices := append(append([]uint32(nil), ancestorIndices...), nIndex)
				if walk(child, childIndex, path, indices) {
					return true
				}
			}
			next += childCount
			if next > uint64(^uint32(0))+1 {
				next = uint64(^uint32(0)) + 1
			}
		}
		return false
	}
	_ = walk(current, 0, nil, nil)
}

// cursorAncestorStack reconstructs the visible ancestor path expected by the
// Go fallback cursor.  Native cursors keep this path in their private stack,
// while the opaque ABI exposes only the current node; retaining a shadow path
// lets us continue with correct Depth/GotoParent behavior if a later native
// operation is unavailable.  The returned slice excludes current and includes
// the cursor root when current is a descendant of root.
func cursorAncestorStack(root, current Node) []Node {
	if root.handle == 0 || current.handle == 0 {
		return nil
	}
	if root.Equal(current) {
		return nil
	}
	reversed := make([]Node, 0, 4)
	for node := current; node.handle != 0 && !node.Equal(root); {
		parent := node.Parent()
		if parent.handle == 0 {
			return nil
		}
		reversed = append(reversed, parent)
		node = parent
	}
	if len(reversed) == 0 {
		return nil
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

// fallbackPathToNode returns the visible ancestor path and pre-order indexes
// for target. It is used only to seed the shadow state after a native cursor
// movement; failure simply yields an empty path and a zero index.
func fallbackPathToNode(root, target Node) ([]Node, []uint32, uint32, bool) {
	if root.handle == 0 || root.tree == nil || target.handle == 0 || target.tree == nil {
		return nil, nil, 0, false
	}
	var walk func(Node, uint32, []Node, []uint32) ([]Node, []uint32, uint32, bool)
	walk = func(node Node, index uint32, ancestors []Node, indices []uint32) ([]Node, []uint32, uint32, bool) {
		if node.Equal(target) {
			return ancestors, indices, index, true
		}
		next := uint64(index) + 1
		if next > uint64(^uint32(0)) {
			// No representable descendant can follow an index at the wire
			// maximum.  Keep walking out of the branch only if a caller asks
			// for the same saturated value; otherwise there is no valid match.
			next = uint64(^uint32(0)) + 1
		}
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.handle == 0 || child.tree == nil {
				continue
			}
			if next > uint64(^uint32(0)) {
				break
			}
			childIndex := uint32(next)
			path := append(append([]Node(nil), ancestors...), node)
			pathIndices := append(append([]uint32(nil), indices...), index)
			if resultPath, resultIndices, resultIndex, ok := walk(child, childIndex, path, pathIndices); ok {
				return resultPath, resultIndices, resultIndex, true
			}
			next += uint64(fallbackVisibleSubtreeCount(child))
			if next > uint64(^uint32(0))+1 {
				next = uint64(^uint32(0)) + 1
			}
		}
		return nil, nil, 0, false
	}
	return walk(root, 0, nil, nil)
}

// lockCursorTreePair acquires read locks for up to two trees.  Tree.Close
// takes the corresponding write lock, so a caller may safely use handles from
// either tree while the returned unlock function is outstanding.  Acquisition
// is coordinated with the same treePairMu used by Tree.ChangedRanges to avoid
// opposite-order deadlocks for two distinct trees.
func lockCursorTreePair(first, second *Tree) func() {
	if first == nil && second == nil {
		return func() {}
	}
	treePairMu.Lock()
	if first != nil {
		first.mu.RLock()
	}
	if second != nil && second != first {
		second.mu.RLock()
	}
	treePairMu.Unlock()
	return func() {
		if second != nil && second != first {
			second.mu.RUnlock()
		}
		if first != nil {
			first.mu.RUnlock()
		}
	}
}

// deleteCursorHandle releases an opaque native cursor.  Cursor deletion only
// frees the cursor's private traversal stack; it does not dereference the
// backing tree.  Consequently it remains safe (and important for avoiding a
// leak) after Tree.Close has already reclaimed that tree.  Runtime.call still
// serializes the operation with Runtime.Close and gracefully handles a module
// that has already gone away.
func deleteCursorHandle(rt *Runtime, handle uint32) error {
	if rt == nil || handle == 0 || rt.closedState() {
		return nil
	}
	_, _, err := rt.call(context.Background(), []string{
		"tsw_cursor_delete", "tsw_tree_cursor_delete", "sitterwasm_cursor_delete", "sitterwasm_tree_cursor_delete",
	}, uint64(handle))
	if err != nil && (isUnsupported(err) || err == ErrClosed) {
		return nil
	}
	return err
}

// Reset repositions the cursor at node and makes it the new root.
//
// The value-oriented API normally passes a Node value, while the established
// go-tree-sitter bindings use *Node.  Accepting either representation here is
// useful at this boundary because Reset is a mutating, no-error compatibility
// method (invalid or nil nodes are simply ignored, matching the zero-value
// behavior of the rest of this cursor implementation).  The strongly typed
// ResetNode helper below is available to callers that prefer compile-time
// checking.
func (c *TreeCursor) Reset(value any) {
	node, ok := nodeValueArg(value)
	if !ok || c == nil || c.closed.Load() || node.handle == 0 || node.tree == nil || node.tree.rt == nil {
		return
	}
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return
	}
	oldTree, oldHandle := c.tree, c.handle
	var oldRT *Runtime
	if oldTree != nil {
		oldRT = oldTree.rt
	}
	newTree := node.tree
	// Keep both the destination cursor's current tree and the requested node's
	// tree alive while the optional native reset reads their opaque handles.
	unlockTrees := lockCursorTreePair(oldTree, newTree)
	native := false
	if oldHandle != 0 && oldRT != nil && oldRT == newTree.rt &&
		(oldTree == nil || oldTree.ensureOpen() == nil) && newTree.ensureOpen() == nil {
		_, _, err := oldRT.call(context.Background(), []string{
			"tsw_cursor_reset", "tsw_tree_cursor_reset", "sitterwasm_cursor_reset", "sitterwasm_tree_cursor_reset",
		}, uint64(oldHandle), uint64(node.handle))
		native = err == nil
	}
	var staleRT *Runtime
	var staleHandle uint32
	// A closed target node is not a valid reset destination.  Leave the cursor
	// untouched in that case; this mirrors the safe value semantics used by the
	// rest of the package and avoids installing a dangling Node.
	if newTree.ensureOpen() != nil {
		if native && oldHandle != 0 {
			// The native reset may have copied a target tree that was closed while
			// the guest call was in flight.  Do not leave the destination handle
			// paired with its old Go tree; invalidate and reclaim it before return.
			staleRT, staleHandle = oldRT, oldHandle
			c.handle = 0
		}
		unlockTrees()
		c.mu.Unlock()
		if staleHandle != 0 {
			_ = deleteCursorHandle(staleRT, staleHandle)
		}
		return
	}
	if !native && oldHandle != 0 {
		// A failed/unsupported native reset leaves the guest cursor's old state
		// intact.  Invalidate it before switching the shadow to another tree so
		// subsequent navigation cannot send that handle to the wrong runtime or
		// observe stale position data.  Delete it after releasing Go locks.
		staleRT, staleHandle = oldRT, oldHandle
		c.handle = 0
	}
	c.node, c.root, c.tree = node, node, newTree
	c.stack = c.stack[:0]
	c.stackIndices = c.stackIndices[:0]
	c.descendantIndex = 0
	unlockTrees()
	c.mu.Unlock()
	if staleHandle != 0 {
		_ = deleteCursorHandle(staleRT, staleHandle)
	}
}

// ResetNode is the explicitly value-shaped spelling of Reset.  It is handy
// for code that wants to avoid the small interface dispatch incurred by the
// pointer/value compatibility form.
func (c *TreeCursor) ResetNode(node Node) { c.Reset(node) }

// ResetTo copies the current position and root information from another
// cursor.  Both cursors are snapshotted while holding their locks, then the
// native operation (when available) is performed while both backing trees are
// kept alive.  The coordination mutex handles self-reset and opposite-order
// concurrent calls without ever taking one cursor's mutex recursively.
func (c *TreeCursor) ResetTo(other *TreeCursor) {
	if c == nil || other == nil || c == other || c.closed.Load() || other.closed.Load() {
		return
	}
	cursorPairMu.Lock()
	c.mu.Lock()
	other.mu.Lock()
	if c.closed.Load() || other.closed.Load() {
		other.mu.Unlock()
		c.mu.Unlock()
		cursorPairMu.Unlock()
		return
	}

	// Snapshot all source state before doing any guest call.  Calling
	// other.CurrentNode here would recursively lock other.mu (and was the
	// original self-deadlock); the shadow is refreshed after every native move.
	srcNode, srcRoot, srcTree := other.node, other.root, other.tree
	srcStack := append([]Node(nil), other.stack...)
	srcStackIndices := append([]uint32(nil), other.stackIndices...)
	srcDescendantIndex := other.descendantIndex
	srcDescendantIndexKnown := other.descendantIndexKnown
	if srcNode.handle == 0 || srcTree == nil || srcTree.rt == nil || srcNode.tree != srcTree {
		other.mu.Unlock()
		c.mu.Unlock()
		cursorPairMu.Unlock()
		return
	}
	oldTree, oldHandle := c.tree, c.handle
	var oldRT *Runtime
	if oldTree != nil {
		oldRT = oldTree.rt
	}

	unlockTrees := lockCursorTreePair(oldTree, srcTree)
	native := false
	if oldHandle != 0 && other.handle != 0 && oldRT != nil && oldRT == srcTree.rt &&
		(oldTree == nil || oldTree.ensureOpen() == nil) && srcTree.ensureOpen() == nil {
		_, _, err := oldRT.call(context.Background(), []string{
			"tsw_cursor_reset_to", "tsw_tree_cursor_reset_to", "sitterwasm_cursor_reset_to", "sitterwasm_tree_cursor_reset_to",
		}, uint64(oldHandle), uint64(other.handle))
		native = err == nil
	}
	var staleRT *Runtime
	var staleHandle uint32
	// Do not install a source node after its tree was closed while locks were
	// held.  The source cursor remains unchanged and the destination is left as
	// it was, just as for an invalid Reset target.
	if srcTree.ensureOpen() != nil {
		if native && oldHandle != 0 {
			// As in Reset, a source tree can be marked closed immediately after
			// the guest copied its cursor.  Drop the now-unsafe native state while
			// both tree locks still protect the source allocation.
			staleRT, staleHandle = oldRT, oldHandle
			c.handle = 0
		}
		unlockTrees()
		other.mu.Unlock()
		c.mu.Unlock()
		cursorPairMu.Unlock()
		if staleHandle != 0 {
			_ = deleteCursorHandle(staleRT, staleHandle)
		}
		return
	}

	if !native && oldHandle != 0 {
		staleRT, staleHandle = oldRT, oldHandle
		c.handle = 0
	}
	c.node, c.root, c.tree = srcNode, srcRoot, srcTree
	c.stack = append(c.stack[:0], srcStack...)
	c.stackIndices = append(c.stackIndices[:0], srcStackIndices...)
	c.descendantIndex = srcDescendantIndex
	c.descendantIndexKnown = srcDescendantIndexKnown
	unlockTrees()
	other.mu.Unlock()
	c.mu.Unlock()
	cursorPairMu.Unlock()
	if staleHandle != 0 {
		_ = deleteCursorHandle(staleRT, staleHandle)
	}
}

// Copy returns an independent cursor at the same position.
func (c *TreeCursor) Copy() *TreeCursor {
	if c == nil || c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	if c.closed.Load() || c.tree == nil || c.node.handle == 0 || c.tree.ensureOpen() != nil {
		c.mu.Unlock()
		return nil
	}
	defer c.mu.Unlock()
	out := &TreeCursor{
		node:                 c.node,
		root:                 c.root,
		tree:                 c.tree,
		stack:                append([]Node(nil), c.stack...),
		stackIndices:         append([]uint32(nil), c.stackIndices...),
		descendantIndex:      c.descendantIndex,
		descendantIndexKnown: c.descendantIndexKnown,
	}
	if c.handle != 0 && c.tree != nil && c.tree.rt != nil {
		result, err := c.nativeCallLocked([]string{
			"tsw_cursor_copy", "tsw_tree_cursor_copy", "sitterwasm_cursor_copy", "sitterwasm_tree_cursor_copy",
		}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			if handle, ok := checkedU32(result[0]); ok {
				out.handle = handle
			}
		}
	}
	goruntime.SetFinalizer(out, func(cursor *TreeCursor) { _ = cursor.Close() })
	return out
}

// Close invalidates the cursor and releases its native guest allocation.
func (c *TreeCursor) Close() error {
	if c == nil || c.closed.Swap(true) {
		return nil
	}
	goruntime.SetFinalizer(c, nil)
	c.mu.Lock()
	handle, tree := c.handle, c.tree
	var rt *Runtime
	if tree != nil {
		rt = tree.rt
	}
	c.handle = 0
	c.stack = nil
	c.stackIndices = nil
	c.descendantIndex = 0
	c.node, c.root, c.tree = Node{}, Node{}, nil
	c.mu.Unlock()
	if handle != 0 {
		// Deletion is safe even when Tree.Close won the race (the native helper
		// only releases the cursor's private stack), and doing it here avoids
		// leaking every cursor that outlives its tree.  Runtime.call serializes
		// this with a concurrent Runtime.Close.
		if err := deleteCursorHandle(rt, handle); err != nil {
			return err
		}
	}
	return nil
}

// nativeMoveLocked invokes a native bool-returning cursor operation. The
// caller must hold c.mu. It refreshes c.node so value methods stay coherent.
func (c *TreeCursor) nativeMoveLocked(names ...string) (used, moved, shadowRebuilt bool) {
	if c.handle == 0 || c.tree == nil || c.tree.rt == nil {
		return false, false, false
	}
	// A native movement may succeed while a compatibility bridge's optional
	// current-node export fails.  Snapshot the complete Go shadow so the caller
	// can safely retry through fallback after we detach that opaque handle.
	oldNode := c.node
	oldStack := append([]Node(nil), c.stack...)
	oldStackIndices := append([]uint32(nil), c.stackIndices...)
	oldDescendantIndex := c.descendantIndex
	oldDescendantIndexKnown := c.descendantIndexKnown
	restoreShadow := func() {
		c.node = oldNode
		c.stack = append(c.stack[:0], oldStack...)
		c.stackIndices = append(c.stackIndices[:0], oldStackIndices...)
		c.descendantIndex = oldDescendantIndex
		c.descendantIndexKnown = oldDescendantIndexKnown
	}
	tree := c.tree
	tree.mu.RLock()
	if err := tree.ensureOpen(); err != nil {
		tree.mu.RUnlock()
		return false, false, false
	}
	result, _, err := tree.rt.call(context.Background(), names, uint64(c.handle))
	if err != nil || len(result) == 0 {
		tree.mu.RUnlock()
		// A partially implemented bridge can mutate its cursor before
		// returning a trap or an empty result vector. Restore the complete
		// value-style shadow before detaching the native handle so the caller's
		// fallback retry starts at the pre-call position.
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, false, false
	}
	moved = result[0] != 0
	var current []uint64
	current, _, _ = tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_node", "tsw_tree_cursor_current_node", "sitterwasm_cursor_current_node", "sitterwasm_tree_cursor_current_node",
	}, uint64(c.handle))
	indexKnown := false
	if indexResult, _, indexErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index",
		"sitterwasm_cursor_current_descendant_index", "sitterwasm_tree_cursor_current_descendant_index",
	}, uint64(c.handle)); indexErr == nil && len(indexResult) != 0 {
		if value, ok := checkedU32(indexResult[0]); ok {
			c.descendantIndex = value
			c.descendantIndexKnown = true
			indexKnown = true
		}
	}
	if !indexKnown {
		c.descendantIndexKnown = false
	}
	// Do not call registerNode while holding RLock (it upgrades to a write
	// lock). Keep the returned handle and register after unlocking below.
	tree.mu.RUnlock()
	if len(current) == 0 {
		// The movement result cannot be reflected in the Go Node shadow.  Drop
		// the native handle and let the caller retry through its deterministic
		// Go fallback, preserving a single coherent state.
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, false, false
	}
	currentHandle, currentOK := checkedU32(current[0])
	if !currentOK || currentHandle == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, false, false
	}
	c.node = tree.registerNode(currentHandle)
	if c.node.handle == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false, false, false
	}
	if !indexKnown {
		if ancestors, indices, index, ok := fallbackPathToNode(c.root, c.node); ok {
			c.stack = ancestors
			c.stackIndices = indices
			c.descendantIndex = index
			c.descendantIndexKnown = true
			shadowRebuilt = true
		}
	}
	return true, moved, shadowRebuilt
}

// nativeGotoDescendantLocked invokes the void-returning Tree-sitter cursor
// operation and refreshes the Go shadow from the guest.  Unlike the other
// movement helpers, ts_tree_cursor_goto_descendant has no return value; using
// nativeMoveArgsLocked for it would therefore mistake a perfectly valid call
// (whose result vector is empty) for an ABI failure and immediately fall back
// from a cursor that has already moved.
//
// The caller must hold c.mu.  As with nativeMoveLocked, a missing current-node
// accessor causes the opaque cursor to be detached and the complete pre-call
// shadow to be restored before the caller retries through the Go fallback.
func (c *TreeCursor) nativeGotoDescendantLocked(index uint32) bool {
	if c == nil || c.handle == 0 || c.tree == nil || c.tree.rt == nil {
		return false
	}
	oldNode := c.node
	oldStack := append([]Node(nil), c.stack...)
	oldStackIndices := append([]uint32(nil), c.stackIndices...)
	oldDescendantIndex := c.descendantIndex
	oldDescendantIndexKnown := c.descendantIndexKnown
	restoreShadow := func() {
		c.node = oldNode
		c.stack = append(c.stack[:0], oldStack...)
		c.stackIndices = append(c.stackIndices[:0], oldStackIndices...)
		c.descendantIndex = oldDescendantIndex
		c.descendantIndexKnown = oldDescendantIndexKnown
	}
	tree := c.tree
	tree.mu.RLock()
	if err := tree.ensureOpen(); err != nil {
		tree.mu.RUnlock()
		return false
	}
	_, _, callErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_goto_descendant", "tsw_tree_cursor_goto_descendant",
		"sitterwasm_cursor_goto_descendant", "sitterwasm_tree_cursor_goto_descendant",
	}, uint64(c.handle), uint64(index))
	if callErr != nil {
		tree.mu.RUnlock()
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false
	}
	current, _, currentErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_node", "tsw_tree_cursor_current_node",
		"sitterwasm_cursor_current_node", "sitterwasm_tree_cursor_current_node",
	}, uint64(c.handle))
	indexResult, _, indexErr := tree.rt.call(context.Background(), []string{
		"tsw_cursor_current_descendant_index", "tsw_tree_cursor_current_descendant_index",
		"sitterwasm_cursor_current_descendant_index", "sitterwasm_tree_cursor_current_descendant_index",
	}, uint64(c.handle))
	indexKnown := false
	if indexErr == nil && len(indexResult) != 0 {
		if value, ok := checkedU32(indexResult[0]); ok {
			c.descendantIndex = value
			c.descendantIndexKnown = true
			indexKnown = true
		}
	}
	if !indexKnown {
		c.descendantIndexKnown = false
	}
	tree.mu.RUnlock()
	if currentErr != nil || len(current) == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false
	}
	h, ok := checkedU32(current[0])
	if !ok || h == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false
	}
	c.node = tree.registerNode(h)
	if c.node.handle == 0 {
		restoreShadow()
		c.invalidateNativeLocked(tree.rt)
		return false
	}
	if !indexKnown {
		if ancestors, indices, currentIndex, pathOK := fallbackPathToNode(c.root, c.node); pathOK {
			c.stack = ancestors
			c.stackIndices = indices
			c.descendantIndex = currentIndex
			c.descendantIndexKnown = true
		}
	}
	return true
}

// i64 decodes a signed i64 WebAssembly result.
func i64(v uint64) int64 { return int64(v) }
