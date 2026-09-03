package sitterwasm_test

import (
	"errors"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

// Convenience language accessors must remain total for a nil receiver.  In
// particular, methods that build a guest-handle argument evaluate that field
// before their lower-level helper can perform a lifecycle check.
func TestNilLanguageConvenienceAccessorsDoNotPanic(t *testing.T) {
	var language *sitterwasm.Language
	if got := language.SymbolName(0); got != "" {
		t.Errorf("nil SymbolName = %q, want empty", got)
	}
	if got := language.FieldNameForID(0); got != "" {
		t.Errorf("nil FieldNameForID = %q, want empty", got)
	}
	if got := language.FieldName(0); got != "" {
		t.Errorf("nil FieldName = %q, want empty", got)
	}
	if got := language.NodeKindForId(0); got != "" {
		t.Errorf("nil NodeKindForId = %q, want empty", got)
	}
	if got := language.NodeKindForID(0); got != "" {
		t.Errorf("nil NodeKindForID = %q, want empty", got)
	}
	if language.NodeKindIsNamed(0) || language.NodeKindIsVisible(0) {
		t.Error("nil language reported a named/visible symbol")
	}
	if _, err := language.SymbolNameE(0); !errors.Is(err, sitterwasm.ErrClosed) {
		t.Errorf("nil SymbolNameE error = %v, want ErrClosed", err)
	}
	if _, err := language.FieldNameForIDE(0); !errors.Is(err, sitterwasm.ErrClosed) {
		t.Errorf("nil FieldNameForIDE error = %v, want ErrClosed", err)
	}
}
