package comparison

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	nativebash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	nativec "github.com/tree-sitter/tree-sitter-c/bindings/go"
	nativecpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	nativego "github.com/tree-sitter/tree-sitter-go/bindings/go"
	nativejava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	nativejavascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	nativejson "github.com/tree-sitter/tree-sitter-json/bindings/go"
	nativepython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	nativeruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	nativerust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	nativetypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	wasm "github.com/zema1/sitterwasm"
)

// TestGrammarParity discovers every generated artifact and compares it with
// the official native grammar release pinned by comparison/go.mod. The mise
// task selects a single subtest with -run when validating one language.
func TestGrammarParity(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "internal", "wasm", "assets", "sitterwasm-*.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no generated grammar artifacts found")
	}
	for _, path := range paths {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "sitterwasm-"), ".wasm")
		t.Run(name, func(t *testing.T) {
			compareGrammarArtifact(t, name, path)
		})
	}
}

func compareGrammarArtifact(t *testing.T, name, path string) {
	t.Helper()
	source, nativeLanguage := nativeGrammarFixture(t, name)
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wasmRuntime, err := wasm.NewRuntime(context.Background(), wasmBytes)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer wasmRuntime.Close()
	wasmLanguage, err := wasmRuntime.LoadLanguage(name)
	if err != nil {
		t.Fatalf("LoadLanguage(%q): %v", name, err)
	}
	wasmParser, err := wasm.NewParserWithRuntime(wasmRuntime)
	if err != nil {
		t.Fatalf("NewParserWithRuntime: %v", err)
	}
	defer wasmParser.Close()
	if err := wasmParser.SetLanguage(wasmLanguage); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	wasmTree, err := wasmParser.Parse([]byte(source), nil)
	if err != nil {
		t.Fatalf("WASM Parse: %v", err)
	}
	defer wasmTree.Close()

	nativeParser := native.NewParser()
	if err := nativeParser.SetLanguage(nativeLanguage); err != nil {
		nativeParser.Close()
		t.Fatalf("native SetLanguage: %v", err)
	}
	defer nativeParser.Close()
	nativeTree := nativeParser.Parse([]byte(source), nil)
	if nativeTree == nil {
		t.Fatal("native Parse returned nil")
	}
	defer nativeTree.Close()

	if got, want := wasmTree.ToSExpression(), nativeTree.RootNode().ToSexp(); got != want {
		t.Fatalf("S-expressions differ:\nWASM:   %s\nNative: %s", got, want)
	}
	if wasmView, nativeView := wasmNodeView(wasmTree.RootNode(), ""), nativeNodeView(nativeTree.RootNode(), ""); !reflect.DeepEqual(wasmView, nativeView) {
		t.Fatalf("tree views differ:\nWASM:   %#v\nNative: %#v", wasmView, nativeView)
	}

	querySource := `(_) @node`
	wasmQuery, err := wasm.NewQuery(wasmLanguage, querySource)
	if err != nil {
		t.Fatalf("WASM NewQuery: %v", err)
	}
	defer wasmQuery.Close()
	nativeQuery, nativeQueryErr := native.NewQuery(nativeLanguage, querySource)
	if nativeQueryErr != nil {
		t.Fatalf("native NewQuery: %v", nativeQueryErr)
	}
	defer nativeQuery.Close()
	wasmCursor := wasm.NewQueryCursor()
	defer wasmCursor.Close()
	nativeCursor := native.NewQueryCursor()
	defer nativeCursor.Close()
	wasmMatches := wasmMatchViews(wasmCursor.Matches(wasmQuery, wasmTree.RootNode(), []byte(source)), []byte(source))
	nativeMatches := nativeMatchViews(nativeCursor.Matches(nativeQuery, nativeTree.RootNode(), []byte(source)), []byte(source))
	if !reflect.DeepEqual(wasmMatches, nativeMatches) {
		t.Fatalf("query matches differ:\nWASM:   %#v\nNative: %#v", wasmMatches, nativeMatches)
	}
}

func nativeGrammarFixture(t *testing.T, name string) (string, *native.Language) {
	t.Helper()
	switch name {
	case "bash":
		return "#!/bin/bash\necho \"$HOME\"\n", native.NewLanguage(nativebash.Language())
	case "c":
		return "int main(void) { return 0; }\n", native.NewLanguage(nativec.Language())
	case "cpp":
		return "#include <vector>\nint main() { std::vector<int> v; return 0; }\n", native.NewLanguage(nativecpp.Language())
	case "go":
		return "package main\nfunc main() {}\n", native.NewLanguage(nativego.Language())
	case "java":
		return "class Main { public static void main(String[] args) {} }\n", native.NewLanguage(nativejava.Language())
	case "javascript":
		return "const answer = 42; function greet(name) { return `hello ${name}`; }", native.NewLanguage(nativejavascript.Language())
	case "json":
		return `{"ok":true,"items":[1,2,3]}`, native.NewLanguage(nativejson.Language())
	case "python":
		return "def greet(name):\n    return name\n", native.NewLanguage(nativepython.Language())
	case "ruby":
		return "def greet(name)\n  puts name\nend\n", native.NewLanguage(nativeruby.Language())
	case "rust":
		return "fn main() { let answer = 42; }\n", native.NewLanguage(nativerust.Language())
	case "typescript":
		return "interface User { name: string }\nconst user: User = { name: 'Ada' };\n", native.NewLanguage(nativetypescript.LanguageTypescript())
	case "tsx":
		return "const element = <div>Hello</div>;\n", native.NewLanguage(nativetypescript.LanguageTSX())
	default:
		t.Skipf("no native parity fixture registered for grammar %q", name)
		return "", nil
	}
}
