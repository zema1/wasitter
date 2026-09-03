package comparison

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	nativejson "github.com/tree-sitter/tree-sitter-json/bindings/go"
	wasm "github.com/zema1/sitterwasm"
)

type corpusCase struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Sexp   string `json:"sexp"`
}

func loadCorpus(t testing.TB) []corpusCase {
	t.Helper()
	path := filepath.Join("..", "testdata", "json_cases.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus %s: %v", path, err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("corpus is empty")
	}
	return cases
}

func newWASM(t testing.TB) (*wasm.Parser, *wasm.Runtime) {
	t.Helper()
	p, rt, err := wasm.NewJSONParser(context.Background())
	if err != nil {
		t.Fatalf("NewJSONParser: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		_ = rt.Close()
	})
	return p, rt
}

func newNative(t testing.TB) (*native.Parser, *native.Language) {
	t.Helper()
	language := native.NewLanguage(nativejson.Language())
	if language == nil {
		t.Fatal("native.NewLanguage returned nil")
	}
	parser := native.NewParser()
	if err := parser.SetLanguage(language); err != nil {
		parser.Close()
		t.Fatalf("native SetLanguage: %v", err)
	}
	t.Cleanup(parser.Close)
	return parser, language
}

type pointView struct {
	Row    uint32
	Column uint32
}

type nodeView struct {
	Type        string
	GrammarType string
	KindID      uint16
	GrammarID   uint16
	Named       bool
	Missing     bool
	Extra       bool
	Error       bool
	HasError    bool
	StartByte   uint32
	EndByte     uint32
	StartPoint  pointView
	EndPoint    pointView
	FieldName   string
	Children    []nodeView
}

func wasmNodeView(n wasm.Node, field string) nodeView {
	v := nodeView{
		Type:        n.Type(),
		GrammarType: n.GrammarType(),
		KindID:      n.KindID(),
		GrammarID:   n.GrammarSymbol(),
		Named:       n.IsNamed(),
		Missing:     n.IsMissing(),
		Extra:       n.IsExtra(),
		Error:       n.IsError(),
		HasError:    n.HasError(),
		StartByte:   n.StartByte(),
		EndByte:     n.EndByte(),
		StartPoint:  pointView{Row: n.StartPoint().Row, Column: n.StartPoint().Column},
		EndPoint:    pointView{Row: n.EndPoint().Row, Column: n.EndPoint().Column},
		FieldName:   field,
	}
	for i := 0; i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		v.Children = append(v.Children, wasmNodeView(child, n.FieldNameForChild(i)))
	}
	return v
}

func nativeNodeView(n *native.Node, field string) nodeView {
	if n == nil {
		return nodeView{}
	}
	v := nodeView{
		Type:        n.Kind(),
		GrammarType: n.GrammarName(),
		KindID:      n.KindId(),
		GrammarID:   n.GrammarId(),
		Named:       n.IsNamed(),
		Missing:     n.IsMissing(),
		Extra:       n.IsExtra(),
		Error:       n.IsError(),
		HasError:    n.HasError(),
		StartByte:   uint32(n.StartByte()),
		EndByte:     uint32(n.EndByte()),
		StartPoint:  pointView{Row: uint32(n.StartPosition().Row), Column: uint32(n.StartPosition().Column)},
		EndPoint:    pointView{Row: uint32(n.EndPosition().Row), Column: uint32(n.EndPosition().Column)},
		FieldName:   field,
	}
	for i := uint32(0); i < uint32(n.ChildCount()); i++ {
		child := n.Child(uint(i))
		if child == nil {
			continue
		}
		v.Children = append(v.Children, nativeNodeView(child, n.FieldNameForChild(i)))
	}
	return v
}

func TestParserCorpusParity(t *testing.T) {
	wasmParser, _ := newWASM(t)
	nativeParser, _ := newNative(t)
	for _, tc := range loadCorpus(t) {
		t.Run(tc.Name, func(t *testing.T) {
			wasmTree, err := wasmParser.Parse([]byte(tc.Source), nil)
			if err != nil {
				t.Fatalf("WASM parse: %v", err)
			}
			t.Cleanup(func() { _ = wasmTree.Close() })
			nativeTree := nativeParser.Parse([]byte(tc.Source), nil)
			if nativeTree == nil {
				t.Fatal("native parse returned nil")
			}
			t.Cleanup(nativeTree.Close)

			if got := wasmTree.ToSExpression(); got != tc.Sexp {
				t.Fatalf("WASM S-expression = %q, want %q", got, tc.Sexp)
			}
			if got := nativeTree.RootNode().ToSexp(); got != tc.Sexp {
				t.Fatalf("native S-expression = %q, want %q", got, tc.Sexp)
			}
			wasmView := wasmNodeView(wasmTree.RootNode(), "")
			nativeView := nativeNodeView(nativeTree.RootNode(), "")
			if !reflect.DeepEqual(wasmView, nativeView) {
				t.Fatalf("tree views differ:\nWASM:   %#v\nNative: %#v", wasmView, nativeView)
			}
		})
	}
}

