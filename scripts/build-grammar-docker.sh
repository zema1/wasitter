#!/bin/sh
# Host entry point for building one pinned official Tree-sitter grammar.

set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
IMAGE=sitterwasm-wasm-builder:zig-0.15.2
REGISTRY="$ROOT/scripts/grammar-registry.tsv"

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <language>" >&2
  exit 2
fi
LANGUAGE=$1
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
field_count=$(awk -F '\t' -v name="$LANGUAGE" '$1 == name { print NF; exit }' "$REGISTRY")
if [ "$field_count" -ne 8 ]; then
  echo "build-grammar: malformed registry entry for $LANGUAGE (expected 8 fields)" >&2
  exit 2
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "build-grammar: Docker is required" >&2
  exit 127
fi

docker build --pull=false --tag "$IMAGE" "$ROOT/docker/wasm-builder"
docker run --rm --init \
  --workdir /workspace \
  --user "$(id -u):$(id -g)" \
  --volume "$ROOT:/workspace:rw" \
  --entrypoint /bin/sh \
  "$IMAGE" /workspace/scripts/build-grammar-in-container.sh "$LANGUAGE"
