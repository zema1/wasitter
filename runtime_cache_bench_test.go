package wasitter

import (
	"context"
	"strings"
	"testing"
)

func BenchmarkGuestAllocation(b *testing.B) {
	rt, err := NewRuntime(context.Background(), BuiltinJSONWASM())
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Warm the export cache outside the measured region.
	ptr, err := rt.allocLocked(32)
	if err != nil {
		b.Fatal(err)
	}
	rt.freeLocked(ptr)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ptr, err := rt.allocLocked(32)
		if err != nil {
			b.Fatal(err)
		}
		rt.freeLocked(ptr)
	}
}

// Each iteration closes its tree so repeated navigation does not retain an
// ever-growing number of guest node wrappers across benchmark iterations.
func BenchmarkJavaScriptFieldTraversal(b *testing.B) {
	source := []byte(strings.Repeat(`function f(x) { return obj.method(x); }`+"\n", 128))
	for _, lookup := range []string{"name", "id"} {
		b.Run(lookup, func(b *testing.B) {
			parser, rt, err := NewJavaScriptParser(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			defer rt.Close()
			defer parser.Close()
			lang := parser.Language()
			defer lang.Close()
			fieldID := lang.FieldIDForName("function")
			b.ReportAllocs()
			b.SetBytes(int64(len(source)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tree, err := parser.Parse(source, nil)
				if err != nil {
					b.Fatal(err)
				}
				count := 0
				stack := []Node{tree.RootNode()}
				for len(stack) > 0 {
					n := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if n.Kind() == "call_expression" {
						var callee Node
						if lookup == "name" {
							callee = n.ChildByFieldName("function")
						} else {
							callee = n.ChildByFieldID(fieldID)
						}
						if callee.Kind() != "member_expression" {
							b.Fatal("wrong callee")
						}
						count++
					}
					children := n.NamedChildCount()
					for j := 0; j < children; j++ {
						stack = append(stack, n.NamedChild(j))
					}
				}
				if err := tree.Close(); err != nil {
					b.Fatal(err)
				}
				if count != 128 {
					b.Fatalf("calls=%d, want 128", count)
				}
			}
		})
	}
}

func BenchmarkJavaScriptParserReuse(b *testing.B) {
	input := []byte(`const load = () => fetch("/items");`)
	for _, reuse := range []bool{false, true} {
		name := "new_runtime"
		if reuse {
			name = "reuse_runtime"
		}
		b.Run(name, func(b *testing.B) {
			var parser *Parser
			var rt *Runtime
			var err error
			if reuse {
				parser, rt, err = NewJavaScriptParser(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				defer rt.Close()
				defer parser.Close()
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !reuse {
					parser, rt, err = NewJavaScriptParser(context.Background())
					if err != nil {
						b.Fatal(err)
					}
				}
				tree, err := parser.Parse(input, nil)
				if err != nil {
					b.Fatal(err)
				}
				if tree.RootNode().HasError() {
					b.Fatal("parse error")
				}
				if err := tree.Close(); err != nil {
					b.Fatal(err)
				}
				if !reuse {
					parser.Close()
					rt.Close()
				}
			}
		})
	}
}
