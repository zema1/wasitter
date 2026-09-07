// Package wasitter runs the upstream Tree-sitter parser and query engine in Go
// through WebAssembly. It uses wazero and requires no CGO or C compiler.
//
// # Creating a parser
//
// [NewJavaScriptParser] and [NewJSONParser] use embedded language modules.
// For other languages, download a wasitter WASM file matching the Go package
// version from https://github.com/zema1/wasitter/releases, then use
// [NewParserFromFile]. Use [NewParserFromWASM] for bytes loaded by your
// application or embedded with go:embed. These constructors return a parser
// ready to parse and its runtime.
//
// Call [Parser.ParseContext] with UTF-8 source to obtain a [Tree]. Use
// [Tree.RootNode] to navigate nodes, or [NewQuery] and [QueryCursor.Matches]
// to find nodes with Tree-sitter query patterns. A successful parse may still
// contain syntax errors; inspect [Node.HasError] separately.
//
// # Lifetimes and reuse
//
// Close queries, cursors, trees, and parsers before their runtime. Close is
// idempotent. A [Node] is a value referring to its tree: keep the tree open
// while using its nodes. Closing a parser does not close its runtime or trees.
//
// Reuse a parser and runtime for sequential files and close each tree when
// finished. A runtime serializes guest execution. For parallel parsing, use
// one runtime and parser per worker. [RuntimeOptions] can configure a shared
// wazero compilation cache to reduce compilation work across runtimes.
//
// # Coordinates and incremental parsing
//
// Byte offsets and [Point] columns refer to UTF-8 bytes; rows are zero-based.
// UTF-16 input is decoded before parsing, so resulting tree coordinates also
// refer to the decoded UTF-8 text. For incremental parsing, call [Tree.Edit]
// with the changed coordinates, then pass the edited tree and updated source
// to [Parser.ParseContext].
//
// # Runtime configuration and errors
//
// For custom wazero options, use [NewRuntimeWithOptions], resolve a grammar
// with [Runtime.LoadLanguage], create a parser with [NewParserWithRuntime],
// then assign the language with [Parser.SetLanguage]. Language names select
// exports in the loaded module; they do not download or register grammars.
// WASM files for web-tree-sitter cannot be loaded directly: wasitter modules
// also contain the Tree-sitter runtime and wasitter ABI bridge.
//
// Use errors.Is with sentinel errors such as [ErrClosed] and [ErrUnsupported],
// and errors.As with [QueryError] or [ABIError] for detailed failures. Some
// accessors offer both a convenience form returning a zero value on failure
// and an E-suffixed form returning an error.
package wasitter
