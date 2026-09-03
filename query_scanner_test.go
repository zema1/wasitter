package sitterwasm

import "testing"

func TestParseTextPredicatesHandlesParenthesesAndComments(t *testing.T) {
	query := `; #eq? @ignored "nope)"
((string) @value (#eq? @value "a)b"))
((string) @other (#match? @other "a\\)b"))`
	predicates := parseTextPredicates(query)
	if len(predicates) != 2 {
		t.Fatalf("parsed %d predicates, want 2: %#v", len(predicates), predicates)
	}
	if predicates[0].op != "eq?" || predicates[0].capture != "value" || predicates[0].value != "a)b" {
		t.Fatalf("first predicate = %#v", predicates[0])
	}
	if predicates[1].op != "match?" || predicates[1].capture != "other" || predicates[1].regex == nil || !predicates[1].regex.MatchString("a)b") {
		t.Fatalf("second predicate = %#v", predicates[1])
	}
}

func TestParseTextPredicatesMalformedInputTerminates(t *testing.T) {
	for _, query := range []string{
		`((number) @n (#eq? @n))`,
		`((number) @n (#match? @n nope))`,
		`((number) @n (#any-of? @n @other))`,
	} {
		// The test itself is intentionally simple; its purpose is to guard the
		// scanner's forward-progress invariant for malformed/partial legacy
		// query sources (which the native compiler would reject earlier).
		_ = parseTextPredicates(query)
	}
}
