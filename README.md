# wasitter

[简体中文](README_CN.md) ·
[Documentation](https://pkg.go.dev/github.com/zema1/wasitter) ·
[Downloads](https://github.com/zema1/wasitter/releases)

wasitter brings [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) to Go
using the upstream C implementation. It compiles the Tree-sitter runtime and
language parsers into WebAssembly, then loads and runs them inside Go programs
with [wazero](https://wazero.io/).

## Features

- No CGO, C compiler, or system shared libraries required. Build and cross-compile
  with the standard Go toolchain.
- Tracks upstream releases and reuses the original parsing implementation,
  avoiding the bugs and maintenance burden of a separate rewrite.
- Packages language support as individual WASM files that can be loaded on
  demand, without bundling every language by default.
- Provides a thin wrapper around the upstream API with Go conventions in mind.
- Uses [native comparison tests](comparison/README.md) to check that key
  behaviors match upstream Tree-sitter.

> WASM execution and calls between Go and WASM add overhead compared with
> native bindings. This performance cost is expected.

## Quick start

Requires **Go 1.23 or newer**.

```sh
go get github.com/zema1/wasitter@latest
```

JavaScript and JSON parsers are embedded in the package. The following example
uses the built-in JavaScript parser:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zema1/wasitter"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	parser, runtime, err := wasitter.NewJavaScriptParser(ctx)
	if err != nil {
		return err
	}
	defer runtime.Close()
	defer parser.Close()

	source := []byte(`function greet(name) { return "Hello, " + name; }`)
	tree, err := parser.ParseContext(ctx, source, nil)
	if err != nil {
		return err
	}
	defer tree.Close()

	function := tree.RootNode().NamedChild(0)
	name := function.ChildByFieldName("name")
	fmt.Println(name.Content(source)) // greet
	return nil
}
```

The runtime runs the language module, and the parser turns source code into
a tree. The example then finds the function node and reads its `name` field.
For JSON, create the parser with `wasitter.NewJSONParser(ctx)`.

## Loading other languages

Download WASM files from [Releases](https://github.com/zema1/wasitter/releases).
Use the release matching the wasitter version in your project. Mixing versions
is unsupported and may lead to incorrect behavior.

There are currently 12 supported languages: JavaScript, JSON, Bash, C, C++, Go,
Java, Python, Ruby, Rust, TypeScript, and TSX.

For Python, download **`wasitter-python.wasm`** to a location such as
`grammars/wasitter-python.wasm`, then load it with the following setup:

```go
parser, runtime, err := wasitter.NewParserFromFile(ctx, "grammars/wasitter-python.wasm")
if err != nil {
	return err
}
defer runtime.Close()
defer parser.Close()

source := []byte("def greet(name):\n    return name\n")
```

The constructor reads the grammar from the WASM module and returns a ready to
use parser. Continue with `ParseContext` as in the quick start. For bytes
loaded with `go:embed`, use
`wasitter.NewParserFromWASM(ctx, wasmBytes)` instead.

## Usage notes

- Reuse a parser and runtime when processing multiple files, and close each
  tree when finished. For parallel parsing, give each worker its own runtime
  and parser.
- Keep a tree open while reading its nodes. Close trees, queries, and parsers
  before closing the runtime. The examples arrange their `defer` calls in
  this order.
- A successful parse does not mean the source has no syntax errors. Tree-sitter
  can produce a tree for incomplete code; check `tree.RootNode().HasError()`
  separately for syntax errors.
- Node positions use UTF-8 byte offsets and byte columns, not character counts
  or UTF-16 code-unit offsets.

The [usage guide](USAGE.md) covers queries, incremental parsing, runtime
configuration, and compatibility.

## Contributing

For adding languages, rebuilding WASM, testing, and releasing, see
[DEVELOPMENT.md](DEVELOPMENT.md).
Report vulnerabilities as described in the [security policy](SECURITY.md).

## License

wasitter is [MIT licensed](LICENSE). License information for the grammars,
runtime, and toolchain is listed in the
[third-party notices](internal/wasm/THIRD_PARTY_NOTICES.md).
Include the Release's `THIRD_PARTY_NOTICES.txt` when distributing WASM files.
