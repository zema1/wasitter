package sitterwasm

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Node is a lightweight, copyable reference to a syntax-tree node. A Node is
// valid only while its Tree remains open.
type Node struct {
	tree   *Tree
	handle uint32
}

// nodeValueArg accepts both representations used by the established Go
// bindings. A nil node pointer is treated as the null-node value, which keeps
// comparison/navigation helpers total and avoids a panic in compatibility
// callers that use an optional *Node.
func nodeValueArg(value any) (Node, bool) {
	switch node := value.(type) {
	case Node:
		return node, true
	case *Node:
		if node == nil {
			return Node{}, true
		}
		return *node, true
	case nil:
		return Node{}, true
	default:
		return Node{}, false
	}
}

// IsNull reports whether this is the null node returned for a missing child.
func (n Node) IsNull() bool {
	if n.handle == 0 || n.tree == nil {
		return true
	}
	v, err := n.boolValue([]string{"tsw_node_is_null", "sitterwasm_node_is_null", "ts_node_is_null", "node_is_null"})
	// A node whose owning tree has been closed is no longer usable and is
	// treated as null for value-style navigation methods.
	if err != nil {
		// Older bridge modules may expose all navigation accessors but omit the
		// optional null predicate. A non-zero wrapper handle paired with a live
		// tree is enough to identify a usable node in that case; treating the
		// missing export as null would make every compatibility traversal appear
		// empty. Lifecycle and malformed-ABI errors still conservatively produce
		// a null value.
		if errors.Is(err, ErrUnsupported) && n.tree.ensureOpen() == nil {
			return false
		}
		return true
	}
	return v
}

// Valid reports whether the node can still be used.
func (n Node) Valid() bool { return !n.IsNull() }

// IsValid is an alias for Valid.
func (n Node) IsValid() bool { return n.Valid() }

// Handle returns the opaque guest node handle. A node is a value, but its
// backing wrapper belongs to the owning Tree; once that tree (or Runtime) is
// closed, exposing the stale guest pointer would invite accidental reuse, so
// the safe accessor reports zero.
func (n Node) Handle() uint32 {
	if n.handle == 0 || n.tree == nil || n.tree.closed.Load() || n.tree.rt == nil || n.tree.rt.closedState() {
		return 0
	}
	return n.handle
}

// Id returns Tree-sitter's stable node identity.  The ABI wrapper handle is a
// short-lived allocation created for each value crossing the WASM boundary;
// exposing that handle here would make two Go values referring to the same
// syntax node appear unequal.  Newer bridges export tsw_node_id, while older
// bridges fall back to the wrapper handle for compatibility.
func (n Node) Id() uintptr {
	if n.handle == 0 || n.tree == nil {
		return 0
	}
	if value, err := n.uintValue([]string{
		"tsw_node_id", "sitterwasm_node_id", "ts_node_id", "node_id",
	}); err == nil && value != 0 {
		return uintptr(value)
	}
	return uintptr(n.Handle())
}

// ID is an initialism-friendly alias for Id.  Node identities are wasm32
// pointers, so the fixed-width form is useful when crossing another ABI.
func (n Node) ID() uint32 { return uint32(n.Id()) }

func (n Node) ensureOpen() error {
	if n.handle == 0 || n.tree == nil {
		return ErrInvalidHandle
	}
	return n.tree.ensureOpen()
}

func (n Node) call(names []string, args ...uint64) ([]uint64, error) {
	if n.tree == nil {
		return nil, ErrInvalidHandle
	}
	n.tree.mu.RLock()
	defer n.tree.mu.RUnlock()
	if err := n.ensureOpen(); err != nil {
		return nil, err
	}
	all := make([]uint64, 0, len(args)+1)
	all = append(all, uint64(n.handle))
	all = append(all, args...)
	result, _, err := n.tree.rt.call(context.Background(), names, all...)
	return result, err
}

func (n Node) boolValue(names []string) (bool, error) {
	result, err := n.call(names)
	if err != nil {
		return false, err
	}
	if len(result) == 0 {
		return false, ErrInvalidHandle
	}
	return result[0] != 0, nil
}

func (n Node) uintValue(names []string, args ...uint64) (uint32, error) {
	result, err := n.call(names, args...)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU32(result[0])
	if !ok {
		// All scalar values represented by this package are wasm32 values. Do
		// not silently truncate an i64 returned by a malformed/foreign bridge;
		// doing so could turn an invalid offset or count into a valid low
		// address and make subsequent navigation observably wrong.
		return 0, ErrInvalidHandle
	}
	return value, nil
}

// Type returns the grammar node type, or an empty string if unavailable.
func (n Node) Type() string {
	s, _ := n.TypeE()
	return s
}

// TypeE returns the grammar node type and any ABI or lifecycle error.
func (n Node) TypeE() (string, error) {
	return n.stringValue([]string{"tsw_node_type", "sitterwasm_node_type", "ts_node_type", "node_type"})
}

// Kind is an idiomatic alias for Type.
func (n Node) Kind() string { return n.Type() }

// KindID returns the numerical grammar symbol id.
func (n Node) KindID() uint16 {
	v, _ := n.uintValue([]string{"tsw_node_kind_id", "tsw_node_symbol", "sitterwasm_node_kind_id", "ts_node_symbol", "node_kind_id"})
	if v > uint32(^uint16(0)) {
		return 0
	}
	return uint16(v)
}

// Symbol is an alias for KindID.
func (n Node) Symbol() uint16 { return n.KindID() }

// KindId is the upstream spelling of KindID.
func (n Node) KindId() uint16 { return n.KindID() }

// GrammarType returns the grammar-specific type when exposed by the bridge.
func (n Node) GrammarType() string {
	s, _ := n.stringValue([]string{"tsw_node_grammar_type", "sitterwasm_node_grammar_type", "ts_node_grammar_type", "node_grammar_type"})
	return s
}

// GrammarName is an alias for GrammarType.
func (n Node) GrammarName() string { return n.GrammarType() }

// GrammarSymbol returns the grammar-specific symbol id for this node.
func (n Node) GrammarSymbol() uint16 {
	v, _ := n.uintValue([]string{"tsw_node_grammar_symbol", "sitterwasm_node_grammar_symbol", "ts_node_grammar_symbol", "node_grammar_symbol"})
	if v > uint32(^uint16(0)) {
		return 0
	}
	return uint16(v)
}

// GrammarId is the upstream spelling of GrammarSymbol.
func (n Node) GrammarId() uint16 { return n.GrammarSymbol() }

// GrammarID is an initialism-friendly alias for GrammarId.
func (n Node) GrammarID() uint16 { return n.GrammarId() }

// Language returns the grammar associated with this node. The guest
// ts_node_language value is authoritative; falling back to the parser's
// cached language keeps compatibility with older bridges that do not expose
// the accessor.
func (n Node) Language() *Language {
	l, _ := n.LanguageE()
	return l
}

