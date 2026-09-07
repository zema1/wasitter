# Usage guide

For installation and a first example, start with [README.md](README.md).
This guide covers loading languages, queries, resource reuse, and compatibility.
See the [API reference](https://pkg.go.dev/github.com/zema1/wasitter) for the
complete interface.

## Loading a language

Download the language's `.wasm` file from the
[Release](https://github.com/zema1/wasitter/releases) matching your Go dependency
and place it in `grammars/`. The following example is a complete program.

### From a file

This complete example loads the downloaded Python module:

```go
package main

import (
	"context"
	"fmt"
	"log"

	wasitter "github.com/zema1/wasitter"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	parser, runtime, err := wasitter.NewParserFromFile(ctx, "grammars/wasitter-python.wasm")
	if err != nil {
		return err
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.ParseContext(ctx, []byte("print('hello')\n"), nil)
	if err != nil {
		return err
	}
	defer tree.Close()
	fmt.Println(tree.RootNode().Type())     // module
	fmt.Println(tree.RootNode().HasError()) // false
	return nil
}
```

`NewParserFromFile` reads the grammar from the WASM module and returns a ready
to use parser in one call. To use another language, load that language's WASM
file. The module contents determine the grammar, not the filename. Built-in
helpers only use embedded modules and do not download or search for external
files.

### Embed the WASM in your application

Copy your chosen WASM into your Go package's `grammars/` directory. Add the
following declaration, then replace the file constructor in the example with
`wasitter.NewParserFromWASM(ctx, pythonWASM)`. The remaining steps are identical:

```go
import _ "embed"

//go:embed grammars/wasitter-python.wasm
var pythonWASM []byte
```

Your compiled executable then needs no external WASM file at runtime.

Use modules built for **wasitter**. Grammar-only `.wasm` files intended for
`web-tree-sitter` do not include wasitter's runtime and ABI bridge and cannot
be loaded directly. To add another language, see
[adding a grammar](DEVELOPMENT.md#building-wasm-artifacts).

### Custom runtime configuration

For compilation caches, custom wazero options, or selecting a particular
export in a module with multiple languages, use the lower-level constructors:
`NewRuntimeWithOptions`, `runtime.Language()` (default language) or
`runtime.LoadLanguage(name)` (named export), `NewParserWithRuntime`, and
`parser.SetLanguage(language)`. Close the language wrapper after setting it;
the parser keeps its own reference.

The convenience constructors return both parser and runtime, like the built-in
helpers. Closing a parser does not close its runtime, so its trees remain usable
until you close them and then the runtime. If construction fails, both returned
pointers are nil and resources created by the constructor are released.

## Querying source code

After parsing the JavaScript source from the README, this query captures
function names. Insert it before `return nil` in that example's `run` function:

```go
query, err := wasitter.NewQuery(parser.Language(), `
    (function_declaration name: (identifier) @name)
`)
if err != nil {
	return err
}
defer query.Close()

cursor := wasitter.NewQueryCursor()
defer cursor.Close()
matches, err := cursor.Matches(query, tree.RootNode(), source)
if err != nil {
	return err
}
for _, match := range matches {
	for _, capture := range match.Captures {
		fmt.Println(capture.Node.Content(source)) // greet
	}
}
```

Queries use Tree-sitter's pattern syntax. Node types and field names come from
the selected language's grammar. Supply the source bytes when executing queries
so text predicates such as `#eq?` and `#match?` can evaluate captured text.

## Incremental parsing

To reuse an existing tree after an edit, call `tree.Edit` with an `InputEdit`
that describes the changed byte range and positions. Then pass the edited tree
to `parser.ParseContext(ctx, updatedSource, tree)`. Keep both trees open while
comparing them, and close each when finished. Passing an old tree without
applying its edits does not describe the new source correctly.

## Reading nodes

Nodes provide type names, byte ranges, children, and fields. Using `tree` from
the README example:

```go
root := tree.RootNode()
fmt.Println(tree.ToSExpression())
for i := 0; i < root.NamedChildCount(); i++ {
	child := root.NamedChild(i)
	fmt.Println(child.Type(), child.StartByte(), child.EndByte())
}
```

A successful parse can produce a tree containing syntax errors. Check
`root.HasError()` when valid input is required. Nodes borrow their tree, so
keep it open while reading nodes. Use `ParseContext` for byte slices,
`ParseInputContext` for callbacks, and their `WithOptionsContext` variants
when progress options are needed.

## Lifecycle and performance

Close trees, queries, cursors, parsers, and languages before their runtime.
Trees may outlive the parser that created them, but the runtime must remain
open. `Close` is idempotent.

For batches of files, create a parser/runtime once and reuse it. Close each
file's tree promptly so its guest nodes are reclaimed:

```go
parser, runtime, err := wasitter.NewJavaScriptParser(ctx)
if err != nil {
	return err
}
defer runtime.Close()
defer parser.Close()
for _, source := range sources {
	tree, err := parser.ParseContext(ctx, source, nil)
	if err != nil {
		return err
	}
	// Read the tree here; retain it only if further work needs its nodes.
	if err := tree.Close(); err != nil {
		return err
	}
}
```

A Runtime serializes guest execution, including calls from separate parsers.
For parallel processing, give each worker its own Runtime and Parser. Workers
can share a `wazero.CompilationCache` via
`wazero.NewRuntimeConfig().WithCompilationCache(cache)` in
`RuntimeOptions.RuntimeConfig` when constructing runtimes
with `NewRuntimeWithOptions`; keep that cache open until all workers stop.
A compilation cache saves compilation work, while reusing a runtime also saves
module instantiation and linear-memory allocation. WASM memory keeps its high
water mark, so retire a worker's runtime after unusually large jobs if needed.

`ChildByFieldName` caches successful field IDs per guest language and uses the
numeric accessor on bridges that support it. Existing traversal code benefits
automatically; older bridges retain name-based lookup. Code with fixed fields
can also resolve `Language.FieldIDForName` once and use `ChildByFieldID`.
Field IDs belong to their language and must not be reused across different
grammars.

## Compatibility

The guest runtime is Tree-sitter C v0.25.0. The checked-in JSON grammar is
v0.24.8 and the checked-in JavaScript grammar is v0.25.0. Releases provide
the other supported grammars as individual WASM files.
WASM execution is provided by [wazero](https://wazero.io/), with WASI preview-1
enabled for the standard C runtime imports.

The WASM module contains the same upstream C parser and query engine as those
versions, but the Go/WASM boundary is an explicit ABI.  Consequently this
project aims for semantic parity, rather than promising bit-for-bit or
100%-identical behavior with every native Tree-sitter build:

- A grammar must be generated against a compatible Tree-sitter language ABI
  (13 through 15 for the bundled runtime). Use grammar files from the wasitter
  release matching your Go module version.
- Tree coordinates exposed by this package are UTF-8 byte offsets and byte
  columns. `ParseUTF16` decodes the supplied Go `[]uint16` into
  UTF-8 before parsing, so offsets in the returned tree refer to that UTF-8
  representation (they are not UTF-16 code-unit offsets).
- Input callbacks are materialized into one UTF-8 buffer before entering the
  guest. The progress callback is a deterministic pre/post hook; the compact
  ABI does not currently provide a callback trampoline for observing every
  parser step. Context cancellation is additionally bridged through
  Tree-sitter's native cancellation flag when the module exports it (the
  bundled module does); runtimes configured with
  `WithCloseOnContextDone` can interrupt guest execution even earlier.
- `SetLogger` retains a Go logger for API compatibility. The bundled ABI does
  not yet expose a native Tree-sitter logger trampoline, so parser diagnostics
  are not automatically forwarded to that callback. A custom bridge may add
  its own logging export.
- Native query matching is delegated to Tree-sitter. Built-in text predicates
  (`#eq?`, `#match?`, and related forms) are evaluated by the Go host because
  source bytes live outside the guest query engine; user-defined predicates
  remain caller-defined. Modules that predate the native query ABI use a
  deliberately small fallback matcher (simple node-type/capture patterns),
  and therefore do not provide the full query language semantics.

These restrictions keep the wire format portable and predictable. They should
be treated as part of the module contract when comparing results with a native
binding or when shipping a custom grammar module.

## Security and trust boundaries

The package executes a caller-selected WebAssembly module. `NewRuntime` does
not verify that a module was produced by this repository; applications loading
modules from outside the checked-in assets should authenticate and pin those
bytes themselves. wazero provides the execution sandbox, and the default
runtime only installs WASI preview-1 imports needed by the Tree-sitter C
runtime. Supplying a custom `RuntimeConfig`, `ModuleConfig`, or import
configuration can expand that boundary and should be reviewed accordingly.

Malformed modules, grammars, and queries are expected to return Go errors, but
resource limits remain an application concern. Set a context deadline or use
`WithCloseOnContextDone` for untrusted or potentially expensive input, and
bound input sizes before parsing. The package does not promise protection from
denial-of-service caused by deliberately large inputs or pathological grammar
queries.
