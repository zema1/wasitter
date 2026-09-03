# Contribution notes

## Build and test

- Use `mise run wasm-build` to regenerate the checked-in JSON WASM artifact.
- Use `mise run build:grammar <language>` to generate a pinned official
  grammar, `mise run test:grammar <language>` to validate an existing artifact,
  and `mise run check:grammar <language>` to do both.
- The parameterized tasks require a row in `scripts/grammar-registry.tsv`.
  Keep repository/tag/archive SHA, parser/scanner paths, and C language entry
  points complete and reviewed; separate multiple scanner paths with spaces and
  add one row per grammar in multi-grammar repos.
- Commit the generated `.wasm` and adjacent `.wasm.sha256` together. The
  artifact test discovers checked-in files automatically; native parity also
  needs a matching binding and fixture in `comparison/`.
- Use `mise run wasm-check` before changing or publishing the artifact; it
  rebuilds in Docker, updates the checksum, and runs fixture checks.
- Use `mise run wasm-verify` when only validating the existing artifact.
- Run `go test ./...`, `CGO_ENABLED=0 go test ./...`, and `go test -race ./...`
  for Go changes. Run `mise run native-test` for Tree-sitter parity changes.
- `BenchmarkGeneratedGrammarParse/<language>` discovers checked-in grammar
  artifacts automatically; a new language needs no benchmark code (an empty
  input is the temporary baseline until a fixture is added).
- Do not add a Makefile or make-based workflow; `mise.toml` is the task entry
  point.

## WASM and compatibility

- The Docker builder is pinned in `docker/wasm-builder/Dockerfile`; keep the
  Zig and base-image checksums synchronized when upgrading it.
- Keep the vendored runtime, generated grammar, ABI bridge, Go ABI constants,
  and comparison module on compatible Tree-sitter versions.
- Treat `internal/wasm/assets/*.wasm` and its `.sha256` file as generated
  release inputs. Never edit them manually or commit a stale checksum.
- The public package must remain CGO-free. Keep wazero and all guest-memory /
  handle lifecycle operations behind the existing Go API.

## Changes and review

- Preserve upstream Tree-sitter semantics where practical, while returning
  idiomatic Go errors and keeping `Close` idempotent.
- Add or update parity tests when parser, query, cursor, encoding, or
  incremental-parse behavior changes.
- Update README and `internal/wasm/THIRD_PARTY_NOTICES.md` when versions,
  build inputs, or licensing information changes.
