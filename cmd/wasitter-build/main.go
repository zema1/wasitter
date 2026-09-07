// Command wasitter-build drives reproducible Tree-sitter grammar builds.
//
// The host-side commands build the pinned Docker image and mount this same
// binary into it. The in-container command then performs registry lookup,
// source download/checksum verification, safe extraction, and direct C/C++
// compiler invocation. Keeping the network and compiler work in the container
// makes the workflow independent of the host toolchain while avoiding shell
// interpolation of registry data.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/zema1/wasitter/internal/grammarbuild"
)

const defaultImage = "wasitter-wasm-builder:zig-0.15.2"

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

type helpError struct{ message string }

func (e *helpError) Error() string { return e.message }

func flagParseError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return &helpError{message: usage()}
	}
	return &usageError{message: err.Error()}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		var help *helpError
		if errors.As(err, &help) {
			fmt.Fprintln(os.Stdout, help.message)
			return
		}
		code := 1
		var usage *usageError
		if errors.As(err, &usage) {
			code = 2
		}
		if errors.Is(err, context.Canceled) {
			code = 130
		}
		fmt.Fprintf(os.Stderr, "wasitter-build: %v\n", err)
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &usageError{message: usage()}
	}
	switch args[0] {
	case "build-grammars":
		return runBuildGrammars(ctx, args[1:])
	case "prepare-grammar-assets":
		return runPrepareGrammarAssets(args[1:])
	case "build-grammar":
		return runBuildGrammar(ctx, args[1:])
	case "test-grammar", "verify-grammar":
		return runVerifyGrammar(args[1:])
	case "check-grammar":
		return runCheckGrammar(ctx, args[1:])
	case "verify-wasm":
		return runVerifyWASM(args[1:])
	case "in-container-build-grammar":
		return runInContainerBuildGrammar(ctx, args[1:])
	case "build-wasm":
		return runBuildWASM(ctx, args[1:])
	case "in-container-build-wasm":
		return runInContainerBuildWASM(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stdout, usage())
		return nil
	default:
		return &usageError{message: fmt.Sprintf("unknown command %q\n\n%s", args[0], usage())}
	}
}

func usage() string {
	return `usage:
  wasitter-build build-grammar [options] <language>
  wasitter-build build-grammars [-root path] [-output-dir path]
  wasitter-build prepare-grammar-assets [-root path] [-artifact-dir path] [-output-dir path]
  wasitter-build test-grammar [options] <language>
  wasitter-build check-grammar [options] <language>
  wasitter-build verify-wasm [options]

Host commands build through Docker. Internal commands are used by the mounted
helper inside that image:
  wasitter-build in-container-build-grammar [options] <language>
  wasitter-build in-container-build-wasm [options]

Options common to grammar commands:
  -root <path>       repository root (default: discover from current directory)
  -registry <path>   registry JSON (default: scripts/grammar-registry.json)
  -image <name>      Docker image tag
  -platform <value>  Docker platform, for example linux/amd64
  -cc <path|name>    C compiler override (primarily for in-container use)
  -cxx <path|name>   C++ compiler override
  -output-dir <path> artifact directory (default: internal/wasm/assets)
`
}

func runVerifyWASM(args []string) error {
	fs := flag.NewFlagSet("verify-wasm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootArg := fs.String("root", "", "repository root")
	outputArg := fs.String("artifact", "", "WASM artifact path")
	checksumArg := fs.String("checksum", "", "artifact SHA-256 sidecar path")
	if err := fs.Parse(args); err != nil {
		return flagParseError(err)
	}
	if fs.NArg() != 0 {
		return &usageError{message: "verify-wasm does not accept positional arguments"}
	}
	root, err := resolveRoot(*rootArg)
	if err != nil {
		return err
	}
	artifact := *outputArg
	if artifact == "" {
		artifact = filepath.Join(root, "internal", "wasm", "assets", "wasitter-json.wasm")
	} else if !filepath.IsAbs(artifact) {
		artifact = filepath.Join(root, artifact)
	}
	checksum := *checksumArg
	if checksum == "" {
		checksum = artifact + ".sha256"
	} else if !filepath.IsAbs(checksum) {
		checksum = filepath.Join(root, checksum)
	}
	digest, err := grammarbuild.VerifyArtifact(artifact, checksum)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "verified %s (%s)\n", artifact, digest)
	return nil
}

