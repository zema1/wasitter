// Package wasitter provides a pure-Go binding to Tree-sitter modules
// compiled to WebAssembly. It has no cgo dependency; wazero executes the
// runtime and grammar in a sandboxed module.
//
// A module normally contains the Tree-sitter C runtime, a generated grammar,
// and the small wasitter shim. See NewRuntime and Runtime.LoadLanguage for
// loading a module and obtaining its language.
package wasitter
