package wasitter_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

// Standard Tree-sitter query syntax places predicates in sibling
// parenthesized expressions (`(number) @n (#eq? @n "1")`). Legacy bridge
// execution must retain that association just like the native query engine.
func TestFallbackTopLevelPredicateSibling(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	rt, err := wasitter.NewRuntime(context.Background(), wasm)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	lang, err := rt.LoadLanguage("json")
	if err != nil {
		t.Fatal(err)
	}
	defer lang.Close()
	p, err := wasitter.NewParserWithRuntime(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetLanguage(lang); err != nil {
		t.Fatal(err)
	}
	tree, err := p.Parse([]byte(`[1, 2]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	q, err := wasitter.NewQuery(tree.Language(), `(number) @n (#eq? @n "1")`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	matches := q.Matches(tree.RootNode())
	if len(matches) != 1 || matches[0].Captures[0].Node.Text() != "1" {
		t.Fatalf("top-level fallback predicate matches = %#v, want only 1", matches)
	}
}

// Keep a small compatibility check for modules built before the optional
// native query ABI was added. The fallback matcher is intentionally limited,
// but the common wildcard and built-in predicate forms should retain their
// useful Tree-sitter behavior.
func TestLegacyQueryFallbackWildcardAndPredicate(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	query, err := wasitter.NewQuery(tree.Language(), `(_) @node`)
	if err != nil {
		t.Fatalf("fallback wildcard query: %v", err)
	}
	defer query.Close()
	if query.Handle() != 0 {
		t.Fatalf("fallback query handle = %#x, want zero", query.Handle())
	}
	if query.StartByteForPattern(0) != 0 || query.EndByteForPattern(0) == 0 {
		t.Fatalf("fallback pattern range = %d..%d, want non-empty source span", query.StartByteForPattern(0), query.EndByteForPattern(0))
	}
	if !query.IsPatternRooted(0) {
		t.Fatal("fallback parenthesized wildcard should report a rooted pattern")
	}
	matches := query.Matches(tree.RootNode())
	if len(matches) != 4 { // document, array, and the two numbers
		t.Fatalf("fallback wildcard matches = %d, want 4", len(matches))
	}
	for _, match := range matches {
		if len(match.Captures) != 1 || query.CaptureName(match.Captures[0].Index) != "node" {
			t.Fatalf("fallback wildcard match = %#v", match)
		}
	}
	// Quoted literals represent anonymous grammar symbols. The compatibility
	// matcher should expose them just like the native query engine.
	punctuation, err := wasitter.NewQuery(tree.Language(), `"," @comma`)
	if err != nil {
		t.Fatalf("fallback anonymous query: %v", err)
	}
	defer punctuation.Close()
	commas := punctuation.Matches(tree.RootNode())
	if len(commas) != 1 || commas[0].Captures[0].Node.Type() != "," {
		t.Fatalf("fallback anonymous matches = %#v, want one comma", commas)
	}

	// parseWithWASM uses the compact `[1, 1]` fixture; excluding `2` should
	// therefore retain both numeric captures.
	predicate, err := wasitter.NewQuery(tree.Language(), `((number) @n (#not-any-of? @n "2"))`)
	if err != nil {
		t.Fatalf("fallback predicate query: %v", err)
	}
	defer predicate.Close()
	matches = predicate.Matches(tree.RootNode())
	if len(matches) != 2 || matches[0].Captures[0].Node.Text() != "1" || matches[1].Captures[0].Node.Text() != "1" {
		t.Fatalf("fallback not-any-of matches = %#v, want both number 1 nodes", matches)
	}

	// A capture attached to an alternation applies to every branch. The
	// compatibility parser should propagate the annotation instead of dropping
	// it when it recursively materializes the branch patterns.
	alternatives, err := wasitter.NewQuery(tree.Language(), `[(number) (array)] @value`)
	if err != nil {
		t.Fatalf("fallback alternation query: %v", err)
	}
	defer alternatives.Close()
	if got := alternatives.CaptureName(0); got != "value" {
		t.Fatalf("fallback alternation capture name = %q, want value", got)
	}
	if got := len(alternatives.Matches(tree.RootNode())); got != 3 {
		t.Fatalf("fallback alternation matches = %d, want 3", got)
	}
}

// Anonymous grammar symbols support the same postfix occurrence modifiers as
// named patterns. Keep the legacy parser's metadata in lock-step with the
// native query compiler for a modifier written before the capture annotation.
func TestFallbackAnonymousTokenPreCaptureQuantifier(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	query, err := wasitter.NewQuery(tree.Language(), `","? @comma`)
	if err != nil {
		t.Fatalf("fallback anonymous pre-capture quantifier: %v", err)
	}
	defer query.Close()
	if got := query.CaptureQuantifierForID(0, 0); got != wasitter.CaptureQuantifierZeroOrOne {
		t.Fatalf("anonymous capture quantifier = %v, want zero-or-one", got)
	}
	// The compact matcher intentionally does not implement repetition state,
	// but it should still expose the underlying comma token when present.
	if got := len(query.Matches(tree.RootNode())); got != 1 {
		t.Fatalf("anonymous pre-capture matches = %d, want 1", got)
	}
}

func TestFallbackRejectsUnknownNodeType(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	query, err := wasitter.NewQuery(tree.Language(), `(does_not_exist) @x`)
	if query != nil {
		query.Close()
		t.Fatal("unknown fallback node type unexpectedly compiled")
	}
	var queryErr *wasitter.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("fallback error = %v, want *QueryError", err)
	}
	if queryErr.Kind != wasitter.QueryErrorNodeType || queryErr.Message != "does_not_exist" {
		t.Fatalf("fallback error = %#v, want node-type diagnostic", queryErr)
	}
}

func TestFallbackRejectsUnknownPredicateCapture(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	query, err := wasitter.NewQuery(tree.Language(), `((number) @n (#eq? @missing "1"))`)
	if query != nil {
		query.Close()
		t.Fatal("unknown fallback predicate capture unexpectedly compiled")
	}
	var queryErr *wasitter.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("fallback error = %v, want *QueryError", err)
	}
	if queryErr.Kind != wasitter.QueryErrorCapture || queryErr.Message != "missing" {
		t.Fatalf("fallback error = %#v, want capture diagnostic", queryErr)
	}
}

func TestFallbackRejectsUnknownField(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	query, err := wasitter.NewQuery(tree.Language(), `(pair badfield: (number))`)
	if query != nil {
		query.Close()
		t.Fatal("unknown fallback field unexpectedly compiled")
	}
	var queryErr *wasitter.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("fallback error = %v, want *QueryError", err)
	}
	if queryErr.Kind != wasitter.QueryErrorField || queryErr.Message != "badfield" || queryErr.Offset != 6 {
		t.Fatalf("fallback error = %#v, want field diagnostic at byte 6", queryErr)
	}
}

// Tree-sitter's query lexer (the portable runtime uses the C locale) accepts
// only ASCII identifier characters for capture names and bare predicate
// values. A legacy bridge has to apply the same lexical rule; otherwise a
// query can compile successfully when the native query ABI is unavailable and
// fail after switching to the bundled/native path.
func TestFallbackRejectsNonASCIIIdentifiers(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	validQuoted, err := wasitter.NewQuery(tree.Language(), `((string) @s (#eq? @s "猫"))`)
	if err != nil {
		t.Fatalf("quoted Unicode predicate value rejected: %v", err)
	}
	if validQuoted == nil {
		t.Fatal("quoted Unicode predicate value returned nil query")
	}
	_ = validQuoted.Close()
	for _, source := range []string{
		`(number) @猫`,
		`((number) @n (#eq? @n 猫))`,
		`((number) @n (#猫? @n "1"))`,
	} {
		t.Run(source, func(t *testing.T) {
			q, err := wasitter.NewQuery(tree.Language(), source)
			if q != nil {
				_ = q.Close()
				t.Fatalf("non-ASCII query unexpectedly compiled: %q", source)
			}
			if err == nil {
				t.Fatalf("non-ASCII query returned nil error: %q", source)
			}
			var queryErr *wasitter.QueryError
			if !errors.As(err, &queryErr) {
				t.Fatalf("error = %T %v, want *QueryError", err, err)
			}
			if queryErr.Kind != wasitter.QueryErrorSyntax {
				t.Fatalf("error kind = %d, want syntax", queryErr.Kind)
			}
		})
	}
}

// A bridge can also expose the compiler while omitting only the cursor
// constructor. Ensure that this partial native surface takes the same
// compatibility path instead of reporting a successful but empty query.
func TestPartialNativeQueryABIUsesFallbackExecution(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	wasm = hideWASMExport(t, wasm, "tsw_query_cursor_new", "old_query_cursor_new")
	tree := parseWithWASM(t, wasm)
	query, err := wasitter.NewQuery(tree.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("partial native query: %v", err)
	}
	defer query.Close()
	if query.Handle() == 0 {
		t.Fatal("query compiler unexpectedly unavailable")
	}
	if got := len(query.Matches(tree.RootNode())); got != 2 {
		t.Fatalf("partial native query matches = %d, want 2", got)
	}
	if err := query.DisableCapture("n"); err != nil {
		t.Fatalf("partial native DisableCapture: %v", err)
	}
	if got := len(query.Matches(tree.RootNode())); got != 2 {
		// Disabling a capture leaves the pattern match itself intact, but the
		// returned match must no longer contain that capture.
		t.Fatalf("partial native disabled-capture match count = %d, want 2", got)
	}
	for _, match := range query.Matches(tree.RootNode()) {
		if len(match.Captures) != 0 {
			t.Fatalf("disabled capture survived partial fallback: %#v", match)
		}
	}
	if err := query.DisablePattern(0); err != nil {
		t.Fatalf("partial native DisablePattern: %v", err)
	}
	if got := len(query.Matches(tree.RootNode())); got != 0 {
		t.Fatalf("partial native disabled-pattern matches = %d, want 0", got)
	}
}

func TestFallbackCursorRangeAndPredicateAfterExec(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(),
		"tsw_query_cursor_new", "old_query_cursor_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `((number) @n (#eq? @n "1"))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := wasitter.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	// Mutating the range after Exec must affect a materialized compatibility
	// cursor too. The fixture is [1, 1], so this range intersects only the
	// second number; the predicate still accepts it.
	c.SetByteRange(4, 5)
	if m, ok := c.NextMatch(); !ok || len(m.Captures) != 1 || m.Captures[0].Node.Text() != "1" {
		t.Fatalf("post-Exec fallback range match = %#v, ok=%v", m, ok)
	}
	if _, ok := c.NextMatch(); ok {
		t.Fatal("post-Exec fallback range returned an extra match")
	}

	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	c.SetByteRange(4, 5)
	if capture, ok := c.NextCapture(); !ok || capture.Node.StartByte() != 4 {
		t.Fatalf("post-Exec fallback capture = %#v, ok=%v", capture, ok)
	}
	if _, ok := c.NextCapture(); ok {
		t.Fatal("post-Exec fallback range returned an extra capture")
	}
}

func TestPartialNativeMissingNextMatchFallsBack(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(),
		"tsw_query_cursor_next_match", "old_query_cursor_next_match")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := wasitter.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		m, ok := c.NextMatch()
		if !ok {
			break
		}
		if len(m.Captures) != 1 {
			t.Fatalf("fallback match = %#v", m)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("missing-next-match fallback count = %d, want 2", count)
	}
}

