package wasitter

import (
	"context"
	runtimepkg "runtime"
	"sync"
	"sync/atomic"
)

// LookaheadIterator enumerates the grammar symbols that are valid in a
// particular parser state.  The iterator is backed by Tree-sitter's native
// TSLookaheadIterator in the WASM guest; keeping it opaque preserves the
// pointer-free ABI while retaining the semantics of the upstream binding.
//
// A LookaheadIterator is mutable and should not be copied after construction.
// Its methods are serialized so accidental concurrent use remains safe.
type LookaheadIterator struct {
	runtime  *Runtime
	language *Language
	handle   atomic.Uint32
	closed   atomic.Bool
	mu       sync.Mutex
}

// NewLookaheadIterator creates an iterator for language and parse state.  An
// invalid state, a closed language, or a module without the optional
// lookahead ABI is reported as an error.
func NewLookaheadIterator(language *Language, state uint16) (*LookaheadIterator, error) {
	if language == nil {
		return nil, ErrNoLanguage
	}
	if err := language.ensureOpen(); err != nil {
		return nil, err
	}
	r := language.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := r.function(
		"tsw_lookahead_iterator_new",
		"wasitter_lookahead_iterator_new",
		"ts_lookahead_iterator_new",
		"lookahead_iterator_new",
	)
	if err != nil {
		return nil, err
	}
	result, callErr := fn.Call(r.Context(), uint64(language.handle), uint64(state))
	if callErr != nil {
		return nil, &ABIError{Function: name, Message: callErr.Error(), Cause: callErr}
	}
	if len(result) == 0 {
		return nil, &ABIError{Function: name, Message: "invalid lookahead iterator state"}
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 lookahead iterator handle"}
	}
	if handle == 0 {
		return nil, &ABIError{Function: name, Message: "invalid lookahead iterator state"}
	}
	it := &LookaheadIterator{runtime: r, language: language.clone()}
	it.handle.Store(handle)
	runtimepkg.SetFinalizer(it, func(iterator *LookaheadIterator) { _ = iterator.Close() })
	return it, nil
}

// LookaheadIterator is the language-oriented convenience constructor.  It
// mirrors go-tree-sitter's Language.LookaheadIterator and returns nil when the
// state or optional ABI is unavailable; callers needing diagnostics can use
// NewLookaheadIterator.
func (l *Language) LookaheadIterator(state uint16) *LookaheadIterator {
	it, err := NewLookaheadIterator(l, state)
	if err != nil {
		return nil
	}
	return it
}

