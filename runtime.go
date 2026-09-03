package sitterwasm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// RuntimeOptions controls creation of a Runtime.
//
// The zero value is suitable for ordinary Tree-sitter shim modules. WASI is
// enabled by default because modules produced by wasi-sdk commonly import it.
type RuntimeOptions struct {
	// RuntimeConfig is used to create the wazero runtime. A nil value selects
	// wazero's default (compiler) configuration.
	RuntimeConfig wazero.RuntimeConfig
	// ModuleConfig controls instantiation. A nil value disables module start
	// functions, which is the safest setting for a library module.
	ModuleConfig wazero.ModuleConfig
	// EnableWASI installs the wasi_snapshot_preview1 host module. It defaults to
	// true when RuntimeOptions is the zero value.
	EnableWASI *bool
	// Configure is called after the wazero runtime is created and before the
	// guest module is instantiated. It can be used to install custom imports.
	Configure func(wazero.Runtime) error
}

// Runtime owns a wazero instance and one instantiated Tree-sitter module.
// A Runtime is safe for concurrent use; individual Parser values additionally
// serialize calls because Tree-sitter parsers are stateful.
type Runtime struct {
	ctx context.Context
	rt  wazero.Runtime
	mod api.Module

	mu     sync.Mutex
	closed atomic.Bool
	funcs  map[string]api.Function
	// allocSizes records the requested size for guest allocations made by the
	// host.  It is primarily needed by wasm-bindgen-style deallocators, whose
	// ABI takes the allocation size and alignment in addition to the pointer.
	// Access is serialized by mu (all callers of allocLocked/freeLocked hold it).
	allocSizes map[uint32]uint32

	// nextAlloc is only used by modules that intentionally omit malloc. It is
	// a conservative fallback for tiny test modules; production shims should
	// export tsw_alloc (or malloc).
	nextAlloc atomic.Uint32
	owned     bool
}

// NewRuntime compiles and instantiates a WebAssembly Tree-sitter module.
func NewRuntime(ctx context.Context, wasm []byte) (*Runtime, error) {
	return NewRuntimeWithOptions(ctx, wasm, RuntimeOptions{})
}

// NewRuntimeFromFile is a convenience wrapper around NewRuntime.
func NewRuntimeFromFile(ctx context.Context, path string) (*Runtime, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return NewRuntime(ctx, b)
}

// NewRuntimeWithOptions creates a Runtime with explicit wazero options.
func NewRuntimeWithOptions(ctx context.Context, wasm []byte, opts RuntimeOptions) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var wr wazero.Runtime
	if opts.RuntimeConfig != nil {
		wr = wazero.NewRuntimeWithConfig(ctx, opts.RuntimeConfig)
	} else {
		wr = wazero.NewRuntime(ctx)
	}
	r := &Runtime{ctx: ctx, rt: wr, funcs: make(map[string]api.Function), allocSizes: make(map[uint32]uint32), owned: true}
	closeWithError := func(err error) (*Runtime, error) {
		// Cleanup must not inherit a caller cancellation.  In particular, a
		// compile/instantiate failure commonly occurs because ctx was canceled;
		// passing that same context to Close can leave WASI resources alive.
		_ = wr.Close(context.Background())
		return nil, err
	}

	enableWASI := true
	if opts.EnableWASI != nil {
		enableWASI = *opts.EnableWASI
	}
	if enableWASI {
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, wr); err != nil {
			return closeWithError(fmt.Errorf("instantiate WASI: %w", err))
		}
	}
	if opts.Configure != nil {
		if err := opts.Configure(wr); err != nil {
			return closeWithError(err)
		}
	}

	compiled, err := wr.CompileModule(ctx, wasm)
	if err != nil {
		return closeWithError(fmt.Errorf("compile WASM: %w", err))
	}
	defer func() { _ = compiled.Close(context.Background()) }()

	config := opts.ModuleConfig
	if config == nil {
		// A library module must not implicitly invoke _start. C/WASI modules
		// generally use _initialize, which is also intentionally left to the
		// module's own bridge when needed.
		config = wazero.NewModuleConfig().WithStartFunctions()
	}
	mod, err := wr.InstantiateModule(ctx, compiled, config)
	if err != nil {
		return closeWithError(fmt.Errorf("instantiate WASM: %w", err))
	}
	r.mod = mod
	goruntime.SetFinalizer(r, func(rt *Runtime) { _ = rt.Close() })
	// A valid heap starts after the current memory. This fallback is only
	// selected when no allocator export exists.
	if mem := mod.Memory(); mem != nil {
		r.nextAlloc.Store(mem.Size())
	}
	return r, nil
}

