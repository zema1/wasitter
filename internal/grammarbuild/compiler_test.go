package grammarbuild

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompileWASMUsesArgumentVectorAndPublishesAtomically(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("fake compiler uses a host POSIX executable")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	compiler := filepath.Join(tmp, "fake-zig")
	logPath := filepath.Join(tmp, "compiler.log")
	const fake = `#!/bin/sh
if [ "$1" = version ]; then
  echo 0.15.2
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_COMPILER_LOG"
compile=0
previous=
output=
for argument do
  if [ "$argument" = -c ]; then compile=1; fi
  if [ "$previous" = -o ]; then output=$argument; fi
  previous=$argument
done
if [ "$compile" -eq 1 ]; then
  : > "$output"
elif [ "${FAIL_LINK:-}" = 1 ]; then
  exit 17
else
  printf 'wasm' > "$output"
fi
`
	if err := os.WriteFile(compiler, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_COMPILER_LOG", logPath)
	output := filepath.Join(tmp, "grammar.wasm")
	if err := CompileWASM(context.Background(), CompilerOptions{
		Root:             root,
		GrammarDirectory: filepath.Join(root, "internal", "wasm", "third_party", "tree-sitter-json"),
		Parser:           "parser.c",
		LanguageFunction: "tree_sitter_json",
		LanguageName:     "json",
		Output:           output,
		CCompiler:        compiler,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "wasm" {
		t.Fatalf("output = %q", data)
	}
	if mode := mustFileMode(t, output); mode != 0o644 {
		t.Fatalf("output mode = %o, want 644", mode)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(lines) != 4 {
		t.Fatalf("compiler invocation count = %d, want 4; log:\n%s", len(lines), logData)
	}
	if !strings.Contains(lines[0], "cc -target wasm32-wasi") || !strings.Contains(lines[len(lines)-1], "-Wl,--export=tsw_abi_version") {
		t.Fatalf("unexpected compiler arguments:\n%s", logData)
	}

	sentinel := []byte("known-good")
	if err := os.WriteFile(output, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIL_LINK", "1")
	err = CompileWASM(context.Background(), CompilerOptions{
		Root:             root,
		GrammarDirectory: filepath.Join(root, "internal", "wasm", "third_party", "tree-sitter-json"),
		Parser:           "parser.c",
		LanguageFunction: "tree_sitter_json",
		LanguageName:     "json",
		Output:           output,
		CCompiler:        compiler,
	})
	if err == nil {
		t.Fatal("link failure unexpectedly succeeded")
	}
	got, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("existing output changed after failed link: got %q", got)
	}
}

func TestCompileWASMSelectsCXXForScanner(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("fake compiler uses a host POSIX executable")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	compiler := filepath.Join(tmp, "fake-zig")
	logPath := filepath.Join(tmp, "compiler.log")
	const fake = `#!/bin/sh
if [ "$1" = version ]; then echo 0.15.2; exit 0; fi
printf '%s\n' "$*" >> "$FAKE_COMPILER_LOG"
previous=
output=
compile=0
for argument do
  if [ "$argument" = -c ]; then compile=1; fi
  if [ "$previous" = -o ]; then output=$argument; fi
  previous=$argument
done
if [ "$compile" -eq 1 ]; then : > "$output"; else printf wasm > "$output"; fi
`
	if err := os.WriteFile(compiler, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	scanner := filepath.Join(tmp, "scanner.cc")
	if err := os.WriteFile(scanner, []byte("void scanner_probe() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(tmp, "grammar.wasm")
	t.Setenv("FAKE_COMPILER_LOG", logPath)
	if err := CompileWASM(context.Background(), CompilerOptions{
		Root:             root,
		GrammarDirectory: filepath.Join(root, "internal", "wasm", "third_party", "tree-sitter-json"),
		Parser:           "parser.c",
		ExtraSources:     []string{scanner},
		LanguageFunction: "tree_sitter_json",
		LanguageName:     "json",
		Output:           output,
		CCompiler:        compiler,
	}); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "c++ -target wasm32-wasi") {
		t.Fatalf("scanner was not compiled/linked with C++ driver; log:\n%s", logData)
	}
}

func TestCompilerArgsKeepCXXDriverSyntaxIndependent(t *testing.T) {
	common := []string{"/runtime", "/grammar", "/grammar/parser.c", "/include", "tree_sitter_demo", `"demo"`}
	zigArgs := compilerArgs(true, "/tool/zig", true, common[0], common[1], common[2], common[3], common[4], common[5])
	if len(zigArgs) < 3 || zigArgs[0] != "c++" || zigArgs[1] != "-target" || zigArgs[2] != "wasm32-wasi" {
		t.Fatalf("Zig C++ arguments begin with %q, want c++ -target wasm32-wasi", zigArgs[:minCompilerArgs(len(zigArgs), 3)])
	}
	clangArgs := compilerArgs(false, "/tool/clang++", true, common[0], common[1], common[2], common[3], common[4], common[5])
	if len(clangArgs) == 0 || clangArgs[0] != "--target=wasm32-wasi" {
		t.Fatalf("clang C++ arguments begin with %q, want --target=wasm32-wasi", clangArgs[:minCompilerArgs(len(clangArgs), 1)])
	}

	_, zigLink := linkerArgs(true, "/tool/zig", true, "/tool/zig", true, []string{"a.o"}, "out.wasm", false)
	if len(zigLink) < 3 || zigLink[0] != "c++" || zigLink[1] != "-target" || zigLink[2] != "wasm32-wasi" {
		t.Fatalf("Zig linker arguments begin with %q, want c++ -target wasm32-wasi", zigLink[:minCompilerArgs(len(zigLink), 3)])
	}
	_, clangLink := linkerArgs(true, "/tool/zig", true, "/tool/clang++", false, []string{"a.o"}, "out.wasm", false)
	if len(clangLink) == 0 || clangLink[0] != "--target=wasm32-wasi" {
		t.Fatalf("clang linker arguments begin with %q, want --target=wasm32-wasi", clangLink[:minCompilerArgs(len(clangLink), 1)])
	}
}

func minCompilerArgs(length, limit int) int {
	if length < limit {
		return length
	}
	return limit
}

func mustFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
