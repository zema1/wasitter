package sitterwasm_test

import (
	"errors"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

func TestQueryMatchesAndCursor(t *testing.T) {
	p, rt, tree := parseJSON(t, `[1, 2, {"x": 3}]`)
	lang := p.Language()
	if lang == nil {
		t.Fatal("Parser.Language() returned nil after SetLanguage")
	}
	query, err := sitterwasm.NewQuery(lang, `(number) @number`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	t.Cleanup(func() { _ = query.Close() })
	if query.Source() != `(number) @number` {
		t.Errorf("Query.Source() = %q", query.Source())
	}
	if query.PatternCount() != 1 || query.CaptureCount() != 1 {
		t.Errorf("query counts = pattern %d capture %d, want 1/1", query.PatternCount(), query.CaptureCount())
	}
	if query.CaptureName(0) != "number" || query.CaptureNameForID(0) != "number" {
		t.Errorf("capture name = %q/%q", query.CaptureName(0), query.CaptureNameForID(0))
	}

	matches := query.Matches(tree.RootNode())
	if len(matches) != 3 {
		t.Fatalf("Query.Matches returned %d matches, want 3", len(matches))
	}
	for i, match := range matches {
		if match.PatternIndex != 0 || len(match.Captures) != 1 {
			t.Errorf("match %d = %#v", i, match)
			continue
		}
		capture := match.Captures[0]
		if capture.Index != 0 || capture.Node.Type() != "number" {
			t.Errorf("capture %d = %#v", i, capture)
		}
	}

	cursor := sitterwasm.NewQueryCursor()
	t.Cleanup(func() { _ = cursor.Close() })
	if err := cursor.Exec(query, tree.RootNode()); err != nil {
		t.Fatalf("QueryCursor.Exec: %v", err)
	}
	seen := 0
	for {
		match, ok := cursor.NextMatch()
		if !ok {
			break
		}
		seen++
		if len(match.Captures) != 1 {
			t.Errorf("cursor match %#v has %d captures", match, len(match.Captures))
		}
	}
	if seen != len(matches) {
		t.Errorf("cursor yielded %d matches, want %d", seen, len(matches))
	}
	if _, ok := cursor.NextCapture(); ok {
		t.Error("NextCapture after exhausting cursor should return false")
	}

	// Keep rt referenced so this test documents that query execution is tied
	// to the same runtime as the tree; the cleanup from parseJSON owns it.
	if rt == nil {
		t.Fatal("parseJSON returned nil runtime")
	}
}

func TestQueryRejectsMalformedPattern(t *testing.T) {
	p, _ := newJSONParser(t)
	lang := p.Language()
	if lang == nil {
		t.Fatal("Parser.Language() returned nil")
	}
	query, err := sitterwasm.NewQuery(lang, `number @missing-parentheses`)
	if query != nil {
		t.Error("malformed query returned a non-nil query")
	}
	var queryErr *sitterwasm.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("error = %v, want *QueryError", err)
	}
	if queryErr.Error() == "" {
		t.Error("QueryError.Error() is empty")
	}
}

func TestTreeCursorNavigation(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, false, null]`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil")
	}
	t.Cleanup(func() { _ = cursor.Close() })
	if cursor.Node().Type() != "document" {
		t.Fatalf("initial cursor node = %q, want document", cursor.Node().Type())
	}
	if cursor.CurrentDepth() != 0 {
		t.Errorf("initial depth = %d, want 0", cursor.CurrentDepth())
	}
	if !cursor.GoToFirstChild() || cursor.Node().Type() != "array" {
		t.Fatalf("GoToFirstChild node = %q; want array", cursor.Node().Type())
	}
	if !cursor.GoToFirstChild() || cursor.Node().Type() != "[" {
		t.Fatalf("nested GoToFirstChild node = %q; want [", cursor.Node().Type())
	}
	if !cursor.GoToNextSibling() || cursor.Node().Type() != "number" {
		t.Fatalf("GoToNextSibling node = %q, want number", cursor.Node().Type())
	}
	if !cursor.GoToNextNamedSibling() || cursor.Node().Type() != "false" {
		t.Fatalf("GoToNextNamedSibling node = %q, want false", cursor.Node().Type())
	}
	if !cursor.GoToPreviousNamedSibling() || cursor.Node().Type() != "number" {
		t.Fatalf("GoToPreviousNamedSibling node = %q, want number", cursor.Node().Type())
	}
	if !cursor.GoToParent() || cursor.Node().Type() != "array" {
		t.Fatalf("GoToParent node = %q, want array", cursor.Node().Type())
	}
	if !cursor.GoToParent() || cursor.Node().Type() != "document" {
		t.Fatalf("second GoToParent node = %q, want document", cursor.Node().Type())
	}
	if cursor.GoToParent() {
		t.Fatalf("cursor should stop at root boundary, node = %q", cursor.Node().Type())
	}
	if cursor.GoToFirstChildForByte(2) && cursor.Node().IsNull() {
		t.Error("GoToFirstChildForByte returned a null node")
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Cursor.Close: %v", err)
	}
	if cursor.GoToFirstChild() {
		t.Error("closed cursor should not move")
	}
}

func TestTreeCursorOptionalNavigationAndFields(t *testing.T) {
	_, _, tree := parseJSON(t, `{"x": [1, 2]}`)
	cursor := tree.Walk()
	if cursor == nil {
		t.Fatal("Tree.Walk returned nil")
	}
	defer cursor.Close()
	root := tree.RootNode()
	if !cursor.GoToFirstChild() || cursor.Node().Type() != "object" {
		t.Fatalf("GoToFirstChild node = %q, want object", cursor.Node().Type())
	}
	if !cursor.GoToFirstChild() || cursor.Node().Type() != "{" {
		t.Fatalf("object first child = %q, want {", cursor.Node().Type())
	}
	if !cursor.GoToNextSibling() || cursor.Node().Type() != "pair" {
		t.Fatalf("object pair child = %q, want pair", cursor.Node().Type())
	}
	if cursor.CurrentFieldName() != "" || cursor.FieldName() != "" {
		t.Errorf("top-level pair field = %q/%q, want empty", cursor.CurrentFieldName(), cursor.FieldName())
	}
	if !cursor.GoToFirstChild() || cursor.Node().Type() != "string" {
		t.Fatalf("pair first child = %q, want string", cursor.Node().Type())
	}
	if cursor.CurrentFieldName() != "key" || cursor.FieldName() != "key" {
		t.Errorf("key field = %q/%q, want key", cursor.CurrentFieldName(), cursor.FieldName())
	}
	if !cursor.GoToNextNamedSibling() || cursor.Node().Type() != "array" {
		t.Fatalf("value field = %q, want array", cursor.Node().Type())
	}
	if cursor.CurrentFieldName() != "value" {
		t.Errorf("value field = %q, want value", cursor.CurrentFieldName())
	}
	if !cursor.GoToLastChild() || cursor.Node().Type() != "]" {
		t.Fatalf("array last child = %q, want ]", cursor.Node().Type())
	}
	if cursor.GoToFirstChildForPoint(sitterwasm.Point{Row: 0, Column: 10}) {
		t.Errorf("leaf GoToFirstChildForPoint unexpectedly moved to %q", cursor.Node().Type())
	}
	cursor.Reset(root)
	if cursor.Node().Type() != "document" || cursor.CurrentDepth() != 0 {
		t.Errorf("Reset(root) = %q depth %d", cursor.Node().Type(), cursor.CurrentDepth())
	}
	if !cursor.GoToFirstChildForPoint(sitterwasm.Point{Row: 0, Column: 3}) {
		t.Fatal("GoToFirstChildForPoint did not enter object")
	}
	if cursor.Node().Type() != "object" {
		t.Errorf("GoToFirstChildForPoint node = %q, want object", cursor.Node().Type())
	}
}

func TestQueryAndCursorClosedErrors(t *testing.T) {
	p, _, tree := parseJSON(t, `[1]`)
	query, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	if err := query.Close(); err != nil {
		t.Fatalf("Query.Close: %v", err)
	}
	if got := query.Matches(tree.RootNode()); got != nil {
		t.Errorf("closed query matches = %#v, want nil", got)
	}
	cursor := sitterwasm.NewQueryCursor()
	if err := cursor.Close(); err != nil {
		t.Fatalf("QueryCursor.Close: %v", err)
	}
	if err := cursor.Exec(query, tree.RootNode()); !errors.Is(err, sitterwasm.ErrClosed) {
		t.Errorf("closed cursor Exec error = %v, want ErrClosed", err)
	}
}

func TestQueryCursorOptionsConfiguredBeforeExec(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	cursor := sitterwasm.NewQueryCursor()
	defer cursor.Close()

	// Zero is a valid range boundary (Tree-sitter interprets an end of zero as
	// the unbounded end sentinel), not the unset sentinel. Configure it before
	// Exec so the native handle has to replay the option on allocation.
	if err := cursor.SetByteRangeE(0, 0); err != nil {
		t.Fatalf("SetByteRangeE(0,0): %v", err)
	}
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := countCursorMatches(cursor); got != 3 {
		t.Fatalf("[0,0] byte range returned %d matches, want 3 (unbounded end)", got)
	}

	// Reusing the cursor must retain the option state and update it correctly.
	if err := cursor.SetByteRangeE(4, 5); err != nil {
		t.Fatalf("SetByteRangeE(4,5): %v", err)
	}
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("second Exec: %v", err)
	}
	match, ok := cursor.NextMatch()
	if !ok || len(match.Captures) != 1 || match.Captures[0].Node.Text() != "2" {
		t.Fatalf("range match = %#v, ok=%v; want number 2", match, ok)
	}
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("byte range cursor returned an extra match")
	}

	if err := cursor.SetPointRangeE(sitterwasm.Point{Row: 1, Column: 0}, sitterwasm.Point{Row: 1, Column: 0}); err != nil {
		t.Fatalf("SetPointRangeE: %v", err)
	}
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("point-range Exec: %v", err)
	}
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("zero-width point range unexpectedly returned a match")
	}

	if err := cursor.SetByteRangeE(5, 4); err == nil {
		t.Fatal("SetByteRangeE accepted a reversed range")
	}
	if err := cursor.SetPointRangeE(sitterwasm.Point{Row: 2}, sitterwasm.Point{Row: 1}); err == nil {
		t.Fatal("SetPointRangeE accepted a reversed range")
	}
}

func countCursorMatches(cursor *sitterwasm.QueryCursor) int {
	count := 0
	for {
		if _, ok := cursor.NextMatch(); !ok {
			return count
		}
		count++
	}
}

func TestQueryCursorOptionsBeforeExecAreApplied(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	cursor := sitterwasm.NewQueryCursor()
	defer cursor.Close()
	cursor.SetMatchLimit(7)
	cursor.SetTimeoutMicros(1234)
	cursor.SetMaxStartDepth(0)
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := cursor.MatchLimit(); got != 7 {
		t.Fatalf("MatchLimit = %d, want 7", got)
	}
	if got := cursor.TimeoutMicros(); got != 1234 {
		t.Fatalf("TimeoutMicros = %d, want 1234", got)
	}
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("max start depth 0 unexpectedly returned a nested number")
	}
}
