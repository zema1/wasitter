#!/bin/sh
# Verify the checked-in WASM fixture against its published SHA-256 digest.
# This script intentionally has no network or compiler dependency, so it can
# run in release jobs and on consumer machines that only have a POSIX shell.

set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
ARTIFACT=${1:-"$ROOT/internal/wasm/assets/sitterwasm-json.wasm"}
CHECKSUM=${2:-"$ARTIFACT.sha256"}

if [ ! -s "$ARTIFACT" ]; then
  echo "verify-wasm: missing or empty artifact: $ARTIFACT" >&2
  exit 1
fi
if [ ! -s "$CHECKSUM" ]; then
  echo "verify-wasm: missing checksum file: $CHECKSUM" >&2
  exit 1
fi

expected=$(awk 'NF { print $1; exit }' "$CHECKSUM")
if ! awk -v digest="$expected" 'BEGIN {
  if (length(digest) != 64) exit 1
  # Avoid interval-regexp extensions so this remains portable to minimal awk.
  if (digest !~ /^[0-9a-fA-F]+$/) exit 1
  exit 0
}' </dev/null; then
  echo "verify-wasm: malformed SHA-256 checksum file: $CHECKSUM" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$ARTIFACT" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$ARTIFACT" | awk '{ print $1 }')
else
  echo "verify-wasm: need sha256sum or shasum" >&2
  exit 127
fi

if [ "$actual" != "$expected" ]; then
  echo "verify-wasm: digest mismatch for $ARTIFACT" >&2
  echo "  expected: $expected" >&2
  echo "  actual:   $actual" >&2
  exit 1
fi

echo "verify-wasm: OK $ARTIFACT ($actual)"
