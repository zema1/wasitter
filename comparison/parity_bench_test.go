package comparison

import (
	"bytes"
	"context"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	nativejson "github.com/tree-sitter/tree-sitter-json/bindings/go"
	wasm "github.com/zema1/sitterwasm"
)

var benchmarkSource = []byte(`{
  "name": "sitterwasm",
  "enabled": true,
  "version": 1,
  "items": [1, 2, 3, 5, 8, 13, 21],
  "nested": {"null": null, "text": "hello", "unicode": "树🌳"}
}`)

func BenchmarkParse(b *testing.B) {
	b.Run("wasm", func(b *testing.B) {
		parser, runtime, err := wasm.NewJSONParser(context.Background())
		if err != nil {
			b.Fatalf("NewJSONParser: %v", err)
		}
		// Cleanup hooks run after testing.B has stopped the benchmark timer, so
		// module teardown and its allocations do not contaminate the result.
		b.Cleanup(func() {
			_ = parser.Close()
			_ = runtime.Close()
		})
		b.ReportAllocs()
		b.SetBytes(int64(len(benchmarkSource)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tree, err := parser.Parse(benchmarkSource, nil)
			if err != nil {
				b.Fatal(err)
			}
			_ = tree.RootNode().Type()
			_ = tree.Close()
		}
	})
	b.Run("native", func(b *testing.B) {
		language := native.NewLanguage(nativejson.Language())
		parser := native.NewParser()
		if err := parser.SetLanguage(language); err != nil {
			parser.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			parser.Close()
		})
		b.ReportAllocs()
		b.SetBytes(int64(len(benchmarkSource)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tree := parser.Parse(benchmarkSource, nil)
			if tree == nil {
				b.Fatal("native parse returned nil")
			}
			_ = tree.RootNode().Kind()
			tree.Close()
		}
	})
}

func BenchmarkIncrementalParse(b *testing.B) {
	// Alternate one same-width edit so every iteration starts with a tree for
	// the opposite source. This keeps tree construction out of benchmark setup
	// after the first iteration while timing the same public operation on both
	// implementations: edit the old tree, incrementally parse, and close it.
	oldSource := append([]byte(nil), benchmarkSource...)
	newSource := bytes.Replace(oldSource, []byte(`"version": 1`), []byte(`"version": 2`), 1)
	editOffset := bytes.Index(oldSource, []byte(`"version": 1`)) + len(`"version": `)
	if editOffset < len(`"version": `) || bytes.Equal(oldSource, newSource) {
		b.Fatal("benchmark version field not found")
	}
	var row, column uint
	for _, value := range oldSource[:editOffset] {
		if value == '\n' {
			row++
			column = 0
		} else {
			column++
		}
	}

	b.Run("wasm", func(b *testing.B) {
		parser, runtime, err := wasm.NewJSONParser(context.Background())
		if err != nil {
			b.Fatalf("NewJSONParser: %v", err)
		}
		b.Cleanup(func() {
			_ = parser.Close()
			_ = runtime.Close()
		})
		tree, err := parser.Parse(oldSource, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = tree.Close() })
		edit := wasm.InputEdit{
			StartByte:   uint32(editOffset),
			OldEndByte:  uint32(editOffset + 1),
			NewEndByte:  uint32(editOffset + 1),
			StartPoint:  wasm.Point{Row: uint32(row), Column: uint32(column)},
			OldEndPoint: wasm.Point{Row: uint32(row), Column: uint32(column + 1)},
			NewEndPoint: wasm.Point{Row: uint32(row), Column: uint32(column + 1)},
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(oldSource)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := tree.Edit(edit); err != nil {
				b.Fatal(err)
			}
			target := newSource
			if i&1 != 0 {
				target = oldSource
			}
			next, err := parser.Parse(target, tree)
			if err != nil {
				b.Fatal(err)
			}
			_ = next.RootNode().Type()
			if err := tree.Close(); err != nil {
				b.Fatal(err)
			}
			tree = next
		}
	})

	b.Run("native", func(b *testing.B) {
		language := native.NewLanguage(nativejson.Language())
		parser := native.NewParser()
		if err := parser.SetLanguage(language); err != nil {
			parser.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			parser.Close()
		})
		tree := parser.Parse(oldSource, nil)
		if tree == nil {
			b.Fatal("native initial parse returned nil")
		}
		b.Cleanup(func() { tree.Close() })
		edit := native.InputEdit{
			StartByte:      uint(editOffset),
			OldEndByte:     uint(editOffset + 1),
			NewEndByte:     uint(editOffset + 1),
			StartPosition:  native.Point{Row: row, Column: column},
			OldEndPosition: native.Point{Row: row, Column: column + 1},
			NewEndPosition: native.Point{Row: row, Column: column + 1},
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(oldSource)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tree.Edit(&edit)
			target := newSource
			if i&1 != 0 {
				target = oldSource
			}
			next := parser.Parse(target, tree)
			if next == nil {
				b.Fatal("native incremental parse returned nil")
			}
			_ = next.RootNode().Kind()
			tree.Close()
			tree = next
		}
	})
}

