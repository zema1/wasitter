package sitterwasm_test

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// The official Go binding intentionally treats an any-* predicate as an
// existential early-return check.  If no item satisfies the condition, its
// SatisfiesTextPredicate implementation reaches the final successful return
// (for capture-to-capture predicates that return additionally requires equal
// capture cardinality).  Keep this slightly surprising behavior stable so a
// query moved between native and WASM bindings produces the same matches.
func TestAnyTextPredicatesMatchOfficialBinding(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	for _, tc := range []struct {
		name  string
		src   string
		count int
	}{
		{name: "any-eq", src: `((number) @n (#any-eq? @n "9"))`, count: 3},
		{name: "any-match", src: `((number) @n (#any-match? @n "^9$"))`, count: 3},
		{name: "any-not-eq", src: `((number) @n (#any-not-eq? @n "1"))`, count: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(p.Language(), tc.src)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			if got := len(q.Matches(tree.RootNode())); got != tc.count {
				t.Fatalf("matches = %d, want %d", got, tc.count)
			}
		})
	}
}

func TestQueryMetadataPopulatesCaptureIdAliases(t *testing.T) {
	p, _, tree := parseJSON(t, `[1]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#eq? @n "1"))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if len(q.TextPredicates) != 1 || len(q.TextPredicates[0]) != 1 {
		t.Fatalf("TextPredicates = %#v", q.TextPredicates)
	}
	pred := q.TextPredicates[0][0]
	if pred.CaptureID != uint32(pred.CaptureId) {
		t.Fatalf("capture aliases disagree: %d/%d", pred.CaptureID, pred.CaptureId)
	}
	if len(q.GeneralPredicates(0)) != 0 {
		t.Fatalf("unexpected general predicates: %#v", q.GeneralPredicates(0))
	}
}

func TestAnyOfEmptyLiteralListIsNotDropped(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	for _, tc := range []struct {
		name  string
		src   string
		count int
	}{
		{name: "any-of", src: `((number) @n (#any-of? @n))`, count: 0},
		{name: "not-any-of", src: `((number) @n (#not-any-of? @n))`, count: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(p.Language(), tc.src)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			if len(q.TextPredicates) != 1 || len(q.TextPredicates[0]) != 1 {
				t.Fatalf("TextPredicates = %#v, want one empty-list predicate", q.TextPredicates)
			}
			if values, ok := q.TextPredicates[0][0].Value.([]string); !ok || len(values) != 0 {
				t.Fatalf("empty any-of values = %#v (ok=%v), want an empty []string", q.TextPredicates[0][0].Value, ok)
			}
			if got := len(q.Matches(tree.RootNode())); got != tc.count {
				t.Fatalf("matches = %d, want %d", got, tc.count)
			}
		})
	}
}

func TestNativePredicateFilteringIsPerPattern(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(),
		"((number) @only_one (#eq? @only_one \"1\"))\n((number) @all_numbers)")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	matches := q.Matches(tree.RootNode())
	if len(matches) != 3 {
		t.Fatalf("matches = %d, want 3 (one filtered pattern + two unfiltered)", len(matches))
	}
	var filtered, unfiltered int
	for _, match := range matches {
		if match.PatternIndex == 0 {
			filtered++
		} else if match.PatternIndex == 1 {
			unfiltered++
		}
	}
	if filtered != 1 || unfiltered != 2 {
		t.Fatalf("pattern match counts = %d/%d, want 1/2", filtered, unfiltered)
	}
}

func TestPredicateExtractionKeepsNamesCaseSensitive(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	// The C query compiler accepts unknown predicates as user-defined.  Since
	// the host has no implementation for #MATCH?, it must leave both matches
	// intact; parsing it as the lowercase built-in #match? would incorrectly
	// filter the result.
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#MATCH? @n "1"))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := len(q.Matches(tree.RootNode())); got != 2 {
		t.Fatalf("matches = %d, want 2 for user-defined case-sensitive predicate", got)
	}
	if got := len(q.GeneralPredicates(0)); got != 1 || q.GeneralPredicates(0)[0].Operator != "MATCH?" {
		t.Fatalf("general predicates = %#v", q.GeneralPredicates(0))
	}
}