type grammarFlags struct {
	root            string
	registry        string
	image           string
	platform        string
	dockerfile      string
	ccompiler       string
	cxxcompiler     string
	output          string
	sourceURL       string
	noImageBuild    bool
	includeLicenses bool
}

func parseGrammarFlags(name string, args []string, allowDocker bool) (grammarFlags, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var options grammarFlags
	fs.StringVar(&options.root, "root", "", "repository root")
	fs.StringVar(&options.registry, "registry", "", "registry JSON path")
	fs.StringVar(&options.image, "image", defaultImage, "Docker image tag")
	fs.StringVar(&options.platform, "platform", "", "Docker platform")
	fs.StringVar(&options.dockerfile, "dockerfile", "", "Dockerfile path")
	fs.StringVar(&options.ccompiler, "cc", "", "C compiler executable (in-container mode)")
	fs.StringVar(&options.cxxcompiler, "cxx", "", "C++ compiler executable (in-container mode)")
	fs.StringVar(&options.output, "output-dir", "", "artifact output directory")
	fs.BoolVar(&options.includeLicenses, "include-licenses", false, "copy upstream licenses alongside artifacts")
	fs.StringVar(&options.sourceURL, "source-url", "", "archive URL override (testing/mirror)")
	if allowDocker {
		fs.BoolVar(&options.noImageBuild, "skip-image-build", false, "do not run docker build")
	}
	// Go's flag package stops parsing at the first positional argument. Mise
	// usually places flags first, but accepting either ordering makes the
	// command friendlier when invoked directly (`... javascript -root .`) and
	// avoids accidentally treating a safe option as a language argument.
	ordered, positional, err := reorderGrammarArgs(args, allowDocker)
	if err != nil {
		return grammarFlags{}, "", &usageError{message: err.Error()}
	}
	if err := fs.Parse(append(ordered, positional...)); err != nil {
		return grammarFlags{}, "", flagParseError(err)
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return grammarFlags{}, "", &usageError{message: fmt.Sprintf("%s requires exactly one language argument", name)}
	}
	if options.root == "" {
		root, err := discoverRoot()
		if err != nil {
			return grammarFlags{}, "", err
		}
		options.root = root
	} else {
		root, err := filepath.Abs(options.root)
		if err != nil {
			return grammarFlags{}, "", fmt.Errorf("resolve root: %w", err)
		}
		options.root = root
	}
	registryInput := options.registry
	if registryInput == "" {
		registryInput = grammarbuild.DefaultRegistryRelativePath
	}
	if !filepath.IsAbs(registryInput) {
		clean := filepath.Clean(registryInput)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return grammarFlags{}, "", &usageError{message: "-registry must stay inside repository root"}
		}
	}
	options.registry = grammarbuild.ResolveRegistryPath(options.root, registryInput)
	return options, remaining[0], nil
}

// reorderGrammarArgs separates the single language positional from flags so
// both `flags language` and `language flags` forms are accepted. Unknown flags
// are left for flag.FlagSet to diagnose; known value-taking flags consume the
// following token before it can be mistaken for the language.
func reorderGrammarArgs(args []string, allowDocker bool) (flags, positional []string, err error) {
	valueFlags := map[string]bool{
		"-root": true, "--root": true,
		"-registry": true, "--registry": true,
		"-image": true, "--image": true,
		"-platform": true, "--platform": true,
		"-dockerfile": true, "--dockerfile": true,
		"-cc": true, "--cc": true,
		"-cxx": true, "--cxx": true,
		"-output-dir": true, "--output-dir": true,
		"-source-url": true, "--source-url": true,
	}
	boolFlags := map[string]bool{"-include-licenses": true, "--include-licenses": true}
	if allowDocker {
		boolFlags["-skip-image-build"], boolFlags["--skip-image-build"] = true, true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := arg
		if before, _, found := strings.Cut(arg, "="); found {
			name = before
		}
		if valueFlags[name] && !strings.Contains(arg, "=") {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			flags = append(flags, args[i])
		} else if boolFlags[name] {
			// no following value
		}
	}
	return flags, positional, nil
}

