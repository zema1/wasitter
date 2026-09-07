package grammarbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// CompilerOptions describes one wasm32-wasi compilation. Every compiler
// argument is constructed as an element of an argv vector, keeping paths with
// spaces and registry-provided metadata unambiguous on every host.
type CompilerOptions struct {
	Root             string
	RuntimeDirectory string
	GrammarDirectory string
	Parser           string
	ExtraSources     []string
	LanguageFunction string
	LanguageName     string
	Output           string
	CCompiler        string
	CXXCompiler      string
	Stdout           io.Writer
	Stderr           io.Writer
}

const exportedSymbols = `
tsw_abi_version tsw_alloc tsw_free tsw_realloc
tsw_language tsw_language_name tsw_language_name_ptr tsw_language_name_len
tsw_language_abi_version tsw_language_abi_version_for tsw_language_metadata
tsw_language_symbol_count tsw_language_state_count tsw_language_field_count
tsw_language_field_id_for_name tsw_language_symbol_type tsw_language_next_state tsw_language_version
tsw_language_symbol_name tsw_language_field_name tsw_language_symbol_for_name
tsw_language_symbol_for_name_named
tsw_lookahead_iterator_new tsw_lookahead_iterator_delete
tsw_lookahead_iterator_reset_state tsw_lookahead_iterator_reset
tsw_lookahead_iterator_language tsw_lookahead_iterator_next
tsw_lookahead_iterator_current_symbol tsw_lookahead_iterator_current_symbol_name
tsw_parser_new tsw_parser_delete tsw_parser_set_language tsw_parser_language
tsw_parser_set_included_ranges tsw_parser_included_ranges_into
tsw_parser_parse tsw_parser_reset tsw_parser_set_timeout_micros tsw_parser_set_timeout
tsw_parser_timeout_micros tsw_parser_set_cancellation_flag tsw_parser_cancellation_flag
tsw_tree_delete tsw_tree_copy tsw_tree_root tsw_tree_root_with_offset tsw_tree_edit
tsw_tree_language tsw_tree_included_ranges_into
tsw_tree_changed_ranges_into tsw_tree_get_changed_ranges tsw_ranges_free
tsw_node_delete tsw_node_type_ptr tsw_node_type_len tsw_node_type tsw_node_string
tsw_node_to_sexp tsw_node_id tsw_node_kind_id tsw_node_grammar_type tsw_node_symbol
tsw_node_grammar_symbol tsw_node_language tsw_node_start_byte tsw_node_end_byte
tsw_node_start_row tsw_node_start_column tsw_node_end_row tsw_node_end_column
tsw_node_start_point tsw_node_end_point tsw_node_is_null tsw_node_is_named
tsw_node_is_missing tsw_node_is_extra tsw_node_has_changes tsw_node_has_error
tsw_node_is_error tsw_node_parse_state tsw_node_next_parse_state tsw_node_parent
tsw_node_eq tsw_node_child_with_descendant tsw_node_child tsw_node_named_child tsw_node_child_count
tsw_node_named_child_count tsw_node_next_sibling tsw_node_prev_sibling
tsw_node_next_named_sibling tsw_node_prev_named_sibling tsw_node_child_by_field_name
tsw_node_child_by_field_id
tsw_node_field_name_for_child_ptr tsw_node_field_name_for_child
tsw_node_field_name_for_named_child tsw_node_first_child_for_byte
tsw_node_first_named_child_for_byte tsw_node_descendant_for_byte_range
tsw_node_named_descendant_for_byte_range tsw_node_descendant_for_point_range
tsw_node_named_descendant_for_point_range tsw_node_descendant_count
tsw_node_edit
tsw_cursor_new tsw_cursor_delete tsw_cursor_reset tsw_cursor_reset_to
tsw_cursor_current_node tsw_cursor_current_field_name_ptr
tsw_cursor_current_field_name_len tsw_cursor_current_field_name tsw_cursor_current_field_id
tsw_cursor_goto_parent tsw_cursor_goto_next_sibling tsw_cursor_goto_previous_sibling
tsw_cursor_goto_first_child tsw_cursor_goto_last_child tsw_cursor_goto_descendant
tsw_cursor_current_descendant_index tsw_cursor_current_depth
tsw_cursor_goto_first_child_for_byte tsw_cursor_goto_first_child_for_point tsw_cursor_copy
tsw_tree_cursor_new tsw_tree_cursor_delete tsw_tree_cursor_reset tsw_tree_cursor_reset_to
tsw_tree_cursor_current_node tsw_tree_cursor_current_field_name_ptr
tsw_tree_cursor_current_field_name_len tsw_tree_cursor_current_field_name tsw_tree_cursor_current_field_id
tsw_tree_cursor_goto_parent tsw_tree_cursor_goto_next_sibling tsw_tree_cursor_goto_previous_sibling
tsw_tree_cursor_goto_prev_sibling tsw_tree_cursor_goto_first_child tsw_tree_cursor_goto_last_child
tsw_tree_cursor_goto_descendant tsw_tree_cursor_current_descendant_index tsw_tree_cursor_current_depth
tsw_tree_cursor_goto_first_child_for_byte tsw_tree_cursor_goto_first_child_for_point tsw_tree_cursor_copy
tsw_query_new tsw_query_delete tsw_query_pattern_count tsw_query_capture_count
tsw_query_string_count tsw_query_start_byte_for_pattern tsw_query_end_byte_for_pattern
tsw_query_predicates_for_pattern_into tsw_query_is_pattern_rooted
tsw_query_is_pattern_non_local tsw_query_is_pattern_guaranteed_at_step
tsw_query_capture_name_ptr tsw_query_capture_name_len tsw_query_capture_name
tsw_query_capture_quantifier_for_id tsw_query_string_value_ptr tsw_query_string_value_len
tsw_query_string_value tsw_query_disable_capture tsw_query_disable_pattern
tsw_query_cursor_new tsw_query_cursor_delete tsw_query_cursor_exec
tsw_query_cursor_next_match tsw_query_cursor_next_capture tsw_query_cursor_next_capture_match
tsw_query_cursor_remove_match tsw_query_cursor_did_exceed_match_limit
tsw_query_cursor_match_limit tsw_query_cursor_set_match_limit
tsw_query_cursor_set_timeout_micros tsw_query_cursor_timeout_micros
tsw_query_cursor_set_byte_range tsw_query_cursor_set_point_range
tsw_query_cursor_set_max_start_depth
tsw_copy_string tsw_last_error_ptr tsw_last_error_len tsw_clear_error
malloc calloc realloc free
`

