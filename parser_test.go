package sitterwasm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	sitterwasm "github.com/zema1/sitterwasm"
)

// newJSONParser is shared by the behavioral tests. The bundled fixture is a
// real Tree-sitter JSON grammar; failures to load it are test failures rather
// than skips, so an accidentally stale/incompatible artifact cannot make the
// suite appear green.
func newJSONParser(t *testing.T) (*sitterwasm.Parser, *sitterwasm.Runtime) {
	t.Helper()
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatalf("NewJSONParser: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		_ = rt.Close()
	})
	return p, rt
}

func parseJSON(t *testing.T, source string) (*sitterwasm.Parser, *sitterwasm.Runtime, *sitterwasm.Tree) {
	t.Helper()
	p, rt := newJSONParser(t)
	tree, err := p.Parse([]byte(source), nil)
	if err != nil {
		t.Fatalf("Parse(%q): %v", source, err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return p, rt, tree
}

func TestJSONParseMatchesNativeSExpressions(t *testing.T) {
	for _, tc := range loadJSONCorpus(t) {
		t.Run(tc.Name, func(t *testing.T) {
			_, _, tree := parseJSON(t, tc.Source)
			if got := tree.ToSExpression(); got != tc.Sexp {
				t.Errorf("ToSExpression() = %q, want %q", got, tc.Sexp)
			}
			if got := tree.ToSexp(); got != tc.Sexp {
				t.Errorf("ToSexp() = %q, want %q", got, tc.Sexp)
			}
			if got := tree.Source(); got != tc.Source {
				t.Errorf("Source() = %q, want %q", got, tc.Source)
			}
		})
	}
}

func TestJSONNodeNavigationAndRanges(t *testing.T) {
	source := "[123, false, {\"x\": null}]"
	_, _, tree := parseJSON(t, source)
	root := tree.RootNode()
	if root.IsNull() || !root.IsNamed() {
		t.Fatalf("root = %#v, want a named non-null node", root)
	}
	if root.Type() != "document" || root.Kind() != "document" {
		t.Fatalf("root type/kind = %q/%q, want document/document", root.Type(), root.Kind())
	}
	if got, want := root.StartByte(), uint32(0); got != want {
		t.Errorf("root.StartByte() = %d, want %d", got, want)
	}
	if got, want := root.EndByte(), uint32(len(source)); got != want {
		t.Errorf("root.EndByte() = %d, want %d", got, want)
	}
	if got, want := root.StartPoint(), (sitterwasm.Point{Row: 0, Column: 0}); got != want {
		t.Errorf("root.StartPoint() = %#v, want %#v", got, want)
	}
	if got, want := root.EndPoint(), (sitterwasm.Point{Row: 0, Column: uint32(len(source))}); got != want {
		t.Errorf("root.EndPoint() = %#v, want %#v", got, want)
	}

	array := root.NamedChild(0)
	if array.Type() != "array" {
		t.Fatalf("root.NamedChild(0).Type() = %q, want array", array.Type())
	}
	if got, want := array.NamedChildCount(), 3; got != want {
		t.Errorf("array.NamedChildCount() = %d, want %d", got, want)
	}
	if got, want := array.ChildCount(), 7; got != want {
		t.Errorf("array.ChildCount() = %d, want %d", got, want)
	}

	childKinds := make([]string, 0, array.ChildCount())
	for i := 0; i < array.ChildCount(); i++ {
		child := array.Child(i)
		childKinds = append(childKinds, child.Type())
		if child.Parent().Type() != "array" {
			t.Errorf("child %d parent = %q, want array", i, child.Parent().Type())
		}
	}
	wantKinds := []string{"[", "number", ",", "false", ",", "object", "]"}
	if !equalStrings(childKinds, wantKinds) {
		t.Errorf("child kinds = %#v, want %#v", childKinds, wantKinds)
	}
	if array.Child(-1).Valid() || array.Child(99).Valid() {
		t.Error("out-of-range Child should return an invalid/null node")
	}

	number := array.NamedChild(0)
	if number.Text() != "123" || number.Content() != "123" {
		t.Errorf("number text/content = %q/%q, want 123/123", number.Text(), number.Content())
	}
	if number.StartByte() != 1 || number.EndByte() != 4 {
		t.Errorf("number byte range = [%d,%d), want [1,4)", number.StartByte(), number.EndByte())
	}
	if number.NextSibling().Type() != "," || number.NextNamedSibling().Type() != "false" {
		t.Errorf("sibling navigation returned %q/%q", number.NextSibling().Type(), number.NextNamedSibling().Type())
	}
	if number.PrevSibling().Type() != "[" {
		t.Errorf("first named child's previous sibling = %q, want [", number.PrevSibling().Type())
	}
	if number.PrevNamedSibling().Valid() {
		t.Error("first named child should not have a previous named sibling")
	}

	object := array.NamedChild(2)
	pair := object.NamedChild(0)
	if pair.Type() != "pair" {
		t.Fatalf("object.NamedChild(0).Type() = %q, want pair", pair.Type())
	}
	key, value := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
	if key.Type() != "string" || value.Type() != "null" {
		t.Errorf("pair fields = %q/%q, want string/null", key.Type(), value.Type())
	}
	if key.Text() != `"x"` || value.Text() != "null" {
		t.Errorf("pair field text = %q/%q", key.Text(), value.Text())
	}
	if pair.ChildByFieldName("missing").Valid() {
		t.Error("missing field should return an invalid/null node")
	}
	if root.Parent().Valid() {
		t.Error("root parent should be a null node")
	}
	descendant := root.DescendantForByteRange(number.StartByte(), number.EndByte())
	if descendant.Type() != "number" || descendant.StartByte() != number.StartByte() || descendant.EndByte() != number.EndByte() {
		t.Errorf("DescendantForByteRange = %q [%d,%d), want number [%d,%d)", descendant.Type(), descendant.StartByte(), descendant.EndByte(), number.StartByte(), number.EndByte())
	}
}

func TestJSONMalformedInputPreservesErrorNodes(t *testing.T) {
	_, _, tree := parseJSON(t, `{"a": 1,}`)
	root := tree.RootNode()
	if !root.HasError() {
		t.Fatal("malformed JSON root.HasError() = false, want true")
	}
	if strings.Contains(tree.ToSExpression(), "(ERROR)") == false {
		t.Fatalf("malformed tree = %q, want an ERROR node", tree.ToSExpression())
	}
	// The parser should still expose the valid prefix and preserve exact bytes.
	if got := root.NamedChild(0).NamedChild(0).ChildByFieldName("value").Text(); got != "1" {
		t.Errorf("recovered value text = %q, want 1", got)
	}
}

func TestUTF8OffsetsAndColumnsRemainByteBased(t *testing.T) {
	source := `{"猫":"树🌳"}`
	_, _, tree := parseJSON(t, source)
	object := tree.RootNode().NamedChild(0)
	pair := object.NamedChild(0)
	key, value := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
	if key.Text() != `"猫"` || value.Text() != `"树🌳"` {
		t.Fatalf("key/value text = %q/%q", key.Text(), value.Text())
	}
	keyStart := bytes.Index([]byte(source), []byte(`"猫"`))
	valueStart := bytes.Index([]byte(source), []byte(`"树🌳"`))
	if keyStart < 0 || valueStart < 0 {
		t.Fatal("test source did not contain expected UTF-8 strings")
	}
	assertNodeByteRange := func(name string, node sitterwasm.Node, start int, text string) {
		t.Helper()
		if got, want := node.StartByte(), uint32(start); got != want {
			t.Errorf("%s.StartByte() = %d, want %d", name, got, want)
		}
		if got, want := node.EndByte(), uint32(start+len([]byte(text))); got != want {
			t.Errorf("%s.EndByte() = %d, want %d", name, got, want)
		}
		if got, want := node.StartPoint(), pointAt([]byte(source), start); got != want {
			t.Errorf("%s.StartPoint() = %#v, want %#v", name, got, want)
		}
		if got, want := node.EndPoint(), pointAt([]byte(source), start+len([]byte(text))); got != want {
			t.Errorf("%s.EndPoint() = %#v, want %#v", name, got, want)
		}
	}
	assertNodeByteRange("key", key, keyStart, `"猫"`)
	assertNodeByteRange("value", value, valueStart, `"树🌳"`)
}

func TestTreeCopyAndNodeEquality(t *testing.T) {
	_, _, tree := parseJSON(t, `[1, {"x": 2}]`)
	clone := tree.Copy()
	if clone == nil {
		t.Fatal("Tree.Copy returned nil")
	}
	t.Cleanup(func() { _ = clone.Close() })
	root, copiedRoot := tree.RootNode(), clone.RootNode()
	if !root.Equal(root) || !root.Eq(root) {
		t.Error("a node should compare equal to itself")
	}
	if root.Equal(copiedRoot) {
		t.Log("copied-tree roots compare equal on this runtime")
	}
	if err := clone.Close(); err != nil {
		t.Fatalf("clone.Close: %v", err)
	}
	if root.IsNull() || root.Type() != "document" {
		t.Error("closing a copied tree invalidated the original tree")
	}
}

func TestNodeRangeQueriesAndMetadata(t *testing.T) {
	source := `[1, false]`
	_, _, tree := parseJSON(t, source)
	root := tree.RootNode()
	array := root.NamedChild(0)
	number, literal := array.NamedChild(0), array.NamedChild(1)
	if number.KindID() == 0 || number.Symbol() != number.KindID() {
		t.Errorf("number kind/symbol = %d/%d", number.KindID(), number.Symbol())
	}
	if number.GrammarSymbol() == 0 {
		t.Error("number GrammarSymbol() = 0")
	}
	if !number.IsValid() || number.IsNull() || !number.IsNamed() || number.IsMissing() || number.IsExtra() || number.IsError() || number.HasError() {
		t.Errorf("unexpected number flags: valid=%v null=%v named=%v missing=%v extra=%v error=%v hasError=%v", number.IsValid(), number.IsNull(), number.IsNamed(), number.IsMissing(), number.IsExtra(), number.IsError(), number.HasError())
	}
	if got := number.Range(); got.StartByte != number.StartByte() || got.EndByte != number.EndByte() || got.StartPoint != number.StartPoint() || got.EndPoint != number.EndPoint() {
		t.Errorf("Range() %#v does not match individual accessors", got)
	}
	if start, err := number.StartByteE(); err != nil || start != number.StartByte() {
		t.Errorf("StartByteE() = %d, %v", start, err)
	}
	if end, err := number.EndByteE(); err != nil || end != number.EndByte() {
		t.Errorf("EndByteE() = %d, %v", end, err)
	}
	if p, err := number.StartPointE(); err != nil || p != number.StartPoint() {
		t.Errorf("StartPointE() = %#v, %v", p, err)
	}
	if p, err := number.EndPointE(); err != nil || p != number.EndPoint() {
		t.Errorf("EndPointE() = %#v, %v", p, err)
	}

	if got := array.FirstChildForByte(number.StartByte()).Type(); got != "number" {
		t.Errorf("FirstChildForByte(number) = %q, want number", got)
	}
	if got := array.FirstNamedChildForByte(number.StartByte()).Type(); got != "number" {
		t.Errorf("FirstNamedChildForByte(number) = %q, want number", got)
	}
	if got := root.ChildWithDescendant(literal).Type(); got != "array" {
		t.Errorf("ChildWithDescendant(literal) = %q, want array", got)
	}
	if got := root.DescendantForPointRange(number.StartPoint(), number.EndPoint()).Type(); got != "number" {
		t.Errorf("DescendantForPointRange(number) = %q, want number", got)
	}
	if got := root.NamedDescendantForByteRange(number.StartByte(), number.EndByte()).Type(); got != "number" {
		t.Errorf("NamedDescendantForByteRange(number) = %q, want number", got)
	}
	if got := root.NamedDescendantForPointRange(number.StartPoint(), number.EndPoint()).Type(); got != "number" {
		t.Errorf("NamedDescendantForPointRange(number) = %q, want number", got)
	}
	if got := root.DescendantCount(); got < 1+array.ChildCount() {
		t.Errorf("DescendantCount() = %d, want at least %d", got, 1+array.ChildCount())
	}
	if got := number.FieldNameForChild(0); got != "" {
		t.Errorf("leaf FieldNameForChild(0) = %q, want empty", got)
	}
	if got := fmt.Sprint(number); got != number.ToSExpression() {
		t.Errorf("String() = %q, ToSExpression() = %q", got, number.ToSExpression())
	}
}

func TestParserLifecycleAndOwnershipErrors(t *testing.T) {
	var unbound sitterwasm.Parser
	if _, err := unbound.Parse([]byte("1"), nil); !errors.Is(err, sitterwasm.ErrNoRuntime) {
		t.Errorf("unbound Parse error = %v, want ErrNoRuntime", err)
	}
	if err := unbound.SetLanguage(nil); !errors.Is(err, sitterwasm.ErrNoRuntime) {
		t.Errorf("unbound SetLanguage error = %v, want ErrNoRuntime", err)
	}

	p, rt := newJSONParser(t)
	if err := p.Close(); err != nil {
		t.Fatalf("first Parser.Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Parser.Close should be idempotent: %v", err)
	}
	if _, err := p.Parse([]byte("1"), nil); !errors.Is(err, sitterwasm.ErrClosed) {
		t.Errorf("Parse after Close = %v, want ErrClosed", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Runtime.Close should be idempotent: %v", err)
	}
}

func TestNewParserWithRuntimeRejectsClosedRuntime(t *testing.T) {
	rt, err := sitterwasm.NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewJSONRuntime: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}
	if parser, err := sitterwasm.NewParserWithRuntime(rt); parser != nil || !errors.Is(err, sitterwasm.ErrClosed) {
		t.Fatalf("NewParserWithRuntime(closed runtime) = parser %v, err %v; want ErrClosed", parser, err)
	}
}

func TestNewParserLazilyBindsLanguageRuntime(t *testing.T) {
	rt, err := sitterwasm.NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewJSONRuntime: %v", err)
	}
	defer rt.Close()
	language, err := rt.LoadLanguage("json")
	if err != nil {
		t.Fatalf("LoadLanguage: %v", err)
	}
	defer language.Close()

	// Match the construction order used by the upstream Go binding.  The
	// parser has no Runtime at construction time; SetLanguage should attach the
	// language's Runtime and allocate the guest parser lazily.
	p := sitterwasm.NewParser()
	if p.Handle() != 0 {
		t.Fatalf("unbound parser handle = %d, want zero", p.Handle())
	}
	if err := p.SetLanguage(language); err != nil {
		t.Fatalf("SetLanguage on unbound parser: %v", err)
	}
	defer p.Close()
	if p.Handle() == 0 {
		t.Fatal("SetLanguage did not allocate a guest parser")
	}
	tree, err := p.Parse([]byte(`{"ok":true}`), nil)
	if err != nil {
		t.Fatalf("Parse after lazy binding: %v", err)
	}
	defer tree.Close()
	if got := tree.RootNode().Type(); got != "document" {
		t.Fatalf("root type = %q, want document", got)
	}
}

func TestParserBoundaryInputsDoNotPanic(t *testing.T) {
	p, _ := newJSONParser(t)
	cases := [][]byte{
		nil,
		{},
		{0},
		{0xff, 0xfe, 0xfd},
		[]byte("\xef\xbb\xbf{}"),
		[]byte("[\"unterminated"),
		bytes.Repeat([]byte("{"), 64*1024),
	}
	for i, input := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			tree, err := p.Parse(input, nil)
			if err != nil {
				t.Fatalf("Parse boundary input: %v", err)
			}
			defer tree.Close()
			if tree.RootNode().IsNull() {
				t.Fatal("boundary parse returned a null root")
			}
			if got := tree.SourceContent(); !bytes.Equal(got, input) {
				t.Errorf("SourceContent differs: got %d bytes, want %d", len(got), len(input))
			}
		})
	}
}

