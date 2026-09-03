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

func TestBuildScriptRejectsControlCharactersInLanguageName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the build helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}

	for name, languageName := range map[string]string{
		"newline": "bad\nname",
		"tab":     "bad\tname",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(sh, "./scripts/build-wasm.sh")
			// The validation runs before compilation. A deliberately missing
			// compiler path keeps this test independent of Zig and wasi-sdk while
			// also proving that the script rejects the name at its own boundary.
			cmd.Env = append(os.Environ(),
				"WASM_CC=/sitterwasm-test/missing-compiler",
				"SITTERWASM_LANGUAGE_NAME="+languageName,
			)
			output, runErr := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatalf("build script error = %v, want an exit error; output: %s", runErr, output)
			}
			if exitErr.ExitCode() != 2 {
				t.Fatalf("build script exit code = %d, want 2; output: %s", exitErr.ExitCode(), output)
			}
			if got := string(output); !strings.Contains(got, "SITTERWASM_LANGUAGE_NAME must not contain control characters") {
				t.Fatalf("build script output = %q, want language-name diagnostic", got)
			}
		})
	}
}

func TestBuildScriptUsesCXXForCXXScanner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the build helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}

	tmp := t.TempDir()
	compiler := filepath.Join(tmp, "fake zig")
	logPath := filepath.Join(tmp, "compiler.log")
	scanner := filepath.Join(tmp, "scanner.cc")
	output := filepath.Join(tmp, "custom.wasm")
	const fakeCompiler = `#!/bin/sh
if [ "$1" = version ]; then
  echo 0.15.2
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_COMPILER_LOG"
previous=
for argument do
  if [ "$previous" = -o ]; then
    : > "$argument"
  fi
  previous=$argument
done
`
	if err := os.WriteFile(compiler, []byte(fakeCompiler), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scanner, []byte("extern \"C\" void scanner_probe(void) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(sh, "./scripts/build-wasm.sh")
	cmd.Env = append(os.Environ(),
		"WASM_CC="+compiler,
		"GRAMMAR_EXTRA_SRC="+scanner,
		"OUT="+output,
		"FAKE_COMPILER_LOG="+logPath,
	)
	if buildOutput, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build script: %v\n%s", err, buildOutput)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var compiled, linked bool
	for _, invocation := range strings.Split(string(logData), "\n") {
		if !strings.HasPrefix(invocation, "c++ ") {
			continue
		}
		if strings.Contains(invocation, scanner) && strings.Contains(invocation, " -c ") {
			compiled = true
		}
		// The linker now writes to a sibling temporary file and publishes it
		// atomically, so the invocation must be recognized by its non-compilation
		// shape rather than by the final output pathname.
		if !strings.Contains(invocation, " -c ") && strings.Contains(invocation, " -o ") {
			linked = true
		}
	}
	if !compiled {
		t.Fatalf("C++ scanner was not compiled with the C++ frontend; log:\n%s", logData)
	}
	if !linked {
		t.Fatalf("C++ objects were not linked with the C++ frontend; log:\n%s", logData)
	}
	if info, err := os.Stat(output); err != nil || info.IsDir() {
		t.Fatalf("output artifact was not created: info=%v err=%v", info, err)
	}
}

func TestBuildScriptPreservesOutputWhenLinkFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the build helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}

	tmp := t.TempDir()
	compiler := filepath.Join(tmp, "fake-compiler")
	output := filepath.Join(tmp, "existing.wasm")
	sentinel := []byte("known-good-artifact")
	if err := os.WriteFile(output, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	const fakeCompiler = `#!/bin/sh
if [ "$1" = version ]; then
  echo 0.15.2
  exit 0
fi
compile=0
for argument do
  if [ "$argument" = -c ]; then compile=1; fi
done
if [ "$compile" -eq 1 ]; then
  previous=
  for argument do
    if [ "$previous" = -o ]; then : > "$argument"; fi
    previous=$argument
  done
  exit 0
fi
# Simulate a linker failure without touching its -o path.
exit 17
`
	if err := os.WriteFile(compiler, []byte(fakeCompiler), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(sh, "./scripts/build-wasm.sh")
	cmd.Env = append(os.Environ(), "WASM_CC="+compiler, "OUT="+output)
	if runErr := cmd.Run(); runErr == nil {
		t.Fatal("build script unexpectedly succeeded")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 17 {
			t.Fatalf("build script error = %v, want linker exit 17", runErr)
		}
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("existing output changed after failed link: got %q, want %q", got, sentinel)
	}
	if leftovers, err := filepath.Glob(filepath.Join(tmp, ".sitterwasm-wasm.*")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Fatalf("temporary output files remain after failed link: %v", leftovers)
	}
}
