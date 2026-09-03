#!/bin/sh
# Download one pinned official grammar and build its sitterwasm artifact.
# This script runs inside docker/wasm-builder; it is not a host entry point.

set -eu

ROOT=/workspace
REGISTRY="$ROOT/scripts/grammar-registry.tsv"
if [ "$#" -ne 1 ]; then
  echo "build-grammar: usage: build-grammar-in-container.sh <language>" >&2
  exit 2
fi
LANGUAGE=$1

if [ -z "$LANGUAGE" ]; then
  echo "build-grammar: usage: build-grammar-in-container.sh <language>" >&2
  exit 2
fi
case "$LANGUAGE" in
  *[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0-9_-]*)
    echo "build-grammar: invalid language name: $LANGUAGE" >&2
    exit 2
    ;;
esac

if [ ! -s "$REGISTRY" ]; then
  echo "build-grammar: registry is missing: $REGISTRY" >&2
  exit 1
fi

entry_count=$(awk -F '\t' -v name="$LANGUAGE" '$1 == name { count++ } END { print count + 0 }' "$REGISTRY")
if [ "$entry_count" -eq 0 ]; then
  echo "build-grammar: language is not in the official grammar registry: $LANGUAGE" >&2
  echo "build-grammar: add a pinned entry to $REGISTRY" >&2
  exit 2
fi
if [ "$entry_count" -ne 1 ]; then
  echo "build-grammar: registry contains duplicate entries for: $LANGUAGE" >&2
  exit 2
fi
ENTRY=$(awk -F '\t' -v name="$LANGUAGE" '$1 == name { print; exit }' "$REGISTRY")
field_count=$(printf '%s\n' "$ENTRY" | awk -F '\t' '{ print NF }')
if [ "$field_count" -ne 8 ]; then
  echo "build-grammar: malformed registry entry for $LANGUAGE (expected 8 fields)" >&2
  exit 2
fi

field() {
  index=$1
  printf '%s\n' "$ENTRY" | awk -F '\t' -v n="$index" '{ print $n }'
}
REPOSITORY=$(field 2)
TAG=$(field 3)
ARCHIVE_SHA256=$(field 4)
PARSER_PATH=$(field 5)
SCANNER_PATHS=$(field 6)
LANGUAGE_FUNCTION=$(field 7)
LANGUAGE_NAME=$(field 8)

