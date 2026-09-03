package sitterwasm

import (
	"strconv"
	"strings"
	"sync"
)

// This file contains the iterator-shaped query API used by the current
// tree-sitter Go binding.  The original implementation in this package
// exposed a zero-argument QueryCursor.Matches method returning a plain slice.
// QueryMatches and QueryCaptures deliberately remain slice aliases so that
// existing callers can still use len and range, while Next provides the
// stateful spelling used by upstream tree-sitter.

func queryUint32Value(value any) (uint32, bool) {
	if value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case uint32:
		return typed, true
	case uint:
		if uint64(typed) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(typed), true
	case uint64:
		if typed > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(typed), true
	case uint16:
		return uint32(typed), true
	case uint8:
		return uint32(typed), true
	case int:
		if typed < 0 || uint64(typed) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(typed), true
	case int64:
		if typed < 0 || uint64(typed) > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(typed), true
	case int32:
		if typed < 0 {
			return 0, false
		}
		return uint32(typed), true
	case int16:
		if typed < 0 {
			return 0, false
		}
		return uint32(typed), true
	case int8:
		if typed < 0 {
			return 0, false
		}
		return uint32(typed), true
	case *uint:
		if typed == nil {
			return 0, false
		}
		return queryUint32Value(*typed)
	case *uint32:
		if typed == nil {
			return 0, false
		}
		return *typed, true
	case *uint64:
		if typed == nil {
			return 0, false
		}
		return queryUint32Value(*typed)
	default:
		return 0, false
	}
}

// queryIteratorKind identifies which native cursor operation a materialized
// compatibility iterator mirrors.  QueryMatches and QueryCaptures are kept as
// slices for source compatibility, but each slice may retain this private
// stream description so a later range setter can replay the native cursor's
// in-progress state.
type queryIteratorKind uint8

const (
	queryIteratorMatches queryIteratorKind = iota
	queryIteratorCaptures
)

type queryRangeOperation struct {
	position   uint64
	kind       uint8 // 0 = byte, 1 = point
	byteStart  uint32
	byteEnd    uint32
	pointStart Point
	pointEnd   Point
}

// queryIteratorStream is deliberately unexported.  The public iterator
// values remain named slices, while this small shadow records only enough
// execution history to reproduce Tree-sitter's range mutation semantics after
// one or more Next calls. In particular, rooted patterns may retain
// in-progress states whose uncaptured root intersects a newly assigned range;
// replaying from the beginning without this history incorrectly drops those
// matches.
type queryIteratorStream struct {
	mu sync.Mutex

	query *Query
	root  Node
	kind  queryIteratorKind

	byteStart, byteEnd          uint32
	pointStart, pointEnd        Point
	byteRangeSet, pointRangeSet bool
	maxDepth                    uint32
	maxDepthSet                 bool
	matchLimit                  uint32
	matchLimitSet               bool
	timeoutMicros               uint64
	timeoutSet                  bool
	predicateText               []byte
	predicateTextSet            bool

	consumed   uint64
	operations []queryRangeOperation
}

// newQueryIteratorStream snapshots the cursor configuration before the
// materializing drain starts. It intentionally does not allocate a second
// guest cursor yet; replay is needed only if a caller later mutates the
// materialized range.
func newQueryIteratorStream(c *QueryCursor, kind queryIteratorKind, text []byte) *queryIteratorStream {
	if c == nil || c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.query == nil || !c.query.native || c.root.IsNull() || c.runtime == nil || c.handle == 0 {
		return nil
	}
	s := &queryIteratorStream{
		query:            c.query,
		root:             c.root,
		kind:             kind,
		byteStart:        c.byteStart,
		byteEnd:          c.byteEnd,
		pointStart:       c.pointStart,
		pointEnd:         c.pointEnd,
		byteRangeSet:     c.byteRangeSet,
		pointRangeSet:    c.pointRangeSet,
		maxDepth:         c.maxDepth,
		maxDepthSet:      c.maxDepthSet,
		matchLimit:       c.matchLimit,
		matchLimitSet:    c.matchLimitSet,
		timeoutMicros:    c.timeoutMicros,
		timeoutSet:       c.timeoutSet,
		predicateTextSet: text != nil,
	}
	if text != nil {
		s.predicateText = append([]byte(nil), text...)
	}
	return s
}

func (s *queryIteratorStream) consumedOne() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.consumed != ^uint64(0) {
		s.consumed++
	}
	s.mu.Unlock()
}

type queryIteratorSnapshot struct {
	query                       *Query
	rootNode                    Node
	kind                        queryIteratorKind
	byteStart, byteEnd          uint32
	pointStart, pointEnd        Point
	byteRangeSet, pointRangeSet bool
	maxDepth                    uint32
	maxDepthSet                 bool
	matchLimit                  uint32
	matchLimitSet               bool
	timeoutMicros               uint64
	timeoutSet                  bool
	text                        []byte
	textSet                     bool
	consumed                    uint64
	operations                  []queryRangeOperation
}