type captureView struct {
	Index uint32
	Type  string
	Start uint32
	End   uint32
	Text  string
}

type matchView struct {
	Pattern  uint32
	Captures []captureView
}

func wasmMatchViews(matches wasm.QueryMatches, source []byte) []matchView {
	var out []matchView
	for match := matches.Next(); match != nil; match = matches.Next() {
		view := matchView{Pattern: match.PatternIndex}
		for _, capture := range match.Captures {
			view.Captures = append(view.Captures, captureView{
				Index: capture.Index,
				Type:  capture.Node.Type(),
				Start: capture.Node.StartByte(),
				End:   capture.Node.EndByte(),
				Text:  capture.Node.Utf8Text(source),
			})
		}
		out = append(out, view)
	}
	return out
}

func nativeMatchViews(matches native.QueryMatches, source []byte) []matchView {
	var out []matchView
	for match := matches.Next(); match != nil; match = matches.Next() {
		view := matchView{Pattern: uint32(match.PatternIndex)}
		for _, capture := range match.Captures {
			view.Captures = append(view.Captures, captureView{
				Index: capture.Index,
				Type:  capture.Node.Kind(),
				Start: uint32(capture.Node.StartByte()),
				End:   uint32(capture.Node.EndByte()),
				Text:  capture.Node.Utf8Text(source),
			})
		}
		out = append(out, view)
	}
	return out
}

func TestQueryParity(t *testing.T) {
	wasmParser, _ := newWASM(t)
	nativeParser, nativeLanguage := newNative(t)
	queries := []string{
		`(number) @number`,
		`(pair key: (string) @key value: (_) @value)`,
		`((number) @number (#match? @number "^(1|2|13)$"))`,
		`[(true) (false) (null)] @literal`,
	}
	source := []byte(`{"name":"sitterwasm","enabled":true,"items":[1,2,13,null],"nested":{"text":"hello"}}`)
	wasmTree, err := wasmParser.Parse(source, nil)
	if err != nil {
		t.Fatalf("WASM parse: %v", err)
	}
	t.Cleanup(func() { _ = wasmTree.Close() })
	nativeTree := nativeParser.Parse(source, nil)
	if nativeTree == nil {
		t.Fatal("native parse returned nil")
	}
	t.Cleanup(nativeTree.Close)

	for i, sourceQuery := range queries {
		t.Run(fmt.Sprintf("query-%d", i), func(t *testing.T) {
			wasmQuery, err := wasm.NewQuery(wasmTree.Language(), sourceQuery)
			if err != nil {
				t.Fatalf("WASM query compile: %v", err)
			}
			t.Cleanup(func() { _ = wasmQuery.Close() })
			nativeQuery, nativeErr := native.NewQuery(nativeLanguage, sourceQuery)
			if nativeErr != nil {
				t.Fatalf("native query compile: %v", nativeErr)
			}
			t.Cleanup(nativeQuery.Close)

			if got, want := wasmQuery.PatternCount(), uint32(nativeQuery.PatternCount()); got != want {
				t.Fatalf("pattern count = %d, want %d", got, want)
			}
			if got, want := wasmQuery.CaptureNames(), nativeQuery.CaptureNames(); !reflect.DeepEqual(got, want) {
				t.Fatalf("capture names = %#v, want %#v", got, want)
			}
			wasmCursor := wasm.NewQueryCursor()
			t.Cleanup(func() { _ = wasmCursor.Close() })
			nativeCursor := native.NewQueryCursor()
			t.Cleanup(nativeCursor.Close)
			wasmMatches := wasmMatchViews(wasmCursor.Matches(wasmQuery, wasmTree.RootNode(), source), source)
			nativeMatches := nativeMatchViews(nativeCursor.Matches(nativeQuery, nativeTree.RootNode(), source), source)
			if !reflect.DeepEqual(wasmMatches, nativeMatches) {
				t.Fatalf("matches differ:\nWASM:   %#v\nNative: %#v", wasmMatches, nativeMatches)
			}
		})
	}
}