func TestPartialNativeMissingNextCaptureUsesMatchFallback(t *testing.T) {
	wasm := wasitter.BuiltinJSONWASM()
	// This symbol appears in the export and linker-name sections. Rename all
	// occurrences so Runtime.function cannot resolve either copy.
	wasm = bytes.ReplaceAll(wasm, []byte("tsw_query_cursor_next_capture"), []byte("old_query_cursor_next_capturX"))
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := wasitter.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		capture, ok := c.NextCapture()
		if !ok {
			break
		}
		if capture.Node.IsNull() {
			t.Fatal("fallback capture is null")
		}
		count++
	}
	if count != 2 {
		t.Fatalf("missing-next-capture fallback count = %d, want 2", count)
	}
}

// Compatibility matches should expose the same zero-based, per-execution ids
// as Tree-sitter's native TSQueryMatch stream.  Assign ids before host-side
// predicate/range filtering so rejected candidates leave the same observable
// gaps as native iteration.
func TestFallbackQueryMatchIDsAreStableAndZeroBased(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	all := q.Matches(tree.RootNode())
	if len(all) != 2 || all[0].ID != 0 || all[1].ID != 1 {
		t.Fatalf("fallback detached ids = %#v, want 0,1", all)
	}

	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	cursor.SetByteRange(4, 5)
	first, ok := cursor.NextMatch()
	if !ok || first.ID != 1 {
		t.Fatalf("fallback cursor filtered id = %d, ok=%v; want 1", first.ID, ok)
	}
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("fallback cursor range returned an extra match")
	}
}