// snapshotRangeOperation appends one setter at the current iterator
// position, then replays the complete history against a fresh native cursor.
func (s *queryIteratorStream) snapshotRangeOperation(op queryRangeOperation) (map[string]int, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	op.position = s.consumed
	s.operations = append(s.operations, op)
	snap := queryIteratorSnapshot{
		query:         s.query,
		rootNode:      s.root,
		kind:          s.kind,
		byteStart:     s.byteStart,
		byteEnd:       s.byteEnd,
		pointStart:    s.pointStart,
		pointEnd:      s.pointEnd,
		byteRangeSet:  s.byteRangeSet,
		pointRangeSet: s.pointRangeSet,
		maxDepth:      s.maxDepth,
		maxDepthSet:   s.maxDepthSet,
		matchLimit:    s.matchLimit,
		matchLimitSet: s.matchLimitSet,
		timeoutMicros: s.timeoutMicros,
		timeoutSet:    s.timeoutSet,
		textSet:       s.predicateTextSet,
		consumed:      s.consumed,
		operations:    append([]queryRangeOperation(nil), s.operations...),
	}
	if s.predicateTextSet {
		snap.text = append([]byte(nil), s.predicateText...)
	}
	s.mu.Unlock()

	// The initial range/depth/options are represented by the snapshot fields;
	// operation history contains only setters made after Exec.
	shadow := NewQueryCursor()
	defer func() { _ = shadow.Close() }()
	if snap.byteRangeSet {
		shadow.SetByteRange(snap.byteStart, snap.byteEnd)
	}
	if snap.pointRangeSet {
		shadow.SetPointRange(snap.pointStart, snap.pointEnd)
	}
	if snap.maxDepthSet {
		shadow.SetMaxStartDepth(snap.maxDepth)
	}
	if snap.matchLimitSet {
		shadow.SetMatchLimit(snap.matchLimit)
	}
	if snap.timeoutSet {
		shadow.SetTimeoutMicros(snap.timeoutMicros)
	}
	if snap.query == nil || snap.rootNode.IsNull() {
		return nil, false
	}
	if err := shadow.Exec(snap.query, snap.rootNode); err != nil {
		return nil, false
	}
	shadow.mu.Lock()
	shadow.predicateTextSet = snap.textSet
	if snap.textSet {
		shadow.predicateText = append([]byte(nil), snap.text...)
	}
	shadow.captureIteratorFullMatch = snap.kind == queryIteratorCaptures
	shadow.mu.Unlock()

	apply := func(position uint64) {
		for _, operation := range snap.operations {
			if operation.position != position {
				continue
			}
			if operation.kind == 0 {
				shadow.SetByteRange(operation.byteStart, operation.byteEnd)
			} else {
				shadow.SetPointRange(operation.pointStart, operation.pointEnd)
			}
		}
	}
	apply(0)
	for i := uint64(0); i < snap.consumed; i++ {
		var ok bool
		if snap.kind == queryIteratorCaptures {
			_, ok = shadow.NextCapture()
		} else {
			_, ok = shadow.NextMatch()
		}
		if !ok {
			// The materialized iterator could only have consumed values that
			// existed, so an early end means the replay configuration no longer
			// describes a usable live tree/query. Report failure and let the
			// caller retain its conservative local filter.
			return nil, false
		}
		apply(i + 1)
	}

	keys := make(map[string]int)
	if snap.kind == queryIteratorCaptures {
		for {
			capture, ok := shadow.NextCapture()
			if !ok {
				break
			}
			keys[queryCaptureKeyString(capture)]++
		}
	} else {
		for {
			match, ok := shadow.NextMatch()
			if !ok {
				break
			}
			keys[queryMatchKey(match)]++
		}
	}
	return keys, true
}

// QueryCursorState describes the position reported to a query progress
// callback.  CurrentByteOffset is measured in UTF-8 bytes from the beginning
// of the execution tree.
type QueryCursorState struct {
	CurrentByteOffset uint32
}

// QueryCursorOptions controls optional query execution behaviour.
//
// Tree-sitter invokes ProgressCallback periodically while walking a tree.  A
// callback returning true requests cancellation.  The WASM bridge does not
// expose a callback trampoline, so the host iterator invokes this callback at
// each materialized match (and once at the end when no match was produced).
type QueryCursorOptions struct {
	ProgressCallback func(QueryCursorState) bool
}

// QueryMatches is a sequence of query matches.  It is intentionally a named
// slice rather than a struct: callers may use len, indexing, and range just as
// they did with the historical QueryCursor.Matches result.  Next consumes the
// sequence from the front and returns nil after it is exhausted.
type QueryMatches []QueryMatch

// QueryCaptures is a sequence of individual query captures.  It likewise
// retains slice semantics for Go callers while exposing the upstream Next
// iterator.  Next returns a one-capture QueryMatch and the capture's ordinal in
// that match.  The match metadata is populated when available; for bridges
// that only expose the compact capture ABI it is necessarily limited to the
// selected capture.
type QueryCaptures []QueryCapture

// FilterPredicates evaluates the built-in predicates for a materialized
// match and returns a predicate-filtered copy.  This is the compatibility
// spelling exposed by smacker/go-tree-sitter.  Tree-sitter's native helper
// returns a non-nil match with an empty capture list when a predicate does not
// match, so retain that shape here instead of returning nil; callers can use
// len(result.Captures) to distinguish the two outcomes without a second error
// channel.  The returned value is detached from the cursor and therefore can
// safely outlive the next cursor operation.
func (c *QueryCursor) FilterPredicates(m *QueryMatch, input []byte) *QueryMatch {
	if m == nil {
		return nil
	}
	result := &QueryMatch{PatternIndex: m.PatternIndex, ID: m.ID}
	if c == nil || c.closed.Load() {
		return result
	}
	query := c.currentQuery()
	if query == nil || !m.SatisfiesTextPredicate(query, nil, nil, input) {
		return result
	}
	result.Captures = append([]QueryCapture(nil), m.Captures...)
	return result
}

