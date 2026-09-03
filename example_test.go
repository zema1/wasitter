package sitterwasm_test

import (
	"context"
	"fmt"

	sitterwasm "github.com/zema1/sitterwasm"
)

func ExampleNewJSONParser() {
	parser, runtime, err := sitterwasm.NewJSONParser(context.Background())
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
