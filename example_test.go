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