// Legacy bridges must accept the same bare predicate symbols as the native
// Tree-sitter query compiler. In particular, property predicates in real
// grammars are commonly written as `#set! name value` (without quotes), and
// older grammars use the `.eq?` prefix instead of `#eq?`.
func TestFallbackBareAndDotPredicatesAndPropertyMetadata(t *testing.T) {
	wasm := hideWASMExport(t, sitterwasm.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()

	q, err := sitterwasm.NewQuery(tree.Language(),
		`((number) @n (.eq? @n one) (#set! kind number) (#is? kind))`)
	if err != nil {
		t.Fatalf("fallback bare/dot query: %v", err)
	}
	defer q.Close()
	// The fixture contains only numeric literals 1; the bare `one` value does
	// not match, proving that the predicate was decoded rather than ignored.
	if got := len(q.Matches(tree.RootNode())); got != 0 {
		t.Fatalf("dot/bare predicate matches = %d, want 0", got)
	}
	if len(q.TextPredicates) != 1 || len(q.TextPredicates[0]) != 1 {
		t.Fatalf("fallback text metadata = %#v, want one predicate", q.TextPredicates)
	}
	if got := q.TextPredicates[0][0].Value; got != "one" {
		t.Fatalf("fallback bare predicate value = %#v, want one", got)
	}
	settings := q.PropertySettings(0)
	if len(settings) != 1 || settings[0].Key != "kind" || settings[0].Value == nil || *settings[0].Value != "number" {
		t.Fatalf("fallback property settings = %#v, want kind=number", settings)
	}
	properties := q.PropertyPredicates(0)
	if len(properties) != 1 || !properties[0].Positive || properties[0].Property.Key != "kind" {
		t.Fatalf("fallback property predicates = %#v, want positive kind", properties)
	}
}

func TestFallbackBarePredicateValuesMatch(t *testing.T) {
	wasm := hideWASMExport(t, sitterwasm.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{name: "eq", src: `((number) @n (#eq? @n 1))`, want: 2},
		{name: "match", src: `((number) @n (#match? @n 1))`, want: 2}, // regexp `1` matches both fixture values
		{name: "any-of", src: `((number) @n (#any-of? @n 1 2))`, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(tree.Language(), tc.src)
			if err != nil {
				t.Fatalf("NewQuery(%q): %v", tc.src, err)
			}
			defer q.Close()
			if got := len(q.Matches(tree.RootNode())); got != tc.want {
				t.Fatalf("matches = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPredicateCaptureNamesMayContainDots(t *testing.T) {
	// Dots are valid in Tree-sitter capture names. Exercise both the native
	// query path and the legacy host-side predicate scanner, whose source
	// parser must identify the complete `@field.name` token.
	for _, tc := range []struct {
		name string
		wasm []byte
	}{
		{name: "native", wasm: sitterwasm.BuiltinJSONWASM()},
		{name: "fallback", wasm: hideWASMExport(t, sitterwasm.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := parseWithWASM(t, tc.wasm)
			defer tree.Close()
			q, err := sitterwasm.NewQuery(tree.Language(), `((number) @field.name (#eq? @field.name "9"))`)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			matches := q.Matches(tree.RootNode())
			if len(matches) != 0 {
				t.Fatalf("matches = %#v, want predicate to reject all values", matches)
			}
			if got := q.CaptureName(0); got != "field.name" {
				t.Fatalf("capture name = %q, want field.name", got)
			}
		})
	}
}

func TestQueryIteratorUsesExplicitTextBufferForPredicates(t *testing.T) {
	p, _, tree := parseJSON(t, `[1]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#eq? @n "2"))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
	defer c.Close()

	// The node's byte range is identical in both one-element arrays.  The
	// upstream iterator evaluates predicates against the explicitly supplied
	// text, so replacing the retained source (`[1]`) with (`[2]`) must make the
	// query match.
	text := []byte(`[2]`)
	matches := c.Matches(q, tree.RootNode(), text)
	if len(matches) != 1 || len(matches[0].Captures) != 1 {
		t.Fatalf("Matches with explicit text = %#v, want one capture", matches)
	}
	if got := matches[0].Captures[0].Node.Text(); got != "1" {
		t.Fatalf("retained node text = %q, want tree text 1", got)
	}

	captures := c.Captures(q, tree.RootNode(), text)
	if len(captures) != 1 {
		t.Fatalf("Captures with explicit text = %#v, want one capture", captures)
	}
}

// A predicate is evaluated by the Go host, but capture iteration must retain
// the ordering produced by Tree-sitter's native next_capture algorithm.  In
// particular, a parent match can be discovered after an earlier sibling
// capture even though the parent node starts at a smaller byte offset.  The
// optional full-match capture ABI lets the host inspect the whole match while
// preserving that interleaving.
func TestPredicateNextCapturePreservesNativeOrder(t *testing.T) {
	p, _, tree := parseJSON(t, `[1,2,{"x":3},4]`)
	defer tree.Close()

	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name:   "parent-before-later-sibling",
			source: "((array (number) @n) @a (#eq? @n \"2\"))\n(number) @m",
			want:   []string{"a@0", "m@1", "n@3", "m@3", "m@10", "m@13"},
		},
		{
			name:   "duplicate-capture",
			source: "((array (number) @n @n) @a (#eq? @n \"2\"))\n(number) @m",
			want:   []string{"a@0", "m@1", "n@3", "n@3", "m@3", "m@10", "m@13"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(p.Language(), tc.source)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			c := sitterwasm.NewQueryCursor()
			defer c.Close()
			if err := c.Exec(q, tree.RootNode()); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			for {
				capture, ok := c.NextCapture()
				if !ok {
					break
				}
				got = append(got, q.CaptureName(capture.Index)+"@"+strconv.FormatUint(uint64(capture.Node.StartByte()), 10))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("captures = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// Older bridge modules may expose next_capture but not the optional
// next_capture_match operation. Ensure the host's compatibility path keeps
// accepted wrappers alive (and does not free them twice) while evaluating a
// text predicate.
func TestPredicateNextCaptureWithoutFullMatchABI(t *testing.T) {
	wasm := hideWASMExport(t, sitterwasm.BuiltinJSONWASM(),
		"tsw_query_cursor_next_capture_match", "old_query_cursor_capture_match_____")
	rt, err := sitterwasm.NewRuntime(context.Background(), wasm)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	lang, err := rt.LoadLanguage("json")
	if err != nil {
		t.Fatal(err)
	}
	defer lang.Close()
	p, err := sitterwasm.NewParserWithRuntime(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetLanguage(lang); err != nil {
		t.Fatal(err)
	}
	tree, err := p.Parse([]byte(`[1,2]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	q, err := sitterwasm.NewQuery(lang, `((number) @n (#eq? @n "1"))`)
	if err != nil {
		t.Fatal(err)
	}
	c := sitterwasm.NewQueryCursor()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	got, ok := c.NextCapture()
	if !ok || got.Node.Text() != "1" {
		t.Fatalf("capture = %#v, ok=%v", got, ok)
	}
	if _, ok := c.NextCapture(); ok {
		t.Fatal("non-matching capture unexpectedly returned")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
}
