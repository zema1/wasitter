package grammarbuild

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PrepareGrammarAssets prepares individual assets from a complete, verified
// registry build. Release checks must execute the modules before this step.
func PrepareGrammarAssets(root, artifactDir, outputDir string) (string, error) {
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(root, path)
	}
	artifactDir, outputDir = resolve(artifactDir), resolve(outputDir)
	registryPath := ResolveRegistryPath(root, "")
	registry, err := LoadRegistry(registryPath)
	if err != nil {
		return "", err
	}
	entries := make(map[string][]byte)
	add := func(name, path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular release input: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries[filepath.ToSlash(name)] = data
		return nil
	}
	if err := add(DefaultRegistryRelativePath, registryPath); err != nil {
		return "", err
	}
	builtRegistry, err := os.ReadFile(filepath.Join(artifactDir, "grammar-registry.json"))
	if err != nil {
		return "", err
	}
	if !bytes.Equal(builtRegistry, entries[DefaultRegistryRelativePath]) {
		return "", fmt.Errorf("build registry differs from checkout; rebuild all grammars")
	}
	var sums strings.Builder
	for _, grammar := range registry.Grammars {
		name := "wasitter-" + grammar.Name + ".wasm"
		path := filepath.Join(artifactDir, name)
		digest, err := VerifyArtifact(path, "")
		if err != nil {
			return "", err
		}
		if err := add(name, path); err != nil {
			return "", err
		}
		data := entries[name]
		if len(data) < 8 || !bytes.Equal(data[:8], []byte{'\x00', 'a', 's', 'm', 1, 0, 0, 0}) {
			return "", fmt.Errorf("invalid WASM header: %s", path)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
			return "", fmt.Errorf("artifact changed while preparing release assets: %s", path)
		}
		checksum := fmt.Sprintf("%s  %s\n", digest, name)
		entries[name+".sha256"] = []byte(checksum)
		sums.WriteString(checksum)
		licenses := filepath.Join(artifactDir, "licenses", grammar.Name)
		files, err := licenseFiles(licenses)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return "", fmt.Errorf("no licenses for %s", grammar.Name)
		}
		for _, rel := range files {
			if err := add(filepath.Join("licenses", grammar.Name, rel), filepath.Join(licenses, rel)); err != nil {
				return "", err
			}
		}
	}
	// Retain source paths as labels in the combined license notices.
	for _, rel := range []string{
		"LICENSE", "internal/wasm/THIRD_PARTY_NOTICES.md",
		"internal/wasm/third_party/tree-sitter/LICENSE",
		"internal/wasm/third_party/tree-sitter/unicode/LICENSE",
		"internal/wasm/third_party/tree-sitter/portable/endian.h",
		"internal/wasm/third_party/tree-sitter-json/LICENSE",
		"internal/wasm/third_party/licenses/zig/LICENSE",
	} {
		if err := add(rel, filepath.Join(root, rel)); err != nil {
			return "", err
		}
	}
	libc := "internal/wasm/third_party/licenses/wasi-libc"
	if err := filepath.WalkDir(filepath.Join(root, libc), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return add(rel, path)
	}); err != nil {
		return "", err
	}
	// Release attachments have flat names. Combine the license texts so users
	// can download and redistribute them without an additional archive.
	var notices strings.Builder
	notices.WriteString("wasitter third-party notices and licenses\n\nGrammar versions and source digests: grammar-registry.json\n\n")
	names := make([]string, 0, len(entries))
	for name := range entries {
		if name == "LICENSE" || strings.HasPrefix(name, "licenses/") || strings.HasPrefix(name, "internal/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&notices, "===== %s =====\n\n%s\n\n", name, entries[name])
		delete(entries, name)
	}
	entries["THIRD_PARTY_NOTICES.txt"] = []byte(notices.String())
	entries["grammar-registry.json"] = entries[DefaultRegistryRelativePath]
	delete(entries, DefaultRegistryRelativePath)
	entries["SHA256SUMS"] = []byte(sums.String())
	if _, err := os.Lstat(outputDir); err == nil {
		return "", fmt.Errorf("release asset directory already exists: %s; remove it or choose a new -output-dir", outputDir)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(outputDir), 0755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(outputDir), ".wasitter-assets-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	for name, data := range entries {
		if err := os.WriteFile(filepath.Join(staging, name), data, 0644); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staging, outputDir); err != nil {
		return "", err
	}
	return outputDir, nil
}
