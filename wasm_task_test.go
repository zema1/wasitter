package sitterwasm_test

import (
	"errors"
	"os"
	"os/exec"
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
	if !strings.Contains(config, "build-wasm-docker.sh") {
		t.Fatal("wasm-build task does not use the Docker entrypoint")
	}

	if runtime.GOOS == "windows" {
		t.Skip("the build helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	for _, script := range []string{
		"scripts/build-wasm-docker.sh",
		"scripts/build-wasm.sh",
		"scripts/verify-wasm.sh",
		"scripts/build-grammar-docker.sh",
		"scripts/build-grammar-in-container.sh",
		"scripts/test-grammar.sh",
	} {
		if output, err := exec.Command(sh, "-n", script).CombinedOutput(); err != nil {
			t.Fatalf("%s has invalid shell syntax: %v\n%s", script, err, output)
		}
	}
}

func TestGrammarTestTaskRejectsUnknownLanguage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the grammar helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}

	cmd := exec.Command(sh, "scripts/test-grammar.sh", "not-a-registered-language")
	output, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("test-grammar error = %v, want an exit error; output: %s", runErr, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("test-grammar exit code = %d, want 2; output: %s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "not in the official grammar registry") {
		t.Fatalf("test-grammar output = %q, want registry diagnostic", output)
	}
}
