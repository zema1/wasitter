# Test corpus

`json_cases.json` contains small JSON inputs and the expected Tree-sitter
S-expressions produced by the vendored C runtime and JSON grammar. The corpus
is consumed by the Go tests to make the WASM result an explicit parity
contract. If the runtime or grammar version changes, regenerate the
S-expressions with the native `ts_parser_parse_string` + `ts_node_string`
driver before updating these expectations.
