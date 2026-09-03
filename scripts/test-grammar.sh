#!/bin/sh
# Validate one checked-in grammar artifact before running semantic tests.
#
# This is deliberately host-side and offline: the build task is responsible
# for downloading/building a grammar, while this task only verifies that the
# requested, registered artifact is present and has the recorded digest.

set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
REGISTRY="$ROOT/scripts/grammar-registry.tsv"

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <language>" >&2
	exit 2
fi

LANGUAGE=$1
case "$LANGUAGE" in
	*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0-9_-]*)
		echo "test-grammar: invalid language name: $LANGUAGE" >&2
		exit 2
		;;
esac

if [ ! -s "$REGISTRY" ]; then
	echo "test-grammar: registry is missing: $REGISTRY" >&2
	exit 1
fi

# Require a complete registry row. This catches accidental comments, blank
# rows, or partially entered metadata before a test can report no matches.
entry_count=$(awk -F '\t' -v name="$LANGUAGE" '$1 == name { count++ } END { print count + 0 }' "$REGISTRY")
if [ "$entry_count" -eq 0 ]; then
	echo "test-grammar: language is not in the official grammar registry: $LANGUAGE" >&2
	exit 2
fi
if [ "$entry_count" -ne 1 ]; then
	echo "test-grammar: registry contains duplicate entries for: $LANGUAGE" >&2
	exit 2
fi
entry=$(awk -F '\t' -v name="$LANGUAGE" '$1 == name { print; exit }' "$REGISTRY")
fields=$(printf '%s\n' "$entry" | awk -F '\t' '{ print NF }')
if [ "$fields" -ne 8 ]; then
	echo "test-grammar: malformed registry entry for $LANGUAGE (expected 8 fields)" >&2
	exit 1
fi

ARTIFACT="$ROOT/internal/wasm/assets/sitterwasm-$LANGUAGE.wasm"
CHECKSUM="$ARTIFACT.sha256"
if [ ! -s "$ARTIFACT" ]; then
	echo "test-grammar: missing or empty artifact: $ARTIFACT" >&2
	exit 1
fi
if [ ! -s "$CHECKSUM" ]; then
	echo "test-grammar: missing checksum file: $CHECKSUM" >&2
	exit 1
fi

# Keep the checksum file self-describing. verify-wasm.sh validates the digest,
# while this check prevents a valid digest for a different file being reused.
recorded_file=$(awk 'NF >= 2 { print $2; exit }' "$CHECKSUM")
expected_file=$(basename "$ARTIFACT")
if [ "$recorded_file" != "$expected_file" ]; then
	echo "test-grammar: checksum names $recorded_file, expected $expected_file" >&2
	exit 1
fi

"$ROOT/scripts/verify-wasm.sh" "$ARTIFACT" "$CHECKSUM"
echo "test-grammar: registry and artifact checks passed for $LANGUAGE"