// Handle returns the opaque guest iterator handle, or zero for a closed
// iterator.
func (it *LookaheadIterator) Handle() uint32 {
	if it == nil {
		return 0
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.closed.Load() || it.runtime == nil || it.runtime.closedState() {
		return 0
	}
	return it.handle.Load()
}

func (it *LookaheadIterator) ensureOpen() error {
	if it == nil || it.closed.Load() || it.handle.Load() == 0 {
		return ErrClosed
	}
	if it.runtime == nil {
		return ErrNoRuntime
	}
	return it.runtime.ensureOpen()
}

// Close releases the guest iterator.  It is idempotent.
func (it *LookaheadIterator) Close() error {
	if it == nil || it.closed.Swap(true) {
		return nil
	}
	runtimepkg.SetFinalizer(it, nil)
	it.mu.Lock()
	handle := it.handle.Swap(0)
	r := it.runtime
	it.mu.Unlock()
	if handle == 0 || r == nil || r.closedState() {
		return nil
	}
	_, _, err := r.call(context.Background(), []string{
		"tsw_lookahead_iterator_delete",
		"wasitter_lookahead_iterator_delete",
		"ts_lookahead_iterator_delete",
		"lookahead_iterator_delete",
	}, uint64(handle))
	if isUnsupported(err) || err == ErrClosed {
		return nil
	}
	return err
}

// Language returns the grammar currently associated with the iterator.
func (it *LookaheadIterator) Language() *Language {
	l, _ := it.LanguageE()
	return l
}

// LanguageE is the error-returning form of Language.
func (it *LookaheadIterator) LanguageE() (*Language, error) {
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	result, name, err := it.runtime.call(it.runtime.Context(), []string{
		"tsw_lookahead_iterator_language",
		"wasitter_lookahead_iterator_language",
		"ts_lookahead_iterator_language",
		"lookahead_iterator_language",
	}, uint64(it.handle.Load()))
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	handle, ok := checkedU32(result[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "returned a non-wasm32 language handle"}
	}
	if handle == 0 {
		return nil, nil
	}
	// Preserve the language wrapper's identity metadata when the guest reports
	// the same grammar.  Returning a synthetic wrapper tagged with the accessor
	// export (for example `tsw_lookahead_iterator_language`) makes otherwise
	// equal languages fail reflect-based comparisons used by upstream callers;
	// the grammar handle is immutable and is the authoritative identity.
	associated := it.language
	if associated != nil && associated.handle == handle && associated.runtime == it.runtime {
		return associated.clone(), nil
	}
	return &Language{runtime: it.runtime, handle: handle, export: name}, nil
}

// Symbol returns the current grammar symbol.
func (it *LookaheadIterator) Symbol() uint16 {
	v, _ := it.SymbolE()
	return v
}

// SymbolE is the error-returning form of Symbol.
func (it *LookaheadIterator) SymbolE() (uint16, error) {
	result, err := it.call([]string{
		"tsw_lookahead_iterator_current_symbol",
		"wasitter_lookahead_iterator_current_symbol",
		"ts_lookahead_iterator_current_symbol",
		"lookahead_iterator_current_symbol",
	})
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU16(result[0])
	if !ok {
		return 0, &ABIError{Function: "lookahead_iterator_current_symbol", Message: "returned a value outside uint16 range"}
	}
	return value, nil
}

// SymbolName returns the display name of the current grammar symbol.
func (it *LookaheadIterator) SymbolName() string {
	s, _ := it.SymbolNameE()
	return s
}

// SymbolNameE is the error-returning form of SymbolName.
func (it *LookaheadIterator) SymbolNameE() (string, error) {
	if err := it.ensureOpen(); err != nil {
		return "", err
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if err := it.ensureOpen(); err != nil {
		return "", err
	}
	r := it.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	result, name, err := r.callLocked(r.Context(), []string{
		"tsw_lookahead_iterator_current_symbol_name",
		"wasitter_lookahead_iterator_current_symbol_name",
		"ts_lookahead_iterator_current_symbol_name",
		"lookahead_iterator_current_symbol_name",
	}, uint64(it.handle.Load()))
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", nil
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: name, Message: "returned a non-wasm32 symbol-name pointer"}
	}
	if ptr == 0 {
		return "", nil
	}
	s, readErr := r.readCString(ptr)
	if readErr != nil {
		return "", &ABIError{Function: name, Message: readErr.Error(), Cause: readErr}
	}
	return s, nil
}

// Next advances the iterator and reports whether another symbol exists.
func (it *LookaheadIterator) Next() bool {
	ok, _ := it.NextE()
	return ok
}

// NextE is the error-returning form of Next.
func (it *LookaheadIterator) NextE() (bool, error) {
	result, err := it.call([]string{
		"tsw_lookahead_iterator_next",
		"wasitter_lookahead_iterator_next",
		"ts_lookahead_iterator_next",
		"lookahead_iterator_next",
	})
	if err != nil {
		return false, err
	}
	if len(result) == 0 {
		return false, ErrInvalidHandle
	}
	return result[0] != 0, nil
}

// ResetState resets the iterator to another parse state.
func (it *LookaheadIterator) ResetState(state uint16) bool {
	ok, _ := it.ResetStateE(state)
	return ok
}

// ResetStateE is the error-returning form of ResetState.
func (it *LookaheadIterator) ResetStateE(state uint16) (bool, error) {
	result, err := it.call([]string{
		"tsw_lookahead_iterator_reset_state",
		"wasitter_lookahead_iterator_reset_state",
		"ts_lookahead_iterator_reset_state",
		"lookahead_iterator_reset_state",
	}, uint64(state))
	if err != nil {
		return false, err
	}
	if len(result) == 0 {
		return false, ErrInvalidHandle
	}
	return result[0] != 0, nil
}

// Reset changes both the language and parse state.  It returns false when the
// guest rejects the language/state pair.
func (it *LookaheadIterator) Reset(language *Language, state uint16) bool {
	ok, _ := it.ResetE(language, state)
	return ok
}

