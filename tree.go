package wasitter

import (
	"context"
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"sync"
	"sync/atomic"
)

// Tree is a syntax tree returned by Parser.Parse.
type Tree struct {
	parser *Parser
	rt     *Runtime
	// handle is atomic so callers may inspect Handle while Close is running.
	// Tree operations still use mu to keep a live handle paired with its guest
	// tree; the atomic prevents stale reads in value-style helpers.
	handle atomic.Uint32

	mu sync.RWMutex
	// lifeMu is a separate lifetime gate for operations (such as native query
	// cursor execution) that may call several Node accessors while retaining a
	// tree.  Tree.Close takes this lock before deleting the guest TSTree.  It is
	// intentionally separate from mu: callers may hold lifeMu.RLock while a
	// Node method takes mu.RLock recursively, which would be unsafe if a writer
	// were already queued on the same RWMutex.
	lifeMu sync.RWMutex
	closed atomic.Bool
	source []byte
	// nodes tracks guest SWNode wrappers allocated by the bridge. Tree-sitter's
	// native TSNode is a value, but the WASM ABI uses an opaque allocation; all
	// wrappers are reclaimed when the owning tree closes.
	nodes map[uint32]struct{}
	// fieldLanguage caches the immutable language reported by a node of this
	// tree. It is guarded by rt.mu, alongside the runtime's field ID cache.
	fieldLanguage uint32
}

// treePairMu serializes acquisition of two tree read locks. Without this
// small guard, simultaneous two-tree operations (ChangedRanges, node
// comparisons, cursor resets, and child-with-descendant queries) could acquire
// the locks in opposite order and deadlock. It is held only while the
// per-tree locks are acquired, so unrelated single-tree operations remain
// concurrent.
var treePairMu sync.Mutex

// treeLifePairMu coordinates acquisition of two lifetime gates.  Query
// cursors can switch execution between trees, and concurrent switches in
// opposite directions must not deadlock while a Tree.Close waits for either
// gate.  The gate itself is separate from treePairMu/tree.mu because callers
// may need to take a tree read lock recursively while the lifetime is held.
var treeLifePairMu sync.Mutex

// lockTreeLifePair retains one or two trees until the returned unlock function
// is called. Tree.Close takes the corresponding write gate before deleting the
// guest tree. The helper is deliberately small and nil-tolerant so it can be
// used by cursor Exec/Close paths without special-case unlock logic.
func lockTreeLifePair(first, second *Tree) func() {
	if first == nil && second == nil {
		return func() {}
	}
	treeLifePairMu.Lock()
	if first != nil {
		first.lifeMu.RLock()
	}
	if second != nil && second != first {
		second.lifeMu.RLock()
	}
	treeLifePairMu.Unlock()
	return func() {
		if second != nil && second != first {
			second.lifeMu.RUnlock()
		}
		if first != nil {
			first.lifeMu.RUnlock()
		}
	}
}

func (t *Tree) registerNode(handle uint32) Node {
	if handle == 0 || t == nil {
		return Node{}
	}
	t.mu.Lock()
	// Runtime.Close can invalidate the entire guest module without taking the
	// tree lock.  Treat that state like a closed tree before registering a
	// freshly returned wrapper; otherwise callers briefly receive a Node whose
	// handle can never be used and whose wrapper cannot be reclaimed normally.
	if t.closed.Load() || t.handle.Load() == 0 || t.rt == nil || t.rt.closedState() {
		t.mu.Unlock()
		// Root/child calls can race with Tree.Close. Reclaim a wrapper that
		// was produced after closure instead of returning a dangling Node.
		if t.rt != nil && !t.rt.closedState() {
			_, _, _ = t.rt.call(context.Background(), []string{"tsw_node_delete", "wasitter_node_delete", "node_delete"}, uint64(handle))
		}
		return Node{}
	}
	if t.nodes == nil {
		t.nodes = make(map[uint32]struct{})
	}
	t.nodes[handle] = struct{}{}
	t.mu.Unlock()
	return Node{tree: t, handle: handle}
}

func (t *Tree) ensureOpen() error {
	if t == nil || t.closed.Load() || t.handle.Load() == 0 {
		return ErrClosed
	}
	if t.rt == nil {
		return ErrNoRuntime
	}
	return t.rt.ensureOpen()
}