func TestParserDeterministicFuzzCorpus(t *testing.T) {
	p, _ := newJSONParser(t)
	rng := rand.New(rand.NewSource(0x5eed))
	for i := 0; i < 100; i++ {
		input := make([]byte, rng.Intn(129))
		if _, err := rng.Read(input); err != nil {
			t.Fatalf("generate input %d: %v", i, err)
		}
		tree, err := p.Parse(input, nil)
		if err != nil {
			t.Fatalf("Parse random input %d (%x): %v", i, input, err)
		}
		if tree.RootNode().IsNull() {
			t.Fatalf("random input %d returned null root", i)
		}
		_ = tree.Close()
	}
}

func TestParsersCanShareRuntimeConcurrently(t *testing.T) {
	rt, err := sitterwasm.NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewJSONRuntime: %v", err)
	}
	defer rt.Close()
	lang, err := sitterwasm.NewLanguage(rt)
	if err != nil {
		t.Fatalf("NewLanguage: %v", err)
	}
	defer lang.Close()
	const workers = 6
	parsers := make([]*sitterwasm.Parser, workers)
	for i := range parsers {
		parsers[i], err = sitterwasm.NewParserWithRuntime(rt)
		if err != nil {
			t.Fatalf("NewParserWithRuntime(%d): %v", i, err)
		}
		if err := parsers[i].SetLanguage(lang); err != nil {
			t.Fatalf("SetLanguage(%d): %v", i, err)
		}
		defer parsers[i].Close()
	}
	var wg sync.WaitGroup
	errCh := make(chan error, workers*4)
	for i, parser := range parsers {
		wg.Add(1)
		go func(worker int, p *sitterwasm.Parser) {
			defer wg.Done()
			for round := 0; round < 4; round++ {
				tree, parseErr := p.Parse([]byte(`[1, {"worker": 2}]`), nil)
				if parseErr != nil {
					errCh <- fmt.Errorf("worker %d round %d: %w", worker, round, parseErr)
					continue
				}
				if tree.RootNode().Type() != "document" {
					errCh <- fmt.Errorf("worker %d round %d: unexpected root", worker, round)
				}
				_ = tree.Close()
			}
		}(i, parser)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestLanguageMetadata(t *testing.T) {
	rt, err := sitterwasm.NewJSONRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewJSONRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	lang, err := sitterwasm.NewLanguage(rt, "json")
	if err != nil {
		t.Fatalf("NewLanguage(json): %v", err)
	}
	t.Cleanup(func() { _ = lang.Close() })
	if lang.Handle() == 0 {
		t.Fatal("Language.Handle() = 0")
	}
	if got := lang.Name(); got != "json" {
		t.Errorf("Language.Name() = %q, want json", got)
	}
	if got := lang.ABIVersion(); got == 0 {
		t.Error("Language.ABIVersion() = 0, want a valid ABI version")
	}
	if got := lang.SymbolCount(); got == 0 {
		t.Error("Language.SymbolCount() = 0")
	}
	if got := lang.FieldCount(); got == 0 {
		t.Error("Language.FieldCount() = 0")
	}
	objectID := lang.SymbolForName("object")
	if objectID == 0 {
		t.Fatal("SymbolForName(object) = 0")
	}
	if got := lang.SymbolName(objectID); got != "object" {
		t.Errorf("SymbolName(SymbolForName(object)) = %q, want object", got)
	}
	// The JSON grammar exposes the key/value fields; field lookup is exercised
	// through Node.ChildByFieldName in the navigation test.
}

func TestParseReaderAndProgressCancellation(t *testing.T) {
	p, _ := newJSONParser(t)
	tree, err := p.ParseReader(bytes.NewBufferString(`[1,2,3]`), nil)
	if err != nil {
		t.Fatalf("ParseReader: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	if got := tree.ToSexp(); got != `(document (array (number) (number) (number)))` {
		t.Errorf("ParseReader tree = %q", got)
	}

	calls := 0
	_, err = p.ParseWithOptions(context.Background(), []byte("1"), nil, &sitterwasm.ParseOptions{
		ProgressCallback: func(sitterwasm.ParseState) bool {
			calls++
			return true
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ParseWithOptions error = %v, want context.Canceled", err)
	}
	if calls == 0 {
		t.Error("ProgressCallback was not invoked")
	}
}

func TestParserConvenienceInputForms(t *testing.T) {
	p, _ := newJSONParser(t)
	for name, parse := range map[string]func() (*sitterwasm.Tree, error){
		"string": func() (*sitterwasm.Tree, error) {
			return p.ParseString(`{"ok": true}`, nil)
		},
		"utf8": func() (*sitterwasm.Tree, error) {
			return p.ParseUTF8([]byte(`{"ok": true}`), nil)
		},
		"compat": func() (*sitterwasm.Tree, error) {
			return p.ParseCompat([]byte(`{"ok": true}`), nil), nil
		},
		"must": func() (*sitterwasm.Tree, error) {
			return p.MustParse([]byte(`{"ok": true}`), nil), nil
		},
		"context": func() (*sitterwasm.Tree, error) {
			return p.ParseContext(context.Background(), []byte(`{"ok": true}`), nil)
		},
		"ctx-alias": func() (*sitterwasm.Tree, error) {
			return p.ParseCtx(context.Background(), []byte(`{"ok": true}`), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree, err := parse()
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tree == nil || tree.RootNode().Type() != "document" {
				t.Fatalf("tree/root = %#v/%q", tree, tree.RootNode().Type())
			}
			_ = tree.Close()
		})
	}

	var calls []struct {
		offset uint32
		point  sitterwasm.Point
	}
	chunks := [][]byte{[]byte(`{"`), []byte("ok"), []byte(`":true}`)}
	i := 0
	tree, err := p.ParseInput(func(offset uint32, point sitterwasm.Point) []byte {
		calls = append(calls, struct {
			offset uint32
			point  sitterwasm.Point
		}{offset, point})
		if i == len(chunks) {
			return nil
		}
		chunk := chunks[i]
		i++
		return chunk
	}, nil)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	defer tree.Close()
	if tree.Source() != `{"ok":true}` {
		t.Errorf("ParseInput source = %q", tree.Source())
	}
	if len(calls) != 4 || calls[0].offset != 0 || calls[1].offset != 2 || calls[2].offset != 4 || calls[3].offset != 11 {
		t.Errorf("ParseInput callback calls = %#v", calls)
	}
	if _, err := p.ParseInput(nil, nil); err == nil {
		t.Error("ParseInput(nil) should return an error")
	}

	utf8Source := `{"x":"猫"}`
	units := utf16.Encode([]rune(utf8Source))
	for name, parse := range map[string]func([]uint16) (*sitterwasm.Tree, error){
		"utf16le": func(input []uint16) (*sitterwasm.Tree, error) {
			return p.ParseUTF16LE(input, nil)
		},
		"utf16be": func(input []uint16) (*sitterwasm.Tree, error) {
			return p.ParseUTF16BE(input, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree, err := parse(units)
			if err != nil {
				t.Fatalf("parse UTF-16: %v", err)
			}
			defer tree.Close()
			if tree.Source() != utf8Source {
				t.Errorf("UTF-16 source = %q, want %q", tree.Source(), utf8Source)
			}
		})
	}
}

func TestParseWithOptionsAcceptsUpstreamCallbackForm(t *testing.T) {
	p, _ := newJSONParser(t)
	source := []byte(`{"callback": [1, 2]}`)
	calls := 0
	tree, err := p.ParseWithOptions(func(offset int, _ sitterwasm.Point) []byte {
		calls++
		if offset >= len(source) {
			return nil
		}
		return source[offset:]
	}, nil, &sitterwasm.ParseOptions{})
	if err != nil {
		t.Fatalf("ParseWithOptions(callback, oldTree, options): %v", err)
	}
	defer tree.Close()
	if calls < 2 || tree.Source() != string(source) {
		t.Fatalf("callback calls/source = %d/%q, want at least 2/%q", calls, tree.Source(), source)
	}

	// The existing context+bytes form remains available through the same
	// method, including a nil options argument.
	contextTree, err := p.ParseWithOptions(context.Background(), []byte(`null`), nil, nil)
	if err != nil {
		t.Fatalf("ParseWithOptions(context, bytes, oldTree, options): %v", err)
	}
	defer contextTree.Close()
	if got := contextTree.RootNode().NamedChild(0).Type(); got != "null" {
		t.Fatalf("context-form root child type = %q, want null", got)
	}
}

func TestParserInputCallbacksAcceptRemainingSuffixes(t *testing.T) {
	p, _ := newJSONParser(t)
	source := []byte(`{"ok": [1, 2]}`)
	// This is the callback shape used by go-tree-sitter: each invocation may
	// return the complete suffix beginning at the requested offset.
	tree, err := p.ParseInput(func(offset uint32, _ sitterwasm.Point) []byte {
		if int(offset) >= len(source) {
			return nil
		}
		return source[offset:]
	}, nil)
	if err != nil {
		t.Fatalf("suffix ParseInput: %v", err)
	}
	defer tree.Close()
	if got := tree.Source(); got != string(source) {
		t.Fatalf("suffix callback source = %q, want %q", got, source)
	}

	units := utf16.Encode([]rune(string(source)))
	utfTree, err := p.ParseUTF16LEWith(func(offset int, _ sitterwasm.Point) []uint16 {
		if offset >= len(units) {
			return nil
		}
		return units[offset:]
	}, nil)
	if err != nil {
		t.Fatalf("UTF-16 suffix callback: %v", err)
	}
	defer utfTree.Close()
	if got := utfTree.Source(); got != string(source) {
		t.Fatalf("UTF-16 suffix source = %q, want %q", got, source)
	}
}

func TestParserInputCallbacksPreserveRepeatedSuffixChunks(t *testing.T) {
	p, _ := newJSONParser(t)
	// The callback contract is positional, not content-based.  In particular,
	// two adjacent chunks may have identical bytes; a collector must not treat
	// the second `aa` as a copy of the prefix already seen.
	source := []byte(`aaaa`)
	chunks := [][]byte{source[:2], source[2:]}
	var calls int
	var offsets []uint32
	tree, err := p.ParseInput(func(offset uint32, _ sitterwasm.Point) []byte {
		offsets = append(offsets, offset)
		if calls >= len(chunks) {
			return nil
		}
		chunk := chunks[calls]
		calls++
		return chunk
	}, nil)
	if err != nil {
		t.Fatalf("ParseInput repeated chunks: %v", err)
	}
	defer tree.Close()
	if got := tree.Source(); got != string(source) {
		t.Fatalf("repeated chunks source = %q, want %q", got, source)
	}
	if calls != len(chunks) || len(offsets) != len(chunks)+1 {
		t.Fatalf("callback calls/offsets = %d/%v, want %d calls plus EOF probe", calls, offsets, len(chunks))
	}
	wantOffsets := []uint32{0, 2, uint32(len(source))}
	for i, want := range wantOffsets {
		if offsets[i] != want {
			t.Fatalf("callback offset[%d] = %d, want %d (all offsets: %v)", i, offsets[i], want, offsets)
		}
	}

	units := utf16.Encode([]rune(string(source)))
	utfChunks := [][]uint16{units[:2], units[2:]}
	utfCalls := 0
	utfTree, err := p.ParseUTF16LEWith(func(offset int, _ sitterwasm.Point) []uint16 {
		if utfCalls >= len(utfChunks) {
			return nil
		}
		chunk := utfChunks[utfCalls]
		utfCalls++
		_ = offset
		return chunk
	}, nil)
	if err != nil {
		t.Fatalf("ParseUTF16LEWith repeated chunks: %v", err)
	}
	defer utfTree.Close()
	if got := utfTree.Source(); got != string(source) {
		t.Fatalf("repeated UTF-16 chunks source = %q, want %q", got, source)
	}
}

func TestParserContextCancellationIsPropagated(t *testing.T) {
	p, _ := newJSONParser(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tree, err := p.ParseContext(ctx, []byte(`{"cancelled": true}`), nil)
	if tree != nil {
		_ = tree.Close()
		t.Fatal("canceled parse returned a tree")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parse error = %v, want context.Canceled", err)
	}
}

func TestParserTimeoutLoggerAndResetAliases(t *testing.T) {
	p, _ := newJSONParser(t)
	if err := p.SetTimeout(1500 * time.Microsecond); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	if got := p.TimeoutMicros(); got != 1500 {
		t.Errorf("TimeoutMicros = %d, want 1500", got)
	}
	if err := p.SetTimeout(-time.Second); err != nil {
		t.Fatalf("SetTimeout(negative): %v", err)
	}
	if got := p.TimeoutMicros(); got != 0 {
		t.Errorf("negative timeout normalized to %d, want 0", got)
	}
	if err := p.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	var gotType, gotMessage string
	logger := sitterwasm.Logger(func(typ, message string) {
		gotType, gotMessage = typ, message
	})
	if err := p.SetLogger(logger); err != nil {
		t.Fatalf("SetLogger: %v", err)
	}
	if p.Logger() == nil {
		t.Fatal("Logger() returned nil after SetLogger")
	}
	if err := p.SetLogger(nil); err != nil {
		t.Fatalf("SetLogger(nil): %v", err)
	}
	if p.Logger() != nil {
		t.Error("Logger() should be nil after SetLogger(nil)")
	}
	_ = gotType
	_ = gotMessage
}

func TestParserCompatibilityCancellationAndDotGraphAPIs(t *testing.T) {
	p, _ := newJSONParser(t)

	// The upstream binding exposes a pointer-sized cancellation flag.  The WASM
	// adapter mirrors that host flag into the guest parser and must honor it
	// before entering the guest, then resume once the caller clears it.
	flag := p.CancellationFlag()
	if flag == nil {
		t.Fatal("CancellationFlag returned nil")
	}
	atomic.StoreUintptr(flag, 1)
	if tree, err := p.Parse([]byte(`null`), nil); tree != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-set cancellation = tree %v, err %v; want context.Canceled", tree, err)
	}
	atomic.StoreUintptr(flag, 0)
	tree, err := p.Parse([]byte(`null`), nil)
	if err != nil {
		t.Fatalf("parse after clearing cancellation: %v", err)
	}
	defer tree.Close()

	if err := p.PrintDotGraphs(nil); err == nil {
		t.Fatal("PrintDotGraphs(nil) unexpectedly succeeded")
	}
	if err := p.StopPrintingDotGraphs(); err != nil {
		t.Fatalf("StopPrintingDotGraphs: %v", err)
	}
	if err := tree.PrintDotGraph(-1); err == nil {
		t.Fatal("Tree.PrintDotGraph(-1) unexpectedly succeeded")
	}
}

func TestParserUTF16OptionsCompatibilityAliases(t *testing.T) {
	p, _ := newJSONParser(t)
	units := utf16.Encode([]rune(`{"ok": true}`))
	calls := 0
	tree, err := p.ParseUTF16LEWithOptions(func(offset int, _ sitterwasm.Point) []uint16 {
		calls++
		if offset >= len(units) {
			return nil
		}
		return units[offset:]
	}, nil, &sitterwasm.ParseOptions{ProgressCallback: func(sitterwasm.ParseState) bool { return false }})
	if err != nil {
		t.Fatalf("ParseUTF16LEWithOptions: %v", err)
	}
	defer tree.Close()
	if calls < 2 || tree.Source() != `{"ok": true}` {
		t.Fatalf("UTF-16 options callback calls/source = %d/%q", calls, tree.Source())
	}
	beTree, err := p.ParseUTF16BEWithOptions(func(offset int, point sitterwasm.Point) []uint16 {
		if offset >= len(units) {
			return nil
		}
		return units[offset:]
	}, nil, nil)
	if err != nil {
		t.Fatalf("ParseUTF16BEWithOptions: %v", err)
	}
	defer beTree.Close()
	if beTree.Source() != `{"ok": true}` {
		t.Fatalf("UTF-16 BE alias source = %q", beTree.Source())
	}
}

func TestParserIncludedRangesValidationAndEmptyReset(t *testing.T) {
	p, _ := newJSONParser(t)
	// Out-of-order and overlapping ranges report the first offending index as a
	// typed error, matching the native Tree-sitter binding.
	err := p.SetIncludedRanges([]sitterwasm.Range{
		{StartByte: 10, EndByte: 12},
		{StartByte: 4, EndByte: 8},
	})
	var rangeErr *sitterwasm.IncludedRangesError
	if !errors.As(err, &rangeErr) || rangeErr.Index != 1 {
		t.Fatalf("out-of-order ranges error = %T/%v, want IncludedRangesError{1}", err, err)
	}
	err = p.SetIncludedRanges([]sitterwasm.Range{{StartByte: 8, EndByte: 4}})
	if !errors.As(err, &rangeErr) || rangeErr.Index != 0 {
		t.Fatalf("reversed range error = %T/%v, want IncludedRangesError{0}", err, err)
	}

	// An empty list is the documented reset-to-whole-document operation, and
	// must not leave a stale prior range configured on the parser.
	if err := p.SetIncludedRanges([]sitterwasm.Range{{StartByte: 0, EndByte: 5}}); err != nil {
		t.Fatalf("SetIncludedRanges(non-empty): %v", err)
	}
	if got := p.IncludedRanges(); len(got) != 1 || got[0].StartByte != 0 || got[0].EndByte != 5 {
		t.Fatalf("configured ranges = %#v, want [0,5)", got)
	}
	if err := p.SetIncludedRanges(nil); err != nil {
		t.Fatalf("SetIncludedRanges(nil): %v", err)
	}
	got := p.IncludedRanges()
	if len(got) != 1 || got[0].StartByte != 0 || got[0].EndByte != ^uint32(0) {
		// Before parsing, Tree-sitter's default range is represented by the
		// UINT32_MAX sentinel; after a parse the tree carries the concrete
		// source extent.
		t.Fatalf("reset ranges = %#v, want one default range", got)
	}
}

func TestIncrementalParseAndChangedRanges(t *testing.T) {
	p, _ := newJSONParser(t)
	oldSource := []byte(`{"a": 1, "b": 2}`)
	newSource := []byte(`{"a": 1, "b": false}`)
	oldTree, err := p.Parse(oldSource, nil)
	if err != nil {
		t.Fatalf("initial Parse: %v", err)
	}
	t.Cleanup(func() { _ = oldTree.Close() })
	index := bytes.Index(oldSource, []byte("2"))
	if index < 0 {
		t.Fatal("test source has no edit target")
	}
	edit := sitterwasm.InputEdit{
		StartByte:   uint32(index),
		OldEndByte:  uint32(index + 1),
		NewEndByte:  uint32(index + 1),
		StartPoint:  pointAt(oldSource, index),
		OldEndPoint: pointAt(oldSource, index+1),
		NewEndPoint: pointAt(newSource, index+1),
	}
	if err := oldTree.Edit(edit); err != nil {
		t.Fatalf("Tree.Edit: %v", err)
	}
	newTree, err := p.Parse(newSource, oldTree)
	if err != nil {
		t.Fatalf("incremental Parse: %v", err)
	}
	t.Cleanup(func() { _ = newTree.Close() })
	if newTree.RootNode().HasError() {
		t.Fatalf("incremental tree has an error: %s", newTree.ToSexp())
	}
	if got := newTree.RootNode().NamedChild(0).NamedChild(1).ChildByFieldName("value").Text(); got != "false" {
		t.Errorf("edited value text = %q, want false", got)
	}
	ranges, err := oldTree.ChangedRanges(newTree)
	if err != nil {
		t.Fatalf("ChangedRanges: %v", err)
	}
	if len(ranges) == 0 {
		t.Fatal("ChangedRanges returned no ranges for a changed scalar")
	}
	found := false
	for _, r := range ranges {
		if r.StartByte <= uint32(index) && uint32(index) < r.EndByte {
			found = true
		}
		if r.EndByte > uint32(len(newSource)) {
			t.Errorf("changed range %#v exceeds source length %d", r, len(newSource))
		}
	}
	if !found {
		t.Errorf("changed ranges %#v do not cover edited byte %d", ranges, index)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
