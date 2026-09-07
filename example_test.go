package wasitter_test

import (
	"context"
	"fmt"

	wasitter "github.com/zema1/wasitter"
)

func ExampleNewJSONParser() {
	parser, runtime, err := wasitter.NewJSONParser(context.Background())
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.Parse([]byte(`{"ok": true}`), nil)
	if err != nil {
		panic(err)
	}
	defer tree.Close()

	fmt.Println(tree.RootNode().Type())
	// Output: document
}

// One parser/runtime can process a batch of independent files. Close each
// tree promptly, then release the parser and runtime after the batch.
func ExampleNewJavaScriptParser() {
	parser, runtime, err := wasitter.NewJavaScriptParser(context.Background())
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()
	for _, source := range []string{`const a = 1;`, `const b = 2;`} {
		tree, err := parser.Parse([]byte(source), nil)
		if err != nil {
			panic(err)
		}
		fmt.Println(tree.RootNode().Type())
		if err := tree.Close(); err != nil {
			panic(err)
		}
	}
	// Output:
	// program
	// program
}

func ExampleNewParserFromWASM() {
	// wasm can also come from go:embed or a download managed by your application.
	wasm := wasitter.BuiltinJavaScriptWASM()
	parser, runtime, err := wasitter.NewParserFromWASM(context.Background(), wasm)
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.Parse([]byte(`function greet(name) { return name; }`), nil)
	if err != nil {
		panic(err)
	}
	defer tree.Close()
	fmt.Println(tree.RootNode().NamedChild(0).ChildByFieldName("name").Content())
	// Output: greet
}

func ExampleNewParserFromFile() {
	// Download the module from the release matching your wasitter version.
	ctx := context.Background()
	parser, runtime, err := wasitter.NewParserFromFile(ctx, "grammars/wasitter-python.wasm")
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.ParseContext(ctx, []byte("print('hello')\n"), nil)
	if err != nil {
		panic(err)
	}
	defer tree.Close()
	fmt.Println(tree.RootNode().Type())
}

func ExampleNewQuery() {
	ctx := context.Background()
	parser, runtime, err := wasitter.NewJSONParser(ctx)
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()

	source := []byte(`[1, 2]`)
	tree, err := parser.ParseContext(ctx, source, nil)
	if err != nil {
		panic(err)
	}
	defer tree.Close()

	language := parser.Language()
	defer language.Close()
	query, err := wasitter.NewQuery(language, `(number) @number`)
	if err != nil {
		panic(err)
	}
	defer query.Close()
	cursor := wasitter.NewQueryCursor()
	defer cursor.Close()
	matches, err := cursor.Matches(query, tree.RootNode(), source)
	if err != nil {
		panic(err)
	}
	for _, match := range matches {
		for _, capture := range match.Captures {
			fmt.Println(query.CaptureName(capture.Index), capture.Node.Content(source))
		}
	}
	// Output:
	// number 1
	// number 2
}

func ExampleTree_Edit() {
	ctx := context.Background()
	parser, runtime, err := wasitter.NewJSONParser(ctx)
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.ParseContext(ctx, []byte(`[1]`), nil)
	if err != nil {
		panic(err)
	}
	defer tree.Close()

	// Insert "0" after "1". All coordinates refer to UTF-8 bytes.
	err = tree.Edit(wasitter.InputEdit{
		StartByte: 2, OldEndByte: 2, NewEndByte: 3,
		StartPoint:  wasitter.Point{Column: 2},
		OldEndPoint: wasitter.Point{Column: 2},
		NewEndPoint: wasitter.Point{Column: 3},
	})
	if err != nil {
		panic(err)
	}
	updated, err := parser.ParseContext(ctx, []byte(`[10]`), tree)
	if err != nil {
		panic(err)
	}
	defer updated.Close()
	fmt.Println(updated.RootNode().NamedChild(0).NamedChild(0).Text())
	// Output: 10
}
