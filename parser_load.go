package wasitter

import (
	"context"
	"os"
)

// NewParserFromFile loads a wasitter WASM module from path and creates a parser
// configured with the grammar exported by that module. The filename does not
// select the grammar.
//
// Like NewJSONParser, it returns the runtime so callers control its lifetime.
// Close trees and the parser before closing the runtime. On failure, resources
// created by this constructor are released and both returned pointers are nil.
func NewParserFromFile(ctx context.Context, path string) (*Parser, *Runtime, error) {
	wasm, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return NewParserFromWASM(ctx, wasm)
}

// NewParserFromWASM creates a parser configured with the grammar exported by a
// wasitter WASM module. It accepts bytes read by the caller or embedded with
// go:embed. For modules containing multiple languages, or custom runtime
// options, use NewRuntimeWithOptions, Runtime.LoadLanguage, and
// NewParserWithRuntime instead.
//
// Close trees and the parser before closing the returned runtime. On failure,
// all resources created here are released and both returned pointers are nil.
func NewParserFromWASM(ctx context.Context, wasm []byte) (*Parser, *Runtime, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	r, err := NewRuntime(ctx, wasm)
	if err != nil {
		return nil, nil, err
	}
	return newParserWithOwnedRuntime(r, "")
}

// newParserWithOwnedRuntime takes ownership of a newly created runtime and
// closes it on failure. An empty language selects the module's default.
func newParserWithOwnedRuntime(r *Runtime, name string) (*Parser, *Runtime, error) {
	grammar, err := r.LoadLanguage(name)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	p, err := NewParserWithRuntime(r)
	if err != nil {
		_ = grammar.Close()
		_ = r.Close()
		return nil, nil, err
	}
	if err := p.SetLanguage(grammar); err != nil {
		_ = grammar.Close()
		_ = p.Close()
		_ = r.Close()
		return nil, nil, err
	}
	_ = grammar.Close()
	return p, r, nil
}
