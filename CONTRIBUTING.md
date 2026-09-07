# Contributing

Thanks for improving wasitter. Changes should preserve the behavior of the
upstream Tree-sitter runtime where the WebAssembly ABI allows it, and should
keep the ordinary package build free of CGO.

## Development setup

Install Go 1.23 or newer, [mise](https://mise.jdx.dev/), and Docker. The host
does not need Zig, wasi-sdk, or a C compiler. Clone the repository and run the
default tests before changing code:

```sh
go test ./...
CGO_ENABLED=0 go test ./...
go test -race ./...
go vet ./...
```

Run `gofmt` on changed Go files. A focused test is useful while iterating, but
pull requests should include the complete checks that apply to the changed
surface. Changes to parser, query, cursor, encoding, or incremental parsing
should also run `mise run native-test`.

## WebAssembly artifacts and grammars

Files under `internal/wasm/assets/` are generated release inputs. Do not edit a
`.wasm` file or its `.sha256` sidecar by hand. Use the pinned tasks instead:

```sh
mise run wasm-check
mise run check:grammar javascript
```

Commit each generated artifact together with its checksum. A new official
grammar must first be added to `scripts/grammar-registry.json` with its
repository, release tag, archive SHA-256, parser and scanner paths, C language
constructor, and exported language name. Keep one reviewed registry object per
grammar, including when several grammars live in one upstream repository.

When updating Tree-sitter, Zig, wasi-libc, or a grammar release, update the
compatibility notes and `internal/wasm/THIRD_PARTY_NOTICES.md` in the same
change. Run `mise run wasm-verify` after checking in artifacts so stale
sidecars are caught locally.

## Pull requests

Describe the user-visible behavior and the reason for the change. Include
parity or regression tests for semantic changes, and mention any generated
artifacts or version pins in the pull request description. Keep unrelated
formatting or dependency updates out of the change so that ABI and grammar
reviews remain auditable.

The CI workflow runs CGO-free tests, race tests, cross-compilation checks, WASM
verification, native parity, and benchmark smoke tests. A maintainer may ask
for additional grammar or platform checks when a change affects those paths.

## Release checklist

Before tagging a release, a maintainer should:

1. Run `go test ./...`, `CGO_ENABLED=0 go test ./...`, `go test -race ./...`,
   and `go vet ./...`.
2. Run `mise run wasm-check`, `mise run native-test`, and the native benchmark
   smoke test.
3. Verify every checked-in `wasitter-*.wasm` file has its matching checksum
   and that registry pins and third-party notices describe the shipped files.
4. Review the public compatibility notes in `README.md` and record notable
   API, grammar, or runtime changes in the release notes.
