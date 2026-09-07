package wasitter_test

import (
	"context"
	"reflect"
	"testing"
	"unicode/utf16"

	wasitter "github.com/zema1/wasitter"
)

func TestNodeUtf16TextUsesUTF8ByteOffsets(t *testing.T) {
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	source := `{"x":"猫🌳"}`
	tree, err := p.Parse([]byte(source), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	value := tree.RootNode().NamedChild(0).NamedChild(0).ChildByFieldName("value")
	want := utf16.Encode([]rune(`"猫🌳"`))
	got := value.Utf16Text(utf16.Encode([]rune(source)))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Utf16Text = %v, want %v", got, want)
	}
}

func TestLanguageWrapperCloseDoesNotInvalidateParser(t *testing.T) {
	p, rt, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	lang := p.Language()
	if lang == nil {
		t.Fatal("Parser.Language returned nil")
	}
	if err := lang.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := p.Parse([]byte(`null`), nil)
	if err != nil {
		t.Fatalf("parser became unusable after closing language wrapper: %v", err)
	}
	defer tree.Close()
	if got := tree.RootNode().NamedChild(0).Type(); got != "null" {
		t.Fatalf("root child type = %q, want null", got)
	}
}
