package wasitter_test

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	wasitter "github.com/zema1/wasitter"
)

var benchmarkJSON = []byte(`{
  "name": "wasitter",
  "enabled": true,
  "version": 1,
  "items": [1, 2, 3, 5, 8, 13, 21],
  "nested": {"null": null, "text": "hello", "unicode": "树🌳"}
}`)

func benchmarkParser(b *testing.B) (*wasitter.Parser, *wasitter.Runtime) {
	b.Helper()
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		b.Fatalf("NewJSONParser: %v", err)
	}
	b.Cleanup(func() {
		_ = p.Close()
		_ = rt.Close()
	})
	return p, rt
}

func BenchmarkJSONParse(b *testing.B) {
	p, _ := benchmarkParser(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkJSON)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree, err := p.Parse(benchmarkJSON, nil)
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}
		_ = tree.RootNode().Type()
		if err := tree.Close(); err != nil {
			b.Fatalf("Tree.Close: %v", err)
		}
	}
}

func BenchmarkJSONParseReuseTree(b *testing.B) {
	p, _ := benchmarkParser(b)
	first, err := p.Parse(benchmarkJSON, nil)
	if err != nil {
		b.Fatalf("initial Parse: %v", err)
	}
	b.Cleanup(func() { _ = first.Close() })
	// Keep the edit inside a scalar so the old tree can be reused while
	// retaining a realistic incremental-parse workload.
	old := append([]byte(nil), benchmarkJSON...)
	needle := []byte("null")
	index := bytes.Index(old, needle)
	if index < 0 {
		b.Fatal("benchmark input does not contain edit target")
	}
	newSource := append([]byte(nil), old[:index]...)
	newSource = append(newSource, []byte("false")...)
	newSource = append(newSource, old[index+len(needle):]...)
	start := uint32(index)
	edit := wasitter.InputEdit{
		StartByte:   start,
		OldEndByte:  start + uint32(len(needle)),
		NewEndByte:  start + uint32(len("false")),
		StartPoint:  pointAt(old, index),
		OldEndPoint: pointAt(old, index+len(needle)),
		NewEndPoint: pointAt(newSource, index+len("false")),
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(newSource)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		oldTree := first.Copy()
		if oldTree == nil {
			b.Fatal("Tree.Copy returned nil")
		}
		if err := oldTree.Edit(edit); err != nil {
			_ = oldTree.Close()
			b.Fatalf("Tree.Edit: %v", err)
		}
		tree, err := p.Parse(newSource, oldTree)
		_ = oldTree.Close()
		if err != nil {
			b.Fatalf("incremental Parse: %v", err)
		}
		if err := tree.Close(); err != nil {
			b.Fatalf("Tree.Close: %v", err)
		}
	}
}

func pointAt(source []byte, offset int) wasitter.Point {
	if offset < 0 {
		offset = 0
	}
	if offset > len(source) {
		offset = len(source)
	}
	var p wasitter.Point
	for _, c := range source[:offset] {
		if c == '\n' {
			p.Row++
			p.Column = 0
		} else {
			p.Column++
		}
	}
	return p
}

func BenchmarkJSONNodeTraversal(b *testing.B) {
	p, _ := benchmarkParser(b)
	tree, err := p.Parse(benchmarkJSON, nil)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	b.Cleanup(func() { _ = tree.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		stack := []wasitter.Node{tree.RootNode()}
		for len(stack) > 0 {
			last := len(stack) - 1
			n := stack[last]
			stack = stack[:last]
			if n.IsNull() {
				continue
			}
			count++
			for j := n.ChildCount() - 1; j >= 0; j-- {
				stack = append(stack, n.Child(j))
			}
		}
		b.ReportMetric(float64(count), "nodes/op")
	}
}

func BenchmarkJSONParseParallel(b *testing.B) {
	data := append([]byte(nil), benchmarkJSON...)
	// A compiled Tree-sitter/WASM instance owns a sizeable linear memory.  Do
	// not create one in every RunParallel callback before that callback has
	// successfully claimed an iteration: at small benchtimes the benchmark may
	// otherwise construct GOMAXPROCS runtimes for a single measured operation.
	// Four independent instances are enough to exercise parallel execution
	// without making the benchmark's setup memory scale unboundedly on large
	// builders.
	previousProcs := runtime.GOMAXPROCS(0)
	workers := previousProcs
	if workers > 4 {
		workers = 4
		runtime.GOMAXPROCS(workers)
		b.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })
	}
	type parserRuntime struct {
		parser  *wasitter.Parser
		runtime *wasitter.Runtime
	}
	pool := make(chan parserRuntime, workers)
	instances := make([]parserRuntime, 0, workers)
	for i := 0; i < workers; i++ {
		p, rt, err := wasitter.NewJSONParser(context.Background())
		if err != nil {
			b.Fatalf("NewJSONParser: %v", err)
		}
		instance := parserRuntime{parser: p, runtime: rt}
		instances = append(instances, instance)
		pool <- instance
	}
	b.Cleanup(func() {
		for _, instance := range instances {
			_ = instance.parser.Close()
			_ = instance.runtime.Close()
		}
	})
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.SetParallelism(1)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		instance := <-pool
		defer func() { pool <- instance }()
		for pb.Next() {
			tree, err := instance.parser.Parse(data, nil)
			if err != nil {
				b.Errorf("Parse: %v", err)
				return
			}
			_ = tree.RootNode().Type()
			_ = tree.Close()
		}
	})
}
