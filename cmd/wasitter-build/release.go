package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zema1/wasitter/internal/grammarbuild"
)

const releaseGrammarDirectory = ".tmp/release/grammars"

// Build into a fresh staging directory. A failed build never leaves a mixed
// set of old and new grammars that the upload step could publish.
func runBuildGrammars(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build-grammars", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootArg := fs.String("root", "", "repository root")
	outputArg := fs.String("output-dir", releaseGrammarDirectory, "output directory inside repository")
	if err := fs.Parse(args); err != nil {
		return flagParseError(err)
	}
	if fs.NArg() != 0 {
		return &usageError{message: "build-grammars does not accept positional arguments"}
	}
	root, err := resolveRoot(*rootArg)
	if err != nil {
		return err
	}
	output := *outputArg
	if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}
	if _, err := containerPath(root, output, ""); err != nil {
		return err
	}
	registryPath := grammarbuild.ResolveRegistryPath(root, "")
	registry, err := grammarbuild.LoadRegistry(registryPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("release staging directory already exists: %s; remove it or choose a new -output-dir", output)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(output), ".grammar-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	options := grammarFlags{root: root, registry: registryPath, image: defaultImage, output: staging, includeLicenses: true}
	for _, grammar := range registry.Grammars {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := runDockerGrammar(ctx, options, grammar); err != nil {
			return fmt.Errorf("build %s: %w", grammar.Name, err)
		}
		options.noImageBuild = true
		if err := verifyGrammar(options, grammar); err != nil {
			return err
		}
	}
	registryBytes, err := os.ReadFile(registryPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "grammar-registry.json"), registryBytes, 0644); err != nil {
		return err
	}
	if err := os.Rename(staging, output); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "built %d grammars in %s\n", len(registry.Grammars), output)
	return nil
}

func runPrepareGrammarAssets(args []string) error {
	fs := flag.NewFlagSet("prepare-grammar-assets", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootArg := fs.String("root", "", "repository root")
	input := fs.String("artifact-dir", releaseGrammarDirectory, "built grammar directory")
	output := fs.String("output-dir", "dist/grammars", "release asset output directory")
	if err := fs.Parse(args); err != nil {
		return flagParseError(err)
	}
	if fs.NArg() != 0 {
		return &usageError{message: "prepare-grammar-assets does not accept positional arguments"}
	}
	root, err := resolveRoot(*rootArg)
	if err != nil {
		return err
	}
	directory, err := grammarbuild.PrepareGrammarAssets(root, *input, *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "prepared release assets in %s\n", directory)
	return nil
}