func loadGrammar(options grammarFlags, language string) (grammarbuild.Registry, grammarbuild.Grammar, error) {
	registry, err := grammarbuild.LoadRegistry(options.registry)
	if err != nil {
		return grammarbuild.Registry{}, grammarbuild.Grammar{}, err
	}
	grammar, err := registry.Lookup(language)
	if err != nil {
		return grammarbuild.Registry{}, grammarbuild.Grammar{}, &usageError{message: err.Error()}
	}
	return registry, grammar, nil
}

func runBuildGrammar(ctx context.Context, args []string) error {
	options, language, err := parseGrammarFlags("build-grammar", args, true)
	if err != nil {
		return err
	}
	_, grammar, err := loadGrammar(options, language)
	if err != nil {
		return err
	}
	return runDockerGrammar(ctx, options, grammar)
}

func runCheckGrammar(ctx context.Context, args []string) error {
	options, language, err := parseGrammarFlags("check-grammar", args, true)
	if err != nil {
		return err
	}
	_, grammar, err := loadGrammar(options, language)
	if err != nil {
		return err
	}
	if err := runDockerGrammar(ctx, options, grammar); err != nil {
		return err
	}
	return verifyGrammar(options, grammar)
}

func runVerifyGrammar(args []string) error {
	options, language, err := parseGrammarFlags("test-grammar", args, false)
	if err != nil {
		return err
	}
	_, grammar, err := loadGrammar(options, language)
	if err != nil {
		return err
	}
	return verifyGrammar(options, grammar)
}

func verifyGrammar(options grammarFlags, grammar grammarbuild.Grammar) error {
	outputDir := options.output
	if outputDir == "" {
		outputDir = filepath.Join(options.root, "internal", "wasm", "assets")
	} else if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(options.root, outputDir)
	}
	outputDir = filepath.Clean(outputDir)
	artifact := filepath.Join(outputDir, "wasitter-"+grammar.Name+".wasm")
	digest, err := grammarbuild.VerifyArtifact(artifact, artifact+".sha256")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "grammar %s: verified %s (%s)\n", grammar.Name, artifact, digest)
	return nil
}

