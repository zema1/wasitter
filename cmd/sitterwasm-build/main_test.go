package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunSubcommandHelp(t *testing.T) {
	err := run(context.Background(), []string{"build-grammar", "-h"})
	var help *helpError
	if !errors.As(err, &help) {
		t.Fatalf("run -h error = %v, want helpError", err)
	}
	if !strings.Contains(help.message, "sitterwasm-build build-grammar") {
		t.Fatalf("help output = %q", help.message)
	}
}

func TestReorderGrammarArgsAllowsFlagsAfterLanguage(t *testing.T) {
	flags, positional, err := reorderGrammarArgs([]string{"javascript", "-root", "/tmp/project", "--skip-image-build"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(positional, ","); got != "javascript" {
		t.Fatalf("positional = %q, want javascript", got)
	}
	if got := strings.Join(flags, " "); got != "-root /tmp/project --skip-image-build" {
		t.Fatalf("flags = %q", got)
	}
}

func TestContainerPathRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := containerPath(root, filepath.Join(root, "..", "outside"), "/workspace/default"); err == nil {
		t.Fatal("containerPath accepted path outside root")
	}
	got, err := containerPath(root, filepath.Join(root, "internal", "wasm"), "/workspace/default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/workspace/internal/wasm" {
		t.Fatalf("container path = %q", got)
	}
}

func TestAppendBindMountQuotesCSVMetacharacters(t *testing.T) {
	got, err := appendBindMount(nil, `/tmp/a,b\c`, "/workspace", true)
	if err != nil {
		t.Fatal(err)
	}
	source := `/tmp/a,b\c`
	if runtime.GOOS == "windows" {
		source = filepath.ToSlash(source)
	}
	want := []string{"--mount", `type=bind,"src=` + source + `",dst=/workspace,readonly`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("bind mount args = %#v, want %#v", got, want)
	}
}

func TestDockerMountFieldEscapesQuotesWithoutBackslashMangling(t *testing.T) {
	got := dockerMountField("src", `/tmp/a"b,c\d:e`)
	want := `"src=/tmp/a""b,c\d:e"`
	if got != want {
		t.Fatalf("Docker mount field = %q, want %q", got, want)
	}
}

func TestAppendBindMountUsesMountForSimplePath(t *testing.T) {
	got, err := appendBindMount(nil, "/tmp/project", "/workspace", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mount", "type=bind,src=/tmp/project,dst=/workspace"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("bind mount args = %#v, want %#v", got, want)
	}
}

func TestAppendBindMountRejectsNUL(t *testing.T) {
	if _, err := appendBindMount(nil, "bad\x00path", "/workspace", false); err == nil {
		t.Fatal("NUL-containing bind source was accepted")
	}
}

func TestHelperBinaryIsStaticLinux(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("requires a host that can execute Go toolchain subprocesses")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := helperBinary(context.Background(), filepath.Clean(filepath.Join(root, "..", "..")), "linux/"+runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4 || string(data[:4]) != "\x7fELF" {
		t.Fatalf("helper is not an ELF executable: %q", data[:min(len(data), 8)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
