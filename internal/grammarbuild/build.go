package grammarbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BuildOptions controls an in-container grammar build. The zero value uses
// the repository's standard paths and invokes CompileWASM directly. Compiler
// and linker arguments are constructed as an argv vector, so registry values
// and source paths are never interpreted by a shell.
type BuildOptions struct {
	Root            string
	CCompiler       string
	CXXCompiler     string
	OutputDirectory string
	Download        DownloadOptions
	Stdout          io.Writer
	Stderr          io.Writer
}

// BuildResult describes the files published by BuildGrammar.
type BuildResult struct {
	Language       string
	ArtifactPath   string
	ChecksumPath   string
	ArtifactSHA256 string
	ArchiveURL     string
}

// BuildGrammar downloads, verifies, extracts, compiles and atomically
// publishes one grammar artifact. It is intended to run inside the pinned
// Docker image; callers on the host should use the command's Docker mode.
func BuildGrammar(ctx context.Context, grammar Grammar, options BuildOptions) (BuildResult, error) {
	if err := grammar.Validate(); err != nil {
		return BuildResult{}, fmt.Errorf("invalid grammar metadata: %w", err)
	}
	root := options.Root
	if root == "" {
		return BuildResult{}, errors.New("build root must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return BuildResult{}, fmt.Errorf("resolve build root: %w", err)
	}
	if options.OutputDirectory == "" {
		options.OutputDirectory = filepath.Join(root, "internal", "wasm", "assets")
	} else if !filepath.IsAbs(options.OutputDirectory) {
		options.OutputDirectory = filepath.Join(root, options.OutputDirectory)
	}
	if err := os.MkdirAll(options.OutputDirectory, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create output directory: %w", err)
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = options.Stdout
	}

	workDir, err := os.MkdirTemp("", "wasitter-grammar-")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create grammar work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	sourceRoot, err := DownloadAndExtract(ctx, grammar, filepath.Join(workDir, "source"), options.Download)
	if err != nil {
		return BuildResult{}, err
	}
	parser := filepath.Join(sourceRoot, filepath.FromSlash(grammar.Parser))
	if info, err := os.Stat(parser); err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("path is a directory")
		}
		return BuildResult{}, fmt.Errorf("parser source %s is unavailable: %w", grammar.Parser, err)
	}

	artifact := filepath.Join(options.OutputDirectory, "wasitter-"+grammar.Name+".wasm")
	extraSources := make([]string, 0, len(grammar.ScannerPaths()))
	for _, scanner := range grammar.ScannerPaths() {
		extraSources = append(extraSources, filepath.Join(sourceRoot, filepath.FromSlash(scanner)))
	}
	if err := CompileWASM(ctx, CompilerOptions{
		Root:             root,
		GrammarDirectory: sourceRoot,
		Parser:           parser,
		ExtraSources:     extraSources,
		LanguageFunction: grammar.LanguageFunction,
		LanguageName:     grammar.LanguageName,
		Output:           artifact,
		CCompiler:        options.CCompiler,
		CXXCompiler:      options.CXXCompiler,
		Stdout:           options.Stdout,
		Stderr:           options.Stderr,
	}); err != nil {
		return BuildResult{}, fmt.Errorf("compile grammar %q: %w", grammar.Name, err)
	}
	digest, checksum, err := WriteArtifactChecksum(artifact)
	if err != nil {
		return BuildResult{}, fmt.Errorf("publish grammar artifact: %w", err)
	}
	return BuildResult{
		Language:       grammar.Name,
		ArtifactPath:   artifact,
		ChecksumPath:   checksum,
		ArtifactSHA256: digest,
		ArchiveURL:     archiveURLForOptions(grammar, options.Download),
	}, nil
}

// WriteArtifactChecksum computes and atomically publishes the sidecar digest
// for artifact. It is used by the bundled (non-registry) build as well as by
// callers that invoke CompileWASM directly.
func WriteArtifactChecksum(artifact string) (string, string, error) {
	info, err := os.Stat(artifact)
	if err != nil {
		return "", "", fmt.Errorf("stat artifact %s: %w", artifact, err)
	}
	if info.IsDir() || info.Size() == 0 {
		return "", "", fmt.Errorf("artifact %s is missing or empty", artifact)
	}
	digest, err := FileSHA256(artifact)
	if err != nil {
		return "", "", err
	}
	checksum := artifact + ".sha256"
	if err := writeChecksumAtomic(checksum, filepath.Base(artifact), digest); err != nil {
		return "", "", err
	}
	if err := os.Chmod(artifact, 0o644); err != nil {
		return "", "", fmt.Errorf("set artifact permissions: %w", err)
	}
	return digest, checksum, nil
}

func archiveURLForOptions(grammar Grammar, options DownloadOptions) string {
	if options.URL != "" {
		return options.URL
	}
	return GrammarArchiveURL(grammar)
}

// FileSHA256 computes a lowercase SHA-256 digest for path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for checksum: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeChecksumAtomic(path, fileName, digest string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wasitter-checksum-*")
	if err != nil {
		return fmt.Errorf("create checksum temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := fmt.Fprintf(tmp, "%s  %s\n", digest, fileName); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write checksum: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set checksum permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close checksum: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish checksum: %w", err)
	}
	return nil
}

// VerifyArtifact checks an artifact and its sidecar checksum without network
// access. It also verifies that the sidecar names the same basename.
func VerifyArtifact(artifact, checksum string) (string, error) {
	if artifact == "" {
		return "", errors.New("artifact path must not be empty")
	}
	if checksum == "" {
		checksum = artifact + ".sha256"
	}
	info, err := os.Stat(artifact)
	if err != nil {
		return "", fmt.Errorf("stat artifact %s: %w", artifact, err)
	}
	if info.IsDir() || info.Size() == 0 {
		return "", fmt.Errorf("artifact %s is missing or empty", artifact)
	}
	data, err := os.ReadFile(checksum)
	if err != nil {
		return "", fmt.Errorf("read checksum %s: %w", checksum, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || !sha256Pattern.MatchString(fields[0]) {
		return "", fmt.Errorf("malformed checksum file %s", checksum)
	}
	if fields[1] != filepath.Base(artifact) {
		return "", fmt.Errorf("checksum file %s names %q, want %q", checksum, fields[1], filepath.Base(artifact))
	}
	actual, err := FileSHA256(artifact)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(fields[0], actual) {
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", artifact, fields[0], actual)
	}
	return actual, nil
}
