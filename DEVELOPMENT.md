# Development

For library usage and downloading grammars, see [README.md](README.md).

## Setup

Use Go 1.23 or newer, [mise](https://mise.jdx.dev/), and Docker.
Native comparison tests also require a C/C++ compiler and CGO.
The public library and build command remain CGO-free.

## Building WASM artifacts

`mise` is the canonical entry point. Every build runs the compiler inside a
pinned Docker image, refreshes the adjacent SHA-256 file, and leaves the WASM
artifact in the repository. The task delegates orchestration to the
CGO-free `cmd/wasitter-build` Go command, so registry parsing and Docker
argument handling do not depend on a host shell:

```sh
mise run wasm-build       # Docker build + WASM generation + checksum update
mise run wasm-verify      # verify the checked-in artifact only
mise run wasm-check       # rebuild, verify, and run fixture checks
```

The command can also be invoked directly while developing the build workflow:

```sh
go run ./cmd/wasitter-build build-grammar javascript
```

To build an official grammar selected by name, pass one argument:

```sh
mise run build:grammar javascript  # writes internal/wasm/assets/wasitter-javascript.wasm
mise run test:grammar javascript   # verify the artifact, smoke-test it, and run native parity
mise run check:grammar javascript  # build followed by the two checks above
```

`build:grammar` downloads the exact release and source-archive digest recorded
in [`scripts/grammar-registry.json`](scripts/grammar-registry.json), then
invokes the same ABI bridge used by the bundled JSON artifact. The output files
are `internal/wasm/assets/wasitter-<language>.wasm` and its `.sha256` sidecar;
commit both files when the grammar is intended to be part of the package.
`go:embed` discovers every checked-in `wasitter-*.wasm` file, and
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
`internal/wasm/assets/wasitter-json.wasm.sha256`). The Go command links
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

## Tests and benchmarks

The test suite compares the bundled parser's corpus and representative query
results with Tree-sitter's expected S-expressions, and also exercises
incremental edits, UTF-8 boundaries, cursors, lifecycle errors, and concurrent
use. Run it with:

```sh
go test ./...
CGO_ENABLED=0 go test ./...
go test -race ./...
go vet ./...
```

Parser, query, and runtime benchmarks are included in the repository:

```sh
go test -run '^$' -bench . -benchmem ./...
```

`BenchmarkGeneratedGrammarParse` automatically creates one WASM parse
sub-benchmark for every checked-in `wasitter-*.wasm` artifact. This keeps
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

## Release assets

The release workflow builds **every grammar in the registry**, including JSON
and JavaScript, and uploads each WASM as an individual Release attachment.
External grammars stay outside the embedded assets, so adding release languages
does not increase the Go library's embedded size.

From the checkout you intend to release:

```sh
go test ./...
CGO_ENABLED=0 go test ./...
go test -race ./...
go vet ./...
mise run wasm-check
mise run native-test
mise run release:build
mise run release:check
mise run release:prepare
```

Run the three release tasks in order:

- `release:build` uses the pinned Docker builder to build all grammars into
  `.tmp/release/grammars/`. It exports licenses from each verified source
  archive, including nested dependency notices, and saves the registry snapshot.
  The final directory appears only after every build and checksum check succeeds.
- `release:check` parses representative inputs and compares all staged grammars
  with their pinned native bindings (trees, node fields, and query results).
  New release grammars need fixtures and native bindings in `comparison/`.
- `release:prepare` verifies the complete grammar set, registry snapshot,
  checksums, WASM headers, and licenses. It writes individual files into
  `dist/grammars/`; it does not execute modules, so run `release:check` first.

Build and preparation reject existing output directories to prevent stale files
from entering a release. Before repeating them, remove the generated
`.tmp/release/grammars/` and `dist/grammars/` directories, or invoke
`build-grammars -output-dir <path>` and
`prepare-grammar-assets -artifact-dir <path> -output-dir <path>` directly.
Build output must be inside the checkout. `WASITTER_GRAMMAR_DIR` selects an
external artifact directory for smoke and native tests; normal tests still
use embedded artifacts.

The prepared Release attachments are:

- `wasitter-<language>.wasm` and its `.wasm.sha256` for every grammar.
- `SHA256SUMS` for verifying all downloaded WASM files at once.
- `grammar-registry.json` with exact source versions and archive digests.
- `THIRD_PARTY_NOTICES.txt`, combining the project, runtime, grammar, and
  toolchain license texts, including nested dependency notices.

To verify downloaded or prepared assets, run the checksum command in their
directory. For one language, use `sha256sum -c wasitter-python.wasm.sha256`;
for the complete set, use `sha256sum -c SHA256SUMS`. On macOS, substitute
`shasum -a 256 -c` for `sha256sum -c`.

Before publishing, review public compatibility notes, grammar pins and licenses,
run the checks above and `mise run native-bench`, inspect the prepared files,
and write release notes. Rebuild after changing any build inputs; comparing
registry snapshots alone does not detect runtime or compiler bridge changes.

[The release workflow](.github/workflows/release.yml) follows the build/artifact/
release structure used by suo5:

| Event | Behavior |
| --- | --- |
| Push any tag | Build, validate, and save the `grammar-assets` Actions artifact. |
| Publish a Release (including a prerelease) | Build and validate its tagged revision, then upload individual files to that Release using `softprops/action-gh-release`. |
| Manual `workflow_dispatch` | Build the selected ref and save the Actions artifact. |

Pushing a tag does not create a Release. Publishing a Release after pushing its
tag triggers a separate build, so uploads do not depend on an earlier Actions
artifact or its retention period. Event-specific concurrency groups keep the
tag and Release runs from cancelling or displacing each other. The publish
job alone has `contents: write`; build jobs have read-only permissions.
Rerunning the Release workflow replaces same-named attachments, as with suo5.
The workflow creates no grammar ZIP; GitHub's Actions artifact download may
still wrap the files in an archive for transport.
