package sitterwasm_test

import (
	"errors"
	"math"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// Upstream exposes QueryError.Error with a value receiver, so both the value
// and pointer forms satisfy the standard error interface.
var (
	_ error = sitterwasm.QueryError{}
	_ error = (*sitterwasm.QueryError)(nil)
)

func TestQueryCaptureQuantifiersRejectsWidePatternIndex(t *testing.T) {
	if uint64(^uint(0)) <= uint64(math.MaxUint32) {
		t.Skip("native uint is 32-bit")
	}
	p, _ := newJSONParser(t)
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	// Keep the value non-constant so this test still compiles on 32-bit
	// architectures; the guard above skips it there before the conversion is
	// used.
	widePattern := uint64(math.MaxUint32) + 1
	if got := q.CaptureQuantifiers(uint(widePattern)); got != nil {
		t.Fatalf("wide pattern index returned %#v, want nil", got)
	}
}

func TestQueryPredicatesForPatternOmitsDoneSeparators(t *testing.T) {
	p, _ := newJSONParser(t)
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#eq? @n "1") (#match? @n "^1$"))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	groups := q.PredicatesForPattern(0)
	if len(groups) != 2 {
		t.Fatalf("predicate groups = %#v, want 2", groups)
	}
	for i, group := range groups {
		if len(group) == 0 {
			t.Fatalf("predicate group %d is empty", i)
		}
		for _, step := range group {
			if step.Type == sitterwasm.QueryPredicateStepTypeDone {
				t.Fatalf("group %d contains Done separator: %#v", i, group)
			}
		}
	}
	flat, err := q.PredicateSteps(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) == 0 || flat[len(flat)-1].Type != sitterwasm.QueryPredicateStepTypeDone {
		t.Fatalf("flat predicate steps = %#v, want trailing Done", flat)
	}
}

func TestQueryErrorKindAndTypeCompatibility(t *testing.T) {
	// Keep the public numbering aligned with go-tree-sitter v0.25. The C
	// enum uses a separate zero-valued TSQueryErrorNone sentinel; conversion
	// is tested below through a real malformed query.
	want := []sitterwasm.QueryErrorKind{
		sitterwasm.QueryErrorSyntax,
		sitterwasm.QueryErrorNodeType,
		sitterwasm.QueryErrorField,
		sitterwasm.QueryErrorCapture,
		sitterwasm.QueryErrorPredicate,
		sitterwasm.QueryErrorStructure,
		sitterwasm.QueryErrorLanguage,
	}
	for i, kind := range want {
		if int(kind) != i {
			t.Fatalf("QueryError kind %d = %d, want %d", i, kind, i)
		}
		if got := sitterwasm.QueryErrorTypeToString(kind); got == "unknown" || got == "" {
			t.Fatalf("QueryErrorTypeToString(%d) = %q", kind, got)
		}
	}
	if got := sitterwasm.QueryErrorTypeToString(sitterwasm.QueryErrorNone); got != "none" {
		t.Fatalf("QueryErrorTypeToString(QueryErrorNone) = %q, want none", got)
	}
	if sitterwasm.QueryErrorType(sitterwasm.QueryErrorPredicate) != sitterwasm.QueryErrorPredicate {
		t.Fatal("QueryErrorType is not an alias for QueryErrorKind")
	}

	p, _ := newJSONParser(t)
	q, err := sitterwasm.NewQuery(p.Language(), `(does_not_exist) @x`)
	if q != nil {
		q.Close()
		t.Fatal("invalid query returned a query")
	}
	var queryErr *sitterwasm.QueryError
	if !errors.As(err, &queryErr) {
		t.Fatalf("error = %v, want *QueryError", err)
	}
	if queryErr.Kind != sitterwasm.QueryErrorNodeType || queryErr.Type != sitterwasm.QueryErrorType(queryErr.Kind) {
		t.Fatalf("error kind/type = %d/%d, want node type and matching alias", queryErr.Kind, queryErr.Type)
	}
}

func TestNewQueryAcceptsCurrentAndHistoricalArgumentOrders(t *testing.T) {
	p, _ := newJSONParser(t)
	defer p.Close()
	lang := p.Language()
	forms := []struct {
		name string
		make func() (*sitterwasm.Query, error)
	}{
		{name: "language-string", make: func() (*sitterwasm.Query, error) {
			return sitterwasm.NewQuery(lang, `(number) @n`)
		}},
		{name: "language-bytes", make: func() (*sitterwasm.Query, error) {
			return sitterwasm.NewQuery(lang, []byte(`(number) @n`))
		}},
		{name: "bytes-language", make: func() (*sitterwasm.Query, error) {
			return sitterwasm.NewQuery([]byte(`(number) @n`), lang)
		}},
		{name: "string-language", make: func() (*sitterwasm.Query, error) {
			return sitterwasm.NewQuery(`(number) @n`, lang)
		}},
	}
	for _, tc := range forms {
		t.Run(tc.name, func(t *testing.T) {
			q, err := tc.make()
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			if q.PatternCount() != 1 || q.CaptureName(0) != "n" {
				t.Fatalf("query metadata = patterns=%d capture=%q", q.PatternCount(), q.CaptureName(0))
			}
		})
	}
	q, err := sitterwasm.CompileQuery([]byte(`(number) @n`), lang)
	if err != nil {
		t.Fatalf("CompileQuery reversed form: %v", err)
	}
	q.Close()
}

func TestNewQueryRejectsUnsupportedArgumentPair(t *testing.T) {
	p, _ := newJSONParser(t)
	defer p.Close()
	if q, err := sitterwasm.NewQuery(42, p.Language()); q != nil || !errors.Is(err, sitterwasm.ErrUnsupported) {
		t.Fatalf("unsupported first argument returned query=%v err=%v", q, err)
	}
	if q, err := sitterwasm.NewQuery([]byte(`(number)`), nil); q != nil || !errors.Is(err, sitterwasm.ErrNoLanguage) {
		t.Fatalf("nil language returned query=%v err=%v", q, err)
	}
}

func TestQueryErrorUsesHistoricalTypeWhenKindUnset(t *testing.T) {
	err := sitterwasm.QueryError{Type: sitterwasm.QueryErrorField, Message: "name", Row: 0, Column: 1}
	got := err.Error()
	want := "Query error at 1:2. Invalid field name name"
	if got != want {
		t.Fatalf("Type-only QueryError.Error() = %q, want %q", got, want)
	}
}

func TestQueryErrorIncludesSourceTokenAndCaret(t *testing.T) {
	p, _ := newJSONParser(t)
	defer p.Close()
	cases := []struct {
		name      string
		source    string
		kind      sitterwasm.QueryErrorKind
		offset    uint32
		message   string
		errorText string
	}{
		{
			name:      "node type",
			source:    `(does_not_exist) @x`,
			kind:      sitterwasm.QueryErrorNodeType,
			offset:    1,
			message:   "does_not_exist",
			errorText: "Query error at 1:2. Invalid node type does_not_exist",
		},
		{
			name:      "field",
			source:    `(pair badfield: (number))`,
			kind:      sitterwasm.QueryErrorField,
			offset:    6,
			message:   "badfield",
			errorText: "Query error at 1:7. Invalid field name badfield",
		},
		{
			name:      "syntax",
			source:    `(pair value: (number)`,
			kind:      sitterwasm.QueryErrorSyntax,
			offset:    21,
			message:   "(pair value: (number)\n                     ^",
			errorText: "Query error at 1:22. Invalid syntax:\n(pair value: (number)\n                     ^",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(p.Language(), tc.source)
			if q != nil {
				q.Close()
				t.Fatal("invalid query unexpectedly compiled")
			}
			var queryErr *sitterwasm.QueryError
			if !errors.As(err, &queryErr) {
				t.Fatalf("error = %v, want *QueryError", err)
			}
			if queryErr.Kind != tc.kind || queryErr.Offset != tc.offset {
				t.Fatalf("kind/offset = %d/%d, want %d/%d", queryErr.Kind, queryErr.Offset, tc.kind, tc.offset)
			}
			if queryErr.Message != tc.message {
				t.Fatalf("message = %q, want %q", queryErr.Message, tc.message)
			}
			if got := queryErr.Error(); got != tc.errorText {
				t.Fatalf("Error() = %q, want %q", got, tc.errorText)
			}
		})
	}
}