// LanguageE is the error-returning form of Language.
func (n Node) LanguageE() (*Language, error) {
	if err := n.ensureOpen(); err != nil {
		return nil, err
	}
	result, err := n.call([]string{
		"tsw_node_language",
		"sitterwasm_node_language",
		"ts_node_language",
		"node_language",
	})
	if err == nil {
		if len(result) == 0 {
			return nil, ErrInvalidHandle
		}
		languageHandle, handleOK := checkedU32(result[0])
		if !handleOK {
			return nil, &ABIError{Function: "tsw_node_language", Message: "returned a non-wasm32 language handle"}
		}
		if languageHandle == 0 {
			if n.tree != nil && n.tree.parser != nil {
				return n.tree.parser.Language(), nil
			}
			return nil, nil
		}
		return &Language{runtime: n.tree.rt, handle: languageHandle, export: "tsw_node_language"}, nil
	}
	if !isUnsupported(err) {
		return nil, err
	}
	if n.tree != nil && n.tree.parser != nil {
		if language := n.tree.parser.Language(); language != nil {
			return language, nil
		}
	}
	return nil, err
}

// IsNamed reports whether the node is named in the grammar.
func (n Node) IsNamed() bool {
	v, _ := n.boolValue([]string{"tsw_node_is_named", "sitterwasm_node_is_named", "ts_node_is_named", "node_is_named"})
	return v
}

// IsMissing reports whether the node was inserted during error recovery.
func (n Node) IsMissing() bool {
	v, _ := n.boolValue([]string{"tsw_node_is_missing", "sitterwasm_node_is_missing", "ts_node_is_missing", "node_is_missing"})
	return v
}

// IsExtra reports whether the node is an extra token.
func (n Node) IsExtra() bool {
	v, _ := n.boolValue([]string{"tsw_node_is_extra", "sitterwasm_node_is_extra", "ts_node_is_extra", "node_is_extra"})
	return v
}

// HasChanges reports whether incremental parsing marked this node changed.
func (n Node) HasChanges() bool {
	v, _ := n.boolValue([]string{"tsw_node_has_changes", "sitterwasm_node_has_changes", "ts_node_has_changes", "node_has_changes"})
	return v
}

// HasError reports whether this node or one of its descendants has an error.
func (n Node) HasError() bool {
	v, _ := n.boolValue([]string{"tsw_node_has_error", "sitterwasm_node_has_error", "ts_node_has_error", "node_has_error"})
	return v
}

// IsError reports whether this node is itself an ERROR node.
func (n Node) IsError() bool {
	v, err := n.boolValue([]string{"tsw_node_is_error", "sitterwasm_node_is_error", "ts_node_is_error", "node_is_error"})
	if err == nil {
		return v
	}
	// `ts_node_is_error` was added to the C API after some of the first
	// sitterwasm bridge modules were published.  The canonical Tree-sitter
	// representation still reserves the all-ones symbol id for ERROR nodes,
	// so recover the predicate through the stable symbol accessor when a
	// compatibility module omits the convenience export.  Keep malformed or
	// closed nodes conservative (KindID returns zero in those cases).
	if isUnsupported(err) {
		return n.KindID() == ^uint16(0)
	}
	return false
}

// ParseState returns the parser state at this node.
func (n Node) ParseState() uint16 {
	v, _ := n.uintValue([]string{"tsw_node_parse_state", "sitterwasm_node_parse_state", "ts_node_parse_state", "node_parse_state"})
	if v > uint32(^uint16(0)) {
		return 0
	}
	return uint16(v)
}

// NextParseState returns the parser state immediately after this node.
func (n Node) NextParseState() uint16 {
	v, _ := n.uintValue([]string{"tsw_node_next_parse_state", "sitterwasm_node_next_parse_state", "ts_node_next_parse_state", "node_next_parse_state"})
	if v > uint32(^uint16(0)) {
		return 0
	}
	return uint16(v)
}

func (n Node) position(names []string) (Point, error) {
	result, err := n.call(names)
	if err != nil {
		return Point{}, err
	}
	if len(result) >= 2 {
		row, rowOK := checkedU32(result[0])
		column, columnOK := checkedU32(result[1])
		if !rowOK || !columnOK {
			return Point{}, ErrInvalidHandle
		}
		return Point{Row: row, Column: column}, nil
	}
	if len(result) == 1 {
		return unpackPoint(result[0]), nil
	}
	return Point{}, ErrInvalidHandle
}

// StartByte returns the inclusive start byte offset.
func (n Node) StartByte() uint32 {
	v, _ := n.uintValue([]string{"tsw_node_start_byte", "sitterwasm_node_start_byte", "ts_node_start_byte", "node_start_byte"})
	return v
}

// StartByteE returns the inclusive start byte offset and any ABI or lifecycle
// error.
func (n Node) StartByteE() (uint32, error) {
	return n.uintValue([]string{"tsw_node_start_byte", "sitterwasm_node_start_byte", "ts_node_start_byte", "node_start_byte"})
}

// EndByte returns the exclusive end byte offset.
func (n Node) EndByte() uint32 {
	v, _ := n.uintValue([]string{"tsw_node_end_byte", "sitterwasm_node_end_byte", "ts_node_end_byte", "node_end_byte"})
	return v
}

// EndByteE returns the exclusive end byte offset and any ABI or lifecycle
// error.
func (n Node) EndByteE() (uint32, error) {
	return n.uintValue([]string{"tsw_node_end_byte", "sitterwasm_node_end_byte", "ts_node_end_byte", "node_end_byte"})
}

// StartPoint returns the inclusive start position.
func (n Node) StartPoint() Point {
	p, _ := n.position([]string{"tsw_node_start_point", "sitterwasm_node_start_point", "ts_node_start_point", "node_start_point"})
	return p
}

// StartPosition is the upstream spelling of StartPoint.
func (n Node) StartPosition() Point { return n.StartPoint() }

// StartPointE returns the inclusive start position and any ABI or lifecycle
// error.
func (n Node) StartPointE() (Point, error) {
	return n.position([]string{"tsw_node_start_point", "sitterwasm_node_start_point", "ts_node_start_point", "node_start_point"})
}

// EndPoint returns the exclusive end position.
func (n Node) EndPoint() Point {
	p, _ := n.position([]string{"tsw_node_end_point", "sitterwasm_node_end_point", "ts_node_end_point", "node_end_point"})
	return p
}

// EndPosition is the upstream spelling of EndPoint.
func (n Node) EndPosition() Point { return n.EndPoint() }

// EndPointE returns the exclusive end position and any ABI or lifecycle
// error.
func (n Node) EndPointE() (Point, error) {
	return n.position([]string{"tsw_node_end_point", "sitterwasm_node_end_point", "ts_node_end_point", "node_end_point"})
}

// Range returns this node's complete source range.
func (n Node) Range() Range {
	return Range{StartByte: n.StartByte(), EndByte: n.EndByte(), StartPoint: n.StartPoint(), EndPoint: n.EndPoint()}
}

