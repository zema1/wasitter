package sitterwasm_test

import (
	"context"
	"errors"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

func TestParserCompatibilityAliasesAndCustomDecoder(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()

	if got := p.OperationLimit(); got != int(p.TimeoutMicros()) {
		t.Fatalf("OperationLimit = %d, TimeoutMicros = %d", got, p.TimeoutMicros())
	}
	if err := p.SetOperationLimit(2500); err != nil {
		t.Fatalf("SetOperationLimit: %v", err)
	}
	if got := p.TimeoutMicros(); got != 2500 {
		t.Fatalf("TimeoutMicros after SetOperationLimit = %d", got)
	}
	if err := p.SetOperationLimit(-1); err != nil {
		t.Fatalf("SetOperationLimit(-1): %v", err)
	}
	if got := p.TimeoutMicros(); got != 0 {
		t.Fatalf("negative operation limit = %d, want 0", got)
	}

	// Decode a tiny ASCII-compatible custom encoding in which each source byte
	// is an ASCII code point. This exercises the host-side conversion while
	// retaining the same callback contract as Tree-sitter.
	input := []byte(`{"ok":true}`)
	tree, err := p.ParseCustomEncoding(func(offset int, _ sitterwasm.Point) []byte {
		if offset >= len(input) {
			return nil
		}
		return input[offset:]
	}, nil, nil, sitterwasm.CustomDecoderFunc(func(data []byte) (int32, uint32) {
		return int32(data[0]), 1
	}))
	if err != nil {
		t.Fatalf("ParseCustomEncoding: %v", err)
	}
	defer tree.Close()
	if got := tree.RootNode().Type(); got != "document" {
		t.Fatalf("custom-decoded root type = %q", got)
	}
	if got := tree.Source(); got != string(input) {
		t.Fatalf("custom-decoded source = %q, want %q", got, input)
	}

	if _, err := p.ParseInputSpec(sitterwasm.Input{
		Read: func(offset uint32, _ sitterwasm.Point) []byte {
			if offset >= uint32(len(input)) {
				return nil
			}
			return input[offset:]
		},
		Encoding: sitterwasm.InputEncodingUTF16,
	}, nil); !errors.Is(err, sitterwasm.ErrUnsupported) {
		t.Fatalf("UTF-16 InputSpec error = %v, want ErrUnsupported", err)
	}
}

func TestCustomDecoderRejectsInvalidProgress(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	_, err = p.ParseCustomEncoding(func(offset int, _ sitterwasm.Point) []byte {
		if offset != 0 {
			return nil
		}
		return []byte("x")
	}, nil, nil, sitterwasm.CustomDecoderFunc(func([]byte) (int32, uint32) {
		return 'x', 0
	}))
	if err == nil {
		t.Fatal("invalid custom decoder progress unexpectedly succeeded")
	}
}