func TestQueryRepeatedCapturesAndPredicateFiltering(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 1]`)
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @left @right)+`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var got []string
	for {
		match, ok := c.NextMatch()
		if !ok {
			break
		}
		if len(match.Captures) != 2 {
			t.Fatalf("repeated match has %d captures: %#v", len(match.Captures), match)
		}
		got = append(got,
			match.Captures[0].Node.Text()+":"+q.CaptureName(match.Captures[0].Index),
			match.Captures[1].Node.Text()+":"+q.CaptureName(match.Captures[1].Index),
		)
	}
	want := []string{"1:left", "1:right", "2:left", "2:right", "1:left", "1:right"}
	if len(got) != len(want) {
		t.Fatalf("repeated captures = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capture[%d] = %q, want %q (all=%#v)", i, got[i], want[i], got)
		}
	}

	// Predicates are evaluated after the guest has produced a complete match;
	// this must retain the same order while dropping the non-matching scalar.
	predicate, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#eq? @n "1"))`)
	if err != nil {
		t.Fatalf("predicate query: %v", err)
	}
	defer predicate.Close()
	if err := c.Exec(predicate, tree.RootNode()); err != nil {
		t.Fatalf("predicate Exec: %v", err)
	}
	var values []string
	for {
		capture, ok := c.NextCapture()
		if !ok {
			break
		}
		values = append(values, capture.Node.Text())
	}
	wantValues := []string{"1", "1"}
	if len(values) != len(wantValues) {
		t.Fatalf("predicate captures = %#v, want %#v", values, wantValues)
	}
	for i := range wantValues {
		if values[i] != wantValues[i] {
			t.Fatalf("predicate capture[%d] = %q, want %q", i, values[i], wantValues[i])
		}
	}
}

