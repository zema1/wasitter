// Package grammarbuild contains the reproducible build plumbing for official
// Tree-sitter grammar artifacts.  It deliberately has no dependency on the
// public wasitter package so the command can be cross-compiled as a small,
// CGO-free helper and mounted into the Docker builder image.
package grammarbuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	// RegistryVersion is the schema version written by the repository registry.
	RegistryVersion = 1
	// DefaultRegistryRelativePath is resolved relative to the repository root.
	DefaultRegistryRelativePath = "scripts/grammar-registry.json"
)

var (
	languageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	repositoryPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	tagPattern          = regexp.MustCompile(`^v[0-9]+(?:\.[0-9]+){2}(?:[-+][A-Za-z0-9_.-]+)?$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	cIdentifierPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Registry is the versioned JSON document consumed by the build command.
// Keeping the top-level object explicit leaves room for adding metadata while
// retaining a useful schema/version check.
type Registry struct {
	Version  int       `json:"version"`
	Grammars []Grammar `json:"grammars"`
}

// Grammar describes one generated parser in an official Tree-sitter release.
// Scanner is a list because some grammars have more than one external scanner
// source.  An empty list means that the parser has no external scanner.
type Grammar struct {
	Name          string `json:"name"`
	Repository    string `json:"repository"`
	Tag           string `json:"tag"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Parser        string `json:"parser"`
	// Scanner is retained as the short spelling used by the first registry
	// format. New documents may use Scanners; the loader accepts either spelling
	// (but never both in one row).
	Scanner          []string `json:"scanner,omitempty"`
	Scanners         []string `json:"scanners,omitempty"`
	LanguageFunction string   `json:"language_function"`
	LanguageName     string   `json:"language_name"`

	// Preserve whether each spelling was present in JSON, including an
	// explicitly empty array. This lets validation reject both keys reliably.
	scannerPresent  bool `json:"-"`
	scannersPresent bool `json:"-"`
}

// UnmarshalJSON accepts the legacy singular scanner key while keeping the
// plural `scanners` spelling canonical. Decoding is strict so direct callers
// get the same typo protection as LoadRegistry.
func (g *Grammar) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	type plain Grammar
	var decoded plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	for _, key := range []string{"scanner", "scanners"} {
		if raw, present := keys[key]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must be an array, not null", key)
		}
	}
	*g = Grammar(decoded)
	_, g.scannerPresent = keys["scanner"]
	_, g.scannersPresent = keys["scanners"]
	return nil
}

// ScannerPaths returns the normalized external-scanner list regardless of
// whether a registry row used the legacy `scanner` or plural `scanners` key.
func (g Grammar) ScannerPaths() []string {
	if g.scannersPresent || g.Scanners != nil {
		return append([]string(nil), g.Scanners...)
	}
	return append([]string(nil), g.Scanner...)
}

// LoadRegistry reads and validates a registry JSON document. Unknown fields
// are rejected so a typo in a security-sensitive pin cannot silently fall
// back to a zero value.
func LoadRegistry(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, fmt.Errorf("read registry %s: %w", path, err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Registry{}, fmt.Errorf("decode registry %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var registry Registry
	if err := dec.Decode(&registry); err != nil {
		return Registry{}, fmt.Errorf("decode registry %s: %w", path, err)
	}
	// A second decode makes trailing JSON values (including two adjacent
	// objects) an error rather than silently accepting a partial document.
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return Registry{}, fmt.Errorf("decode registry %s: trailing JSON value", path)
	} else if !errors.Is(err, io.EOF) {
		return Registry{}, fmt.Errorf("decode registry %s: trailing data: %w", path, err)
	}
	if err := registry.Validate(); err != nil {
		return Registry{}, fmt.Errorf("validate registry %s: %w", path, err)
	}
	return registry, nil
}

// rejectDuplicateJSONKeys performs a small streaming walk before the normal
// struct decode. encoding/json intentionally accepts duplicate object members
// and keeps the last value; that behavior is surprising for a pinned registry
// because a second archive digest or source path could silently replace the
// reviewed value. The walk also rejects a second top-level JSON value.
func rejectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(dec, "$"); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func scanJSONValue(dec *json.Decoder, location string) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object member at %s is not a string key", location)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec, location+"."+key); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s was not closed", location)
		}
	case '[':
		index := 0
		for dec.More() {
			if err := scanJSONValue(dec, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s was not closed", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delim, location)
	}
	return nil
}

