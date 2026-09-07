package wasitter

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
)

// ParseInput parses callback input using the runtime's context. The callback
// is collected into memory and decoded to UTF-8 before parsing. Returned tree
// offsets refer to that UTF-8 representation. Pass nil for oldTree for a new parse.
func (p *Parser) ParseInput(input Input, oldTree *Tree) (*Tree, error) {
	return p.ParseInputContext(p.runtimeContext(), input, oldTree)
}

// ParseInputContext parses callback input with a caller-supplied context.
// See [Parser.ParseInput] for encoding and buffering behavior.
func (p *Parser) ParseInputContext(ctx context.Context, input Input, oldTree *Tree) (*Tree, error) {
	return p.ParseInputWithOptionsContext(ctx, input, oldTree, nil)
}

// ParseInputWithOptionsContext parses callback input with a context and
// progress options. The callback is collected before entering the guest;
// progress callbacks run before and after parsing. See [Parser.ParseInput].
func (p *Parser) ParseInputWithOptionsContext(ctx context.Context, input Input, oldTree *Tree, options *ParseOptions) (*Tree, error) {
	if input.Read == nil {
		return nil, ErrInvalidHandle
	}
	if err := p.ensureOpen(); err != nil {
		return nil, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	data, err := collectInputEncoding(input)
	if err != nil {
		return nil, err
	}
	return p.parseWithOptionsContext(ctx, data, oldTree, options)
}

// collectInputEncoding materializes an Input descriptor and normalizes its
// contents to UTF-8.  The generic UTF16 value is intentionally rejected
// because it does not carry byte-order information; explicit LE/BE values are
// accepted.
func collectInputEncoding(input Input) ([]byte, error) {
	switch input.Encoding {
	case InputEncodingUTF8:
		return collectUTF8Input(input.Read)
	case InputEncodingUTF16LE:
		raw, err := collectUTF16BytesInput(input.Read, binary.LittleEndian)
		if err != nil {
			return nil, err
		}
		return utf16BytesToUTF8(raw, binary.LittleEndian)
	case InputEncodingUTF16BE:
		raw, err := collectUTF16BytesInput(input.Read, binary.BigEndian)
		if err != nil {
			return nil, err
		}
		return utf16BytesToUTF8(raw, binary.BigEndian)
	default:
		return nil, fmt.Errorf("%w: ParseInput requires UTF-8 or explicit UTF-16LE/UTF-16BE input", ErrUnsupported)
	}
}

// collectUTF16BytesInput is the callback collector for byte-oriented UTF-16
// descriptors.  Tree-sitter reports callback offsets in bytes but reports the
// point column in UTF-16 code units; keep that distinction while accepting
// chunks that split a code unit across callback boundaries.
func collectUTF16BytesInput(read ReadFunc, order binary.ByteOrder) ([]byte, error) {
	if read == nil {
		return nil, ErrInvalidHandle
	}
	var all []byte
	point := Point{}
	var pending byte
	havePending := false
	const maxCalls = 1 << 20
	for calls := 0; calls < maxCalls; calls++ {
		chunk := read(uint32(len(all)), point)
		if len(chunk) == 0 {
			if havePending {
				return nil, fmt.Errorf("wasitter: UTF-16 input has odd byte length %d", len(all))
			}
			return all, nil
		}
		if uint64(len(all))+uint64(len(chunk)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("wasitter: input exceeds uint32 byte offset")
		}
		all = append(all, chunk...)
		for _, b := range chunk {
			if !havePending {
				pending = b
				havePending = true
				continue
			}
			var pair [2]byte
			if order == binary.LittleEndian {
				pair[0], pair[1] = pending, b
			} else {
				pair[0], pair[1] = pending, b
			}
			unit := order.Uint16(pair[:])
			if unit == '\n' {
				point.Row++
				point.Column = 0
			} else {
				point.Column++
			}
			havePending = false
		}
	}
	return nil, fmt.Errorf("wasitter: input callback exceeded %d calls: %w", maxCalls, io.ErrNoProgress)
}
