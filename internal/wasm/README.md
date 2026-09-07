# Tree-sitter WASM artifacts

Each checked-in `assets/wasitter-<language>.wasm` contains the upstream
Tree-sitter C runtime (v0.25.0), one generated grammar, and the small
`wasitter_abi.c` adapter. The JSON artifact uses tree-sitter-json v0.24.8;
the JavaScript artifact uses tree-sitter-javascript v0.25.0. Every module is a
`wasm32-wasi` module and can be loaded by the Go package without CGO. The Go
runtime installs the WASI preview-1 imports used by the standard C clock and
diagnostics functions.

Rebuild the artifact through the repository's `mise` task (recommended):

```sh
mise run wasm-build
```

For a pinned official grammar, use the parameterized task from the repository
root:

```sh
mise run build:grammar javascript
mise run test:grammar javascript
```

The task delegates to the CGO-free `cmd/wasitter-build` helper and reads
`scripts/grammar-registry.json`. It downloads the exact tagged source archive
in Docker, verifies its archive SHA-256, compiles the parser and any listed
external scanner, and writes both the WASM file and its adjacent `.sha256` file
under `assets/`. See the root README for the current registry and the procedure
for adding an object. An object is intentionally required for each grammar so
builds remain reviewable and reproducible; repositories containing multiple
grammars get one explicit object per grammar.

The fixed `wasm-build` task runs the same Go helper inside the pinned Docker
image in `docker/wasm-builder/`; the helper invokes Zig (or an explicitly
selected clang) directly for the low-level C/C++ compile, updates the adjacent
checksum, and uses only checked-in files under `internal/wasm/third_party` for
the default build. The host needs Go, mise, and Docker, but not Zig, wasi-sdk,
or a C compiler. Repeated builds with the same image and sources are
byte-for-byte reproducible. The directory deliberately avoids Go's special
`vendor` name so these sources and their licenses remain in published module
zip files.

All build logic is implemented by `cmd/wasitter-build` and
`internal/grammarbuild`. Docker mounts a small Go helper and the checkout; the
container invokes the compiler through an argument vector, with no shell
entrypoint or host compiler environment variables.

The checked-in artifacts were built with Zig 0.15.2 in the pinned builder
image. Their SHA-256 digests are recorded in the sidecars
`assets/wasitter-json.wasm.sha256` and
`assets/wasitter-javascript.wasm.sha256`. Run `mise run wasm-verify` (or
`go run ./cmd/wasitter-build verify-wasm`) to validate the JSON digest
without rebuilding. `mise run test:grammar <language>` validates any selected
grammar artifact offline before running its semantic parity checks. Builds are
published through an atomic sibling-file rename, so a compiler failure leaves
an existing output untouched. See
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for the source and statically
linked toolchain notices that accompany it.

The adapter uses 32-bit opaque handles because all pointers in wasm32 are
32-bit. Parser and tree handles are direct guest pointers. Node handles point
to a small `TSNode` wrapper and are reclaimed by `Tree.Close` in the Go layer.
The `tsw_node_id` accessor exposes Tree-sitter's stable underlying node
identity separately from those transient wrapper handles, so repeated Go node
values compare consistently with native bindings.
Strings returned by node/language accessors are borrowed until the next call;
S-expression strings use a reusable guest buffer and must be copied by the
caller immediately.

The parser exports an optional cancellation-flag pair
(`tsw_parser_set_cancellation_flag`/`tsw_parser_cancellation_flag`). The Go
host allocates a wasm32 `size_t` cell and updates it when a parse context is
cancelled, so cancellation does not rely solely on wazero's interrupt mode.

The optional native query and cursor surface uses the same opaque-handle
convention. `tsw_query_new` receives a source pointer/length and an optional
8-byte little-endian error record (`error_offset`, `TSQueryError`). Query
cursor matches are copied into caller-provided memory: an 8-byte header
(`id`, `pattern_index`, `capture_count`) followed by 8-byte capture records
(`node_handle`, `capture_index`). `tsw_query_cursor_next_match` returns the
required byte count and retains a match when the output buffer is too small,
so hosts can size buffers with a probe call. Tree cursor handles are exposed
under both `tsw_cursor_*` and `tsw_tree_cursor_*` names.

The optional `tsw_query_cursor_next_capture_match` export follows the native
`ts_query_cursor_next_capture` stream while returning the complete originating
match (a 16-byte header containing the selected capture ordinal, followed by
the capture records). The Go host uses it to evaluate built-in text predicates
without changing capture ordering; older bridge modules may omit this export
and continue to use the compatibility path.

To build another official grammar, add a reviewed object to
`scripts/grammar-registry.json` and run `mise run check:grammar <language>`.
The object records the parser path, a `scanners` array (which may be empty),
the C language constructor, and the exported language name. Parser and scanner
paths ending in `.cc`, `.cpp`, `.cxx`, or `.C` automatically select the C++
frontend and linker; C++ compilation disables exceptions and RTTI, matching
Tree-sitter's WASM build constraints. Multiple grammars in one upstream
repository require one object per grammar and explicit source paths. Private
grammars are outside this repository's pinned release workflow.

The standard `mise run wasm-build` task builds the checked-in JSON fixture.
Parameterized official grammar tasks use the registry workflow above. Keeping
all source pins in the registry makes builds auditable and reproducible.

The vendored source licenses are documented in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