// Next returns and consumes the next match.  The returned pointer refers to
// the element retained by the iterator and remains valid until the caller
// discards it.  Calling Next on a nil or exhausted iterator returns nil.
func (matches *QueryMatches) Next() *QueryMatch {
	if matches == nil {
		return nil
	}
	for len(*matches) != 0 {
		match := &(*matches)[0]
		*matches = (*matches)[1:]
		if match.stream != nil {
			match.stream.consumedOne()
		}
		if match.cursor != nil && match.cursor.isMatchRemoved(match.ID) {
			continue
		}
		return match
	}
	return nil
}

// Next returns and consumes the next capture.  The returned match contains
// the complete originating match when that information was retained by the
// cursor; otherwise it contains the selected capture only.  The second return
// value is the capture ordinal (as in tree-sitter's C API), not the query's
// capture-name index.
func (captures *QueryCaptures) Next() (*QueryMatch, uint) {
	if captures == nil {
		return nil, 0
	}
	var capture QueryCapture
	found := false
	for len(*captures) != 0 {
		capture = (*captures)[0]
		*captures = (*captures)[1:]
		if capture.stream != nil {
			capture.stream.consumedOne()
		} else if capture.match != nil && capture.match.stream != nil {
			capture.match.stream.consumedOne()
		}
		if capture.match != nil && capture.match.cursor != nil && capture.match.cursor.isMatchRemoved(capture.match.ID) {
			continue
		}
		found = true
		break
	}
	if !found {
		return nil, 0
	}
	if capture.match != nil {
		match := capture.match
		if capture.ordinal >= uint32(len(match.Captures)) {
			// Some older bridges expose only the selected capture in the
			// compact record while still reporting its ordinal. Pad a private
			// copy so callers can safely index match.Captures[ordinal], as they
			// can with upstream's full TSQueryMatch value.
			copyMatch := *match
			copyMatch.Captures = make([]QueryCapture, int(capture.ordinal)+1)
			copy(copyMatch.Captures, match.Captures)
			copyMatch.Captures[capture.ordinal] = capture.public()
			match = &copyMatch
		}
		return match, uint(capture.ordinal)
	}
	match := &QueryMatch{
		PatternIndex: capture.patternIndex,
		ID:           capture.matchID,
		Captures:     []QueryCapture{capture.public()},
	}
	return match, 0
}

// SetByteRange restricts an already materialized match sequence to matches
// intersecting [start, end).  The cursor range is also updated when the
// sequence originated from a cursor.  Reversed ranges are ignored, matching
// Tree-sitter's no-op behaviour for this setter.
func (matches *QueryMatches) SetByteRange(start, end any) {
	if matches == nil {
		return
	}
	start32, okStart := queryUint32Value(start)
	end32, okEnd := queryUint32Value(end)
	if !okStart || !okEnd {
		return
	}
	// Both endpoints cross a uint32 WASM ABI.  A start beyond that width must
	// be rejected before conversion; otherwise a large 64-bit value could wrap
	// to a small offset after the end is clamped below.
	if uint64(start32) > uint64(^uint32(0)) {
		return
	}
	// Tree-sitter reserves an end byte of zero for an unbounded range.  Apply
	// that sentinel normalization before checking ordering; otherwise a valid
	// range such as (12, 0) is mistaken for a reversed range when this setter
	// is called on the materialized iterator (the cursor setter already follows
	// the native convention).
	if end32 == 0 {
		end32 = ^uint32(0)
	}
	if start32 > end32 {
		return
	}
	var nativeCursor *QueryCursor
	var stream *queryIteratorStream
	for _, match := range *matches {
		if match.cursor != nil {
			nativeCursor = match.cursor
			match.cursor.SetByteRange(start32, end32)
			if match.stream != nil {
				stream = match.stream
			}
			break
		}
	}
	// QueryMatches is a materialized compatibility slice, while native
	// Tree-sitter keeps the cursor stream lazy. If the caller changes the
	// range after consuming part of that slice, filtering only by captured
	// nodes is observably wrong for rooted patterns whose uncaptured root
	// intersects the range (for example `(array (_) @item)`). Re-run the
	// native query with the new range and use its match signatures as an
	// eligibility multiset for the still-unconsumed snapshot. This preserves
	// the native root-intersection semantics without changing the public slice
	// representation. Legacy/fallback cursors retain the local filter below.
	if stream != nil {
		if eligible, ok := stream.snapshotRangeOperation(queryRangeOperation{kind: 0, byteStart: start32, byteEnd: end32}); ok {
			filtered := (*matches)[:0]
			for _, match := range *matches {
				key := queryMatchKey(match)
				if eligible[key] > 0 {
					filtered = append(filtered, match)
					eligible[key]--
				}
			}
			*matches = filtered
			return
		}
	} else if nativeCursor != nil {
		if eligible, ok := nativeCursor.nativeRangeMatchKeys(false); ok {
			filtered := (*matches)[:0]
			for _, match := range *matches {
				key := queryMatchKey(match)
				if eligible[key] > 0 {
					filtered = append(filtered, match)
					eligible[key]--
				}
			}
			*matches = filtered
			return
		}
	}
	filtered := (*matches)[:0]
	for _, match := range *matches {
		if queryMatchIntersectsByteRange(match, start32, end32) {
			filtered = append(filtered, match)
		}
	}
	*matches = filtered
}

