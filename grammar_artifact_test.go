package sitterwasm_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
	"github.com/zema1/sitterwasm/internal/grammarbuild"
)

// TestGeneratedGrammarWASM is intentionally artifact-driven. A grammar task
// can add another sitterwasm-<name>.wasm file without requiring a new Go test;
// the test discovers it, loads its exported language, and parses a minimal
// representative input.
func TestGeneratedGrammarWASM(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("internal", "wasm", "assets", "sitterwasm-*.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no generated grammar artifacts found")
	}
	registry, err := grammarbuild.LoadRegistry(filepath.Join("scripts", "grammar-registry.json"))
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "sitterwasm-"), ".wasm")
		t.Run(name, func(t *testing.T) {
			grammar, err := registry.Lookup(name)
			if err != nil {
				t.Fatalf("artifact %q is not registered: %v", name, err)
			}
			if grammar.Name != name {
				t.Fatalf("registry name = %q, want %q", grammar.Name, name)
			}
			wasm, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := grammarbuild.VerifyArtifact(path, path+".sha256"); err != nil {
				t.Fatalf("VerifyArtifact: %v", err)
			}
			rt, err := sitterwasm.NewRuntime(context.Background(), wasm)
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			defer rt.Close()
			language, err := rt.LoadLanguage(name)
			if err != nil {
				t.Fatalf("LoadLanguage(%q): %v", name, err)
			}
			parser, err := sitterwasm.NewParserWithRuntime(rt)
			if err != nil {
				t.Fatalf("NewParserWithRuntime: %v", err)
			}
			defer parser.Close()
			if err := parser.SetLanguage(language); err != nil {
				t.Fatalf("SetLanguage: %v", err)
			}
			source := generatedGrammarSample(name)
			tree, err := parser.Parse([]byte(source), nil)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			defer tree.Close()
			root, err := tree.RootNodeE()
			if err != nil {
				t.Fatalf("RootNode: %v", err)
			}
			if root.IsNull() || root.Type() == "" {
				t.Fatalf("invalid root node: %#v", root)
			}
		})
	}
}

func TestGrammarRegistryIsWellFormed(t *testing.T) {
	registry, err := grammarbuild.LoadRegistry(filepath.Join("scripts", "grammar-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]struct{})
	for _, grammar := range registry.Grammars {
		name := grammar.Name
		if _, ok := seen[name]; ok {
			t.Fatalf("registry contains duplicate language %q", name)
		}
		seen[name] = struct{}{}
		if err := grammar.Validate(); err != nil {
			t.Fatalf("registry grammar %q is invalid: %v", name, err)
		}
	}
	for _, required := range []string{"json", "javascript"} {
		if _, ok := seen[required]; !ok {
			t.Fatalf("registry is missing required language %q", required)
		}
	}
}

func generatedGrammarSample(language string) string {
	switch language {
	case "bash":
		return "#!/bin/bash\necho \"$HOME\"\n"
	case "c":
		return "int main(void) { return 0; }\n"
	case "cpp":
		return "#include <vector>\nint main() { return 0; }\n"
	case "java":
		return "class Main { public static void main(String[] args) {} }\n"
	case "json":
		return `{"ok":true}`
	case "javascript":
		return `const answer = 42; function greet(name) { return ` + "`hello ${name}`" + `; }`
	case "python":
		return "def greet(name):\n    return name\n"
	case "go":
		return "package main\nfunc main() {}\n"
	case "rust":
		return "fn main() {}\n"
	case "ruby":
		return "def greet(name)\n  puts name\nend\n"
	case "typescript":
		return "interface User { name: string }\n"
	case "tsx":
		return "const element = <div>Hello</div>;\n"
	default:
		return ""
	}
}

func TestJavaScriptConvenienceParser(t *testing.T) {
	parser, rt, err := sitterwasm.NewJavaScriptParser(context.Background())
	if err != nil {
		t.Fatalf("NewJavaScriptParser: %v", err)
	}
	defer parser.Close()
	defer rt.Close()
	tree, err := parser.Parse([]byte("const answer = 42;"), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	defer tree.Close()
	if got, want := tree.RootNode().Type(), "program"; got != want {
		t.Fatalf("root type = %q, want %q", got, want)
	}
}

func TestBuiltinGrammarLookup(t *testing.T) {
	for _, language := range []string{"json", "javascript"} {
		data := sitterwasm.BuiltinWASM(language)
		if len(data) < 8 || string(data[:4]) != "\x00asm" {
			t.Fatalf("BuiltinWASM(%q) returned an invalid module (%d bytes)", language, len(data))
		}
	}
	if got := sitterwasm.BuiltinWASM("../json"); got != nil {
		t.Fatal("BuiltinWASM accepted a path traversal name")
	}
	if got := sitterwasm.BuiltinWASM("not-checked-in"); got != nil {
		t.Fatal("BuiltinWASM returned an unregistered artifact")
	}

	rt, err := sitterwasm.NewBuiltinRuntime(context.Background(), " JavaScript ")
	if err != nil {
		t.Fatalf("NewBuiltinRuntime(normalized name): %v", err)
	}
	defer rt.Close()
	language, err := rt.LoadLanguage("javascript")
	if err != nil || language == nil {
		t.Fatalf("LoadLanguage after NewBuiltinRuntime: %v", err)
	}
	defer language.Close()
}
