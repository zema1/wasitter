package wasitter_test

import (
	"errors"
	"math"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

func TestQueryCursorUpstreamIteratorCompatibility(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()

	root := tree.RootNode()
	rootPtr := &root
	matches := cursor.Matches(q, rootPtr, []byte(`[1, 2]`))
	if len(matches) != 2 {
		t.Fatalf("Matches len = %d, want 2", len(matches))
	}
	if got := matches[0].Captures[0].Node.Text(); got != "1" {
		t.Fatalf("first match text = %q, want 1", got)
	}
	if got := matches.Next(); got == nil || got.Captures[0].Node.Text() != "1" {
		t.Fatalf("first Next result = %#v", got)
	}
	if got := matches.Next(); got == nil || got.Captures[0].Node.Text() != "2" {
		t.Fatalf("second Next result = %#v", got)
	}
	if got := matches.Next(); got != nil {
		t.Fatalf("exhausted Next result = %#v", got)
	}

	captures := cursor.Captures(q, tree.RootNode(), []byte(`[1, 2]`))
	if len(captures) != 2 {
		t.Fatalf("Captures len = %d, want 2", len(captures))
	}
	match, ordinal := captures.Next()
	if match == nil || ordinal != 0 || len(match.Captures) == 0 || match.Captures[0].Node.Text() != "1" {
		t.Fatalf("first capture iterator result = %#v, ordinal %d", match, ordinal)
	}
}

func TestQueryCursorExecAcceptsNodeValueAndPointer(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	root := tree.RootNode()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	if err := cursor.Exec(q, &root); err != nil {
		t.Fatalf("Exec(pointer): %v", err)
	}
	if _, ok := cursor.NextMatch(); !ok {
		t.Fatal("Exec(pointer) produced no matches")
	}
	if err := cursor.Exec(q, root); err != nil {
		t.Fatalf("Exec(value): %v", err)
	}
	if _, ok := cursor.NextMatch(); !ok {
		t.Fatal("Exec(value) produced no matches")
	}
	if err := cursor.Exec(q, (*wasitter.Node)(nil)); !errors.Is(err, wasitter.ErrInvalidHandle) {
		t.Fatalf("Exec(nil pointer) error = %v, want ErrInvalidHandle", err)
	}
}

func TestQueryCursorIteratorOptionsAndDepthForms(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()

	depth := uint(0)
	cursor.SetMaxStartDepth(&depth)
	seen := 0
	matches := cursor.MatchesWithOptions(q, tree.RootNode(), []byte(`[1, 2, 3]`), wasitter.QueryCursorOptions{
		ProgressCallback: func(state wasitter.QueryCursorState) bool {
			seen++
			return true
		},
	})
	if len(matches) > 1 || seen == 0 {
		t.Fatalf("cancelled iterator len/callbacks = %d/%d", len(matches), seen)
	}
	// A nil pointer is the upstream spelling for clearing the depth limit.
	cursor.SetMaxStartDepth((*uint)(nil))
	if got := cursor.Matches(q, tree.RootNode(), []byte(`[1, 2, 3]`)); len(got) != 3 {
		t.Fatalf("cleared depth matches = %d, want 3", len(got))
	}
}

func TestQueryCursorExecWithOptionsDrivesNextProgressCallback(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	calls := 0
	if err := cursor.ExecWithOptions(q, tree.RootNode(), wasitter.QueryCursorOptions{
		ProgressCallback: func(wasitter.QueryCursorState) bool {
			calls++
			return calls >= 2
		},
	}); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for {
		if _, ok := cursor.NextMatch(); !ok {
			break
		}
		seen++
	}
	if calls < 2 {
		t.Fatalf("ExecWithOptions callback calls = %d, want at least 2", calls)
	}
	if seen >= 3 {
		t.Fatalf("callback cancellation yielded %d matches, want fewer than 3", seen)
	}
}

func TestQueryCursorCapturesIteratorRemove(t *testing.T) {
	p, _, tree := parseJSON(t, `{"a": 1, "b": 2}`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(pair key: (string) @key value: (_) @value)`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	captures := cursor.Captures(q, tree.RootNode(), []byte(`{"a": 1, "b": 2}`))
	match, index := captures.Next()
	if match == nil {
		t.Fatal("missing first capture")
	}
	if index >= uint(len(match.Captures)) {
		t.Fatalf("capture ordinal %d out of range %d", index, len(match.Captures))
	}
	match.Remove()
	// The remaining capture from the same match must be discarded; captures
	// from the second pair remain available.
	seen := 0
	for m, i := captures.Next(); m != nil; m, i = captures.Next() {
		if i >= uint(len(m.Captures)) {
			t.Fatalf("capture ordinal %d out of range %d", i, len(m.Captures))
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("removing first match discarded all captures")
	}
}

func TestQueryIteratorZeroWidthRangeIntersection(t *testing.T) {
	p, _, tree := parseJSON(t, `["abc"]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(string) @s`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(q, tree.RootNode(), []byte(`["abc"]`))
	matches.SetByteRange(3, 3) // strictly inside the string node
	if len(matches) != 1 {
		t.Fatalf("interior zero-width range returned %d matches, want 1", len(matches))
	}
	matches = cursor.Matches(q, tree.RootNode(), []byte(`["abc"]`))
	matches.SetByteRange(1, 1) // exactly at the node's start is outside
	if len(matches) != 0 {
		t.Fatalf("boundary zero-width range returned %d matches, want 0", len(matches))
	}
}

func TestQueryIteratorRangeSentinels(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()

	// Tree-sitter treats an end byte of zero as the unbounded-end sentinel.
	matches := cursor.Matches(q, tree.RootNode(), []byte(`[1, 2]`))
	matches.SetByteRange(4, 0)
	if len(matches) != 1 || matches[0].Captures[0].Node.Text() != "2" {
		t.Fatalf("byte sentinel range = %#v, want only 2", matches)
	}

	// The point-range iterator setter uses the analogous (0,0) sentinel.
	matches = cursor.Matches(q, tree.RootNode(), []byte(`[1, 2]`))
	matches.SetPointRange(wasitter.Point{Row: 0, Column: 4}, wasitter.Point{})
	if len(matches) != 1 || matches[0].Captures[0].Node.Text() != "2" {
		t.Fatalf("point sentinel range = %#v, want only 2", matches)
	}
}

func TestQueryIteratorRejectsWideByteRangeAndExtraArgs(t *testing.T) {
	if uint64(^uint(0)) <= uint64(math.MaxUint32) {
		t.Skip("native uint is 32-bit")
	}
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	matches := cursor.Matches(q, tree.RootNode(), []byte(`[1, 2]`))
	// Build these as runtime values so the file compiles on 32-bit hosts. The
	// architecture guard above skips the assertion before the narrowing cast on
	// those hosts.
	wideStart := uint64(math.MaxUint32) + 1
	matches.SetByteRange(uint(wideStart), uint(wideStart+1))
	if len(matches) != 2 {
		t.Fatalf("wide byte range changed matches: got %d, want 2", len(matches))
	}
	if got := cursor.Matches(q, tree.RootNode(), []byte(`[1, 2]`), nil, nil, nil); got != nil {
		t.Fatalf("extra iterator arguments returned %#v, want nil", got)
	}
}