// NewRuntimeFromModule wraps an already-instantiated wazero module. Closing
// the returned Runtime closes the module, but does not close the caller's
// wazero runtime.
func NewRuntimeFromModule(ctx context.Context, module api.Module) (*Runtime, error) {
	if module == nil {
		return nil, fmt.Errorf("sitterwasm: nil module")
	}
	// A closed module cannot be made usable again.  More importantly, keeping
	// a wrapper around one would make the first ExportedFunction/Memory call
	// fail deep inside wazero (and older wazero versions could panic there).
	// Check this before touching the module's exports so callers get the same
	// deterministic lifecycle error as they do from a Runtime that we own.
	if module.IsClosed() {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r := &Runtime{ctx: ctx, mod: module, funcs: make(map[string]api.Function), allocSizes: make(map[uint32]uint32)}
	if mem := module.Memory(); mem != nil {
		r.nextAlloc.Store(mem.Size())
	}
	goruntime.SetFinalizer(r, func(rt *Runtime) { _ = rt.Close() })
	return r, nil
}

// Module returns the underlying wazero module. It is primarily useful for
// advanced integrations and custom ABI extensions.
func (r *Runtime) Module() api.Module {
	if r == nil {
		return nil
	}
	return r.mod
}

// Context returns the context used for calls into the guest module.
func (r *Runtime) Context() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// ABIVersion reports the wire ABI version advertised by the loaded module.
// A zero value is returned when the optional export is unavailable or the
// runtime has been closed; callers that need to distinguish those cases can
// use ABIVersionE.
func (r *Runtime) ABIVersion() uint32 {
	v, _ := r.ABIVersionE()
	return v
}

// AbiVersion is the mixed-case spelling commonly used by Tree-sitter's Go
// binding. It is an alias of ABIVersion.
func (r *Runtime) AbiVersion() uint32 { return r.ABIVersion() }

// ABIVersionE returns the module's advertised sitterwasm wire ABI version.
// Modules built before the version export are still usable through the
// compatibility paths, so an absent export is reported as ErrUnsupported
// rather than treated as a malformed module.
func (r *Runtime) ABIVersionE() (uint32, error) {
	if err := r.ensureOpen(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result, name, err := r.callLocked(r.Context(), []string{
		"tsw_abi_version",
		"sitterwasm_abi_version",
		"abi_version",
	})
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, &ABIError{Function: name, Message: "returned no ABI version"}
	}
	version, ok := checkedU32(result[0])
	if !ok {
		return 0, &ABIError{Function: name, Message: "returned a non-wasm32 ABI version"}
	}
	return version, nil
}

// AbiVersionE is the error-returning alias of ABIVersionE.
func (r *Runtime) AbiVersionE() (uint32, error) { return r.ABIVersionE() }

// Close releases the WASM module and runtime. It is idempotent.
func (r *Runtime) Close() error {
	if r == nil || r.closed.Swap(true) {
		return nil
	}
	goruntime.SetFinalizer(r, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	cleanupCtx := context.Background()
	var closeErr error
	if r.mod != nil {
		closeErr = r.mod.Close(cleanupCtx)
	}
	if r.owned && r.rt != nil {
		if err := r.rt.Close(cleanupCtx); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (r *Runtime) ensureOpen() error {
	if r.closedState() {
		return ErrClosed
	}
	return nil
}

// closedState reports whether a runtime or its underlying module can no
// longer accept calls.  The module may be closed independently by a caller of
// NewRuntimeFromModule, or automatically by wazero when a runtime configured
// with WithCloseOnContextDone observes cancellation.  Keeping this check in a
// single helper lets value-style accessors and destructors treat both cases
// consistently.
func (r *Runtime) closedState() bool {
	if r == nil || r.closed.Load() || r.mod == nil {
		return true
	}
	return r.mod.IsClosed()
}

// function resolves the first exported function with one of the supplied
// names. The aliases let the Go package work with both the canonical tsw_ ABI
// and early/experimental bridge modules.
func (r *Runtime) function(names ...string) (api.Function, string, error) {
	if len(names) == 0 {
		return nil, "", fmt.Errorf("%w: missing export name", ErrUnsupported)
	}
	if err := r.ensureOpen(); err != nil {
		return nil, "", err
	}
	for _, name := range names {
		if fn, ok := r.funcs[name]; ok {
			return fn, name, nil
		}
		if fn := r.mod.ExportedFunction(name); fn != nil {
			r.funcs[name] = fn
			return fn, name, nil
		}
	}
	// The module may have been closed between the initial lifecycle check and
	// the export lookup (for example by wazero's close-on-context-done mode).
	// Preserve ErrClosed in that race instead of misreporting a missing export.
	if r.closedState() {
		return nil, names[0], ErrClosed
	}
	return nil, names[0], fmt.Errorf("%w: %s", ErrUnsupported, names[0])
}

func (r *Runtime) call(ctx context.Context, names []string, args ...uint64) ([]uint64, string, error) {
	if ctx == nil {
		ctx = r.Context()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := r.function(names...)
	if err != nil {
		return nil, name, err
	}
	result, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, name, &ABIError{Function: name, Message: err.Error(), Cause: err}
	}
	return result, name, nil
}

// callLocked invokes an export while the runtime mutex is held. It is used by
// helpers that must read a returned pointer before another guest call can
// overwrite a temporary string.
func (r *Runtime) callLocked(ctx context.Context, names []string, args ...uint64) ([]uint64, string, error) {
	if ctx == nil {
		ctx = r.Context()
	}
	fn, name, err := r.function(names...)
	if err != nil {
		return nil, name, err
	}
	result, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, name, &ABIError{Function: name, Message: err.Error(), Cause: err}
	}
	return result, name, nil
}

func (r *Runtime) memory() (api.Memory, error) {
	if err := r.ensureOpen(); err != nil {
		return nil, err
	}
	m := r.mod.Memory()
	if m == nil {
		return nil, fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
	}
	return m, nil
}

// allocLocked is alloc's implementation with r.mu already held.
func (r *Runtime) allocLocked(size uint32) (uint32, error) {
	if err := r.ensureOpen(); err != nil {
		return 0, err
	}
	// Resolve allocator aliases by signature. A few Rust/Wasm toolchains export
	// `__wbindgen_malloc` in a two-argument (size, alignment) form; invoking
	// such a function with the C `malloc(size)` ABI can trap. Skip incompatible
	// exports and continue to the next alias instead.
	allocatorNames := []string{"tsw_alloc", "sitterwasm_alloc", "malloc", "__wbindgen_malloc"}
	var unsupportedErr error
	// Keep track of whether an allocator export was found at all.  The bump
	// allocator is only safe for modules that deliberately omit an allocator;
	// silently falling back when a module exports an incompatible allocator can
	// hand out memory that the guest's own free/realloc implementation cannot
	// understand.
	allocatorFound := false
	for _, allocatorName := range allocatorNames {
		fn := r.mod.ExportedFunction(allocatorName)
		if fn == nil {
			continue
		}
		allocatorFound = true
		paramTypes := fn.Definition().ParamTypes()
		resultTypes := fn.Definition().ResultTypes()
		// Most C/Rust shims use malloc(size).  wasm-bindgen has appeared in
		// both one-argument and two-argument forms (the latter accepts an
		// alignment); pass a conservative alignment of one when needed.  Do
		// not invoke arbitrary arities, since wazero reports an arity trap only
		// after entering the guest and a malformed module should produce a
		// regular Unsupported error instead.
		validParams := len(paramTypes) == 1
		if allocatorName == "__wbindgen_malloc" && len(paramTypes) == 2 {
			validParams = true
		}
		if validParams {
			for _, typ := range paramTypes {
				if typ != api.ValueTypeI32 && typ != api.ValueTypeI64 {
					validParams = false
					break
				}
			}
		}
		validResult := len(resultTypes) == 1 &&
			(resultTypes[0] == api.ValueTypeI32 || resultTypes[0] == api.ValueTypeI64)
		if !validParams || !validResult {
			unsupportedErr = fmt.Errorf("%w: allocator %s has unsupported signature", ErrUnsupported, allocatorName)
			continue
		}
		args := []uint64{uint64(size)}
		if allocatorName == "__wbindgen_malloc" && len(paramTypes) == 2 {
			args = append(args, 1) // alignment; one is valid for every non-zero size
		}
		out, callErr := fn.Call(r.ctx, args...)
		if callErr != nil || len(out) == 0 {
			if callErr == nil {
				callErr = ErrInvalidHandle
			}
			return 0, &ABIError{Function: allocatorName, Message: callErr.Error(), Cause: callErr}
		}
		ptr, ok := checkedU32(out[0])
		if !ok {
			return 0, &ABIError{Function: allocatorName, Message: "allocator returned a non-wasm32 pointer"}
		}
		if ptr == 0 {
			return 0, fmt.Errorf("sitterwasm: guest allocation of %d bytes failed", size)
		}
		// Allocators cross the ABI as wasm32 offsets.  Reject an offset that
		// cannot hold the requested block before any caller attempts a memory
		// write; otherwise a malformed custom allocator could make later code
		// silently operate on a wrapped/invalid pointer.
		if uint64(ptr)+uint64(size) > uint64(1)<<32 {
			return 0, &ABIError{Function: allocatorName, Message: "allocator returned an overflowing pointer"}
		}
		if mem := r.mod.Memory(); mem != nil {
			if ptr >= mem.Size() || size > mem.Size()-ptr {
				return 0, &ABIError{Function: allocatorName, Message: "allocator returned a pointer outside guest memory"}
			}
		}
		if r.allocSizes != nil {
			r.allocSizes[ptr] = size
		}
		return ptr, nil
	}
	if unsupportedErr == nil {
		unsupportedErr = fmt.Errorf("%w: allocator export", ErrUnsupported)
	}
	err := unsupportedErr
	if allocatorFound {
		// At least one allocator was exported but none had a supported ABI.
		// Refuse the unsafe bump fallback; callers can still provide an alias
		// module exporting tsw_alloc/malloc with the canonical signature.
		return 0, err
	}
	// A closed runtime must never fall through to the bump allocator. In
	// particular, Runtime.Close can race an allocation that was waiting for the
	// mutex; propagating ErrClosed avoids handing out pointers into a dead
	// module.
	if !errors.Is(err, ErrUnsupported) {
		return 0, err
	}
	// Conservative bump allocation fallback for test modules. Grow memory as
	// needed; this path is not used by the supplied bridge (which exports
	// tsw_alloc).
	mem := r.mod.Memory()
	if mem == nil {
		return 0, err
	}
	ptr := r.nextAlloc.Load()
	if ptr < 65536 {
		ptr = 65536
	}
	end := uint64(ptr) + uint64(size)
	// `end` is an exclusive address.  Although 2^32 is a valid exclusive
	// endpoint in the wasm32 address space, it cannot be represented by the
	// uint32 bump-pointer state and would wrap the next allocation to zero.
	// Reject that final one-byte boundary instead of handing out overlapping
	// pointers on the next call.
	if end >= uint64(1)<<32 {
		return 0, fmt.Errorf("sitterwasm: guest allocation exceeds wasm32 address space")
	}
	if end > uint64(mem.Size()) {
		pages := uint32((end - uint64(mem.Size()) + 65535) / 65536)
		if _, ok := mem.Grow(pages); !ok {
			return 0, fmt.Errorf("sitterwasm: guest memory exhausted")
		}
	}
	r.nextAlloc.Store(uint32(end))
	if r.allocSizes != nil {
		r.allocSizes[ptr] = size
	}
	return ptr, nil
}

func (r *Runtime) freeLocked(ptr uint32) {
	if ptr == 0 || r == nil || r.closedState() {
		return
	}
	// Retrieve the size before deleting the bookkeeping entry.  A pointer may
	// be reused by the guest after this call, so retaining stale sizes would be
	// worse than omitting them for unknown guest-owned blocks.
	size := uint32(0)
	if r.allocSizes != nil {
		size = r.allocSizes[ptr]
		delete(r.allocSizes, ptr)
	}
	for _, freeName := range []string{"tsw_free", "sitterwasm_free", "free", "__wbindgen_free"} {
		fn := r.mod.ExportedFunction(freeName)
		if fn == nil {
			continue
		}
		definition := fn.Definition()
		paramTypes := definition.ParamTypes()
		// A custom module may export an unrelated function under one of the
		// conventional deallocator names.  Calling it with the pointer-shaped
		// ABI arguments would make wazero raise an arity/type trap.  Restrict
		// dispatch to integer parameters, which are the only types that can
		// represent wasm32 offsets and sizes.  Result values are intentionally
		// ignored: C/Rust deallocators conventionally return void, while a few
		// compatibility shims return a status code that is safe to discard.
		validParams := true
		for _, typ := range paramTypes {
			if typ != api.ValueTypeI32 && typ != api.ValueTypeI64 {
				validParams = false
				break
			}
		}
		if !validParams {
			continue
		}
		params := len(paramTypes)
		var args []uint64
		switch params {
		case 1:
			args = []uint64{uint64(ptr)}
		case 2:
			// Common Rust shims use (ptr, size).
			if freeName != "__wbindgen_free" {
				continue
			}
			args = []uint64{uint64(ptr), uint64(size)}
		case 3:
			// wasm-bindgen's canonical deallocator is (ptr, size, align).
			if freeName != "__wbindgen_free" {
				continue
			}
			args = []uint64{uint64(ptr), uint64(size), 1}
		default:
			continue
		}
		_, _ = fn.Call(context.Background(), args...)
		return
	}
}

// freeRangesLocked releases a block returned by one of the legacy range
// accessors.  Those accessors historically returned an allocation prefixed
// with a count and, unlike ordinary host allocations, may expose an explicit
// `ranges_free(ptr, count)` export.  Calling such an export with the ordinary
// one-argument free ABI is a WebAssembly arity trap, so inspect the signature
// before invoking it.  The caller must hold r.mu.
func (r *Runtime) freeRangesLocked(ptr, count uint32) {
	if r == nil || ptr == 0 || r.closedState() {
		return
	}
	// The block consists of a four-byte count followed by 24-byte records. A
	// checked size is useful for wasm-bindgen-style three-argument deallocators
	// and also avoids wrapping when a malformed guest reports a huge count.
	blockSize := uint64(4) + uint64(count)*24
	if blockSize > uint64(^uint32(0)) {
		// A malformed guest count cannot be represented by the wasm32
		// deallocator ABI.  Do not pass a wrapped size/count to a custom free
		// function: that can make it walk arbitrary memory.  The module will
		// reclaim the allocation when it is closed, so leaking this one block is
		// preferable to corrupting the guest heap.
		return
	}
	if mem := r.mod.Memory(); mem != nil {
		if ptr >= mem.Size() || blockSize > uint64(mem.Size()-ptr) {
			return
		}
	}
	for _, name := range []string{"tsw_ranges_free", "sitterwasm_ranges_free", "ranges_free", "__wbindgen_free", "tsw_free", "sitterwasm_free", "free"} {
		fn := r.mod.ExportedFunction(name)
		if fn == nil {
			continue
		}
		paramTypes := fn.Definition().ParamTypes()
		validParams := true
		for _, typ := range paramTypes {
			if typ != api.ValueTypeI32 && typ != api.ValueTypeI64 {
				validParams = false
				break
			}
		}
		if !validParams {
			continue
		}
		params := len(paramTypes)
		var args []uint64
		switch params {
		case 1:
			args = []uint64{uint64(ptr)}
		case 2:
			// The explicit ranges_free ABI takes (ptr, count).  For
			// wasm-bindgen, the second argument is the allocation size.
			if name == "__wbindgen_free" {
				args = []uint64{uint64(ptr), blockSize}
			} else if name == "tsw_ranges_free" || name == "sitterwasm_ranges_free" || name == "ranges_free" {
				args = []uint64{uint64(ptr), uint64(count)}
			} else {
				continue
			}
		case 3:
			if name != "__wbindgen_free" {
				continue
			}
			args = []uint64{uint64(ptr), blockSize, 1}
		default:
			continue
		}
		_, _ = fn.Call(context.Background(), args...)
		if r.allocSizes != nil {
			delete(r.allocSizes, ptr)
		}
		return
	}
	// Preserve the ordinary allocator fallback when no dedicated range
	// deallocator exists.  This also handles modules that only export tsw_free.
	r.freeLocked(ptr)
}

// deleteGuestTreeWhileLocked releases a tree handle during an operation that
// still owns Runtime.mu.  It deliberately bypasses Runtime.function's
// ensureOpen check: Runtime.Close sets the closed bit before waiting for the
// in-flight call to finish, but the module itself is not closed until this
// lock is released.  This lets callers discard a just-produced tree without
// leaking it when Close wins a race with parsing.
func deleteGuestTreeWhileLocked(r *Runtime, handle uint32) {
	if r == nil || handle == 0 || r.mod == nil {
		return
	}
	for _, name := range []string{"tsw_tree_delete", "sitterwasm_tree_delete", "ts_tree_delete", "tree_delete"} {
		fn := r.funcs[name]
		if fn == nil {
			fn = r.mod.ExportedFunction(name)
			if fn != nil {
				r.funcs[name] = fn
			}
		}
		if fn == nil || len(fn.Definition().ParamTypes()) != 1 {
			continue
		}
		paramType := fn.Definition().ParamTypes()[0]
		if paramType != api.ValueTypeI32 && paramType != api.ValueTypeI64 {
			continue
		}
		_, _ = fn.Call(context.Background(), uint64(handle))
		return
	}
}

// callWithInput allocates a temporary guest buffer, copies data into it, calls
// an export whose arguments are prefix...,ptr,len, and finally frees the
// buffer. The entire transaction is serialized so another parser cannot
// overwrite the input between allocation and invocation.
func (r *Runtime) callWithInput(ctx context.Context, names []string, prefix []uint64, data []byte) ([]uint64, string, error) {
	if ctx == nil {
		ctx = r.Context()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uint64(len(data)) > uint64(^uint32(0)) {
		return nil, names[0], fmt.Errorf("sitterwasm: input exceeds uint32 byte offset")
	}
	ptr, err := r.allocLocked(uint32(len(data)))
	if err != nil {
		return nil, names[0], err
	}
	defer r.freeLocked(ptr)
	mem := r.mod.Memory()
	if mem == nil || !mem.Write(ptr, data) {
		return nil, names[0], io.ErrShortWrite
	}
	args := make([]uint64, 0, len(prefix)+2)
	args = append(args, prefix...)
	args = append(args, uint64(ptr), uint64(len(data)))
	result, name, err := r.callLocked(ctx, names, args...)
	return result, name, err
}

func (r *Runtime) readBytes(ptr, length uint32) ([]byte, error) {
	m, err := r.memory()
	if err != nil {
		return nil, err
	}
	b, ok := m.Read(ptr, length)
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (r *Runtime) readCString(ptr uint32) (string, error) {
	m, err := r.memory()
	if err != nil {
		return "", err
	}
	if ptr == 0 {
		return "", nil
	}
	if ptr >= m.Size() {
		return "", io.ErrUnexpectedEOF
	}
	// Limit scans to one memory size so malformed guest pointers cannot cause
	// an unbounded read.
	max := m.Size() - ptr
	b, ok := m.Read(ptr, max)
	if !ok {
		return "", io.ErrUnexpectedEOF
	}
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), nil
		}
	}
	return string(b), nil
}

// checkedU32 decodes a wasm32 handle/result without silently truncating an
// i64 value returned by a malformed or non-wasm32 compatibility module.
func checkedU32(v uint64) (uint32, bool) {
	if v > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(v), true
}

// checkedU16 decodes a wasm result that is documented to carry a 16-bit
// Tree-sitter symbol/field/state id.  Do not silently truncate an i64 result
// from a malformed compatibility module: doing so could make an invalid id
// appear to refer to an unrelated grammar symbol.
func checkedU16(v uint64) (uint16, bool) {
	if v > uint64(^uint16(0)) {
		return 0, false
	}
	return uint16(v), true
}

func packPoint(p Point) uint64 {
	return uint64(p.Row)<<32 | uint64(p.Column)
}

func unpackPoint(v uint64) Point {
	return Point{Row: uint32(v >> 32), Column: uint32(v)}
}

func putU32(dst []byte, off int, value uint32) {
	binary.LittleEndian.PutUint32(dst[off:off+4], value)
}

func getU32(src []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(src[off : off+4])
}
