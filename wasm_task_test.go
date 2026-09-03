package sitterwasm_test

import (
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
	for _, task := range []string{"[tasks.wasm-build]", "[tasks.wasm-verify]", "[tasks.wasm-check]"} {
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
	if output, err := exec.Command(sh, "-n", "scripts/build-wasm-docker.sh").CombinedOutput(); err != nil {
		t.Fatalf("Docker build helper has invalid shell syntax: %v\n%s", err, output)
	}
}
