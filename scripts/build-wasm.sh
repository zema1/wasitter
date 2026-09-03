#!/bin/sh
# Build the bundled Tree-sitter runtime and JSON grammar into one WASI module.
#
# The script intentionally uses a compiler already installed by the caller.
# Zig is preferred because `zig cc` ships a deterministic wasm32-wasi libc;
# clang from wasi-sdk is supported as well.  No package manager or network
# access is needed at build time.

set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-"$ROOT/internal/wasm/assets/sitterwasm-json.wasm"}
WASM_CC=${WASM_CC:-}
WASM_CXX=${WASM_CXX:-}
COMPILER=clang

if [ -z "$WASM_CC" ]; then
  if command -v zig >/dev/null 2>&1; then
    WASM_CC=$(command -v zig)
    COMPILER=zig
  elif command -v clang >/dev/null 2>&1; then
    WASM_CC=$(command -v clang)
  else
    echo "build-wasm: need zig or a wasi-sdk clang (set WASM_CC)" >&2
    exit 127
  fi
elif "$WASM_CC" version >/dev/null 2>&1; then
  # `zig version` succeeds; ordinary clang does not have this subcommand.
  COMPILER=zig
fi

RUNTIME="${RUNTIME_SRC_DIR:-$ROOT/internal/wasm/third_party/tree-sitter}"
GRAMMAR="${GRAMMAR_SRC_DIR:-$ROOT/internal/wasm/third_party/tree-sitter-json}"
BRIDGE="$ROOT/internal/wasm/src/sitterwasm_abi.c"
INCLUDE="$ROOT/internal/wasm/include"

# A grammar normally consists of parser.c and, optionally, one or more
# external-scanner sources.  Keep the defaults convenient for the bundled JSON
# fixture while allowing downstream projects to rebuild the same bridge for a
# generated grammar without editing this script.  GRAMMAR_SRC may be an
# absolute path or a path relative to the repository root.  For convenience,
# a relative value that is not found below the repository root is also tried
# relative to GRAMMAR_SRC_DIR (for example `GRAMMAR_SRC=parser.c` together with
# `GRAMMAR_SRC_DIR=path/to/generated`).  GRAMMAR_EXTRA_SRC is a
# whitespace-separated list of additional C or C++ sources (for example
# `src/scanner.c` or `src/scanner.cc`). C++ sources select the matching C++
# driver for both compilation and linking; set WASM_CXX when a wasi-sdk
# `clang++` cannot be inferred from WASM_CC.
GRAMMAR_SRC_INPUT=${GRAMMAR_SRC:-"$GRAMMAR/parser.c"}
GRAMMAR_EXTRA_SRC=${GRAMMAR_EXTRA_SRC:-}
SITTERWASM_LANGUAGE_FN=${SITTERWASM_LANGUAGE_FN:-tree_sitter_json}
SITTERWASM_LANGUAGE_NAME=${SITTERWASM_LANGUAGE_NAME:-json}

case "$SITTERWASM_LANGUAGE_FN" in
  ''|*[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_]*|[0-9]*)
    echo "build-wasm: SITTERWASM_LANGUAGE_FN must be a C identifier" >&2
    exit 2
    ;;
esac

# The language name becomes a quoted C string literal below. Quotes and
# backslashes can be escaped safely, but literal control characters (notably a
# newline) cannot be represented by passing one -D argument to every supported
# compiler. Reject them before any source is compiled so malformed metadata
# produces one deterministic diagnostic instead of a compiler-specific parse
# error. Shell variables cannot contain NUL; [:cntrl:] covers the remaining
# control characters without rejecting UTF-8 bytes in a minimal C locale.
case "$SITTERWASM_LANGUAGE_NAME" in
  *[[:cntrl:]]*)
    echo "build-wasm: SITTERWASM_LANGUAGE_NAME must not contain control characters" >&2
    exit 2
    ;;
esac