// CompileWASM compiles the bundled runtime, a generated parser, optional
// external scanners, and the ABI bridge into a wasm32-wasi module. The output
// is written through a sibling temporary file and renamed only after linking
// succeeds, preserving an existing artifact on compiler failure.
func CompileWASM(ctx context.Context, options CompilerOptions) error {
	root, err := absoluteRequired(options.Root, "root")
	if err != nil {
		return err
	}
	runtimeDir := options.RuntimeDirectory
	if runtimeDir == "" {
		runtimeDir = filepath.Join(root, "internal", "wasm", "third_party", "tree-sitter")
	}
	runtimeDir, err = resolveAbsolute(root, runtimeDir)
	if err != nil {
		return fmt.Errorf("resolve runtime directory: %w", err)
	}
	grammarDir := options.GrammarDirectory
	if grammarDir == "" {
		grammarDir = filepath.Join(root, "internal", "wasm", "third_party", "tree-sitter-json")
	}
	grammarDir, err = resolveAbsolute(root, grammarDir)
	if err != nil {
		return fmt.Errorf("resolve grammar directory: %w", err)
	}
	parserInput := options.Parser
	if parserInput == "" {
		parserInput = filepath.Join(grammarDir, "parser.c")
	}
	parser, err := resolveSource(root, grammarDir, parserInput)
	if err != nil {
		return fmt.Errorf("resolve parser source: %w", err)
	}
	bridge := filepath.Join(root, "internal", "wasm", "src", "wasitter_abi.c")
	include := filepath.Join(root, "internal", "wasm", "include")
	for _, required := range []struct {
		name string
		path string
	}{
		{"runtime library", filepath.Join(runtimeDir, "lib.c")},
		{"runtime API header", filepath.Join(runtimeDir, "include", "tree_sitter", "api.h")},
		{"parser source", parser},
		{"ABI bridge", bridge},
	} {
		if info, statErr := os.Stat(required.path); statErr != nil || info.IsDir() {
			if statErr == nil {
				statErr = errors.New("path is a directory")
			}
			return fmt.Errorf("missing %s %s: %w", required.name, required.path, statErr)
		}
	}
	if options.LanguageFunction == "" {
		options.LanguageFunction = "tree_sitter_json"
	}
	if !cIdentifier(options.LanguageFunction) {
		return fmt.Errorf("language function %q is not a C identifier", options.LanguageFunction)
	}
	if options.LanguageName == "" {
		options.LanguageName = "json"
	}
	if err := validateCString(options.LanguageName); err != nil {
		return fmt.Errorf("language name: %w", err)
	}
	output := options.Output
	if output == "" {
		output = filepath.Join(root, "internal", "wasm", "assets", "wasitter-json.wasm")
	} else if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve output: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	stdout, stderr := options.Stdout, options.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = stdout
	}

	compiler, zig, err := selectCompiler(ctx, options.CCompiler)
	if err != nil {
		return err
	}
	cxx := options.CXXCompiler
	cxxZig := zig
	needsCXX := isCXXSource(parser)
	for _, extra := range options.ExtraSources {
		if isCXXSource(extra) {
			needsCXX = true
		}
	}
	if needsCXX {
		cxx, cxxZig, err = selectCXXCompiler(ctx, cxx, compiler, zig)
		if err != nil {
			return err
		}
	}

	buildDir, err := os.MkdirTemp("", "wasitter-build-")
	if err != nil {
		return fmt.Errorf("create compiler work directory: %w", err)
	}
	defer os.RemoveAll(buildDir)
	nameDefine := `"` + escapeCString(options.LanguageName) + `"`
	baseArgs := compilerArgs(zig, compiler, false, runtimeDir, grammarDir, parser, include, options.LanguageFunction, nameDefine)
	compile := func(source, object string, cxxMode bool) error {
		driver := compiler
		args := append([]string(nil), baseArgs...)
		if cxxMode {
			driver = cxx
			args = compilerArgs(cxxZig, cxx, true, runtimeDir, grammarDir, parser, include, options.LanguageFunction, nameDefine)
		}
		args = append(args, "-c", source, "-o", object)
		return runCompiler(ctx, driver, args, stdout, stderr)
	}
	objects := make([]string, 0, len(options.ExtraSources)+3)
	runtimeObject := filepath.Join(buildDir, "tree-sitter.o")
	if err := compile(filepath.Join(runtimeDir, "lib.c"), runtimeObject, false); err != nil {
		return fmt.Errorf("compile runtime: %w", err)
	}
	grammarObject := filepath.Join(buildDir, "grammar.o")
	if err := compile(parser, grammarObject, isCXXSource(parser)); err != nil {
		return fmt.Errorf("compile parser: %w", err)
	}
	bridgeObject := filepath.Join(buildDir, "bridge.o")
	if err := compile(bridge, bridgeObject, false); err != nil {
		return fmt.Errorf("compile ABI bridge: %w", err)
	}
	objects = append(objects, runtimeObject, grammarObject, bridgeObject)
	for i, extraInput := range options.ExtraSources {
		extra, err := resolveSource(root, grammarDir, extraInput)
		if err != nil {
			return fmt.Errorf("resolve scanner source %q: %w", extraInput, err)
		}
		if info, statErr := os.Stat(extra); statErr != nil || info.IsDir() {
			if statErr == nil {
				statErr = errors.New("path is a directory")
			}
			return fmt.Errorf("missing scanner source %s: %w", extra, statErr)
		}
		object := filepath.Join(buildDir, fmt.Sprintf("grammar-extra-%d.o", i))
		if err := compile(extra, object, isCXXSource(extra)); err != nil {
			return fmt.Errorf("compile scanner %s: %w", extraInput, err)
		}
		objects = append(objects, object)
	}

	tmp, err := os.CreateTemp(filepath.Dir(output), ".wasitter-wasm-*")
	if err != nil {
		return fmt.Errorf("create temporary wasm output: %w", err)
	}
	tmpOutput := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpOutput)
		return fmt.Errorf("close temporary wasm output: %w", err)
	}
	defer os.Remove(tmpOutput)
	linkZig, linkCompiler := zig, compiler
	if needsCXX {
		linkZig, linkCompiler = cxxZig, cxx
	}
	oldMemorySyntax := linkZig && oldZigMemorySyntax(ctx, linkCompiler)
	linkDriver, linkArgs := linkerArgs(zig, compiler, needsCXX, cxx, cxxZig, objects, tmpOutput, oldMemorySyntax)
	if err := runCompiler(ctx, linkDriver, linkArgs, stdout, stderr); err != nil {
		return fmt.Errorf("link wasm: %w", err)
	}
	if info, statErr := os.Stat(tmpOutput); statErr != nil || info.IsDir() || info.Size() == 0 {
		if statErr == nil {
			statErr = errors.New("linker produced an empty artifact")
		}
		return fmt.Errorf("linker produced invalid output: %w", statErr)
	}
	return publishOutput(tmpOutput, output)
}