// SetPointRange restricts an already materialized match sequence to matches
// intersecting the supplied point range.  Reversed ranges are ignored.
func (matches *QueryMatches) SetPointRange(start, end Point) {
	if matches == nil {
		return
	}
	// (0,0) is Tree-sitter's unbounded end-point sentinel.  Normalize it
	// before the ordering check so callers can use the same range convention
	// through both QueryCursor and QueryMatches.
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	if comparePoint(start, end) > 0 {
		return
	}
	var nativeCursor *QueryCursor
	var stream *queryIteratorStream
	for _, match := range *matches {
		if match.cursor != nil {
			nativeCursor = match.cursor
			match.cursor.SetPointRange(start, end)
			if match.stream != nil {
				stream = match.stream
			}
			break
		}
	}
	if stream != nil {
		if eligible, ok := stream.snapshotRangeOperation(queryRangeOperation{kind: 1, pointStart: start, pointEnd: end}); ok {
			filtered := (*matches)[:0]
			for _, match := range *matches {
				key := queryMatchKey(match)
				if eligible[key] > 0 {
					filtered = append(filtered, match)
					eligible[key]--
				}
			}
			*matches = filtered
			return
		}
	} else if nativeCursor != nil {
		if eligible, ok := nativeCursor.nativeRangeMatchKeys(true); ok {
			filtered := (*matches)[:0]
			for _, match := range *matches {
				key := queryMatchKey(match)
				if eligible[key] > 0 {
					filtered = append(filtered, match)
					eligible[key]--
				}
			}
			*matches = filtered
			return
		}
	}
	filtered := (*matches)[:0]
	for _, match := range *matches {
		if queryMatchIntersectsPointRange(match, start, end) {
			filtered = append(filtered, match)
		}
	}
	*matches = filtered
}

// SetByteRange applies a byte range to a materialized capture sequence.
func (captures *QueryCaptures) SetByteRange(start, end any) {
	if captures == nil {
		return
	}
	start32, okStart := queryUint32Value(start)
	end32, okEnd := queryUint32Value(end)
	if !okStart || !okEnd {
		return
	}
	if uint64(start32) > uint64(^uint32(0)) {
		return
	}
	if end32 == 0 {
		end32 = ^uint32(0)
	}
	if start32 > end32 {
		return
	}
	var nativeCursor *QueryCursor
	var stream *queryIteratorStream
	for _, capture := range *captures {
		if capture.match != nil && capture.match.cursor != nil {
			nativeCursor = capture.match.cursor
			capture.match.cursor.SetByteRange(start32, end32)
			if capture.stream != nil {
				stream = capture.stream
			} else if capture.match.stream != nil {
				stream = capture.match.stream
			}
			break
		}
	}
	if stream != nil {
		if eligible, ok := stream.snapshotRangeOperation(queryRangeOperation{kind: 0, byteStart: start32, byteEnd: end32}); ok {
			filtered := (*captures)[:0]
			for _, capture := range *captures {
				key := queryCaptureKeyString(capture)
				if eligible[key] > 0 {
					filtered = append(filtered, capture)
					eligible[key]--
				}
			}
			*captures = filtered
			return
		}
	} else if nativeCursor != nil {
		if eligible, ok := nativeCursor.nativeRangeCaptureKeys(false); ok {
			filtered := (*captures)[:0]
			for _, capture := range *captures {
				key := makeQueryCaptureKey(capture)
				if eligible[key] > 0 {
					filtered = append(filtered, capture)
					eligible[key]--
				}
			}
			*captures = filtered
			return
		}
	}
	filtered := (*captures)[:0]
	for _, capture := range *captures {
		if nodeIntersectsCaptureByteRange(capture.Node, start32, end32) {
			filtered = append(filtered, capture)
		}
	}
	*captures = filtered
}

// SetPointRange applies a point range to a materialized capture sequence.
func (captures *QueryCaptures) SetPointRange(start, end Point) {
	if captures == nil {
		return
	}
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	if comparePoint(start, end) > 0 {
		return
	}
	var nativeCursor *QueryCursor
	var stream *queryIteratorStream
	for _, capture := range *captures {
		if capture.match != nil && capture.match.cursor != nil {
			nativeCursor = capture.match.cursor
			capture.match.cursor.SetPointRange(start, end)
			if capture.stream != nil {
				stream = capture.stream
			} else if capture.match.stream != nil {
				stream = capture.match.stream
			}
			break
		}
	}
	if stream != nil {
		if eligible, ok := stream.snapshotRangeOperation(queryRangeOperation{kind: 1, pointStart: start, pointEnd: end}); ok {
			filtered := (*captures)[:0]
			for _, capture := range *captures {
				key := queryCaptureKeyString(capture)
				if eligible[key] > 0 {
					filtered = append(filtered, capture)
					eligible[key]--
				}
			}
			*captures = filtered
			return
		}
	} else if nativeCursor != nil {
		if eligible, ok := nativeCursor.nativeRangeCaptureKeys(true); ok {
			filtered := (*captures)[:0]
			for _, capture := range *captures {
				key := makeQueryCaptureKey(capture)
				if eligible[key] > 0 {
					filtered = append(filtered, capture)
					eligible[key]--
				}
			}
			*captures = filtered
			return
		}
	}
	filtered := (*captures)[:0]
	for _, capture := range *captures {
		if nodeIntersectsCapturePointRange(capture.Node, start, end) {
			filtered = append(filtered, capture)
		}
	}
	*captures = filtered
}

// Matches executes a query and returns its matches.  With no arguments it
// snapshots the current cursor execution, preserving the historical API.  To
// mirror upstream tree-sitter, the preferred form is
//
//	cursor.Matches(query, node, source)
//
// where node may be either Node or *Node.  Invalid argument lists return a nil
// sequence because the historical method had no error result; callers that
// need diagnostics should use Exec followed by NextMatch.
func (c *QueryCursor) Matches(args ...any) QueryMatches {
	if c == nil || c.closed.Load() {
		return nil
	}
	if len(args) == 0 {
		// The compatibility cursor keeps matches in c.matches.  Native cursors
		// stream matches directly from the guest, so drain that stream when the
		// caller asks for a slice snapshot.
		snapshot := QueryMatches(c.matchesSnapshot())
		if len(snapshot) != 0 {
			return snapshot
		}
		c.mu.Lock()
		native := c.query != nil && c.query.native && c.handle != 0
		c.mu.Unlock()
		if !native {
			return snapshot
		}
		return c.collectMatches(nil, QueryCursorOptions{})
	}
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil
	}
	if err := c.Exec(query, node); err != nil {
		return nil
	}
	return c.collectMatches(text, options)
}

