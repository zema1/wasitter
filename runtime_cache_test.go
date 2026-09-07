package wasitter

import (
	"context"
	"errors"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// Count actual wazero export lookups: repeatedly obtaining a new api.Function
// allocates backend execution state even when the guest allocation is tiny.
type countingExportModule struct {
	api.Module
	aliases        map[string]string
	lookups        map[string]int
	freeCalls      int
	freeContextErr error
}

func (m *countingExportModule) ExportedFunction(name string) api.Function {
	m.lookups[name]++
	export := name
	if alias := m.aliases[name]; alias != "" {
		export = alias
	}
	fn := m.Module.ExportedFunction(export)
	if name == "tsw_free" && fn != nil {
		return &countingFreeFunction{Function: fn, module: m}
	}
	return fn
}

type countingFreeFunction struct {
	api.Function
	module *countingExportModule
}

func (fn *countingFreeFunction) Call(ctx context.Context, args ...uint64) ([]uint64, error) {
	fn.module.freeCalls++
	fn.module.freeContextErr = ctx.Err()
	return fn.Function.Call(ctx, args...)
}

func TestAllocatorFunctionsReusedAndCleanupAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := NewRuntime(ctx, BuiltinJSONWASM())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mod := &countingExportModule{Module: rt.mod, lookups: make(map[string]int)}
	rt.mod = mod
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var last uint32
	for i := 0; i < 64; i++ {
		ptr, err := rt.allocLocked(32)
		if err != nil {
			t.Fatal(err)
		}
		if !rt.mod.Memory().WriteByte(ptr, byte(i)) {
			t.Fatal("allocation outside guest memory")
		}
		if i == 63 {
			last = ptr
			break
		}
		rt.freeLocked(ptr)
	}
	// Free must still call the cached function with a cleanup context.
	cancel()
	rt.freeLocked(last)
	if mod.freeCalls != 64 || mod.freeContextErr != nil {
		t.Fatalf("free calls=%d, last context error=%v", mod.freeCalls, mod.freeContextErr)
	}
	if len(rt.allocSizes) != 0 {
		t.Fatalf("unreleased host allocations: %v", rt.allocSizes)
	}
	for _, name := range []string{"tsw_alloc", "tsw_free"} {
		if got := mod.lookups[name]; got != 1 {
			t.Errorf("%s resolved %d times, want once", name, got)
		}
	}

}

func TestCachedAllocatorSkipsIncompatibleAliases(t *testing.T) {
	rt, err := NewRuntime(context.Background(), BuiltinJSONWASM())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mod := &countingExportModule{Module: rt.mod, lookups: make(map[string]int), aliases: map[string]string{
		"tsw_alloc": "tsw_abi_version", // Wrong parameter count; must not become a bump allocator.
		"malloc":    "tsw_alloc",
		"tsw_free":  "tsw_language_field_id_for_name", // Three parameters only allowed for bindgen free.
		"free":      "tsw_free",
	}}
	rt.mod = mod
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i := 0; i < 8; i++ {
		ptr, err := rt.allocLocked(24)
		if err != nil {
			t.Fatal(err)
		}
		rt.freeLocked(ptr)
	}
	for _, name := range []string{"tsw_alloc", "malloc", "tsw_free", "free"} {
		if mod.lookups[name] != 1 {
			t.Errorf("%s resolved %d times", name, mod.lookups[name])
		}
	}
	// A separate runtime with only incompatible aliases must still reject
	// allocation; the function cache must not bypass signature checks.
	bad, err := NewRuntime(context.Background(), BuiltinJSONWASM())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	bad.mod = &countingExportModule{Module: bad.mod, lookups: make(map[string]int), aliases: map[string]string{
		"tsw_alloc": "tsw_abi_version", "wasitter_alloc": "tsw_abi_version",
		"malloc": "tsw_abi_version", "__wbindgen_malloc": "tsw_abi_version",
	}}
	bad.mu.Lock()
	defer bad.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := bad.allocLocked(24); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported allocator: %v", err)
		}
	}
}
