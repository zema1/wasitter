# sitterwasm

Tree-sitter for Go, backed by a portable `wasm32-wasi` module. The package
embeds the upstream Tree-sitter C runtime and checked-in grammar artifacts, so
ordinary Go builds do not require CGO, a C compiler, or a platform-specific
shared library. JSON is bundled by default; additional official grammars can
be generated and committed with the same workflow.

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
`Runtime.LoadLanguage` then resolves the grammar exported by that runtime. For
checked-in artifacts, `NewBuiltinRuntime(ctx, "javascript")` and
`NewJavaScriptParser(ctx)` are convenient equivalents to the JSON helpers.
The public API uses Go values (`Point`, `Range`, `InputEdit`) while keeping
Tree-sitter's byte offsets and node semantics.

## Building WASM artifacts

`mise` is the canonical entry point. Every build runs the compiler inside a
pinned Docker image, refreshes the adjacent SHA-256 file, and leaves the WASM
artifact in the repository. The task delegates orchestration to the
CGO-free `cmd/sitterwasm-build` Go command, so registry parsing and Docker
argument handling do not depend on a host shell:

```sh
mise run wasm-build       # Docker build + WASM generation + checksum update
mise run wasm-verify      # verify the checked-in artifact only
mise run wasm-check       # rebuild, verify, and run fixture checks
```

The command can also be invoked directly while developing the build workflow:

```sh
go run ./cmd/sitterwasm-build build-grammar javascript
```

To build an official grammar selected by name, pass one argument:

```sh
mise run build:grammar javascript  # writes internal/wasm/assets/sitterwasm-javascript.wasm
mise run test:grammar javascript   # verify the artifact, smoke-test it, and run native parity
mise run check:grammar javascript  # build followed by the two checks above
```

`build:grammar` downloads the exact release and source-archive digest recorded
in [`scripts/grammar-registry.json`](scripts/grammar-registry.json), then
invokes the same ABI bridge used by the bundled JSON artifact. The output files
are `internal/wasm/assets/sitterwasm-<language>.wasm` and its `.sha256` sidecar;
commit both files when the grammar is intended to be part of the package.
`go:embed` discovers every checked-in `sitterwasm-*.wasm` file, and
`BuiltinWASM("<language>")` can retrieve it at runtime.

The registry uses JSON rather than YAML so the CGO-free build command can use
Go's standard library without adding a parser dependency; the versioned object
and arrays remain easy to review and edit.

The registry is a deliberately pinned allowlist rather than an unversioned
"latest" downloader. It currently covers:

| Language | Upstream release | External scanner |
| --- | --- | --- |
| bash | tree-sitter-bash v0.25.1 | C |
| c | tree-sitter-c v0.24.2 | — |
| cpp | tree-sitter-cpp v0.23.4 | C |
| go | tree-sitter-go v0.25.0 | — |
| java | tree-sitter-java v0.23.5 | — |
| javascript | tree-sitter-javascript v0.25.0 | C |
| json | tree-sitter-json v0.24.8 | — |
| python | tree-sitter-python v0.25.0 | C |
| ruby | tree-sitter-ruby v0.23.1 | C |
| rust | tree-sitter-rust v0.24.2 | C |
| tsx | tree-sitter-typescript v0.23.2 | C |
| typescript | tree-sitter-typescript v0.23.2 | C |

To add another official grammar, add a fully pinned object (repository, release
tag, archive SHA-256, parser path, optional `scanners` path array, C language
function, and exported language name) to the `grammars` array in
[`scripts/grammar-registry.json`](scripts/grammar-registry.json), then run
`mise run check:grammar <name>`. Repos that contain multiple grammars (such as
TypeScript/TSX), C++ scanners, multiple scanner files, or non-standard
generated-source layouts need explicit objects and source paths. The registry
loader validates the schema, rejects duplicate names and unsafe paths, and
only permits official Tree-sitter repositories.
The build intentionally fails for names absent from the registry so a typo or
an unreviewed network dependency cannot silently produce a release artifact.

The host therefore needs Go, `mise`, and Docker; it does not need Zig, wasi-sdk,
a C compiler, or CGO. The Go command builds a small static helper, mounts it
with the checkout, and performs the source download, archive verification,
extraction, and compilation inside the pinned container. The image is based on
a pinned Alpine digest and downloads Zig 0.15.2 with a verified SHA-256
checksum. Docker caches the builder image, so subsequent builds do not
redownload the toolchain.