// MatchesWithOptions is the options-aware upstream spelling.  It accepts the
// same Node/value argument variants as Matches: (query, node, text, options).
func (c *QueryCursor) MatchesWithOptions(args ...any) QueryMatches {
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil
	}
	if err := c.Exec(query, node); err != nil {
		return nil
	}
	return c.collectMatches(text, options)
}

// MatchesE is the error-returning counterpart of Matches. It is useful when
// callers need to distinguish an invalid/closed cursor from an empty result;
// the argument forms are identical to Matches.
func (c *QueryCursor) MatchesE(args ...any) (QueryMatches, error) {
	if c == nil || c.closed.Load() {
		return nil, ErrClosed
	}
	if len(args) == 0 {
		return c.Matches(), nil
	}
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil, ErrUnsupported
	}
	if err := c.Exec(query, node); err != nil {
		return nil, err
	}
	return c.collectMatches(text, options), nil
}

// MatchesWithOptionsE is the error-returning counterpart of
// MatchesWithOptions.
func (c *QueryCursor) MatchesWithOptionsE(args ...any) (QueryMatches, error) {
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil, ErrUnsupported
	}
	if err := c.Exec(query, node); err != nil {
		return nil, err
	}
	return c.collectMatches(text, options), nil
}

// Captures executes a query and returns its captures in source/Tree-sitter
// order.  It accepts (query, node, text), with node being either Node or
// *Node.  With no arguments it snapshots the current cursor stream.
func (c *QueryCursor) Captures(args ...any) QueryCaptures {
	if c == nil || c.closed.Load() {
		return nil
	}
	if len(args) == 0 {
		return c.collectCaptures(nil, QueryCursorOptions{})
	}
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil
	}
	if err := c.Exec(query, node); err != nil {
		return nil
	}
	return c.collectCaptures(text, options)
}

// CapturesWithOptions is the options-aware variant of Captures.
func (c *QueryCursor) CapturesWithOptions(args ...any) QueryCaptures {
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil
	}
	if err := c.Exec(query, node); err != nil {
		return nil
	}
	return c.collectCaptures(text, options)
}

// CapturesE is the error-returning counterpart of Captures.
func (c *QueryCursor) CapturesE(args ...any) (QueryCaptures, error) {
	if c == nil || c.closed.Load() {
		return nil, ErrClosed
	}
	if len(args) == 0 {
		return c.Captures(), nil
	}
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil, ErrUnsupported
	}
	if err := c.Exec(query, node); err != nil {
		return nil, err
	}
	return c.collectCaptures(text, options), nil
}

// CapturesWithOptionsE is the error-returning counterpart of
// CapturesWithOptions.
func (c *QueryCursor) CapturesWithOptionsE(args ...any) (QueryCaptures, error) {
	query, node, text, options, ok := parseQueryIteratorArgs(args)
	if !ok {
		return nil, ErrUnsupported
	}
	if err := c.Exec(query, node); err != nil {
		return nil, err
	}
	return c.collectCaptures(text, options), nil
}

// ExecWithOptions executes a query while retaining options for a subsequent
// NextMatch/NextCapture loop.  Progress callbacks are invoked by the explicit
// iterator methods only; this method exists as an ergonomic counterpart for
// callers that prefer the error-returning Exec API.
func (c *QueryCursor) ExecWithOptions(args ...any) error {
	if len(args) < 2 {
		return ErrUnsupported
	}
	query, node, options, ok := parseQueryExecOptionsArgs(args)
	if !ok {
		return ErrUnsupported
	}
	if err := c.Exec(query, node); err != nil {
		return err
	}
	c.mu.Lock()
	// Keep the callback for callers that subsequently use Matches/Captures
	// without passing options again.  This field is intentionally optional;
	// old cursors and direct Next* calls remain unaffected.
	c.progressCallback = options.ProgressCallback
	c.progressPersistent = options.ProgressCallback != nil
	c.progressCanceled = false
	c.mu.Unlock()
	return nil
}

func parseQueryExecOptionsArgs(args []any) (query *Query, node Node, options QueryCursorOptions, ok bool) {
	if len(args) < 2 || len(args) > 4 {
		return nil, Node{}, QueryCursorOptions{}, false
	}
	if len(args) == 2 {
		query, node, _, _, ok = parseQueryIteratorArgs(args)
		return query, node, QueryCursorOptions{}, ok
	}
	// The idiomatic form is (query, node, options).  Also accept
	// (query, node, text, options) for callers sharing one argument builder with
	// MatchesWithOptions.
	if len(args) == 3 {
		query, node, _, _, ok = parseQueryIteratorArgs([]any{args[0], args[1]})
		if !ok {
			return nil, Node{}, QueryCursorOptions{}, false
		}
		if args[2] == nil {
			return query, node, QueryCursorOptions{}, true
		}
		switch value := args[2].(type) {
		case QueryCursorOptions:
			options = value
		case *QueryCursorOptions:
			if value == nil {
				return nil, Node{}, QueryCursorOptions{}, false
			}
			options = *value
		default:
			return nil, Node{}, QueryCursorOptions{}, false
		}
		return query, node, options, true
	}
	query, node, _, options, ok = parseQueryIteratorArgs(args)
	return query, node, options, ok
}