func (t *Tree) runtime() *Runtime {
	if t == nil {
		return nil
	}
	return t.rt
}

// Handle returns the opaque guest tree handle.
func (t *Tree) Handle() uint32 {
	if t == nil || t.closed.Load() || t.rt == nil || t.rt.closedState() {
		return 0
	}
	h := t.handle.Load()
	// Close flips the lifecycle bit before waiting for the tree lock. Recheck
	// after loading the token so callers never observe a guest handle once a
	// close has begun (the atomic handle alone would otherwise briefly expose
	// a stale value during that window).
	if t.closed.Load() || t.rt.closedState() {
		return 0
	}
	return h
}

// Language returns the grammar that produced this tree. The guest
// ts_tree_language accessor is queried first; a parser-owned fallback keeps
// trees from older bridge modules usable.
func (t *Tree) Language() *Language {
	l, _ := t.LanguageE()
	return l
}

// LanguageE is the error-returning form of Language.
func (t *Tree) LanguageE() (*Language, error) {
	if t == nil {
		return nil, ErrClosed
	}
	t.mu.RLock()
	if err := t.ensureOpen(); err != nil {
		t.mu.RUnlock()
		return nil, err
	}
	result, name, err := t.rt.call(context.Background(), []string{
		"tsw_tree_language",
		"wasitter_tree_language",
		"ts_tree_language",
		"tree_language",
	}, uint64(t.handle.Load()))
	if err == nil {
		if t.rt.closedState() {
			t.mu.RUnlock()
			return nil, ErrClosed
		}
		if len(result) != 0 {
			languageHandle, handleOK := checkedU32(result[0])
			if !handleOK {
				t.mu.RUnlock()
				return nil, &ABIError{Function: name, Message: "returned a non-wasm32 language handle"}
			}
			if languageHandle != 0 {
				language := &Language{runtime: t.rt, handle: languageHandle, export: name}
				t.mu.RUnlock()
				return language, nil
			}
		}
		// A null language is a valid result for an unattached tree. Prefer a
		// parser fallback when one is available, as Parse always attaches one.
		parser := t.parser
		t.mu.RUnlock()
		if parser != nil {
			return parser.Language(), nil
		}
		return nil, nil
	}
	if !isUnsupported(err) {
		t.mu.RUnlock()
		return nil, err
	}
	parser := t.parser
	t.mu.RUnlock()
	if parser != nil {
		if language := parser.Language(); language != nil {
			return language, nil
		}
	}
	return nil, fmt.Errorf("%w: tree language accessor", err)
}

// RootNode returns the tree's root node. If the tree is closed or the guest
// does not provide a root export, it returns a null Node; use RootNodeE for the
// corresponding error.
func (t *Tree) RootNode() Node {
	n, _ := t.RootNodeE()
	return n
}

// RootNodeError is an alternate spelling for RootNodeE retained for callers
// that prefer an explicit error suffix.
func (t *Tree) RootNodeError() (Node, error) { return t.RootNodeE() }

// RootNodeE returns the tree's root node and any ABI error.
func (t *Tree) RootNodeE() (Node, error) {
	if t == nil {
		return Node{}, ErrClosed
	}
	t.mu.RLock()
	if err := t.ensureOpen(); err != nil {
		t.mu.RUnlock()
		return Node{}, err
	}
	result, name, err := t.rt.call(context.Background(), []string{"tsw_tree_root", "tsw_tree_root_node", "wasitter_tree_root", "ts_tree_root_node", "tree_root"}, uint64(t.handle.Load()))
	t.mu.RUnlock()
	if err != nil {
		return Node{}, err
	}
	if len(result) == 0 {
		return Node{}, ErrInvalidHandle
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return Node{}, &ABIError{Function: name, Message: "returned a non-wasm32 node handle"}
	}
	if handle == 0 {
		return Node{}, ErrInvalidHandle
	}
	return t.registerNode(handle), nil
}

