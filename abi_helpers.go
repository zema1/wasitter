package wasitter

import (
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// wasmIntegerType reports whether a wasm value can safely carry one of the
// pointer/length/status values used by the wasitter ABI.  The bridge uses
// i32 for wasm32 pointers, while a few compatibility modules expose i64
// results; both are representable at the wazero call boundary.
func wasmIntegerType(value api.ValueType) bool {
	return value == api.ValueTypeI32 || value == api.ValueTypeI64
}

// validateIntegerSignature checks an exported function before a caller
// invokes it.  Wazero turns an arity/type mismatch into a trap, which is
// recoverable as an error but is needlessly opaque and, for compatibility
// aliases, can select an unrelated export.  Keeping this check in one place
// makes malformed custom modules deterministic at the public API boundary.
// A negative parameter/result count means "any count"; all values that are
// present must still be integer-shaped when integerParams/integerResults are
// true.
func validateIntegerSignature(fn api.Function, parameterCount, resultCount int, integerParams, integerResults bool) error {
	if fn == nil {
		return fmt.Errorf("%w: missing export", ErrUnsupported)
	}
	definition := fn.Definition()
	params := definition.ParamTypes()
	results := definition.ResultTypes()
	if parameterCount >= 0 && len(params) != parameterCount {
		return fmt.Errorf("%w: unsupported function arity (got %d parameters, want %d)", ErrUnsupported, len(params), parameterCount)
	}
	if resultCount >= 0 && len(results) != resultCount {
		return fmt.Errorf("%w: unsupported function result count (got %d results, want %d)", ErrUnsupported, len(results), resultCount)
	}
	if integerParams {
		for _, typ := range params {
			if !wasmIntegerType(typ) {
				return fmt.Errorf("%w: function has a non-integer parameter", ErrUnsupported)
			}
		}
	}
	if integerResults {
		for _, typ := range results {
			if !wasmIntegerType(typ) {
				return fmt.Errorf("%w: function has a non-integer result", ErrUnsupported)
			}
		}
	}
	return nil
}

// lookupIntegerFunctionLocked returns the first alias whose signature is
// compatible with the requested shape. The caller must hold Runtime.mu. A
// malformed export is skipped so a later, valid compatibility alias can be
// used. If exports exist but all are malformed, the returned error describes
// the incompatibility; if none exist it is ErrUnsupported.
func lookupIntegerFunctionLocked(r *Runtime, names []string, parameterCount, resultCount int, integerParams, integerResults bool) (api.Function, string, error) {
	return lookupIntegerFunctionShapesLocked(r, names, []int{parameterCount}, []int{resultCount}, integerParams, integerResults)
}

// lookupIntegerFunctionAllowedLocked is the arity-set variant used by
// compatibility exports that legitimately support more than one shape (for
// example a language ABI-version accessor accepting either zero or one
// language handle). The caller must hold Runtime.mu.
func lookupIntegerFunctionAllowedLocked(r *Runtime, names []string, parameterCounts []int, resultCount int, integerParams, integerResults bool) (api.Function, string, error) {
	return lookupIntegerFunctionShapesLocked(r, names, parameterCounts, []int{resultCount}, integerParams, integerResults)
}

// lookupIntegerFunctionShapesLocked is the fully general shape variant. A
// negative entry in either count slice means any count for that dimension;
// all other entries are matched exactly. The caller must hold Runtime.mu.
func lookupIntegerFunctionShapesLocked(r *Runtime, names []string, parameterCounts, resultCounts []int, integerParams, integerResults bool) (api.Function, string, error) {
	if r == nil {
		return nil, "", ErrNoRuntime
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("%w: missing export name", ErrUnsupported)
	}
	var malformed error
	for _, name := range names {
		fn := r.funcs[name]
		if fn == nil && r.mod != nil {
			fn = r.mod.ExportedFunction(name)
			if fn != nil {
				r.funcs[name] = fn
			}
		}
		if fn == nil {
			continue
		}
		validArity := false
		var signatureErr error
		for _, parameterCount := range parameterCounts {
			for _, resultCount := range resultCounts {
				if err := validateIntegerSignature(fn, parameterCount, resultCount, integerParams, integerResults); err == nil {
					validArity = true
					break
				} else {
					signatureErr = err
				}
			}
			if validArity {
				break
			}
		}
		if !validArity {
			if signatureErr == nil {
				signatureErr = fmt.Errorf("%w: unsupported function signature", ErrUnsupported)
			}
			malformed = fmt.Errorf("%w: %s: %v", ErrUnsupported, name, signatureErr)
			continue
		}
		return fn, name, nil
	}
	if malformed != nil {
		return nil, names[0], malformed
	}
	return nil, names[0], fmt.Errorf("%w: %s", ErrUnsupported, names[0])
}
