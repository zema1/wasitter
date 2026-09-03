package sitterwasm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	sitterwasm "github.com/zema1/sitterwasm"
)

// A parser close publishes its lifecycle bit before waiting for an in-flight
// parse.  If the guest has already produced a tree, the parse tail must still
// discard that tree instead of returning a value from a parser that is being
// destroyed.  The post-call progress hook gives this test a deterministic
// point at which to race Close without relying on parser timing.
func TestParserCloseRacingParseDoesNotReturnTree(t *testing.T) {
	parser, runtime, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatalf("NewJSONParser: %v", err)
	}
	defer runtime.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	parseDone := make(chan error, 1)
	go func() {
		_, parseErr := parser.ParseWithOptions(
			context.Background(),
			[]byte(`{"close":true}`),
			nil,
			&sitterwasm.ParseOptions{ProgressCallback: func(state sitterwasm.ParseState) bool {
				if state.CurrentByteOffset != 0 {
					close(entered)
					<-release
				}
				return false
			}},
		)
		parseDone <- parseErr
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("parse did not reach its post-call progress hook")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- parser.Close() }()
	// Close marks the parser closed before waiting for the parse mutex.  Give
	// that goroutine a scheduling opportunity, then let the parse finish.
	time.Sleep(time.Millisecond)
	close(release)

	select {
	case parseErr := <-parseDone:
		if !errors.Is(parseErr, sitterwasm.ErrClosed) {
			t.Fatalf("racing parse error = %v, want ErrClosed", parseErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("racing parse did not finish")
	}
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("Parser.Close: %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Parser.Close did not finish")
	}
}
