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
		fmt.Println(tree.RootNode().Kind())
		if err := tree.Close(); err != nil {
			panic(err)
		}
	}
	// Output:
	// program
	// program
}
