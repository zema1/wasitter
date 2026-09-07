package wasitter_test

import (
	_ "embed"
	"encoding/json"
	"testing"
)

// The expected S-expressions were generated with the same Tree-sitter C
// runtime and JSON grammar used to build the bundled module. Keeping them in
// a small machine-readable corpus makes native/WASM parity reviews easy.
//
//go:embed testdata/json_cases.json
var jsonCorpus []byte

type jsonCorpusCase struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Sexp   string `json:"sexp"`
}

func loadJSONCorpus(t *testing.T) []jsonCorpusCase {
	t.Helper()
	var cases []jsonCorpusCase
	if err := json.Unmarshal(jsonCorpus, &cases); err != nil {
		t.Fatalf("decode JSON corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("JSON corpus is empty")
	}
	seen := make(map[string]struct{}, len(cases))
	for _, tc := range cases {
		if tc.Name == "" || tc.Sexp == "" {
			t.Fatalf("corpus case has missing name/sexp: %#v", tc)
		}
		if _, ok := seen[tc.Name]; ok {
			t.Fatalf("duplicate corpus case %q", tc.Name)
		}
		seen[tc.Name] = struct{}{}
	}
	return cases
}

func TestJSONCorpusIsValid(t *testing.T) {
	_ = loadJSONCorpus(t)
}
