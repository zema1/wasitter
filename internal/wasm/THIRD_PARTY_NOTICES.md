# Third-party notices

## Tree-sitter sources

The files under `third_party/tree-sitter` are from the Tree-sitter C runtime
(`github.com/tree-sitter/go-tree-sitter`, v0.25.0) and are distributed under
the MIT license; see [`third_party/tree-sitter/LICENSE`](third_party/tree-sitter/LICENSE).

The JSON grammar under `third_party/tree-sitter-json` is from
`github.com/tree-sitter/tree-sitter-json`, v0.24.8, also under the MIT license;
see [`third_party/tree-sitter-json/LICENSE`](third_party/tree-sitter-json/LICENSE).

The Tree-sitter source snapshot includes ICU-derived Unicode headers under
`third_party/tree-sitter/unicode`. Their Unicode/ICU notices and license terms
are preserved in
[`third_party/tree-sitter/unicode/LICENSE`](third_party/tree-sitter/unicode/LICENSE).
The portable endian compatibility header identifies itself as public domain;
its original notice is preserved in
[`third_party/tree-sitter/portable/endian.h`](third_party/tree-sitter/portable/endian.h).

`src/sitterwasm_abi.c` and `include/sitterwasm_abi.h` are original shim code
for this project and are MIT licensed with the rest of the repository.

## Embedded WASM toolchain code

The checked-in `assets/sitterwasm-json.wasm` is produced with Zig 0.15.2 by
the pinned Docker builder in `docker/wasm-builder/Dockerfile`. In addition to
the sources described above, the artifact statically links the WASI C library
and compiler runtime support selected by Zig.

The Zig license applicable to its compiler-runtime code is preserved at
[`third_party/licenses/zig/LICENSE`](third_party/licenses/zig/LICENSE).
The wasi-libc distribution is multi-licensed and incorporates separately
licensed musl and cloudlibc work. Its complete top-level license set and the
referenced third-party notices are preserved under
[`third_party/licenses/wasi-libc`](third_party/licenses/wasi-libc):

- [`LICENSE`](third_party/licenses/wasi-libc/LICENSE)
- [`LICENSE-APACHE`](third_party/licenses/wasi-libc/LICENSE-APACHE)
- [`LICENSE-APACHE-LLVM`](third_party/licenses/wasi-libc/LICENSE-APACHE-LLVM)
- [`LICENSE-MIT`](third_party/licenses/wasi-libc/LICENSE-MIT)
- [`musl-COPYRIGHT`](third_party/licenses/wasi-libc/musl-COPYRIGHT)
- [`cloudlibc-LICENSE`](third_party/licenses/wasi-libc/cloudlibc-LICENSE)
- [`emmalloc-NOTICE`](third_party/licenses/wasi-libc/emmalloc-NOTICE)
- [`dlmalloc-NOTICE`](third_party/licenses/wasi-libc/dlmalloc-NOTICE)