func absoluteRequired(value, name string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return filepath.Abs(value)
}

func resolveAbsolute(root, value string) (string, error) {
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Abs(filepath.Join(root, value))
}

func resolveSource(root, grammarDir, value string) (string, error) {
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative source path %q escapes its source directory", value)
	}
	rootPath := filepath.Join(root, clean)
	if info, err := os.Stat(rootPath); err == nil && !info.IsDir() {
		return filepath.Clean(rootPath), nil
	}
	return filepath.Clean(filepath.Join(grammarDir, clean)), nil
}

func cIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func validateCString(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func escapeCString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func isCXXSource(value string) bool {
	switch filepath.Ext(value) {
	case ".cc", ".cpp", ".cxx", ".C":
		return true
	default:
		return false
	}
}

func selectCompiler(ctx context.Context, configured string) (string, bool, error) {
	if configured != "" {
		path, err := resolveExecutable(configured)
		if err != nil {
			return "", false, fmt.Errorf("configured C compiler %q: %w", configured, err)
		}
		return path, isZigCompiler(ctx, path), nil
	}
	if zig, err := exec.LookPath("zig"); err == nil {
		return zig, true, nil
	}
	if clang, err := exec.LookPath("clang"); err == nil {
		return clang, false, nil
	}
	return "", false, errors.New("need zig or a wasi-sdk clang (set CCompiler)")
}

func resolveExecutable(value string) (string, error) {
	if filepath.IsAbs(value) || strings.ContainsAny(value, `/\\`) {
		info, err := os.Stat(value)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", errors.New("path is a directory")
		}
		return value, nil
	}
	return exec.LookPath(value)
}

