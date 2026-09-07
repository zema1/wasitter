package wasitter

// The compatibility query matcher intentionally implements only a small
// subset of Tree-sitter's execution semantics.  It must nevertheless reject
// malformed query syntax in the same places as the native compiler.  Without
// a structural pass, fallbackPatternFromExpression used to take the first
// token from an expression and silently ignore the rest, so inputs such as
// `(number stray)` or `(number value:)` appeared to compile successfully.
//
// This parser is deliberately syntax-only: node/field existence and built-in
// predicate arity are checked by the existing validation passes, while this
// file only verifies delimiters and the shape of pattern expressions.  It is
// kept independent from the matcher so adding support for another execution
// subset does not require weakening query validation.

type fallbackStructureParser struct {
	source string
	pos    int
	end    int
}

func validateFallbackStructure(source string) error {
	p := fallbackStructureParser{source: source, end: len(source)}
	for {
		p.skipSpace()
		if p.pos >= p.end {
			return nil
		}
		if err := p.parsePattern(true); err != nil {
			return err
		}
	}
}

func (p *fallbackStructureParser) syntax(at int) error {
	return fallbackSyntaxError(p.source, at)
}

func (p *fallbackStructureParser) skipSpace() {
	for p.pos < p.end {
		switch p.source[p.pos] {
		case ' ', '\t', '\r', '\n', ',':
			// Commas have historically been treated as separators by the
			// compatibility parser. Preserve that permissive behavior here;
			// changing it would be an unrelated compatibility break for old
			// callers that used comma-separated fallback patterns.
			p.pos++
		case ';':
			p.pos = skipQueryComment(p.source, p.pos)
		default:
			return
		}
	}
}

func (p *fallbackStructureParser) scanIdentifier() (int, int) {
	start := p.pos
	if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
		return start, start
	}
	p.pos++
	for p.pos < p.end && isQueryIdentContinueByte(p.source[p.pos]) {
		p.pos++
	}
	return start, p.pos
}

func (p *fallbackStructureParser) parsePattern(topLevel bool) error {
	p.skipSpace()
	start := p.pos
	if p.pos >= p.end {
		return p.syntax(start)
	}

	switch p.source[p.pos] {
	case '(':
		if err := p.parseParen(); err != nil {
			return err
		}
	case '[':
		if err := p.parseAlternatives(); err != nil {
			return err
		}
	case '"':
		if !p.parseQuoted() {
			return p.syntax(start)
		}
	case '_':
		// `_` is Tree-sitter's named-node wildcard and is valid both as a
		// root pattern and as a child pattern.
		p.pos++
	case '*':
		// The fallback matcher historically accepted `(*)` as a wildcard
		// spelling. Keep accepting it while the native compiler's semantic
		// validation (when available) remains authoritative.
		p.pos++
	case '@', ')', ']':
		return p.syntax(start)
	default:
		if !isQueryIdentStartByte(p.source[p.pos]) {
			return p.syntax(start)
		}
		idStart, idEnd := p.scanIdentifier()
		p.skipSpace()
		// A bare identifier at query level is a field-prefixed pattern. A
		// named node itself must be parenthesized; this distinction catches
		// malformed inputs such as `number` while preserving the useful
		// native diagnostic for `name: (number)`.
		if p.pos >= p.end || p.source[p.pos] != ':' {
			_ = idStart
			_ = idEnd
			if topLevel || !topLevel {
				return p.syntax(start)
			}
		}
		p.pos++ // ':'
		p.skipSpace()
		if p.pos >= p.end || p.source[p.pos] == ')' || p.source[p.pos] == ']' {
			return p.syntax(p.pos)
		}
		if err := p.parsePattern(false); err != nil {
			return err
		}
	}

	// Pattern suffixes are parsed outside the atom. This is what permits
	// `(number)+ @n`, `[(number) (string)] @value`, and grouped forms such as
	// `((number) @n)` while still rejecting `(number @n)` inside one node
	// expression (the latter is handled by parseNode's child loop).
	for {
		p.skipSpace()
		if p.pos >= p.end {
			break
		}
		switch p.source[p.pos] {
		case '?', '*', '+':
			p.pos++
		case '@':
			captureStart := p.pos
			p.pos++
			if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
				return p.syntax(captureStart)
			}
			p.scanIdentifier()
		default:
			return nil
		}
	}
	return nil
}

func (p *fallbackStructureParser) parseParen() error {
	open := p.pos
	p.pos++ // '('
	p.skipSpace()
	if p.pos >= p.end {
		return p.syntax(p.pos)
	}
	if p.source[p.pos] == ')' {
		return p.syntax(p.pos)
	}

	// A dot/pound immediately after an opening parenthesis always starts a
	// predicate in Tree-sitter's grammar. A bare dot therefore reports syntax,
	// rather than being mistaken for an immediate-child marker.
	if p.source[p.pos] == '#' || p.source[p.pos] == '.' {
		return p.parsePredicate()
	}

	// An opening parenthesis or bracket denotes a grouped sequence. Every
	// element in that sequence is a complete pattern (with its own suffixes).
	if p.source[p.pos] == '(' || p.source[p.pos] == '[' {
		count := 0
		for {
			p.skipSpace()
			if p.pos >= p.end {
				return p.syntax(p.pos)
			}
			if p.source[p.pos] == ')' {
				if count == 0 {
					return p.syntax(p.pos)
				}
				p.pos++
				return nil
			}
			if p.source[p.pos] == '.' {
				// A sibling anchor is a marker before the following child;
				// it cannot terminate a group by itself.
				p.pos++
				p.skipSpace()
				if p.pos >= p.end || p.source[p.pos] == ')' {
					return p.syntax(p.pos)
				}
			}
			if err := p.parsePattern(false); err != nil {
				return err
			}
			count++
		}
	}

	// Otherwise this is a node pattern. The first node token is parsed
	// separately from its children so a stray bare token cannot be silently
	// ignored.
	if err := p.parseNode(open); err != nil {
		return err
	}
	return nil
}

