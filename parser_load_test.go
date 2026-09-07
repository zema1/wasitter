package wasitter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

func TestParserFromWASMAndFile(t *testing.T) {
	for _, mode := range []string{"bytes", "file"} {
		t.Run(mode, func(t *testing.T) {
			wasm := BuiltinJavaScriptWASM()
			var p *Parser
			var rt *Runtime
			var err error
			if mode == "bytes" {
				p, rt, err = NewParserFromWASM(context.Background(), wasm)
			} else {
				// Language comes from the module, not its filename.
				path := filepath.Join(t.TempDir(), "rust.wasm")
				if err := os.WriteFile(path, wasm, 0600); err != nil {
					t.Fatal(err)
				}
				p, rt, err = NewParserFromFile(context.Background(), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			defer p.Close()
			language := p.Language()
			defer language.Close()
			if name := language.Name(); name != "javascript" {
				t.Fatalf("language = %q", name)
			}
			// Reuse the parser and retain the last tree after closing the parser.
			var tree *Tree
			for i := 0; i < 2; i++ {
				source := []byte(`function greet(name) { return name; }`)
				tree, err = p.ParseContext(context.Background(), source, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tree.Close()
				if root := tree.RootNode(); root.HasError() || root.Type() != "program" {
					t.Fatalf("unexpected parse: %s", tree.ToSExpression())
				}
				if got := tree.RootNode().NamedChild(0).ChildByFieldName("name").Content(source); got != "greet" {
					t.Fatalf("function name = %q", got)
				}
				if i == 0 {
					if err := tree.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if rt.Module().IsClosed() {
				t.Fatal("closing parser closed runtime")
			}
			if got := tree.RootNode().Type(); got != "program" {
				t.Fatalf("retained tree root = %q", got)
			}
		})
	}
}

func TestParserFromWASMAndFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.wasm")
	if p, rt, err := NewParserFromFile(context.Background(), path); p != nil || rt != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file = %v, %v, %v", p, rt, err)
	}
	for _, wasm := range [][]byte{nil, []byte("invalid"), {0, 'a', 's', 'm', 1, 0, 0, 0}} {
		if p, rt, err := NewParserFromWASM(context.Background(), wasm); p != nil || rt != nil || err == nil {
			t.Fatalf("invalid module = %v, %v, %v", p, rt, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, rt, err := NewParserFromWASM(ctx, BuiltinJSONWASM()); p != nil || rt != nil || err == nil {
		t.Fatalf("canceled construction = %v, %v, %v", p, rt, err)
	}
}

// Hide exports to exercise cleanup after runtime creation, parser allocation,
// and language assignment without relying on finalizers.
type parserLoadFailureModule struct {
	api.Module
	blocked string
	deletes int
}

func (m *parserLoadFailureModule) ExportedFunction(name string) api.Function {
	if name == m.blocked {
		return nil
	}
	fn := m.Module.ExportedFunction(name)
	if name == "tsw_parser_delete" && fn != nil {
		return &parserLoadDeleteFunction{Function: fn, module: m}
	}
	return fn
}

type parserLoadDeleteFunction struct {
	api.Function
	module *parserLoadFailureModule
}

func (f *parserLoadDeleteFunction) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	f.module.deletes++
	return f.Function.Call(ctx, params...)
}

func TestParserLoadFailureClosesOwnedRuntime(t *testing.T) {
	for _, stage := range []string{"language", "parser", "set language"} {
		t.Run(stage, func(t *testing.T) {
			wasm := BuiltinJSONWASM()
			if stage == "language" {
				// A valid module with one page of memory, but no language exports.
				wasm = []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 5, 3, 1, 0, 1}
			}
			rt, err := NewRuntime(context.Background(), wasm)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			mod := &parserLoadFailureModule{Module: rt.mod}
			switch stage {
			case "parser":
				mod.blocked = "tsw_parser_new"
			case "set language":
				mod.blocked = "tsw_parser_set_language"
			}
			rt.mod = mod
			p, returned, err := newParserWithOwnedRuntime(rt, "")
			if err == nil || p != nil || returned != nil {
				t.Fatalf("failed construction = %v, %v, %v", p, returned, err)
			}
			if !mod.IsClosed() || !rt.closed.Load() {
				t.Fatal("runtime not closed on failure")
			}
			if stage == "set language" && mod.deletes != 1 {
				t.Fatalf("parser delete calls = %d", mod.deletes)
			}
		})
	}
}