func TestQueryCursorRemoveMatchDropsBufferedCaptures(t *testing.T) {
	p, _, tree := parseJSON(t, `{"b": 2}`)
	q, err := sitterwasm.NewQuery(p.Language(), `((pair key: (string) @key value: (_) @value) (#match? @key "b"))`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	first, ok := c.NextCapture()
	if !ok || first.Node.Text() != `"b"` {
		t.Fatalf("first capture = %#v, ok=%v", first, ok)
	}
	if err := c.RemoveMatchE(0); err != nil {
		t.Fatalf("RemoveMatchE: %v", err)
	}
	if second, ok := c.NextCapture(); ok {
		t.Fatalf("buffered capture survived RemoveMatchE: %#v", second)
	}
}

func TestQueryRejectsMalformedBuiltInPredicate(t *testing.T) {
	p, _ := newJSONParser(t)
	for _, source := range []string{
		`((number) @n (#eq? @n))`,
		`((number) @n (#match? @n "["))`,
		`((number) @n (#set!))`,
	} {
		q, err := sitterwasm.NewQuery(p.Language(), source)
		if q != nil {
			q.Close()
			t.Fatalf("%q unexpectedly compiled", source)
		}
		var queryErr *sitterwasm.QueryError
		if !errors.As(err, &queryErr) {
			t.Fatalf("%q error = %v, want *QueryError", source, err)
		}
		if queryErr.Kind != sitterwasm.QueryErrorPredicate {
			t.Fatalf("%q error kind = %d, want QueryErrorPredicate", source, queryErr.Kind)
		}
	}
}

func TestQueryPredicateValidationMatchesNativeErrorShape(t *testing.T) {
	p, _ := newJSONParser(t)
	cases := []struct {
		name, source, message string
	}{
		{
			name:    "eq arity",
			source:  `((number) @n (#eq? @n))`,
			message: "Wrong number of arguments to #eq? predicate. Expected 2, got 1.",
		},
		{
			name:    "eq literal first argument",
			source:  `((number) @n (#eq? "x" "1"))`,
			message: "First argument to #eq? predicate must be a capture name. Got literal x.",
		},
		{
			name:    "match capture pattern",
			source:  `((number) @n (#match? @n @n))`,
			message: "Second argument to #match? predicate must be a literal. Got capture @n.",
		},
		{
			name:    "property duplicate capture",
			source:  `((number) @n (#set! @n @n))`,
			message: "Invalid arguments to set! predicate. Unexpected second capture name @n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := sitterwasm.NewQuery(p.Language(), tc.source)
			if q != nil {
				_ = q.Close()
				t.Fatal("malformed predicate unexpectedly compiled")
			}
			queryErr, ok := err.(*sitterwasm.QueryError)
			if !ok {
				t.Fatalf("error = %T %v, want *QueryError", err, err)
			}
			if queryErr.Message != tc.message {
				t.Fatalf("message = %q, want %q", queryErr.Message, tc.message)
			}
			if queryErr.Offset != 0 || queryErr.Column != 0 {
				t.Fatalf("predicate offset/column = %d/%d, want 0/0", queryErr.Offset, queryErr.Column)
			}
		})
	}
}