// RootNodeWithOffset returns the root shifted by a UTF-8 byte offset and
// point offset. It returns [ErrUnsupported] when the module lacks this operation.
func (t *Tree) RootNodeWithOffset(offset uint32, offsetPoint Point) (Node, error) {
	if t == nil {
		return Node{}, ErrClosed
	}
	offsetBytes := offset
	t.mu.RLock()
	if err := t.ensureOpen(); err != nil {
		t.mu.RUnlock()
		return Node{}, err
	}
	result, name, err := t.rt.call(context.Background(), []string{"tsw_tree_root_with_offset", "ts_tree_root_node_with_offset"}, uint64(t.handle.Load()), uint64(offsetBytes), packPoint(offsetPoint))
	t.mu.RUnlock()
	if err != nil {
		return Node{}, err
	}
	if len(result) == 0 {
		return Node{}, ErrInvalidHandle
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return Node{}, &ABIError{Function: name, Message: "returned a non-wasm32 node handle"}
	}
	if handle == 0 {
		return Node{}, ErrInvalidHandle
	}
	return t.registerNode(handle), nil
}

// Copy creates a shallow copy of a syntax tree.
func (t *Tree) Copy() *Tree {
	c, _ := t.CopyE()
	return c
}

// CopyE creates a tree copy and returns any ABI error.
func (t *Tree) CopyE() (*Tree, error) {
	if t == nil {
		return nil, ErrClosed
	}
	t.mu.RLock()
	if err := t.ensureOpen(); err != nil {
		t.mu.RUnlock()
		return nil, err
	}
	result, name, err := t.rt.call(context.Background(), []string{"tsw_tree_copy", "wasitter_tree_copy", "ts_tree_copy", "tree_copy"}, uint64(t.handle.Load()))
	src := append([]byte(nil), t.source...)
	t.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, ErrInvalidHandle
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 tree handle"}
	}
	if handle == 0 {
		return nil, ErrInvalidHandle
	}
	// Runtime.Close may have won a race immediately after the guest copy
	// completed.  Returning a wrapper around a tree in a module that is already
	// closed is surprising (and leaks the freshly allocated guest tree when the
	// module remains open long enough to observe it), so discard it and report a
	// deterministic lifecycle error.
	// Serialize the final lifecycle check with Runtime.Close.  Without taking
	// Runtime.mu here, Close could mark the runtime closed between the check
	// and wrapper construction, returning a tree that is already unusable at
	// the instant it is observed by the caller.
	t.rt.mu.Lock()
	if t.rt.closedState() {
		// Runtime.Close marks the runtime closed before waiting for its mutex.
		// If it is waiting now, the freshly copied tree still exists in the
		// guest and must be released before we hand control back to Close.  The
		// helper intentionally bypasses the normal lifecycle check while this
		// lock is held.  If the module was already torn down, the helper is a
		// harmless no-op.
		deleteGuestTreeWhileLocked(t.rt, handle)
		t.rt.mu.Unlock()
		return nil, ErrClosed
	}
	// Keep Runtime.mu held through wrapper publication. Runtime.Close marks
	// the runtime closed before waiting for this lock; releasing it before
	// constructing the Go wrapper would allow Close to win the tiny gap and
	// return a tree whose guest module was already torn down. Object creation
	// itself performs no guest calls, so retaining the lock here is safe.
	copyTree := &Tree{parser: t.parser, rt: t.rt, source: src, nodes: make(map[uint32]struct{})}
	copyTree.handle.Store(handle)
	goruntime.SetFinalizer(copyTree, func(tree *Tree) { _ = tree.Close() })
	t.rt.mu.Unlock()
	return copyTree, nil
}

