#!/bin/sh
# Build the checked-in JSON Tree-sitter WASM artifact in Docker.
#
# This intentionally has one fixed workflow: build the pinned image, mount the
# checkout, run the low-level compiler driver, and refresh the adjacent digest.
# Custom grammars belong to the low-level scripts/build-wasm.sh interface.

set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
IMAGE=sitterwasm-wasm-builder:zig-0.15.2
OUT_PATH="$ROOT/internal/wasm/assets/sitterwasm-json.wasm"
CHECKSUM_PATH="$OUT_PATH.sha256"

if ! command -v docker >/dev/null 2>&1; then
  echo "build-wasm-docker: Docker is required" >&2
  exit 127
fi

# Docker layer caching makes this inexpensive after the first invocation while
# still ensuring Docker notices changes to the pinned Dockerfile.
echo "build-wasm-docker: building image $IMAGE" >&2
docker build --pull=false --tag "$IMAGE" "$ROOT/docker/wasm-builder"

# The image sets PATH, HOME, and ZIG_GLOBAL_CACHE_DIR itself. No host
# environment is forwarded into the build, which keeps output reproducible.
docker run --rm --init \
  --workdir /workspace \
  --user "$(id -u):$(id -g)" \
  --volume "$ROOT:/workspace:rw" \
  "$IMAGE"

if [ ! -s "$OUT_PATH" ]; then
  echo "build-wasm-docker: build completed without a non-empty artifact: $OUT_PATH" >&2
  exit 1
fi

# Refresh the digest only after a successful build. Use a sibling temporary
# file so an interrupted hash/write cannot leave a partial checksum.
CHECKSUM_TMP=$(mktemp "$ROOT/internal/wasm/assets/.sitterwasm-sha256.XXXXXX")
cleanup() {
  if [ -e "${CHECKSUM_TMP:-}" ]; then
    rm -f "$CHECKSUM_TMP"
  fi
}
trap cleanup EXIT HUP INT TERM

if command -v sha256sum >/dev/null 2>&1; then
  digest=$(sha256sum "$OUT_PATH" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  digest=$(shasum -a 256 "$OUT_PATH" | awk '{ print $1 }')
else
  echo "build-wasm-docker: need sha256sum or shasum" >&2
  exit 127
fi
printf '%s  %s\n' "$digest" "$(basename "$OUT_PATH")" > "$CHECKSUM_TMP"
mv -f "$CHECKSUM_TMP" "$CHECKSUM_PATH"
CHECKSUM_TMP=
chmod 0644 "$OUT_PATH" "$CHECKSUM_PATH"

echo "build-wasm-docker: wrote $OUT_PATH"
echo "build-wasm-docker: wrote $CHECKSUM_PATH ($digest)"