func parseQueryIteratorArgs(args []any) (query *Query, node Node, text []byte, options QueryCursorOptions, ok bool) {
	if len(args) < 2 || len(args) > 4 {
		return nil, Node{}, nil, QueryCursorOptions{}, false
	}
	query, ok = args[0].(*Query)
	if !ok || query == nil {
		return nil, Node{}, nil, QueryCursorOptions{}, false
	}
	switch value := args[1].(type) {
	case Node:
		node = value
	case *Node:
		if value == nil {
			return nil, Node{}, nil, QueryCursorOptions{}, false
		}
		node = *value
	default:
		return nil, Node{}, nil, QueryCursorOptions{}, false
	}
	if len(args) >= 3 && args[2] != nil {
		switch value := args[2].(type) {
		case []byte:
			text = value
		case string:
			text = []byte(value)
		default:
			return nil, Node{}, nil, QueryCursorOptions{}, false
		}
	}
	if len(args) >= 4 && args[3] != nil {
		switch value := args[3].(type) {
		case QueryCursorOptions:
			options = value
		case *QueryCursorOptions:
			if value == nil {
				return nil, Node{}, nil, QueryCursorOptions{}, false
			}
			options = *value
		default:
			return nil, Node{}, nil, QueryCursorOptions{}, false
		}
	}
	return query, node, text, options, true
}

func (c *QueryCursor) collectMatches(text []byte, options QueryCursorOptions) QueryMatches {
	if c == nil {
		return nil
	}
	// Remember the caller-provided source for the duration of this drain.  A
	// non-nil empty slice is intentional input too; it must not be conflated
	// with the nil/default tree source when evaluating text predicates.
	c.mu.Lock()
	c.predicateText = text
	c.predicateTextSet = text != nil
	c.mu.Unlock()
	if options.ProgressCallback == nil {
		options.ProgressCallback = c.progressCallbackValue()
	}
	stream := newQueryIteratorStream(c, queryIteratorMatches, text)
	result := make(QueryMatches, 0)
	for {
		match, found := c.NextMatch()
		if !found {
			break
		}
		match.stream = stream
		result = append(result, match)
		if options.ProgressCallback != nil {
			if options.ProgressCallback(QueryCursorState{CurrentByteOffset: matchOffset(match)}) {
				// Tree-sitter treats a progress callback that returns true as a
				// cancellation of the current cursor execution, rather than merely
				// a request to stop materializing this particular slice.  Remember
				// the cancellation so callers that continue with NextMatch cannot
				// accidentally resume the guest stream. Exec resets this bit for a
				// new execution.
				c.markProgressCanceled()
				break
			}
		}
	}
	if len(result) == 0 && options.ProgressCallback != nil {
		// A zero-match query still gets one useful progress notification.  The
		// callback can use this to cancel an expensive empty traversal.
		if options.ProgressCallback(QueryCursorState{CurrentByteOffset: c.rootOffset()}) {
			c.markProgressCanceled()
		}
	}
	return result
}

func (c *QueryCursor) collectCaptures(text []byte, options QueryCursorOptions) QueryCaptures {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.predicateText = text
	c.predicateTextSet = text != nil
	c.mu.Unlock()
	if options.ProgressCallback == nil {
		options.ProgressCallback = c.progressCallbackValue()
	}
	// When the optional full-match capture ABI is available, ask NextCapture
	// to retain the complete originating match so QueryCaptures.Next can expose
	// the same match/index pair as upstream. The flag is scoped to this drain;
	// direct NextCapture callers keep the lower-allocation compact path.
	c.mu.Lock()
	c.captureIteratorFullMatch = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.captureIteratorFullMatch = false
		c.mu.Unlock()
	}()
	stream := newQueryIteratorStream(c, queryIteratorCaptures, text)
	result := make(QueryCaptures, 0)
	for {
		capture, found := c.NextCapture()
		if !found {
			break
		}
		if capture.match == nil {
			capture.match = &QueryMatch{
				PatternIndex: capture.patternIndex,
				ID:           capture.matchID,
				Captures:     []QueryCapture{capture.public()},
				cursor:       c,
			}
		}
		capture.stream = stream
		if capture.match != nil {
			capture.match.stream = stream
		}
		result = append(result, capture)
		if options.ProgressCallback != nil {
			if options.ProgressCallback(QueryCursorState{CurrentByteOffset: capture.Node.StartByte()}) {
				c.markProgressCanceled()
				break
			}
		}
	}
	if len(result) == 0 && options.ProgressCallback != nil {
		if options.ProgressCallback(QueryCursorState{CurrentByteOffset: c.rootOffset()}) {
			c.markProgressCanceled()
		}
	}
	return result
}

// markProgressCanceled records cancellation for the current execution.  It
// is deliberately separate from runStoredProgress because Matches/Captures
// invoke a one-shot callback after a value has already been yielded.  The
// callback runs outside c.mu, so acquire the lock only for the short state
// transition and leave user code entirely outside the critical section.
func (c *QueryCursor) markProgressCanceled() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.closed.Load() {
		c.cancelProgressLocked()
	}
	c.mu.Unlock()
}

// cancelProgressLocked terminates a cursor stream that was stopped by a
// progress callback. The native query cursor can retain pointers into the
// execution tree even after the callback asks it to stop, so its guest handle
// must be destroyed before releasing treeLifeUnlock. Otherwise Tree.Close may
// delete the backing tree while the still-live cursor points at it. The caller
// must hold c.mu.
func (c *QueryCursor) cancelProgressLocked() {
	if c == nil {
		return
	}
	c.progressCanceled = true
	c.deleteNativeHandleLocked()
	c.releaseTreeLifeLocked()
}