// Predicate scope must remain per pattern even when the same capture name is
// reused. A source-wide compatibility predicate list would require the one
// capture to satisfy both literals and incorrectly reject every match.
func TestFallbackPredicatesAreScopedPerPattern(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(),
		"((number) @n (#eq? @n \"1\"))\n((number) @n (#eq? @n \"2\"))")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	matches := q.Matches(tree.RootNode())
	if len(matches) != 2 {
		t.Fatalf("fallback per-pattern matches = %#v, want two first-pattern matches", matches)
	}
	if len(q.TextPredicates) != 2 || len(q.TextPredicates[0]) != 1 || len(q.TextPredicates[1]) != 1 {
		t.Fatalf("fallback predicate metadata = %#v, want one predicate per pattern", q.TextPredicates)
	}
	if got := q.TextPredicates[0][0].Value; got != "1" {
		t.Fatalf("fallback first predicate value = %#v, want 1", got)
	}
	if got := q.TextPredicates[1][0].Value; got != "2" {
		t.Fatalf("fallback second predicate value = %#v, want 2", got)
	}
	for _, match := range matches {
		if match.PatternIndex != 0 || len(match.Captures) != 1 || match.Captures[0].Node.Text() != "1" {
			t.Fatalf("fallback per-pattern match = %#v, want pattern 0 capture 1", match)
		}
	}
}

