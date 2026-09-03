package sitterwasm_test

import (
	"errors"
	"io"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestParseReaderChecksLifecycleBeforeReading(t *testing.T) {
	var parser sitterwasm.Parser
	called := false
	_, err := parser.ParseReader(readerFunc(func([]byte) (int, error) {
		called = true
		return 0, io.EOF
	}))
	if !errors.Is(err, sitterwasm.ErrNoRuntime) {
		t.Fatalf("ParseReader on an unbound parser = %v, want ErrNoRuntime", err)
	}
	if called {
		t.Fatal("ParseReader invoked the reader after lifecycle validation failed")
	}
}

func TestParseReaderRejectsInvalidReadCount(t *testing.T) {
	parser, runtime, err := sitterwasm.NewJSONParser(nil)
	if err != nil {
		t.Fatalf("NewJSONParser: %v", err)
	}
	defer runtime.Close()
	defer parser.Close()

	_, err = parser.ParseReader(readerFunc(func(p []byte) (int, error) {
		return len(p) + 1, nil
	}))
	if err == nil {
		t.Fatal("ParseReader accepted an invalid reader count")
	}
}
