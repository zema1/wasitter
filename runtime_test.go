package wasitter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestBuiltinJSONWASMIsCopiedAndHasValidMagic(t *testing.T) {
	a := BuiltinJSONWASM()
	if len(a) < 8 {
		t.Fatalf("BuiltinJSONWASM length = %d, want a WebAssembly header", len(a))
	}
	if string(a[:4]) != "\x00asm" {
		t.Fatalf("BuiltinJSONWASM magic = %q, want \\x00asm", a[:4])
	}
	original := append([]byte(nil), a...)
	a[0] ^= 0xff
	b := BuiltinJSONWASM()
	if string(b) != string(original) {
		t.Fatal("BuiltinJSONWASM returned storage that can be mutated by callers")
	}
}

func TestBuiltinJSONWASMExportsRequiredABI(t *testing.T) {
	rt, err := NewRuntime(context.Background(), BuiltinJSONWASM())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close()
	for _, name := range []string{
		"tsw_abi_version",
		"tsw_alloc",
		"tsw_free",
		"tsw_language",
		"tsw_parser_new",
		"tsw_parser_set_language",
		"tsw_parser_parse",
		"tsw_tree_delete",
		"tsw_tree_root",
		"tsw_node_type",
		"tsw_node_child",
	} {
		if fn := rt.Module().ExportedFunction(name); fn == nil {
			t.Errorf("required export %q is missing", name)
		}
	}
	fn := rt.Module().ExportedFunction("tsw_abi_version")
	result, err := fn.Call(context.Background())
	if err != nil {
		t.Fatalf("tsw_abi_version: %v", err)
	}
	const wantABIVersion = 1
	if len(result) != 1 || uint32(result[0]) != wantABIVersion {
		t.Fatalf("tsw_abi_version = %#v, want %d", result, wantABIVersion)
	}
	if got := rt.ABIVersion(); got != wantABIVersion {
		t.Fatalf("Runtime.ABIVersion() = %d, want %d", got, wantABIVersion)
	}
	if got, err := rt.ABIVersionE(); err != nil || got != wantABIVersion {
		t.Fatalf("Runtime.ABIVersionE() = %d, %v; want %d", got, err, wantABIVersion)
	}
}

func TestRuntimeRejectsMalformedWASM(t *testing.T) {
	if _, err := NewRuntime(context.Background(), []byte("not a wasm module")); err == nil {
		t.Fatal("NewRuntime accepted malformed WebAssembly")
	}
}

func TestRuntimeFromFileAndCloseAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "json.wasm")
	if err := os.WriteFile(path, BuiltinJSONWASM(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rt, err := NewRuntimeFromFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewRuntimeFromFile: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := rt.LoadLanguage(""); !errors.Is(err, ErrClosed) {
		t.Errorf("LoadLanguage after Close = %v, want ErrClosed", err)
	}
}

func TestRuntimeNilContextIsAccepted(t *testing.T) {
	rt, err := NewRuntime(nil, BuiltinJSONWASM())
	if err != nil {
		t.Fatalf("NewRuntime(nil context): %v", err)
	}
	if rt.Context() == nil {
		t.Fatal("Runtime.Context returned nil")
	}
	_ = rt.Close()
}

func TestLanguageAliasesAndMetadataMethods(t *testing.T) {
	rt, err := NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewJSONRuntime: %v", err)
	}
	defer rt.Close()
	byName, err := rt.Language("json")
	if err != nil {
		t.Fatalf("Runtime.Language(json): %v", err)
	}
	defer byName.Close()
	generic, err := NewLanguage(rt)
	if err != nil {
		t.Fatalf("NewLanguage(generic): %v", err)
	}
	defer generic.Close()
	if byName.Handle() == 0 || generic.Handle() == 0 {
		t.Fatalf("language handles = %d/%d", byName.Handle(), generic.Handle())
	}
	if byName.ExportName() == "" || generic.ExportName() == "" {
		t.Errorf("export names = %q/%q", byName.ExportName(), generic.ExportName())
	}
	if byName.Name() != "json" || generic.Name() != "json" {
		t.Errorf("language names = %q/%q", byName.Name(), generic.Name())
	}
	if byName.ABIVersion() != byName.ABIVersion() || byName.Version() != byName.ABIVersion() {
		t.Errorf("ABI aliases disagree: %d/%d/%d", byName.ABIVersion(), byName.Version(), byName.ABIVersion())
	}
	if byName.StateCount() == 0 || byName.SymbolCount() == 0 || byName.FieldCount() == 0 {
		t.Errorf("metadata counts = states %d symbols %d fields %d", byName.StateCount(), byName.SymbolCount(), byName.FieldCount())
	}
	if name, err := byName.NameE(); err != nil || name != "json" {
		t.Errorf("NameE() = %q, %v", name, err)
	}
	if version, err := byName.ABIVersionE(); err != nil || version == 0 {
		t.Errorf("ABIVersionE() = %d, %v", version, err)
	}
	if id := byName.SymbolForName("number"); id == 0 || byName.SymbolName(id) != "number" {
		t.Errorf("symbol round trip failed for number (id=%d)", id)
	}
	if id, err := byName.SymbolForNameE("number"); err != nil || id == 0 {
		t.Errorf("SymbolForNameE(number) = %d, %v", id, err)
	}
	if name, err := byName.SymbolNameE(byName.SymbolForName("number")); err != nil || name != "number" {
		t.Errorf("SymbolNameE(number) = %q, %v", name, err)
	}
	if _, err := byName.FieldNameForIDE(0); err != nil {
		t.Errorf("FieldNameForIDE(0): %v", err)
	}
}

func TestRuntimeOptionsInterpreterAndConfigure(t *testing.T) {
	configured := false
	rt, err := NewRuntimeWithOptions(context.Background(), BuiltinJSONWASM(), RuntimeOptions{
		RuntimeConfig: wazero.NewRuntimeConfigInterpreter(),
		Configure: func(w wazero.Runtime) error {
			configured = w != nil
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuntimeWithOptions(interpreter): %v", err)
	}
	if !configured {
		t.Error("RuntimeOptions.Configure was not called")
	}
	if _, err := NewLanguage(rt); err != nil {
		t.Errorf("interpreter runtime language load: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}
}

func TestRuntimeRejectsModuleWithoutMemory(t *testing.T) {
	ctx := context.Background()
	empty := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	if rt, err := NewRuntime(ctx, empty); rt != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewRuntime without memory = %v, %v", rt, err)
	}
	host := wazero.NewRuntime(ctx)
	defer host.Close(ctx)
	module, err := host.Instantiate(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	if rt, err := NewRuntimeFromModule(ctx, module); rt != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewRuntimeFromModule without memory = %v, %v", rt, err)
	}
	if module.IsClosed() {
		t.Fatal("failed wrapper closed caller's module")
	}
}
