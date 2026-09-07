package wasitter_test

import (
	"errors"
	"testing"

	"github.com/zema1/wasitter"
)

func TestQueryCollectionReportsLifecycleErrors(t *testing.T) {
	p, _, tree := parseJSON(t, `[1]`)
	defer tree.Close()
	language := p.Language()
	defer language.Close()
	query, err := wasitter.NewQuery(language, `(number) @n`)
	if err != nil {
		t.Fatal(err)
	}
	defer query.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()

	if _, err := cursor.Matches(query, wasitter.Node{}, nil); !errors.Is(err, wasitter.ErrInvalidHandle) {
		t.Fatalf("null root error = %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cursor.Matches(query, tree.RootNode(), nil); !errors.Is(err, wasitter.ErrClosed) {
		t.Fatalf("Matches on closed cursor = %v", err)
	}
	if _, err := cursor.Captures(query, tree.RootNode(), nil); !errors.Is(err, wasitter.ErrClosed) {
		t.Fatalf("Captures on closed cursor = %v", err)
	}
}

func TestNewQueryRequiresLiveLanguage(t *testing.T) {
	if q, err := wasitter.NewQuery(nil, `(number)`); q != nil || !errors.Is(err, wasitter.ErrNoLanguage) {
		t.Fatalf("nil language = %v, %v", q, err)
	}
	p, _ := newJSONParser(t)
	language := p.Language()
	if err := language.Close(); err != nil {
		t.Fatal(err)
	}
	if q, err := wasitter.NewQuery(language, `(number)`); q != nil || !errors.Is(err, wasitter.ErrClosed) {
		t.Fatalf("closed language = %v, %v", q, err)
	}
}
