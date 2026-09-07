package wasitter

import (
	"errors"
	"fmt"
)

// Errors returned by the WASM-backed API.
var (
	ErrClosed        = errors.New("wasitter: object is closed")
	ErrNoRuntime     = errors.New("wasitter: no WASM runtime configured")
	ErrNoLanguage    = errors.New("wasitter: parser has no language")
	ErrUnsupported   = errors.New("wasitter: operation is not supported by this module")
	ErrInvalidHandle = errors.New("wasitter: invalid WASM handle")
	// ErrOperationLimit is returned when Tree-sitter stops parsing because the
	// parser timeout/operation budget was exhausted. It mirrors the sentinel
	// exposed by the established Go bindings and lets callers distinguish this
	// expected, resumable condition from a malformed ABI call.
	ErrOperationLimit = errors.New("wasitter: parser operation limit reached")
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
		return "wasitter: ABI call failed"
	}
	if e.Message == "" {
		return "wasitter: " + e.Function + " failed"
	}
	return "wasitter: " + e.Function + ": " + e.Message
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
		return "wasitter: language has an incompatible ABI version"
	}
	return fmt.Sprintf("wasitter: language ABI version %d is incompatible (supported %d..%d)", e.Version, MinCompatibleLanguageVersion, LanguageVersion)
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
		return "wasitter: invalid included ranges"
	}
	return fmt.Sprintf("wasitter: invalid included range at index %d", e.Index)
}