type cursorView struct {
	Type      string
	StartByte uint32
	EndByte   uint32
	Field     string
	FieldID   uint16
	Depth     uint32
	Index     uint32
}

func wasmCursorTrace(cursor *wasm.TreeCursor) []cursorView {
	var out []cursorView
	for {
		node := cursor.Node()
		out = append(out, cursorView{
			Type:      node.Type(),
			StartByte: node.StartByte(),
			EndByte:   node.EndByte(),
			Field:     cursor.FieldName(),
			FieldID:   cursor.FieldID(),
			Depth:     cursor.Depth(),
			Index:     cursor.DescendantIndex(),
		})
		if cursor.GotoFirstChild() {
			continue
		}
		for {
			if cursor.GotoNextSibling() {
				break
			}
			if !cursor.GotoParent() {
				return out
			}
		}
	}
}

func nativeCursorTrace(cursor *native.TreeCursor) []cursorView {
	var out []cursorView
	for {
		node := cursor.Node()
		out = append(out, cursorView{
			Type:      node.Kind(),
			StartByte: uint32(node.StartByte()),
			EndByte:   uint32(node.EndByte()),
			Field:     cursor.FieldName(),
			FieldID:   cursor.FieldId(),
			Depth:     cursor.Depth(),
			Index:     cursor.DescendantIndex(),
		})
		if cursor.GotoFirstChild() {
			continue
		}
		for {
			if cursor.GotoNextSibling() {
				break
			}
			if !cursor.GotoParent() {
				return out
			}
		}
	}
}

func TestCursorParity(t *testing.T) {
	wasmParser, _ := newWASM(t)
	nativeParser, _ := newNative(t)
	source := []byte(`{"items":[1,{"x":true},null],"name":"树🌳"}`)
	wasmTree, err := wasmParser.Parse(source, nil)
	if err != nil {
		t.Fatalf("WASM parse: %v", err)
	}
	t.Cleanup(func() { _ = wasmTree.Close() })
	nativeTree := nativeParser.Parse(source, nil)
	if nativeTree == nil {
		t.Fatal("native parse returned nil")
	}
	t.Cleanup(nativeTree.Close)
	wasmCursor := wasmTree.RootNode().Walk()
	if wasmCursor == nil {
		t.Fatal("WASM Walk returned nil")
	}
	t.Cleanup(func() { _ = wasmCursor.Close() })
	nativeCursor := nativeTree.RootNode().Walk()
	t.Cleanup(nativeCursor.Close)
	wasmTrace := wasmCursorTrace(wasmCursor)
	nativeTrace := nativeCursorTrace(nativeCursor)
	if !reflect.DeepEqual(wasmTrace, nativeTrace) {
		t.Fatalf("cursor traces differ:\nWASM:   %#v\nNative: %#v", wasmTrace, nativeTrace)
	}

	// Compare the two indexed child-search operations as well as the ordinary
	// depth-first walk. Both are commonly used by editor integrations.
	for offset := uint32(0); offset <= uint32(len(source)); offset++ {
		wasmCursor.Reset(wasmTree.RootNode())
		nativeCursor.Reset(*nativeTree.RootNode())
		wasmIndex := wasmCursor.GotoFirstChildForByte(offset)
		nativeIndex := nativeCursor.GotoFirstChildForByte(offset)
		if (wasmIndex == nil) != (nativeIndex == nil) {
			t.Fatalf("byte %d child presence differs: WASM=%v native=%v", offset, wasmIndex, nativeIndex)
		}
		if wasmIndex != nil && uint32(*wasmIndex) != uint32(*nativeIndex) {
			t.Fatalf("byte %d child index = %d, want %d", offset, *wasmIndex, *nativeIndex)
		}
		if got, want := wasmCursor.Node().Type(), nativeCursor.Node().Kind(); got != want {
			t.Fatalf("byte %d child node = %q, want %q", offset, got, want)
		}
	}

	// Repeat the indexed search using point coordinates. Deriving the points
	// from every byte boundary (including the end of the document) catches
	// differences in newline handling and UTF-8 byte columns that a byte-only
	// check cannot see.
	pointAt := func(offset int) wasm.Point {
		var point wasm.Point
		for _, value := range source[:offset] {
			if value == '\n' {
				point.Row++
				point.Column = 0
			} else {
				point.Column++
			}
		}
		return point
	}
	for offset := 0; offset <= len(source); offset++ {
		point := pointAt(offset)
		wasmCursor.Reset(wasmTree.RootNode())
		nativeCursor.Reset(*nativeTree.RootNode())
		wasmIndex := wasmCursor.GotoFirstChildForPoint(point)
		nativeIndex := nativeCursor.GotoFirstChildForPoint(native.Point{Row: uint(point.Row), Column: uint(point.Column)})
		if (wasmIndex == nil) != (nativeIndex == nil) {
			t.Fatalf("point (%d,%d) child presence differs: WASM=%v native=%v", point.Row, point.Column, wasmIndex, nativeIndex)
		}
		if wasmIndex != nil && uint32(*wasmIndex) != uint32(*nativeIndex) {
			t.Fatalf("point (%d,%d) child index = %d, want %d", point.Row, point.Column, *wasmIndex, *nativeIndex)
		}
		if got, want := wasmCursor.Node().Type(), nativeCursor.Node().Kind(); got != want {
			t.Fatalf("point (%d,%d) child node = %q, want %q", point.Row, point.Column, got, want)
		}
	}
}

