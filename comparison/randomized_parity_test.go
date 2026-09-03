package comparison

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	native "github.com/tree-sitter/go-tree-sitter"
	wasm "github.com/zema1/sitterwasm"
)

// TestRandomizedParserParity supplements the hand-written corpus with a
// deterministic differential test. Each generated document is parsed by the
// checked-in WASM runtime and by the pinned upstream C runtime. We compare
// both Tree-sitter's complete S-expression and a recursive view of every node
// (including symbols, fields, flags, byte ranges, and point ranges).
//
// Keep the seed and case count fixed: a failure must be directly reproducible
// in CI, and this is intended to stay a quick unit test rather than a fuzzer.
func TestRandomizedParserParity(t *testing.T) {
	const (
		seed      = int64(0x51_77_45_45)
		caseCount = 32
	)

	random := rand.New(rand.NewSource(seed))
	wasmParser, _ := newWASM(t)
	nativeParser, _ := newNative(t)

	for i := 0; i < caseCount; i++ {
		// Always use an object at the top level so each one-byte corruption
		// below is guaranteed to turn the source into invalid JSON.
		value := map[string]any{
			"case":    i,
			"payload": randomJSONValue(random, 0),
			"tag":     randomJSONString(random),
		}
		var (
			source []byte
			err    error
		)
		if i%2 == 0 {
			source, err = json.Marshal(value)
		} else {
			source, err = json.MarshalIndent(value, "", "  ")
		}
		if err != nil {
			t.Fatalf("seed %#x case %d: marshal: %v", seed, i, err)
		}
		if !json.Valid(source) {
			t.Fatalf("seed %#x case %d: generator produced invalid JSON: %q", seed, i, source)
		}

		assertRandomTreeParity(t, wasmParser, nativeParser, source,
			fmt.Sprintf("seed=%#x case=%d valid", seed, i))

		malformed := lightlyCorruptJSON(source, i)
		if json.Valid(malformed) {
			t.Fatalf("seed %#x case %d: mutation remained valid JSON: %q", seed, i, malformed)
		}
		assertRandomTreeParity(t, wasmParser, nativeParser, malformed,
			fmt.Sprintf("seed=%#x case=%d malformed", seed, i))
	}
}

func assertRandomTreeParity(
	t *testing.T,
	wasmParser *wasm.Parser,
	nativeParser *native.Parser,
	source []byte,
	label string,
) {
	t.Helper()

	wasmTree, err := wasmParser.Parse(source, nil)
	if err != nil {
		t.Fatalf("%s: WASM parse %q: %v", label, source, err)
	}
	defer wasmTree.Close()
	nativeTree := nativeParser.Parse(source, nil)
	if nativeTree == nil {
		t.Fatalf("%s: native parse %q returned nil", label, source)
	}
	defer nativeTree.Close()

	wasmSExpression := wasmTree.ToSExpression()
	nativeSExpression := nativeTree.RootNode().ToSexp()
	if wasmSExpression != nativeSExpression {
		t.Fatalf("%s: S-expressions differ for %q:\nWASM:   %s\nNative: %s",
			label, source, wasmSExpression, nativeSExpression)
	}

	wasmView := wasmNodeView(wasmTree.RootNode(), "")
	nativeView := nativeNodeView(nativeTree.RootNode(), "")
	if !reflect.DeepEqual(wasmView, nativeView) {
		t.Fatalf("%s: recursive tree views differ for %q:\nWASM:   %#v\nNative: %#v",
			label, source, wasmView, nativeView)
	}
}

func randomJSONValue(random *rand.Rand, depth int) any {
	if depth >= 3 {
		return randomJSONScalar(random)
	}

	switch random.Intn(6) {
	case 0, 1:
		length := random.Intn(4)
		values := make([]any, length)
		for i := range values {
			values[i] = randomJSONValue(random, depth+1)
		}
		return values
	case 2, 3:
		length := random.Intn(4)
		values := make(map[string]any, length)
		for i := 0; i < length; i++ {
			key := fmt.Sprintf("k%d_%s", i, randomJSONString(random))
			values[key] = randomJSONValue(random, depth+1)
		}
		return values
	default:
		return randomJSONScalar(random)
	}
}

func randomJSONScalar(random *rand.Rand) any {
	switch random.Intn(5) {
	case 0:
		return nil
	case 1:
		return random.Intn(2) == 0
	case 2:
		return random.Intn(2_000_001) - 1_000_000
	case 3:
		numbers := [...]json.Number{"0", "-0", "0.125", "-12.5", "6.022e23", "1E-9"}
		return numbers[random.Intn(len(numbers))]
	default:
		return randomJSONString(random)
	}
}

func randomJSONString(random *rand.Rand) string {
	parts := [...]string{
		"", "alpha", "with space", "quote\"slash\\", "line\nfeed",
		"tab\tvalue", "树", "🌳", "混合🌳text", "<tag>&value",
	}
	return parts[random.Intn(len(parts))]
}

func lightlyCorruptJSON(source []byte, caseIndex int) []byte {
	corrupt := append([]byte(nil), bytes.TrimSpace(source)...)
	switch caseIndex % 6 {
	case 0:
		// Remove the top-level closing brace.
		return corrupt[:len(corrupt)-1]
	case 1:
		// Add a trailing comma to the top-level object.
		return append(append(corrupt[:len(corrupt)-1:len(corrupt)-1], ','), '}')
	case 2:
		// Remove the first key/value separator.
		colon := bytes.IndexByte(corrupt, ':')
		return append(corrupt[:colon:colon], corrupt[colon+1:]...)
	case 3:
		// Remove the opening quote of the first object key.
		quote := bytes.IndexByte(corrupt, '"')
		return append(corrupt[:quote:quote], corrupt[quote+1:]...)
	case 4:
		// Insert an unmatched array closer before the object closer.
		return append(append(corrupt[:len(corrupt)-1:len(corrupt)-1], ']'), '}')
	default:
		// Add a second top-level value.
		return append(corrupt, []byte(" true")...)
	}
}
