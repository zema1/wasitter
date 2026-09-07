package wasitter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// benchmarkWASM returns the checked-in fixture when one is available. During
// development the compiler output lives under .tmp; keeping that fallback
// makes the benchmark useful before the generated asset is promoted to its
// final location.
func benchmarkWASM(b *testing.B) []byte {
	b.Helper()
	if data := BuiltinJSONWASM(); len(data) != 0 {
		return data
	}
	candidates := []string{
		filepath.Join("internal", "wasm", "assets", "wasitter-json.wasm"),
		filepath.Join("testdata", "json.wasm"),
		filepath.Join(".tmp", "json.wasm"),
		filepath.Join(".tmp", "json-small.wasm"),
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
			return data
		}
	}
	b.Skip("JSON WebAssembly fixture is not available")
	return nil
}

func BenchmarkRuntimeInstantiate(b *testing.B) {
	wasm := benchmarkWASM(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(wasm)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt, err := NewRuntime(ctx, wasm)
		if err != nil {
			b.Fatalf("NewRuntime: %v", err)
		}
		if err := rt.Close(); err != nil {
			b.Fatalf("Runtime.Close: %v", err)
		}
	}
}

func BenchmarkRuntimeCompileInstantiateParallel(b *testing.B) {
	wasm := benchmarkWASM(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(wasm)))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rt, err := NewRuntime(ctx, wasm)
			if err != nil {
				b.Errorf("NewRuntime: %v", err)
				return
			}
			if err := rt.Close(); err != nil {
				b.Errorf("Runtime.Close: %v", err)
				return
			}
		}
	})
}
