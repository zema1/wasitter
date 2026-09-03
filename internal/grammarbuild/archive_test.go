package grammarbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDownloadAndExtractVerifiesAndRejectsLinks(t *testing.T) {
	archive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-demo/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "tree-sitter-demo/src/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "tree-sitter-demo/src/parser.c", data: []byte("const int parser = 1;\n")},
	})
	digest := sha256.Sum256(archive)
	grammar := Grammar{
		Name:             "demo",
		Repository:       "tree-sitter/tree-sitter-demo",
		Tag:              "v1.2.3",
		ArchiveSHA256:    hex.EncodeToString(digest[:]),
		Parser:           "src/parser.c",
		LanguageFunction: "tree_sitter_demo",
		LanguageName:     "demo",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	root, err := DownloadAndExtract(context.Background(), grammar, t.TempDir(), DownloadOptions{
		Client:   server.Client(),
		URL:      server.URL + "/archive.tar.gz",
		Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src", "parser.c"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "const int parser = 1;\n" {
		t.Fatalf("extracted parser = %q", data)
	}

	linkArchive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-demo/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	linkDigest := sha256.Sum256(linkArchive)
	grammar.ArchiveSHA256 = hex.EncodeToString(linkDigest[:])
	linkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(linkArchive)
	}))
	defer linkServer.Close()
	if _, err := DownloadAndExtract(context.Background(), grammar, t.TempDir(), DownloadOptions{
		Client: server.Client(), URL: linkServer.URL, Attempts: 1,
	}); err == nil || !strings.Contains(err.Error(), "link type") {
		t.Fatalf("link archive error = %v, want link-type diagnostic", err)
	}
}

func TestDownloadAndExtractChecksumMismatch(t *testing.T) {
	archive := makeArchive(t, []tarEntry{{name: "root/file", data: []byte("hello")}})
	grammar := Grammar{
		Name:             "demo",
		Repository:       "tree-sitter/tree-sitter-demo",
		Tag:              "v1.2.3",
		ArchiveSHA256:    strings.Repeat("0", 64),
		Parser:           "file",
		LanguageFunction: "tree_sitter_demo",
		LanguageName:     "demo",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	_, err := DownloadAndExtract(context.Background(), grammar, t.TempDir(), DownloadOptions{
		Client: server.Client(), URL: server.URL, Attempts: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
}

func TestExtractTarGzRejectsNormalizedTraversal(t *testing.T) {
	archive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-demo/../escaped", data: []byte("must not be accepted")},
	})
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(archivePath, t.TempDir(), 0, 0); err == nil || !strings.Contains(err.Error(), "unsafe tar entry") {
		t.Fatalf("normalized traversal error = %v, want unsafe-entry diagnostic", err)
	}
}

func TestExtractTarGzCreatesTopLevelDirectoryEntry(t *testing.T) {
	archive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-empty/", mode: 0o755, typeflag: tar.TypeDir},
	})
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	root, err := ExtractTarGz(archivePath, destination, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("extracted root = %q, stat error = %v", root, err)
	}
}

func TestExtractTarGzRejectsPreexistingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated privileges on Windows")
	}
	archive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-demo/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "tree-sitter-demo/src/parser.c", data: []byte("must not escape")},
	})
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(destination, "tree-sitter-demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(archivePath, destination, 0, 0); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("preexisting symlink error = %v, want symlink diagnostic", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "src", "parser.c")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote through preexisting symlink: stat error = %v", err)
	}
}

func TestExtractTarGzRejectsSymlinkInDestinationParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated privileges on Windows")
	}
	archive := makeArchive(t, []tarEntry{
		{name: "tree-sitter-demo/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "tree-sitter-demo/src/parser.c", data: []byte("must not escape")},
	})
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(parent, "redirect")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(link, "extract")
	if _, err := ExtractTarGz(archivePath, destination, 0, 0); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("parent symlink error = %v, want symlink diagnostic", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "extract")); !os.IsNotExist(err) {
		t.Fatalf("extraction followed parent symlink: stat error = %v", err)
	}
}

type tarEntry struct {
	name     string
	data     []byte
	mode     int64
	typeflag byte
	linkname string
}

func makeArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: entry.name, Mode: mode, Size: int64(len(entry.data)), Typeflag: typeflag, Linkname: entry.linkname}
		if typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(entry.data) > 0 && typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, bytes.NewReader(entry.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
