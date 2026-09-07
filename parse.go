package wasitter

import "context"

// Parse is a package-level convenience matching the historical Go
// Tree-sitter binding. It parses content with language and returns the root
// node. Errors are intentionally discarded for this compatibility helper;
// callers that need diagnostics should use ParseCtx.
func Parse(content []byte, language *Language) *Node {
	node, _ := ParseCtx(context.Background(), content, language)
	return node
}

// ParseCtx is the error-aware package-level parsing convenience. The returned
// node keeps its backing Tree alive through its internal tree reference, so it
// remains usable until the node is garbage-collected (or the owning runtime is
// closed). Applications that need explicit lifetime control should construct a
// Parser and retain the returned Tree directly.
func ParseCtx(ctx context.Context, content []byte, language *Language) (*Node, error) {
	if language == nil {
		return nil, ErrNoLanguage
	}
	if err := language.ensureOpen(); err != nil {
		return nil, err
	}
	parser, err := NewParserWithRuntime(language.runtime)
	if err != nil {
		return nil, err
	}
	if err := parser.SetLanguage(language); err != nil {
		_ = parser.Close()
		return nil, err
	}
	tree, err := parser.ParseContext(ctx, content, nil)
	if err != nil {
		_ = parser.Close()
		return nil, err
	}
	node, err := tree.RootNodeE()
	if err != nil {
		_ = tree.Close()
		_ = parser.Close()
		return nil, err
	}
	// Node contains a pointer to tree, and Tree in turn retains parser. Leave
	// both attached to the returned value so their finalizers can reclaim the
	// guest allocations once the caller drops the node.
	return &node, nil
}
