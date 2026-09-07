package wasitter_test

import (
	"context"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

func TestRuntimeCloseInvalidatesPublicGuestHandles(t *testing.T) {
	runtime, err := wasitter.NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	language, err := runtime.LoadLanguage("json")
	if err != nil {
		t.Fatal(err)
	}
	parser, err := wasitter.NewParserWithRuntime(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := parser.SetLanguage(language); err != nil {
		t.Fatal(err)
	}
	lookahead := language.LookaheadIterator(0)
	if lookahead == nil {
		t.Fatal("LookaheadIterator returned nil for state zero")
	}

	if language.Handle() == 0 || parser.Handle() == 0 || lookahead.Handle() == 0 {
		t.Fatal("a live object exposed a zero guest handle")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	if got := language.Handle(); got != 0 {
		t.Errorf("Language.Handle after Runtime.Close = %#x, want zero", got)
	}
	if got := parser.Handle(); got != 0 {
		t.Errorf("Parser.Handle after Runtime.Close = %#x, want zero", got)
	}
	if got := lookahead.Handle(); got != 0 {
		t.Errorf("LookaheadIterator.Handle after Runtime.Close = %#x, want zero", got)
	}

	// Object-level closes remain idempotent even though the module already
	// released the corresponding guest allocations.
	if err := lookahead.Close(); err != nil {
		t.Errorf("LookaheadIterator.Close after Runtime.Close: %v", err)
	}
	if err := parser.Close(); err != nil {
		t.Errorf("Parser.Close after Runtime.Close: %v", err)
	}
	if err := language.Close(); err != nil {
		t.Errorf("Language.Close after Runtime.Close: %v", err)
	}
}