func TestQueryMetadataParityAPI(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @n (#eq? @n "1") (#set! "kind" "number") (#is? "kind"))`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	names := q.CaptureNames()
	if len(names) != 1 || names[0] != "n" {
		t.Fatalf("CaptureNames = %#v", names)
	}
	if id, ok := q.CaptureIndexForName("n"); !ok || id != 0 {
		t.Fatalf("CaptureIndexForName = %d, %v", id, ok)
	}
	if _, ok := q.CaptureIndexForName("missing"); ok {
		t.Fatal("missing capture unexpectedly found")
	}
	quantifiers := q.CaptureQuantifiers(0)
	if len(quantifiers) != 1 || quantifiers[0] != sitterwasm.CaptureQuantifierOne {
		t.Fatalf("CaptureQuantifiers = %#v", quantifiers)
	}
	if len(q.TextPredicates) != 1 || len(q.TextPredicates[0]) != 1 {
		t.Fatalf("TextPredicates = %#v", q.TextPredicates)
	}
	if q.TextPredicates[0][0].Type != sitterwasm.TextPredicateTypeEqString {
		t.Fatalf("text predicate type = %v", q.TextPredicates[0][0].Type)
	}
	if got, ok := q.TextPredicates[0][0].Value.(string); !ok || got != "1" {
		t.Fatalf("text predicate value = %#v", q.TextPredicates[0][0].Value)
	}
	if len(q.PropertySettings(0)) != 1 || q.PropertySettings(0)[0].Key != "kind" {
		t.Fatalf("PropertySettings = %#v", q.PropertySettings(0))
	}
	if len(q.PropertyPredicates(0)) != 1 || !q.PropertyPredicates(0)[0].Positive {
		t.Fatalf("PropertyPredicates = %#v", q.PropertyPredicates(0))
	}
	if len(q.GeneralPredicates(0)) != 0 {
		t.Fatalf("GeneralPredicates = %#v", q.GeneralPredicates(0))
	}
	if got := len(q.Matches(tree.RootNode())); got != 1 {
		t.Fatalf("predicate matches = %d, want 1", got)
	}
}

func TestQueryCapturePredicateMetadataUsesUpstreamValueType(t *testing.T) {
	p, _ := newJSONParser(t)
	q, err := sitterwasm.NewQuery(p.Language(), `((number) @left @right (#eq? @left @right))`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if len(q.TextPredicates) != 1 || len(q.TextPredicates[0]) != 1 {
		t.Fatalf("TextPredicates = %#v", q.TextPredicates)
	}
	pred := q.TextPredicates[0][0]
	if pred.Type != sitterwasm.TextPredicateTypeEqCapture {
		t.Fatalf("predicate type = %v, want EqCapture", pred.Type)
	}
	value, ok := pred.Value.(uint)
	if !ok || value != 1 {
		t.Fatalf("capture predicate value = %#v (ok=%v), want uint(1)", pred.Value, ok)
	}
}

func TestQueryMatchRemovalAndCaptureHelpers(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	first, ok := c.NextMatch()
	if !ok {
		t.Fatal("missing first match")
	}
	if len(first.NodesForCaptureIndex(0)) != 1 {
		t.Fatalf("first match id/captures = %d/%#v", first.Id(), first.Captures)
	}
	if err := first.RemoveE(); err != nil {
		t.Fatalf("RemoveE: %v", err)
	}
	seen := 1
	for {
		if _, ok := c.NextMatch(); !ok {
			break
		}
		seen++
	}
	if seen != 3 {
		t.Fatalf("matches after removing emitted match = %d, want 3 (remove only in-progress state)", seen)
	}
}

func TestQueryCursorNextCaptureSkipsZeroCaptureMatches(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `(_)* @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
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
			t.Fatal("NextCapture returned a null node")
		}
		count++
	}
	if count == 0 {
		t.Fatal("NextCapture returned no captures")
	}
}

func TestQueryCursorNextCapturePreservesNativeOrder(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2]`)
	defer tree.Close()
	q, err := sitterwasm.NewQuery(p.Language(), `(array (number) @n) @a`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c := sitterwasm.NewQueryCursor()
	defer c.Close()
	if err := c.Exec(q, tree.RootNode()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		capture, ok := c.NextCapture()
		if !ok {
			break
		}
		got = append(got, q.CaptureName(capture.Index)+"="+capture.Node.Text())
	}
	want := []string{"a=[1, 2]", "a=[1, 2]", "n=1", "n=2"}
	if len(got) != len(want) {
		t.Fatalf("capture count = %d, got %#v want %#v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capture[%d] = %q, want %q (all=%#v)", i, got[i], want[i], got)
		}
	}
}