func (c *QueryCursor) progressCallbackValue() func(QueryCursorState) bool {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	callback := c.progressCallback
	c.mu.Unlock()
	return callback
}

// runStoredProgress invokes the callback retained by ExecWithOptions.  It is
// intentionally called outside the cursor's main iteration lock: callbacks
// are user code and may inspect (or even close) the cursor.  A generation
// token prevents a callback racing with a subsequent Exec from cancelling the
// newly started execution.
func (c *QueryCursor) runStoredProgress() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return true
	}
	if !c.progressPersistent || c.progressCallback == nil || c.progressCanceled {
		canceled := c.progressCanceled
		c.mu.Unlock()
		return canceled
	}
	callback := c.progressCallback
	generation := c.progressGeneration
	offset := c.root.StartByte()
	c.mu.Unlock()

	stop := callback(QueryCursorState{CurrentByteOffset: offset})
	c.mu.Lock()
	if generation == c.progressGeneration && !c.closed.Load() && stop {
		c.cancelProgressLocked()
	}
	canceled := c.progressCanceled || c.closed.Load()
	c.mu.Unlock()
	return canceled
}

func (c *QueryCursor) isMatchRemoved(id uint32) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	_, removed := c.removedMatches[id]
	c.mu.Unlock()
	return removed
}

func (c *QueryCursor) currentQuery() *Query {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.query
}

func (c *QueryCursor) rootOffset() uint32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	root := c.root
	c.mu.Unlock()
	if root.IsNull() {
		return 0
	}
	return root.StartByte()
}

func matchOffset(match QueryMatch) uint32 {
	if len(match.Captures) == 0 {
		return 0
	}
	return match.Captures[0].Node.StartByte()
}

// queryMatchKey is a stable, allocation-local description of a materialized
// match. Native match ids are intentionally not part of the key: rerunning a
// query with a different range can renumber states even though the underlying
// capture set is unchanged. Pattern index, capture order/index, and node
// coordinates are sufficient to distinguish the observable match values and
// let range setters reconcile an eager compatibility slice with a fresh
// native execution.
func queryMatchKey(match QueryMatch) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(uint64(match.PatternIndex), 10))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(len(match.Captures)))
	for _, capture := range match.Captures {
		b.WriteByte('|')
		b.WriteString(strconv.FormatUint(uint64(capture.Index), 10))
		b.WriteByte('@')
		b.WriteString(strconv.FormatUint(uint64(capture.Node.StartByte()), 10))
		b.WriteByte('-')
		b.WriteString(strconv.FormatUint(uint64(capture.Node.EndByte()), 10))
		start := capture.Node.StartPoint()
		end := capture.Node.EndPoint()
		b.WriteByte('[')
		b.WriteString(strconv.FormatUint(uint64(start.Row), 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatUint(uint64(start.Column), 10))
		b.WriteByte(';')
		b.WriteString(strconv.FormatUint(uint64(end.Row), 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatUint(uint64(end.Column), 10))
		b.WriteByte(']')
	}
	return b.String()
}

func queryCaptureKeyString(capture QueryCapture) string {
	key := makeQueryCaptureKey(capture)
	var b strings.Builder
	b.WriteString(strconv.FormatUint(uint64(key.pattern), 10))
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(uint64(key.ordinal), 10))
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(uint64(key.index), 10))
	b.WriteByte('@')
	b.WriteString(strconv.FormatUint(uint64(key.start), 10))
	b.WriteByte('-')
	b.WriteString(strconv.FormatUint(uint64(key.end), 10))
	b.WriteByte('[')
	b.WriteString(strconv.FormatUint(uint64(key.startRow), 10))
	b.WriteByte(',')
	b.WriteString(strconv.FormatUint(uint64(key.startColumn), 10))
	b.WriteByte(';')
	b.WriteString(strconv.FormatUint(uint64(key.endRow), 10))
	b.WriteByte(',')
	b.WriteString(strconv.FormatUint(uint64(key.endColumn), 10))
	b.WriteByte(']')
	return b.String()
}

// queryCaptureKey identifies one capture event in a materialized capture
// iterator. The ordinal is the index within the originating match, while the
// pattern and node coordinates distinguish otherwise identical captures from
// different patterns/locations.
type queryCaptureSignature struct {
	pattern uint32
	ordinal uint32
	index   uint32
	start   uint32
	end     uint32
	startRow,
	startColumn,
	endRow,
	endColumn uint32
}

func makeQueryCaptureKey(capture QueryCapture) queryCaptureSignature {
	start := capture.Node.StartPoint()
	end := capture.Node.EndPoint()
	pattern, ordinal := capture.patternIndex, capture.ordinal
	if capture.match != nil {
		if pattern == 0 && capture.match.PatternIndex != 0 {
			pattern = capture.match.PatternIndex
		}
		// A compact capture record may not retain its ordinal, but a full
		// originating match does. Use the explicit record when available and
		// otherwise derive the first matching occurrence by position below.
		if ordinal == 0 {
			for i, candidate := range capture.match.Captures {
				if candidate.Index == capture.Index && candidate.Node.StartByte() == capture.Node.StartByte() && candidate.Node.EndByte() == capture.Node.EndByte() {
					ordinal = uint32(i)
					break
				}
			}
		}
	}
	return queryCaptureSignature{
		pattern:     pattern,
		ordinal:     ordinal,
		index:       capture.Index,
		start:       capture.Node.StartByte(),
		end:         capture.Node.EndByte(),
		startRow:    start.Row,
		startColumn: start.Column,
		endRow:      end.Row,
		endColumn:   end.Column,
	}
}

