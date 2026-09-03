package sitterwasm_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

func TestCompatibilityCursorConstructorAndInputCtx(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()

	rootTree, err := p.Parse([]byte(`[1]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rootTree.Close()
	root := rootTree.RootNode()
	cursor := sitterwasm.NewTreeCursor(&root)
	if cursor == nil || cursor.CurrentNode().Type() != "document" {
		t.Fatalf("NewTreeCursor current node = %#v", cursor)
	}
	defer cursor.Close()
	if cursor.CurrentNodePtr() == nil || cursor.NodePtr() == nil {
		t.Fatal("pointer cursor accessors returned nil")
	}

	input := []byte(`[2]`)
	tree, err := p.ParseInputCtx(context.Background(), nil, sitterwasm.Input{
		Read: func(offset uint32, _ sitterwasm.Point) []byte {
			if offset >= uint32(len(input)) {
				return nil
			}
			return input[offset:]
		},
		Encoding: sitterwasm.InputEncodingUTF8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if got := tree.RootNode().NamedChild(0).NamedChild(0).Text(); got != "2" {
		t.Fatalf("ParseInputCtx text = %q", got)
	}
}

func TestCompatibilityNilOldTreeArgumentForms(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()

	// The compatibility dispatcher also accepts (ctx, oldTree, input). An
	// untyped nil old tree is represented as an interface nil and must still
	// select this alternate form.
	tree, err := p.ParseCtx(context.Background(), nil, []byte(`[4]`))
	if err != nil {
		t.Fatalf("ParseCtx(ctx, nil, input): %v", err)
	}
	defer tree.Close()
	if got := tree.RootNode().NamedChild(0).NamedChild(0).Text(); got != "4" {
		t.Fatalf("ParseCtx alternate form text = %q", got)
	}

	input := []byte(`[5]`)
	// Likewise, the upstream ParseInput form is (oldTree, Input).
	inputTree, err := p.ParseInput(nil, sitterwasm.Input{
		Read: func(offset uint32, _ sitterwasm.Point) []byte {
			if offset >= uint32(len(input)) {
				return nil
			}
			return input[offset:]
		},
		Encoding: sitterwasm.InputEncodingUTF8,
	})
	if err != nil {
		t.Fatalf("ParseInput(nil, Input): %v", err)
	}
	defer inputTree.Close()
	if got := inputTree.RootNode().NamedChild(0).NamedChild(0).Text(); got != "5" {
		t.Fatalf("ParseInput modern form text = %q", got)
	}
}

func TestCompatibilityInputUTF16ByteOrders(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()

	for _, tc := range []struct {
		name     string
		encoding sitterwasm.InputEncoding
		order    binary.ByteOrder
	}{
		{"little", sitterwasm.InputEncodingUTF16LE, binary.LittleEndian},
		{"big", sitterwasm.InputEncodingUTF16BE, binary.BigEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units := []uint16{'[', '3', ']'}
			raw := make([]byte, len(units)*2)
			for i, unit := range units {
				tc.order.PutUint16(raw[i*2:], unit)
			}
			tree, err := p.ParseInputCtx(context.Background(), nil, sitterwasm.Input{
				Read: func(offset uint32, _ sitterwasm.Point) []byte {
					if offset >= uint32(len(raw)) {
						return nil
					}
					// Deliberately split in the middle of a code unit to exercise
					// the collector's carry handling.
					end := int(offset) + 3
					if end > len(raw) {
						end = len(raw)
					}
					return raw[offset:end]
				},
				Encoding: tc.encoding,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			if got := tree.Source(); got != "[3]" {
				t.Fatalf("decoded source = %q", got)
			}
		})
	}
}

func TestCallbackParsingChecksLifecycleBeforeCallback(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	called := false
	_, err = p.ParseInput(func(uint32, sitterwasm.Point) []byte {
		called = true
		return []byte("null")
	}, nil)
	if !errors.Is(err, sitterwasm.ErrClosed) {
		t.Fatalf("ParseInput after close = %v, want ErrClosed", err)
	}
	if called {
		t.Fatal("ParseInput invoked callback after parser close")
	}
}

func TestCompatibilityNodeIterators(t *testing.T) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	tree, err := p.Parse([]byte(`[1, {"x": 2}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	root := tree.RootNode()
	it := sitterwasm.NewIterator(&root, sitterwasm.DFSMode)
	defer it.Close()
	var got []string
	if err := it.ForEach(func(node *sitterwasm.Node) error {
		got = append(got, node.Type())
		return nil
	}); err != io.EOF {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "document" {
		t.Fatalf("DFS iterator = %#v", got)
	}
	named := sitterwasm.NewNamedIterator(&root, sitterwasm.BFSMode)
	defer named.Close()
	if node, err := named.Next(); err != nil || node == nil || node.Type() != "document" {
		t.Fatalf("BFS first = %#v, %v", node, err)
	}
}
