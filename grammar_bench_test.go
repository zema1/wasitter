package sitterwasm_test

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// BenchmarkGeneratedGrammarParse exercises every grammar artifact checked into
// the package. Keeping the benchmark artifact-driven means adding a row to the
// registry and committing its .wasm file automatically adds a benchmark case;
// no benchmark source change is needed for each new language.
//
// Runtime/parser setup is intentionally outside the timed region. The measured
// operation is the public parse path, including construction and disposal of
// the result tree. Known grammars use the small fixtures shared with the
// artifact smoke tests; an unrecognised grammar falls back to an empty input
// so a new official grammar still has a useful, runnable baseline before a
// richer language fixture is added.
func BenchmarkGeneratedGrammarParse(b *testing.B) {
	paths, err := filepath.Glob(filepath.Join("internal", "wasm", "assets", "sitterwasm-*.wasm"))
	if err != nil {
		b.Fatal(err)
	}
	if len(paths) == 0 {
		b.Skip("no checked-in grammar artifacts")
	}
	sort.Strings(paths)
	for _, path := range paths {
		name := benchmarkGrammarName(path)
		if name == "" {
			continue
		}
		b.Run(name, func(b *testing.B) {
			wasm := sitterwasm.BuiltinWASM(name)
			if len(wasm) == 0 {
				b.Skip("artifact is not embedded")
			}
			rt, err := sitterwasm.NewRuntime(context.Background(), wasm)
			if err != nil {
				b.Fatalf("NewRuntime: %v", err)
			}
			b.Cleanup(func() { _ = rt.Close() })
			language, err := rt.LoadLanguage(name)
			if err != nil {
				b.Fatalf("LoadLanguage(%q): %v", name, err)
			}
			b.Cleanup(func() { _ = language.Close() })
			parser, err := sitterwasm.NewParserWithRuntime(rt)
			if err != nil {
				b.Fatalf("NewParserWithRuntime: %v", err)
			}
			b.Cleanup(func() { _ = parser.Close() })
			if err := parser.SetLanguage(language); err != nil {
				b.Fatalf("SetLanguage: %v", err)
			}

			source := []byte(generatedGrammarSample(name))
			// generatedGrammarSample intentionally returns an empty string for
			// languages without a checked-in fixture. Parsing empty input is a
			// valid baseline for all Tree-sitter grammars.
			b.ReportAllocs()
			b.SetBytes(int64(len(source)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tree, err := parser.Parse(source, nil)
				if err != nil {
					b.Fatalf("Parse: %v", err)
				}
				root, err := tree.RootNodeE()
				if err != nil {
					_ = tree.Close()
					b.Fatalf("RootNode: %v", err)
				}
				if root.IsNull() || root.Type() == "" {
					_ = tree.Close()
					b.Fatalf("invalid root node for %q", name)
				}
				if err := tree.Close(); err != nil {
					b.Fatalf("Tree.Close: %v", err)
				}
			}
		})
	}
}

func benchmarkGrammarName(path string) string {
	name := filepath.Base(path)
	name = strings.TrimPrefix(name, "sitterwasm-")
	return strings.TrimSuffix(name, ".wasm")
}