// Validate checks every row and rejects duplicate names. It is exported so
// callers generating a registry programmatically can apply the same checks.
func (r Registry) Validate() error {
	if r.Version != RegistryVersion {
		return fmt.Errorf("unsupported registry version %d (want %d)", r.Version, RegistryVersion)
	}
	if len(r.Grammars) == 0 {
		return errors.New("registry contains no grammars")
	}
	seen := make(map[string]struct{}, len(r.Grammars))
	for i, grammar := range r.Grammars {
		if err := grammar.Validate(); err != nil {
			return fmt.Errorf("grammar %d: %w", i, err)
		}
		key := strings.ToLower(grammar.Name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate grammar name %q", grammar.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Validate checks a single grammar row. Paths are slash-separated archive
// paths; accepting backslashes would make validation differ across hosts and
// could turn a harmless-looking path into traversal on Windows.
func (g Grammar) Validate() error {
	if !languageNamePattern.MatchString(g.Name) {
		return fmt.Errorf("name %q is not a safe language name", g.Name)
	}
	if strings.ToLower(g.Name) != g.Name {
		return fmt.Errorf("name %q must be lowercase", g.Name)
	}
	if !repositoryPattern.MatchString(g.Repository) {
		return fmt.Errorf("repository %q is not a valid GitHub owner/repository", g.Repository)
	}
	owner := strings.SplitN(g.Repository, "/", 2)[0]
	if owner != "tree-sitter" && owner != "tree-sitter-grammars" {
		return fmt.Errorf("repository %q is not an official Tree-sitter repository", g.Repository)
	}
	if strings.Contains(g.Repository, "..") {
		return fmt.Errorf("repository %q contains a traversal component", g.Repository)
	}
	if !tagPattern.MatchString(g.Tag) {
		return fmt.Errorf("tag %q is not a pinned release tag", g.Tag)
	}
	if !sha256Pattern.MatchString(g.ArchiveSHA256) {
		return fmt.Errorf("archive_sha256 for %q is not a 64-character hex digest", g.Name)
	}
	if err := validateArchivePath(g.Parser, false); err != nil {
		return fmt.Errorf("parser path: %w", err)
	}
	if (g.scannerPresent && g.scannersPresent) || (g.Scanner != nil && g.Scanners != nil) {
		return errors.New("scanner and scanners cannot both be set")
	}
	for i, scanner := range g.ScannerPaths() {
		if err := validateArchivePath(scanner, false); err != nil {
			return fmt.Errorf("scanner path %d: %w", i, err)
		}
	}
	if !cIdentifierPattern.MatchString(g.LanguageFunction) {
		return fmt.Errorf("language_function %q is not a C identifier", g.LanguageFunction)
	}
	if err := validateLanguageString(g.LanguageName); err != nil {
		return fmt.Errorf("language_name: %w", err)
	}
	return nil
}

// Lookup returns a grammar by its canonical lower-case name. User input is
// trimmed and lower-cased for convenience, but any other characters are
// rejected by the same validation used for registry rows.
func (r Registry) Lookup(name string) (Grammar, error) {
	canonical := strings.ToLower(strings.TrimSpace(name))
	if !languageNamePattern.MatchString(canonical) || strings.ContainsAny(name, "\r\n\t") {
		return Grammar{}, fmt.Errorf("invalid language name %q", name)
	}
	for _, grammar := range r.Grammars {
		if grammar.Name == canonical {
			return grammar, nil
		}
	}
	return Grammar{}, fmt.Errorf("language %q is not in the official grammar registry", name)
}

func validateLanguageString(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func validateArchivePath(value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("must not be empty")
	}
	if strings.IndexByte(value, 0) >= 0 || strings.Contains(value, "\\") {
		return fmt.Errorf("%q contains an invalid path separator", value)
	}
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("%q must be relative", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q contains an unsafe path component", value)
		}
	}
	return nil
}

// ResolveRegistryPath resolves a user-provided path against root. Absolute
// paths remain absolute; relative paths are kept rooted in the checkout so a
// command launched from another working directory behaves deterministically.
func ResolveRegistryPath(root, path string) string {
	if path == "" {
		path = DefaultRegistryRelativePath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, filepath.Clean(path))
}
