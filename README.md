# sitterwasm

Tree-sitter for Go, backed by a portable `wasm32-wasi` module. The package
embeds the upstream Tree-sitter C runtime and a JSON grammar, so ordinary Go
builds do not require CGO, a C compiler, or a platform-specific shared
library.

## Install

```sh
go get github.com/zema1/sitterwasm@latest
```

The module requires Go 1.23 or newer. Its ordinary build path is pure Go and
can be cross-compiled with `CGO_ENABLED=0`.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"

	sitterwasm "github.com/zema1/sitterwasm"
)

func main() {
	ctx := context.Background()
	parser, runtime, err := sitterwasm.NewJSONParser(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer runtime.Close()
	defer parser.Close()

	tree, err := parser.Parse([]byte(`{"ok": true}`), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer tree.Close()
	fmt.Println(tree.RootNode().Type()) // document
}
```

`NewRuntime` accepts caller-supplied grammar module bytes; `NewLanguage` or
`Runtime.LoadLanguage` then resolves the grammar exported by that runtime. The
public API uses Go values (`Point`, `Range`, `InputEdit`) while keeping
Tree-sitter's byte offsets and node semantics.

## Building the fixture

`mise` is the canonical entry point. The build task runs the compiler inside a
pinned Docker image, refreshes the adjacent SHA-256 file, and leaves the WASM
artifact in the repository:

```sh
mise run wasm-build       # Docker build + WASM generation + checksum update
mise run wasm-verify      # verify the checked-in artifact only
mise run wasm-check       # rebuild, verify, and run fixture checks
```

The host therefore needs only `mise` and Docker; it does not need Zig,
wasi-sdk, a C compiler, or CGO. The image is based on a pinned Alpine digest
and downloads Zig 0.15.2 with a verified SHA-256 checksum. Docker caches the
builder image, so subsequent builds do not redownload the toolchain.

The low-level [`scripts/build-wasm.sh`](scripts/build-wasm.sh) driver remains
available for advanced users who intentionally provide a compiler on the host;
normal development and release builds should use the mise task above. It
accepts its existing variables for custom grammar experiments, but those are
outside the fixed release workflow. See
[`internal/wasm/README.md`](internal/wasm/README.md) for the ABI and custom
grammar build notes (including out-of-tree grammars and external scanners),
and
[`internal/wasm/THIRD_PARTY_NOTICES.md`](internal/wasm/THIRD_PARTY_NOTICES.md)
for third-party license information.

The expected SHA-256 is kept beside the artifact in
`internal/wasm/assets/sitterwasm-json.wasm.sha256`. Both the Docker wrapper and
the low-level build script link through temporary sibling files and publish
only after a successful link, so a failed build cannot truncate an existing
artifact.

## Building a custom grammar

For a custom grammar, invoke the low-level script directly with its documented
source variables. The standard `mise` task intentionally builds only the
checked-in JSON fixture, which keeps release output deterministic:

```sh
GRAMMAR_SRC_DIR=/absolute/path/to/tree-sitter-python/src \
GRAMMAR_SRC=parser.c \
GRAMMAR_EXTRA_SRC=scanner.c \
SITTERWASM_LANGUAGE_FN=tree_sitter_python \
SITTERWASM_LANGUAGE_NAME=python \
OUT=./tree-sitter-python.wasm \
./scripts/build-wasm.sh
```

Omit `GRAMMAR_EXTRA_SRC` for grammars without an external scanner. Files ending
in `.cc`, `.cpp`, `.cxx`, or `.C` are compiled and linked as C++; when using
wasi-sdk, set `WASM_CXX=/path/to/clang++` if it is not next to `WASM_CC`.
As in Tree-sitter's own WASM builds, C++ scanners are compiled without
exceptions or RTTI. Load the result with `NewRuntime`, resolve it with
`Runtime.LoadLanguage("python")`, then create a parser with
`NewParserWithRuntime` and call `SetLanguage`. The grammar's generated language
ABI must be supported by the bundled runtime.

### Updating Tree-sitter or the grammar

Keep the runtime sources, generated grammar sources, and native comparison
module on compatible Tree-sitter releases. After replacing the vendored
runtime or regenerating a grammar, run `mise run wasm-check`; it rebuilds the
module in Docker, updates `*.wasm.sha256`, and exercises the ABI fixture. Then
run `mise run native-test` and `mise run native-bench` before committing the new WASM
file, checksum, version notes, and any license changes. If a new runtime
introduces a language ABI outside the supported range, update the ABI bridge
and the Go constants in `language.go` together with the compatibility tests.

## Compatibility

The guest runtime is Tree-sitter C v0.25.0 and the bundled JSON grammar is
v0.24.8. WASM execution is provided by [wazero](https://wazero.io/), with WASI
preview-1 enabled for the standard C runtime imports.

The WASM module contains the same upstream C parser and query engine as those
versions, but the Go/WASM boundary is an explicit ABI.  Consequently this
project aims for semantic parity, rather than promising bit-for-bit or
100%-identical behavior with every native Tree-sitter build:

- A grammar must be generated against a compatible Tree-sitter language ABI
  (13 through 15 for the bundled runtime).  The embedded artifact contains
  only the JSON grammar; use `scripts/build-wasm.sh` to produce an artifact for
  another grammar.
- Tree coordinates exposed by this package are UTF-8 byte offsets and byte
  columns. `ParseUTF16LE`/`ParseUTF16BE` decode the supplied Go `[]uint16` into
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

## Tests and benchmarks

The test suite compares the bundled parser's corpus and representative query
results with Tree-sitter's expected S-expressions, and also exercises
incremental edits, UTF-8 boundaries, cursors, lifecycle errors, and concurrent
use. Run it with:

```sh
go test ./...
go test -race ./...
```

Parser, query, and runtime benchmarks are included in the repository:

```sh
go test -run '^$' -bench . -benchmem ./...
```

CI also runs a one-iteration benchmark smoke test and the native benchmark
suite. These checks keep the benchmark entry points executable; reported
numbers vary by host and are not a fixed performance promise.

For an apples-to-apples native comparison, the optional `comparison/` module
pins the upstream Go binding and JSON grammar (and is intentionally separate
because it requires CGO):

```sh
mise run native-test
mise run native-bench
```