# Resolve relative source paths from the project root.  This keeps invocations
# from a different working directory deterministic while retaining the usual
# `GRAMMAR_SRC=path/to/parser.c` ergonomics.
resolve_source() {
  case "$1" in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s\n' "$ROOT/$1" ;;
  esac
}
# Keep the source-directory overrides consistent with GRAMMAR_SRC: callers
# often provide repository-relative paths while invoking this script from a
# different working directory.  The default values are already absolute, so
# this is a no-op for ordinary builds.
RUNTIME=$(resolve_source "$RUNTIME")
GRAMMAR=$(resolve_source "$GRAMMAR")
GRAMMAR_SRC=$(resolve_source "$GRAMMAR_SRC_INPUT")
# If the caller supplied a short source name alongside GRAMMAR_SRC_DIR, accept
# the intuitive directory-relative spelling as well.  Root-relative paths
# retain precedence, so this cannot change an existing successful invocation.
case "$GRAMMAR_SRC_INPUT" in
  /*) ;;
  *)
    if [ ! -f "$GRAMMAR_SRC" ] && [ -f "$GRAMMAR/$GRAMMAR_SRC_INPUT" ]; then
      GRAMMAR_SRC="$GRAMMAR/$GRAMMAR_SRC_INPUT"
    fi
    ;;
esac

for required in "$RUNTIME/lib.c" "$RUNTIME/include/tree_sitter/api.h" \
  "$GRAMMAR_SRC" "$BRIDGE"; do
  if [ ! -f "$required" ]; then
    echo "build-wasm: missing source: $required" >&2
    exit 1
  fi
done

BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/sitterwasm-build.XXXXXX")
TMP_OUT=
cleanup() {
  if [ -n "${TMP_OUT:-}" ] && [ -e "$TMP_OUT" ]; then
    rm -f "$TMP_OUT"
  fi
  if [ -n "${BUILD_DIR:-}" ] && [ -d "$BUILD_DIR" ]; then
    rm -rf "$BUILD_DIR"
  fi
}
trap cleanup EXIT HUP INT TERM

if [ "$COMPILER" = zig ]; then
  # Keep the target as a pair of arguments instead of embedding it in a
  # whitespace-separated flag string.  The latter is easy to accidentally
  # expand into one malformed argument when a caller supplies a path with
  # spaces (which is common on Windows and in CI workspaces).
  TARGET_FLAG="-target"
  TARGET_VALUE="wasm32-wasi"
  # Zig 0.13 parses wasm linker memory sizes as hexadecimal while newer Zig
  # releases parse decimal byte counts.  Select the spelling from `zig
  # version` so the documented toolchain range remains usable.
  ZIG_VERSION=$("$WASM_CC" version 2>/dev/null || true)
  case "$ZIG_VERSION" in
    0.13.*|0.12.*|0.11.*) MEMORY_FLAGS="-Wl,--initial-memory=0x02000000 -Wl,--max-memory=0x10000000" ;;
    *) MEMORY_FLAGS="-Wl,--initial-memory=33554432 -Wl,--max-memory=268435456" ;;
  esac
else
  TARGET_FLAG="--target=wasm32-wasi"
  TARGET_VALUE=""
  MEMORY_FLAGS="-Wl,--initial-memory=33554432 -Wl,--max-memory=268435456"
fi

# Keep these defines in sync with the upstream Tree-sitter WASM build.  The
# __EMSCRIPTEN__ compatibility path selects portable endian/clock helpers when
# compiling with wasi-sdk or Zig's clang frontend.  Do not assemble this into
# one shell string: each include path and preprocessor definition is passed as
# its own argument below, so custom grammars stored under a directory with
# spaces continue to build correctly.
#
# The language name is a C string literal. Escape the two characters that have
# meaning inside a C string before handing the single resulting argument to
# the compiler. (Tree-sitter grammar names are normally simple identifiers,
# but names such as "embedded template" are valid metadata.)
LANGUAGE_NAME_ESCAPED=$(printf '%s' "$SITTERWASM_LANGUAGE_NAME" | sed 's/\\/\\\\/g; s/"/\\"/g')
LANGUAGE_NAME_DEFINE="\"$LANGUAGE_NAME_ESCAPED\""

compile_c() {
  src=$1
  obj=$2
  # Keep every option quoted independently.  In particular, `$RUNTIME` and
  # `dirname "$GRAMMAR_SRC"` may contain spaces.
  if [ "$COMPILER" = zig ]; then
    "$WASM_CC" cc "$TARGET_FLAG" "$TARGET_VALUE" -O2 -std=c11 \
      -fvisibility=hidden -D__EMSCRIPTEN__ -D_POSIX_C_SOURCE=200112L \
      -D_DEFAULT_SOURCE "-DSITTERWASM_LANGUAGE_FN=$SITTERWASM_LANGUAGE_FN" \
      "-DSITTERWASM_LANGUAGE_NAME=$LANGUAGE_NAME_DEFINE" \
      "-I$INCLUDE" "-I$RUNTIME/include" "-I$RUNTIME" "-I$GRAMMAR" \
      "-I$(dirname "$GRAMMAR_SRC")" -c "$src" -o "$obj"
  else
    "$WASM_CC" "$TARGET_FLAG" -O2 -std=c11 -fvisibility=hidden \
      -D__EMSCRIPTEN__ -D_POSIX_C_SOURCE=200112L -D_DEFAULT_SOURCE \
      "-DSITTERWASM_LANGUAGE_FN=$SITTERWASM_LANGUAGE_FN" \
      "-DSITTERWASM_LANGUAGE_NAME=$LANGUAGE_NAME_DEFINE" \
      "-I$INCLUDE" "-I$RUNTIME/include" "-I$RUNTIME" "-I$GRAMMAR" \
      "-I$(dirname "$GRAMMAR_SRC")" -c "$src" -o "$obj"
  fi
}

resolve_cxx() {
  if [ -n "$WASM_CXX" ]; then
    return
  fi
  if [ "$COMPILER" = zig ]; then
    # Zig selects the C++ frontend through its `c++` subcommand.
    WASM_CXX=$WASM_CC
    return
  fi
  # A wasi-sdk installation keeps clang and clang++ next to each other. Use
  # that matching driver before consulting PATH, where an unrelated host
  # clang++ may otherwise be selected.
  case "$WASM_CC" in
    */*)
      cxx_candidate=$(dirname "$WASM_CC")/clang++
      if [ -x "$cxx_candidate" ]; then
        WASM_CXX=$cxx_candidate
      fi
      ;;
  esac
  if [ -z "$WASM_CXX" ] && command -v clang++ >/dev/null 2>&1; then
    WASM_CXX=$(command -v clang++)
  fi
  if [ -z "$WASM_CXX" ]; then
    echo "build-wasm: C++ grammar source requires clang++ (set WASM_CXX)" >&2
    exit 127
  fi
}