func isZigCompiler(ctx context.Context, compiler string) bool {
	cmd := exec.CommandContext(ctx, compiler, "version")
	return cmd.Run() == nil
}

func oldZigMemorySyntax(ctx context.Context, compiler string) bool {
	cmd := exec.CommandContext(ctx, compiler, "version")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	version := strings.TrimSpace(string(output))
	return strings.HasPrefix(version, "0.11.") || strings.HasPrefix(version, "0.12.") || strings.HasPrefix(version, "0.13.")
}

func selectCXXCompiler(ctx context.Context, configured, compiler string, zig bool) (string, bool, error) {
	if configured != "" {
		path, err := resolveExecutable(configured)
		if err != nil {
			return "", false, err
		}
		return path, isZigCompiler(ctx, path), nil
	}
	if zig {
		return compiler, true, nil
	}
	if dir := filepath.Dir(compiler); dir != "." {
		candidate := filepath.Join(dir, "clang++")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, isZigCompiler(ctx, candidate), nil
		}
	}
	if cxx, err := exec.LookPath("clang++"); err == nil {
		return cxx, isZigCompiler(ctx, cxx), nil
	}
	return "", false, errors.New("C++ grammar source requires clang++ (set CXXCompiler)")
}

func compilerArgs(zig bool, compiler string, cxx bool, runtimeDir, grammarDir, parser, include, function, nameDefine string) []string {
	args := make([]string, 0, 24)
	if zig {
		if cxx {
			args = append(args, "c++")
		} else {
			args = append(args, "cc")
		}
		args = append(args, "-target", "wasm32-wasi")
	} else {
		args = append(args, "--target=wasm32-wasi")
	}
	if cxx {
		args = append(args, "-O2", "-std=c++17", "-fno-exceptions", "-fno-rtti")
	} else {
		args = append(args, "-O2", "-std=c11")
	}
	args = append(args,
		"-fvisibility=hidden", "-D__EMSCRIPTEN__", "-D_POSIX_C_SOURCE=200112L", "-D_DEFAULT_SOURCE",
		"-DWASITTER_LANGUAGE_FN="+function, "-DWASITTER_LANGUAGE_NAME="+nameDefine,
		"-I"+include, "-I"+filepath.Join(runtimeDir, "include"), "-I"+runtimeDir,
		"-I"+grammarDir, "-I"+filepath.Dir(parser),
	)
	return args
}

