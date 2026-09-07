package wasitter

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestFieldLookupCacheAndLegacyFallback(t *testing.T) {
	for _, grammar := range []string{"json", "javascript"} {
		t.Run(grammar, func(t *testing.T) {
			for _, hidden := range []string{"", "tsw_node_child_by_field_id", "tsw_node_language", "tsw_language_field_id_for_name"} {
				name := hidden
				if name == "" {
					name = "cached"
				}
				t.Run(name, func(t *testing.T) {
					wasm := BuiltinWASM(grammar)
					if hidden != "" {
						if !bytes.Contains(wasm, []byte(hidden)) {
							t.Fatal("missing fixture export", hidden)
						}
						wasm = bytes.Replace(wasm, []byte(hidden), []byte("old"+hidden[3:]), 1)
					}
					rt, err := NewRuntime(context.Background(), wasm)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					lang, err := rt.LoadLanguage(grammar)
					if err != nil {
						t.Fatal(err)
					}
					defer lang.Close()
					parser, err := NewParserWithRuntime(rt)
					if err != nil {
						t.Fatal(err)
					}
					defer parser.Close()
					if err := parser.SetLanguage(lang); err != nil {
						t.Fatal(err)
					}
					input := []byte(`{"key": [1, {"other": true}]}`)
					if grammar == "javascript" {
						input = []byte(`const x = (arg) => obj.method(arg); function f(y) { return x(y); }`)
					}
					tree, err := parser.Parse(input, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tree.Close()
					// Language wrapper/parser lifetime must not invalidate tree lookups.
					if err := parser.Close(); err != nil {
						t.Fatal(err)
					}
					fields := []string{"", "not_a_field", "value\x00suffix"}
					for id := uint16(1); uint32(id) <= lang.FieldCount(); id++ {
						fields = append(fields, lang.FieldNameForID(id))
					}
					stack := []Node{tree.RootNode()}
					for len(stack) > 0 {
						n := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						for _, field := range fields {
							// Call the original guest name accessor independently of the cache.
							result, _, err := rt.callWithInput(context.Background(), []string{"tsw_node_child_by_field_name"}, []uint64{uint64(n.handle)}, []byte(field))
							if err != nil {
								t.Fatal(err)
							}
							h, ok := guestNodeHandle(result)
							if !ok {
								t.Fatal("invalid guest child result")
							}
							want := tree.registerNode(h)
							got := n.ChildByFieldName(field)
							if !got.Equal(want) {
								t.Fatalf("%s field %q: got %s, want %s", n.Kind(), field, got.Kind(), want.Kind())
							}
						}
						for i := 0; i < n.NamedChildCount(); i++ {
							stack = append(stack, n.NamedChild(i))
						}
					}
					root := tree.RootNode()
					if err := tree.Close(); err != nil {
						t.Fatal(err)
					}
					if !root.ChildByFieldName("value").IsNull() {
						t.Fatal("closed tree returned a cached child")
					}
				})
			}
		})
	}
}

func TestFieldCacheSharedAcrossTreesAndConcurrentReads(t *testing.T) {
	parser, rt, err := NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer parser.Close()
	var nodes []Node
	for _, input := range []string{`{"a":1}`, `{"b":2}`} {
		tree, err := parser.Parse([]byte(input), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()
		nodes = append(nodes, tree.RootNode().NamedChild(0).NamedChild(0))
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				for i, n := range nodes {
					if got := n.ChildByFieldName("value").Utf8Text([]byte(fmt.Sprintf(`{"x":%d}`, i+1))); got != fmt.Sprint(i+1) {
						t.Errorf("cached field returned %q", got)
					}
				}
			}
		}()
	}
	wg.Wait()
	// Invalid caller-controlled names must not grow the cache indefinitely.
	for i := 0; i < 100; i++ {
		nodes[0].ChildByFieldName(fmt.Sprintf("unknown_%d", i))
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.fieldIDs) != 1 {
		t.Fatalf("language cache count=%d", len(rt.fieldIDs))
	}
	for _, fields := range rt.fieldIDs {
		if len(fields) != 1 || fields["value"] == 0 {
			t.Fatalf("unexpected field cache: %v", fields)
		}
	}
}
