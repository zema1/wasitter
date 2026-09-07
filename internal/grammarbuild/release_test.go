package grammarbuild

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func releaseFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join("..", "..")
	for _, base := range []string{"LICENSE", "internal/wasm/THIRD_PARTY_NOTICES.md", "internal/wasm/third_party/tree-sitter/LICENSE", "internal/wasm/third_party/tree-sitter/unicode/LICENSE", "internal/wasm/third_party/tree-sitter/portable/endian.h", "internal/wasm/third_party/tree-sitter-json/LICENSE", "internal/wasm/third_party/licenses"} {
		err := filepath.WalkDir(filepath.Join(repo, base), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			writeReleaseFile(t, filepath.Join(root, rel), data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	registry, err := LoadRegistry(filepath.Join(repo, DefaultRegistryRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	registry.Grammars = registry.Grammars[:2]
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input")
	writeReleaseFile(t, filepath.Join(root, DefaultRegistryRelativePath), data)
	writeReleaseFile(t, filepath.Join(input, "grammar-registry.json"), data)
	for _, grammar := range registry.Grammars {
		path := filepath.Join(input, "wasitter-"+grammar.Name+".wasm")
		writeReleaseFile(t, path, []byte{'\x00', 'a', 's', 'm', 1, 0, 0, 0})
		if _, _, err := WriteArtifactChecksum(path); err != nil {
			t.Fatal(err)
		}
		writeReleaseFile(t, filepath.Join(input, "licenses", grammar.Name, "LICENSE"), []byte("upstream license"))
		writeReleaseFile(t, filepath.Join(input, "licenses", grammar.Name, "scanner", "NOTICE.txt"), []byte("scanner notice"))
	}
	return root, input
}

func writeReleaseFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareGrammarAssets(t *testing.T) {
	root, input := releaseFixture(t)
	output, err := PrepareGrammarAssets(root, input, "dist/grammars")
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	for _, file := range files {
		if file.IsDir() || strings.HasSuffix(file.Name(), ".zip") {
			t.Fatalf("unexpected release asset: %s", file.Name())
		}
		data, err := os.ReadFile(filepath.Join(output, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		entries[file.Name()] = data
	}
	for _, required := range []string{"wasitter-json.wasm", "wasitter-json.wasm.sha256", "wasitter-javascript.wasm", "wasitter-javascript.wasm.sha256", "SHA256SUMS", "grammar-registry.json", "THIRD_PARTY_NOTICES.txt"} {
		if len(entries[required]) == 0 {
			t.Errorf("missing release asset %s", required)
		}
	}
	for _, name := range []string{"json", "javascript"} {
		path := filepath.Join(output, "wasitter-"+name+".wasm")
		if _, err := VerifyArtifact(path, ""); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(filepath.Join(input, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(original, entries[filepath.Base(path)]) {
			t.Fatal("WASM changed")
		}
	}
	for _, notice := range []string{"licenses/json/LICENSE", "licenses/javascript/scanner/NOTICE.txt", "upstream license", "scanner notice", "internal/wasm/third_party/tree-sitter/unicode/LICENSE", "internal/wasm/third_party/licenses/wasi-libc/musl-COPYRIGHT"} {
		if !strings.Contains(string(entries["THIRD_PARTY_NOTICES.txt"]), notice) {
			t.Errorf("missing license notice %s", notice)
		}
	}
	if len(strings.Split(strings.TrimSpace(string(entries["SHA256SUMS"])), "\n")) != 2 {
		t.Fatal("incomplete checksums")
	}
	second, err := PrepareGrammarAssets(root, input, "dist/repeated")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range entries {
		got, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("non-deterministic asset %s", name)
		}
	}
	if _, err := PrepareGrammarAssets(root, input, "dist/grammars"); err == nil {
		t.Fatal("overwrote existing asset directory")
	}
	writeReleaseFile(t, filepath.Join(input, "wasitter-json.wasm"), []byte("corrupted"))
	if _, err := PrepareGrammarAssets(root, input, "dist/invalid"); err == nil {
		t.Fatal("accepted stale checksum")
	}
	if _, err := os.Stat(filepath.Join(root, "dist/invalid")); !os.IsNotExist(err) {
		t.Fatal("failed preparation left output directory")
	}
}

func TestPrepareGrammarAssetsRejectsIncompleteInputs(t *testing.T) {
	tests := map[string]func(*testing.T, string, string){
		"missing grammar": func(t *testing.T, root, input string) {
			if err := os.Remove(filepath.Join(input, "wasitter-json.wasm")); err != nil {
				t.Fatal(err)
			}
		},
		"missing licenses": func(t *testing.T, root, input string) {
			if err := os.RemoveAll(filepath.Join(input, "licenses/json")); err != nil {
				t.Fatal(err)
			}
		},
		"changed registry": func(t *testing.T, root, input string) {
			writeReleaseFile(t, filepath.Join(input, "grammar-registry.json"), []byte("{}"))
		},
		"invalid wasm": func(t *testing.T, root, input string) {
			path := filepath.Join(input, "wasitter-json.wasm")
			writeReleaseFile(t, path, []byte("not wasm"))
			if _, _, err := WriteArtifactChecksum(path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root, input := releaseFixture(t)
			mutate(t, root, input)
			if _, err := PrepareGrammarAssets(root, input, "dist/grammars"); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}

}

func TestExportLicenses(t *testing.T) {
	source, output := t.TempDir(), t.TempDir()
	if err := exportLicenses(source, output); err == nil {
		t.Fatal("accepted missing license")
	}
	for _, path := range []string{"LICENSE", "src/dependency/Notice.txt", "vendor/COPYING", "LICENSES/MIT.txt"} {
		writeReleaseFile(t, filepath.Join(source, path), []byte(path))
	}
	writeReleaseFile(t, filepath.Join(source, "src/parser.c"), []byte("not a license"))
	if err := exportLicenses(source, output); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"LICENSE", "src/dependency/Notice.txt", "vendor/COPYING", "LICENSES/MIT.txt"} {
		data, err := os.ReadFile(filepath.Join(output, path))
		if err != nil || string(data) != path {
			t.Fatalf("license %s: %s, %v", path, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "src/parser.c")); !os.IsNotExist(err) {
		t.Fatal("copied non-license source")
	}
}