func linkerArgs(zig bool, compiler string, needsCXX bool, cxx string, cxxZig bool, objects []string, output string, oldMemorySyntax bool) (string, []string) {
	driver := compiler
	args := make([]string, 0, len(objects)+len(strings.Fields(exportedSymbols))+16)
	if needsCXX {
		driver = cxx
		if cxxZig {
			args = append(args, "c++", "-target", "wasm32-wasi")
		} else {
			args = append(args, "--target=wasm32-wasi")
		}
	} else if zig {
		args = append(args, "cc", "-target", "wasm32-wasi")
	} else {
		args = append(args, "--target=wasm32-wasi")
	}
	initialMemory, maxMemory := "33554432", "268435456"
	if oldMemorySyntax {
		initialMemory, maxMemory = "0x02000000", "0x10000000"
	}
	args = append(args, "-Wl,--no-entry", "-Wl,--export-memory", "-Wl,--initial-memory="+initialMemory, "-Wl,--max-memory="+maxMemory, "-Wl,--strip-all")
	for _, symbol := range strings.Fields(exportedSymbols) {
		args = append(args, "-Wl,--export="+symbol)
	}
	args = append(args, objects...)
	args = append(args, "-o", output)
	return driver, args
}

func publishOutput(tmpOutput, output string) error {
	if err := os.Chmod(tmpOutput, 0o644); err != nil {
		return fmt.Errorf("set wasm permissions: %w", err)
	}
	if err := os.Rename(tmpOutput, output); err == nil {
		return nil
	} else if _, statErr := os.Stat(output); statErr == nil {
		// POSIX rename replaces atomically. Windows does not permit replacing an
		// existing file, so make the best cross-platform effort there; builds in
		// the pinned Linux container retain the atomic path above.
		if removeErr := os.Remove(output); removeErr != nil {
			return fmt.Errorf("publish wasm output: %w (remove existing: %v)", err, removeErr)
		}
		if renameErr := os.Rename(tmpOutput, output); renameErr != nil {
			return fmt.Errorf("publish wasm output: %w", renameErr)
		}
		return nil
	} else {
		return fmt.Errorf("publish wasm output: %w", err)
	}
}

func runCompiler(ctx context.Context, compiler string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, compiler, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}
