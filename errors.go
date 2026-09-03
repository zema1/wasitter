package sitterwasm

import (
	"errors"
	"fmt"
)

// Errors returned by the WASM-backed API.
var (
	ErrClosed        = errors.New("sitterwasm: object is closed")
	ErrNoRuntime     = errors.New("sitterwasm: no WASM runtime configured")
	ErrNoLanguage    = errors.New("sitterwasm: parser has no language")
	ErrUnsupported   = errors.New("sitterwasm: operation is not supported by this module")
	ErrInvalidHandle = errors.New("sitterwasm: invalid WASM handle")
	// ErrOperationLimit is returned when Tree-sitter stops parsing because the
	// parser timeout/operation budget was exhausted. It mirrors the sentinel
	// exposed by the established Go bindings and lets callers distinguish this
	// expected, resumable condition from a malformed ABI call.
	ErrOperationLimit = errors.New("sitterwasm: parser operation limit reached")
)

func isUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }

// ABIError describes an error returned by a shim function.
type ABIError struct {
	Function string
	Message  string
	// Cause retains the original wazero/context error when one exists.  It is
	// intentionally optional so existing ABIError literals remain source
	// compatible; errors.Is/As can still identify cancellation and deadline
	// errors returned by a guest call.
	Cause error
}

func (e *ABIError) Error() string {
	if e == nil {
		return "sitterwasm: ABI call failed"
	}
	if e.Message == "" {
		return "sitterwasm: " + e.Function + " failed"
	}
	return "sitterwasm: " + e.Function + ": " + e.Message
}

// Unwrap exposes the underlying guest-call error to errors.Is/As.
func (e *ABIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// LanguageError reports a grammar/runtime ABI version mismatch.  It mirrors
// the error exposed by the upstream Go binding while retaining the concrete
// version so callers can decide whether to rebuild a grammar or select a
// different runtime.
type LanguageError struct {
	Version uint32
}

func (e *LanguageError) Error() string {
	if e == nil {
		return ""
	}
	if e.Version == 0 {
		return "sitterwasm: language has an incompatible ABI version"
	}
	return fmt.Sprintf("sitterwasm: language ABI version %d is incompatible (supported %d..%d)", e.Version, MIN_COMPATIBLE_LANGUAGE_VERSION, LANGUAGE_VERSION)
}

// IncludedRangesError reports the first range that violates Tree-sitter's
// ordering rules.  Ranges must be ordered by StartByte, must not overlap, and
// each range's EndByte must be greater than or equal to its StartByte.
//
// This mirrors the error returned by the native Go binding and lets callers
// use errors.As to identify the offending entry without parsing an error
// string.
type IncludedRangesError struct {
	Index uint32
}

func (e *IncludedRangesError) Error() string {
	if e == nil {
		return "sitterwasm: invalid included ranges"
	}
	return fmt.Sprintf("sitterwasm: invalid included range at index %d", e.Index)
}