// Edit updates tree coordinates after a source change. Describe the edit
// using UTF-8 byte offsets and points, then pass this tree to [Parser.ParseContext]
// along with the updated source to reuse unchanged portions of the tree.
func (t *Tree) Edit(edit InputEdit) error {
	if err := t.ensureOpen(); err != nil {
		return err
	}

	// TSInputEdit is nine little-endian uint32 values (36 bytes).  The shared
	// encoder also resolves the upstream *Position compatibility aliases on
	// InputEdit, keeping Tree.Edit and Node.Edit byte-for-byte consistent.
	buf := encodeInputEdit(edit)
	// Editing a TSTree mutates its internal coordinate data.  Use the write
	// lock so concurrent node/cursor calls cannot observe a partially edited
	// tree, and so Tree.Close cannot delete it during the edit transaction.
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	r := t.rt
	r.mu.Lock()
	ptr, err := r.allocLocked(uint32(len(buf)))
	if err != nil {
		r.mu.Unlock()
		return err
	}
	defer func() {
		r.freeLocked(ptr)
		r.mu.Unlock()
	}()
	if mem := r.mod.Memory(); mem == nil || !mem.Write(ptr, buf) {
		return io.ErrShortWrite
	}
	fn, name, err := r.function("tsw_tree_edit", "wasitter_tree_edit", "ts_tree_edit", "tree_edit")
	if err != nil {
		// Some experimental bridges expose the nine scalar fields directly.
		if !isUnsupported(err) {
			return err
		}
		fn, name, err = r.function("tsw_tree_edit_scalars", "tree_edit_scalars")
		if err != nil {
			return err
		}
		args := make([]uint64, 10)
		args[0] = uint64(t.handle.Load())
		for i := 0; i < 9; i++ {
			args[i+1] = uint64(getU32(buf, i*4))
		}
		result, callErr := fn.Call(r.ctx, args...)
		if callErr != nil {
			return &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
		}
		if len(result) > 0 && int32(result[0]) < 0 {
			return &ABIError{Function: name, Message: "tree edit rejected"}
		}
		if r.closedState() {
			return ErrClosed
		}
		return nil
	}
	result, callErr := fn.Call(r.ctx, uint64(t.handle.Load()), uint64(ptr))
	if callErr != nil {
		err = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if err != nil {
		return err
	}
	if r.closedState() {
		return ErrClosed
	}
	if len(result) > 0 && int32(result[0]) < 0 {
		return &ABIError{Function: name, Message: "tree edit rejected"}
	}
	return nil
}

// SetSource updates the source bytes retained by this wrapper. Tree-sitter
// itself only tracks coordinates; callers should invoke Edit first when using
// incremental parsing.
func (t *Tree) SetSource(source []byte) error {
	if t == nil {
		return ErrClosed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	t.source = append(t.source[:0], source...)
	return nil
}

// SourceContent returns a copy of the bytes used for parsing.
func (t *Tree) SourceContent() []byte {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]byte(nil), t.source...)
}

// Source returns SourceContent as a string.
func (t *Tree) Source() string { return string(t.SourceContent()) }

// ToSExpression serializes the full tree using Tree-sitter's S-expression
// format. The guest bridge may provide a direct serializer; otherwise this
// falls back to a Go traversal.
func (t *Tree) ToSExpression() string {
	root := t.RootNode()
	return root.ToSExpression()
}

// ToSexp is the upstream spelling.
func (t *Tree) ToSexp() string { return t.ToSExpression() }

// Walk creates a cursor positioned at the root node.
func (t *Tree) Walk() *TreeCursor {
	root := t.RootNode()
	return newTreeCursor(root)
}

// IncludedRanges returns the source ranges that were used to parse this tree.
// For an ordinary parse this is one range covering the complete input. If the
// guest bridge predates the included-ranges ABI, it returns nil; callers that
// need diagnostics can use IncludedRangesE.
func (t *Tree) IncludedRanges() []Range {
	ranges, _ := t.IncludedRangesE()
	return ranges
}

