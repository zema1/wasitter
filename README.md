# wasitter

[![CI](https://img.shields.io/github/actions/workflow/status/zema1/wasitter/ci.yml?branch=main&label=CI&style=flat-square)](https://github.com/zema1/wasitter/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zema1/wasitter?include_prereleases&style=flat-square)](https://github.com/zema1/wasitter/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/zema1/wasitter?style=flat-square&logo=go)](https://go.dev/dl/)
[![Go Reference](https://img.shields.io/badge/Go-reference-00ADD8?style=flat-square&logo=go)](https://pkg.go.dev/github.com/zema1/wasitter)
[![License](https://img.shields.io/github/license/zema1/wasitter?style=flat-square)](LICENSE)

[简体中文](README_CN.md) ·
[Downloads](https://github.com/zema1/wasitter/releases)

wasitter brings [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) to Go
using the upstream C implementation. It compiles the Tree-sitter runtime and
language parsers into WebAssembly, then loads and runs them inside Go programs
with [wazero](https://wazero.io/).

## Features

- No CGO, C compiler, or system shared libraries required. Build and cross-compile
  with the standard Go toolchain.
- Tracks upstream releases and reuses the original parsing implementation,
  reducing the risk of bugs and the maintenance burden associated with a rewrite.
- Packages language support as individual WASM files that can be loaded on
  demand, without bundling every language by default.
- Provides a thin wrapper around the upstream API that follows Go conventions.
- Uses [native comparison tests](comparison/README.md) to check that key
  behaviors match upstream Tree-sitter.

> WASM execution and calls between Go and WASM add overhead, so wasitter is
> slightly slower than the native implementation. I'll do my best to address
> unexpected performance issues.

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

The runtime executes the language module, and the parser turns source code into
a syntax tree. The example then finds the function node and reads its `name` field.
For JSON, create the parser with `wasitter.NewJSONParser(ctx)`.

## Loading other languages

Download WASM files from the [releases page](https://github.com/zema1/wasitter/releases).
Choose the release that matches the wasitter version in your project. Mixing versions
is unsupported and may lead to incorrect behavior.

There are currently 12 supported languages: JavaScript, JSON, Bash, C, C++, Go,
Java, Python, Ruby, Rust, TypeScript, and TSX.

For Python, download **`wasitter-python.wasm`** to a location such as
`grammars/wasitter-python.wasm`, then load it as follows:

```go
parser, runtime, err := wasitter.NewParserFromFile(ctx, "grammars/wasitter-python.wasm")
if err != nil {
	return err
}
defer runtime.Close()
defer parser.Close()

source := []byte("def greet(name):\n    return name\n")
```

The constructor loads the grammar from the WASM module and returns a parser
that is ready to use. Then call `ParseContext` as in the quick start example.
If you already have the WASM bytes, for example through `go:embed`, use
`wasitter.NewParserFromWASM(ctx, wasmBytes)` instead.

## Usage notes

- Reuse a parser and runtime when processing multiple files, and close each
  tree when you are done with it. For parallel parsing, give each worker its own runtime
  and parser.
- Keep a tree open while reading its nodes. Close trees, queries, and parsers
  before closing the runtime. The examples use `defer` to ensure this cleanup
  order; deferred calls run in reverse order.
- A successful parse does not mean the source has no syntax errors. Tree-sitter
  can produce a tree for incomplete code; check `tree.RootNode().HasError()`
  separately for syntax errors.
- Node positions use UTF-8 byte offsets and byte columns, not character counts
  or UTF-16 code-unit offsets.

The [usage guide](USAGE.md) covers queries, incremental parsing, runtime
configuration, and compatibility.

## Contributing

For instructions on adding languages, rebuilding WASM, running tests, and
publishing releases, see
[DEVELOPMENT.md](DEVELOPMENT.md).

## License

wasitter is licensed under the [MIT License](LICENSE). License information for
the grammars, runtime, and toolchain is available in the
[third-party notices](internal/wasm/THIRD_PARTY_NOTICES.md).
When distributing WASM files, include `THIRD_PARTY_NOTICES.txt` from the same release.