func BenchmarkQueryMatches(b *testing.B) {
	const sourceQuery = `(number) @number`
	b.Run("wasm", func(b *testing.B) {
		parser, runtime, err := wasm.NewJSONParser(context.Background())
		if err != nil {
			b.Fatalf("NewJSONParser: %v", err)
		}
		b.Cleanup(func() {
			_ = parser.Close()
			_ = runtime.Close()
		})
		tree, err := parser.Parse(benchmarkSource, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = tree.Close() })
		query, err := wasm.NewQuery(tree.Language(), sourceQuery)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = query.Close() })
		cursor := wasm.NewQueryCursor()
		b.Cleanup(func() { _ = cursor.Close() })
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			matches := cursor.Matches(query, tree.RootNode(), benchmarkSource)
			count := 0
			for matches.Next() != nil {
				count++
			}
			if count == 0 {
				b.Fatal("WASM query returned no matches")
			}
		}
	})
	b.Run("native", func(b *testing.B) {
		language := native.NewLanguage(nativejson.Language())
		parser := native.NewParser()
		if err := parser.SetLanguage(language); err != nil {
			parser.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			parser.Close()
		})
		tree := parser.Parse(benchmarkSource, nil)
		if tree == nil {
			b.Fatal("native parse returned nil")
		}
		b.Cleanup(func() { tree.Close() })
		query, queryErr := native.NewQuery(language, sourceQuery)
		if queryErr != nil {
			b.Fatal(queryErr)
		}
		b.Cleanup(func() { query.Close() })
		cursor := native.NewQueryCursor()
		b.Cleanup(func() { cursor.Close() })
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			matches := cursor.Matches(query, tree.RootNode(), benchmarkSource)
			count := 0
			for matches.Next() != nil {
				count++
			}
			if count == 0 {
				b.Fatal("native query returned no matches")
			}
		}
	})
}

func BenchmarkCursorTraversal(b *testing.B) {
	b.Run("wasm", func(b *testing.B) {
		parser, runtime, err := wasm.NewJSONParser(context.Background())
		if err != nil {
			b.Fatalf("NewJSONParser: %v", err)
		}
		b.Cleanup(func() {
			_ = parser.Close()
			_ = runtime.Close()
		})
		tree, err := parser.Parse(benchmarkSource, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = tree.Close() })
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cursor := tree.RootNode().Walk()
			visited := false
			for {
				visited = true
				if cursor.GotoFirstChild() {
					continue
				}
				for {
					if cursor.GotoNextSibling() {
						break
					}
					if !cursor.GotoParent() {
						_ = cursor.Close()
						goto nextWASM
					}
				}
			}
		nextWASM:
			if !visited {
				b.Fatal("empty WASM cursor traversal")
			}
		}
	})
	b.Run("native", func(b *testing.B) {
		language := native.NewLanguage(nativejson.Language())
		parser := native.NewParser()
		if err := parser.SetLanguage(language); err != nil {
			parser.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			parser.Close()
		})
		tree := parser.Parse(benchmarkSource, nil)
		if tree == nil {
			b.Fatal("native parse returned nil")
		}
		b.Cleanup(func() { tree.Close() })
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cursor := tree.RootNode().Walk()
			visited := false
			for {
				visited = true
				if cursor.GotoFirstChild() {
					continue
				}
				for {
					if cursor.GotoNextSibling() {
						break
					}
					if !cursor.GotoParent() {
						cursor.Close()
						goto nextNative
					}
				}
			}
		nextNative:
			if !visited {
				b.Fatal("empty native cursor traversal")
			}
		}
	})
}