func TestIncrementalParity(t *testing.T) {
	wasmParser, _ := newWASM(t)
	nativeParser, _ := newNative(t)
	oldSource := []byte(`{"value":1,"items":[true,null]}`)
	newSource := []byte(`{"value":42,"items":[true,null]}`)
	oldOffset := uint32(len(`{"value":`))
	wasmOld, err := wasmParser.Parse(oldSource, nil)
	if err != nil {
		t.Fatalf("WASM initial parse: %v", err)
	}
	t.Cleanup(func() { _ = wasmOld.Close() })
	nativeOld := nativeParser.Parse(oldSource, nil)
	if nativeOld == nil {
		t.Fatal("native initial parse returned nil")
	}
	t.Cleanup(nativeOld.Close)
	editPoint := wasm.Point{Row: 0, Column: oldOffset}
	wasmEdit := wasm.InputEdit{
		StartByte:   oldOffset,
		OldEndByte:  oldOffset + 1,
		NewEndByte:  oldOffset + 2,
		StartPoint:  editPoint,
		OldEndPoint: wasm.Point{Row: 0, Column: oldOffset + 1},
		NewEndPoint: wasm.Point{Row: 0, Column: oldOffset + 2},
	}
	if err := wasmOld.Edit(wasmEdit); err != nil {
		t.Fatalf("WASM edit: %v", err)
	}
	nativeOld.Edit(&native.InputEdit{
		StartByte:      uint(oldOffset),
		OldEndByte:     uint(oldOffset + 1),
		NewEndByte:     uint(oldOffset + 2),
		StartPosition:  native.Point{Row: 0, Column: uint(oldOffset)},
		OldEndPosition: native.Point{Row: 0, Column: uint(oldOffset + 1)},
		NewEndPosition: native.Point{Row: 0, Column: uint(oldOffset + 2)},
	})
	wasmNew, err := wasmParser.Parse(newSource, wasmOld)
	if err != nil {
		t.Fatalf("WASM incremental parse: %v", err)
	}
	t.Cleanup(func() { _ = wasmNew.Close() })
	nativeNew := nativeParser.Parse(newSource, nativeOld)
	if nativeNew == nil {
		t.Fatal("native incremental parse returned nil")
	}
	t.Cleanup(nativeNew.Close)
	if got, want := wasmNew.ToSExpression(), nativeNew.RootNode().ToSexp(); got != want {
		t.Fatalf("incremental S-expression = %q, want %q", got, want)
	}
	wasmChanged, err := wasmOld.ChangedRanges(wasmNew)
	if err != nil {
		t.Fatalf("WASM changed ranges: %v", err)
	}
	nativeChanged := nativeOld.ChangedRanges(nativeNew)
	if len(wasmChanged) != len(nativeChanged) {
		t.Fatalf("changed range count = %d, want %d", len(wasmChanged), len(nativeChanged))
	}
	for i, got := range wasmChanged {
		want := nativeChanged[i]
		if got.StartByte != uint32(want.StartByte) || got.EndByte != uint32(want.EndByte) ||
			got.StartPoint.Row != uint32(want.StartPoint.Row) || got.StartPoint.Column != uint32(want.StartPoint.Column) ||
			got.EndPoint.Row != uint32(want.EndPoint.Row) || got.EndPoint.Column != uint32(want.EndPoint.Column) {
			t.Fatalf("changed range %d = %#v, want %#v", i, got, want)
		}
	}
}