compile_cxx() {
  src=$1
  obj=$2
  resolve_cxx
  if [ "$COMPILER" = zig ]; then
    "$WASM_CXX" c++ "$TARGET_FLAG" "$TARGET_VALUE" -O2 -std=c++17 \
      -fno-exceptions -fno-rtti \
      -fvisibility=hidden -D__EMSCRIPTEN__ -D_POSIX_C_SOURCE=200112L \
      -D_DEFAULT_SOURCE "-DSITTERWASM_LANGUAGE_FN=$SITTERWASM_LANGUAGE_FN" \
      "-DSITTERWASM_LANGUAGE_NAME=$LANGUAGE_NAME_DEFINE" \
      "-I$INCLUDE" "-I$RUNTIME/include" "-I$RUNTIME" "-I$GRAMMAR" \
      "-I$(dirname "$GRAMMAR_SRC")" -c "$src" -o "$obj"
  else
    "$WASM_CXX" "$TARGET_FLAG" -O2 -std=c++17 -fno-exceptions -fno-rtti \
      -fvisibility=hidden \
      -D__EMSCRIPTEN__ -D_POSIX_C_SOURCE=200112L -D_DEFAULT_SOURCE \
      "-DSITTERWASM_LANGUAGE_FN=$SITTERWASM_LANGUAGE_FN" \
      "-DSITTERWASM_LANGUAGE_NAME=$LANGUAGE_NAME_DEFINE" \
      "-I$INCLUDE" "-I$RUNTIME/include" "-I$RUNTIME" "-I$GRAMMAR" \
      "-I$(dirname "$GRAMMAR_SRC")" -c "$src" -o "$obj"
  fi
}

compile_c "$RUNTIME/lib.c" "$BUILD_DIR/tree-sitter.o"
compile_c "$GRAMMAR_SRC" "$BUILD_DIR/grammar.o"
compile_c "$BRIDGE" "$BUILD_DIR/bridge.o"

# Keep the historical object order for reproducible output.  POSIX `sh` has no
# arrays, so the script's (unused) positional-argument vector is used as a
# small, correctly quoted object list; every append below adds one pathname as
# one argument, even when BUILD_DIR contains spaces.
set -- "$BUILD_DIR/tree-sitter.o" "$BUILD_DIR/grammar.o" "$BUILD_DIR/bridge.o"
NEEDS_CXX=0