The Go command is the only supported build implementation. It receives
registry metadata as structured values and invokes the compiler directly inside
Docker; no compiler or source-path environment variables are required. See
[`internal/wasm/README.md`](internal/wasm/README.md) for the ABI and custom
grammar registry notes (including multi-file external scanners),
and
[`internal/wasm/THIRD_PARTY_NOTICES.md`](internal/wasm/THIRD_PARTY_NOTICES.md)
for third-party license information.

The expected SHA-256 is kept beside each artifact (for example,
`internal/wasm/assets/sitterwasm-json.wasm.sha256`). The Go command links
through temporary sibling files and publishes only after a successful link, so
a failed build cannot truncate an existing artifact.

Private or otherwise unregistered grammars are intentionally outside the
release workflow. Add an explicitly reviewed official grammar object to
[`scripts/grammar-registry.json`](scripts/grammar-registry.json) before
building it; this keeps source pins, scanner paths, and language entry points
auditable. The generated artifact can then be loaded with `NewRuntimeFromFile`
or `NewRuntime` and `Runtime.LoadLanguage`.

### Updating Tree-sitter or a grammar

Keep the runtime sources, generated grammar sources, registry object, and native
comparison module on compatible Tree-sitter releases. For a registry grammar,
run `mise run check:grammar <language>`; it downloads and builds in Docker,
refreshes the sidecar digest, and exercises both the WASM smoke test and native
parity. Then run `go test ./...`, `mise run native-test`, and
`mise run native-bench` before committing the WASM file, checksum, version
notes, and license changes. If a new runtime introduces a language ABI outside
the supported range, update the ABI bridge and the Go constants in
`language.go` together with the compatibility tests.

## Compatibility

The guest runtime is Tree-sitter C v0.25.0. The checked-in JSON grammar is
v0.24.8 and the checked-in JavaScript grammar is v0.25.0; other registry
grammars are generated on demand and become package assets only when committed.
WASM execution is provided by [wazero](https://wazero.io/), with WASI preview-1
enabled for the standard C runtime imports.

The WASM module contains the same upstream C parser and query engine as those
versions, but the Go/WASM boundary is an explicit ABI.  Consequently this
project aims for semantic parity, rather than promising bit-for-bit or
100%-identical behavior with every native Tree-sitter build:

- A grammar must be generated against a compatible Tree-sitter language ABI
  (13 through 15 for the bundled runtime). The embedded artifacts are the
  checked-in files under `internal/wasm/assets`; use
  `mise run build:grammar <language>` for a pinned official grammar.
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

`BenchmarkGeneratedGrammarParse` automatically creates one WASM parse
sub-benchmark for every checked-in `sitterwasm-*.wasm` artifact. This keeps
new registry grammars visible in performance runs without adding per-language
benchmark code. Filter to one artifact when iterating on it:

```sh
go test -run '^$' -bench '^BenchmarkGeneratedGrammarParse/javascript$' -benchmem .
```

Runtime and parser setup is outside the timed region; each iteration parses the
grammar's representative fixture and closes the result tree. Grammars without
a fixture use an empty input as a baseline until a language-specific fixture
is added. These are WASM-only throughput measurements; the `comparison/`
module remains the place for apples-to-apples native timing and requires CGO.

CI also runs a one-iteration benchmark smoke test and the native benchmark
suite. These checks keep the benchmark entry points executable; reported
numbers vary by host and are not a fixed performance promise.

For an apples-to-apples native comparison, the optional `comparison/` module
pins the upstream Go bindings for the registry grammars (and is intentionally
separate because it requires CGO):

```sh
mise run native-test
mise run native-bench
```

`mise run test:grammar <language>` first checks that the requested registry object,
WASM file, and checksum all exist (this part is offline and does not require
Docker). It then runs a small parse smoke test in the CGO-free module and
compares S-expressions, node metadata, tree shape, and a wildcard query against
the corresponding upstream native Go binding. This is a semantic parity check;
upstream release archives and compiler toolchains are not expected to produce
byte-for-byte identical WASM modules.