// IncludedRangesE is the error-returning form of IncludedRanges.
func (t *Tree) IncludedRangesE() ([]Range, error) {
	if t == nil {
		return nil, ErrClosed
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if err := t.ensureOpen(); err != nil {
		return nil, err
	}
	r := t.rt
	var ranges []Range
	var temporaryPtr uint32
	r.mu.Lock()
	fn, name, lookupErr := r.function(
		"tsw_tree_included_ranges_into",
		"wasitter_tree_included_ranges_into",
		"ts_tree_included_ranges_into",
		"tree_included_ranges_into",
		"tsw_tree_included_ranges",
		"wasitter_tree_included_ranges",
		"tree_included_ranges",
	)
	if lookupErr == nil {
		params := len(fn.Definition().ParamTypes())
		switch {
		case params == 3:
			// First call with a zero output capacity obtains the total count.
			countResult, callErr := fn.Call(r.Context(), uint64(t.handle.Load()), 0, 0)
			if callErr != nil {
				lookupErr = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
				break
			}
			if len(countResult) == 0 {
				lookupErr = &ABIError{Function: name, Message: "returned no range count"}
				break
			}
			count, countOK := checkedU32(countResult[0])
			if !countOK {
				lookupErr = &ABIError{Function: name, Message: "returned a non-wasm32 range count"}
				break
			}
			if count == 0 {
				break
			}
			if uint64(count) > uint64(^uint32(0))/24 {
				lookupErr = fmt.Errorf("wasitter: included range count overflows wasm32 memory")
				break
			}
			ptr, allocErr := r.allocLocked(count * 24)
			if allocErr != nil {
				lookupErr = allocErr
				break
			}
			temporaryPtr = ptr
			copyResult, copyErr := fn.Call(r.Context(), uint64(t.handle.Load()), uint64(ptr), uint64(count))
			if copyErr != nil {
				lookupErr = &ABIError{Function: name, Message: copyErr.Error(), Cause: copyErr}
				break
			}
			if len(copyResult) == 0 {
				lookupErr = &ABIError{Function: name, Message: "returned no range count"}
				break
			}
			if mem := r.mod.Memory(); mem == nil {
				lookupErr = fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
			} else if b, ok := mem.Read(ptr, count*24); !ok {
				lookupErr = io.ErrUnexpectedEOF
			} else {
				ranges = decodeRanges(b, count)
			}
		case params == 1:
			// Legacy one-argument forms may return a pointer to a block prefixed
			// by a uint32 count, just like the older changed-ranges helper.
			result, callErr := fn.Call(r.Context(), uint64(t.handle.Load()))
			if callErr != nil {
				lookupErr = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
				break
			}
			if len(result) == 0 {
				break
			}
			ptr, ptrOK := checkedU32(result[0])
			if !ptrOK {
				lookupErr = &ABIError{Function: name, Message: "returned a non-wasm32 range pointer"}
				break
			}
			if ptr == 0 {
				break
			}
			var legacyCount uint32
			if mem := r.mod.Memory(); mem == nil {
				lookupErr = fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
			} else {
				count, ok := mem.ReadUint32Le(ptr)
				if !ok {
					lookupErr = io.ErrUnexpectedEOF
				} else {
					legacyCount = count
					payloadBytes := uint64(count) * 24
					if uint64(ptr) > uint64(^uint32(0))-4 ||
						payloadBytes > (uint64(1)<<32)-uint64(ptr)-4 {
						lookupErr = fmt.Errorf("wasitter: included range count overflows wasm32 memory")
					} else if b, ok := mem.Read(ptr+4, uint32(payloadBytes)); !ok {
						lookupErr = io.ErrUnexpectedEOF
					} else {
						ranges = decodeRanges(b, count)
					}
				}
			}
			// The legacy helper's returned block is guest-owned.  Its dedicated
			// ranges_free export takes (ptr, count), while ordinary allocators
			// take one argument; freeRangesLocked performs the arity-safe
			// dispatch and passes the actual count when available.
			r.freeRangesLocked(ptr, legacyCount)
		default:
			lookupErr = &ABIError{Function: name, Message: "unsupported included-ranges signature"}
		}
	}
	if temporaryPtr != 0 {
		r.freeLocked(temporaryPtr)
	}
	r.mu.Unlock()
	if lookupErr != nil {
		if isUnsupported(lookupErr) {
			// A parser that does not expose the optional accessor still has
			// enough information to describe the ordinary full-document range.
			// This mirrors Tree-sitter's default included range and is more useful
			// than silently returning nil.
			source := append([]byte(nil), t.source...)
			return []Range{{StartByte: 0, EndByte: uint32(len(source)), EndPoint: pointForBytes(source)}}, nil
		}
		return nil, lookupErr
	}
	if r.closedState() {
		return nil, ErrClosed
	}
	return ranges, nil
}

// GetChangedRanges compares this tree with a newer tree. If the bridge does
// not expose native changed-range support, a conservative range covering the
// source differences is returned.
func (t *Tree) GetChangedRanges(newTree *Tree) []Range {
	r, _ := t.ChangedRanges(newTree)
	return r
}

// ChangedRanges is the error-returning form of GetChangedRanges.
func (t *Tree) ChangedRanges(newTree *Tree) ([]Range, error) {
	if t == nil {
		return nil, ErrClosed
	}
	if newTree == nil {
		return nil, fmt.Errorf("wasitter: nil new tree")
	}
	if newTree.rt != t.rt {
		return nil, fmt.Errorf("wasitter: trees belong to different runtimes")
	}
	// Hold both tree locks for the complete guest call. Tree.Close marks a tree
	// closed before taking this write lock, so this prevents the backing TSTree
	// (and either SWNode wrapper) from being freed while C reads it.
	treePairMu.Lock()
	t.mu.RLock()
	if newTree != t {
		newTree.mu.RLock()
	}
	treePairMu.Unlock()
	defer t.mu.RUnlock()
	if newTree != t {
		defer newTree.mu.RUnlock()
	}
	if err := t.ensureOpen(); err != nil {
		return nil, err
	}
	if err := newTree.ensureOpen(); err != nil {
		return nil, err
	}
	r := t.rt
	// The _into ABI accepts (old,new,out_ptr,capacity) and returns the total
	// count. The legacy two-argument ABI returns a block prefixed by count.
	var nativeRanges []Range
	nativeOK := false
	r.mu.Lock()
	fn, name, lookupErr := r.function("tsw_tree_changed_ranges_into", "tsw_tree_changed_ranges", "wasitter_tree_changed_ranges_into", "ts_tree_get_changed_ranges", "tsw_tree_get_changed_ranges", "tree_changed_ranges")
	if lookupErr == nil {
		params := len(fn.Definition().ParamTypes())
		if params == 4 {
			countResult, callErr := fn.Call(r.ctx, uint64(t.handle.Load()), uint64(newTree.handle.Load()), 0, 0)
			if callErr == nil && len(countResult) > 0 {
				nativeOK = true
				count, countOK := checkedU32(countResult[0])
				if !countOK {
					lookupErr = &ABIError{Function: name, Message: "returned a non-wasm32 changed-range count"}
					count = 0
				}
				if count > 0 {
					if uint64(count) > uint64(^uint32(0))/24 {
						lookupErr = fmt.Errorf("wasitter: changed-range count overflows wasm32 memory")
						count = 0
					}
				}
				if count > 0 {
					ptr, allocErr := r.allocLocked(count * 24)
					if allocErr != nil {
						lookupErr = allocErr
					} else {
						result, copyErr := fn.Call(r.ctx, uint64(t.handle.Load()), uint64(newTree.handle.Load()), uint64(ptr), uint64(count))
						if copyErr != nil {
							lookupErr = &ABIError{Function: name, Message: copyErr.Error(), Cause: copyErr}
						} else if len(result) == 0 {
							lookupErr = &ABIError{Function: name, Message: "changed-range output was truncated"}
						} else if copied, copiedOK := checkedU32(result[0]); !copiedOK || copied < count {
							lookupErr = &ABIError{Function: name, Message: "changed-range output was truncated"}
						} else if mem := r.mod.Memory(); mem == nil {
							lookupErr = fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
						} else if b, ok := mem.Read(ptr, count*24); !ok {
							lookupErr = io.ErrUnexpectedEOF
						} else {
							nativeRanges = decodeRanges(b, count)
						}
						r.freeLocked(ptr)
					}
				}
			} else if callErr != nil {
				lookupErr = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
			} else {
				lookupErr = &ABIError{Function: name, Message: "returned no changed-range count"}
			}
		} else if params == 2 {
			result, callErr := fn.Call(r.ctx, uint64(t.handle.Load()), uint64(newTree.handle.Load()))
			if callErr == nil {
				nativeOK = true
			}
			if callErr == nil && len(result) > 0 {
				ptr, ptrOK := checkedU32(result[0])
				if !ptrOK {
					lookupErr = &ABIError{Function: name, Message: "returned a non-wasm32 changed-range pointer"}
					ptr = 0
				}
				if ptr != 0 {
					var legacyCount uint32
					if mem := r.mod.Memory(); mem != nil {
						if count, ok := mem.ReadUint32Le(ptr); ok {
							legacyCount = count
							payloadBytes := uint64(count) * 24
							if uint64(ptr) <= uint64(^uint32(0))-4 &&
								payloadBytes <= (uint64(1)<<32)-uint64(ptr)-4 {
								if b, bok := mem.Read(ptr+4, uint32(payloadBytes)); bok {
									nativeRanges = decodeRanges(b, count)
								} else {
									lookupErr = io.ErrUnexpectedEOF
								}
							} else {
								lookupErr = fmt.Errorf("wasitter: changed-range block overflows wasm32 memory")
							}
						} else {
							lookupErr = io.ErrUnexpectedEOF
						}
					} else {
						lookupErr = fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
					}
					// Legacy range blocks may be owned by a dedicated
					// `(ptr,count)` deallocator rather than the ordinary one-argument
					// allocator.  Dispatch by signature so custom bridges do not leak
					// (or trap from an arity mismatch).
					r.freeRangesLocked(ptr, legacyCount)
				}
			}
			if callErr != nil {
				lookupErr = &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
			}
		} else {
			// Never invoke an export with an unrelated arity: doing so causes a
			// wazero arity trap instead of a recoverable compatibility error.
			lookupErr = fmt.Errorf("%w: changed-ranges export has unsupported signature", ErrUnsupported)
		}
	}
	r.mu.Unlock()
	if !isUnsupported(lookupErr) && lookupErr != nil {
		return nil, lookupErr
	}
	if r.closedState() {
		return nil, ErrClosed
	}
	if nativeOK {
		return nativeRanges, nil
	}
	// Fallback is useful for simple fixture modules and does not claim exact
	// structural ranges: report the complete changed source span.
	oldSource := append([]byte(nil), t.source...)
	newSource := append([]byte(nil), newTree.source...)
	if string(oldSource) == string(newSource) {
		return nil, nil
	}
	end := uint32(len(newSource))
	return []Range{{StartByte: 0, EndByte: end, EndPoint: pointForBytes(newSource)}}, nil
}

func decodeRanges(b []byte, count uint32) []Range {
	const width = 24
	if uint64(count)*width > uint64(len(b)) {
		count = uint32(len(b) / width)
	}
	// A uint32 count may exceed the host int range on 32-bit systems. The
	// backing byte slice necessarily bounds the actual number of records, so
	// clamp to a representable capacity before constructing the Go slice.
	if value, ok := hostInt(count); ok {
		count = uint32(value)
	} else {
		count = uint32(len(b) / width)
	}
	out := make([]Range, 0, int(count))
	for i := uint32(0); i < count; i++ {
		o := int(i) * width
		out = append(out, Range{
			StartByte:  getU32(b, o),
			EndByte:    getU32(b, o+4),
			StartPoint: Point{Row: getU32(b, o+8), Column: getU32(b, o+12)},
			EndPoint:   Point{Row: getU32(b, o+16), Column: getU32(b, o+20)},
		})
	}
	return out
}

func pointForBytes(b []byte) Point {
	var p Point
	for _, c := range b {
		if c == '\n' {
			p.Row++
			p.Column = 0
		} else {
			p.Column++
		}
	}
	return p
}

// Close releases the guest tree. It is idempotent.
func (t *Tree) Close() error {
	if t == nil {
		return nil
	}
	// Acquire the lifetime gate before changing the closed bit or touching the
	// guest tree. Native query-cursor operations hold lifeMu.RLock across all
	// calls that dereference nodes, so this ordering keeps Tree.Close from
	// deleting the backing TSTree underneath an in-flight query. The atomic
	// transition is performed while the gate is held to make repeated Close
	// calls cheap and deterministic.
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()
	if t.closed.Swap(true) {
		return nil
	}
	goruntime.SetFinalizer(t, nil)
	t.mu.Lock()
	if t.rt == nil || t.handle.Load() == 0 {
		t.mu.Unlock()
		return nil
	}
	handle := t.handle.Load()
	t.handle.Store(0)
	nodes := make([]uint32, 0, len(t.nodes))
	for node := range t.nodes {
		nodes = append(nodes, node)
	}
	t.nodes = nil
	t.mu.Unlock()
	if t.rt.closedState() {
		return nil
	}
	// Free node wrappers before the backing tree. A single runtime call lock
	// keeps this safe when several trees are closed concurrently.
	for _, node := range nodes {
		_, _, _ = t.rt.call(context.Background(), []string{"tsw_node_delete", "wasitter_node_delete", "node_delete"}, uint64(node))
	}
	_, _, err := t.rt.call(context.Background(), []string{"tsw_tree_delete", "wasitter_tree_delete", "ts_tree_delete", "tree_delete"}, uint64(handle))
	if errors.Is(err, ErrClosed) {
		return nil
	}
	return err
}
