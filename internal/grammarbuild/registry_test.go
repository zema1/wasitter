package grammarbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRegistry(t *testing.T) {
	registry, err := LoadRegistry(filepath.Join("..", "..", "scripts", "grammar-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Grammars) < 2 {
		t.Fatalf("registry has %d grammars, want at least 2", len(registry.Grammars))
	}
	grammar, err := registry.Lookup(" JavaScript ")
	if err != nil {
		t.Fatal(err)
	}
	if grammar.Name != "javascript" || len(grammar.ScannerPaths()) != 1 {
		t.Fatalf("unexpected JavaScript row: %+v", grammar)
	}
}

func TestRegistryRejectsUnsafeRows(t *testing.T) {
	base := Grammar{
		Name:             "demo",
		Repository:       "tree-sitter/tree-sitter-demo",
		Tag:              "v1.2.3",
		ArchiveSHA256:    strings.Repeat("a", 64),
		Parser:           "src/parser.c",
		LanguageFunction: "tree_sitter_demo",
		LanguageName:     "demo",
	}
	tests := map[string]func(*Grammar){
		"traversal parser":  func(g *Grammar) { g.Parser = "../parser.c" },
		"backslash scanner": func(g *Grammar) { g.Scanner = []string{`src\\scanner.c`} },
		"untrusted owner":   func(g *Grammar) { g.Repository = "evil/example" },
		"mutable tag":       func(g *Grammar) { g.Tag = "main" },
		"bad function":      func(g *Grammar) { g.LanguageFunction = "tree-sitter-demo" },
		"bad digest":        func(g *Grammar) { g.ArchiveSHA256 = "not-a-digest" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			row := base
			mutate(&row)
			if err := row.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

func TestRegistryRejectsUnknownAndTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"version":1,"grammars":[],"oops":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(unknown); err == nil {
		t.Fatal("unknown field was accepted")
	}
	trailing := filepath.Join(dir, "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"version":1,"grammars":[]} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(trailing); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func TestRegistryRejectsDuplicateJSONKeys(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("a", 64)
	row := `"name":"demo","repository":"tree-sitter/tree-sitter-demo","tag":"v1.2.3","archive_sha256":"` + digest + `","parser":"src/parser.c","language_function":"tree_sitter_demo","language_name":"demo"`
	tests := map[string]string{
		"top level":        `{"version":1,"version":1,"grammars":[{` + row + `}]}`,
		"grammar field":    `{"version":1,"grammars":[{` + row + `,"name":"other"}]}`,
		"escaped spelling": `{"version":1,"grammars":[{"name":"demo","repository":"tree-sitter/tree-sitter-demo","tag":"v1.2.3","archive_sha256":"` + digest + `","parser":"src/parser.c","language_function":"tree_sitter_demo","language_name":"demo","scann\u0065rs":[],"scanners":[]}]}`,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRegistry(path); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
				t.Fatalf("LoadRegistry error = %v, want duplicate-key diagnostic", err)
			}
		})
	}
}

func TestRegistryAcceptsLegacyScannerKeyAndRejectsBothSpellings(t *testing.T) {
	dir := t.TempDir()
	row := `{"version":1,"grammars":[{"name":"demo","repository":"tree-sitter/tree-sitter-demo","tag":"v1.2.3","archive_sha256":"` + strings.Repeat("a", 64) + `","parser":"src/parser.c","scanner":["src/scanner.c"],"language_function":"tree_sitter_demo","language_name":"demo"}]}`
	path := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Grammars[0].ScannerPaths(); len(got) != 1 || got[0] != "src/scanner.c" {
		t.Fatalf("legacy scanner paths = %#v", got)
	}
	both := strings.Replace(row, `"scanner":["src/scanner.c"]`, `"scanner":[],"scanners":["src/scanner.c"]`, 1)
	path = filepath.Join(dir, "both.json")
	if err := os.WriteFile(path, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil || !strings.Contains(err.Error(), "cannot both") {
		t.Fatalf("both scanner spellings error = %v", err)
	}
}

func TestRegistryRejectsNullScanners(t *testing.T) {
	dir := t.TempDir()
	base := `{"version":1,"grammars":[{"name":"demo","repository":"tree-sitter/tree-sitter-demo","tag":"v1.2.3","archive_sha256":"` + strings.Repeat("a", 64) + `","parser":"src/parser.c","scanners":[],"language_function":"tree_sitter_demo","language_name":"demo"}]}`
	null := strings.Replace(base, `"scanners":[]`, `"scanners":null`, 1)
	path := filepath.Join(dir, "null.json")
	if err := os.WriteFile(path, []byte(null), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil || !strings.Contains(err.Error(), "must be an array") {
		t.Fatalf("null scanners error = %v", err)
	}
}

func TestProgrammaticScannerSpellings(t *testing.T) {
	base := Grammar{
		Name:             "demo",
		Repository:       "tree-sitter/tree-sitter-demo",
		Tag:              "v1.2.3",
		ArchiveSHA256:    strings.Repeat("a", 64),
		Parser:           "src/parser.c",
		LanguageFunction: "tree_sitter_demo",
		LanguageName:     "demo",
	}
	legacy := base
	legacy.Scanner = []string{}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy empty scanner: %v", err)
	}
	if got := legacy.ScannerPaths(); len(got) != 0 {
		t.Fatalf("legacy empty scanner paths = %#v", got)
	}
	canonical := base
	canonical.Scanners = []string{}
	if err := canonical.Validate(); err != nil {
		t.Fatalf("canonical empty scanner: %v", err)
	}
	both := base
	both.Scanner = []string{}
	both.Scanners = []string{}
	if err := both.Validate(); err == nil || !strings.Contains(err.Error(), "cannot both") {
		t.Fatalf("both programmatic scanner fields error = %v", err)
	}
}