if [ -n "$GRAMMAR_EXTRA_SRC" ]; then
  extra_index=0
  # Word splitting is intentional: callers provide a small, local list of
  # scanner sources. Each object gets a unique name even when scanners share a
  # basename.
  for extra in $GRAMMAR_EXTRA_SRC; do
    extra_src=$(resolve_source "$extra")
    case "$extra" in
      /*) ;;
      *)
        if [ ! -f "$extra_src" ] && [ -f "$GRAMMAR/$extra" ]; then
          extra_src="$GRAMMAR/$extra"
        fi
        ;;
    esac
    if [ ! -f "$extra_src" ]; then
      echo "build-wasm: missing grammar source: $extra_src" >&2
      exit 1
    fi
    case "$extra_src" in
      *.cc|*.cpp|*.cxx|*.C)
        NEEDS_CXX=1
        compile_cxx "$extra_src" "$BUILD_DIR/grammar-extra-$extra_index.o"
        ;;
      *)
        compile_c "$extra_src" "$BUILD_DIR/grammar-extra-$extra_index.o"
        ;;
    esac
    set -- "$@" "$BUILD_DIR/grammar-extra-$extra_index.o"
    extra_index=$((extra_index + 1))
  done
fi

# Keep the exported surface explicit.  This makes accidental ABI additions
# impossible and allows dead-code elimination for the C runtime.
EXPORTS="
tsw_abi_version tsw_alloc tsw_free tsw_realloc
tsw_language tsw_language_name tsw_language_name_ptr tsw_language_name_len
tsw_language_abi_version tsw_language_abi_version_for tsw_language_metadata
tsw_language_symbol_count tsw_language_state_count tsw_language_field_count
tsw_language_field_id_for_name tsw_language_symbol_type tsw_language_next_state tsw_language_version
tsw_language_symbol_name tsw_language_field_name tsw_language_symbol_for_name
tsw_language_symbol_for_name_named
tsw_lookahead_iterator_new tsw_lookahead_iterator_delete
tsw_lookahead_iterator_reset_state tsw_lookahead_iterator_reset
tsw_lookahead_iterator_language tsw_lookahead_iterator_next
tsw_lookahead_iterator_current_symbol tsw_lookahead_iterator_current_symbol_name
tsw_parser_new tsw_parser_delete tsw_parser_set_language tsw_parser_language
tsw_parser_set_included_ranges tsw_parser_included_ranges_into
tsw_parser_parse tsw_parser_reset tsw_parser_set_timeout_micros tsw_parser_set_timeout
tsw_parser_timeout_micros tsw_parser_set_cancellation_flag tsw_parser_cancellation_flag
tsw_tree_delete tsw_tree_copy tsw_tree_root tsw_tree_root_with_offset tsw_tree_edit
tsw_tree_language tsw_tree_included_ranges_into
tsw_tree_changed_ranges_into tsw_tree_get_changed_ranges tsw_ranges_free
tsw_node_delete tsw_node_type_ptr tsw_node_type_len tsw_node_type tsw_node_string
tsw_node_to_sexp tsw_node_id tsw_node_kind_id tsw_node_grammar_type tsw_node_symbol
tsw_node_grammar_symbol tsw_node_language tsw_node_start_byte tsw_node_end_byte
tsw_node_start_row tsw_node_start_column tsw_node_end_row tsw_node_end_column
tsw_node_start_point tsw_node_end_point tsw_node_is_null tsw_node_is_named
tsw_node_is_missing tsw_node_is_extra tsw_node_has_changes tsw_node_has_error
tsw_node_is_error tsw_node_parse_state tsw_node_next_parse_state tsw_node_parent
tsw_node_eq tsw_node_child_with_descendant tsw_node_child tsw_node_named_child tsw_node_child_count
tsw_node_named_child_count tsw_node_next_sibling tsw_node_prev_sibling
tsw_node_next_named_sibling tsw_node_prev_named_sibling tsw_node_child_by_field_name
tsw_node_child_by_field_id
tsw_node_field_name_for_child_ptr tsw_node_field_name_for_child
tsw_node_field_name_for_named_child tsw_node_first_child_for_byte
tsw_node_first_named_child_for_byte tsw_node_descendant_for_byte_range
tsw_node_named_descendant_for_byte_range tsw_node_descendant_for_point_range
tsw_node_named_descendant_for_point_range tsw_node_descendant_count
tsw_node_edit
tsw_cursor_new tsw_cursor_delete tsw_cursor_reset tsw_cursor_reset_to
tsw_cursor_current_node tsw_cursor_current_field_name_ptr
tsw_cursor_current_field_name_len tsw_cursor_current_field_name tsw_cursor_current_field_id
tsw_cursor_goto_parent tsw_cursor_goto_next_sibling tsw_cursor_goto_previous_sibling
tsw_cursor_goto_first_child tsw_cursor_goto_last_child tsw_cursor_goto_descendant
tsw_cursor_current_descendant_index tsw_cursor_current_depth
tsw_cursor_goto_first_child_for_byte tsw_cursor_goto_first_child_for_point tsw_cursor_copy
tsw_tree_cursor_new tsw_tree_cursor_delete tsw_tree_cursor_reset tsw_tree_cursor_reset_to
tsw_tree_cursor_current_node tsw_tree_cursor_current_field_name_ptr
tsw_tree_cursor_current_field_name_len tsw_tree_cursor_current_field_name tsw_tree_cursor_current_field_id
tsw_tree_cursor_goto_parent tsw_tree_cursor_goto_next_sibling tsw_tree_cursor_goto_previous_sibling
tsw_tree_cursor_goto_prev_sibling tsw_tree_cursor_goto_first_child tsw_tree_cursor_goto_last_child
tsw_tree_cursor_goto_descendant tsw_tree_cursor_current_descendant_index tsw_tree_cursor_current_depth
tsw_tree_cursor_goto_first_child_for_byte tsw_tree_cursor_goto_first_child_for_point tsw_tree_cursor_copy
tsw_query_new tsw_query_delete tsw_query_pattern_count tsw_query_capture_count
tsw_query_string_count tsw_query_start_byte_for_pattern tsw_query_end_byte_for_pattern
tsw_query_predicates_for_pattern_into tsw_query_is_pattern_rooted
tsw_query_is_pattern_non_local tsw_query_is_pattern_guaranteed_at_step
tsw_query_capture_name_ptr tsw_query_capture_name_len tsw_query_capture_name
tsw_query_capture_quantifier_for_id tsw_query_string_value_ptr tsw_query_string_value_len
tsw_query_string_value tsw_query_disable_capture tsw_query_disable_pattern
tsw_query_cursor_new tsw_query_cursor_delete tsw_query_cursor_exec
tsw_query_cursor_next_match tsw_query_cursor_next_capture
tsw_query_cursor_next_capture_match
tsw_query_cursor_remove_match
tsw_query_cursor_did_exceed_match_limit tsw_query_cursor_match_limit
tsw_query_cursor_set_match_limit tsw_query_cursor_set_timeout_micros
tsw_query_cursor_timeout_micros tsw_query_cursor_set_byte_range
tsw_query_cursor_set_point_range tsw_query_cursor_set_max_start_depth
tsw_copy_string tsw_last_error_ptr tsw_last_error_len tsw_clear_error
malloc calloc realloc free
"

LINK_FLAGS="-Wl,--no-entry -Wl,--export-memory \
  ${MEMORY_FLAGS} \
  -Wl,--strip-all"
for symbol in $EXPORTS; do
  LINK_FLAGS="$LINK_FLAGS -Wl,--export=$symbol"
done

mkdir -p "$(dirname "$OUT")"
# Link into a temporary file next to the requested output and publish it only
# after the linker succeeds.  A failed compile/link must never truncate an
# already-working checked-in fixture (or a caller's custom artifact).  Keeping
# the temporary file in the destination directory also makes the final rename
# atomic on the usual POSIX filesystems.
OUT_DIR=$(dirname "$OUT")
TMP_OUT=$(mktemp "$OUT_DIR/.sitterwasm-wasm.XXXXXX")

# `$@` contains the explicitly ordered object list assembled above.  Expanding
# it as `"$@"` preserves one argument per object, including paths with spaces.
if [ "$COMPILER" = zig ]; then
  if [ "$NEEDS_CXX" -eq 1 ]; then
    resolve_cxx
    "$WASM_CXX" c++ "$TARGET_FLAG" "$TARGET_VALUE" $LINK_FLAGS \
      "$@" -o "$TMP_OUT"
  else
    "$WASM_CC" cc "$TARGET_FLAG" "$TARGET_VALUE" $LINK_FLAGS \
      "$@" -o "$TMP_OUT"
  fi
else
  if [ "$NEEDS_CXX" -eq 1 ]; then
    resolve_cxx
    "$WASM_CXX" "$TARGET_FLAG" $LINK_FLAGS "$@" -o "$TMP_OUT"
  else
    "$WASM_CC" "$TARGET_FLAG" $LINK_FLAGS "$@" -o "$TMP_OUT"
  fi
fi

# `mv` replaces the destination in one filesystem operation.  The output is
# intentionally published only after a successful linker exit status.
mv -f "$TMP_OUT" "$OUT"
TMP_OUT=

echo "build-wasm: wrote $OUT"
