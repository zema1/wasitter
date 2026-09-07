# Native parity harness

This directory is a separate Go module so the main `wasitter` module remains
CGO-free. It builds the upstream Tree-sitter Go bindings for the checked-in
grammar artifacts with CGO, then compares their parser, query, and cursor
results with the portable WASM implementation.

Run the checks from the repository root with:

```sh
mise run native-test
mise run native-bench
```

The harness intentionally pins the native dependencies to the same runtime and
grammar releases recorded in the registry (the core runtime is
`go-tree-sitter` v0.25.0). `TestGrammarParity` discovers generated artifacts
and compares each one for which a native binding and fixture are registered.
When adding a grammar, add its native module and fixture in
`grammar_parity_test.go` as part of the same change. This is a development/CI
comparison tool, not a dependency of applications importing `wasitter`.

## Benchmark methodology

Each WASM/native benchmark pair uses the same source bytes and performs the
same observable operation. Runtime, parser, tree, and query setup is outside
the timed region. Parse timings include creating and closing each result tree;
incremental-parse timings include applying one same-width edit, parsing with
the edited old tree, and closing that old tree; the benchmark alternates the
edit in both directions so every iteration can reuse the preceding result.
query timings reuse a compiled query and cursor on both sides; traversal
timings include creating and closing a cursor on each iteration. Allocation
counts therefore describe the public operation rather than one-time setup.

Benchmark results depend heavily on the Go version, CPU, and enabled CGO
toolchain. For comparisons intended for publication, collect multiple samples
on an otherwise idle machine, for example:

```sh
cd comparison
CGO_ENABLED=1 go test -run '^$' -bench . -benchmem -count=5
```
