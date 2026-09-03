package sitterwasm

import (
	"context"
	"embed"
)

// The built-in JSON artifact is generated from the C runtime and grammar by
// scripts/build-wasm.sh. Keeping it embedded makes the package immediately
// usable without a compiler or cgo on the target machine.
//
//go:embed internal/wasm/assets/sitterwasm-json.wasm
var builtinAssets embed.FS

// BuiltinJSONWASM returns a copy of the embedded JSON Tree-sitter module.
func BuiltinJSONWASM() []byte {
	b, err := builtinAssets.ReadFile("internal/wasm/assets/sitterwasm-json.wasm")
	if err != nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// NewJSONRuntime creates a Runtime containing the built-in JSON grammar.
func NewJSONRuntime(ctx context.Context) (*Runtime, error) {
	r, err := NewRuntime(ctx, BuiltinJSONWASM())
	if err != nil {
		return nil, err
	}
	if _, err := r.LoadLanguage("json"); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// NewJSONParser is a convenience constructor that returns a parser already
// configured with the built-in JSON grammar.
func NewJSONParser(ctx context.Context) (*Parser, *Runtime, error) {
	r, err := NewJSONRuntime(ctx)
	if err != nil {
		return nil, nil, err
	}
	language, err := r.LoadLanguage("json")
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	p, err := NewParserWithRuntime(r)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	if err := p.SetLanguage(language); err != nil {
		_ = p.Close()
		_ = r.Close()
		return nil, nil, err
	}
	return p, r, nil
}
