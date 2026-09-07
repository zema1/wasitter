package wasitter_test

import (
	"context"
	"errors"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

func TestNativeQueryABIAndMetadata(t *testing.T) {
	p, rt := newJSONParser(t)
	q, err := wasitter.NewQuery(p.Language(), `(number) @number`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	if q.Handle() == 0 {
		t.Fatal("query handle is zero; bundled module should use native ABI")
	}
	if q.PatternCount() != 1 || q.CaptureCount() != 1 || q.StringCount() != 0 {
		t.Fatalf("query counts = patterns=%d captures=%d strings=%d", q.PatternCount(), q.CaptureCount(), q.StringCount())
	}
	if q.CaptureName(0) != "number" {
		t.Fatalf("capture name = %q", q.CaptureName(0))
	}
	if q.StartByteForPattern(0) != 0 || q.EndByteForPattern(0) == 0 {
		t.Fatalf("pattern offsets = %d..%d", q.StartByteForPattern(0), q.EndByteForPattern(0))
	}
	if rt == nil {
		t.Fatal("nil runtime")
	}
}

func TestNativeQueryPredicatesAndCursorRanges(t *testing.T) {
	p, _ := newJSONParser(t)
	tree, err := p.Parse([]byte(`[1, 2, 1]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), `((number) @n (#eq? @n "1"))`)
	if err != nil {
		t.Fatalf("predicate query: %v", err)
	}
	defer q.Close()
	matches := q.Matches(tree.RootNode())
	if len(matches) != 2 {
		t.Fatalf("predicate matches = %d, want 2", len(matches))
	}
	for _, m := range matches {
		if len(m.Captures) != 1 || m.Captures[0].Node.Text() != "1" {
			t.Errorf("unexpected predicate match: %#v", m)
		}
	}

	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	var captures []string
	for {
		capture, ok := cursor.NextCapture()
		if !ok {
			break
		}
		captures = append(captures, capture.Node.Text())
	}
	if len(captures) != 2 || captures[0] != "1" || captures[1] != "1" {
		t.Fatalf("captures = %#v", captures)
	}

	// A byte range that intersects only the middle scalar should suppress the
	// two outer matches while retaining the complete matched node.
	rangeQuery, err := wasitter.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rangeQuery.Close()
	if err := cursor.Exec(rangeQuery, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	cursor.SetByteRange(4, 5)
	m, ok := cursor.NextMatch()
	if !ok || len(m.Captures) != 1 || m.Captures[0].Node.Text() != "2" {
		t.Fatalf("range match = %#v, ok=%v", m, ok)
	}
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("range cursor returned an extra match")
	}
}

func TestNativeQueryErrorsAndDisableOperations(t *testing.T) {
	p, _ := newJSONParser(t)
	q, err := wasitter.NewQuery(p.Language(), `(does_not_exist) @x`)
	if q != nil {
		t.Fatal("invalid query returned a query value")
	}
	var queryErr *wasitter.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("error = %v, want QueryError", err)
	}
	if queryErr.Kind != wasitter.QueryErrorNodeType {
		t.Fatalf("query error kind = %d, want node type", queryErr.Kind)
	}

	q, err = wasitter.NewQuery(p.Language(), `(number) @n (string) @s`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.DisableCapture("n"); err != nil {
		t.Fatalf("DisableCapture: %v", err)
	}
	tree, err := p.Parse([]byte(`[1, "x"]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	for _, match := range q.Matches(tree.RootNode()) {
		for _, capture := range match.Captures {
			if capture.Node.Type() == "number" {
				t.Errorf("disabled number capture still returned: %#v", match)
			}
		}
	}
	if err := q.DisablePattern(0); err != nil {
		t.Fatalf("DisablePattern: %v", err)
	}
}

func TestQueryContextConstructorStillWorks(t *testing.T) {
	// Keep a direct context call in the parity suite so accidental changes to
	// constructor defaults are caught by a test that does not use package
	// helpers.
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	if _, err := p.Parse([]byte(`null`), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNativeQueryMultiplePatternsWildcardsAndQuantifiers(t *testing.T) {
	p, _ := newJSONParser(t)
	tree, err := p.Parse([]byte(`{"a": [1, 2], "b": "x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	q, err := wasitter.NewQuery(p.Language(), "(number) @n\n(string) @s")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.PatternCount() != 2 || q.CaptureCount() != 2 {
		t.Fatalf("counts = %d/%d", q.PatternCount(), q.CaptureCount())
	}
	if q.CaptureName(1) != "s" {
		t.Fatalf("capture 1 = %q", q.CaptureName(1))
	}
	all := q.Matches(tree.RootNode())
	if len(all) != 5 {
		t.Fatalf("multiple-pattern matches = %d, want 5", len(all))
	}
	for _, match := range all {
		if match.PatternIndex > 1 || len(match.Captures) != 1 {
			t.Fatalf("invalid pattern match = %#v", match)
		}
	}

	wild, err := wasitter.NewQuery(p.Language(), `(_) @any`)
	if err != nil {
		t.Fatal(err)
	}
	defer wild.Close()
	if got := len(wild.Matches(tree.RootNode())); got == 0 {
		t.Fatal("wildcard returned no named nodes")
	}

	repeated, err := wasitter.NewQuery(p.Language(), `[(number) @n (string) @s]`)
	if err != nil {
		t.Fatal(err)
	}
	defer repeated.Close()
	if repeated.CaptureQuantifierForID(0, 0) == wasitter.CaptureQuantifierZero {
		t.Error("capture quantifier unexpectedly zero")
	}
}

func TestNativeQueryTextPredicateVariants(t *testing.T) {
	p, _ := newJSONParser(t)
	tree, err := p.Parse([]byte(`[1, 2, 3]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		{name: "match", text: `((number) @n (#match? @n "^2"))`, want: 1},
		{name: "any-of", text: `((number) @n (#any-of? @n "1" "3"))`, want: 2},
		{name: "not-eq", text: `((number) @n (#not-eq? @n "2"))`, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := wasitter.NewQuery(p.Language(), tc.text)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			if got := len(q.Matches(tree.RootNode())); got != tc.want {
				t.Fatalf("matches = %d, want %d", got, tc.want)
			}
		})
	}
}