// ByteRange returns the half-open byte interval covered by this node.
func (n Node) ByteRange() (uint32, uint32) { return n.StartByte(), n.EndByte() }

// Parent returns the parent node, or a null node for the root.
func (n Node) Parent() Node {
	return n.related([]string{"tsw_node_parent", "sitterwasm_node_parent", "ts_node_parent", "node_parent"})
}

// NextSibling returns the next sibling node.
func (n Node) NextSibling() Node {
	return n.related([]string{"tsw_node_next_sibling", "sitterwasm_node_next_sibling", "ts_node_next_sibling", "node_next_sibling"})
}

// PrevSibling returns the previous sibling node.
func (n Node) PrevSibling() Node {
	return n.related([]string{"tsw_node_prev_sibling", "sitterwasm_node_prev_sibling", "ts_node_prev_sibling", "node_prev_sibling"})
}

// NextNamedSibling returns the next named sibling node.
func (n Node) NextNamedSibling() Node {
	return n.related([]string{"tsw_node_next_named_sibling", "sitterwasm_node_next_named_sibling", "ts_node_next_named_sibling", "node_next_named_sibling"})
}

// PrevNamedSibling returns the previous named sibling node.
func (n Node) PrevNamedSibling() Node {
	return n.related([]string{"tsw_node_prev_named_sibling", "sitterwasm_node_prev_named_sibling", "ts_node_prev_named_sibling", "node_prev_named_sibling"})
}

func (n Node) related(names []string) Node {
	result, err := n.call(names)
	handle, ok := guestNodeHandle(result)
	if err != nil || !ok || handle == 0 {
		return Node{}
	}
	return n.tree.registerNode(handle)
}

// guestNodeHandle decodes a node handle returned by the wasm ABI. A zero
// handle is the null-node sentinel; a wider i64 result is malformed and must
// never be truncated into a potentially valid wasm32 pointer.
func guestNodeHandle(result []uint64) (uint32, bool) {
	if len(result) == 0 {
		return 0, false
	}
	return checkedU32(result[0])
}

// hostInt converts a wasm32 count to the host's int without wrapping on
// 32-bit platforms. Counts larger than MaxInt cannot be represented by the
// value-oriented Go API even though they fit the wire type.
func hostInt(value uint32) (int, bool) {
	maxInt := uint64(^uint(0) >> 1)
	if uint64(value) > maxInt {
		return 0, false
	}
	return int(value), true
}

// ChildCount returns the number of children, including anonymous extras.
func (n Node) ChildCount() int {
	v, _ := n.uintValue([]string{"tsw_node_child_count", "sitterwasm_node_child_count", "ts_node_child_count", "node_child_count"})
	if value, ok := hostInt(v); ok {
		return value
	}
	return 0
}

// ChildCountE returns the number of children and any ABI, lifecycle, or host
// integer-overflow error.
func (n Node) ChildCountE() (int, error) {
	v, err := n.uintValue([]string{"tsw_node_child_count", "sitterwasm_node_child_count", "ts_node_child_count", "node_child_count"})
	if err != nil {
		return 0, err
	}
	value, ok := hostInt(v)
	if !ok {
		return 0, ErrInvalidHandle
	}
	return value, nil
}

// NamedChildCount returns the number of named children.
func (n Node) NamedChildCount() int {
	v, _ := n.uintValue([]string{"tsw_node_named_child_count", "sitterwasm_node_named_child_count", "ts_node_named_child_count", "node_named_child_count"})
	if value, ok := hostInt(v); ok {
		return value
	}
	return 0
}

// NamedChildCountE returns the number of named children and any ABI,
// lifecycle, or host integer-overflow error.
func (n Node) NamedChildCountE() (int, error) {
	v, err := n.uintValue([]string{"tsw_node_named_child_count", "sitterwasm_node_named_child_count", "ts_node_named_child_count", "node_named_child_count"})
	if err != nil {
		return 0, err
	}
	value, ok := hostInt(v)
	if !ok {
		return 0, ErrInvalidHandle
	}
	return value, nil
}