func (p *fallbackStructureParser) parseNode(_ int) error {
	if p.pos >= p.end {
		return p.syntax(p.pos)
	}
	switch p.source[p.pos] {
	case '"':
		if !p.parseQuoted() {
			return p.syntax(p.pos)
		}
	case '_', '*':
		p.pos++
	default:
		if !isQueryIdentStartByte(p.source[p.pos]) {
			return p.syntax(p.pos)
		}
		start, end := p.scanIdentifier()
		word := p.source[start:end]
		// `MISSING` may be followed by an optional named or anonymous node.
		// The optional argument is part of the root atom, not a child.
		if word == "MISSING" {
			p.skipSpace()
			if p.pos < p.end && p.source[p.pos] == '"' {
				if !p.parseQuoted() {
					return p.syntax(p.pos)
				}
			} else if p.pos < p.end && isQueryIdentStartByte(p.source[p.pos]) {
				p.scanIdentifier()
			}
		}
	}

	p.skipSpace()
	// Supertype/subtype syntax is structurally valid even though the compact
	// matcher does not enforce subtype relationships.
	if p.pos < p.end && p.source[p.pos] == '/' {
		slash := p.pos
		p.pos++
		if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
			return p.syntax(slash)
		}
		p.scanIdentifier()
		p.skipSpace()
	}

	for {
		p.skipSpace()
		if p.pos >= p.end {
			return p.syntax(p.pos)
		}
		switch p.source[p.pos] {
		case ')':
			p.pos++
			return nil
		case '!':
			p.pos++
			p.skipSpace()
			if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
				return p.syntax(p.pos)
			}
			p.scanIdentifier()
		case '.':
			// Immediate-child marker. It must be followed by a child
			// pattern; `.eq?` is not a child and will fail below.
			p.pos++
			p.skipSpace()
			if p.pos >= p.end || p.source[p.pos] == ')' {
				return p.syntax(p.pos)
			}
			if err := p.parsePattern(false); err != nil {
				return err
			}
		case '(', '[', '"', '_', '*':
			if err := p.parsePattern(false); err != nil {
				return err
			}
		default:
			if !isQueryIdentStartByte(p.source[p.pos]) {
				return p.syntax(p.pos)
			}
			fieldStart, fieldEnd := p.scanIdentifier()
			p.skipSpace()
			if p.pos >= p.end || p.source[p.pos] != ':' {
				return p.syntax(fieldStart)
			}
			p.pos++
			p.skipSpace()
			if p.pos >= p.end || p.source[p.pos] == ')' || p.source[p.pos] == ']' {
				return p.syntax(p.pos)
			}
			if err := p.parsePattern(false); err != nil {
				return err
			}
			_ = fieldEnd
		}
	}
}

func (p *fallbackStructureParser) parseAlternatives() error {
	p.pos++ // '['
	count := 0
	for {
		p.skipSpace()
		if p.pos >= p.end {
			return p.syntax(p.pos)
		}
		if p.source[p.pos] == ']' {
			if count == 0 {
				return p.syntax(p.pos)
			}
			p.pos++
			return nil
		}
		if p.source[p.pos] == ')' {
			return p.syntax(p.pos)
		}
		if err := p.parsePattern(false); err != nil {
			return err
		}
		count++
	}
}

func (p *fallbackStructureParser) parsePredicate() error {
	start := p.pos
	p.pos++ // '#' or '.'
	if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
		return p.syntax(start + 1)
	}
	p.scanIdentifier()
	for {
		p.skipSpace()
		if p.pos >= p.end {
			return p.syntax(p.pos)
		}
		if p.source[p.pos] == ')' {
			p.pos++
			return nil
		}
		switch p.source[p.pos] {
		case '@':
			at := p.pos
			p.pos++
			if p.pos >= p.end || !isQueryIdentStartByte(p.source[p.pos]) {
				return p.syntax(at)
			}
			p.scanIdentifier()
		case '"':
			if !p.parseQuoted() {
				return p.syntax(p.pos)
			}
		default:
			if !isQueryIdentStartByte(p.source[p.pos]) {
				return p.syntax(p.pos)
			}
			p.scanIdentifier()
		}
	}
}

func (p *fallbackStructureParser) parseQuoted() bool {
	if p.pos >= p.end || p.source[p.pos] != '"' {
		return false
	}
	next := skipQueryQuoted(p.source, p.pos)
	if next <= p.pos || next > p.end || next == p.end && (p.end == 0 || p.source[p.end-1] != '"') {
		return false
	}
	if _, ok := decodeQueryString(p.source[p.pos:next]); !ok {
		return false
	}
	p.pos = next
	return true
}