func runDockerGrammar(ctx context.Context, options grammarFlags, grammar grammarbuild.Grammar) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("Docker is required: %w", err)
	}
	if options.image == "" {
		return &usageError{message: "-image must not be empty"}
	}
	dockerfile := options.dockerfile
	if dockerfile == "" {
		dockerfile = filepath.Join(options.root, "docker", "wasm-builder", "Dockerfile")
	} else if !filepath.IsAbs(dockerfile) {
		// CLI paths are rooted at the selected checkout, just like registry and
		// output paths. This keeps invocations from a nested working directory
		// deterministic.
		dockerfile = filepath.Join(options.root, dockerfile)
	}
	dockerfile = filepath.Clean(dockerfile)
	if !options.noImageBuild {
		buildArgs := []string{"build", "--pull=false", "--tag", options.image, "-f", dockerfile, filepath.Dir(dockerfile)}
		if options.platform != "" {
			buildArgs = []string{"build", "--platform", options.platform, "--pull=false", "--tag", options.image, "-f", dockerfile, filepath.Dir(dockerfile)}
		}
		if err := runCommand(ctx, "docker", buildArgs, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
	}
	helper, cleanup, err := helperBinary(ctx, options.root, options.platform)
	if err != nil {
		return err
	}
	defer cleanup()
	registryInside := options.registry
	if filepath.IsAbs(registryInside) {
		// All host paths under root are mounted at /workspace. Convert the
		// registry path without using string interpolation in a shell command.
		if rel, relErr := filepath.Rel(options.root, registryInside); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			registryInside = filepath.ToSlash(filepath.Join("/workspace", rel))
		} else {
			return fmt.Errorf("registry path %s must be inside repository root", registryInside)
		}
	}
	outputInside, err := containerPath(options.root, options.output, "/workspace/internal/wasm/assets")
	if err != nil {
		return err
	}
	ccInside, err := containerExecutable(options.root, options.ccompiler)
	if err != nil {
		return err
	}
	cxxInside, err := containerExecutable(options.root, options.cxxcompiler)
	if err != nil {
		return err
	}
	args := []string{"run", "--rm", "--init", "--workdir", "/workspace"}
	if options.platform != "" {
		args = append(args, "--platform", options.platform)
	}
	var mountArgs []string
	mountArgs, err = appendBindMount(mountArgs, options.root, "/workspace", false)
	if err != nil {
		return err
	}
	mountArgs, err = appendBindMount(mountArgs, helper, "/usr/local/bin/wasitter-build", true)
	if err != nil {
		return err
	}
	args = append(args, mountArgs...)
	if uid, gid, ok := currentNumericUser(); ok {
		args = append(args, "--user", uid+":"+gid)
	}
	args = append(args,
		"--entrypoint", "/usr/local/bin/wasitter-build",
		options.image,
		"in-container-build-grammar",
		"-root", "/workspace",
		"-registry", registryInside,
		"-output-dir", outputInside,
	)
	if ccInside != "" {
		args = append(args, "-cc", ccInside)
	}
	if cxxInside != "" {
		args = append(args, "-cxx", cxxInside)
	}
	if options.sourceURL != "" {
		args = append(args, "-source-url", options.sourceURL)
	}
	if options.includeLicenses {
		args = append(args, "-include-licenses")
	}
	args = append(args, grammar.Name)
	if err := runCommand(ctx, "docker", args, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	return nil
}