// ResetE is the error-returning form of Reset.
func (it *LookaheadIterator) ResetE(language *Language, state uint16) (bool, error) {
	if language == nil {
		return false, ErrNoLanguage
	}
	if err := language.ensureOpen(); err != nil {
		return false, err
	}
	if it == nil || it.runtime != language.runtime {
		return false, ErrNoRuntime
	}
	result, err := it.call([]string{
		"tsw_lookahead_iterator_reset",
		"wasitter_lookahead_iterator_reset",
		"ts_lookahead_iterator_reset",
		"lookahead_iterator_reset",
	}, uint64(language.handle), uint64(state))
	if err != nil {
		return false, err
	}
	if len(result) == 0 {
		return false, ErrInvalidHandle
	}
	ok := result[0] != 0
	if ok {
		it.mu.Lock()
		it.language = language.clone()
		it.mu.Unlock()
	}
	return ok, nil
}

// Iter consumes the remaining iterator and returns symbol ids in guest order.
func (it *LookaheadIterator) Iter() []uint16 {
	values, _ := it.IterE()
	return values
}

// IterE is the error-returning form of Iter.
func (it *LookaheadIterator) IterE() ([]uint16, error) {
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	result := make([]uint16, 0)
	for {
		ok, err := it.nextLocked()
		if err != nil {
			return nil, err
		}
		if !ok {
			return result, nil
		}
		symbol, err := it.symbolLocked()
		if err != nil {
			return nil, err
		}
		result = append(result, symbol)
	}
}

// IterNames consumes the remaining iterator and returns symbol names in guest
// order.
func (it *LookaheadIterator) IterNames() []string {
	values, _ := it.IterNamesE()
	return values
}

// IterNamesE is the error-returning form of IterNames.
func (it *LookaheadIterator) IterNamesE() ([]string, error) {
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for {
		ok, err := it.nextLocked()
		if err != nil {
			return nil, err
		}
		if !ok {
			return result, nil
		}
		name, err := it.symbolNameLocked()
		if err != nil {
			return nil, err
		}
		result = append(result, name)
	}
}

func (it *LookaheadIterator) call(names []string, args ...uint64) ([]uint64, error) {
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if err := it.ensureOpen(); err != nil {
		return nil, err
	}
	all := make([]uint64, 0, len(args)+1)
	all = append(all, uint64(it.handle.Load()))
	all = append(all, args...)
	result, _, err := it.runtime.call(it.runtime.Context(), names, all...)
	return result, err
}

func (it *LookaheadIterator) nextLocked() (bool, error) {
	if err := it.ensureOpen(); err != nil {
		return false, err
	}
	result, _, err := it.runtime.call(it.runtime.Context(), []string{
		"tsw_lookahead_iterator_next",
		"wasitter_lookahead_iterator_next",
		"ts_lookahead_iterator_next",
		"lookahead_iterator_next",
	}, uint64(it.handle.Load()))
	if err != nil {
		return false, err
	}
	if len(result) == 0 {
		return false, ErrInvalidHandle
	}
	return result[0] != 0, nil
}

func (it *LookaheadIterator) symbolLocked() (uint16, error) {
	result, _, err := it.runtime.call(it.runtime.Context(), []string{
		"tsw_lookahead_iterator_current_symbol",
		"wasitter_lookahead_iterator_current_symbol",
		"ts_lookahead_iterator_current_symbol",
		"lookahead_iterator_current_symbol",
	}, uint64(it.handle.Load()))
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, ErrInvalidHandle
	}
	value, ok := checkedU16(result[0])
	if !ok {
		return 0, &ABIError{Function: "lookahead_iterator_current_symbol", Message: "returned a value outside uint16 range"}
	}
	return value, nil
}

func (it *LookaheadIterator) symbolNameLocked() (string, error) {
	r := it.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	result, name, err := r.callLocked(r.Context(), []string{
		"tsw_lookahead_iterator_current_symbol_name",
		"wasitter_lookahead_iterator_current_symbol_name",
		"ts_lookahead_iterator_current_symbol_name",
		"lookahead_iterator_current_symbol_name",
	}, uint64(it.handle.Load()))
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", nil
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: name, Message: "returned a non-wasm32 symbol-name pointer"}
	}
	if ptr == 0 {
		return "", nil
	}
	s, readErr := r.readCString(ptr)
	if readErr != nil {
		return "", &ABIError{Function: name, Message: readErr.Error(), Cause: readErr}
	}
	return s, nil
}