// Child returns the child at index, or a null node if out of bounds.
func (n Node) Child(index int) Node {
	// The wire ABI carries child indexes as uint32.  On 64-bit hosts an
	// arbitrary int can be wider than that; truncating it would make a huge
	// out-of-range request accidentally address a valid low index in the guest.
	if index < 0 || uint64(index) > uint64(^uint32(0)) {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_child", "sitterwasm_node_child", "ts_node_child", "node_child"}, uint64(index))
	handle, ok := guestNodeHandle(result)
	if err != nil || !ok || handle == 0 {
		return Node{}
	}
	return n.tree.registerNode(handle)
}

// NamedChild returns the named child at index.
func (n Node) NamedChild(index int) Node {
	if index < 0 || uint64(index) > uint64(^uint32(0)) {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_named_child", "sitterwasm_node_named_child", "ts_node_named_child", "node_named_child"}, uint64(index))
	handle, ok := guestNodeHandle(result)
	if err != nil || !ok || handle == 0 {
		return Node{}
	}
	return n.tree.registerNode(handle)
}

// ChildByFieldName returns a child identified by grammar field name.
func (n Node) ChildByFieldName(field string) Node {
	if n.tree == nil {
		return Node{}
	}
	n.tree.mu.RLock()
	if err := n.ensureOpen(); err != nil {
		n.tree.mu.RUnlock()
		return Node{}
	}
	result, _, err := n.tree.rt.callWithInput(context.Background(), []string{"tsw_node_child_by_field_name", "sitterwasm_node_child_by_field_name", "ts_node_child_by_field_name", "node_child_by_field_name"}, []uint64{uint64(n.handle)}, []byte(field))
	n.tree.mu.RUnlock()
	handle, ok := guestNodeHandle(result)
	if err != nil || !ok || handle == 0 {
		return Node{}
	}
	return n.tree.registerNode(handle)
}

// ChildByFieldId resolves a numeric field id through the node's language.
func (n Node) ChildByFieldId(fieldID uint16) Node {
	// Newer bridges expose the native numeric operation directly. This avoids
	// a language-name round trip and preserves aliases accurately.
	if result, err := n.call([]string{
		"tsw_node_child_by_field_id",
		"sitterwasm_node_child_by_field_id",
		"ts_node_child_by_field_id",
		"node_child_by_field_id",
	}, uint64(fieldID)); err == nil && len(result) > 0 {
		if h, ok := guestNodeHandle(result); ok && h != 0 {
			return n.tree.registerNode(h)
		}
		return Node{}
	}
	lang := n.Language()
	if lang == nil {
		return Node{}
	}
	name := lang.FieldNameForID(fieldID)
	if name == "" {
		return Node{}
	}
	return n.ChildByFieldName(name)
}

// ChildByFieldID is an initialism-friendly alias for ChildByFieldId.
func (n Node) ChildByFieldID(fieldID uint16) Node { return n.ChildByFieldId(fieldID) }

// FieldNameForChild returns the field name associated with a child.
func (n Node) FieldNameForChild(index int) string {
	if index < 0 || uint64(index) > uint64(^uint32(0)) {
		return ""
	}
	s, err := n.stringValue([]string{"tsw_node_field_name_for_child_ptr", "tsw_node_field_name_for_child", "tsw_node_field_name", "sitterwasm_node_field_name_for_child", "ts_node_field_name_for_child", "node_field_name_for_child"}, uint64(index))
	if err == nil {
		return s
	}
	// The field-name accessor was added after the first sitterwasm bridge
	// revisions.  Recover the answer from the stable child/field APIs when a
	// compatibility module omits it.  Do this only for an explicitly missing
	// export: an empty string from a native accessor is a meaningful
	// "no-field" result and must not be replaced by a best-effort guess.
	if isUnsupported(err) {
		return n.fieldNameForChildFallback(index, false)
	}
	return ""
}

// FieldNameForNamedChild returns the field name associated with a named child.
func (n Node) FieldNameForNamedChild(index int) string {
	if index < 0 || uint64(index) > uint64(^uint32(0)) {
		return ""
	}
	s, err := n.stringValue([]string{"tsw_node_field_name_for_named_child", "sitterwasm_node_field_name_for_named_child", "ts_node_field_name_for_named_child", "node_field_name_for_named_child"}, uint64(index))
	if err == nil {
		return s
	}
	if isUnsupported(err) {
		return n.fieldNameForChildFallback(index, true)
	}
	return ""
}

// fieldNameForChildFallback derives a field name through the language field
// table when a bridge does not expose ts_node_field_name_for_* accessors.
// Tree-sitter field ids are one-based, and ChildByFieldId performs the same
// hidden-node/inherited-field resolution as the native field-name helper.  The
// fallback is intentionally used only for legacy modules; complete bridges
// take the O(1) native path above.
func (n Node) fieldNameForChildFallback(index int, namedOnly bool) string {
	if n.handle == 0 || n.tree == nil || n.ensureOpen() != nil || index < 0 {
		return ""
	}
	var child Node
	if namedOnly {
		child = n.NamedChild(index)
	} else {
		child = n.Child(index)
	}
	if child.handle == 0 || child.tree == nil {
		return ""
	}
	language := n.Language()
	if language == nil || !language.Valid() {
		return ""
	}
	fieldCount := language.FieldCount()
	// Guard the loop against malformed compatibility metadata advertising a
	// count larger than the uint16 field-id domain.  Such a module cannot
	// provide a representable answer, but iterating the valid prefix remains
	// safe and useful.
	if fieldCount > uint32(^uint16(0)) {
		fieldCount = uint32(^uint16(0))
	}
	for rawID := uint32(1); rawID <= fieldCount; rawID++ {
		fieldID := uint16(rawID)
		name := language.FieldNameForID(fieldID)
		if name == "" {
			continue
		}
		candidate := n.ChildByFieldId(fieldID)
		if candidate.handle != 0 && candidate.Equal(child) {
			return name
		}
	}
	return ""
}

// ChildrenByFieldName returns all direct children carrying field. The
// optional cursor is accepted for parity with upstream Go bindings.
func (n Node) ChildrenByFieldName(field string, cursors ...*TreeCursor) []Node {
	if n.IsNull() || field == "" {
		return nil
	}
	children := n.Children(cursors...)
	if len(children) == 0 {
		return nil
	}
	result := make([]Node, 0, len(children))
	for i, child := range children {
		if n.FieldNameForChild(i) == field {
			result = append(result, child)
		}
	}
	return result
}

// ChildrenByFieldID is the numeric-field variant of ChildrenByFieldName.
func (n Node) ChildrenByFieldID(fieldID uint16, cursors ...*TreeCursor) []Node {
	lang := n.Language()
	if lang == nil {
		return nil
	}
	return n.ChildrenByFieldName(lang.FieldNameForID(fieldID), cursors...)
}

// Children returns all direct children. An optional cursor is reused when
// supplied; otherwise a temporary cursor is created.
func (n Node) Children(cursors ...*TreeCursor) []Node {
	if n.IsNull() {
		return nil
	}
	var c *TreeCursor
	ownCursor := false
	if len(cursors) != 0 {
		c = cursors[0]
	}
	if c == nil {
		c = n.Walk()
		ownCursor = true
	}
	if c == nil {
		return nil
	}
	if ownCursor {
		defer func() { _ = c.Close() }()
	}
	// Reset is deliberately a no-op for a null/closed node or cursor.  Without
	// this check, reusing a cursor that failed to reset would walk whichever
	// tree/position it happened to hold previously and return unrelated
	// children.  Verify the reset before starting the iteration; cross-runtime
	// cursors are supported by Reset and are therefore accepted when the native
	// node comparison confirms the destination.
	c.Reset(n)
	if current := c.Node(); current.IsNull() || !current.Equal(n) {
		return nil
	}
	if !c.GoToFirstChild() {
		return nil
	}
	result := make([]Node, 0, n.ChildCount())
	for {
		result = append(result, c.Node())
		if !c.GoToNextSibling() {
			break
		}
	}
	return result
}

// NamedChildren returns all direct named children.
func (n Node) NamedChildren(cursors ...*TreeCursor) []Node {
	children := n.Children(cursors...)
	if len(children) == 0 {
		return nil
	}
	result := make([]Node, 0, n.NamedChildCount())
	for _, child := range children {
		if child.IsNamed() {
			result = append(result, child)
		}
	}
	return result
}

// FirstChildForByte returns the first direct child that ends after byte.
func (n Node) FirstChildForByte(offset uint32) Node {
	result, err := n.call([]string{"tsw_node_first_child_for_byte", "sitterwasm_node_first_child_for_byte", "ts_node_first_child_for_byte", "node_first_child_for_byte"}, uint64(offset))
	handle, ok := guestNodeHandle(result)
	if err == nil {
		if ok && handle != 0 {
			return n.tree.registerNode(handle)
		}
		// A successful native call returning the null sentinel means that no
		// matching child exists. Preserve that result; only an unsupported or
		// malformed call should enter the compatibility traversal below.
		if ok {
			return Node{}
		}
	}
	return n.firstChildForByteFallback(offset, false)
}

// FirstNamedChildForByte is the named variant of FirstChildForByte.
func (n Node) FirstNamedChildForByte(offset uint32) Node {
	result, err := n.call([]string{"tsw_node_first_named_child_for_byte", "sitterwasm_node_first_named_child_for_byte", "ts_node_first_named_child_for_byte", "node_first_named_child_for_byte"}, uint64(offset))
	handle, ok := guestNodeHandle(result)
	if err == nil {
		if ok && handle != 0 {
			return n.tree.registerNode(handle)
		}
		if ok {
			return Node{}
		}
	}
	return n.firstChildForByteFallback(offset, true)
}

// firstChildForByteFallback mirrors ts_node__first_child_for_byte for
// compatibility modules that predate the dedicated accessor. The public
// Child API already exposes visible children, so a depth-first search over
// those children naturally skips hidden grammar productions while still
// allowing an anonymous node to lead to a named descendant.
func (n Node) firstChildForByteFallback(offset uint32, namedOnly bool) Node {
	if n.IsNull() {
		return Node{}
	}
	var visit func(Node) Node
	visit = func(parent Node) Node {
		for i := 0; i < parent.ChildCount(); i++ {
			child := parent.Child(i)
			if child.IsNull() || child.EndByte() <= offset {
				continue
			}
			if !namedOnly || child.IsNamed() {
				return child
			}
			if child.ChildCount() > 0 {
				if descendant := visit(child); !descendant.IsNull() {
					return descendant
				}
			}
		}
		return Node{}
	}
	return visit(n)
}

// ChildWithDescendant returns the direct child containing descendant. If
// descendant is the receiver itself, Tree-sitter returns a null node; callers
// that need to preserve the receiver can check Equal before calling. Both a
// Node value and *Node pointer are accepted so code written for either the
// value-oriented sitterwasm API or the native Go binding compiles unchanged.
func (n Node) ChildWithDescendant(value any) Node {
	descendant, ok := nodeValueArg(value)
	if !ok {
		return Node{}
	}
	// TSNode relationships are only defined within one concrete TSTree.  A
	// Runtime can own many independent trees, and passing a node from another
	// tree to the C helper is particularly dangerous: the two nodes may have
	// identical byte ranges, causing the helper to return a plausible-looking
	// child from the receiver instead of a null node.  Reject that case before
	// entering the guest (Tree.Copy also creates a distinct TSTree, so it is
	// intentionally covered by this identity check).
	if n.tree == nil || descendant.tree == nil || n.tree != descendant.tree || n.handle == 0 || descendant.handle == 0 {
		return Node{}
	}
	if n.ensureOpen() != nil || descendant.ensureOpen() != nil {
		return Node{}
	}
	// Keep both backing trees alive while the guest reads both SWNode wrappers.
	// Share the global two-tree acquisition coordinator with ChangedRanges,
	// cursor reset, and Node.Equal so opposite-order operations cannot deadlock.
	treePairMu.Lock()
	var unlockDescendant func()
	if n.tree != descendant.tree {
		descendant.tree.mu.RLock()
		unlockDescendant = descendant.tree.mu.RUnlock
	}
	if descendant.ensureOpen() != nil {
		if unlockDescendant != nil {
			unlockDescendant()
		}
		treePairMu.Unlock()
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_child_with_descendant", "sitterwasm_node_child_with_descendant", "ts_node_child_with_descendant", "node_child_with_descendant"}, uint64(descendant.handle))
	var nativeHandle uint32
	if err == nil && len(result) != 0 {
		if handle, ok := checkedU32(result[0]); ok {
			nativeHandle = handle
		}
	}
	if unlockDescendant != nil {
		unlockDescendant()
	}
	treePairMu.Unlock()
	if nativeHandle != 0 {
		return n.tree.registerNode(nativeHandle)
	}

	// A null native result means either that the descendant is outside n or
	// that it is n itself (Tree-sitter deliberately returns a null node for the
	// latter).  Keep the fallback outside treePairMu: Parent/Equal acquire that
	// mutex themselves, and recursively taking it here would deadlock.
	if n.Equal(descendant) {
		return Node{}
	}
	for current := descendant; !current.IsNull(); {
		parent := current.Parent()
		if parent.IsNull() {
			return Node{}
		}
		if parent.Equal(n) {
			return current
		}
		current = parent
	}
	return Node{}
}

// DescendantCount returns the total number of descendants, including self when
// the bridge exposes that exact Tree-sitter operation.
func (n Node) DescendantCount() int {
	if n.IsNull() {
		return 0
	}
	v, err := n.uintValue([]string{"tsw_node_descendant_count", "sitterwasm_node_descendant_count", "ts_node_descendant_count", "node_descendant_count"})
	if err == nil {
		if value, ok := hostInt(v); ok {
			return value
		}
		return 0
	}
	count := 1
	for i := 0; i < n.ChildCount(); i++ {
		count += n.Child(i).DescendantCount()
	}
	return count
}

// DescendantForByteRange returns the smallest node covering [start,end).
func (n Node) DescendantForByteRange(start, end uint32) Node {
	// The range APIs are defined for half-open intervals with start <= end.
	// A few Tree-sitter releases return the first child for reversed bounds;
	// normalize that edge case here so native and compatibility bridges agree.
	if start > end {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_descendant_for_byte_range", "sitterwasm_node_descendant_for_byte_range", "ts_node_descendant_for_byte_range", "node_descendant_for_byte_range"}, uint64(start), uint64(end))
	if err == nil && len(result) > 0 {
		// A zero handle is the native null-node result, not an invitation to
		// approximate the query in Go.  Falling back after a successful null
		// response turns an out-of-bounds range into the receiver node and
		// diverges from Tree-sitter's exact semantics.  Use the compatibility
		// traversal only when the optional export is genuinely unavailable (or
		// returned a malformed result vector).
		if handle, ok := guestNodeHandle(result); ok {
			if handle == 0 {
				return Node{}
			}
			return n.tree.registerNode(handle)
		}
	}
	if err == nil && len(result) == 0 {
		// An export that returned no values is malformed, so retain the useful
		// compatibility behavior rather than exposing a false null node.
		return n.descendantFallback(start, end, false)
	}
	if err != nil && !isUnsupported(err) {
		return Node{}
	}
	return n.descendantFallback(start, end, false)
}

// NamedDescendantForByteRange is the named-node variant of
// DescendantForByteRange.
func (n Node) NamedDescendantForByteRange(start, end uint32) Node {
	if start > end {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_named_descendant_for_byte_range", "sitterwasm_named_descendant_for_byte_range", "sitterwasm_node_named_descendant_for_byte_range", "ts_node_named_descendant_for_byte_range", "node_named_descendant_for_byte_range"}, uint64(start), uint64(end))
	if err == nil && len(result) > 0 {
		if handle, ok := guestNodeHandle(result); ok {
			if handle == 0 {
				return Node{}
			}
			return n.tree.registerNode(handle)
		}
	}
	if err == nil && len(result) == 0 {
		return n.descendantFallback(start, end, true)
	}
	if err != nil && !isUnsupported(err) {
		return Node{}
	}
	return n.descendantFallback(start, end, true)
}

func (n Node) descendantFallback(start, end uint32, named bool) Node {
	if n.IsNull() || start > end {
		return Node{}
	}
	// This mirrors ts_node__descendant_for_byte_range in Tree-sitter's C
	// runtime.  In particular, the query need not be wholly contained by the
	// receiver (the runtime returns the receiver when no narrower child spans
	// the range), and an empty child is allowed to match a point exactly at its
	// end.  Walking through Child rather than recursively scanning every child
	// also preserves Tree-sitter's visible-child ordering for grammars with
	// hidden productions.
	node, lastVisible := n, n
	for {
		var descended bool
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.IsNull() {
				continue
			}
			nodeEnd := child.EndByte()
			if nodeEnd < end {
				continue
			}
			empty := child.StartByte() == nodeEnd
			if (empty && nodeEnd < start) || (!empty && nodeEnd <= start) {
				continue
			}
			if start < child.StartByte() {
				break
			}
			node = child
			if !named || child.IsNamed() {
				lastVisible = child
			}
			descended = true
			break
		}
		if !descended {
			break
		}
	}
	return lastVisible
}

// DescendantForPointRange is the point-coordinate variant.
func (n Node) DescendantForPointRange(start, end Point) Node {
	if comparePoint(start, end) > 0 {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_descendant_for_point_range", "sitterwasm_node_descendant_for_point_range", "ts_node_descendant_for_point_range", "node_descendant_for_point_range"}, packPoint(start), packPoint(end))
	if err == nil && len(result) > 0 {
		if handle, ok := guestNodeHandle(result); ok {
			if handle == 0 {
				return Node{}
			}
			return n.tree.registerNode(handle)
		}
	}
	if err == nil && len(result) == 0 {
		return n.descendantPointFallback(start, end, false)
	}
	if err != nil && !isUnsupported(err) {
		return Node{}
	}
	return n.descendantPointFallback(start, end, false)
}

// NamedDescendantForPointRange is the named-node variant of
// DescendantForPointRange.
func (n Node) NamedDescendantForPointRange(start, end Point) Node {
	if comparePoint(start, end) > 0 {
		return Node{}
	}
	result, err := n.call([]string{"tsw_node_named_descendant_for_point_range", "sitterwasm_named_descendant_for_point_range", "sitterwasm_node_named_descendant_for_point_range", "ts_node_named_descendant_for_point_range", "node_named_descendant_for_point_range"}, packPoint(start), packPoint(end))
	if err == nil && len(result) > 0 {
		if handle, ok := guestNodeHandle(result); ok {
			if handle == 0 {
				return Node{}
			}
			return n.tree.registerNode(handle)
		}
	}
	if err == nil && len(result) == 0 {
		return n.descendantPointFallback(start, end, true)
	}
	if err != nil && !isUnsupported(err) {
		return Node{}
	}
	return n.descendantPointFallback(start, end, true)
}

func (n Node) descendantPointFallback(start, end Point, named bool) Node {
	if n.IsNull() || comparePoint(start, end) > 0 {
		return Node{}
	}
	// Keep the same boundary rules as ts_node__descendant_for_point_range;
	// point and byte ranges intentionally have parallel, but not interchangeable,
	// comparisons when a node is zero-width.
	node, lastVisible := n, n
	for {
		var descended bool
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.IsNull() {
				continue
			}
			nodeEnd := child.EndPoint()
			if comparePoint(nodeEnd, end) < 0 {
				continue
			}
			empty := child.StartPoint() == nodeEnd
			if (empty && comparePoint(nodeEnd, start) < 0) ||
				(!empty && comparePoint(nodeEnd, start) <= 0) {
				continue
			}
			if comparePoint(start, child.StartPoint()) < 0 {
				break
			}
			node = child
			if !named || child.IsNamed() {
				lastVisible = child
			}
			descended = true
			break
		}
		if !descended {
			break
		}
	}
	return lastVisible
}

func comparePoint(a, b Point) int {
	if a.Row < b.Row || (a.Row == b.Row && a.Column < b.Column) {
		return -1
	}
	if a == b {
		return 0
	}
	return 1
}

// Text returns the source slice covered by the node. The returned bytes are a
// copy and remain independent of the caller's source buffer. Node access is
// valid while the owning tree is open; after Tree.Close the node is treated as
// null because its guest range metadata has been released.
func (n Node) Text() string {
	if n.tree == nil {
		return ""
	}
	source := n.tree.SourceContent()
	start, end := n.StartByte(), n.EndByte()
	if uint64(start) > uint64(len(source)) {
		return ""
	}
	if uint64(end) > uint64(len(source)) {
		end = uint32(len(source))
	}
	if end < start {
		return ""
	}
	return string(source[start:end])
}

// Content returns the bytes covered by this node. With no argument it uses
// the source retained by the owning Tree; an optional source slice mirrors
// the older Go binding API and is useful when the caller keeps source data
// outside the tree wrapper.
func (n Node) Content(sources ...[]byte) string {
	if len(sources) != 0 {
		return n.Utf8Text(sources[0])
	}
	return n.Text()
}

// Utf8Text returns this node's text from source bytes.
func (n Node) Utf8Text(source []byte) string {
	start, end := n.StartByte(), n.EndByte()
	if uint64(start) > uint64(len(source)) {
		return ""
	}
	if uint64(end) > uint64(len(source)) {
		end = uint32(len(source))
	}
	if end < start {
		return ""
	}
	return string(source[start:end])
}

// Utf16Text returns the code units covered by this node.
func (n Node) Utf16Text(source []uint16) []uint16 {
	if len(source) == 0 || n.IsNull() {
		return nil
	}
	// Node offsets are UTF-8 byte offsets (the stable WASM ABI always parses
	// UTF-8, including when the caller used ParseUTF16*).  Indexing a UTF-16
	// slice directly with those offsets is only correct for ASCII and silently
	// returns the wrong text as soon as a multi-byte rune appears.  Build the
	// byte-boundary table while decoding the original code units so surrogate
	// pairs and malformed UTF-16 are handled exactly like utf16.Decode.
	startByte, endByte := n.StartByte(), n.EndByte()
	if endByte < startByte {
		return nil
	}
	start, end, ok := utf16UnitRangeForUTF8Bytes(source, startByte, endByte)
	if !ok {
		return nil
	}
	return append([]uint16(nil), source[start:end]...)
}

// utf16UnitRangeForUTF8Bytes maps a half-open UTF-8 byte range to the
// corresponding UTF-16 code-unit range. Tree-sitter node boundaries are
// normally at rune boundaries; accepting a boundary in the middle of a rune
// by snapping to the containing unit keeps this helper total for trees made
// by custom/invalid-input grammars while never producing an out-of-bounds
// slice.
func utf16UnitRangeForUTF8Bytes(source []uint16, startByte, endByte uint32) (start, end int, ok bool) {
	// boundaryAtByte records an exact boundary. If a caller supplies an
	// interior byte offset, the first boundary after it is used for the end and
	// the preceding boundary for the start, preserving a non-negative range.
	type boundary struct {
		byteOffset uint32
		unitOffset int
	}
	boundaries := make([]boundary, 1, len(source)+1)
	boundaries[0] = boundary{}
	byteOffset := uint32(0)
	for unit := 0; unit < len(source); {
		runeValue := rune(source[unit])
		units := 1
		if runeValue >= 0xD800 && runeValue <= 0xDBFF {
			if unit+1 < len(source) &&
				rune(source[unit+1]) >= 0xDC00 && rune(source[unit+1]) <= 0xDFFF {
				decoded := utf16.Decode(source[unit : unit+2])
				// Decode returns RuneError for an unpaired sequence. A valid
				// high/low pair is the only case in which two code units are
				// consumed as one rune.
				if len(decoded) == 1 && decoded[0] != unicode.ReplacementChar {
					runeValue = decoded[0]
					units = 2
				}
			}
		}
		if units == 1 {
			decoded := utf16.Decode([]uint16{source[unit]})
			if len(decoded) == 1 {
				runeValue = decoded[0]
			}
		}
		width := uint32(len(string(runeValue)))
		// A uint32 overflow would imply a source larger than the ABI can
		// represent. Return !ok instead of wrapping and slicing incorrectly.
		if ^uint32(0)-byteOffset < width {
			return 0, 0, false
		}
		byteOffset += width
		unit += units
		boundaries = append(boundaries, boundary{byteOffset: byteOffset, unitOffset: unit})
	}
	if startByte > byteOffset || endByte > byteOffset {
		return 0, 0, false
	}
	// Find the boundaries surrounding each requested byte offset. Exact
	// boundaries are preferred; otherwise snap start upward and end downward so
	// the returned units still cover only complete UTF-8 runes.
	findStart := func(target uint32) int {
		for i := 0; i < len(boundaries); i++ {
			if boundaries[i].byteOffset >= target {
				return boundaries[i].unitOffset
			}
		}
		return len(source)
	}
	findEnd := func(target uint32) int {
		for i := len(boundaries) - 1; i >= 0; i-- {
			if boundaries[i].byteOffset <= target {
				return boundaries[i].unitOffset
			}
		}
		return 0
	}
	start, end = findStart(startByte), findEnd(endByte)
	if end < start {
		// Interior/overlapping offsets can otherwise cross after snapping. An
		// empty range is the least surprising, safe result.
		end = start
	}
	return start, end, true
}

// ToSExpression serializes this node and descendants.
func (n Node) ToSExpression() string {
	return n.toSExpression(true)
}

// toSExpression is the compatibility serializer used when a bridge does not
// expose ts_node_string. Tree-sitter treats an anonymous leaf differently
// depending on whether it is the root of the requested node: a root token is
// rendered as `("[")`, while the same token nested below a visible node is
// rendered as the bare quoted symbol (`"["`). Keeping the root bit explicit
// avoids wrapping every anonymous child in an extra pair of parentheses.
func (n Node) toSExpression(root bool) string {
	if n.IsNull() {
		return "(NULL)"
	}
	if s, err := n.stringValue([]string{"tsw_node_to_sexp", "tsw_node_string", "sitterwasm_node_to_sexp", "sitterwasm_node_string", "ts_node_string", "ts_node_to_sexp", "node_to_sexp", "node_string"}); err == nil && s != "" {
		return s
	}
	// Generic fallback that follows Tree-sitter's conventional formatting.  In
	// particular, anonymous grammar tokens are quoted (e.g. `("[")`), field
	// names are retained, and missing nodes use Tree-sitter's `(MISSING …)`
	// spelling.  This path is used for older custom bridges that do not expose
	// the native serializer, so keeping these details here avoids a surprising
	// semantic difference from the bundled runtime.
	typ := n.Type()
	named := n.IsNamed()
	if !named {
		typ = strconv.Quote(typ)
	}
	if n.IsMissing() {
		typ = "MISSING " + typ
	}
	children := n.ChildCount()
	if children == 0 {
		// Anonymous children are emitted as a quoted symbol without wrapping
		// parentheses. A direct call on an anonymous node is a root request and
		// therefore retains Tree-sitter's `("token")` spelling.
		if !named && !n.IsMissing() && !root {
			return typ
		}
		return "(" + typ + ")"
	}
	parts := make([]string, 0, children)
	for i := 0; i < children; i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		part := child.toSExpression(false)
		if field := n.FieldNameForChild(i); field != "" {
			part = field + ": " + part
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "(" + typ + ")"
	}
	return "(" + typ + " " + strings.Join(parts, " ") + ")"
}

// ToSexp is the upstream spelling.
func (n Node) ToSexp() string { return n.ToSExpression() }

// SExpression is a descriptive alias for ToSExpression.
func (n Node) SExpression() string { return n.ToSExpression() }

// Walk creates a cursor rooted at this node.
func (n Node) Walk() *TreeCursor {
	return newTreeCursor(n)
}

// String implements fmt.Stringer and returns the S-expression.
func (n Node) String() string { return n.ToSExpression() }

// Equal compares two node references.
func (n Node) Equal(value any) bool {
	other, ok := nodeValueArg(value)
	if !ok {
		return false
	}
	// Tree-sitter represents a null TSNode with a zero tree pointer and zero
	// id.  Consequently two null nodes compare equal (`ts_node_eq` compares
	// those two fields directly), while a null node and a live node do not.
	// Handle the value-shaped zero Node locally before requiring an owning tree;
	// this also makes `var n Node; n.Equal(Node{})` useful and matches the
	// upstream semantics without an unnecessary guest call.
	null := n.handle == 0 || n.tree == nil
	otherNull := other.handle == 0 || other.tree == nil
	if null || otherNull {
		return null && otherNull
	}
	if n.tree == nil || other.tree == nil || n.tree.rt != other.tree.rt || n.handle == 0 || other.handle == 0 {
		return false
	}
	if n.ensureOpen() != nil || other.ensureOpen() != nil {
		return false
	}
	// The C operation dereferences both opaque wrappers. Protect the second
	// tree as well as the receiver's tree, and use the same acquisition
	// coordinator as ChangedRanges and cursor reset. A single coordinator for
	// every two-tree operation prevents a lock inversion when Equal and another
	// pair operation race while Tree.Close has writers queued on both trees.
	treePairMu.Lock()
	defer treePairMu.Unlock()
	if n.tree != other.tree {
		other.tree.mu.RLock()
		defer other.tree.mu.RUnlock()
		// The initial lifecycle check above is only a snapshot.  A concurrent
		// Tree.Close can mark the other tree closed and reclaim its SWNode
		// wrapper before we acquire this read lock.  Tree.Close sets the closed
		// bit before waiting for mu, so re-check it after taking the lock; this
		// prevents passing a freed foreign wrapper to tsw_node_eq.
		if err := other.tree.ensureOpen(); err != nil {
			return false
		}
	}
	result, err := n.call([]string{"tsw_node_eq", "sitterwasm_node_eq", "ts_node_eq", "node_eq"}, uint64(other.handle))
	if err == nil && len(result) > 0 {
		return result[0] != 0
	}
	// Older bridge modules may not expose a dedicated equality operation.  The
	// wrapper handle is intentionally transient (a fresh SWNode allocation is
	// returned by every accessor), so comparing handles here would report false
	// for two Go values that refer to the same underlying Tree-sitter node.  Use
	// the stable TSNode identity when the optional accessor is available, then
	// fall back to a conservative source-range/symbol comparison for legacy
	// modules that predate it.
	if n.tree != other.tree {
		// ts_node_eq also requires the underlying TSTree pointer to match.  We
		// cannot establish that relationship without the native operation, and
		// two independently parsed trees can otherwise have identical metadata.
		return false
	}
	if n.handle == other.handle {
		return true
	}
	if firstID, ok := n.stableIdentityForEqual(); ok {
		if secondID, secondOK := other.stableIdentityForEqual(); secondOK {
			return firstID == secondID
		}
	}
	first, firstOK := n.equalMetadata()
	second, secondOK := other.equalMetadata()
	if firstOK && secondOK {
		return first == second
	}
	// With no stable identity or complete metadata, retain the old conservative
	// behavior rather than guessing that unrelated wrappers are equal.
	return false
}

// stableIdentityForEqual asks a compatibility bridge for the underlying
// TSNode identity.  It deliberately does not call Id, whose own legacy
// fallback is the transient wrapper handle and would reintroduce the bug this
// helper is intended to avoid.  The caller must hold treePairMu; no method in
// this helper acquires that mutex recursively.
func (n Node) stableIdentityForEqual() (uint32, bool) {
	result, err := n.call([]string{
		"tsw_node_id", "sitterwasm_node_id", "ts_node_id", "node_id",
	})
	if err != nil || len(result) == 0 {
		return 0, false
	}
	id, ok := checkedU32(result[0])
	return id, ok && id != 0
}

type nodeEqualMetadata struct {
	startByte, endByte   uint32
	startPoint, endPoint Point
	grammarSymbol        uint16
}

// equalMetadata is a conservative fallback for very old bridges that expose
// node navigation but neither ts_node_eq nor tsw_node_id.  Requiring all
// fields to be readable avoids treating a collection of zero values returned
// after an ABI error as an equality proof.  Source ranges and grammar symbols
// uniquely identify ordinary nodes; the stable-ID path above remains the
// authoritative route whenever available.
func (n Node) equalMetadata() (nodeEqualMetadata, bool) {
	if n.tree == nil || n.handle == 0 {
		return nodeEqualMetadata{}, false
	}
	startByte, err := n.uintValue([]string{
		"tsw_node_start_byte", "sitterwasm_node_start_byte", "ts_node_start_byte", "node_start_byte",
	})
	if err != nil {
		return nodeEqualMetadata{}, false
	}
	endByte, err := n.uintValue([]string{
		"tsw_node_end_byte", "sitterwasm_node_end_byte", "ts_node_end_byte", "node_end_byte",
	})
	if err != nil {
		return nodeEqualMetadata{}, false
	}
	start, err := n.position([]string{
		"tsw_node_start_point", "sitterwasm_node_start_point", "ts_node_start_point", "node_start_point",
	})
	if err != nil {
		return nodeEqualMetadata{}, false
	}
	end, err := n.position([]string{
		"tsw_node_end_point", "sitterwasm_node_end_point", "ts_node_end_point", "node_end_point",
	})
	if err != nil {
		return nodeEqualMetadata{}, false
	}
	symbol, err := n.uintValue([]string{
		"tsw_node_kind_id", "tsw_node_symbol", "sitterwasm_node_kind_id", "ts_node_symbol", "node_kind_id",
	})
	if err != nil {
		return nodeEqualMetadata{}, false
	}
	grammarSymbol, err := n.uintValue([]string{
		"tsw_node_grammar_symbol", "ts_node_grammar_symbol",
	})
	if err != nil {
		// Grammar-symbol metadata was added after the original bridge.  It is
		// useful but not required for the legacy range/symbol proof; use the
		// public symbol in its place when that optional accessor is absent.
		grammarSymbol = symbol
	}
	return nodeEqualMetadata{
		startByte: startByte, endByte: endByte,
		startPoint: start, endPoint: end,
		grammarSymbol: uint16(grammarSymbol),
	}, true
}

// Eq is an alias for Equal.
func (n Node) Eq(other any) bool { return n.Equal(other) }

// Equals is the upstream spelling of Equal.
func (n Node) Equals(other any) bool { return n.Equal(other) }

// Edit updates this node's coordinates after an input edit. The stable shim
// owns node values through the tree, so Tree.Edit is preferred; this method is
// provided as a compatibility no-op that validates ownership.
func (n Node) Edit(edit any) error {
	if err := n.ensureOpen(); err != nil {
		return err
	}
	editValue, err := inputEditValue(edit)
	if err != nil {
		return err
	}
	// TSNode is a value, so editing it must not edit the owning TSTree.  Encode
	// the same nine-field TSInputEdit record used by Tree.Edit and invoke the
	// optional node-edit export while retaining the tree read lock.
	buf := encodeInputEdit(editValue)
	n.tree.mu.RLock()
	defer n.tree.mu.RUnlock()
	if err := n.ensureOpen(); err != nil {
		return err
	}
	r := n.tree.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	ptr, err := r.allocLocked(uint32(len(buf)))
	if err != nil {
		return err
	}
	defer r.freeLocked(ptr)
	mem := r.mod.Memory()
	if mem == nil || !mem.Write(ptr, buf) {
		return io.ErrShortWrite
	}
	fn, name, err := r.function("tsw_node_edit", "sitterwasm_node_edit", "ts_node_edit", "node_edit")
	if err != nil {
		return err
	}
	if _, callErr := fn.Call(r.Context(), uint64(n.handle), uint64(ptr)); callErr != nil {
		return &ABIError{Function: name, Message: callErr.Error()}
	}
	return nil
}

// Close releases this node wrapper when the bridge supports explicit node
// ownership. Tree.Close remains safe to call afterward; repeated closes are
// ignored by the tree's registry.
func (n Node) Close() error {
	if n.handle == 0 || n.tree == nil {
		return nil
	}
	// Node is a value type and may have been copied. Do not eagerly free the
	// guest allocation here; the owning tree registry releases it exactly once.
	return nil
}

func (n Node) stringValue(names []string, args ...uint64) (string, error) {
	if n.tree == nil {
		return "", ErrInvalidHandle
	}
	n.tree.mu.RLock()
	defer n.tree.mu.RUnlock()
	if err := n.ensureOpen(); err != nil {
		return "", err
	}
	r := n.tree.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	all := make([]uint64, 0, len(args)+1)
	all = append(all, uint64(n.handle))
	all = append(all, args...)
	result, name, err := r.callLocked(context.Background(), names, all...)
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", ErrInvalidHandle
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: "node string accessor", Message: "returned a non-wasm32 string pointer"}
	}
	if ptr == 0 {
		return "", nil
	}
	if len(result) >= 2 {
		length, lengthOK := checkedU32(result[1])
		if !lengthOK {
			return "", &ABIError{Function: "node string accessor", Message: "returned a non-wasm32 string length"}
		}
		if mem := r.mod.Memory(); mem != nil {
			if b, ok := mem.Read(ptr, length); ok {
				return string(b), nil
			}
		}
	}
	s, e := r.readCString(ptr)
	if e != nil {
		return "", &ABIError{Function: name, Message: e.Error()}
	}
	return s, nil
}
