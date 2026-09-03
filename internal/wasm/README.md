# Tree-sitter WASM artifacts

Each checked-in `assets/sitterwasm-<language>.wasm` contains the upstream
Tree-sitter C runtime (v0.25.0), one generated grammar, and the small
`sitterwasm_abi.c` adapter. The JSON artifact uses tree-sitter-json v0.24.8;
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

The task reads `scripts/grammar-registry.tsv`, downloads the exact tagged
source archive in Docker, verifies its archive SHA-256, compiles the parser and
any listed external scanner, and writes both the WASM file and its adjacent
`.sha256` file under `assets/`. See the root README for the current registry
and the procedure for adding a row. A row is intentionally required for each
grammar so builds remain reviewable and reproducible; repositories containing
multiple grammars get one explicit row per grammar.

The fixed `wasm-build` task runs `scripts/build-wasm.sh` inside the pinned
Docker image in `docker/wasm-builder/`, updates the adjacent checksum, and uses
only checked-in files under `internal/wasm/third_party` for the default build.
The
host does not need Zig, wasi-sdk, or a C compiler. Repeated builds with the
same image and sources are byte-for-byte reproducible. The directory
deliberately avoids Go's special `vendor` name so these sources and their
licenses remain in published module zip files.

The low-level `WASM_CC=/path/to/zig ./scripts/build-wasm.sh` command remains
available for debugging or environments that intentionally do not use Docker.

The checked-in artifacts were built with Zig 0.15.2 in the pinned builder
image. Their SHA-256 digests are recorded in the sidecars
`assets/sitterwasm-json.wasm.sha256` and
`assets/sitterwasm-javascript.wasm.sha256`. Run `mise run wasm-verify` (or
`../../scripts/verify-wasm.sh`) to validate the JSON digest without rebuilding;
`mise run test:grammar <language>` validates any selected grammar artifact
offline before running its semantic parity checks.
Builds are published through
an atomic sibling-file rename, so a compiler failure leaves an existing output
untouched.
See [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for the source and statically
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

To build a different grammar, replace `third_party/tree-sitter-json/parser.c`
and define `SITTERWASM_LANGUAGE_FN` (and optionally
`SITTERWASM_LANGUAGE_NAME`) in the bridge compile flags. Grammars with external
scanners may add C or C++ scanner sources to the three-object link in
`scripts/build-wasm.sh`. Parser and scanner extensions `.cc`, `.cpp`, `.cxx`,
and `.C` select the C++ frontend and linker; set `WASM_CXX` explicitly when the matching
wasi-sdk `clang++` cannot be inferred from `WASM_CC`. C++ scanner compilation
disables exceptions and RTTI, matching the constraints normally used by
Tree-sitter WASM grammars.
`RUNTIME_SRC_DIR`, `GRAMMAR_SRC_DIR`, and `GRAMMAR_SRC` can be used to point at
repository-relative or absolute replacement sources; relative paths are
resolved from the project root, regardless of the caller's working directory.
When `GRAMMAR_SRC` (or an entry in `GRAMMAR_EXTRA_SRC`) is not found at that
root-relative path, it is also tried relative to `GRAMMAR_SRC_DIR`, so a short
`GRAMMAR_SRC=parser.c` works naturally with an out-of-tree grammar directory.
Single source, output, and temporary-directory paths are passed to the
compiler without shell word splitting, including paths containing spaces. The
documented `GRAMMAR_EXTRA_SRC` value remains a whitespace-separated list, so
individual entries in that list should use whitespace-free paths. The grammar
registry uses the same representation when a grammar has multiple scanners.

The standard `mise run wasm-build` task builds the checked-in JSON fixture.
Parameterized official grammar tasks use the registry workflow above. Sources
outside the checkout are intended for the low-level script and must be
provided through its documented source variables.

The vendored source licenses are documented in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