func runInContainerBuildGrammar(ctx context.Context, args []string) error {
	options, language, err := parseGrammarFlags("in-container-build-grammar", args, false)
	if err != nil {
		return err
	}
	_, grammar, err := loadGrammar(options, language)
	if err != nil {
		return err
	}
	result, err := grammarbuild.BuildGrammar(ctx, grammar, grammarbuild.BuildOptions{
		Root:            options.root,
		CCompiler:       options.ccompiler,
		CXXCompiler:     options.cxxcompiler,
		OutputDirectory: options.output,
		IncludeLicenses: options.includeLicenses,
		Download: grammarbuild.DownloadOptions{
			URL: options.sourceURL,
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "grammar %s: wrote %s (%s)\n", result.Language, result.ArtifactPath, result.ArtifactSHA256)
	return nil
}

func runBuildWASM(ctx context.Context, args []string) error {
	// The bundled JSON source is checked into the repository, so this path only
	// needs to route the pinned Docker lifecycle and direct compiler invocation.
	fs := flag.NewFlagSet("build-wasm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootArg := fs.String("root", "", "repository root")
	image := fs.String("image", defaultImage, "Docker image tag")
	platform := fs.String("platform", "", "Docker platform")
	if err := fs.Parse(args); err != nil {
		return flagParseError(err)
	}
	if fs.NArg() != 0 {
		return &usageError{message: "build-wasm does not accept positional arguments"}
	}
	if *image == "" {
		return &usageError{message: "-image must not be empty"}
	}
	root, err := resolveRoot(*rootArg)
	if err != nil {
		return err
	}
	return runDockerBundledWASM(ctx, root, *image, *platform)
}

func resolveRoot(value string) (string, error) {
	if value == "" {
		return discoverRoot()
	}
	root, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	return root, nil
}

func runInContainerBuildWASM(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("in-container-build-wasm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", "/workspace", "repository root")
	ccompiler := fs.String("cc", "", "C compiler executable")
	cxxcompiler := fs.String("cxx", "", "C++ compiler executable")
	if err := fs.Parse(args); err != nil {
		return flagParseError(err)
	}
	if fs.NArg() != 0 {
		return &usageError{message: "in-container-build-wasm does not accept positional arguments"}
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if err := grammarbuild.CompileWASM(ctx, grammarbuild.CompilerOptions{
		Root:        rootAbs,
		CCompiler:   *ccompiler,
		CXXCompiler: *cxxcompiler,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	}); err != nil {
		return fmt.Errorf("build bundled WASM: %w", err)
	}
	artifact := filepath.Join(rootAbs, "internal", "wasm", "assets", "wasitter-json.wasm")
	if digest, checksum, err := grammarbuild.WriteArtifactChecksum(artifact); err != nil {
		return fmt.Errorf("publish bundled WASM checksum: %w", err)
	} else {
		fmt.Fprintf(os.Stdout, "wrote %s (%s)\n", checksum, digest)
	}
	return nil
}

func runDockerBundledWASM(ctx context.Context, root, image, platform string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("Docker is required: %w", err)
	}
	if image == "" {
		return &usageError{message: "-image must not be empty"}
	}
	dockerfile := filepath.Join(root, "docker", "wasm-builder", "Dockerfile")
	buildArgs := []string{"build", "--pull=false", "--tag", image, "-f", dockerfile, filepath.Dir(dockerfile)}
	if platform != "" {
		buildArgs = []string{"build", "--platform", platform, "--pull=false", "--tag", image, "-f", dockerfile, filepath.Dir(dockerfile)}
	}
	if err := runCommand(ctx, "docker", buildArgs, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	helper, cleanup, err := helperBinary(ctx, root, platform)
	if err != nil {
		return err
	}
	defer cleanup()
	// Bundled WASM builds use only sources mounted from the checkout and the
	// compiler already present in the pinned image. Disable networking so a
	// build cannot accidentally depend on (or mutate) external state.
	runArgs := []string{"run", "--rm", "--init", "--network", "none", "--workdir", "/workspace"}
	if platform != "" {
		runArgs = append(runArgs, "--platform", platform)
	}
	var mountArgs []string
	mountArgs, err = appendBindMount(mountArgs, root, "/workspace", false)
	if err != nil {
		return err
	}
	mountArgs, err = appendBindMount(mountArgs, helper, "/usr/local/bin/wasitter-build", true)
	if err != nil {
		return err
	}
	runArgs = append(runArgs, mountArgs...)
	if uid, gid, ok := currentNumericUser(); ok {
		runArgs = append(runArgs, "--user", uid+":"+gid)
	}
	runArgs = append(runArgs,
		"--entrypoint", "/usr/local/bin/wasitter-build", image,
		"in-container-build-wasm", "-root", "/workspace",
	)
	if err := runCommand(ctx, "docker", runArgs, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func discoverRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "scripts", "grammar-registry.json")); err == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not discover repository root; pass -root")
}

func helperBinary(ctx context.Context, root, platform string) (string, func(), error) {
	arch := runtime.GOARCH
	if platform != "" {
		parts := strings.Split(platform, "/")
		if len(parts) != 2 || parts[0] != "linux" {
			return "", func() {}, &usageError{message: "-platform must be linux/<arch> for the mounted Go helper"}
		}
		arch = parts[1]
	}
	if arch != "amd64" && arch != "arm64" && arch != "386" && arch != "riscv64" {
		return "", func() {}, fmt.Errorf("unsupported helper architecture %q", arch)
	}
	// Always build a static Linux helper. The host process may be a dynamically
	// linked glibc binary (including the one produced by `go run`), which would
	// fail to start in the Alpine builder image. Keeping the temporary file in
	// the checkout also makes it visible to Docker Desktop/remote daemons that
	// only share the repository directory.
	tmpDir := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create helper directory: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "wasitter-build-helper-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create helper path: %w", err)
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	_ = os.Remove(path)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, "./cmd/wasitter-build")
	cmd.Dir = root
	cmd.Env = replaceEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      arch,
	})
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, fmt.Errorf("build Linux helper: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() {
		_ = os.Remove(path)
		// Keep the checkout tidy when this was the last helper build. A
		// concurrent invocation may still own another file; in that case the
		// directory removal simply has no effect.
		_ = os.Remove(tmpDir)
	}, nil
}

func replaceEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, replaced := values[key]; replaced {
				continue
			}
		}
		result = append(result, item)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

// appendBindMount appends the Docker arguments needed for a host bind mount.
// Docker's --mount parser uses encoding/csv semantics. In particular, a comma
// cannot be escaped with a backslash (it would become part of the path), and a
// quote must surround the complete key=value field rather than only its value.
// Encode fields accordingly so commas, quotes, drive-letter colons, and
// backslashes in a host path all survive the Docker CLI unchanged. Windows
// paths are normalized to forward slashes before being encoded.
func appendBindMount(args []string, source, destination string, readOnly bool) ([]string, error) {
	if source == "" {
		return nil, errors.New("bind mount source must not be empty")
	}
	if strings.IndexByte(source, 0) >= 0 || strings.IndexByte(destination, 0) >= 0 {
		return nil, errors.New("bind mount paths must not contain NUL")
	}
	if runtime.GOOS == "windows" {
		source = filepath.ToSlash(source)
	}
	mount := "type=bind," + dockerMountField("src", source) + "," + dockerMountField("dst", destination)
	if readOnly {
		mount += ",readonly"
	}
	return append(args, "--mount", mount), nil
}

// dockerMountField returns one CSV field in Docker's --mount grammar. The
// field includes its key so a comma in a value cannot terminate the key/value
// pair. This is deliberately not strconv.Quote: Go string escapes such as
// `\n` and `\\` are not interpreted by Docker's CSV reader.
func dockerMountField(key, value string) string {
	field := key + "=" + value
	if !strings.ContainsAny(field, ",\"\r\n") {
		return field
	}
	return `"` + strings.ReplaceAll(field, `"`, `""`) + `"`
}

// containerPath maps a host path under root to the corresponding /workspace
// path. Empty values use fallback. Paths outside the mounted checkout are
// rejected because passing them to the container would either silently target
// an unmounted location or require an additional, surprising bind mount.
func containerPath(root, value, fallback string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	// Command-line paths are conventionally relative to the selected
	// repository root, even when the command itself is launched from a nested
	// directory. Resolve them against root before checking the bind mount.
	if !filepath.IsAbs(value) {
		value = filepath.Join(root, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", value, err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s must be inside repository root", value)
	}
	return filepath.ToSlash(filepath.Join("/workspace", rel)), nil
}

// containerExecutable maps an explicitly path-based compiler override into
// the checkout mount, while leaving a bare command name (for example `zig` or
// `clang`) for the image's PATH to resolve. This keeps the common CLI usage
// simple and still supports reproducible custom toolchain paths.
func containerExecutable(root, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if filepath.IsAbs(value) || strings.ContainsAny(value, `/\\`) {
		return containerPath(root, value, "")
	}
	return value, nil
}

func currentNumericUser() (uid, gid string, ok bool) {
	if runtime.GOOS == "windows" {
		return "", "", false
	}
	u, err := user.Current()
	if err != nil {
		return "", "", false
	}
	if _, err := strconv.Atoi(u.Uid); err != nil {
		return "", "", false
	}
	if _, err := strconv.Atoi(u.Gid); err != nil {
		return "", "", false
	}
	return u.Uid, u.Gid, true
}
