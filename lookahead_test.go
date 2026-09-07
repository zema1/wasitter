package wasitter_test

import (
	"context"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

func TestLookaheadIteratorJSON(t *testing.T) {
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	tree, err := p.Parse([]byte(`{"key": 1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	lang := p.Language()
	if lang == nil {
		t.Fatal("parser language is nil")
	}
	// A parsed node carries a state that is guaranteed to be valid for the
	// grammar.  Its next state is also a useful non-zero state for lookahead.
	node := tree.RootNode().NamedChild(0)
	state := node.ParseState()
	if state == 0 {
		state = node.NextParseState()
	}
	if state == 0 {
		t.Skip("JSON fixture did not expose a non-zero parse state")
	}
	it, err := wasitter.NewLookaheadIterator(lang, state)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	if it.Handle() == 0 || it.Language() == nil {
		t.Fatal("lookahead iterator is not initialized")
	}
	names, err := it.IterNamesE()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("lookahead iterator returned no symbols")
	}
	if !it.ResetState(state) {
		t.Fatal("ResetState rejected a valid state")
	}
	symbols := it.Iter()
	if len(symbols) != len(names) {
		t.Fatalf("Iter returned %d symbols, want %d", len(symbols), len(names))
	}
	for i, symbol := range symbols {
		if lang.NodeKindForID(symbol) != names[i] {
			t.Fatalf("symbol[%d] = %q, want %q", i, lang.NodeKindForID(symbol), names[i])
		}
	}
}
