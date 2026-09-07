package wasitter_test

import (
	"context"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

// Defined function types are common when an application attaches metrics or
// buffering state to a Tree-sitter callback. The compatibility dispatchers
// receive callbacks through any, so verify they preserve Go's normal
// assignment compatibility instead of requiring an explicit conversion.
type namedUint32Reader func(uint32, wasitter.Point) []byte
type namedIntReader func(int, wasitter.Point) []byte

func TestDefinedInputCallbackTypesAreAccepted(t *testing.T) {
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()

	source := []byte(`[7]`)
	uintReader := namedUint32Reader(func(offset uint32, _ wasitter.Point) []byte {
		if offset >= uint32(len(source)) {
			return nil
		}
		return source[offset:]
	})
	tree, err := p.ParseInput(uintReader, nil)
	if err != nil {
		t.Fatalf("ParseInput(defined uint32 callback): %v", err)
	}
	if got := tree.Source(); got != string(source) {
		t.Fatalf("ParseInput source = %q, want %q", got, source)
	}
	_ = tree.Close()

	intReader := namedIntReader(func(offset int, _ wasitter.Point) []byte {
		if offset < 0 || offset >= len(source) {
			return nil
		}
		return source[offset:]
	})
	tree, err = p.ParseWithOptions(intReader, nil, nil)
	if err != nil {
		t.Fatalf("ParseWithOptions(defined int callback): %v", err)
	}
	if got := tree.Source(); got != string(source) {
		t.Fatalf("ParseWithOptions source = %q, want %q", got, source)
	}
	_ = tree.Close()
}