case "$REPOSITORY" in
  tree-sitter/*) ;;
  *) echo "build-grammar: registry repository must be under tree-sitter/: $REPOSITORY" >&2; exit 2 ;;
esac
case "$TAG" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "build-grammar: registry tag must be a release tag: $TAG" >&2; exit 2 ;;
esac
case "$ARCHIVE_SHA256" in
  '') echo "build-grammar: registry checksum is empty for $LANGUAGE" >&2; exit 2 ;;
esac
if ! printf '%s\n' "$ARCHIVE_SHA256" | awk 'length($0) == 64 && $0 !~ /[^0-9a-fA-F]/ { ok=1 } END { exit(ok ? 0 : 1) }'; then
  echo "build-grammar: registry checksum is malformed for $LANGUAGE" >&2
  exit 2
fi
case "$PARSER_PATH" in
  /*|../*|*/../*|*'/..')
    echo "build-grammar: parser path must stay inside the archive: $PARSER_PATH" >&2
    exit 2
    ;;
esac
case "$SCANNER_PATHS" in
  '')
    echo "build-grammar: scanner path field is empty for $LANGUAGE" >&2
    exit 2
    ;;
  -) ;;
  *)
    # The registry permits a whitespace-separated list for grammars that have
    # more than one external scanner source. Paths intentionally cannot contain
    # whitespace; archive paths use ordinary POSIX names and this keeps the TSV
    # format simple and shell-portable.
    for scanner_path in $SCANNER_PATHS; do
      case "$scanner_path" in
        /*|../*|*/../*|*'/..')
          echo "build-grammar: scanner path must stay inside the archive: $scanner_path" >&2
          exit 2
          ;;
      esac
    done
    ;;
esac
case "$LANGUAGE_FUNCTION" in
  ''|*[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_]*|[0-9]*)
    echo "build-grammar: language function is not a C identifier: $LANGUAGE_FUNCTION" >&2
    exit 2
    ;;
esac
case "$LANGUAGE_NAME" in
  ''|*[[:cntrl:]]*)
    echo "build-grammar: language name must be non-empty and free of control characters: $LANGUAGE_NAME" >&2
    exit 2
    ;;
esac

BUILD_DIR=$(mktemp -d /tmp/sitterwasm-grammar.XXXXXX)
CHECKSUM_TMP=
cleanup() {
  if [ -n "${CHECKSUM_TMP:-}" ] && [ -e "$CHECKSUM_TMP" ]; then
    rm -f "$CHECKSUM_TMP"
  fi
  if [ -n "${BUILD_DIR:-}" ] && [ -d "$BUILD_DIR" ]; then
    rm -rf "$BUILD_DIR"
  fi
}
trap cleanup EXIT HUP INT TERM

ARCHIVE="$BUILD_DIR/grammar.tar.gz"
URL="https://github.com/$REPOSITORY/archive/refs/tags/$TAG.tar.gz"
echo "build-grammar: downloading $REPOSITORY $TAG" >&2
curl --fail --location --retry 3 --silent --show-error "$URL" -o "$ARCHIVE"
printf '%s  %s\n' "$ARCHIVE_SHA256" "$ARCHIVE" | sha256sum -c -

mkdir -p "$BUILD_DIR/src"
tar -xzf "$ARCHIVE" -C "$BUILD_DIR/src" --strip-components=1
GRAMMAR_ROOT="$BUILD_DIR/src"
PARSER="$GRAMMAR_ROOT/$PARSER_PATH"
if [ ! -f "$PARSER" ]; then
  echo "build-grammar: parser source is missing: $PARSER_PATH" >&2
  exit 1
fi

EXTRA=
if [ "$SCANNER_PATHS" != - ]; then
  for scanner_path in $SCANNER_PATHS; do
    SCANNER="$GRAMMAR_ROOT/$scanner_path"
    if [ ! -f "$SCANNER" ]; then
      echo "build-grammar: scanner source is missing: $scanner_path" >&2
      exit 1
    fi
    if [ -n "$EXTRA" ]; then
      EXTRA="$EXTRA "
    fi
    EXTRA="${EXTRA}${SCANNER}"
  done
fi

OUT="$ROOT/internal/wasm/assets/sitterwasm-$LANGUAGE.wasm"
if [ -n "$EXTRA" ]; then
  GRAMMAR_EXTRA_SRC="$EXTRA" \
  GRAMMAR_SRC_DIR="$GRAMMAR_ROOT" \
  GRAMMAR_SRC="$PARSER" \
  SITTERWASM_LANGUAGE_FN="$LANGUAGE_FUNCTION" \
  SITTERWASM_LANGUAGE_NAME="$LANGUAGE_NAME" \
  OUT="$OUT" \
  /workspace/scripts/build-wasm.sh
else
  GRAMMAR_SRC_DIR="$GRAMMAR_ROOT" \
  GRAMMAR_SRC="$PARSER" \
  SITTERWASM_LANGUAGE_FN="$LANGUAGE_FUNCTION" \
  SITTERWASM_LANGUAGE_NAME="$LANGUAGE_NAME" \
  OUT="$OUT" \
  /workspace/scripts/build-wasm.sh
fi

if [ ! -s "$OUT" ]; then
  echo "build-grammar: compiler produced no artifact: $OUT" >&2
  exit 1
fi

CHECKSUM="$OUT.sha256"
CHECKSUM_TMP=$(mktemp "$ROOT/internal/wasm/assets/.sitterwasm-$LANGUAGE-sha256.XXXXXX")
digest=$(sha256sum "$OUT" | awk '{ print $1 }')
printf '%s  %s\n' "$digest" "$(basename "$OUT")" > "$CHECKSUM_TMP"
mv -f "$CHECKSUM_TMP" "$CHECKSUM"
CHECKSUM_TMP=
chmod 0644 "$OUT" "$CHECKSUM"

echo "build-grammar: wrote $OUT"
echo "build-grammar: wrote $CHECKSUM ($digest)"