// nativeRangeMatchKeys re-executes the current native cursor with its current
// range/options and returns a multiset of match signatures. It is used only
// when a caller applies SetByteRange/SetPointRange to an already materialized
// QueryMatches value; in that situation the original lazy guest stream is no
// longer available to answer root-intersection questions. The bool result is
// false when the cursor is a legacy/partial bridge or the replay cannot be
// performed, allowing callers to retain their existing compatibility filter.
func (c *QueryCursor) nativeRangeMatchKeys(_ bool) (map[string]int, bool) {
	if c == nil || c.closed.Load() {
		return nil, false
	}
	c.mu.Lock()
	query, root := c.query, c.root
	if query == nil || !query.native || c.runtime == nil || c.handle == 0 || !c.nativeMatchAvailable {
		c.mu.Unlock()
		return nil, false
	}
	maxDepth, maxDepthSet := c.maxDepth, c.maxDepthSet
	matchLimit, matchLimitSet := c.matchLimit, c.matchLimitSet
	timeout, timeoutSet := c.timeoutMicros, c.timeoutSet
	byteStart, byteEnd, byteSet := c.byteStart, c.byteEnd, c.byteRangeSet
	pointStart, pointEnd, pointSet := c.pointStart, c.pointEnd, c.pointRangeSet
	textSet := c.predicateTextSet
	var text []byte
	if textSet {
		text = make([]byte, len(c.predicateText))
		copy(text, c.predicateText)
	}
	c.mu.Unlock()

	temporary := NewQueryCursor()
	defer func() { _ = temporary.Close() }()
	if maxDepthSet {
		temporary.SetMaxStartDepth(maxDepth)
	}
	if matchLimitSet {
		temporary.SetMatchLimit(matchLimit)
	}
	if timeoutSet {
		temporary.SetTimeoutMicros(timeout)
	}
	if byteSet {
		temporary.SetByteRange(byteStart, byteEnd)
	}
	if pointSet {
		temporary.SetPointRange(pointStart, pointEnd)
	}
	if err := temporary.Exec(query, root); err != nil {
		return nil, false
	}
	matches := temporary.collectMatches(text, QueryCursorOptions{})
	keys := make(map[string]int, len(matches))
	for _, match := range matches {
		keys[queryMatchKey(match)]++
	}
	return keys, true
}

// nativeRangeCaptureKeys is the capture-stream counterpart of
// nativeRangeMatchKeys. It preserves Tree-sitter's capture-level range rules
// (including complete matches whose uncaptured root intersects the range) by
// replaying the native cursor before reducing the result to capture-event
// signatures.
func (c *QueryCursor) nativeRangeCaptureKeys(_ bool) (map[queryCaptureSignature]int, bool) {
	if c == nil || c.closed.Load() {
		return nil, false
	}
	c.mu.Lock()
	query, root := c.query, c.root
	if query == nil || !query.native || c.runtime == nil || c.handle == 0 || (!c.nativeCaptureAvailable && !c.nativeMatchAvailable) {
		c.mu.Unlock()
		return nil, false
	}
	maxDepth, maxDepthSet := c.maxDepth, c.maxDepthSet
	matchLimit, matchLimitSet := c.matchLimit, c.matchLimitSet
	timeout, timeoutSet := c.timeoutMicros, c.timeoutSet
	byteStart, byteEnd, byteSet := c.byteStart, c.byteEnd, c.byteRangeSet
	pointStart, pointEnd, pointSet := c.pointStart, c.pointEnd, c.pointRangeSet
	textSet := c.predicateTextSet
	var text []byte
	if textSet {
		text = make([]byte, len(c.predicateText))
		copy(text, c.predicateText)
	}
	c.mu.Unlock()

	temporary := NewQueryCursor()
	defer func() { _ = temporary.Close() }()
	if maxDepthSet {
		temporary.SetMaxStartDepth(maxDepth)
	}
	if matchLimitSet {
		temporary.SetMatchLimit(matchLimit)
	}
	if timeoutSet {
		temporary.SetTimeoutMicros(timeout)
	}
	if byteSet {
		temporary.SetByteRange(byteStart, byteEnd)
	}
	if pointSet {
		temporary.SetPointRange(pointStart, pointEnd)
	}
	if err := temporary.Exec(query, root); err != nil {
		return nil, false
	}
	captures := temporary.collectCaptures(text, QueryCursorOptions{})
	keys := make(map[queryCaptureSignature]int, len(captures))
	for _, capture := range captures {
		keys[makeQueryCaptureKey(capture)]++
	}
	return keys, true
}

func queryMatchIntersectsByteRange(match QueryMatch, start, end uint32) bool {
	if len(match.Captures) == 0 {
		if !match.root.IsNull() {
			return nodeIntersectsByteRange(match.root, start, end)
		}
		return true
	}
	for _, capture := range match.Captures {
		if nodeIntersectsByteRange(capture.Node, start, end) {
			return true
		}
	}
	return false
}

func queryMatchIntersectsPointRange(match QueryMatch, start, end Point) bool {
	if len(match.Captures) == 0 {
		if !match.root.IsNull() {
			return nodeIntersectsPointRange(match.root, start, end)
		}
		return true
	}
	for _, capture := range match.Captures {
		if nodeIntersectsPointRange(capture.Node, start, end) {
			return true
		}
	}
	return false
}

// public returns a copy without the private iterator bookkeeping fields.
func (capture QueryCapture) public() QueryCapture {
	capture.match = nil
	capture.matchID = 0
	capture.patternIndex = 0
	capture.ordinal = 0
	return capture
}
