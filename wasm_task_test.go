package sitterwasm_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWASMBuildTaskEntrypoint(t *testing.T) {
	miseConfig, err := os.ReadFile("mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	config := string(miseConfig)
	for _, task := range []string{
		"[tasks.wasm-build]",
		"[tasks.wasm-verify]",
		"[tasks.wasm-check]",
		"[tasks.\"build:grammar\"]",
		"[tasks.\"test:grammar\"]",
		"[tasks.\"check:grammar\"]",
	} {
		if !strings.Contains(config, task) {
			t.Fatalf("mise.toml does not define %s", task)
		}
	}
	if !strings.Contains(config, "go run ./cmd/sitterwasm-build") {
		t.Fatal("mise tasks do not use the Go build command")
	}
	if strings.Contains(config, ".sh") {
		t.Fatal("mise tasks must not depend on shell entrypoint scripts")
	}
	if _, err := os.Stat(filepath.Join("scripts", "grammar-registry.json")); err != nil {
		t.Fatalf("JSON grammar registry is unavailable: %v", err)
	}
}

func TestGrammarTestTaskRejectsUnknownLanguage(t *testing.T) {
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("cannot execute a host Go subprocess from this target")
	}
	cmd := exec.Command("go", "run", "./cmd/sitterwasm-build", "test-grammar", "not-a-registered-language")
	output, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("sitterwasm-build error = %v, want an exit error; output: %s", runErr, output)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("sitterwasm-build exit code = %d, want non-zero; output: %s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "not in the official grammar registry") {
		t.Fatalf("sitterwasm-build output = %q, want registry diagnostic", output)
	}
}

func TestGrammarCommandRejectsShellSyntaxAsLanguage(t *testing.T) {
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("cannot execute a host Go subprocess from this target")
	}
	marker := filepath.Join(t.TempDir(), "injected")
	malicious := "bad;touch " + marker
	cmd := exec.Command("go", "run", "./cmd/sitterwasm-build", "test-grammar", malicious)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatal("sitterwasm-build unexpectedly accepted shell syntax")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("language argument caused command injection (marker stat error: %v); output: %s", err, output)
	}
	if !strings.Contains(string(output), "invalid language name") {
		t.Fatalf("sitterwasm-build output = %q, want invalid-language diagnostic", output)
	}
}