// An unfiltered pattern must retain its own empty predicate vector.  If the
// compatibility matcher treats that vector as nil, it falls back to the
// source-wide predicate scanner and accidentally applies a preceding pattern's
// predicate to every later pattern.
func TestFallbackUnfilteredPatternDoesNotInheritPredicate(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(),
		"((number) @n (#eq? @n \"1\"))\n(number) @n")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	matches := q.Matches(tree.RootNode())
	// The fixture is [1, 1], so the first pattern contributes two matches and
	// the unfiltered second pattern contributes two more.
	if len(matches) != 4 {
		t.Fatalf("fallback inherited predicate: got %d matches (%#v), want 4", len(matches), matches)
	}
	var first, second int
	for _, match := range matches {
		switch match.PatternIndex {
		case 0:
			first++
		case 1:
			second++
		}
	}
	if first != 2 || second != 2 {
		t.Fatalf("fallback pattern counts = %d/%d, want 2/2", first, second)
	}
}

func TestFallbackAlternativesSharePatternIndex(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `[(number) (array)] @value`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.PatternCount(); got != 1 {
		t.Fatalf("fallback alternative PatternCount = %d, want 1", got)
	}
	for _, match := range q.Matches(tree.RootNode()) {
		if match.PatternIndex != 0 {
			t.Fatalf("fallback alternative pattern index = %d, want 0", match.PatternIndex)
		}
	}
	if err := q.DisablePattern(0); err != nil {
		t.Fatal(err)
	}
	if got := len(q.Matches(tree.RootNode())); got != 0 {
		t.Fatalf("disabled fallback alternative matches = %d, want 0", got)
	}
}

func TestFallbackAlternativePredicateAppliesToEveryBranch(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `[(number) (array)] @value (#eq? @value "1")`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	// The compatibility fixture is [1, 1]. The number branches pass; the
	// array branch must be rejected by the same predicate scope.
	matches := q.Matches(tree.RootNode())
	if len(matches) != 2 {
		t.Fatalf("alternative predicate matches = %#v, want two number branches", matches)
	}
	for _, match := range matches {
		if len(match.Captures) != 1 || match.Captures[0].Node.Type() != "number" {
			t.Fatalf("alternative predicate admitted non-number branch: %#v", match)
		}
	}
}

func TestFallbackAlternativeFollowedByPatternKeepsIndices(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), "[(number) (array)] @value\n(number) @number")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.PatternCount(); got != 2 {
		t.Fatalf("fallback grouped PatternCount = %d, want 2", got)
	}
	seen := map[uint32]int{}
	for _, match := range q.Matches(tree.RootNode()) {
		seen[match.PatternIndex]++
	}
	if seen[0] != 3 || seen[1] != 2 {
		t.Fatalf("fallback grouped pattern counts = %#v, want 3/2", seen)
	}
}

func TestFallbackGroupedMetadataUsesSourcePatternIndices(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), "[(number) (array)] @value\n((number) @n (#eq? @n \"2\"))")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if len(q.TextPredicatesForPattern(0)) != 0 || len(q.TextPredicatesForPattern(1)) != 1 {
		t.Fatalf("grouped metadata = %#v, want predicate only on pattern 1", q.TextPredicates)
	}
}

// Max-start-depth is defined in terms of a pattern's root. Even a pattern with
// no captures must therefore be filtered by the depth of the node it matched;
// using the execution root as a proxy would incorrectly admit every
// descendant.
func TestFallbackMaxStartDepthUsesPatternAnchor(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	q, err := wasitter.NewQuery(tree.Language(), `(number)`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := wasitter.NewQueryCursor()
	defer c.Close()
	c.SetMaxStartDepth(uint32(1))
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	matches, err := c.Matches(q, tree.RootNode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("fallback depth matches = %#v, want no depth-2 number roots", matches)
	}
}

func TestFallbackRejectsMalformedBuiltInPredicate(t *testing.T) {
	wasm := hideWASMExport(t, wasitter.BuiltinJSONWASM(), "tsw_query_new", "old_query_new")
	tree := parseWithWASM(t, wasm)
	defer tree.Close()
	for _, source := range []string{
		`((number) @n (#eq? @n))`,
		`((number) @n (#match? @n "["))`,
		`((number) @n (#set!))`,
	} {
		q, err := wasitter.NewQuery(tree.Language(), source)
		if q != nil {
			q.Close()
			t.Fatalf("%q unexpectedly compiled", source)
		}
		var queryErr *wasitter.QueryError
		if !errors.As(err, &queryErr) || queryErr.Kind != wasitter.QueryErrorPredicate {
			t.Fatalf("%q err=%T %v, want predicate QueryError", source, err, err)
		}
	}
}
