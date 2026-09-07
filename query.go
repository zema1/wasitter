package wasitter

// Query support is deliberately delegated to the Tree-sitter guest whenever
// possible.  Query matching has a large amount of semantics (alternatives,
// repetitions, anchors and predicates); keeping it in the upstream C runtime
// is what gives the WASM binding the same results as the native library.  A
// small matcher is retained for old bridge modules that do not expose the
// optional query ABI.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	runtimepkg "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
)

// CaptureQuantifier mirrors TSQuantifier.
type CaptureQuantifier uint8

const (
	// CaptureQuantifierZero means that a capture is not repeated.
	CaptureQuantifierZero CaptureQuantifier = iota
	// CaptureQuantifierZeroOrOne means that a capture may occur zero or one time.
	CaptureQuantifierZeroOrOne
	// CaptureQuantifierZeroOrMore means that a capture may occur any number of times.
	CaptureQuantifierZeroOrMore
	// CaptureQuantifierOne means that a capture occurs exactly once.
	CaptureQuantifierOne
	// CaptureQuantifierOneOrMore means that a capture occurs at least once.
	CaptureQuantifierOneOrMore
)

// QueryErrorKind identifies why a query could not be compiled.
//
// The values intentionally follow go-tree-sitter's public API (Syntax starts
// at zero).  They therefore do not have the same numeric representation as
// the C TSQueryError enum, whose zero value is TSQueryErrorNone; raw values
// returned by the guest are translated by newQueryError below.
type QueryErrorKind uint8

const (
	// QueryErrorSyntax reports malformed query syntax.
	QueryErrorSyntax QueryErrorKind = iota
	// QueryErrorNodeType reports an unknown grammar node type.
	QueryErrorNodeType
	// QueryErrorField reports an unknown grammar field name.
	QueryErrorField
	// QueryErrorCapture reports an invalid capture name.
	QueryErrorCapture
	// QueryErrorPredicate reports an invalid predicate expression.
	QueryErrorPredicate
	// QueryErrorStructure reports an impossible query pattern.
	QueryErrorStructure
	// QueryErrorLanguage reports a query/language incompatibility.
	QueryErrorLanguage
)

// QueryErrorNone is retained for callers that inspect the raw C-style
// sentinel.  It is deliberately outside the public go-tree-sitter range so
// that QueryErrorSyntax remains zero as in the upstream Go binding.
const QueryErrorNone QueryErrorKind = ^QueryErrorKind(0)

// QueryErrorKindToString returns the stable, human-readable name used by
// Tree-sitter bindings for an error kind.
func QueryErrorKindToString(errorType QueryErrorKind) string {
	switch QueryErrorKind(errorType) {
	case QueryErrorNone:
		return "none"
	case QueryErrorSyntax:
		return "syntax"
	case QueryErrorNodeType:
		return "node type"
	case QueryErrorField:
		return "field"
	case QueryErrorCapture:
		return "capture"
	case QueryErrorPredicate:
		return "predicate"
	case QueryErrorStructure:
		return "structure"
	case QueryErrorLanguage:
		return "language"
	default:
		return "unknown"
	}
}

// QueryError reports a query compilation error and source location.
type QueryError struct {
	Message string
	Offset  uint32
	Row     uint32
	Column  uint32
	Kind    QueryErrorKind
}

func (e QueryError) Error() string {
	kind := e.Kind
	// Keep the diagnostic shape used by go-tree-sitter.  In particular, syntax
	// and structure errors carry the offending source line and a caret in
	// Message, while name errors carry the invalid token.  Prefixing those
	// messages with their category makes errors useful even when callers only
	// log Error() and do not inspect Kind.
	label := ""
	switch kind {
	case QueryErrorField:
		label = "Invalid field name "
	case QueryErrorNodeType:
		label = "Invalid node type "
	case QueryErrorCapture:
		label = "Invalid capture name "
	case QueryErrorPredicate:
		label = "Invalid predicate: "
	case QueryErrorStructure:
		label = "Impossible pattern:\n"
	case QueryErrorSyntax:
		label = "Invalid syntax:\n"
	}
	if label == "" {
		return e.Message
	}
	return fmt.Sprintf("Query error at %d:%d. %s%s", e.Row+1, e.Column+1, label, e.Message)
}

// QueryPredicateStepType and QueryPredicateStep describe optional predicate
// metadata exposed by newer bridge modules.
type QueryPredicateStepType uint8

const (
	// QueryPredicateStepDone terminates one predicate expression.
	QueryPredicateStepDone QueryPredicateStepType = iota
	// QueryPredicateStepCapture identifies a capture argument.
	QueryPredicateStepCapture
	// QueryPredicateStepString identifies a string-table argument.
	QueryPredicateStepString
)

// QueryPredicateStep is one wire-level step in a query predicate expression.
type QueryPredicateStep struct {
	Type    QueryPredicateStepType
	ValueID uint32
}

// QueryProperty describes a key/value property used by a query predicate.
// The guest remains responsible for evaluating predicates.
type QueryProperty struct {
	Key       string
	Value     *string
	CaptureID *uint32
}

// QueryPredicateArg is one argument passed to a general query predicate.
type QueryPredicateArg struct {
	CaptureID *uint32
	String    *string
}

// QueryPredicate describes a general (non-built-in) query predicate.
type QueryPredicate struct {
	Operator string
	Args     []QueryPredicateArg
}

// TextPredicateType identifies one of Tree-sitter's built-in text
// predicates.  The values intentionally follow the upstream Go binding.
type TextPredicateType uint8

const (
	// TextPredicateTypeEqCapture compares the text of two captures.
	TextPredicateTypeEqCapture TextPredicateType = iota
	// TextPredicateTypeEqString compares a capture's text with a string.
	TextPredicateTypeEqString
	// TextPredicateTypeMatchString matches a capture's text against a regexp.
	TextPredicateTypeMatchString
	// TextPredicateTypeAnyString tests a capture against a set of strings.
	TextPredicateTypeAnyString
)

// TextPredicateCapture describes a built-in text predicate attached to a
// query pattern. Value is one of: a uint capture id (for EqCapture), a
// string (for EqString), a *regexp.Regexp (for MatchString), or []string (for
// AnyString). Keeping Value as any mirrors Tree-sitter's upstream binding and
// lets callers inspect all predicate forms without a second tagged union.
type TextPredicateCapture struct {
	Value         any
	Type          TextPredicateType
	CaptureID     uint32
	Positive      bool
	MatchAllNodes bool
}

// PropertyPredicate describes an is?/is-not? predicate.
type PropertyPredicate struct {
	Property QueryProperty
	Positive bool
}

type fallbackPattern struct {
	nodeType string
	capture  string
	literal  bool // nodeType came from a quoted anonymous-token literal
	// rooted reports whether the source-level pattern has one root node.  The
	// compact matcher cannot reproduce all query structure, but this bit is
	// still useful to callers inspecting metadata on legacy bridges. Anonymous
	// literal patterns (for example `"," @comma`) are intentionally unrooted,
	// matching ts_query_is_pattern_rooted's treatment of grammar tokens.
	rooted bool
	// nodeOffset points at the node-type token within source. It is used only
	// for diagnostics on legacy bridges, whose query compiler is unavailable
	// to report TSQueryErrorNodeType offsets directly.
	nodeOffset int
	// patternIndex identifies the original top-level query pattern. An
	// alternative expression (`[(number) (string)]`) is one Tree-sitter
	// pattern even though the compatibility matcher materializes one simple
	// branch per node type, so all of its branches share this index.
	patternIndex uint32
	// sourceStart/sourceEnd delimit the enclosing pattern in the original
	// query source.  Legacy bridges do not expose ts_query_* pattern byte
	// ranges, so retaining these offsets lets host-side predicate metadata stay
	// scoped to its own pattern when capture names are reused.
	sourceStart int
	sourceEnd   int
	// captures retains every capture attached to a simple fallback pattern.
	// The original compatibility matcher only carried one name; keeping the
	// legacy field populated as the first entry preserves old callers while
	// allowing multi-capture patterns to expose a useful result.
	captures []string
	// quantifiers records the occurrence modifier for each capture name in
	// this source-level pattern. The fallback matcher does not reproduce the
	// full repetition state machine, but exposing these modifiers keeps the
	// query metadata API useful on legacy bridges (`@x?`, `@x*`, `@x+`).
	quantifiers map[string]CaptureQuantifier
	// predicates are the built-in text predicates belonging to this pattern.
	// Keeping them on the materialized compatibility pattern avoids applying a
	// source-wide predicate list to every pattern when capture names are reused
	// across alternatives or top-level patterns.
	predicates []textPredicate
}

// textPredicate is the subset of Tree-sitter's built-in text predicates that
// the host must evaluate.  The C query engine records predicate steps but
// cannot inspect source bytes through this ABI, so evaluating these here is
// both necessary and consistent with the upstream Go binding.
type textPredicate struct {
	op           string
	capture      string
	otherCapture string
	value        string
	values       []string
	regex        *regexp.Regexp
}

// Query is a compiled query associated with one language.
type Query struct {
	mu       sync.RWMutex
	language *Language
	runtime  *Runtime
	// handle is atomic so Handle/Close and guest calls can safely race at the
	// Go level.  Runtime.mu still serializes the actual WASM invocation.
	handle        atomic.Uint32
	source        string
	native        bool
	fallback      []fallbackPattern
	fallbackNames []string
	// fallbackPatternCount is the number of original top-level patterns. It
	// can be smaller than len(fallback) when an alternative is flattened into
	// several compatibility branches.
	fallbackPatternCount uint32
	// disabledCaptures/disabledPatterns mirror the native query mutators on
	// the host side. They are also consulted when a partially exported bridge
	// has a compiler but no cursor ABI and execution falls back to the compact
	// matcher.
	disabledCaptures map[string]struct{}
	disabledPatterns map[uint32]struct{}
	predicates       []textPredicate
	predicateMu      sync.Mutex
	predicateCache   map[uint32][]textPredicate

	// TextPredicates is populated for native queries from the guest's
	// predicate-step metadata. It is exported for parity with the upstream Go
	// binding; callers should treat the returned slices as read-only.
	TextPredicates     [][]TextPredicateCapture
	propertyPredicates [][]PropertyPredicate
	propertySettings   [][]QueryProperty
	generalPredicates  [][]QueryPredicate
	closed             atomic.Bool
}

// NewQuery compiles a Tree-sitter query for language. Source is UTF-8 query
// text, for example "(identifier) @name". Compilation errors can be inspected
// with errors.As and [QueryError]. Close the query before its [Runtime].
// The query keeps its own language reference; the caller may close language
// after construction. Execute queries using [QueryCursor].
func NewQuery(language *Language, source string) (*Query, error) {
	if language == nil {
		return nil, ErrNoLanguage
	}
	if err := language.ensureOpen(); err != nil {
		return nil, err
	}
	// Language is an immutable view whose Close method only invalidates that
	// wrapper. Keep a private copy so callers may safely close/reuse their
	// temporary language value after compiling a query.
	q := &Query{language: language.clone(), runtime: language.runtime, source: source, predicateCache: make(map[uint32][]textPredicate)}
	if q.runtime != nil {
		handle, compileErr, supported := q.compileNative(source)
		if supported {
			if compileErr != nil {
				return nil, compileErr
			}
			q.handle.Store(handle)
			q.native = true
			// The C runtime deliberately leaves predicate evaluation to the
			// host binding.  Validate the built-in predicates here as the
			// upstream Go binding does, so malformed arity/types (and invalid
			// regular expressions) fail at compilation instead of silently
			// producing surprising matches later.
			if predicateErr := q.validateNativePredicates(); predicateErr != nil {
				_ = q.Close()
				return nil, predicateErr
			}
			q.predicates = parseTextPredicates(source)
			q.populateMetadata()
			runtimepkg.SetFinalizer(q, func(query *Query) { _ = query.Close() })
			return q, nil
		}
	}
	patterns, err := parseFallbackPatterns(source)
	if err != nil {
		return nil, err
	}
	q.fallback = patterns
	q.fallbackNames = fallbackCaptureNames(patterns)
	q.fallbackPatternCount = fallbackPatternCount(patterns)
	if validationErr := validateFallbackNodeTypes(q.language, q.source, patterns); validationErr != nil {
		return nil, validationErr
	}
	if validationErr := validateFallbackFields(q.language, q.source); validationErr != nil {
		return nil, validationErr
	}
	q.predicates = parseTextPredicates(source)
	// Legacy bridge modules do not expose Tree-sitter's query compiler, so
	// there is no guest-side validation of predicate arity/types (or regex
	// syntax).  Run the same validation that the native path performs before
	// publishing the compatibility query.  Without this check malformed
	// built-ins such as `#eq? @n` were silently accepted and either ignored or
	// evaluated as an unrelated equality test.
	if predicateErr := validateFallbackPredicates(source, q.fallbackNames); predicateErr != nil {
		return nil, predicateErr
	}
	q.populateMetadata()
	runtimepkg.SetFinalizer(q, func(query *Query) { _ = query.Close() })
	return q, nil
}

func (q *Query) ensureOpen() error {
	if q == nil || q.closed.Load() {
		return ErrClosed
	}
	if q.runtime == nil {
		return ErrNoRuntime
	}
	if q.language != nil {
		if err := q.language.ensureOpen(); err != nil {
			return err
		}
	}
	return q.runtime.ensureOpen()
}

// Handle returns the opaque guest query handle.
func (q *Query) Handle() uint32 {
	// A query handle is an address into the guest module.  Runtime.Close (or
	// wazero's close-on-context-done mode) invalidates that address even when
	// Query.Close has not been called yet, so mirror Node/Language/Parser and
	// hide stale pointers from callers once the owning runtime is gone.
	if q == nil || q.closed.Load() || q.runtime == nil || q.runtime.closedState() {
		return 0
	}
	handle := q.handle.Load()
	// Pair the initial check with a second one after the atomic load.  This
	// closes the small race where Query.Close or Runtime.Close runs between the
	// first check and returning the value.
	if q.closed.Load() || q.runtime.closedState() {
		return 0
	}
	return handle
}

// Source returns the original UTF-8 query source.
func (q *Query) Source() string {
	if q == nil {
		return ""
	}
	return q.source
}

// PatternCount reports the number of top-level patterns in the query.
func (q *Query) PatternCount() uint32 {
	if q == nil || q.closed.Load() {
		return 0
	}
	if q.native {
		return q.queryUint([]string{"tsw_query_pattern_count", "wasitter_query_pattern_count", "ts_query_pattern_count", "query_pattern_count"})
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.fallbackPatternCount != 0 {
		return q.fallbackPatternCount
	}
	return uint32(len(q.fallback))
}

// CaptureCount reports the number of distinct capture names in the query.
func (q *Query) CaptureCount() uint32 {
	if q == nil || q.closed.Load() {
		return 0
	}
	if q.native {
		return q.queryUint([]string{"tsw_query_capture_count", "wasitter_query_capture_count", "ts_query_capture_count", "query_capture_count"})
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	return uint32(len(q.fallbackNames))
}

// StringCount reports the number of string literals in the query string table.
func (q *Query) StringCount() uint32 {
	if q == nil || q.closed.Load() || !q.native {
		return 0
	}
	return q.queryUint([]string{"tsw_query_string_count", "wasitter_query_string_count", "ts_query_string_count", "query_string_count"})
}

// CaptureName returns the capture name associated with id, or an empty string
// when id is not present.
func (q *Query) CaptureName(id uint32) string {
	s, _ := q.CaptureNameE(id)
	return s
}

// CaptureNameE returns the capture name associated with id and reports query
// or ABI errors that CaptureName intentionally hides.
func (q *Query) CaptureNameE(id uint32) (string, error) {
	if q == nil || q.closed.Load() {
		return "", ErrClosed
	}
	if q.native {
		return q.queryString(id,
			[]string{"tsw_query_capture_name_ptr", "tsw_query_capture_name", "wasitter_query_capture_name", "ts_query_capture_name_for_id", "query_capture_name"},
			[]string{"tsw_query_capture_name_len", "wasitter_query_capture_name_len", "ts_query_capture_name_len", "query_capture_name_len"})
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if uint64(id) < uint64(len(q.fallbackNames)) {
		return q.fallbackNames[id], nil
	}
	return "", nil
}

// CaptureQuantifierForID returns the repetition modifier for a capture in a
// pattern.
func (q *Query) CaptureQuantifierForID(pattern, capture uint32) CaptureQuantifier {
	if q == nil || q.closed.Load() {
		return CaptureQuantifierZero
	}
	if !q.native {
		q.mu.RLock()
		defer q.mu.RUnlock()
		if uint64(capture) >= uint64(len(q.fallbackNames)) {
			return CaptureQuantifierZero
		}
		name := q.fallbackNames[capture]
		for _, candidate := range q.fallback {
			if candidate.patternIndex != pattern || candidate.quantifiers == nil {
				continue
			}
			if value, ok := candidate.quantifiers[name]; ok {
				return value
			}
		}
		return CaptureQuantifierZero
	}
	return CaptureQuantifier(q.queryUintWithArgs([]string{"tsw_query_capture_quantifier_for_id", "wasitter_query_capture_quantifier_for_id", "ts_query_capture_quantifier_for_id", "query_capture_quantifier_for_id"}, uint64(pattern), uint64(capture)))
}

// StringValue returns a string-table entry by id, or an empty string when the
// id is unavailable.
func (q *Query) StringValue(id uint32) string {
	s, _ := q.StringValueE(id)
	return s
}

// StringValueE returns a string-table entry and reports query or ABI errors.
func (q *Query) StringValueE(id uint32) (string, error) {
	if q == nil || q.closed.Load() {
		return "", ErrClosed
	}
	if !q.native {
		return "", nil
	}
	return q.queryString(id,
		[]string{"tsw_query_string_value_ptr", "tsw_query_string_value", "wasitter_query_string_value", "ts_query_string_value_for_id", "query_string_value"},
		[]string{"tsw_query_string_value_len", "wasitter_query_string_value_len", "ts_query_string_value_len", "query_string_value_len"})
}

// StartByteForPattern returns the source byte offset where pattern begins.
func (q *Query) StartByteForPattern(pattern uint32) uint32 {
	if q == nil || q.closed.Load() {
		return 0
	}
	if !q.native {
		q.mu.RLock()
		candidate, found := q.fallbackPatternForGroup(pattern)
		q.mu.RUnlock()
		if !found || candidate.sourceStart < 0 || uint64(candidate.sourceStart) > uint64(^uint32(0)) {
			return 0
		}
		return uint32(candidate.sourceStart)
	}
	return q.queryUintWithArgs([]string{"tsw_query_start_byte_for_pattern", "wasitter_query_start_byte_for_pattern", "ts_query_start_byte_for_pattern", "query_start_byte_for_pattern"}, uint64(pattern))
}

// EndByteForPattern returns the source byte offset immediately after pattern.
func (q *Query) EndByteForPattern(pattern uint32) uint32 {
	if q == nil || q.closed.Load() {
		return 0
	}
	if !q.native {
		q.mu.RLock()
		candidate, found := q.fallbackPatternForGroup(pattern)
		q.mu.RUnlock()
		if !found || candidate.sourceEnd < 0 || uint64(candidate.sourceEnd) > uint64(^uint32(0)) {
			return 0
		}
		return uint32(candidate.sourceEnd)
	}
	return q.queryUintWithArgs([]string{"tsw_query_end_byte_for_pattern", "wasitter_query_end_byte_for_pattern", "ts_query_end_byte_for_pattern", "query_end_byte_for_pattern"}, uint64(pattern))
}

// IsPatternRooted reports whether pattern has a single root node.
func (q *Query) IsPatternRooted(pattern uint32) bool {
	if q == nil || q.closed.Load() {
		return false
	}
	if !q.native {
		q.mu.RLock()
		candidate, found := q.fallbackPatternForGroup(pattern)
		q.mu.RUnlock()
		return found && candidate.rooted
	}
	return q.queryBool([]string{"tsw_query_is_pattern_rooted", "wasitter_query_is_pattern_rooted", "ts_query_is_pattern_rooted", "query_is_pattern_rooted"}, pattern)
}

// IsPatternNonLocal reports whether pattern contains a non-local rule.
func (q *Query) IsPatternNonLocal(pattern uint32) bool {
	return q.queryBool([]string{"tsw_query_is_pattern_non_local", "wasitter_query_is_pattern_non_local", "ts_query_is_pattern_non_local", "query_is_pattern_non_local"}, pattern)
}

// IsPatternGuaranteedAtStep reports whether a bytecode step is guaranteed to
// advance a pattern match.
func (q *Query) IsPatternGuaranteedAtStep(offset uint32) bool {
	return q.queryBool([]string{"tsw_query_is_pattern_guaranteed_at_step", "wasitter_query_is_pattern_guaranteed_at_step", "ts_query_is_pattern_guaranteed_at_step", "query_is_pattern_guaranteed_at_step"}, offset)
}

// DisableCapture disables all captures with name for subsequent executions.
func (q *Query) DisableCapture(name string) error {
	if err := q.ensureOpen(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed.Load() {
		return ErrClosed
	}
	if !q.native {
		if q.disabledCaptures == nil {
			q.disabledCaptures = make(map[string]struct{})
		}
		q.disabledCaptures[name] = struct{}{}
		for i := range q.fallback {
			if q.fallback[i].capture == name {
				q.fallback[i].capture = ""
			}
			for j := range q.fallback[i].captures {
				if q.fallback[i].captures[j] == name {
					q.fallback[i].captures[j] = ""
				}
			}
		}
		return nil
	}
	// The query ABI carries both the source pointer and length as uint32.
	// Converting an oversized Go string directly to uint32 would wrap and make
	// the guest read a truncated (or unrelated) range.  Report the condition
	// before allocating or invoking the guest.
	if uint64(len(name)) > uint64(^uint32(0)) {
		return fmt.Errorf("wasitter: capture name exceeds uint32 wasm length")
	}
	r := q.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, fnName, err := r.function("tsw_query_disable_capture", "wasitter_query_disable_capture", "ts_query_disable_capture", "query_disable_capture")
	if err != nil {
		return err
	}
	ptr, err := r.allocLocked(uint32(len(name)))
	if err != nil {
		return err
	}
	defer r.freeLocked(ptr)
	if mem := r.mod.Memory(); mem == nil || !mem.Write(ptr, []byte(name)) {
		return fmt.Errorf("wasitter: cannot write capture name")
	}
	if _, callErr := fn.Call(r.Context(), uint64(q.handle.Load()), uint64(ptr), uint64(len(name))); callErr != nil {
		return &ABIError{Function: fnName, Message: callErr.Error()}
	}
	if q.disabledCaptures == nil {
		q.disabledCaptures = make(map[string]struct{})
	}
	q.disabledCaptures[name] = struct{}{}
	return nil
}

// DisablePattern disables a top-level pattern for subsequent executions.
func (q *Query) DisablePattern(pattern uint32) error {
	if err := q.ensureOpen(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed.Load() {
		return ErrClosed
	}
	if !q.native {
		if q.disabledPatterns == nil {
			q.disabledPatterns = make(map[uint32]struct{})
		}
		q.disabledPatterns[pattern] = struct{}{}
		// Alternatives are flattened into several compatibility branches but
		// share one source-level pattern index. Disable every branch in that
		// group, matching ts_query_disable_pattern's behavior.
		for i := range q.fallback {
			if q.fallback[i].patternIndex == pattern {
				q.fallback[i].nodeType = ""
			}
		}
		return nil
	}
	_, _, err := q.runtime.call(q.runtime.Context(), []string{"tsw_query_disable_pattern", "wasitter_query_disable_pattern", "ts_query_disable_pattern", "query_disable_pattern"}, uint64(q.handle.Load()), uint64(pattern))
	if err == nil {
		q.disabledPatternsLockless(pattern)
	}
	return err
}

// disabledPatternsLockless records a successful native mutation. Callers hold
// q.mu while invoking this helper.
func (q *Query) disabledPatternsLockless(pattern uint32) {
	if q.disabledPatterns == nil {
		q.disabledPatterns = make(map[uint32]struct{})
	}
	q.disabledPatterns[pattern] = struct{}{}
}

// PredicateSteps is optional because the compact ABI does not expose raw
// predicate arrays yet.
func (q *Query) PredicateSteps(pattern uint32) ([]QueryPredicateStep, error) {
	if err := q.ensureOpen(); err != nil {
		return nil, err
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed.Load() {
		return nil, ErrClosed
	}
	if !q.native {
		return nil, fmt.Errorf("%w: query predicate metadata", ErrUnsupported)
	}
	r := q.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, name, err := r.function(
		"tsw_query_predicates_for_pattern_into",
		"wasitter_query_predicates_for_pattern_into",
		"ts_query_predicates_for_pattern_into",
		"query_predicates_for_pattern_into",
	)
	if err != nil {
		return nil, err
	}
	probe, callErr := fn.Call(r.Context(), uint64(q.handle.Load()), uint64(pattern), 0, 0)
	if callErr != nil {
		return nil, &ABIError{Function: name, Message: callErr.Error()}
	}
	if len(probe) == 0 {
		return nil, &ABIError{Function: name, Message: "predicate metadata probe returned no size"}
	}
	required, ok := checkedU32(probe[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "predicate metadata size is not a wasm32 value"}
	}
	if required == 0 {
		return nil, nil
	}
	// A real query cannot contain anywhere near this many predicate steps. Keep
	// a hard upper bound so a malformed/custom guest cannot force the host to
	// allocate most of the wasm address space during a metadata probe.
	if required > 64<<20 {
		return nil, &ABIError{Function: name, Message: "predicate metadata size is unreasonably large"}
	}
	if required%8 != 0 {
		return nil, &ABIError{Function: name, Message: "invalid predicate metadata size"}
	}
	ptr, allocErr := r.allocLocked(required)
	if allocErr != nil {
		return nil, allocErr
	}
	defer r.freeLocked(ptr)
	result, callErr := fn.Call(r.Context(), uint64(q.handle.Load()), uint64(pattern), uint64(ptr), uint64(required))
	if callErr != nil {
		return nil, &ABIError{Function: name, Message: callErr.Error()}
	}
	if len(result) == 0 {
		return nil, &ABIError{Function: name, Message: "predicate metadata call returned no size"}
	}
	resultSize, ok := checkedU32(result[0])
	if !ok {
		return nil, &ABIError{Function: name, Message: "predicate metadata result is not a wasm32 value"}
	}
	if resultSize < required {
		return nil, &ABIError{Function: name, Message: "predicate metadata was truncated"}
	}
	mem := r.mod.Memory()
	if mem == nil {
		return nil, fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
	}
	b, ok := mem.Read(ptr, required)
	if !ok {
		return nil, fmt.Errorf("wasitter: predicate metadata points outside guest memory")
	}
	steps := make([]QueryPredicateStep, 0, required/8)
	for off := uint32(0); off < required; off += 8 {
		valueID := binary.LittleEndian.Uint32(b[off+4 : off+8])
		steps = append(steps, QueryPredicateStep{
			Type:    QueryPredicateStepType(binary.LittleEndian.Uint32(b[off : off+4])),
			ValueID: valueID,
		})
	}
	return steps, nil
}

// PredicatesForPattern returns predicate steps split at Done sentinels. The
// returned slices and their elements are independent copies, so callers may
// inspect or modify them without changing query state. This mirrors
// go-tree-sitter's PredicatesForPattern API while retaining PredicateSteps for
// callers that need the flat wire representation.
func (q *Query) PredicatesForPattern(pattern uint32) [][]QueryPredicateStep {
	steps, err := q.PredicateSteps(pattern)
	if err != nil || len(steps) == 0 {
		return nil
	}
	result := make([][]QueryPredicateStep, 0, 1)
	start := 0
	for i, step := range steps {
		if step.Type != QueryPredicateStepDone {
			continue
		}
		// Done is a wire-format separator, not part of the public predicate
		// group. The upstream PredicatesForPattern API omits this sentinel;
		// callers that need it can use PredicateSteps instead.
		if i > start {
			part := append([]QueryPredicateStep(nil), steps[start:i]...)
			result = append(result, part)
		}
		start = i + 1
	}
	if start < len(steps) {
		result = append(result, append([]QueryPredicateStep(nil), steps[start:]...))
	}
	return result
}

// PredicatesForPatternE is the error-returning form of PredicatesForPattern.
func (q *Query) PredicatesForPatternE(pattern uint32) ([][]QueryPredicateStep, error) {
	steps, err := q.PredicateSteps(pattern)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, nil
	}
	result := make([][]QueryPredicateStep, 0, 1)
	start := 0
	for i, step := range steps {
		if step.Type == QueryPredicateStepDone {
			if i > start {
				result = append(result, append([]QueryPredicateStep(nil), steps[start:i]...))
			}
			start = i + 1
		}
	}
	if start < len(steps) {
		result = append(result, append([]QueryPredicateStep(nil), steps[start:]...))
	}
	return result, nil
}

// CaptureNames returns the names of all captures in query order. The result
// is a copy, so modifying it does not mutate query state.
func (q *Query) CaptureNames() []string {
	if q == nil || q.closed.Load() {
		return nil
	}
	count := q.CaptureCount()
	if count == 0 {
		return []string{}
	}
	result := make([]string, count)
	for i := uint32(0); i < count; i++ {
		result[i] = q.CaptureName(i)
	}
	return result
}

// CaptureNamesE is the error-returning form of CaptureNames. A malformed or
// closed query stops at the first ABI error.
func (q *Query) CaptureNamesE() ([]string, error) {
	if err := q.ensureOpen(); err != nil {
		return nil, err
	}
	count := q.CaptureCount()
	result := make([]string, count)
	for i := uint32(0); i < count; i++ {
		name, err := q.CaptureNameE(i)
		if err != nil {
			return nil, err
		}
		result[i] = name
	}
	return result, nil
}

// CaptureIndexForName returns the numeric capture id for name. It follows the
// upstream API's uint result while CaptureIndexForName32 provides the native
// wasm-width spelling for code that prefers uint32.
func (q *Query) CaptureIndexForName(name string) (uint, bool) {
	if q == nil || q.closed.Load() {
		return 0, false
	}
	for i, candidate := range q.CaptureNames() {
		if candidate == name {
			return uint(i), true
		}
	}
	return 0, false
}

// CaptureIndexForName32 is the uint32 counterpart of CaptureIndexForName.
func (q *Query) CaptureIndexForName32(name string) (uint32, bool) {
	index, ok := q.CaptureIndexForName(name)
	return uint32(index), ok
}

// CaptureQuantifiers returns the quantifier for every capture id in a
// pattern. Invalid/closed patterns return nil, matching the package's
// value-oriented error policy; CaptureQuantifiersE exposes the error.
func (q *Query) CaptureQuantifiers(pattern uint) []CaptureQuantifier {
	// The guest ABI indexes patterns with uint32.  Do not let a wider native
	// uint silently wrap around and expose quantifiers for an unrelated pattern
	// on 64-bit hosts.
	if uint64(pattern) > uint64(^uint32(0)) {
		return nil
	}
	values, _ := q.CaptureQuantifiersE(uint32(pattern))
	return values
}

// CaptureQuantifiersE is the error-returning form of CaptureQuantifiers.
func (q *Query) CaptureQuantifiersE(pattern uint32) ([]CaptureQuantifier, error) {
	if err := q.ensureOpen(); err != nil {
		return nil, err
	}
	if pattern >= q.PatternCount() {
		return nil, fmt.Errorf("wasitter: pattern index %d out of range", pattern)
	}
	count := q.CaptureCount()
	result := make([]CaptureQuantifier, count)
	for capture := uint32(0); capture < count; capture++ {
		result[capture] = q.CaptureQuantifierForID(pattern, capture)
	}
	return result, nil
}

// CaptureQuantifiersForPattern is an alias that accepts the package's native
// uint32 index type.
func (q *Query) CaptureQuantifiersForPattern(pattern uint32) []CaptureQuantifier {
	values, _ := q.CaptureQuantifiersE(pattern)
	return values
}

// TextPredicatesForPattern returns the built-in text predicates attached to a
// pattern. The returned slice is a copy.
func (q *Query) TextPredicatesForPattern(pattern uint32) []TextPredicateCapture {
	if q == nil || q.closed.Load() || uint64(pattern) >= uint64(len(q.TextPredicates)) {
		return nil
	}
	return cloneTextPredicates(q.TextPredicates[pattern])
}

// PropertyPredicates returns is?/is-not? predicates for a pattern.
func (q *Query) PropertyPredicates(pattern uint) []PropertyPredicate {
	if q == nil || q.closed.Load() || uint64(pattern) >= uint64(len(q.propertyPredicates)) {
		return nil
	}
	return clonePropertyPredicates(q.propertyPredicates[pattern])
}

// PropertySettings returns set! properties for a pattern.
func (q *Query) PropertySettings(pattern uint) []QueryProperty {
	if q == nil || q.closed.Load() || uint64(pattern) >= uint64(len(q.propertySettings)) {
		return nil
	}
	return cloneQueryProperties(q.propertySettings[pattern])
}

// GeneralPredicates returns user-defined predicates for a pattern.
func (q *Query) GeneralPredicates(pattern uint) []QueryPredicate {
	if q == nil || q.closed.Load() || uint64(pattern) >= uint64(len(q.generalPredicates)) {
		return nil
	}
	return cloneGeneralPredicates(q.generalPredicates[pattern])
}

func cloneTextPredicates(in []TextPredicateCapture) []TextPredicateCapture {
	if in == nil {
		return []TextPredicateCapture{}
	}
	out := make([]TextPredicateCapture, len(in))
	copy(out, in)
	for i := range out {
		if values, ok := out[i].Value.([]string); ok {
			// Preserve the distinction between a nil value (which denotes an
			// unavailable/malformed predicate payload) and an explicitly empty
			// literal list. Tree-sitter's `#any-of? @capture` form is valid and
			// exposes a non-nil, zero-length []string in TextPredicates; using a
			// nil append target here would collapse that observable shape when
			// callers request the defensive copy through
			// TextPredicatesForPattern.
			if values == nil {
				out[i].Value = []string(nil)
			} else {
				out[i].Value = append([]string{}, values...)
			}
		}
	}
	return out
}

func cloneQueryProperties(in []QueryProperty) []QueryProperty {
	if in == nil {
		return []QueryProperty{}
	}
	out := make([]QueryProperty, len(in))
	for i, property := range in {
		out[i] = property
		if property.Value != nil {
			value := *property.Value
			out[i].Value = &value
		}
		if property.CaptureID != nil {
			id := *property.CaptureID
			out[i].CaptureID = &id
		}
	}
	return out
}

func clonePropertyPredicates(in []PropertyPredicate) []PropertyPredicate {
	if in == nil {
		return []PropertyPredicate{}
	}
	out := make([]PropertyPredicate, len(in))
	for i, predicate := range in {
		out[i] = predicate
		props := cloneQueryProperties([]QueryProperty{predicate.Property})
		out[i].Property = props[0]
	}
	return out
}

func cloneGeneralPredicates(in []QueryPredicate) []QueryPredicate {
	if in == nil {
		return []QueryPredicate{}
	}
	out := make([]QueryPredicate, len(in))
	for i, predicate := range in {
		out[i].Operator = predicate.Operator
		out[i].Args = make([]QueryPredicateArg, len(predicate.Args))
		for j, arg := range predicate.Args {
			out[i].Args[j] = arg
			if arg.CaptureID != nil {
				id := *arg.CaptureID
				out[i].Args[j].CaptureID = &id
			}
			if arg.String != nil {
				value := *arg.String
				out[i].Args[j].String = &value
			}
		}
	}
	return out
}

// NewQueryProperty constructs query metadata. A nil captureID denotes a
// property not tied to a capture. It copies captureID when provided.
func NewQueryProperty(key string, value *string, captureID *uint32) QueryProperty {
	property := QueryProperty{Key: key, Value: value}
	if captureID != nil {
		copyID := *captureID
		property.CaptureID = &copyID
	}

	return property
}

// populateMetadata eagerly decodes predicate steps so the exported
// TextPredicates field is useful immediately after NewQuery. Metadata is
// optional in old bridge modules; failures are intentionally ignored there.
func (q *Query) populateMetadata() {
	if q == nil {
		return
	}
	patterns := len(q.fallback)
	if q.fallbackPatternCount != 0 {
		patterns = int(q.fallbackPatternCount)
	}
	if q.native {
		patterns = int(q.PatternCount())
	}
	if patterns < 0 {
		patterns = 0
	}
	q.TextPredicates = make([][]TextPredicateCapture, patterns)
	q.propertyPredicates = make([][]PropertyPredicate, patterns)
	q.propertySettings = make([][]QueryProperty, patterns)
	q.generalPredicates = make([][]QueryPredicate, patterns)
	// The upstream Go binding exposes empty (non-nil) slices for patterns that
	// have no predicate metadata. Initialize every slot accordingly; callers
	// often compare these vectors with reflect.DeepEqual and should observe the
	// same shape regardless of whether the metadata came from the native ABI or
	// the source-level fallback decoder.
	for i := 0; i < patterns; i++ {
		q.TextPredicates[i] = []TextPredicateCapture{}
		q.propertyPredicates[i] = []PropertyPredicate{}
		q.propertySettings[i] = []QueryProperty{}
		q.generalPredicates[i] = []QueryPredicate{}
	}
	for i := 0; i < patterns; i++ {
		if q.native {
			steps, err := q.PredicateSteps(uint32(i))
			if err == nil {
				text, properties, settings, general := q.publicPredicatesFromSteps(steps)
				if text == nil {
					text = []TextPredicateCapture{}
				}
				if properties == nil {
					properties = []PropertyPredicate{}
				}
				if settings == nil {
					settings = []QueryProperty{}
				}
				if general == nil {
					general = []QueryPredicate{}
				}
				q.TextPredicates[i] = text
				q.propertyPredicates[i] = properties
				q.propertySettings[i] = settings
				q.generalPredicates[i] = general
				continue
			}
		}
		// Legacy modules do not expose predicate metadata. Use the source
		// offsets retained by the fallback parser when available; a partially
		// exported native bridge may instead provide ts_query_* pattern ranges.
		// The source-wide scanner remains the final compatibility fallback for
		// old artifacts that predate both forms of range metadata. Decode all
		// four public predicate vectors here, not just text predicates: bare
		// `#set! name value` and user-defined predicates are common in editor
		// integrations and should not disappear merely because a bridge lacks the
		// optional predicate-step export.
		patternSource := ""
		if !q.native {
			if pattern, found := q.fallbackPatternForGroup(uint32(i)); found {
				if pattern.sourceStart >= 0 && pattern.sourceEnd > pattern.sourceStart && pattern.sourceEnd <= len(q.source) {
					patternSource = q.source[pattern.sourceStart:pattern.sourceEnd]
				}
			}
		} else if q.native {
			if scoped, ok := q.sourcePredicatesForPattern(uint32(i)); ok {
				// A partial native bridge may expose pattern byte ranges but not
				// predicate steps. Keep the scoped text list and decode property /
				// general metadata from the same source span below.
				patternSource = q.source[q.StartByteForPattern(uint32(i)):q.EndByteForPattern(uint32(i))]
				q.TextPredicates[i] = q.publicTextPredicatesFromPrivate(scoped)
				if q.TextPredicates[i] == nil {
					q.TextPredicates[i] = []TextPredicateCapture{}
				}
				_, properties, settings, general := fallbackMetadataFromSource(patternSource, q.CaptureNames())
				if properties == nil {
					properties = []PropertyPredicate{}
				}
				if settings == nil {
					settings = []QueryProperty{}
				}
				if general == nil {
					general = []QueryPredicate{}
				}
				q.propertyPredicates[i] = properties
				q.propertySettings[i] = settings
				q.generalPredicates[i] = general
				continue
			}
		}
		if patternSource != "" {
			text, properties, settings, general := fallbackMetadataFromSource(patternSource, q.CaptureNames())
			if text == nil {
				text = []TextPredicateCapture{}
			}
			if properties == nil {
				properties = []PropertyPredicate{}
			}
			if settings == nil {
				settings = []QueryProperty{}
			}
			if general == nil {
				general = []QueryPredicate{}
			}
			q.TextPredicates[i] = text
			q.propertyPredicates[i] = properties
			q.propertySettings[i] = settings
			q.generalPredicates[i] = general
		} else {
			q.TextPredicates[i] = q.publicTextPredicatesFromPrivate(q.predicates)
			if q.TextPredicates[i] == nil {
				q.TextPredicates[i] = []TextPredicateCapture{}
			}
		}
	}
}

// validateNativePredicates performs the predicate checks that the upstream Go
// binding layers on top of ts_query_new.  Tree-sitter's C compiler accepts
// user-defined predicates (and leaves their evaluation to the host), so only
// the documented built-ins are checked here.  A module without the optional
// predicate-step ABI is left untouched for compatibility with older bridge
// versions.
func (q *Query) validateNativePredicates() error {
	if q == nil || !q.native {
		return nil
	}
	patternCount := q.PatternCount()
	for pattern := uint32(0); pattern < patternCount; pattern++ {
		steps, err := q.PredicateSteps(pattern)
		if err != nil {
			if isUnsupported(err) {
				return nil
			}
			return err
		}
		start := 0
		for i := 0; i <= len(steps); i++ {
			if i < len(steps) && steps[i].Type != QueryPredicateStepDone {
				continue
			}
			part := steps[start:i]
			start = i + 1
			if len(part) == 0 {
				continue
			}
			if part[0].Type != QueryPredicateStepString {
				return q.predicateValidationError(pattern, fmt.Sprintf(
					"Expected predicate to start with a function name. Got %s.",
					q.describePredicateStep(part[0]),
				))
			}
			op := q.StringValue(part[0].ValueID)
			switch op {
			case "eq?", "not-eq?", "any-eq?", "any-not-eq?":
				if len(part) != 3 {
					// The upstream Go binding keeps this diagnostic's historical
					// `#eq?` spelling even for the related not-/any- operators.
					// Preserve it so callers comparing native and WASM errors see
					// the same text.
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"Wrong number of arguments to #eq? predicate. Expected 2, got %d.",
						len(part)-1,
					))
				}
				if part[1].Type != QueryPredicateStepCapture {
					literal := q.predicateLiteral(part[1])
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"First argument to #eq? predicate must be a capture name. Got literal %s.",
						literal,
					))
				}
				if part[2].Type != QueryPredicateStepCapture && part[2].Type != QueryPredicateStepString {
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"Second argument to #eq? predicate must be a capture name or a literal. Got %s.",
						q.describePredicateStep(part[2]),
					))
				}

			case "match?", "not-match?", "any-match?", "any-not-match?":
				if len(part) != 3 {
					// See the note above: this is intentionally the historical
					// #match? label for all four match predicate spellings.
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"Wrong number of arguments to #match? predicate. Expected 2, got %d.",
						len(part)-1,
					))
				}
				if part[1].Type != QueryPredicateStepCapture {
					literal := q.predicateLiteral(part[1])
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"First argument to #match? predicate must be a capture name. Got literal %s.",
						literal,
					))
				}
				if part[2].Type != QueryPredicateStepString {
					got := q.describePredicateStep(part[2])
					if part[2].Type == QueryPredicateStepCapture {
						got = "capture " + got
					}
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"Second argument to #match? predicate must be a literal. Got %s.",
						got,
					))
				}
				patternText := q.StringValue(part[2].ValueID)
				if _, regexErr := regexp.Compile(patternText); regexErr != nil {
					return q.predicateValidationError(pattern, fmt.Sprintf("Invalid regex: '%s'", patternText))
				}

			case "any-of?", "not-any-of?":
				// The upstream binding accepts an empty literal list (the C
				// representation still has the operator and capture steps),
				// although normal query authoring uses at least one value. Keep
				// that compatibility while rejecting malformed argument kinds.
				if len(part) < 2 {
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"Wrong number of arguments to #any-of? predicate. Expected at least 1, got %d.",
						len(part)-1,
					))
				}
				if part[1].Type != QueryPredicateStepCapture {
					literal := q.predicateLiteral(part[1])
					return q.predicateValidationError(pattern, fmt.Sprintf(
						"First argument to #any-of? predicate must be a capture name. Got literal %s.",
						literal,
					))
				}
				for _, arg := range part[2:] {
					if arg.Type != QueryPredicateStepString {
						got := q.describePredicateStep(arg)
						if arg.Type == QueryPredicateStepCapture {
							got = "capture " + got
						}
						return q.predicateValidationError(pattern, fmt.Sprintf(
							"Arguments to #any-of? predicate must be literals. Got %s.",
							got,
						))
					}
				}

			case "set!", "is?", "is-not?":
				if len(part) < 2 || len(part) > 4 {
					return q.predicateValidationError(pattern, fmt.Sprintf(
						// parseProperty in the upstream Go binding receives the
						// operator name without a leading '#', unlike the text
						// predicate diagnostics above which historically spell
						// `#eq?`/`#match?`. Keep this distinction for exact error
						// parity (notably `set!`).
						"Wrong number of arguments to %s predicate. Expected 1 to 3, got %d.",
						op, len(part)-1,
					))
				}
				if message, ok := q.propertyValidationMessage(op, part[1:]); !ok {
					return q.predicateValidationError(pattern, message)
				}
			}
		}
	}
	return nil
}

func (q *Query) predicateValidationError(pattern uint32, message string) error {
	// Match upstream go-tree-sitter's predicateError convention. Predicate
	// diagnostics are associated with the pattern's source row, but the native
	// binding deliberately leaves Offset and Column at zero (the predicate
	// parser only reports a pattern index). Keeping that shape matters to
	// callers that compare *QueryError values across native and WASM builds.
	offset := q.StartByteForPattern(pattern)
	row, _ := sourcePosition(q.source, offset)
	return &QueryError{Message: message, Row: row, Column: 0, Offset: 0, Kind: QueryErrorPredicate}
}

func (q *Query) describePredicateStep(step QueryPredicateStep) string {
	switch step.Type {
	case QueryPredicateStepCapture:
		return "@" + q.CaptureName(step.ValueID)
	case QueryPredicateStepString:
		return q.StringValue(step.ValueID)
	default:
		return "<unknown>"
	}
}

// predicateLiteral formats the value used in the native binding's
// "Got literal ..." diagnostics.  Predicate-step values are indexes into the
// guest query tables, so going through StringValue/CaptureName also keeps a
// malformed custom bridge from making validation panic.
func (q *Query) predicateLiteral(step QueryPredicateStep) string {
	if step.Type == QueryPredicateStepString {
		return q.StringValue(step.ValueID)
	}
	return q.describePredicateStep(step)
}

// propertyValidationMessage reports the specific diagnostics used by the
// upstream Go binding for duplicate captures, an omitted key, and a fourth
// argument. The bool result is true only when the argument list is valid (the
// actual parsed property is produced separately below).
func (q *Query) propertyValidationMessage(operator string, steps []QueryPredicateStep) (string, bool) {
	if len(steps) == 0 || len(steps) > 3 {
		return fmt.Sprintf("Wrong number of arguments to %s predicate. Expected 1 to 3, got %d.", operator, len(steps)), false
	}
	var captureSeen bool
	var keySeen, valueSeen bool
	for _, step := range steps {
		switch step.Type {
		case QueryPredicateStepCapture:
			if captureSeen {
				return fmt.Sprintf(
					"Invalid arguments to %s predicate. Unexpected second capture name @%s",
					operator, q.CaptureName(step.ValueID),
				), false
			}
			captureSeen = true
		case QueryPredicateStepString:
			if !keySeen {
				keySeen = true
			} else if !valueSeen {
				valueSeen = true
			} else {
				// This mirrors parseProperty's historical wording. Although the
				// native implementation prefixes the token with '@' even when
				// the fourth argument is a literal, retaining that quirk is
				// preferable to producing a different cross-binding diagnostic.
				return fmt.Sprintf(
					"Invalid arguments to %s predicate. Unexpected third argument @%s",
					operator, q.StringValue(step.ValueID),
				), false
			}
		default:
			return fmt.Sprintf("Invalid arguments to %s predicate.", operator), false
		}
	}
	if !keySeen {
		return fmt.Sprintf("Invalid arguments to %s predicate. Missing key argument", operator), false
	}
	return "", true
}

func (q *Query) publicTextPredicatesFromPrivate(predicates []textPredicate) []TextPredicateCapture {
	result := make([]TextPredicateCapture, 0, len(predicates))
	for _, p := range predicates {
		captureID, ok := q.CaptureIndexForName32(p.capture)
		if !ok {
			continue
		}
		positive, matchAll := predicateSemantics(p.op)
		entry := TextPredicateCapture{CaptureID: captureID, Positive: positive, MatchAllNodes: matchAll}
		switch p.op {
		case "eq?", "not-eq?", "any-eq?", "any-not-eq?":
			if p.otherCapture != "" {
				id, found := q.CaptureIndexForName32(p.otherCapture)
				if !found {
					continue
				}
				// go-tree-sitter exposes capture references in the predicate
				// value as uint. Keep that public
				// shape even though the wire ABI itself is uint32-wide.
				entry.Type, entry.Value = TextPredicateTypeEqCapture, uint(id)
			} else {
				entry.Type, entry.Value = TextPredicateTypeEqString, p.value
			}
		case "match?", "not-match?", "any-match?", "any-not-match?":
			entry.Type = TextPredicateTypeMatchString
			if p.regex != nil {
				entry.Value = p.regex
			} else {
				entry.Value = p.value
			}
		case "any-of?", "not-any-of?":
			entry.Type, entry.Value = TextPredicateTypeAnyString, append([]string(nil), p.values...)
		default:
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (q *Query) publicPredicatesFromSteps(steps []QueryPredicateStep) (text []TextPredicateCapture, properties []PropertyPredicate, settings []QueryProperty, general []QueryPredicate) {
	if len(steps) == 0 {
		return nil, nil, nil, nil
	}
	for start := 0; start < len(steps); {
		end := start
		for end < len(steps) && steps[end].Type != QueryPredicateStepDone {
			end++
		}
		part := steps[start:end]
		start = end + 1
		if len(part) == 0 || part[0].Type != QueryPredicateStepString {
			continue
		}
		rawOperator := q.StringValue(part[0].ValueID)
		op := rawOperator
		if op == "" {
			continue
		}
		argString := func(step QueryPredicateStep) (string, bool) {
			switch step.Type {
			case QueryPredicateStepString:
				return q.StringValue(step.ValueID), true
			case QueryPredicateStepCapture:
				return q.CaptureName(step.ValueID), false
			default:
				return "", false
			}
		}
		if op == "eq?" || op == "not-eq?" || op == "any-eq?" || op == "any-not-eq?" || op == "match?" || op == "not-match?" || op == "any-match?" || op == "any-not-match?" || op == "any-of?" || op == "not-any-of?" {
			// any-of?/not-any-of? accept an empty literal list.  The C
			// representation then contains only the operator and capture
			// steps (plus the Done sentinel, which is removed above), so the
			// minimum arity is two for those operators and three otherwise.
			minPartLen := 3
			if op == "any-of?" || op == "not-any-of?" {
				minPartLen = 2
			}
			if len(part) < minPartLen || part[1].Type != QueryPredicateStepCapture {
				continue
			}
			positive, matchAll := predicateSemantics(op)
			entry := TextPredicateCapture{CaptureID: part[1].ValueID, Positive: positive, MatchAllNodes: matchAll}
			switch op {
			case "eq?", "not-eq?", "any-eq?", "any-not-eq?":
				if len(part) != 3 {
					continue
				}
				if part[2].Type == QueryPredicateStepCapture {
					// The upstream binding uses uint for a capture reference in
					// TextPredicateCapture.Value; the guest step remains uint32.
					entry.Type, entry.Value = TextPredicateTypeEqCapture, uint(part[2].ValueID)
				} else if value, ok := argString(part[2]); ok {
					entry.Type, entry.Value = TextPredicateTypeEqString, value
				} else {
					continue
				}
			case "match?", "not-match?", "any-match?", "any-not-match?":
				if len(part) != 3 || part[2].Type != QueryPredicateStepString {
					continue
				}
				value := q.StringValue(part[2].ValueID)
				re, err := regexp.Compile(value)
				if err != nil {
					continue
				}
				entry.Type, entry.Value = TextPredicateTypeMatchString, re
			case "any-of?", "not-any-of?":
				entry.Type = TextPredicateTypeAnyString
				values := make([]string, 0, len(part)-2)
				for _, step := range part[2:] {
					if step.Type != QueryPredicateStepString {
						values = nil
						break
					}
					values = append(values, q.StringValue(step.ValueID))
				}
				// An operator followed only by its capture is accepted by the
				// upstream query compiler.  Preserve the empty literal list so
				// evaluation still rejects every present node for any-of? (and
				// accepts none for not-any-of?), rather than silently dropping the
				// predicate.
				entry.Value = values
			}
			text = append(text, entry)
			continue
		}
		if op == "set!" || op == "is?" || op == "is-not?" {
			property, ok := q.publicPropertyFromSteps(part[1:])
			if !ok {
				continue
			}
			if op == "set!" {
				settings = append(settings, property)
			} else {
				properties = append(properties, PropertyPredicate{Property: property, Positive: op == "is?"})
			}
			continue
		}
		args := make([]QueryPredicateArg, 0, len(part)-1)
		for _, step := range part[1:] {
			switch step.Type {
			case QueryPredicateStepCapture:
				id := step.ValueID
				args = append(args, QueryPredicateArg{CaptureID: &id})
			case QueryPredicateStepString:
				value := q.StringValue(step.ValueID)
				args = append(args, QueryPredicateArg{String: &value})
			}
		}
		general = append(general, QueryPredicate{Operator: rawOperator, Args: args})
	}
	return text, properties, settings, general
}

func (q *Query) publicPropertyFromSteps(steps []QueryPredicateStep) (QueryProperty, bool) {
	if len(steps) == 0 || len(steps) > 3 {
		return QueryProperty{}, false
	}
	var property QueryProperty
	keySet := false
	for _, step := range steps {
		switch step.Type {
		case QueryPredicateStepCapture:
			if property.CaptureID != nil {
				return QueryProperty{}, false
			}
			id := step.ValueID
			property.CaptureID = &id
		case QueryPredicateStepString:
			value := q.StringValue(step.ValueID)
			if !keySet {
				property.Key = value
				keySet = true
			} else if property.Value == nil {
				property.Value = &value
			} else {
				return QueryProperty{}, false
			}
		default:
			return QueryProperty{}, false
		}
	}
	return property, keySet
}

// QueryMatch is one query pattern match.
type QueryMatch struct {
	PatternIndex uint32
	Captures     []QueryCapture
	ID           uint32
	// cursor is set for matches yielded by QueryCursor. Matches returned by
	// Query.Matches are detached after their temporary cursor closes.
	cursor *QueryCursor
	// root is the execution subtree associated with this match. It lets the
	// slice-shaped iterator apply Tree-sitter's range-intersection semantics to
	// patterns whose root is not itself captured.
	root Node
	// anchor is the node at which the compatibility matcher started this
	// pattern.  Native Tree-sitter applies max-start-depth and range pruning to
	// a pattern's root, not to whichever child happens to be captured.  The
	// fallback matcher cannot retain the full structural pattern, but preserving
	// this anchor avoids a particularly surprising divergence for queries such
	// as `(array (number) @n)` where the capture is one level below the root.
	anchor Node
	// predicates is populated only by the compatibility matcher. A non-nil
	// slice (including an empty one) means the match has an explicit per-pattern
	// predicate set and must not fall back to Query.predicates, which is retained
	// solely for legacy callers and metadata compatibility.
	predicates []textPredicate
	// stream links a materialized iterator value to its private replay history.
	// It is intentionally unexported so the public match shape remains stable.
	stream *queryIteratorStream
}

// QueryCapture is a node captured by a query.
type QueryCapture struct {
	Node  Node
	Index uint32
	// The following fields are iterator bookkeeping. They are intentionally
	// unexported so the public shape remains the two-field C-compatible record,
	// while QueryCaptures.Next can reconstruct the originating match.
	match        *QueryMatch
	matchID      uint32
	patternIndex uint32
	ordinal      uint32
	stream       *queryIteratorStream
}

// NodesForCaptureIndex returns all nodes in this match having captureIndex.
func (m QueryMatch) NodesForCaptureIndex(captureIndex uint) []Node {
	result := make([]Node, 0)
	for _, capture := range m.Captures {
		if uint(capture.Index) == captureIndex {
			result = append(result, capture.Node)
		}
	}
	return result
}

// NodesForCaptureIndex32 is the uint32 counterpart of NodesForCaptureIndex.
func (m QueryMatch) NodesForCaptureIndex32(captureIndex uint32) []Node {
	return m.NodesForCaptureIndex(uint(captureIndex))
}

// Remove removes this match from its originating cursor. It is a no-op for a
// detached match (for example one returned by Query.Matches), matching the
// upstream method's no-return API. RemoveE exposes failures when callers need
// to distinguish a closed or unsupported cursor.
func (m QueryMatch) Remove() { _ = m.RemoveE() }

// RemoveE is the error-returning form of Remove.
func (m QueryMatch) RemoveE() error {
	if m.cursor == nil {
		return nil
	}
	return m.cursor.RemoveMatchE(m.ID)
}

// SatisfiesTextPredicate evaluates the built-in text predicates attached to
// this match. buffer1 and buffer2 are accepted for source compatibility with
// upstream bindings; this implementation stores source text in the owning
// tree and therefore does not need the split buffers.
func (m QueryMatch) SatisfiesTextPredicate(query *Query, buffer1, buffer2, text []byte) bool {
	if query == nil {
		return false
	}
	if len(text) != 0 {
		// The normal evaluator reads Node.Text through the tree's retained source.
		// For callers supplying an explicit text buffer, use a temporary copy of
		// each node's text where possible so behavior remains useful on trees
		// created from a reader/callback.
		return query.satisfiesWithText(m, text)
	}
	return query.satisfies(m)
}

// Matches returns all matches below root using the tree's retained source.
// It returns nil on execution failure. Use [QueryCursor.Matches] when errors
// must be distinguished from an empty result. Returned nodes belong to root's
// tree and must not be used after that tree is closed.
func (q *Query) Matches(root Node) []QueryMatch {
	if q == nil || q.closed.Load() || root.IsNull() {
		return nil
	}
	if q.native {
		cursor := NewQueryCursor()
		defer func() { _ = cursor.Close() }()
		execErr := cursor.Exec(q, root)
		if execErr == nil {
			matches := make([]QueryMatch, 0)
			for {
				match, ok := cursor.NextMatch()
				if !ok {
					break
				}
				// Query.Matches returns detached value matches. Do not leave a
				// pointer to the temporary cursor that is closed below; callers may
				// safely invoke QueryMatch.Remove/RemoveE on the returned values.
				match.cursor = nil
				matches = append(matches, match)
			}
			return matches
		}
		// A native execution failure (for example a canceled/closed runtime or
		// malformed guest ABI) must not be silently converted into an empty
		// compatibility result.  Only an explicitly unsupported cursor surface
		// is eligible for the legacy matcher below.
		if !isUnsupported(execErr) {
			return nil
		}
		// Exec may fail on a legacy/partial bridge. Release the cursor before
		// falling back to the compatibility matcher; otherwise only the finalizer
		// would reclaim its native handle.
		_ = cursor.Close()
	}
	matches := q.matchesFallback(root)
	if len(q.predicates) == 0 {
		return matches
	}
	// Legacy bridge modules do not expose predicate metadata or a native
	// cursor. Apply the source-level built-in predicates to the materialized
	// fallback matches so they retain the same filtering behavior as native
	// queries. (Unknown predicates remain host-defined and are ignored.)
	filtered := make([]QueryMatch, 0, len(matches))
	for _, match := range matches {
		if q.satisfies(match) {
			filtered = append(filtered, match)
		}
	}
	return filtered
}

func (q *Query) matchesFallback(root Node) []QueryMatch {
	q.mu.RLock()
	patterns := append([]fallbackPattern(nil), q.fallback...)
	names := append([]string(nil), q.fallbackNames...)
	source := q.source
	native := q.native
	disabledCaptures := make(map[string]struct{}, len(q.disabledCaptures))
	for name := range q.disabledCaptures {
		disabledCaptures[name] = struct{}{}
	}
	disabledPatterns := make(map[uint32]struct{}, len(q.disabledPatterns))
	for pattern := range q.disabledPatterns {
		disabledPatterns[pattern] = struct{}{}
	}
	q.mu.RUnlock()
	// A partially exported bridge may provide the native query compiler but
	// omit the cursor ABI. In that case NewQuery quite correctly retains the
	// native handle, yet there is no way to execute it through WASM. Reuse the
	// compatibility matcher as a useful last resort for its supported subset
	// instead of silently returning an empty result. Keep this parsing local to
	// the fallback path so complete native modules pay no cost and continue to
	// get exact Tree-sitter semantics.
	if native && len(patterns) == 0 && strings.TrimSpace(source) != "" {
		if parsed, err := parseFallbackPatterns(source); err == nil {
			patterns = parsed
			if len(names) == 0 {
				names = fallbackCaptureNames(parsed)
			}
		}
	}
	nameIDs := make(map[string]uint32, len(names))
	for i, name := range names {
		nameIDs[name] = uint32(i)
	}
	result := make([]QueryMatch, 0)
	var visit func(Node)
	visit = func(node Node) {
		if node.IsNull() {
			return
		}
		for _, pattern := range patterns {
			patternIndex := pattern.patternIndex
			if _, disabled := disabledPatterns[patternIndex]; disabled {
				continue
			}
			// The legacy matcher cannot reproduce arbitrary structural
			// constraints, but it can still honor Tree-sitter's two wildcard
			// node forms: `*` matches named and anonymous nodes, while `_`
			// matches named nodes only. Treating `_` as a literal node type
			// silently drops a very common query when a bridge lacks the native
			// query ABI.
			if pattern.nodeType == "" {
				continue
			}
			matchesNode := pattern.nodeType == node.Type()
			if !pattern.literal && (pattern.nodeType == "*" || (pattern.nodeType == "_" && node.IsNamed())) {
				matchesNode = true
			}
			if !matchesNode {
				continue
			}
			var captures []QueryCapture
			captureNames := pattern.captures
			if len(captureNames) == 0 && pattern.capture != "" {
				captureNames = []string{pattern.capture}
			}
			for _, captureName := range captureNames {
				if captureName == "" {
					continue
				}
				if _, disabled := disabledCaptures[captureName]; disabled {
					continue
				}
				if id, ok := nameIDs[captureName]; ok {
					captures = append(captures, QueryCapture{Node: node, Index: id})
				}
			}
			// Tree-sitter assigns match ids from a zero-based sequence for each
			// cursor execution.  The compatibility matcher does not have a native
			// cursor state to provide those ids, so assign them while materializing
			// the unfiltered candidate stream.  Assigning before range/predicate
			// filtering preserves the gaps native callers observe when an earlier
			// match is rejected by a host-side predicate.
			// Preserve an explicit empty predicate list for patterns that have no
			// built-in predicates. A nil slice means "metadata unavailable" to
			// predicatesForMatch and therefore falls back to q.predicates (the
			// source-wide legacy scanner). Without this distinction, a query with
			// one filtered pattern followed by an unfiltered pattern could apply
			// the first pattern's predicate to both and incorrectly reject the
			// latter. Native Tree-sitter keeps predicate metadata scoped per
			// pattern, including an empty vector.
			patternPredicates := pattern.predicates
			if patternPredicates == nil {
				patternPredicates = []textPredicate{}
			}
			result = append(result, QueryMatch{
				PatternIndex: patternIndex,
				Captures:     captures,
				ID:           uint32(len(result)),
				anchor:       node,
				predicates:   append([]textPredicate{}, patternPredicates...),
			})
		}
		for i := 0; i < node.ChildCount(); i++ {
			visit(node.Child(i))
		}
	}
	visit(root)
	return result
}

// Close releases the guest query handle. It is safe to call Close more than
// once; subsequent calls return nil.
func (q *Query) Close() error {
	if q == nil || q.closed.Swap(true) {
		return nil
	}
	runtimepkg.SetFinalizer(q, nil)
	// Pair the atomic closed transition with the handle swap. Native callers
	// hold q.mu.RLock while invoking the guest, so deletion cannot race a call
	// that has already obtained the handle.
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.native && q.runtime != nil && q.handle.Load() != 0 && !q.runtime.closedState() {
		handle := q.handle.Swap(0)
		// Cleanup remains best-effort even when the runtime's caller context has
		// been canceled.  A canceled context can make wazero skip the guest call
		// entirely, leaving the query allocation live in an otherwise usable
		// module.
		_, _, err := q.runtime.call(context.Background(), []string{"tsw_query_delete", "wasitter_query_delete", "ts_query_delete", "query_delete"}, uint64(handle))
		if errors.Is(err, ErrClosed) {
			return nil
		}
		return err
	}
	q.handle.Store(0)
	return nil
}

func (q *Query) compileNative(source string) (uint32, error, bool) {
	r := q.runtime
	if uint64(len(source)) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("wasitter: query source exceeds uint32 wasm offset"), true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, fnName, err := r.function("tsw_query_new", "wasitter_query_new", "ts_query_new", "query_new")
	if err != nil {
		if isUnsupported(err) {
			return 0, nil, false
		}
		return 0, err, true
	}
	// The stable shim accepts (language, source_ptr, source_len, error_ptr),
	// while early prototypes exposed the same operation without the optional
	// error-output pointer.  Inspect the signature before entering the guest so
	// a partial/custom module yields a regular ABI diagnostic instead of a
	// WebAssembly arity trap.  Both forms use wasm integer values only.
	paramTypes := fn.Definition().ParamTypes()
	if len(paramTypes) != 3 && len(paramTypes) != 4 {
		return 0, &ABIError{Function: fnName, Message: "unsupported query constructor signature"}, true
	}
	for _, typ := range paramTypes {
		if typ != api.ValueTypeI32 && typ != api.ValueTypeI64 {
			return 0, &ABIError{Function: fnName, Message: "query constructor has non-integer parameters"}, true
		}
	}
	resultTypes := fn.Definition().ResultTypes()
	if len(resultTypes) != 1 || (resultTypes[0] != api.ValueTypeI32 && resultTypes[0] != api.ValueTypeI64) {
		return 0, &ABIError{Function: fnName, Message: "query constructor has unsupported result signature"}, true
	}
	ptr, err := r.allocLocked(uint32(len(source)))
	if err != nil {
		return 0, err, true
	}
	defer r.freeLocked(ptr)
	if mem := r.mod.Memory(); mem == nil || !mem.Write(ptr, []byte(source)) {
		return 0, fmt.Errorf("wasitter: cannot write query source"), true
	}
	var errorPtr uint32
	if len(paramTypes) == 4 {
		errorPtr, err = r.allocLocked(8)
		if err != nil {
			return 0, err, true
		}
		defer r.freeLocked(errorPtr)
	}
	args := []uint64{uint64(q.language.handle), uint64(ptr), uint64(len(source))}
	if len(paramTypes) == 4 {
		args = append(args, uint64(errorPtr))
	}
	result, callErr := fn.Call(r.Context(), args...)
	if callErr != nil {
		return 0, &ABIError{Function: fnName, Message: callErr.Error()}, true
	}
	var offset, rawKind uint32
	if errorPtr != 0 {
		if mem := r.mod.Memory(); mem != nil {
			if b, ok := mem.Read(errorPtr, 8); ok {
				offset = binary.LittleEndian.Uint32(b[:4])
				rawKind = binary.LittleEndian.Uint32(b[4:8])
			}
		}
	}
	if len(result) == 0 {
		return 0, newQueryError(source, offset, rawKind, r.lastErrorLocked()), true
	}
	handle, handleOK := checkedU32(result[0])
	if !handleOK {
		return 0, &ABIError{Function: fnName, Message: "query handle is not a wasm32 value"}, true
	}
	if handle == 0 {
		return 0, newQueryError(source, offset, rawKind, r.lastErrorLocked()), true
	}
	return handle, nil, true
}

func newQueryError(source string, offset, rawKind uint32, guestMessage string) error {
	row, column := sourcePosition(source, offset)
	kind := queryErrorKindFromRaw(rawKind)
	message := queryErrorDetail(source, offset, kind, guestMessage)
	return &QueryError{Message: message, Offset: offset, Row: row, Column: column, Kind: kind}
}

// queryErrorDetail reconstructs the source-oriented diagnostics emitted by
// the native Go binding.  The C API intentionally reports only an error kind
// and byte offset, while our small ABI also exposes a short generic message;
// deriving the token/caret here gives callers actionable errors without
// requiring a second parser in the guest.
func queryErrorDetail(source string, offset uint32, kind QueryErrorKind, guestMessage string) string {
	// Keep the raw offset on QueryError, but clamp only the slice used for
	// formatting.  A malformed/custom guest must not make the host panic by
	// returning an offset beyond the supplied source.
	off := uint64(offset)
	if off > uint64(len(source)) {
		off = uint64(len(source))
	}
	position := int(off)

	switch kind {
	case QueryErrorNodeType, QueryErrorField, QueryErrorCapture:
		// Name errors point at either an unquoted identifier or the first byte
		// after an opening quote.  Match Tree-sitter's token scanner: quoted
		// names may contain arbitrary bytes and escaped quotes, whereas
		// unquoted names stop at the first character outside [A-Za-z0-9_-].
		suffix := source[position:]
		if len(suffix) != 0 {
			end := len(suffix)
			quoted := position > 0 && source[position-1] == '"'
			if quoted {
				backslashes := 0
				for i := 0; i < len(suffix); i++ {
					c := suffix[i]
					if c == '"' && backslashes%2 == 0 {
						end = i
						break
					}
					if c == '\\' {
						backslashes++
					} else {
						backslashes = 0
					}
				}
			} else {
				for i := 0; i < len(suffix); i++ {
					c := suffix[i]
					if !((c >= 'a' && c <= 'z') ||
						(c >= 'A' && c <= 'Z') ||
						(c >= '0' && c <= '9') || c == '_' || c == '-') {
						end = i
						break
					}
				}
			}
			if end > 0 {
				return suffix[:end]
			}
		}

	case QueryErrorSyntax, QueryErrorStructure:
		// Locate the line containing the byte offset.  Query offsets are UTF-8
		// byte offsets, so indexing the string (rather than ranging over runes)
		// keeps the caret column consistent with Tree-sitter.
		lineStart := strings.LastIndexByte(source[:position], '\n') + 1
		lineEnd := strings.IndexByte(source[position:], '\n')
		if lineEnd < 0 {
			lineEnd = len(source) - position
		}
		line := source[lineStart : position+lineEnd]
		if line != "" {
			return line + "\n" + strings.Repeat(" ", position-lineStart) + "^"
		}
		return "Unexpected EOF"
	}

	if guestMessage != "" {
		return guestMessage
	}
	switch kind {
	case QueryErrorNodeType:
		return "unknown node type"
	case QueryErrorField:
		return "unknown field name"
	case QueryErrorCapture:
		return "invalid capture name"
	case QueryErrorStructure:
		return "invalid query structure"
	case QueryErrorLanguage:
		return "query language mismatch"
	default:
		return "query syntax error"
	}
}

// queryErrorKindFromRaw translates the C TSQueryError numbering to the
// public go-tree-sitter numbering.  C reserves zero for TSQueryErrorNone,
// while the Go API starts QueryErrorSyntax at zero and inserts the
// host-validated QueryErrorPredicate before Structure/Language.
func queryErrorKindFromRaw(raw uint32) QueryErrorKind {
	switch raw {
	case 0: // TSQueryErrorNone; a failed compile with no detail is syntax-like.
		return QueryErrorSyntax
	case 1: // TSQueryErrorSyntax
		return QueryErrorSyntax
	case 2: // TSQueryErrorNodeType
		return QueryErrorNodeType
	case 3: // TSQueryErrorField
		return QueryErrorField
	case 4: // TSQueryErrorCapture
		return QueryErrorCapture
	case 5: // TSQueryErrorStructure
		return QueryErrorStructure
	case 6: // TSQueryErrorLanguage
		return QueryErrorLanguage
	default:
		// Unknown guest values should remain diagnosable without exposing a
		// value that could be mistaken for one of the documented kinds.
		return QueryErrorSyntax
	}
}

func sourcePosition(source string, offset uint32) (uint32, uint32) {
	if uint64(offset) > uint64(len(source)) {
		offset = uint32(len(source))
	}
	row, column := uint32(0), uint32(0)
	for i := uint32(0); i < offset; i++ {
		if source[i] == '\n' {
			row++
			column = 0
		} else {
			column++
		}
	}
	return row, column
}

func parseFallbackPatterns(source string) ([]fallbackPattern, error) {
	// Tree-sitter's query lexer accepts non-ASCII text only inside quoted
	// strings.  In particular, capture names, node identifiers, predicate
	// operators, and bare predicate values are scanned with the C-locale
	// `iswalnum` check and therefore reject high-bit code points.  Run this
	// inexpensive lexical guard before the compatibility parser so a legacy
	// bridge reports the same syntax error as the native compiler instead of
	// silently treating a malformed token as an unrooted pattern.  Quoted
	// literals and comments are skipped, preserving valid Unicode source/text
	// values such as `(string) @s (#eq? @s "猫")`.
	if offset := firstFallbackNonASCIIOutsideLiteral(source); offset >= 0 {
		return nil, fallbackSyntaxError(source, offset)
	}
	// An empty query is valid Tree-sitter syntax and produces a query with no
	// patterns.  Keep the same behavior when a legacy bridge lacks the native
	// query compiler instead of reporting a spurious syntax error.
	if strings.TrimSpace(source) == "" {
		return []fallbackPattern{}, nil
	}
	// The compatibility matcher extracts only a node type from each
	// expression. Validate the complete expression first so malformed suffixes
	// cannot be silently discarded when the native query ABI is unavailable.
	if err := validateFallbackStructure(source); err != nil {
		return nil, err
	}
	patterns, next, err := parseFallbackTopLevel(source, 0, len(source), false)
	if err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		return nil, &QueryError{Message: "expected a parenthesized node pattern", Kind: QueryErrorSyntax}
	}
	_ = next
	return patterns, nil
}

// firstFallbackNonASCIIOutsideLiteral returns the byte offset of the first
// high-bit byte outside a query string or semicolon comment. Query strings are
// byte-oriented UTF-8 payloads and may contain arbitrary Unicode; identifiers
// elsewhere must remain in Tree-sitter's ASCII-compatible C-locale subset.
func firstFallbackNonASCIIOutsideLiteral(source string) int {
	inString := false
	escaped := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ';':
			for i+1 < len(source) && source[i+1] != '\n' {
				i++
			}
		default:
			if c >= 0x80 {
				return i
			}
		}
	}
	return -1
}

// fallbackPatternCount returns the number of source-level top-level patterns
// represented by flattened compatibility branches. Pattern indices are
// assigned by parseFallbackTopLevelWithGroups; keep this helper defensive for
// callers that construct fallbackPattern values directly in package tests.
func fallbackPatternCount(patterns []fallbackPattern) uint32 {
	if len(patterns) == 0 {
		return 0
	}
	var max uint32
	for _, pattern := range patterns {
		if pattern.patternIndex > max {
			max = pattern.patternIndex
		}
	}
	// The zero value is a valid index, so max+1 is the count. Guard overflow
	// for malformed synthetic inputs rather than wrapping to zero.
	if max == ^uint32(0) {
		return ^uint32(0)
	}
	return max + 1
}

// validateFallbackNodeTypes performs the small amount of semantic validation
// that a legacy bridge cannot delegate to ts_query_new.  Without this check a
// typo such as `(does_not_exist) @x` would compile successfully on a module
// missing the optional query ABI, then simply produce no matches.  When the
// language symbol lookup itself is unavailable we retain the historical
// permissive fallback behavior; custom bridge modules are allowed to expose
// only the parser/tree subset.
func validateFallbackNodeTypes(language *Language, source string, patterns []fallbackPattern) error {
	if language == nil {
		return nil
	}
	refs := scanFallbackNodeTypeRefs(source)
	// Keep a defensive fallback for package-internal callers that construct a
	// compatibility pattern directly without retaining a source expression.
	if len(refs) == 0 {
		for _, pattern := range patterns {
			if pattern.nodeType == "" {
				continue
			}
			refs = append(refs, fallbackNodeTypeRef{
				name:    pattern.nodeType,
				literal: pattern.literal,
				offset:  pattern.nodeOffset,
			})
		}
	}
	for _, ref := range refs {
		if ref.name == "" || (!ref.literal && (ref.name == "*" || ref.name == "_")) {
			continue
		}
		id, err := language.SymbolForNameNamedE(ref.name, !ref.literal)
		if err != nil {
			if isUnsupported(err) {
				// A bridge without language symbol metadata cannot validate
				// names. Leave all remaining patterns untouched rather than
				// rejecting otherwise usable compatibility queries.
				return nil
			}
			return err
		}
		if id != 0 {
			continue
		}
		offset := ref.offset
		if offset < 0 || offset > len(source) {
			offset = 0
		}
		row, column := sourcePosition(source, uint32(offset))
		return &QueryError{
			Message: ref.name,
			Offset:  uint32(offset),
			Row:     row,
			Column:  column,
			Kind:    QueryErrorNodeType,
		}
	}
	return nil
}

type fallbackNodeTypeRef struct {
	name    string
	literal bool
	offset  int
}

// scanFallbackNodeTypeRefs finds named and anonymous node references at every
// nesting depth. Predicate S-expressions are skipped as a unit so their
// quoted string values are not mistaken for anonymous grammar tokens.
func scanFallbackNodeTypeRefs(source string) []fallbackNodeTypeRef {
	refs := make([]fallbackNodeTypeRef, 0)
	for i := 0; i < len(source); {
		switch source[i] {
		case ';':
			i = skipQueryComment(source, i)
			continue
		case '"':
			next := skipQueryQuoted(source, i)
			if next > i && next <= len(source) {
				if value, ok := decodeQueryString(source[i:next]); ok {
					refs = append(refs, fallbackNodeTypeRef{name: value, literal: true, offset: i})
				}
			}
			i = next
			continue
		case '(':
			start := skipFallbackSpace(source, i+1, len(source))
			if start >= len(source) {
				i++
				continue
			}
			if source[start] == '#' || source[start] == '.' {
				if _, after, ok := scanTextPredicateOperator(source, start); ok {
					if close, balanced := fallbackBalancedEnd(source, i, len(source), '(', ')'); balanced {
						i = close + 1
						continue
					}
					// The enclosing syntax validator will report the missing
					// delimiter. Stop here so predicate literals still cannot
					// leak into the node-name pass.
					i = len(source)
					_ = after
					continue
				}
			}
			if source[start] == '"' {
				next := skipQueryQuoted(source, start)
				if next > start && next <= len(source) {
					if value, ok := decodeQueryString(source[start:next]); ok {
						refs = append(refs, fallbackNodeTypeRef{name: value, literal: true, offset: start})
					}
				}
				i = next
				continue
			}
			if source[start] != '(' && source[start] != '[' {
				end := start
				for end < len(source) && isFallbackNodeToken(source[end]) {
					end++
				}
				if end > start {
					refs = append(refs, fallbackNodeTypeRef{name: source[start:end], offset: start})
				}
			}
		}
		i++
	}
	return refs
}

// validateFallbackFields checks field labels (`field: (node)`) when the
// bridge does not provide the native query compiler. Field ids in generated
// grammars start at one, so a zero result from the optional lookup denotes an
// unknown name. As with node validation, an unavailable accessor leaves the
// compatibility path permissive for custom/older modules.
func validateFallbackFields(language *Language, source string) error {
	if language == nil || source == "" {
		return nil
	}
	for i := 0; i < len(source); {
		switch source[i] {
		case '"':
			i = skipQueryQuoted(source, i)
			continue
		case ';':
			i = skipQueryComment(source, i)
			continue
		case '#', '.':
			// Skip a complete predicate expression while looking for
			// structural field labels. A colon in a malformed predicate
			// argument should be diagnosed by predicate validation, not
			// misreported as an unknown grammar field.
			if _, after, ok := scanTextPredicateOperator(source, i); ok {
				end := scanTextPredicateEnd(source, after)
				if end < len(source) {
					i = end + 1
				} else {
					i = end
				}
				continue
			}
		}
		if !isFallbackNodeToken(source[i]) {
			i++
			continue
		}
		start := i
		for i < len(source) && isFallbackNodeToken(source[i]) {
			i++
		}
		labelEnd := skipQueryWhitespace(source, i)
		if labelEnd >= len(source) || source[labelEnd] != ':' {
			continue
		}
		// A label immediately following a predicate operator is not a
		// structural field. Predicate arguments cannot contain ':' in valid
		// query syntax, but keeping this guard avoids turning malformed user
		// predicates into misleading field diagnostics.
		before := start - 1
		for before >= 0 && (source[before] == ' ' || source[before] == '\t' || source[before] == '\r' || source[before] == '\n') {
			before--
		}
		if before >= 0 && (source[before] == '#' || source[before] == '.') {
			continue
		}
		id, err := language.fieldIDForName(source[start:i])
		if err != nil {
			if isUnsupported(err) {
				return nil
			}
			return err
		}
		if id != 0 {
			continue
		}
		row, column := sourcePosition(source, uint32(start))
		return &QueryError{
			Message: source[start:i],
			Offset:  uint32(start),
			Row:     row,
			Column:  column,
			Kind:    QueryErrorField,
		}
	}
	return nil
}

func fallbackAllocatePatternGroup(counter *uint32, forcedGroup *uint32) uint32 {
	if forcedGroup != nil {
		return *forcedGroup
	}
	if counter == nil {
		return 0
	}
	group := *counter
	if *counter != ^uint32(0) {
		(*counter)++
	}
	return group
}

// parseFallbackTopLevel recognizes the small query subset available when a
// legacy bridge has no native query compiler. It deliberately parses balanced
// expressions instead of using a regexp: a regexp sees every nested child
// pattern as a separate top-level match, which makes a query such as
// `(pair key: (string) @k)` produce spurious patterns. The scanner understands
// quoted strings and Tree-sitter's semicolon comments so parentheses in either
// context do not terminate the enclosing expression.
func parseFallbackTopLevel(source string, begin, end int, inAlternation bool) ([]fallbackPattern, int, error) {
	counter := uint32(0)
	return parseFallbackTopLevelWithGroups(source, begin, end, inAlternation, &counter, nil)
}

// parseFallbackTopLevelWithGroups is the implementation behind
// parseFallbackTopLevel. forcedGroup is non-nil while parsing the interior of
// an alternatives expression; every branch then retains the enclosing
// Tree-sitter pattern index. At the root, each actual pattern expression
// consumes the next value from counter.
func parseFallbackTopLevelWithGroups(source string, begin, end int, inAlternation bool, counter *uint32, forcedGroup *uint32) ([]fallbackPattern, int, error) {
	patterns := make([]fallbackPattern, 0)
	for i := begin; i < end; {
		i = skipFallbackSpace(source, i, end)
		if i >= end {
			break
		}
		switch source[i] {
		case '(':
			close, ok := fallbackBalancedEnd(source, i, end, '(', ')')
			if !ok {
				return nil, i, fallbackSyntaxError(source, i)
			}
			pattern := fallbackPatternFromExpression(source, i, close)
			if pattern.nodeType != "" {
				pattern.patternIndex = fallbackAllocatePatternGroup(counter, forcedGroup)
				// Keep built-in predicates attached to the enclosing pattern. The
				// compatibility matcher intentionally does not parse arbitrary
				// structure, but it can still preserve the predicate scope that the
				// native query compiler applies when capture names are reused.
				pattern.predicates = parseTextPredicates(source[i : close+1])
				if pattern.predicates == nil {
					pattern.predicates = []textPredicate{}
				}
				// A capture written immediately after the closing parenthesis is
				// attached to this pattern (the common `(node) @capture` form).
				j := skipFallbackSpace(source, close+1, end)
				pendingQuantifier := CaptureQuantifierOne
				if j < end {
					if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
						pendingQuantifier = quantifier
						j = skipFallbackSpace(source, j+1, end)
					}
				}
				for j < end && source[j] == '@' {
					name, next := fallbackCaptureToken(source, j, end)
					if name == "" {
						break
					}
					pattern.captures = appendUniqueString(pattern.captures, name)
					if pattern.capture == "" {
						pattern.capture = name
					}
					captureQuantifier := pendingQuantifier
					j = skipFallbackSpace(source, next, end)
					if j < end {
						if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
							captureQuantifier = quantifier
							j = skipFallbackSpace(source, j+1, end)
						}
					}
					if pattern.quantifiers == nil {
						pattern.quantifiers = make(map[string]CaptureQuantifier)
					}
					pattern.quantifiers[name] = captureQuantifier
					pendingQuantifier = CaptureQuantifierOne
				}
				inferFallbackQuantifiers(source, i, j, &pattern)
				if j > close+1 {
					// Include captures and any occurrence modifier in the
					// source span exposed by EndByteForPattern.  Native
					// Tree-sitter reports the complete pattern extent, not just
					// the opening node expression.
					pattern.sourceEnd = j
				}
				patterns = append(patterns, pattern)
				if j > close+1 {
					i = j
					continue
				}
			} else if len(patterns) != 0 && fallbackIsPredicateExpression(source, i, close) {
				// Tree-sitter permits predicates as a sibling expression after
				// the captured pattern, e.g. `(number) @n (#eq? @n "1")`.
				// The compact fallback parser initially treated that sibling as an
				// empty node pattern and silently dropped it, causing every match
				// to pass the predicate. Associate consecutive top-level predicate
				// expressions with the immediately preceding pattern and extend its
				// source span so metadata decoding sees the same expression.
				predicateValues := parseTextPredicates(source[i : close+1])
				last := &patterns[len(patterns)-1]
				// A predicate following an alternatives expression belongs to the
				// enclosing pattern, not just its final flattened branch. Propagate
				// it to every branch sharing that group so host-side filtering stays
				// equivalent to the native query engine.
				group := last.patternIndex
				for index := range patterns {
					if patterns[index].patternIndex != group {
						continue
					}
					patterns[index].predicates = append(patterns[index].predicates, predicateValues...)
					patterns[index].sourceEnd = close + 1
				}
			}
			i = close + 1
		case '"':
			// Anonymous token patterns may appear at the top level without an
			// additional pair of parentheses (`"," @comma`).  Decode the
			// literal and consume any captures/quantifier attached to it.
			next := skipQueryQuoted(source, i)
			if next <= i || next > end {
				return nil, i, fallbackSyntaxError(source, i)
			}
			value, ok := decodeQueryString(source[i:next])
			if !ok {
				return nil, i, fallbackSyntaxError(source, i)
			}
			pattern := fallbackPattern{nodeType: value, literal: true, sourceStart: i, sourceEnd: next, nodeOffset: i}
			pattern.patternIndex = fallbackAllocatePatternGroup(counter, forcedGroup)
			j := skipFallbackSpace(source, next, end)
			// A quantifier may occur between an anonymous token and its capture
			// (`","? @comma`), just as it may occur between a parenthesized
			// pattern and its capture (`(number)+ @n`).  The old fallback branch
			// only looked for a modifier after the capture, which both reported
			// the wrong CaptureQuantifier and left a valid suffix to be rejected
			// as a stray top-level token.  Consume the pre-capture modifier and
			// carry it into the first capture below.
			pendingQuantifier := CaptureQuantifierOne
			if j < end {
				if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
					pendingQuantifier = quantifier
					j = skipFallbackSpace(source, j+1, end)
				}
			}
			for j < end && source[j] == '@' {
				name, after := fallbackCaptureToken(source, j, end)
				if name == "" {
					break
				}
				pattern.captures = appendUniqueString(pattern.captures, name)
				if pattern.capture == "" {
					pattern.capture = name
				}
				captureQuantifier := pendingQuantifier
				j = skipFallbackSpace(source, after, end)
				if j < end {
					if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
						captureQuantifier = quantifier
						j = skipFallbackSpace(source, j+1, end)
					}
				}
				if pattern.quantifiers == nil {
					pattern.quantifiers = make(map[string]CaptureQuantifier)
				}
				pattern.quantifiers[name] = captureQuantifier
				pendingQuantifier = CaptureQuantifierOne
			}
			if j < end && (source[j] == '?' || source[j] == '+' || source[j] == '*') {
				// A modifier without a directly attached capture applies to the
				// anonymous token itself. It is consumed for syntax compatibility;
				// any capture added by an enclosing alternatives suffix is assigned
				// below using the same modifier.
				j++
			}
			inferFallbackQuantifiers(source, i, j, &pattern)
			if j > next {
				pattern.sourceEnd = j
			}
			patterns = append(patterns, pattern)
			if j > next {
				i = j
			} else {
				i = next
			}
		case '[':
			close, ok := fallbackBalancedEnd(source, i, end, '[', ']')
			if !ok {
				return nil, i, fallbackSyntaxError(source, i)
			}
			// Alternatives contain independent top-level patterns. Parse the
			// interior recursively; a trailing capture on the bracket itself is
			// uncommon and is intentionally left to the first pattern only.
			group := fallbackAllocatePatternGroup(counter, forcedGroup)
			inner, _, innerErr := parseFallbackTopLevelWithGroups(source, i+1, close, true, counter, &group)
			if innerErr != nil {
				return nil, i, innerErr
			}
			// Captures written after an alternation apply to every branch (for
			// example `[(number) (string)] @value`). Propagate those annotations
			// to each compatibility pattern so CaptureNames and materialized
			// matches retain the same useful shape as the native query engine.
			j := skipFallbackSpace(source, close+1, end)
			pendingQuantifier := CaptureQuantifierOne
			if j < end {
				if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
					pendingQuantifier = quantifier
					j = skipFallbackSpace(source, j+1, end)
				}
			}
			var trailing []string
			for j < end && source[j] == '@' {
				name, next := fallbackCaptureToken(source, j, end)
				if name == "" {
					break
				}
				trailing = appendUniqueString(trailing, name)
				captureQuantifier := pendingQuantifier
				j = skipFallbackSpace(source, next, end)
				if j < end {
					if quantifier, ok := fallbackQuantifierAt(source[j]); ok {
						captureQuantifier = quantifier
						j = skipFallbackSpace(source, j+1, end)
					}
				}
				for index := range inner {
					if inner[index].quantifiers == nil {
						inner[index].quantifiers = make(map[string]CaptureQuantifier)
					}
					inner[index].quantifiers[name] = captureQuantifier
				}
				pendingQuantifier = CaptureQuantifierOne
			}
			if len(trailing) != 0 {
				for index := range inner {
					inner[index].patternIndex = group
					for _, name := range trailing {
						inner[index].captures = appendUniqueString(inner[index].captures, name)
						if inner[index].capture == "" {
							inner[index].capture = name
						}
					}
				}
			}
			for index := range inner {
				// Empty alternatives are rejected by the native compiler; for
				// defensive compatibility ensure any branch parser that did not
				// assign a group still points at the enclosing pattern.
				inner[index].patternIndex = group
				// Every flattened branch represents one source-level
				// alternatives pattern. Expose the enclosing bracket range so
				// byte-offset metadata and predicate scoping agree across all
				// branches.
				inner[index].sourceStart = i
				if j > close+1 {
					inner[index].sourceEnd = j
				} else {
					inner[index].sourceEnd = close + 1
				}
			}
			patterns = append(patterns, inner...)
			if j > close+1 {
				i = j
			} else {
				i = close + 1
			}
		case '?', '+', '*':
			// A top-level pattern may carry a quantifier (for example
			// `(number) @n+` or `[(number) (string)]*`).  The compact
			// compatibility matcher cannot reproduce repetition semantics, but
			// it can still match the underlying node pattern. Consume the
			// quantifier explicitly so strict validation below does not reject a
			// syntactically valid query.
			i++
		default:
			// Tree-sitter query sources consist of parenthesized patterns (or
			// bracketed alternatives), whitespace, comments, and optional
			// quantifiers. Silently skipping an arbitrary token here used to make
			// malformed legacy queries appear valid and, worse, could discover a
			// later pattern after an invalid prefix. Report the first unexpected
			// byte with the same source-oriented diagnostic used for unbalanced
			// expressions. `inAlternation` is retained for the recursive scanner's
			// signature; the closing bracket is outside the recursive `end` bound
			// and therefore never reaches this branch.
			return nil, i, fallbackSyntaxError(source, i)
		}
	}
	return patterns, end, nil
}

func skipFallbackSpace(source string, i, end int) int {
	for i < end {
		switch source[i] {
		case ' ', '\t', '\r', '\n', ',':
			i++
		case ';':
			for i < end && source[i] != '\n' {
				i++
			}
		default:
			return i
		}
	}
	return i
}

func fallbackQuantifierAt(value byte) (CaptureQuantifier, bool) {
	switch value {
	case '?':
		return CaptureQuantifierZeroOrOne, true
	case '*':
		return CaptureQuantifierZeroOrMore, true
	case '+':
		return CaptureQuantifierOneOrMore, true
	default:
		return CaptureQuantifierZero, false
	}
}

// inferFallbackQuantifiers recovers occurrence modifiers for captures found
// inside a simple compatibility pattern. It intentionally stops at predicate
// expressions so references such as `(#eq? @name "x")` are not mistaken for
// additional pattern annotations. The fallback matcher still emits one node
// per branch (it does not implement the full repetition state machine), but
// metadata consumers can rely on the same quantifier values as native queries.
func inferFallbackQuantifiers(source string, begin, end int, pattern *fallbackPattern) {
	if pattern == nil || len(pattern.captures) == 0 {
		return
	}
	if begin < 0 {
		begin = 0
	}
	if end > len(source) {
		end = len(source)
	}
	if begin >= end {
		return
	}
	declared := make(map[string]struct{}, len(pattern.captures))
	for _, name := range pattern.captures {
		if name != "" {
			declared[name] = struct{}{}
		}
	}
	if len(declared) == 0 {
		return
	}
	if pattern.quantifiers == nil {
		pattern.quantifiers = make(map[string]CaptureQuantifier)
	}
	for i := begin; i < end; {
		switch source[i] {
		case '"':
			next := skipQueryQuoted(source, i)
			if next <= i {
				return
			}
			i = next
			continue
		case ';':
			i = skipQueryComment(source, i)
			continue
		case '#':
			// A hash always starts a predicate in the subset understood by the
			// fallback parser; do not scan its capture arguments as annotations.
			return
		case '.':
			if _, _, isPredicate := scanTextPredicateOperator(source, i); isPredicate {
				return
			}
		}
		if source[i] != '@' {
			i++
			continue
		}
		name, next := fallbackCaptureToken(source, i, end)
		if name == "" {
			i++
			continue
		}
		if _, ok := declared[name]; !ok {
			i = next
			continue
		}
		quantifier := CaptureQuantifierOne
		before := i - 1
		for before >= begin && (source[before] == ' ' || source[before] == '\t' || source[before] == '\r' || source[before] == '\n' || source[before] == ',') {
			before--
		}
		if before >= begin {
			if value, ok := fallbackQuantifierAt(source[before]); ok {
				quantifier = value
			}
		}
		after := skipFallbackSpace(source, next, end)
		if after < end {
			if value, ok := fallbackQuantifierAt(source[after]); ok {
				quantifier = value
			}
		}
		if existing, exists := pattern.quantifiers[name]; !exists || quantifier != CaptureQuantifierOne {
			pattern.quantifiers[name] = quantifier
		} else if existing == CaptureQuantifierZero {
			pattern.quantifiers[name] = quantifier
		}
		i = next
	}
}

func fallbackBalancedEnd(source string, open, end int, left, right byte) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := open; i < end; i++ {
		c := source[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c == ';' {
			for i < end && source[i] != '\n' {
				i++
			}
			if i >= end {
				break
			}
			c = source[i]
		}
		if c == left {
			depth++
		} else if c == right {
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return end, false
}

func fallbackPatternFromExpression(source string, open, close int) fallbackPattern {
	pattern := fallbackPattern{sourceStart: open, sourceEnd: close + 1}
	i := skipFallbackSpace(source, open+1, close)
	if i >= close {
		return pattern
	}
	// Nested grouping is used for predicates and parenthesized patterns. Walk
	// to the first actual node token rather than treating the opening grouping
	// delimiter as the node type.
	if source[i] == '(' {
		if nestedClose, ok := fallbackBalancedEnd(source, i, close, '(', ')'); ok {
			pattern = fallbackPatternFromExpression(source, i, nestedClose)
		}
	} else if source[i] == '"' {
		// Anonymous grammar symbols are written as quoted literals (for
		// example, `(",") @comma`).  Their Node.Type value is the decoded
		// literal without quotes, so retain that value for the compatibility
		// matcher instead of treating the expression as an empty pattern.
		next := skipQueryQuoted(source, i)
		if next <= close && next > i {
			if value, ok := decodeQueryString(source[i:next]); ok {
				pattern.nodeType = value
				pattern.literal = true
				pattern.nodeOffset = i
			}
		}
	} else {
		start := i
		for i < close && isFallbackNodeToken(source[i]) {
			i++
		}
		if i > start {
			pattern.nodeType = source[start:i]
			pattern.rooted = true
			pattern.nodeOffset = start
		}
	}
	if pattern.nodeType == "" {
		return pattern
	}
	// Nested grouping may replace pattern with a child value.  Restore the
	// outer source span so predicates attached to the enclosing expression are
	// included when metadata is decoded later.
	pattern.sourceStart, pattern.sourceEnd = open, close+1
	// Capture annotations before the first predicate belong to the pattern.
	// This intentionally includes captures nested in child expressions; the
	// fallback matcher cannot enforce child structure, but returning those
	// names is more useful than dropping them entirely.  Scan quoted literals
	// and semicolon comments explicitly: an `@` in `(string "email@host")`
	// is text, not a capture annotation.  The old byte-by-byte scanner treated
	// it as a real capture and made CaptureNames/IndexForName diverge from the
	// native query compiler.
	inString := false
	escaped := false
	for j := open + 1; j < close; j++ {
		c := source[j]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ';':
			// A comment extends through (but not including) the newline. The
			// loop increment below then resumes normal scanning on that newline.
			for j < close && source[j] != '\n' {
				j++
			}
		case '#':
			// Predicate arguments may contain capture-looking tokens, but those
			// belong to predicate metadata rather than the fallback pattern's
			// capture list. The enclosing pattern's predicates are parsed by
			// parseTextPredicates separately.
			j = close
		case '.':
			// A dot can either be the immediate-child marker (`.` followed by a
			// child pattern) or the historical predicate prefix (`.eq?`). Only
			// the latter terminates capture scanning; otherwise captures in the
			// following child expression still belong to this compatibility
			// pattern.
			if _, _, predicate := scanTextPredicateOperator(source, j); predicate {
				j = close
			}
		default:
			if c != '@' {
				continue
			}
			name, next := fallbackCaptureToken(source, j, close)
			if name == "" {
				continue
			}
			pattern.captures = appendUniqueString(pattern.captures, name)
			if pattern.capture == "" {
				pattern.capture = name
			}
			j = next - 1
		}
	}
	return pattern
}

// fallbackIsPredicateExpression reports whether a top-level parenthesized
// expression is a Tree-sitter predicate sibling.  Predicates are commonly
// written outside the node pattern (`(number) @n (#eq? @n "1")`), while the
// fallback matcher also accepts the compact nested spelling
// `((number) @n (#eq? ...))`.  Only a valid `#name`/`.name` operator at the
// first non-whitespace byte is considered here; malformed expressions are
// left for the regular syntax scanner to diagnose.
func fallbackIsPredicateExpression(source string, open, close int) bool {
	if open < 0 || close <= open || close > len(source) || source[open] != '(' {
		return false
	}
	i := skipFallbackSpace(source, open+1, close)
	if i >= close || (source[i] != '#' && source[i] != '.') {
		return false
	}
	_, _, ok := scanTextPredicateOperator(source, i)
	if ok {
		return true
	}
	// Unknown/malformed predicate names are still valid user-defined
	// predicates only when the operator token itself is lexically valid. A
	// bare `#` should therefore remain a syntax error rather than being
	// swallowed as an empty compatibility pattern.
	return false
}

func isFallbackNodeToken(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' || c == '*'
}

func fallbackCaptureToken(source string, at, end int) (string, int) {
	if at >= end || source[at] != '@' {
		return "", at
	}
	i := at + 1
	if i >= end || !isQueryIdentStartByte(source[i]) {
		return "", at
	}
	i++
	for i < end && isQueryIdentContinueByte(source[i]) {
		i++
	}
	if i == at+1 {
		return "", at
	}
	return source[at+1 : i], i
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func fallbackCaptureNames(patterns []fallbackPattern) []string {
	result := make([]string, 0)
	for _, pattern := range patterns {
		captures := pattern.captures
		if len(captures) == 0 && pattern.capture != "" {
			captures = []string{pattern.capture}
		}
		for _, capture := range captures {
			if capture != "" {
				result = appendUniqueString(result, capture)
			}
		}
	}
	return result
}

func fallbackSyntaxError(source string, offset int) *QueryError {
	if offset < 0 {
		offset = 0
	}
	if offset > len(source) {
		offset = len(source)
	}
	row, column := sourcePosition(source, uint32(offset))
	lineStart := strings.LastIndexByte(source[:offset], '\n') + 1
	lineEnd := strings.IndexByte(source[offset:], '\n')
	if lineEnd < 0 {
		lineEnd = len(source) - offset
	}
	line := source[lineStart : offset+lineEnd]
	message := "Unexpected EOF"
	if line != "" {
		message = line + "\n" + strings.Repeat(" ", offset-lineStart) + "^"
	}
	return &QueryError{Message: message, Offset: uint32(offset), Row: row, Column: column, Kind: QueryErrorSyntax}
}

// parseTextPredicates extracts the built-in predicates whose evaluation needs
// source text.  Unknown/user-defined predicates stay in the guest and are
// intentionally left untouched.  Quoted arguments are decoded using the same
// Go escaping rules used by Tree-sitter's query strings. A small state-machine
// scanner is used instead of a regular expression: predicate string literals
// are allowed to contain `)` (and escaped quotes), so a `[^)]*` expression
// would truncate the predicate and silently disable filtering on legacy
// bridges. The scanner also ignores `#` characters in quoted node literals
// and semicolon comments.
func parseTextPredicates(source string) []textPredicate {
	var result []textPredicate
	for offset := 0; offset < len(source); {
		c := source[offset]
		if c == '"' {
			offset = skipQueryQuoted(source, offset)
			continue
		}
		if c == ';' {
			offset = skipQueryComment(source, offset)
			continue
		}
		// Tree-sitter accepts both `#eq?` and the historical `.eq?`
		// predicate prefix.  The latter is used by a number of grammars (and
		// appears in the upstream query test-suite), so keep the compatibility
		// scanner in lock-step with the native query parser. A plain dot that is
		// part of a node/capture token simply fails scanTextPredicateOperator and
		// is consumed one byte at a time below.
		if c != '#' && c != '.' {
			offset++
			continue
		}

		operator, afterOperator, ok := scanTextPredicateOperator(source, offset)
		if !ok {
			offset++
			continue
		}
		// Built-in predicate names are case-sensitive. Unknown operators are
		// intentionally left to callers/user predicates and are not evaluated
		// by this host-side compatibility scanner.
		if !isTextPredicateOperator(operator) {
			offset = afterOperator
			continue
		}
		position := skipQueryWhitespace(source, afterOperator)
		if position >= len(source) || source[position] != '@' {
			offset = afterOperator
			continue
		}
		capture, afterCapture := fallbackCaptureToken(source, position, len(source))
		if capture == "" {
			offset = afterOperator
			continue
		}
		end := scanTextPredicateEnd(source, afterCapture)
		tail := source[afterCapture:end]
		nextOffset := end
		if nextOffset < len(source) {
			nextOffset++
		}
		args := predicateArgs(tail)
		// any-of?/not-any-of? permit an empty literal list (the native
		// compiler represents this as just the operator and capture). Other
		// built-ins require at least one argument after the capture.
		if len(args) == 0 && operator != "any-of?" && operator != "not-any-of?" {
			offset = nextOffset
			continue
		}
		p := textPredicate{op: operator, capture: capture}
		if len(args) != 0 && strings.HasPrefix(args[0], "@") {
			p.otherCapture = strings.TrimPrefix(args[0], "@")
		} else if len(args) != 0 {
			if decoded, ok := decodePredicateValue(args[0]); ok {
				p.value = decoded
			} else {
				offset = nextOffset
				continue
			}
		}
		// An empty regular expression is valid and matches every string. Keep
		// it compiled just like the native predicate parser; guarding on
		// p.value != "" would make legacy bridge behavior diverge.
		if strings.Contains(operator, "match?") {
			if re, err := regexp.Compile(p.value); err == nil {
				p.regex = re
			}
		}
		if strings.Contains(operator, "any-of?") {
			p.values = make([]string, 0, len(args))
			for _, arg := range args {
				if decoded, ok := decodePredicateValue(arg); ok {
					p.values = append(p.values, decoded)
				}
			}
		}
		result = append(result, p)
		// Continue after the closing delimiter. This both avoids rediscovering
		// nested `#` tokens in a predicate string and keeps the scanner's
		// quote/comment state synchronized with the query grammar.
		offset = nextOffset
	}
	return result
}

// fallbackPredicateToken is the small lexical representation needed to
// validate built-in predicates when a legacy bridge has no native query
// compiler.  Tree-sitter predicate arguments are either capture names or
// quoted string literals; keeping those two forms distinct lets the validator
// reproduce the useful diagnostics emitted by the upstream Go binding.
type fallbackPredicateToken struct {
	text    string
	capture bool
	// quoted is true for both quoted string literals and bare symbols.  In
	// Tree-sitter's query grammar bare symbols in predicate argument position
	// are string values too (for example `#set! name something` and
	// `#match? @n i`).  Retaining the historical field name keeps the internal
	// validator compact while treating both forms identically.
	quoted bool
	offset int // byte offset within the predicate tail
}

// validateFallbackPredicates validates every built-in predicate in source.
// The native compiler performs these checks while constructing TSQuery, but a
// compatibility module may only provide the parser/tree ABI.  In that case
// accepting malformed predicates would make the same source behave
// differently depending on which optional exports a module happens to have.
// Unknown predicates remain user-defined and are intentionally left alone.
func validateFallbackPredicates(source string, captureNames []string) error {
	declared := make(map[string]struct{}, len(captureNames))
	for _, name := range captureNames {
		declared[name] = struct{}{}
	}
	for offset := 0; offset < len(source); {
		switch source[offset] {
		case '"':
			offset = skipQueryQuoted(source, offset)
			continue
		case ';':
			offset = skipQueryComment(source, offset)
			continue
		case '#', '.':
			// Continue below.
		default:
			offset++
			continue
		}
		op, afterOp, ok := scanTextPredicateOperator(source, offset)
		if !ok {
			offset++
			continue
		}
		end := scanTextPredicateEnd(source, afterOp)
		// Keep the source scanner moving even when a malformed predicate has no
		// closing delimiter; parseFallbackPatterns reports an enclosing syntax
		// error separately, while this path should still never loop.
		next := end
		if next < len(source) {
			next++
		}
		if !isFallbackValidatedPredicateOperator(op) {
			offset = next
			continue
		}
		tailStart := skipQueryWhitespace(source, afterOp)
		tokens, lexicalOK := fallbackPredicateTokens(source[tailStart:end])
		if !lexicalOK {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid arguments to %s predicate.", op))
		}
		if err := validateFallbackPredicate(source, offset, tailStart, op, tokens, declared); err != nil {
			return err
		}
		offset = next
	}
	return nil
}

// fallbackPredicateGroup is the lexical representation shared by the
// fallback metadata decoder and the strict predicate validator.  The native
// query ABI exposes predicate steps directly; legacy bridges do not, so the
// host must recover the same public metadata from the source text.
type fallbackPredicateGroup struct {
	operator string
	tokens   []fallbackPredicateToken
}

// scanFallbackPredicateGroups returns all predicate S-expressions in source.
// It understands both `#name` and the historical `.name` prefix and skips
// quoted strings/comments while looking for operators. Malformed groups are
// omitted; parseFallbackPatterns/validateFallbackPredicates report the actual
// syntax or argument error to the caller.
func scanFallbackPredicateGroups(source string) []fallbackPredicateGroup {
	var result []fallbackPredicateGroup
	for offset := 0; offset < len(source); {
		switch source[offset] {
		case '"':
			offset = skipQueryQuoted(source, offset)
			continue
		case ';':
			offset = skipQueryComment(source, offset)
			continue
		case '#', '.':
			// Continue below.
		default:
			offset++
			continue
		}
		op, afterOp, ok := scanTextPredicateOperator(source, offset)
		if !ok {
			offset++
			continue
		}
		end := scanTextPredicateEnd(source, afterOp)
		next := end
		if next < len(source) {
			next++
		}
		base := skipQueryWhitespace(source, afterOp)
		if base <= end {
			if tokens, lexicalOK := fallbackPredicateTokens(source[base:end]); lexicalOK {
				result = append(result, fallbackPredicateGroup{operator: op, tokens: tokens})
			}
		}
		offset = next
	}
	return result
}

// fallbackPropertyFromTokens converts the source-level representation of a
// set!/is?/is-not? predicate to the public QueryProperty shape. The validator
// has already checked the argument grammar, but keep this helper defensive so
// malformed custom bridges cannot panic metadata callers.
func fallbackPropertyFromTokens(tokens []fallbackPredicateToken, captureIDs map[string]uint32) (QueryProperty, bool) {
	if len(tokens) == 0 || len(tokens) > 3 {
		return QueryProperty{}, false
	}
	var property QueryProperty
	keySet := false
	for _, token := range tokens {
		if token.capture {
			id, ok := captureIDs[token.text]
			if !ok || property.CaptureID != nil {
				return QueryProperty{}, false
			}
			copyID := id
			property.CaptureID = &copyID
			continue
		}
		if !token.quoted {
			return QueryProperty{}, false
		}
		value := token.text
		if !keySet {
			property.Key = value
			keySet = true
		} else if property.Value == nil {
			property.Value = &value
		} else {
			return QueryProperty{}, false
		}
	}
	return property, keySet
}

// fallbackMetadataFromSource decodes all public predicate metadata available
// from a source-level fallback pattern. This fills the same four vectors that
// go-tree-sitter exposes (text predicates, property predicates/settings, and
// user-defined predicates), which is especially useful for grammars built
// against an older bridge lacking ts_query_predicates_for_pattern_into.
func fallbackMetadataFromSource(source string, captureNames []string) (text []TextPredicateCapture, properties []PropertyPredicate, settings []QueryProperty, general []QueryPredicate) {
	captureIDs := make(map[string]uint32, len(captureNames))
	for i, name := range captureNames {
		captureIDs[name] = uint32(i)
	}
	// Reuse the same text scanner used by the execution path so escaped/bare
	// predicate values have identical decoding and regular-expression handling.
	text = (&Query{fallbackNames: append([]string(nil), captureNames...)}).publicTextPredicatesFromPrivate(parseTextPredicates(source))
	for _, group := range scanFallbackPredicateGroups(source) {
		op := group.operator
		switch op {
		case "eq?", "not-eq?", "any-eq?", "any-not-eq?",
			"match?", "not-match?", "any-match?", "any-not-match?",
			"any-of?", "not-any-of?":
			// Already represented in text above.
			continue
		case "set!", "is?", "is-not?":
			property, ok := fallbackPropertyFromTokens(group.tokens, captureIDs)
			if !ok {
				continue
			}
			if op == "set!" {
				settings = append(settings, property)
			} else {
				properties = append(properties, PropertyPredicate{Property: property, Positive: op == "is?"})
			}
		default:
			args := make([]QueryPredicateArg, 0, len(group.tokens))
			for _, token := range group.tokens {
				if token.capture {
					id, ok := captureIDs[token.text]
					if !ok {
						continue
					}
					copyID := id
					args = append(args, QueryPredicateArg{CaptureID: &copyID})
				} else if token.quoted {
					value := token.text
					args = append(args, QueryPredicateArg{String: &value})
				}
			}
			general = append(general, QueryPredicate{Operator: op, Args: args})
		}
	}
	return text, properties, settings, general
}

func isFallbackValidatedPredicateOperator(operator string) bool {
	switch operator {
	case "eq?", "not-eq?", "any-eq?", "any-not-eq?",
		"match?", "not-match?", "any-match?", "any-not-match?",
		"any-of?", "not-any-of?", "set!", "is?", "is-not?":
		return true
	default:
		return false
	}
}

// fallbackPredicateTokens lexes the text between a predicate operator and
// its closing parenthesis. Quoted strings and bare identifier symbols both
// represent string values in Tree-sitter's query grammar; punctuation and
// malformed tokens are rejected instead of silently skipped.
func fallbackPredicateTokens(tail string) ([]fallbackPredicateToken, bool) {
	tokens := make([]fallbackPredicateToken, 0, 3)
	for i := 0; i < len(tail); {
		for i < len(tail) {
			switch tail[i] {
			case ' ', '\t', '\r', '\n':
				i++
				continue
			case ';':
				for i < len(tail) && tail[i] != '\n' {
					i++
				}
				continue
			}
			break
		}
		if i >= len(tail) {
			break
		}
		switch tail[i] {
		case '@':
			name, next := fallbackCaptureToken(tail, i, len(tail))
			if name == "" {
				return nil, false
			}
			tokens = append(tokens, fallbackPredicateToken{text: name, capture: true, offset: i})
			i = next
		case '"':
			next := skipQueryQuoted(tail, i)
			if next <= i || next > len(tail) || next == len(tail) && (len(tail) == 0 || tail[len(tail)-1] != '"') {
				return nil, false
			}
			raw := tail[i:next]
			value, ok := decodeQueryString(raw)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, fallbackPredicateToken{text: value, quoted: true, offset: i})
			i = next
		default:
			// Bare symbols are legal predicate string values.  This follows the
			// Tree-sitter query lexer (`stream_is_ident_start` and
			// `stream_scan_identifier`): the first byte is alphanumeric, `_`, or
			// `-`; subsequent bytes may additionally contain `.`, `?`, and `!`.
			// Keep the scanner byte-oriented (as the query ABI is) while accepting
			// UTF-8 bytes conservatively through isQueryIdentByte; a malformed or
			// punctuation-only token remains an invalid argument.
			if !isQueryIdentStartByte(tail[i]) {
				// Parentheses and other punctuation are not legal predicate
				// arguments. Consume one token only to keep diagnostics deterministic;
				// the caller returns the generic invalid-arguments message.
				return nil, false
			}
			start := i
			i++
			for i < len(tail) && isQueryIdentContinueByte(tail[i]) {
				i++
			}
			tokens = append(tokens, fallbackPredicateToken{text: tail[start:i], quoted: true, offset: start})
		}
	}
	return tokens, true
}

func fallbackPredicateError(source string, offset int, message string) error {
	row, _ := sourcePosition(source, uint32(offset))
	// Match validateNativePredicates: predicate diagnostics carry the pattern
	// row but intentionally leave byte offset/column at zero.
	return &QueryError{Message: message, Row: row, Column: 0, Offset: 0, Kind: QueryErrorPredicate}
}

func fallbackPredicateTokenDescription(token fallbackPredicateToken) string {
	if token.capture {
		return "@" + token.text
	}
	return token.text
}

func validateFallbackPredicate(source string, offset, tokenBase int, operator string, tokens []fallbackPredicateToken, declared map[string]struct{}) error {
	// Capture references must name a capture declared somewhere in the query.
	// This is checked by ts_query_new for both built-ins and user-defined
	// predicates; doing it here prevents a legacy module from accepting a
	// query that native Tree-sitter rejects.
	for _, token := range tokens {
		if token.capture {
			if _, ok := declared[token.text]; !ok {
				captureOffset := tokenBase + token.offset + 1 // skip '@'
				if captureOffset < 0 {
					captureOffset = 0
				}
				if captureOffset > len(source) {
					captureOffset = len(source)
				}
				row, column := sourcePosition(source, uint32(captureOffset))
				return &QueryError{Message: token.text, Row: row, Column: column, Offset: uint32(captureOffset), Kind: QueryErrorCapture}
			}
		}
	}
	name := func(index int) string {
		if index < 0 || index >= len(tokens) {
			return ""
		}
		return fallbackPredicateTokenDescription(tokens[index])
	}
	switch operator {
	case "eq?", "not-eq?", "any-eq?", "any-not-eq?":
		if len(tokens) != 2 {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Wrong number of arguments to #eq? predicate. Expected 2, got %d.", len(tokens)))
		}
		if !tokens[0].capture {
			return fallbackPredicateError(source, offset, fmt.Sprintf("First argument to #eq? predicate must be a capture name. Got literal %s.", name(0)))
		}
		if !tokens[1].capture && !tokens[1].quoted {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Second argument to #eq? predicate must be a capture name or a literal. Got %s.", name(1)))
		}
	case "match?", "not-match?", "any-match?", "any-not-match?":
		if len(tokens) != 2 {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Wrong number of arguments to #match? predicate. Expected 2, got %d.", len(tokens)))
		}
		if !tokens[0].capture {
			return fallbackPredicateError(source, offset, fmt.Sprintf("First argument to #match? predicate must be a capture name. Got literal %s.", name(0)))
		}
		if !tokens[1].quoted {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Second argument to #match? predicate must be a literal. Got %s.", name(1)))
		}
		if _, err := regexp.Compile(tokens[1].text); err != nil {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid regex: '%s'", tokens[1].text))
		}
	case "any-of?", "not-any-of?":
		if len(tokens) < 1 {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Wrong number of arguments to #any-of? predicate. Expected at least 1, got %d.", len(tokens)))
		}
		if !tokens[0].capture {
			return fallbackPredicateError(source, offset, fmt.Sprintf("First argument to #any-of? predicate must be a capture name. Got literal %s.", name(0)))
		}
		for _, token := range tokens[1:] {
			if !token.quoted {
				return fallbackPredicateError(source, offset, fmt.Sprintf("Arguments to #any-of? predicate must be literals. Got %s.", fallbackPredicateTokenDescription(token)))
			}
		}
	case "set!", "is?", "is-not?":
		if len(tokens) == 0 || len(tokens) > 3 {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Wrong number of arguments to %s predicate. Expected 1 to 3, got %d.", operator, len(tokens)))
		}
		captureSeen, keySeen, valueSeen := false, false, false
		for _, token := range tokens {
			if token.capture {
				if captureSeen {
					return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid arguments to %s predicate. Unexpected second capture name @%s", operator, token.text))
				}
				captureSeen = true
				continue
			}
			if !token.quoted {
				return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid arguments to %s predicate.", operator))
			}
			if !keySeen {
				keySeen = true
			} else if !valueSeen {
				valueSeen = true
			} else {
				return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid arguments to %s predicate. Unexpected third argument @%s", operator, token.text))
			}
		}
		if !keySeen {
			return fallbackPredicateError(source, offset, fmt.Sprintf("Invalid arguments to %s predicate. Missing key argument", operator))
		}
	}
	return nil
}

func isTextPredicateOperator(operator string) bool {
	switch operator {
	case "eq?", "not-eq?", "any-eq?", "any-not-eq?",
		"match?", "not-match?", "any-match?", "any-not-match?",
		"any-of?", "not-any-of?":
		return true
	default:
		return false
	}
}

func scanTextPredicateOperator(source string, offset int) (operator string, next int, ok bool) {
	if offset < 0 || offset >= len(source) || (source[offset] != '#' && source[offset] != '.') {
		return "", offset, false
	}
	position := offset + 1
	if position >= len(source) || !isQueryIdentStartByte(source[position]) {
		return "", position, false
	}
	position++
	for position < len(source) && isQueryIdentContinueByte(source[position]) {
		position++
	}
	return source[offset+1 : position], position, true
}

// isQueryIdentStartByte/isQueryIdentContinueByte mirror the identifier
// scanner used by Tree-sitter's query grammar.  The scanner operates on
// decoded code points and, in the C locale used by the portable runtime,
// `iswalnum` accepts only ASCII alphanumeric characters.  Keep the fallback
// lexer equally strict: accepting an arbitrary high-bit UTF-8 byte here would
// make a legacy bridge accept capture names and bare predicate values that the
// native query compiler rejects.  UTF-8 remains fully supported for quoted
// query strings; those are decoded by decodeQueryString and never pass through
// this identifier predicate.
func isQueryIdentStartByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-'
}

func isQueryIdentContinueByte(c byte) bool {
	return isQueryIdentStartByte(c) || c == '.' || c == '?' || c == '!'
}

func skipQueryWhitespace(source string, offset int) int {
	for offset < len(source) {
		switch source[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func skipQueryQuoted(source string, offset int) int {
	if offset < 0 || offset >= len(source) || source[offset] != '"' {
		return offset + 1
	}
	escaped := false
	for offset = offset + 1; offset < len(source); offset++ {
		c := source[offset]
		if c == '"' && !escaped {
			return offset + 1
		}
		if c == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
	return len(source)
}

func skipQueryComment(source string, offset int) int {
	for offset < len(source) && source[offset] != '\n' {
		offset++
	}
	return offset
}

// scanTextPredicateEnd returns the first closing ')' at the predicate's
// nesting level. Parentheses inside quoted strings and semicolon comments are
// ignored, while nested parentheses in a malformed/custom query are balanced
// defensively before the outer delimiter is selected.
func scanTextPredicateEnd(source string, offset int) int {
	depth := 0
	for position := offset; position < len(source); position++ {
		switch source[position] {
		case '"':
			position = skipQueryQuoted(source, position) - 1
		case ';':
			position = skipQueryComment(source, position) - 1
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return position
			}
			depth--
		}
	}
	return len(source)
}

func predicateArgs(tail string) []string {
	var args []string
	for i := 0; i < len(tail); {
		for i < len(tail) && (tail[i] == ' ' || tail[i] == '\t' || tail[i] == '\r' || tail[i] == '\n') {
			i++
		}
		if i >= len(tail) {
			break
		}
		if tail[i] == '@' {
			start := i
			i++
			for i < len(tail) && (tail[i] == '_' || tail[i] == '-' || tail[i] == '.' || tail[i] >= 'a' && tail[i] <= 'z' || tail[i] >= 'A' && tail[i] <= 'Z' || tail[i] >= '0' && tail[i] <= '9') {
				i++
			}
			args = append(args, tail[start:i])
			continue
		}
		if tail[i] == '"' {
			start := i
			i++
			escaped := false
			for i < len(tail) {
				if tail[i] == '"' && !escaped {
					i++
					break
				}
				if tail[i] == '\\' && !escaped {
					escaped = true
				} else {
					escaped = false
				}
				i++
			}
			args = append(args, tail[start:i])
			continue
		}
		// Bare predicate symbols are string values in the Tree-sitter query
		// grammar. Keep the raw token (without quotes) so decodePredicateValue
		// can handle it alongside quoted literals. Unknown punctuation is
		// skipped for this best-effort metadata scanner; the strict fallback
		// validator reports malformed arguments when a legacy bridge is used.
		if isQueryIdentStartByte(tail[i]) {
			start := i
			i++
			for i < len(tail) && isQueryIdentContinueByte(tail[i]) {
				i++
			}
			args = append(args, tail[start:i])
			continue
		}
		i++
	}
	return args
}

func decodeQueryString(value string) (string, bool) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	decoded, err := strconv.Unquote(value)
	return decoded, err == nil
}

// decodePredicateValue accepts either a quoted query string or a bare symbol.
// Tree-sitter stores both forms in the same predicate string table. Bare
// symbols are intentionally returned unchanged (including '.', '?', and '!'
// continuation characters), while malformed quoted strings are rejected.
func decodePredicateValue(value string) (string, bool) {
	if strings.HasPrefix(value, "\"") {
		return decodeQueryString(value)
	}
	if value == "" || !isQueryIdentStartByte(value[0]) {
		return "", false
	}
	for i := 1; i < len(value); i++ {
		if !isQueryIdentContinueByte(value[i]) {
			return "", false
		}
	}
	return value, true
}

// textPredicatesForPattern returns the built-in predicates attached to one
// pattern. Native query metadata is preferred because it is produced by the
// same parser that executes the query; the source scanner is retained for
// legacy modules that only implement the small fallback matcher.
func (q *Query) textPredicatesForPattern(pattern uint32) []textPredicate {
	if q == nil {
		return nil
	}
	q.predicateMu.Lock()
	if q.predicateCache == nil {
		q.predicateCache = make(map[uint32][]textPredicate)
	}
	if cached, ok := q.predicateCache[pattern]; ok {
		out := append([]textPredicate(nil), cached...)
		q.predicateMu.Unlock()
		return out
	}
	q.predicateMu.Unlock()

	var parsed []textPredicate
	if q.native {
		if steps, err := q.PredicateSteps(pattern); err == nil {
			parsed = q.textPredicatesFromSteps(steps)
			// A native pattern with no predicate steps is a successful,
			// intentionally empty result.  Keep it non-nil so we do not fall
			// through to q.predicates (the source-wide legacy scanner), which
			// would incorrectly apply another pattern's predicates here.
			if parsed == nil {
				parsed = []textPredicate{}
			}
		}
	}
	if parsed == nil {
		// Legacy bridges generally expose only one pattern. A partially native
		// bridge may nevertheless provide pattern byte ranges, in which case
		// parse the corresponding source slice before falling back to the
		// source-wide scanner. This keeps reused capture names scoped correctly.
		if q.native {
			if scoped, ok := q.sourcePredicatesForPattern(pattern); ok {
				parsed = scoped
			}
		}
		if parsed == nil {
			if !q.native {
				candidate, found := q.fallbackPatternForGroup(pattern)
				if found && candidate.sourceStart >= 0 && candidate.sourceEnd > candidate.sourceStart && candidate.sourceEnd <= len(q.source) {
					parsed = parseTextPredicates(q.source[candidate.sourceStart:candidate.sourceEnd])
				}
			}
			if parsed == nil {
				parsed = append([]textPredicate(nil), q.predicates...)
			}
		}
	}
	q.predicateMu.Lock()
	q.predicateCache[pattern] = append([]textPredicate(nil), parsed...)
	q.predicateMu.Unlock()
	return parsed
}

func (q *Query) fallbackPatternForGroup(group uint32) (fallbackPattern, bool) {
	if q == nil {
		return fallbackPattern{}, false
	}
	// Prefer a branch carrying predicate metadata. For an alternatives
	// expression a trailing sibling predicate is propagated to every branch,
	// but this preference also handles compatibility values produced by older
	// parsers that attached it only to the final branch.
	var first fallbackPattern
	found := false
	for _, candidate := range q.fallback {
		if candidate.patternIndex != group {
			continue
		}
		if !found {
			first, found = candidate, true
		}
		if len(candidate.predicates) != 0 {
			return candidate, true
		}
	}
	return first, found
}

// sourcePredicatesForPattern returns a best-effort predicate scan for one
// native pattern when the optional predicate-step ABI is unavailable. The
// range exports are part of the original query API and are cheap to probe; if
// either export is absent (or reports an unusable range), the caller can fall
// back to the legacy source-wide scanner.
func (q *Query) sourcePredicatesForPattern(pattern uint32) ([]textPredicate, bool) {
	if q == nil || !q.native || q.closed.Load() {
		return nil, false
	}
	start, end := q.StartByteForPattern(pattern), q.EndByteForPattern(pattern)
	if end <= start || uint64(end) > uint64(len(q.source)) {
		return nil, false
	}
	return parseTextPredicates(q.source[start:end]), true
}

func (q *Query) textPredicatesFromSteps(steps []QueryPredicateStep) []textPredicate {
	if len(steps) == 0 {
		return nil
	}
	var result []textPredicate
	start := 0
	for i := 0; i <= len(steps); i++ {
		if i < len(steps) && steps[i].Type != QueryPredicateStepDone {
			continue
		}
		part := steps[start:i]
		start = i + 1
		if len(part) < 2 || part[0].Type != QueryPredicateStepString {
			continue
		}
		op := q.StringValue(part[0].ValueID)
		if op == "" {
			continue
		}
		arg := func(step QueryPredicateStep) (string, bool, bool) {
			switch step.Type {
			case QueryPredicateStepCapture:
				return q.CaptureName(step.ValueID), true, true
			case QueryPredicateStepString:
				return q.StringValue(step.ValueID), false, true
			default:
				return "", false, false
			}
		}
		first, firstCapture, ok := arg(part[1])
		if !ok || !firstCapture {
			continue
		}
		p := textPredicate{op: op, capture: first}
		switch op {
		case "eq?", "not-eq?", "any-eq?", "any-not-eq?":
			if len(part) != 3 {
				continue
			}
			second, secondCapture, ok := arg(part[2])
			if !ok {
				continue
			}
			if secondCapture {
				p.otherCapture = second
			} else {
				p.value = second
			}
		case "match?", "not-match?", "any-match?", "any-not-match?":
			if len(part) != 3 || part[2].Type != QueryPredicateStepString {
				continue
			}
			p.value = q.StringValue(part[2].ValueID)
			if re, err := regexp.Compile(p.value); err == nil {
				p.regex = re
			} else {
				continue
			}
		case "any-of?", "not-any-of?":
			if len(part) < 2 {
				continue
			}
			for _, step := range part[2:] {
				if step.Type != QueryPredicateStepString {
					p.values = nil
					break
				}
				p.values = append(p.values, q.StringValue(step.ValueID))
			}
		default:
			continue
		}
		result = append(result, p)
	}
	return result
}

func (q *Query) satisfies(match QueryMatch) bool {
	predicates := q.predicatesForMatch(match)
	if len(predicates) == 0 {
		return true
	}
	for _, predicate := range predicates {
		var nodes []Node
		var other []Node
		for _, capture := range match.Captures {
			name := q.CaptureName(capture.Index)
			if name == predicate.capture {
				nodes = append(nodes, capture.Node)
			}
			if predicate.otherCapture != "" && name == predicate.otherCapture {
				other = append(other, capture.Node)
			}
		}
		if !evaluateTextPredicate(predicate, nodes, other) {
			return false
		}
	}
	return true
}

func (q *Query) satisfiesWithText(match QueryMatch, text []byte) bool {
	// Preserve the public helper's historical nil/empty-buffer behavior: when
	// no explicit bytes are supplied, evaluate against the tree-retained text.
	// Cursor iterators use satisfiesWithTextBuffer directly so an explicitly
	// supplied empty buffer remains distinguishable from the default.
	if len(text) == 0 {
		return q.satisfies(match)
	}
	return q.satisfiesWithTextBuffer(match, text)
}

// satisfiesWithTextBuffer evaluates built-in predicates against exactly the
// supplied source buffer, including a non-nil empty buffer.  It is kept
// separate from satisfiesWithText because the upstream iterator stores the
// caller's text slice and does not substitute the tree's source bytes.
func (q *Query) satisfiesWithTextBuffer(match QueryMatch, text []byte) bool {
	predicates := q.predicatesForMatch(match)
	if len(predicates) == 0 {
		return true
	}
	for _, predicate := range predicates {
		var nodes, other []Node
		for _, capture := range match.Captures {
			name := q.CaptureName(capture.Index)
			if name == predicate.capture {
				nodes = append(nodes, capture.Node)
			}
			if predicate.otherCapture != "" && name == predicate.otherCapture {
				other = append(other, capture.Node)
			}
		}
		if !evaluateTextPredicateBytes(predicate, nodes, other, text) {
			return false
		}
	}
	return true
}

// predicatesForMatch selects the predicate scope for a materialized match.
// Compatibility matches carry an explicit (possibly empty) per-pattern slice;
// native matches instead resolve metadata from the guest query. Keeping this
// distinction is essential when two fallback patterns reuse a capture name
// but attach different predicates.
func (q *Query) predicatesForMatch(match QueryMatch) []textPredicate {
	if match.predicates != nil {
		return match.predicates
	}
	return q.textPredicatesForPattern(match.PatternIndex)
}

func nodeTextBytes(node Node, text []byte) string {
	if node.IsNull() {
		return ""
	}
	start, end := node.StartByte(), node.EndByte()
	if start > end || uint64(end) > uint64(len(text)) {
		return node.Text()
	}
	return string(text[start:end])
}

func evaluateTextPredicateBytes(predicate textPredicate, nodes, other []Node, text []byte) bool {
	positive, matchAll := predicateSemantics(predicate.op)

	// Capture-to-capture predicates compare corresponding repeated captures,
	// matching Tree-sitter's host binding. For the `any-*` forms the first
	// satisfying pair is enough; the non-any forms require equal cardinality
	// and every pair to satisfy the requested relation.
	if predicate.otherCapture != "" {
		// This deliberately follows go-tree-sitter's SatisfiesTextPredicate
		// implementation.  The non-"any" forms require every corresponding
		// pair to satisfy the relation and equal capture cardinalities.  The
		// "any" forms return as soon as one pair satisfies it; if no pair does,
		// the upstream implementation reaches its final equal-cardinality check
		// (rather than returning false merely because no pair matched).
		paired := len(nodes)
		if len(other) < paired {
			paired = len(other)
		}
		for i := 0; i < paired; i++ {
			equal := nodeTextBytes(nodes[i], text) == nodeTextBytes(other[i], text)
			condition := equal == positive
			if !matchAll && condition {
				return true
			}
			if matchAll && !condition {
				return false
			}
		}
		return len(nodes) == len(other)
	}

	// Optional captures are allowed to be absent. This is how the upstream
	// binding treats eq?/match?/any-of? predicates on a zero-occurrence
	// quantified capture.
	if len(nodes) == 0 {
		return true
	}
	compare := func(node Node) bool {
		value := nodeTextBytes(node, text)
		if strings.Contains(predicate.op, "any-of?") {
			for _, candidate := range predicate.values {
				if value == candidate {
					return true
				}
			}
			return false
		}
		if predicate.regex != nil {
			return predicate.regex.MatchString(value)
		}
		return value == predicate.value
	}
	if matchAll {
		for _, node := range nodes {
			if compare(node) != positive {
				return false
			}
		}
		return true
	}
	for _, node := range nodes {
		if compare(node) == positive {
			return true
		}
	}
	// For any-* predicates, this is intentionally true when no node matched.
	// It mirrors the upstream host binding's loop/final-return behavior and is
	// observable for quantified captures (including an all-nonmatching group).
	return true
}

func evaluateTextPredicate(predicate textPredicate, nodes, other []Node) bool {
	positive, matchAll := predicateSemantics(predicate.op)
	if predicate.otherCapture != "" {
		paired := len(nodes)
		if len(other) < paired {
			paired = len(other)
		}
		for i := 0; i < paired; i++ {
			condition := (nodes[i].Text() == other[i].Text()) == positive
			if !matchAll && condition {
				return true
			}
			if matchAll && !condition {
				return false
			}
		}
		return len(nodes) == len(other)
	}
	if len(nodes) == 0 {
		return true
	}
	compare := func(node Node) bool {
		if strings.Contains(predicate.op, "any-of?") {
			for _, value := range predicate.values {
				if node.Text() == value {
					return true
				}
			}
			return false
		}
		if predicate.regex != nil {
			return predicate.regex.MatchString(node.Text())
		}
		return node.Text() == predicate.value
	}
	if matchAll {
		for _, node := range nodes {
			if compare(node) != positive {
				return false
			}
		}
		return true
	}
	for _, node := range nodes {
		if compare(node) == positive {
			return true
		}
	}
	return true
}

// predicateSemantics returns the polarity and quantifier semantics used by
// Tree-sitter's built-in text predicates. The `any-not-*` spellings are
// existential predicates over the negated condition; treating them as a
// negated existential (as a naive `any` implementation would) changes their
// result whenever repeated captures contain both matching and non-matching
// nodes.
func predicateSemantics(op string) (positive, matchAll bool) {
	base := strings.TrimPrefix(op, "any-")
	positive = !strings.HasPrefix(base, "not-")
	// any-of?/not-any-of? are deliberately all-node predicates in the upstream
	// binding, despite their name. Every other any-* form is existential.
	matchAll = !strings.HasPrefix(op, "any-") || strings.HasPrefix(op, "any-of?") || op == "not-any-of?"
	return positive, matchAll
}

func (q *Query) queryUint(names []string) uint32 { return q.queryUintWithArgs(names) }

func (q *Query) queryUintWithArgs(names []string, args ...uint64) uint32 {
	if q == nil || q.runtime == nil || q.closed.Load() {
		return 0
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed.Load() {
		return 0
	}
	callArgs := append([]uint64{uint64(q.handle.Load())}, args...)
	result, _, err := q.runtime.call(context.Background(), names, callArgs...)
	if err != nil || len(result) == 0 {
		return 0
	}
	value, ok := checkedU32(result[0])
	if !ok {
		// All query scalar accessors in the stable ABI are uint32.  Never
		// silently truncate an i64 result from a malformed/foreign module into
		// a seemingly valid low handle or count.
		return 0
	}
	return value
}

func (q *Query) queryBool(names []string, arg uint32) bool {
	return q.queryUintWithArgs(names, uint64(arg)) != 0
}

func (q *Query) queryString(id uint32, pointerNames, lengthNames []string) (string, error) {
	if err := q.ensureOpen(); err != nil {
		return "", err
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed.Load() {
		return "", ErrClosed
	}
	r := q.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	ptrFn, fnName, err := r.function(pointerNames...)
	if err != nil {
		return "", err
	}
	result, callErr := ptrFn.Call(r.Context(), uint64(q.handle.Load()), uint64(id))
	if callErr != nil {
		return "", &ABIError{Function: fnName, Message: callErr.Error()}
	}
	if len(result) == 0 {
		return "", nil
	}
	ptr, ptrOK := checkedU32(result[0])
	if !ptrOK {
		return "", &ABIError{Function: fnName, Message: "string pointer is not a wasm32 value"}
	}
	if ptr == 0 {
		return "", nil
	}
	mem := r.mod.Memory()
	if mem == nil {
		return "", fmt.Errorf("%w: module has no exported memory", ErrUnsupported)
	}
	if len(result) > 1 {
		length, lengthOK := checkedU32(result[1])
		if !lengthOK {
			return "", &ABIError{Function: fnName, Message: "string length is not a wasm32 value"}
		}
		if b, ok := mem.Read(ptr, length); ok {
			return string(b), nil
		}
		return "", &ABIError{Function: fnName, Message: "string pointer points outside guest memory"}
	}
	if lengthFn, _, lengthErr := r.function(lengthNames...); lengthErr == nil {
		if lengths, callLengthErr := lengthFn.Call(r.Context(), uint64(q.handle.Load()), uint64(id)); callLengthErr == nil && len(lengths) != 0 {
			length, lengthOK := checkedU32(lengths[0])
			if !lengthOK {
				return "", &ABIError{Function: fnName, Message: "string length is not a wasm32 value"}
			}
			if b, ok := mem.Read(ptr, length); ok {
				return string(b), nil
			}
			return "", &ABIError{Function: fnName, Message: "string pointer points outside guest memory"}
		}
	}
	value, readErr := r.readCString(ptr)
	if readErr != nil {
		return "", &ABIError{Function: fnName, Message: readErr.Error()}
	}
	return value, nil
}

// QueryCursor is stateful and safe for independent concurrent callers through
// its mutex.  Native cursor handles are allocated lazily on the first Exec.
type QueryCursor struct {
	mu sync.Mutex

	query   *Query
	root    Node
	runtime *Runtime
	handle  uint32
	// queryScratchPtr/queryScratchCap identify a reusable guest buffer used by
	// the size-probed query iterator ABI. Keeping this allocation across
	// captures avoids a malloc/free pair for every match while retaining the
	// same wire-format validation. The buffer belongs to runtime and is freed
	// whenever the native cursor handle is retired.
	queryScratchPtr uint32
	queryScratchCap uint32
	// nativeMatchAvailable/nativeCaptureAvailable record which iterator
	// exports were present when the current execution was started.  A partial
	// bridge may provide the query compiler and cursor exec operation while
	// omitting one of the iteration functions; treating that case as ordinary
	// exhaustion would silently lose results, so the missing surface falls
	// back to the host matcher (or the other native iterator) explicitly.
	nativeMatchAvailable   bool
	nativeCaptureAvailable bool
	fallbackPrepared       bool
	// treeLifeUnlock retains the execution tree's lifetime read lock for the
	// complete native cursor stream.  Tree-sitter query cursors keep pointers
	// into the tree after ts_query_cursor_exec returns; releasing this lock at
	// the end of Exec would let Tree.Close delete that tree before a later
	// NextMatch/NextCapture call.  The closure is owned by the cursor while an
	// execution is active and is released when the stream is exhausted, when a
	// new execution replaces it, or when the cursor closes.  Access is guarded
	// by mu.
	treeLifeUnlock func()
	matches        []QueryMatch
	index          int
	closed         atomic.Bool

	byteStart, byteEnd   uint32
	pointStart, pointEnd Point
	maxDepth             uint32
	// The zero value of each range/depth is meaningful to Tree-sitter (for
	// example, a byte range of [0,0] matches nothing and a max start depth of
	// zero restricts matching to the execution root).  Keep explicit flags so
	// callers can set those values before the native cursor is allocated.
	byteRangeSet, pointRangeSet, maxDepthSet bool
	matchLimit                               uint32
	matchLimitSet                            bool
	timeoutMicros                            uint64
	timeoutSet                               bool
	progressCallback                         func(QueryCursorState) bool
	// progressPersistent is set by ExecWithOptions.  The upstream cursor
	// options remain active while callers consume the stateful Next* methods;
	// retaining the callback here gives that form the same cancellation hook.
	// progressCanceled is deliberately host-side: the compact ABI has no Go
	// callback trampoline, so a callback that asks to stop prevents the next
	// guest iteration.
	progressPersistent bool
	progressCanceled   bool
	progressGeneration uint64
	// predicateText is the source buffer supplied to the iterator-shaped
	// Matches/Captures APIs.  Tree-sitter evaluates text predicates against that
	// buffer (which may intentionally differ from the bytes retained by the
	// parsed tree), so cursor execution must remember it while draining native
	// results.  predicateTextSet distinguishes an explicitly supplied empty
	// buffer from the nil/default tree source.
	predicateText    []byte
	predicateTextSet bool
	// lastCapture* records metadata for the most recent NextCapture event.
	// QueryCaptures uses it to expose the upstream match id/pattern/ordinal
	// while retaining the compact QueryCapture return type of this package.
	lastCaptureID            uint32
	lastCapturePattern       uint32
	lastCaptureOrdinal       uint32
	lastCaptureValid         bool
	captureIteratorFullMatch bool
	captureQueue             []queuedQueryCapture
	// predicateCapturesPrepared records that the native match stream has
	// already been drained for the predicate-aware capture iterator.  Native
	// Tree-sitter's next_capture interleaves captures from overlapping matches;
	// collecting and ordering the complete stream lets the host apply a
	// predicate to an entire match while retaining that observable order.
	predicateCapturesPrepared bool
	removedMatches            map[uint32]struct{}
}

// queuedQueryCapture retains the originating match id alongside a capture.
// The id is needed to implement RemoveMatch consistently when a fallback
// cursor (or a predicate-filtered native cursor) has buffered captures.
type queuedQueryCapture struct {
	capture        QueryCapture
	matchID        uint32
	patternIndex   uint32
	captureOrdinal uint32
}

// NewQueryCursor allocates an empty query cursor. The cursor is reusable after
// each Exec call and should be closed when no longer needed.
func NewQueryCursor() *QueryCursor {
	// Tree-sitter initializes a fresh cursor's capture-list limit to
	// UINT32_MAX. Keep that default in the host shadow too, so accessors called
	// before the first Exec (and compatibility cursors without a native handle)
	// report the same value as a native cursor. `matchLimitSet` remains false so
	// allocating a native cursor still gets its own C default unless the caller
	// explicitly configured a limit.
	c := &QueryCursor{matchLimit: ^uint32(0)}
	runtimepkg.SetFinalizer(c, func(cursor *QueryCursor) { _ = cursor.Close() })
	return c
}

// Handle returns the opaque guest cursor handle, or zero when the cursor is
// closed or has not yet executed a query.
func (c *QueryCursor) Handle() uint32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.handle == 0 || c.runtime == nil || c.runtime.closedState() {
		return 0
	}
	// A cursor's native state retains pointers into the execution tree.  If a
	// tree was closed independently (or the runtime was torn down underneath a
	// partial/legacy cursor), the guest address is no longer usable even though
	// the Go cursor wrapper may not have observed the close yet.
	if c.root.tree != nil && c.root.tree.closed.Load() {
		return 0
	}
	handle := c.handle
	if c.closed.Load() || c.runtime.closedState() || (c.root.tree != nil && c.root.tree.closed.Load()) {
		return 0
	}
	return handle
}

// releaseTreeLifeLocked releases the lifetime gate retained for the current
// query execution.  The caller must hold c.mu.  Keeping this transition in a
// helper makes every terminal iterator path (including progress cancellation)
// release the gate exactly once.
func (c *QueryCursor) releaseTreeLifeLocked() {
	if c == nil || c.treeLifeUnlock == nil {
		return
	}
	unlock := c.treeLifeUnlock
	c.treeLifeUnlock = nil
	unlock()
}

// Exec starts query at node. Consume the results with
// [QueryCursor.NextMatch] or [QueryCursor.NextCapture], or use
// [QueryCursor.Matches] to collect results in one call. Query and node must
// belong to the same runtime. Close the cursor before the tree and query.
func (c *QueryCursor) Exec(query *Query, node Node) error {
	if c == nil || c.closed.Load() {
		return ErrClosed
	}
	if query == nil {
		return ErrUnsupported
	}
	if err := query.ensureOpen(); err != nil {
		return err
	}
	if node.IsNull() {
		return ErrInvalidHandle
	}
	if node.tree == nil || node.tree.rt != query.runtime {
		return fmt.Errorf("wasitter: query and node belong to different runtimes")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	// If this cursor already has an active native stream, retire its guest
	// handle while the old execution tree is still retained.  Deleting the
	// cursor before releasing the old lifetime gate prevents Tree.Close from
	// racing a native destructor that may still inspect the old tree.
	if c.treeLifeUnlock != nil {
		c.deleteNativeHandleLocked()
		c.releaseTreeLifeLocked()
	}
	// Keep the query alive while its handle is passed to the guest. This pairs
	// with Query.Close's write lock and prevents a use-after-delete when callers
	// execute and close a query concurrently.
	query.mu.RLock()
	defer query.mu.RUnlock()
	if query.closed.Load() {
		return ErrClosed
	}
	// The native query cursor retains pointers into the execution tree after
	// this method returns. Keep the destination tree alive until the stream is
	// drained (or the cursor is reset/closed).  The previous stream, if any,
	// was retired above while its own lifetime gate was still held.
	unlockTrees := lockTreeLifePair(nil, node.tree)
	keepTreeLife := false
	defer func() {
		if !keepTreeLife {
			// An ABI failure after the new execution has been installed must not
			// leave a cursor pointing at a tree whose lifetime gate is about to be
			// released. Retire any handle allocated for this attempt while the
			// gate is still held, then clear the execution snapshot so a later
			// Next* call cannot dereference stale native state.
			c.deleteNativeHandleLocked()
			c.query, c.root, c.runtime = nil, Node{}, nil
			c.matches, c.index, c.captureQueue = nil, 0, nil
			c.predicateCapturesPrepared = false
			unlockTrees()
		}
	}()
	if err := node.tree.ensureOpen(); err != nil {
		return err
	}
	// A native query cursor handle is tied to its Runtime.  Reusing a cursor
	// with a query from another runtime must release the old guest allocation
	// before replacing the ownership fields.
	if c.handle != 0 && c.runtime != nil && c.runtime != query.runtime {
		c.deleteNativeHandleLocked()
	}
	c.query, c.root, c.runtime = query, node, query.runtime
	c.matches, c.index, c.captureQueue = nil, 0, nil
	c.nativeMatchAvailable, c.nativeCaptureAvailable = false, false
	c.fallbackPrepared = false
	c.lastCaptureID, c.lastCapturePattern, c.lastCaptureOrdinal, c.lastCaptureValid = 0, 0, 0, false
	c.captureIteratorFullMatch = false
	c.predicateText = nil
	c.predicateTextSet = false
	c.progressCallback = nil
	c.progressPersistent = false
	c.progressCanceled = false
	c.progressGeneration++
	c.predicateCapturesPrepared = false
	c.removedMatches = nil
	if query.native {
		if c.handle == 0 {
			result, fnName, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_new", "wasitter_query_cursor_new", "ts_query_cursor_new", "query_cursor_new"})
			if err == nil && len(result) != 0 {
				handle, ok := checkedU32(result[0])
				if !ok {
					return &ABIError{Function: fnName, Message: "query cursor handle is not a wasm32 value"}
				}
				c.handle = handle
			}
			if c.handle == 0 && err != nil && !isUnsupported(err) {
				return err
			}
		}
		if c.handle != 0 {
			_, _, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_exec", "wasitter_query_cursor_exec", "ts_query_cursor_exec", "query_cursor_exec"}, uint64(c.handle), uint64(query.handle.Load()), uint64(node.handle))
			if err == nil {
				// Resolve iterator exports once per execution.  The cursor ABI is
				// intentionally optional and older/experimental bridges can expose
				// only one of next_match/next_capture.  Remembering the capability
				// avoids conflating a missing function with an exhausted stream.
				r := c.runtime
				r.mu.Lock()
				_, _, matchErr := r.function(
					"tsw_query_cursor_next_match",
					"wasitter_query_cursor_next_match",
					"ts_query_cursor_next_match",
					"query_cursor_next_match",
				)
				_, _, captureErr := r.function(
					"tsw_query_cursor_next_capture",
					"wasitter_query_cursor_next_capture",
					"ts_query_cursor_next_capture",
					"query_cursor_next_capture",
				)
				r.mu.Unlock()
				c.nativeMatchAvailable = matchErr == nil
				c.nativeCaptureAvailable = captureErr == nil
				if !c.nativeMatchAvailable && !c.nativeCaptureAvailable {
					// There is no native iteration surface at all. Retire the
					// otherwise-useless guest cursor; materialize the compatibility
					// candidates lazily on the first Next* call. Deferring that work
					// also avoids recursively acquiring query.mu while the read lock
					// held by this Exec call is still active.
					c.deleteNativeHandleLocked()
					c.treeLifeUnlock = unlockTrees
					keepTreeLife = true
					return nil
				}
				// Cursor options may be configured before the first Exec.  Apply
				// every explicitly configured option, including zero values.
				c.applyNativeOptionsLocked()
				c.treeLifeUnlock = unlockTrees
				keepTreeLife = true
				return nil
			}
			if !isUnsupported(err) {
				return err
			}
			// The handle may have been allocated successfully but the module may
			// not implement the exec operation. Detach it before entering the
			// compatibility matcher so a later Next call cannot use a cursor that
			// was never initialized for this query/tree.
			c.deleteNativeHandleLocked()
		}
	}
	// A cursor that previously ran a native query can be reused for a
	// compatibility/fallback query.  Do not accidentally consume the stale
	// native handle in that case.
	if !query.native && c.handle != 0 {
		c.deleteNativeHandleLocked()
	}
	c.matches = c.filterFallbackMatches(query.matchesFallback(node))
	for i := range c.matches {
		c.matches[i].cursor = c
		c.matches[i].root = c.root
	}
	c.treeLifeUnlock = unlockTrees
	keepTreeLife = true
	return nil
}

// applyNativeOptionsLocked reapplies options that were configured before (or
// between) Exec calls.  The caller must hold c.mu; Runtime.call serializes the
// actual guest invocation.  Optional exports are deliberately best-effort so
// older bridge modules continue to work.
func (c *QueryCursor) applyNativeOptionsLocked() {
	if c == nil || c.handle == 0 || c.runtime == nil {
		return
	}
	r := c.runtime
	if c.byteRangeSet {
		_, _, _ = r.call(r.Context(), []string{"tsw_query_cursor_set_byte_range", "wasitter_query_cursor_set_byte_range", "ts_query_cursor_set_byte_range", "query_cursor_set_byte_range"}, uint64(c.handle), uint64(c.byteStart), uint64(c.byteEnd))
	}
	if c.pointRangeSet {
		_, _, _ = r.call(r.Context(), []string{"tsw_query_cursor_set_point_range", "wasitter_query_cursor_set_point_range", "ts_query_cursor_set_point_range", "query_cursor_set_point_range"}, uint64(c.handle), packPoint(c.pointStart), packPoint(c.pointEnd))
	}
	if c.maxDepthSet {
		_, _, _ = r.call(r.Context(), []string{"tsw_query_cursor_set_max_start_depth", "wasitter_query_cursor_set_max_start_depth", "ts_query_cursor_set_max_start_depth", "query_cursor_set_max_start_depth"}, uint64(c.handle), uint64(c.maxDepth))
	}
	if c.matchLimitSet {
		_, _, _ = r.call(r.Context(), []string{"tsw_query_cursor_set_match_limit", "wasitter_query_cursor_set_match_limit", "ts_query_cursor_set_match_limit", "query_cursor_set_match_limit"}, uint64(c.handle), uint64(c.matchLimit))
	}
	if c.timeoutSet {
		_, _, _ = r.call(r.Context(), []string{"tsw_query_cursor_set_timeout_micros", "wasitter_query_cursor_set_timeout_micros", "ts_query_cursor_set_timeout_micros", "query_cursor_set_timeout_micros"}, uint64(c.handle), c.timeoutMicros)
	}
}

// deleteNativeHandleLocked releases a query cursor handle while retaining the
// cursor's option state.  The caller must hold c.mu.
func (c *QueryCursor) deleteNativeHandleLocked() {
	if c == nil {
		return
	}
	handle, r := c.handle, c.runtime
	scratchPtr := c.queryScratchPtr
	c.handle = 0
	c.queryScratchPtr = 0
	c.queryScratchCap = 0
	if r == nil || r.closedState() {
		return
	}
	if handle != 0 {
		_, _, _ = r.call(context.Background(), []string{"tsw_query_cursor_delete", "wasitter_query_cursor_delete", "ts_query_cursor_delete", "query_cursor_delete"}, uint64(handle))
	}
	if scratchPtr != 0 && !r.closedState() {
		r.mu.Lock()
		r.freeLocked(scratchPtr)
		r.mu.Unlock()
	}
}

// ensureQueryScratchLocked returns a reusable guest buffer with at least
// required bytes. The caller must hold both c.mu and c.runtime.mu. Query
// iterator records are size-probed, so retaining this buffer removes a guest
// allocation and deallocation from every subsequent record.
func (c *QueryCursor) ensureQueryScratchLocked(required uint32) (uint32, error) {
	if c == nil || required == 0 {
		return 0, ErrInvalidHandle
	}
	r := c.runtime
	if r == nil {
		return 0, ErrNoRuntime
	}
	if c.queryScratchPtr != 0 && c.queryScratchCap >= required {
		return c.queryScratchPtr, nil
	}
	// All query record parsers reject records larger than this bound. Grow
	// geometrically within that same bound so alternating capture counts do not
	// repeatedly allocate and free guest memory.
	const maxQueryScratch = uint32(64 << 20)
	capacity := c.queryScratchCap
	if capacity < 16 {
		capacity = 16
	}
	for capacity < required {
		if capacity > maxQueryScratch/2 {
			capacity = required
			break
		}
		capacity *= 2
	}
	if capacity > maxQueryScratch {
		capacity = required
	}
	ptr, err := r.allocLocked(capacity)
	if err != nil {
		return 0, err
	}
	oldPtr := c.queryScratchPtr
	c.queryScratchPtr = ptr
	c.queryScratchCap = capacity
	if oldPtr != 0 {
		r.freeLocked(oldPtr)
	}
	return ptr, nil
}

// prepareFallbackMatchesLocked materializes candidates for a native query
// whose bridge does not expose a usable next_match/next_capture operation.
// The caller must hold c.mu.  Query.matchesFallback intentionally supports
// this partial-native case by parsing the common compatibility subset from
// the original query source and preserving disabled captures/patterns.
func (c *QueryCursor) prepareFallbackMatchesLocked() {
	if c == nil || c.fallbackPrepared || c.query == nil || c.root.IsNull() {
		return
	}
	c.matches = c.filterFallbackMatches(c.query.matchesFallback(c.root))
	for i := range c.matches {
		c.matches[i].cursor = c
		c.matches[i].root = c.root
	}
	c.index = 0
	c.captureQueue = nil
	c.fallbackPrepared = true
}

// filterFallbackMatches applies the same coarse range/depth constraints that
// native TSQueryCursor applies.  It is only used by old bridge modules which
// don't expose the query-cursor ABI.
func (c *QueryCursor) filterFallbackMatches(matches []QueryMatch) []QueryMatch {
	if c == nil || len(matches) == 0 {
		return matches
	}
	if !c.byteRangeSet && !c.pointRangeSet && !c.maxDepthSet {
		return matches
	}
	filtered := make([]QueryMatch, 0, len(matches))
	for _, match := range matches {
		if c.fallbackMatchAllowedLocked(match) {
			filtered = append(filtered, match)
		}
	}
	return filtered
}

// fallbackMatchAllowedLocked applies the coarse range/depth constraints used
// by compatibility cursors.  It is deliberately evaluated at iteration time
// as well as during Exec: Tree-sitter permits callers to change a cursor's
// range after an execution has started, and a fallback cursor has already
// materialized its candidate matches in that case.  The caller must hold
// c.mu when invoking this helper (the fields it reads are cursor state).
func (c *QueryCursor) fallbackMatchAllowedLocked(match QueryMatch) bool {
	if c == nil {
		return false
	}
	if !c.byteRangeSet && !c.pointRangeSet && !c.maxDepthSet {
		return true
	}
	// The compatibility matcher records the node at which the pattern was
	// started. Use that anchor for depth checks and as an additional range
	// participant: Tree-sitter considers the complete match (including an
	// uncaptured pattern root), so looking only at child captures can wrongly
	// discard a match whose root intersects the requested range.
	anchor := match.anchor
	if anchor.IsNull() {
		anchor = match.root
	}
	if anchor.IsNull() {
		anchor = c.root
	}
	if len(match.Captures) == 0 {
		if c.byteRangeSet && !nodeIntersectsByteRange(anchor, c.byteStart, c.byteEnd) {
			return false
		}
		if c.pointRangeSet && !nodeIntersectsPointRange(anchor, c.pointStart, c.pointEnd) {
			return false
		}
		if c.maxDepthSet {
			if d, ok := nodeDepthFromRoot(anchor, c.root); !ok || d > c.maxDepth {
				return false
			}
		}
		return true
	}
	// A match intersects a range when any captured node intersects it. This
	// mirrors Tree-sitter's documented overlap semantics: the complete match
	// remains eligible even if another capture extends outside the range.
	if c.byteRangeSet {
		intersects := false
		if !anchor.IsNull() && nodeIntersectsByteRange(anchor, c.byteStart, c.byteEnd) {
			intersects = true
		}
		for _, capture := range match.Captures {
			if nodeIntersectsByteRange(capture.Node, c.byteStart, c.byteEnd) {
				intersects = true
				break
			}
		}
		if !intersects {
			return false
		}
	}
	if c.pointRangeSet {
		intersects := false
		if !anchor.IsNull() && nodeIntersectsPointRange(anchor, c.pointStart, c.pointEnd) {
			intersects = true
		}
		for _, capture := range match.Captures {
			if nodeIntersectsPointRange(capture.Node, c.pointStart, c.pointEnd) {
				intersects = true
				break
			}
		}
		if !intersects {
			return false
		}
	}
	if c.maxDepthSet {
		depthOK := false
		if !anchor.IsNull() {
			if d, ok := nodeDepthFromRoot(anchor, c.root); ok && d <= c.maxDepth {
				depthOK = true
			}
		}
		if !depthOK {
			return false
		}
	}
	return true
}

// fallbackCaptureAllowedLocked applies range/depth filtering to an
// individual capture.  Unlike match filtering, next_capture is specified to
// skip captures that lie outside the cursor range even when another capture
// in the same match intersects it.  The caller must hold c.mu.
func (c *QueryCursor) fallbackCaptureAllowedLocked(capture QueryCapture) bool {
	if c == nil || capture.Node.IsNull() {
		return false
	}
	// Capture iteration uses Tree-sitter's capture-level range predicate. It
	// differs subtly from the match-level test for zero-width nodes: a capture
	// whose empty point is exactly at the range start is considered preceding
	// the range (the native code uses `end <= start`), whereas a pattern root at
	// that point may still make the complete match eligible. Keep the stricter
	// helper here so NextCapture and QueryCaptures.Set* agree with C.
	if c.byteRangeSet && !nodeIntersectsCaptureByteRange(capture.Node, c.byteStart, c.byteEnd) {
		return false
	}
	if c.pointRangeSet && !nodeIntersectsCapturePointRange(capture.Node, c.pointStart, c.pointEnd) {
		return false
	}
	if c.maxDepthSet {
		if d, ok := nodeDepthFromRoot(capture.Node, c.root); !ok || d > c.maxDepth {
			return false
		}
	}
	return true
}

func nodeIntersectsByteRange(node Node, start, end uint32) bool {
	if node.IsNull() {
		return false
	}
	// Tree-sitter uses an end byte of zero as UINT32_MAX (the unbounded-end
	// sentinel), matching ts_query_cursor_set_byte_range.
	if end == 0 {
		end = ^uint32(0)
	}
	ns, ne := node.StartByte(), node.EndByte()
	if ns == ne {
		// Tree-sitter's match-level range test treats an empty node as
		// intersecting when its point is at or after the range start and
		// strictly before the range end.  In particular, a zero-width/missing
		// node at the range's start is included (the non-empty half-open test
		// below would otherwise reject it because ns == end for a one-byte
		// range).  The C cursor separately normalizes an end of zero to the
		// unbounded sentinel before reaching this check.
		return ns >= start && ns < end
	}
	return ns < end && ne > start
}

func nodeIntersectsPointRange(node Node, start, end Point) bool {
	if node.IsNull() {
		return false
	}
	// Likewise, (0,0) is the point-range unbounded-end sentinel in Tree-sitter.
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	ns, ne := node.StartPoint(), node.EndPoint()
	if ns == ne {
		return comparePoint(ns, start) >= 0 && comparePoint(ns, end) < 0
	}
	return comparePoint(ns, end) < 0 && comparePoint(ne, start) > 0
}

// nodeIntersectsCaptureByteRange mirrors the stricter range check used by
// ts_query_cursor_next_capture.  Match-level traversal intentionally treats a
// zero-width pattern root at the range start as intersecting, but captures use
// an end<=start exclusion and therefore omit that empty capture.
func nodeIntersectsCaptureByteRange(node Node, start, end uint32) bool {
	if node.IsNull() {
		return false
	}
	if end == 0 {
		end = ^uint32(0)
	}
	ns, ne := node.StartByte(), node.EndByte()
	if ns == ne {
		return ns > start && ns < end
	}
	return ns < end && ne > start
}

func nodeIntersectsCapturePointRange(node Node, start, end Point) bool {
	if node.IsNull() {
		return false
	}
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	ns, ne := node.StartPoint(), node.EndPoint()
	if ns == ne {
		return comparePoint(ns, start) > 0 && comparePoint(ns, end) < 0
	}
	return comparePoint(ns, end) < 0 && comparePoint(ne, start) > 0
}

func nodeDepthFromRoot(node, root Node) (uint32, bool) {
	if node.IsNull() || root.IsNull() {
		return 0, false
	}
	var depth uint32
	for current := node; !current.IsNull(); {
		if current.Equal(root) {
			return depth, true
		}
		parent := current.Parent()
		if parent.IsNull() {
			return 0, false
		}
		current, depth = parent, depth+1
	}
	return 0, false
}

// SetByteRange restricts matches to those intersecting the UTF-8 byte
// range [start, end). An end of zero means no upper bound. Invalid ranges are
// ignored; use [QueryCursor.SetByteRangeE] to report errors.
func (c *QueryCursor) SetByteRange(start, end uint32) *QueryCursor {
	if c == nil || c.closed.Load() {
		return c
	}
	start32, end32 := start, end
	// Tree-sitter reserves an end byte of zero for an unbounded range. Normalize
	// it in the host state before validating the ordering; otherwise a perfectly
	// valid range such as (12, 0) is incorrectly rejected as reversed.
	if end32 == 0 {
		end32 = ^uint32(0)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || start32 > end32 {
		return c
	}
	c.byteStart, c.byteEnd = start32, end32
	c.byteRangeSet = true
	if c.handle != 0 && c.runtime != nil {
		_, _, _ = c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_byte_range", "wasitter_query_cursor_set_byte_range", "ts_query_cursor_set_byte_range", "query_cursor_set_byte_range"}, uint64(c.handle), uint64(start32), uint64(end32))
	}
	return c
}

// SetByteRangeE applies a byte range to the cursor and reports conversion or
// guest ABI errors. The end value of zero retains Tree-sitter's unbounded-end
// sentinel semantics.
func (c *QueryCursor) SetByteRangeE(start, end uint32) error {
	if c == nil || c.closed.Load() {
		return ErrClosed
	}
	start32, end32 := start, end
	if end32 == 0 {
		end32 = ^uint32(0)
	}
	if start32 > end32 {
		return fmt.Errorf("wasitter: invalid byte range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	c.byteStart, c.byteEnd, c.byteRangeSet = start32, end32, true
	if c.handle == 0 || c.runtime == nil {
		return nil
	}
	result, name, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_byte_range", "wasitter_query_cursor_set_byte_range", "ts_query_cursor_set_byte_range", "query_cursor_set_byte_range"}, uint64(c.handle), uint64(start32), uint64(end32))
	if err != nil {
		if isUnsupported(err) {
			return nil
		}
		return err
	}
	if len(result) != 0 && result[0] == 0 {
		return &ABIError{Function: name, Message: "invalid byte range"}
	}
	return nil
}

// SetPointRange restricts future matches to the supplied row/column range and
// returns c for fluent configuration.
func (c *QueryCursor) SetPointRange(start, end Point) *QueryCursor {
	if c == nil || c.closed.Load() {
		return c
	}
	// Point{0,0} is the native unbounded-end sentinel. Normalize before the
	// ordering check for parity with ts_query_cursor_set_point_range.
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || comparePoint(start, end) > 0 {
		return c
	}
	c.pointStart, c.pointEnd = start, end
	c.pointRangeSet = true
	if c.handle != 0 && c.runtime != nil {
		_, _, _ = c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_point_range", "wasitter_query_cursor_set_point_range", "ts_query_cursor_set_point_range", "query_cursor_set_point_range"}, uint64(c.handle), packPoint(start), packPoint(end))
	}
	return c
}

// SetPointRangeE applies a point range and reports conversion or guest ABI
// errors.
func (c *QueryCursor) SetPointRangeE(start, end Point) error {
	if c == nil || c.closed.Load() {
		return ErrClosed
	}
	if end == (Point{}) {
		end = Point{Row: ^uint32(0), Column: ^uint32(0)}
	}
	if comparePoint(start, end) > 0 {
		return fmt.Errorf("wasitter: invalid point range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	c.pointStart, c.pointEnd, c.pointRangeSet = start, end, true
	if c.handle == 0 || c.runtime == nil {
		return nil
	}
	result, name, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_point_range", "wasitter_query_cursor_set_point_range", "ts_query_cursor_set_point_range", "query_cursor_set_point_range"}, uint64(c.handle), packPoint(start), packPoint(end))
	if err != nil {
		if isUnsupported(err) {
			return nil
		}
		return err
	}
	if len(result) != 0 && result[0] == 0 {
		return &ABIError{Function: name, Message: "invalid point range"}
	}
	return nil
}

// SetMatchLimit sets the maximum number of in-progress matches. Use
// [QueryCursor.DidExceedMatchLimit] to detect whether execution reached it.
// Use [QueryCursor.SetMatchLimitE] to report guest ABI errors.
func (c *QueryCursor) SetMatchLimit(limit uint32) *QueryCursor {
	if c == nil || c.closed.Load() {
		return c
	}
	limit32 := limit
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return c
	}
	c.matchLimit, c.matchLimitSet = limit32, true
	if c.runtime != nil && c.handle != 0 {
		_, _, _ = c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_match_limit", "wasitter_query_cursor_set_match_limit", "ts_query_cursor_set_match_limit", "query_cursor_set_match_limit"}, uint64(c.handle), uint64(limit32))
	}
	return c
}

// SetMatchLimitE is the error-reporting counterpart of SetMatchLimit.
func (c *QueryCursor) SetMatchLimitE(limit uint32) error {
	if c == nil || c.closed.Load() {
		return ErrClosed
	}
	limit32 := limit
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	c.matchLimit, c.matchLimitSet = limit32, true
	if c.runtime == nil || c.handle == 0 {
		return nil
	}
	_, _, err := c.runtime.call(c.runtime.Context(), []string{
		"tsw_query_cursor_set_match_limit",
		"wasitter_query_cursor_set_match_limit",
		"ts_query_cursor_set_match_limit",
		"query_cursor_set_match_limit",
	}, uint64(c.handle), uint64(limit32))
	if err != nil && !isUnsupported(err) {
		return err
	}
	return nil
}

// MatchLimit returns the maximum number of in-progress matches allowed by the
// cursor.
func (c *QueryCursor) MatchLimit() uint32 {
	if c == nil || c.closed.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return 0
	}
	if c.runtime != nil && c.handle != 0 {
		result, _, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_match_limit", "wasitter_query_cursor_match_limit", "ts_query_cursor_match_limit", "query_cursor_match_limit"}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			if value, ok := checkedU32(result[0]); ok {
				return value
			}
		}
	}
	return c.matchLimit
}

// DidExceedMatchLimit reports whether the most recent execution hit its match
// limit.
func (c *QueryCursor) DidExceedMatchLimit() bool {
	if c == nil || c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.runtime == nil || c.handle == 0 {
		return false
	}
	result, _, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_did_exceed_match_limit", "wasitter_query_cursor_did_exceed_match_limit", "ts_query_cursor_did_exceed_match_limit", "query_cursor_did_exceed_match_limit"}, uint64(c.handle))
	return err == nil && len(result) != 0 && result[0] != 0
}

// SetTimeoutMicros sets the execution timeout in microseconds and returns c
// for fluent configuration.
func (c *QueryCursor) SetTimeoutMicros(timeout uint64) *QueryCursor {
	if c == nil || c.closed.Load() {
		return c
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return c
	}
	c.timeoutMicros, c.timeoutSet = timeout, true
	if c.runtime != nil && c.handle != 0 {
		_, _, _ = c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_timeout_micros", "wasitter_query_cursor_set_timeout_micros", "ts_query_cursor_set_timeout_micros", "query_cursor_set_timeout_micros"}, uint64(c.handle), timeout)
	}
	return c
}

// TimeoutMicros returns the configured execution timeout in microseconds.
func (c *QueryCursor) TimeoutMicros() uint64 {
	if c == nil || c.closed.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return 0
	}
	if c.runtime != nil && c.handle != 0 {
		result, _, err := c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_timeout_micros", "wasitter_query_cursor_timeout_micros", "ts_query_cursor_timeout_micros", "query_cursor_timeout_micros"}, uint64(c.handle))
		if err == nil && len(result) != 0 {
			return result[0]
		}
	}
	return c.timeoutMicros
}

// SetMaxStartDepth limits the depth at which a query pattern can start.
// Zero restricts starts to the execution root. Use [QueryCursor.ClearMaxStartDepth]
// to remove the limit.
func (c *QueryCursor) SetMaxStartDepth(depth uint32) *QueryCursor {
	if c == nil || c.closed.Load() {
		return c
	}
	value := depth
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return c
	}
	c.maxDepth, c.maxDepthSet = value, true
	if c.runtime != nil && c.handle != 0 {
		_, _, _ = c.runtime.call(c.runtime.Context(), []string{"tsw_query_cursor_set_max_start_depth", "wasitter_query_cursor_set_max_start_depth", "ts_query_cursor_set_max_start_depth", "query_cursor_set_max_start_depth"}, uint64(c.handle), uint64(value))
	}
	c.mu.Unlock()
	return c
}

// ClearMaxStartDepth removes any previously configured maximum start depth.
func (c *QueryCursor) ClearMaxStartDepth() *QueryCursor {
	return c.SetMaxStartDepth(^uint32(0))
}

// RemoveMatch removes an in-progress match by id. It is the idiomatic
// error-free counterpart of RemoveMatchE; unsupported legacy cursors simply
// ignore the request.
func (c *QueryCursor) RemoveMatch(matchID uint32) {
	_ = c.RemoveMatchE(matchID)
}

// RemoveMatchE removes an in-progress match and reports lifecycle/ABI errors.
func (c *QueryCursor) RemoveMatchE(matchID uint32) error {
	if c == nil || c.closed.Load() {
		return ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	// A native query cursor retains references into the execution tree just
	// like NextMatch/NextCapture. Exec normally keeps that lifetime gate held
	// for the whole stream. For a defensive/legacy cursor that has a handle but
	// no retained gate, acquire a temporary one around this operation.
	var unlockTree func()
	if c.treeLifeUnlock == nil {
		unlockTree = lockTreeLifePair(c.root.tree, nil)
		defer unlockTree()
	}
	if c.root.tree != nil {
		if err := c.root.tree.ensureOpen(); err != nil {
			return err
		}
	}
	// Keep host-buffered captures in sync with the native cursor.  The
	// predicate-filtered NextCapture path materializes a complete match before
	// yielding its first capture; removing that match must discard captures
	// which are still queued locally as well as the native in-progress state.
	markRemoved := func() {
		if c.removedMatches == nil {
			c.removedMatches = make(map[uint32]struct{})
		}
		c.removedMatches[matchID] = struct{}{}
		if len(c.captureQueue) != 0 {
			filtered := c.captureQueue[:0]
			for _, queued := range c.captureQueue {
				if queued.matchID != matchID {
					filtered = append(filtered, queued)
				}
			}
			c.captureQueue = filtered
		}
	}
	if c.query != nil && c.query.native && c.handle != 0 && c.runtime != nil {
		c.query.mu.RLock()
		defer c.query.mu.RUnlock()
		if c.query.closed.Load() {
			return ErrClosed
		}
		_, _, err := c.runtime.call(c.runtime.Context(), []string{
			"tsw_query_cursor_remove_match",
			"wasitter_query_cursor_remove_match",
			"ts_query_cursor_remove_match",
			"query_cursor_remove_match",
		}, uint64(c.handle), uint64(matchID))
		if err != nil && !isUnsupported(err) {
			return err
		}
		if err == nil {
			markRemoved()
			return nil
		}
	}
	// Fallback cursors have materialized matches rather than native
	// in-progress state. Mark the id and discard any queued match with it.
	markRemoved()
	return nil
}

// NextMatch advances the active execution and returns its next complete match.
// The boolean is false when the execution is exhausted or unavailable.
func (c *QueryCursor) NextMatch() (matchResult QueryMatch, foundResult bool) {
	if c == nil || c.closed.Load() {
		return QueryMatch{}, false
	}
	if c.runStoredProgress() {
		// A callback retained by ExecWithOptions may cancel before the first
		// native step. Release the execution-tree lifetime gate so Tree.Close
		// is not blocked forever by a canceled cursor that the caller never
		// drains.
		c.mu.Lock()
		c.releaseTreeLifeLocked()
		c.mu.Unlock()
		return QueryMatch{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return QueryMatch{}, false
	}
	// Exec retains the tree gate while the native stream is active. A cursor
	// created by an older/partial implementation may lack that retention; use
	// a short-lived guard as a safety net for that case.
	var temporaryUnlock func()
	if c.treeLifeUnlock == nil && c.query != nil && c.root.tree != nil {
		temporaryUnlock = lockTreeLifePair(c.root.tree, nil)
		defer temporaryUnlock()
	}
	defer func() {
		if !foundResult {
			c.releaseTreeLifeLocked()
		}
	}()
	if c.query != nil && c.query.native && c.nativeMatchAvailable && c.handle != 0 && c.runtime != nil && c.root.tree != nil {
		if err := c.root.tree.ensureOpen(); err != nil {
			return QueryMatch{}, false
		}
		for {
			q := c.query
			q.mu.RLock()
			if q.closed.Load() {
				q.mu.RUnlock()
				return QueryMatch{}, false
			}
			raw, ok := c.nextNativeMatchLocked()
			q.mu.RUnlock()
			if !ok {
				return QueryMatch{}, false
			}
			// Build temporary value nodes for predicate evaluation.  They are not
			// registered until the match is accepted, which avoids both leaks and
			// double frees when a text predicate rejects a match.
			temporary := QueryMatch{PatternIndex: raw.match.PatternIndex, ID: raw.match.ID}
			for _, capture := range raw.captures {
				temporary.Captures = append(temporary.Captures, QueryCapture{Node: Node{tree: c.root.tree, handle: capture.node}, Index: capture.index})
			}
			if c.satisfiesQueryMatchLocked(c.query, temporary) {
				accepted := QueryMatch{PatternIndex: raw.match.PatternIndex, ID: raw.match.ID, cursor: c, root: c.root, Captures: make([]QueryCapture, 0, len(raw.captures))}
				for _, capture := range raw.captures {
					accepted.Captures = append(accepted.Captures, QueryCapture{Node: c.root.tree.registerNode(capture.node), Index: capture.index})
				}
				return accepted, true
			}
			// A rejected match still owns wrappers allocated in the guest.
			for _, capture := range raw.captures {
				if capture.node != 0 {
					_, _, _ = c.runtime.call(context.Background(), []string{"tsw_node_delete", "wasitter_node_delete", "node_delete"}, uint64(capture.node))
				}
			}
		}
	}
	if c.query != nil && c.query.native && !c.nativeMatchAvailable {
		// A compiler/exec-only bridge has no native match iterator. Build the
		// compatibility candidates lazily so Query.Matches and NextMatch remain
		// useful instead of reporting a false empty stream.
		c.prepareFallbackMatchesLocked()
	}
	for c.index < len(c.matches) {
		match := c.matches[c.index]
		c.index++
		if _, removed := c.removedMatches[match.ID]; removed {
			continue
		}
		// Compatibility cursors materialize candidates during Exec. Apply
		// mutable range/depth options again here so setters invoked after Exec
		// have the same effect as they do on a native cursor.
		if c.query == nil || !c.query.native || c.handle == 0 || !c.nativeMatchAvailable {
			if !c.fallbackMatchAllowedLocked(match) {
				continue
			}
		}
		if c.query != nil && !c.satisfiesQueryMatchLocked(c.query, match) {
			continue
		}
		match.cursor = c
		match.root = c.root
		return match, true
	}
	return QueryMatch{}, false
}

// satisfiesQueryMatchLocked evaluates built-in text predicates using the
// iterator's explicit source buffer when one was supplied.  The caller must
// hold c.mu; Query itself remains safe through its predicate metadata locks.
func (c *QueryCursor) satisfiesQueryMatchLocked(query *Query, match QueryMatch) bool {
	if query == nil {
		return true
	}
	if c != nil && c.predicateTextSet {
		return query.satisfiesWithTextBuffer(match, c.predicateText)
	}
	return query.satisfies(match)
}

type rawQueryMatch struct {
	match    QueryMatch
	captures []rawQueryCapture
}

// rawPredicateCaptureMatch is one item from the optional predicate-aware
// capture ABI.  Unlike next_match, the guest obtains the item from
// ts_query_cursor_next_capture, so the selected capture and its complete
// originating match retain Tree-sitter's native interleaving order.
type rawPredicateCaptureMatch struct {
	match          QueryMatch
	captures       []rawQueryCapture
	captureOrdinal uint32
}

type rawQueryCapture struct {
	id             uint32
	patternIndex   uint32
	captureOrdinal uint32
	node           uint32
	index          uint32
	captureIndex   uint32
}

func (c *QueryCursor) nextNativeMatchLocked() (rawQueryMatch, bool) {
	r := c.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, _, err := r.function("tsw_query_cursor_next_match", "wasitter_query_cursor_next_match", "ts_query_cursor_next_match", "query_cursor_next_match")
	if err != nil {
		return rawQueryMatch{}, false
	}
	first, callErr := fn.Call(r.Context(), uint64(c.handle), 0, 0)
	if callErr != nil || len(first) == 0 {
		return rawQueryMatch{}, false
	}
	required, requiredOK := checkedU32(first[0])
	if !requiredOK || required == 0 {
		return rawQueryMatch{}, false
	}
	if required < 8 || required > 64<<20 {
		return rawQueryMatch{}, false
	}
	ptr, allocErr := c.ensureQueryScratchLocked(required)
	if allocErr != nil {
		return rawQueryMatch{}, false
	}
	result, callErr := fn.Call(r.Context(), uint64(c.handle), uint64(ptr), uint64(required))
	if callErr != nil || len(result) == 0 {
		return rawQueryMatch{}, false
	}
	resultSize, resultOK := checkedU32(result[0])
	if !resultOK || resultSize < required {
		return rawQueryMatch{}, false
	}
	mem := r.mod.Memory()
	if mem == nil {
		return rawQueryMatch{}, false
	}
	b, ok := mem.Read(ptr, required)
	if !ok || len(b) < 8 {
		return rawQueryMatch{}, false
	}
	count := binary.LittleEndian.Uint16(b[6:8])
	if uint64(8)+uint64(count)*8 > uint64(len(b)) {
		return rawQueryMatch{}, false
	}
	raw := rawQueryMatch{match: QueryMatch{PatternIndex: uint32(binary.LittleEndian.Uint16(b[4:6])), ID: binary.LittleEndian.Uint32(b[:4])}, captures: make([]rawQueryCapture, int(count))}
	for i := uint16(0); i < count; i++ {
		offset := 8 + int(i)*8
		raw.captures[int(i)] = rawQueryCapture{node: binary.LittleEndian.Uint32(b[offset : offset+4]), index: binary.LittleEndian.Uint32(b[offset+4 : offset+8])}
	}
	return raw, true
}

// NextCapture advances the active execution and returns its next capture. The
// boolean is false when the execution is exhausted or unavailable.
func (c *QueryCursor) NextCapture() (captureResult QueryCapture, foundResult bool) {
	if c == nil || c.closed.Load() {
		return QueryCapture{}, false
	}
	if c.runStoredProgress() {
		c.mu.Lock()
		c.releaseTreeLifeLocked()
		c.mu.Unlock()
		return QueryCapture{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return QueryCapture{}, false
	}
	var temporaryUnlock func()
	if c.treeLifeUnlock == nil && c.query != nil && c.root.tree != nil {
		temporaryUnlock = lockTreeLifePair(c.root.tree, nil)
		defer temporaryUnlock()
	}
	defer func() {
		if !foundResult {
			c.releaseTreeLifeLocked()
		}
	}()
	if c.query != nil && c.query.native && (c.nativeCaptureAvailable || c.nativeMatchAvailable) && c.handle != 0 && c.runtime != nil && c.root.tree != nil {
		if err := c.root.tree.ensureOpen(); err != nil {
			return QueryCapture{}, false
		}
		// When no host-side text predicates need filtering, use Tree-sitter's
		// native capture iterator directly unless the slice-shaped Captures API
		// requested full originating matches (captureIteratorFullMatch). Unlike
		// iterating next_match and flattening each match, this preserves the
		// global source-order semantics for overlapping/nested matches (for
		// example a parent capture can be followed by another parent before
		// either child's capture). Queries with built-in text predicates, and
		// full-match capture iteration, continue through the path below.
		if !c.query.hasTextPredicates() && c.nativeCaptureSupported() && !c.captureIteratorFullMatch {
			q := c.query
			q.mu.RLock()
			if q.closed.Load() {
				q.mu.RUnlock()
				return QueryCapture{}, false
			}
			raw, ok := c.nextNativeCaptureLocked()
			q.mu.RUnlock()
			if !ok {
				return QueryCapture{}, false
			}
			if raw.node == 0 {
				return QueryCapture{}, false
			}
			node := c.root.tree.registerNode(raw.node)
			if node.IsNull() {
				return QueryCapture{}, false
			}
			c.lastCaptureID = raw.id
			c.lastCapturePattern = raw.patternIndex
			c.lastCaptureOrdinal = raw.captureOrdinal
			c.lastCaptureValid = true
			return QueryCapture{Node: node, Index: raw.captureIndex, matchID: raw.id, patternIndex: raw.patternIndex, ordinal: raw.captureOrdinal}, true
		}
		// Built-in text predicates are evaluated by the Go host, but the
		// cursor must still expose captures in Tree-sitter's native
		// next_capture order.  The optional full-match ABI supplies the
		// complete originating match for each native capture event, allowing
		// us to filter without flattening next_match results (which can move a
		// parent capture ahead of an earlier sibling capture).
		if (c.query.hasTextPredicates() || c.captureIteratorFullMatch) && c.nativePredicateCaptureSupported() {
			return c.nextPredicateCaptureNativeLocked()
		}
		if len(c.captureQueue) == 0 {
			for {
				q := c.query
				q.mu.RLock()
				if q.closed.Load() {
					q.mu.RUnlock()
					return QueryCapture{}, false
				}
				raw, ok := c.nextNativeMatchLocked()
				q.mu.RUnlock()
				if !ok {
					return QueryCapture{}, false
				}
				match := QueryMatch{PatternIndex: raw.match.PatternIndex, ID: raw.match.ID}
				for _, capture := range raw.captures {
					match.Captures = append(match.Captures, QueryCapture{Node: Node{tree: c.root.tree, handle: capture.node}, Index: capture.index})
				}
				if c.satisfiesQueryMatchLocked(c.query, match) && len(raw.captures) != 0 {
					accepted := make([]queuedQueryCapture, 0, len(raw.captures))
					var fullMatch *QueryMatch
					if c.captureIteratorFullMatch {
						fullMatch = &QueryMatch{
							PatternIndex: raw.match.PatternIndex,
							ID:           raw.match.ID,
							cursor:       c,
							root:         c.root,
							Captures:     make([]QueryCapture, 0, len(raw.captures)),
						}
						registered := 0
						for _, capture := range raw.captures {
							wrapped := c.root.tree.registerNode(capture.node)
							if wrapped.IsNull() {
								fullMatch = nil
								break
							}
							fullMatch.Captures = append(fullMatch.Captures, QueryCapture{Node: wrapped, Index: capture.index})
							registered++
						}
						if fullMatch == nil {
							// The wrappers already registered above belong to the
							// tree registry. Only the unprocessed guest wrappers
							// need immediate cleanup; freeing the registered ones
							// here would make Tree.Close double-free them.
							for i := registered; i < len(raw.captures); i++ {
								if raw.captures[i].node != 0 {
									_, _, _ = c.runtime.call(context.Background(), []string{
										"tsw_node_delete", "wasitter_node_delete", "node_delete",
									}, uint64(raw.captures[i].node))
								}
							}
						}
					}
					if c.captureIteratorFullMatch && fullMatch == nil {
						// The tree closed while wrappers were being registered.
						// Unprocessed wrappers were reclaimed above; registered
						// wrappers remain owned by the tree and are reclaimed by its
						// normal close path.
						continue
					}
					registrationFailed := false
					for ordinal, capture := range raw.captures {
						var value QueryCapture
						if fullMatch != nil {
							value = fullMatch.Captures[ordinal]
							value.match = fullMatch
						} else {
							wrapped := c.root.tree.registerNode(capture.node)
							if wrapped.IsNull() {
								registrationFailed = true
								break
							}
							value = QueryCapture{Node: wrapped, Index: capture.index}
						}
						accepted = append(accepted, queuedQueryCapture{
							capture:        value,
							matchID:        raw.match.ID,
							patternIndex:   raw.match.PatternIndex,
							captureOrdinal: uint32(ordinal),
						})
					}
					if registrationFailed {
						// registerNode may already have reclaimed the wrapper that
						// failed. Reclaim every remaining guest wrapper exactly once;
						// wrappers successfully registered before the failure remain in
						// the tree registry and will be released by Tree.Close.
						// If tree shutdown caused registration to fail, the failed
						// wrapper has already been reclaimed (or the module is gone),
						// so avoid issuing a second free into the guest.
						if !c.root.tree.closed.Load() && !c.runtime.closedState() {
							for i := len(accepted); i < len(raw.captures); i++ {
								if raw.captures[i].node != 0 {
									_, _, _ = c.runtime.call(context.Background(), []string{
										"tsw_node_delete", "wasitter_node_delete", "node_delete",
									}, uint64(raw.captures[i].node))
								}
							}
						}
						return QueryCapture{}, false
					}
					c.captureQueue = append(c.captureQueue, accepted...)
					// A native match can legitimately have zero captures (for
					// example, a quantified pattern). Keep searching until a
					// capture is available instead of indexing an empty queue.
					if len(c.captureQueue) != 0 {
						break
					}
					// The accepted match had no usable captures (for example, a
					// defensive bridge response). Continue draining without
					// attempting to free wrappers that were not allocated.
					continue
				}
				// A rejected match still owns all wrappers returned by the guest.
				// Keep this cleanup strictly on the rejection path; accepted
				// wrappers are registered with the owning tree and must live until
				// Tree.Close.
				for _, capture := range raw.captures {
					if capture.node != 0 {
						_, _, _ = c.runtime.call(context.Background(), []string{"tsw_node_delete", "wasitter_node_delete", "node_delete"}, uint64(capture.node))
					}
				}
			}
		}
		queued := c.captureQueue[0]
		c.captureQueue = c.captureQueue[1:]
		c.lastCaptureID = queued.matchID
		c.lastCapturePattern = queued.patternIndex
		c.lastCaptureOrdinal = queued.captureOrdinal
		c.lastCaptureValid = true
		queued.capture.matchID = queued.matchID
		queued.capture.patternIndex = queued.patternIndex
		queued.capture.ordinal = queued.captureOrdinal
		return queued.capture, true
	}
	if c.query != nil && c.query.native && !c.nativeCaptureAvailable && !c.nativeMatchAvailable {
		// Neither native iterator is available on this partial bridge. Fall
		// back to the host matcher before flattening captures.
		c.prepareFallbackMatchesLocked()
	}
	// Compatibility cursors materialize complete matches. Flatten one match at
	// a time so repeated captures are all returned, while preserving match
	// order and honoring RemoveMatch calls for queued matches.
	for {
		if len(c.captureQueue) != 0 {
			queued := c.captureQueue[0]
			c.captureQueue = c.captureQueue[1:]
			// A fallback queue may have been populated before the caller changed
			// the cursor range. Drop captures that no longer intersect, matching
			// ts_query_cursor_next_capture's per-capture range behavior. Native
			// queues can also arise on partial bridges, so apply the same guard
			// whenever no live native cursor is available.
			if c.query == nil || !c.query.native || c.handle == 0 {
				if !c.fallbackCaptureAllowedLocked(queued.capture) {
					continue
				}
			}
			c.lastCaptureID = queued.matchID
			c.lastCapturePattern = queued.patternIndex
			c.lastCaptureOrdinal = queued.captureOrdinal
			c.lastCaptureValid = true
			queued.capture.matchID = queued.matchID
			queued.capture.patternIndex = queued.patternIndex
			queued.capture.ordinal = queued.captureOrdinal
			return queued.capture, true
		}
		if c.index >= len(c.matches) {
			break
		}
		match := c.matches[c.index]
		c.index++
		if _, removed := c.removedMatches[match.ID]; removed {
			continue
		}
		// The fallback execution path materializes candidates without the
		// native query engine's predicate evaluation. Apply built-in text
		// predicates before flattening a match into captures, just as
		// NextMatch does. Without this check a legacy cursor's Captures API
		// would return values rejected by Query.Matches/NextMatch.
		if c.query != nil && !c.satisfiesQueryMatchLocked(c.query, match) {
			continue
		}
		fullMatch := &match
		for ordinal, capture := range match.Captures {
			if !c.fallbackCaptureAllowedLocked(capture) {
				continue
			}
			capture.match = fullMatch
			capture.matchID = match.ID
			capture.patternIndex = match.PatternIndex
			capture.ordinal = uint32(ordinal)
			c.captureQueue = append(c.captureQueue, queuedQueryCapture{capture: capture, matchID: match.ID, patternIndex: match.PatternIndex, captureOrdinal: uint32(ordinal)})
		}
	}
	return QueryCapture{}, false
}

func (q *Query) hasTextPredicates() bool {
	if q == nil {
		return false
	}
	if len(q.predicates) != 0 {
		return true
	}
	for _, predicates := range q.TextPredicates {
		if len(predicates) != 0 {
			return true
		}
	}
	return false
}

func (c *QueryCursor) nativeCaptureSupported() bool {
	if c == nil || c.runtime == nil || c.runtime.closedState() {
		return false
	}
	r := c.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _, err := r.function(
		"tsw_query_cursor_next_capture",
		"wasitter_query_cursor_next_capture",
		"ts_query_cursor_next_capture",
		"query_cursor_next_capture",
	)
	return err == nil
}

func (c *QueryCursor) nextNativeCaptureLocked() (rawQueryCapture, bool) {
	r := c.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, _, err := r.function("tsw_query_cursor_next_capture", "wasitter_query_cursor_next_capture", "ts_query_cursor_next_capture", "query_cursor_next_capture")
	if err != nil {
		return rawQueryCapture{}, false
	}
	ptr, allocErr := c.ensureQueryScratchLocked(16)
	if allocErr != nil {
		return rawQueryCapture{}, false
	}
	result, callErr := fn.Call(r.Context(), uint64(c.handle), uint64(ptr), 16)
	if callErr != nil || len(result) == 0 {
		return rawQueryCapture{}, false
	}
	resultSize, resultOK := checkedU32(result[0])
	if !resultOK || resultSize < 16 {
		return rawQueryCapture{}, false
	}
	mem := r.mod.Memory()
	if mem == nil {
		return rawQueryCapture{}, false
	}
	b, ok := mem.Read(ptr, 16)
	if !ok || len(b) < 16 {
		return rawQueryCapture{}, false
	}
	return rawQueryCapture{
		id:             binary.LittleEndian.Uint32(b[0:4]),
		patternIndex:   uint32(binary.LittleEndian.Uint16(b[4:6])),
		captureOrdinal: uint32(binary.LittleEndian.Uint16(b[6:8])),
		node:           binary.LittleEndian.Uint32(b[8:12]),
		captureIndex:   binary.LittleEndian.Uint32(b[12:16]),
	}, true
}

// nativePredicateCaptureSupported reports whether the guest exposes the
// optional full-match capture operation. It only resolves the function name;
// unlike a probe call it never advances cursor state.
func (c *QueryCursor) nativePredicateCaptureSupported() bool {
	if c == nil || c.runtime == nil || c.runtime.closedState() {
		return false
	}
	r := c.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _, err := r.function(
		"tsw_query_cursor_next_capture_match",
		"wasitter_query_cursor_next_capture_match",
		"ts_query_cursor_next_capture_match",
		"query_cursor_next_capture_match",
	)
	return err == nil
}

// nextNativePredicateCaptureLocked obtains one native next_capture event and
// the complete capture list for its originating match. The caller must hold
// c.mu; Runtime.mu is acquired internally while invoking the guest.
func (c *QueryCursor) nextNativePredicateCaptureLocked() (rawPredicateCaptureMatch, bool) {
	r := c.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, fnName, err := r.function(
		"tsw_query_cursor_next_capture_match",
		"wasitter_query_cursor_next_capture_match",
		"ts_query_cursor_next_capture_match",
		"query_cursor_next_capture_match",
	)
	if err != nil {
		return rawPredicateCaptureMatch{}, false
	}
	probe, callErr := fn.Call(r.Context(), uint64(c.handle), 0, 0)
	if callErr != nil || len(probe) == 0 {
		return rawPredicateCaptureMatch{}, false
	}
	required, requiredOK := checkedU32(probe[0])
	if !requiredOK || required == 0 {
		return rawPredicateCaptureMatch{}, false
	}
	if required < 16 || required > 64<<20 {
		return rawPredicateCaptureMatch{}, false
	}
	ptr, allocErr := c.ensureQueryScratchLocked(required)
	if allocErr != nil {
		return rawPredicateCaptureMatch{}, false
	}
	result, callErr := fn.Call(r.Context(), uint64(c.handle), uint64(ptr), uint64(required))
	if callErr != nil || len(result) == 0 {
		return rawPredicateCaptureMatch{}, false
	}
	resultSize, resultOK := checkedU32(result[0])
	if !resultOK || resultSize < required {
		return rawPredicateCaptureMatch{}, false
	}
	mem := r.mod.Memory()
	if mem == nil {
		return rawPredicateCaptureMatch{}, false
	}
	b, ok := mem.Read(ptr, required)
	if !ok || len(b) < 16 {
		return rawPredicateCaptureMatch{}, false
	}
	count := binary.LittleEndian.Uint16(b[6:8])
	ordinal := binary.LittleEndian.Uint32(b[8:12])
	needed := uint64(16) + uint64(count)*8
	if needed > uint64(len(b)) || ordinal >= uint32(count) {
		_ = fnName // retain the resolved name for parity with ABI diagnostics
		return rawPredicateCaptureMatch{}, false
	}
	raw := rawPredicateCaptureMatch{
		match: QueryMatch{
			PatternIndex: uint32(binary.LittleEndian.Uint16(b[4:6])),
			ID:           binary.LittleEndian.Uint32(b[:4]),
		},
		captureOrdinal: ordinal,
		captures:       make([]rawQueryCapture, int(count)),
	}
	for i := uint16(0); i < count; i++ {
		offset := 16 + int(i)*8
		raw.captures[int(i)] = rawQueryCapture{
			node:  binary.LittleEndian.Uint32(b[offset : offset+4]),
			index: binary.LittleEndian.Uint32(b[offset+4 : offset+8]),
		}
	}
	return raw, true
}

// nextPredicateCaptureNativeLocked returns the next accepted capture while
// preserving the native query cursor's interleaving. The caller must hold
// c.mu and the execution tree's lifetime read lock (as NextCapture does).
func (c *QueryCursor) nextPredicateCaptureNativeLocked() (QueryCapture, bool) {
	for {
		q := c.query
		q.mu.RLock()
		if q.closed.Load() {
			q.mu.RUnlock()
			return QueryCapture{}, false
		}
		raw, ok := c.nextNativePredicateCaptureLocked()
		q.mu.RUnlock()
		if !ok {
			return QueryCapture{}, false
		}
		if len(raw.captures) == 0 {
			// Native next_capture normally never yields a zero-capture match,
			// but tolerate a defensive bridge response and continue draining.
			continue
		}

		temporary := QueryMatch{
			PatternIndex: raw.match.PatternIndex,
			ID:           raw.match.ID,
			Captures:     make([]QueryCapture, 0, len(raw.captures)),
		}
		for _, capture := range raw.captures {
			temporary.Captures = append(temporary.Captures, QueryCapture{
				Node:  Node{tree: c.root.tree, handle: capture.node},
				Index: capture.index,
			})
		}
		accepted := c.satisfiesQueryMatchLocked(q, temporary)
		selectedIndex := raw.captureOrdinal
		if selectedIndex >= uint32(len(raw.captures)) {
			// The ABI parser already checks this; keep the host-side guard so a
			// malformed custom module cannot panic the caller.
			for _, capture := range raw.captures {
				if capture.node != 0 {
					_, _, _ = c.runtime.call(context.Background(), []string{
						"tsw_node_delete", "wasitter_node_delete", "node_delete",
					}, uint64(capture.node))
				}
			}
			continue
		}

		// The bridge allocates one independent node wrapper per capture in the
		// returned full match. Retain only the selected wrapper and reclaim the
		// rest immediately; this keeps the owning Tree's node registry bounded
		// even for a large capture stream.
		if accepted {
			selected := raw.captures[selectedIndex]
			if c.captureIteratorFullMatch {
				// Captures() needs the complete match, just like upstream's
				// QueryCaptures iterator. Register each wrapper for the duration of
				// the owning tree rather than discarding the non-selected captures.
				eventMatch := &QueryMatch{
					PatternIndex: raw.match.PatternIndex,
					ID:           raw.match.ID,
					Captures:     make([]QueryCapture, 0, len(raw.captures)),
					cursor:       c,
				}
				for _, capture := range raw.captures {
					wrapped := c.root.tree.registerNode(capture.node)
					if wrapped.IsNull() {
						// registerNode reclaims a wrapper when the tree is closing;
						// stop cleanly instead of returning a partially valid match.
						return QueryCapture{}, false
					}
					eventMatch.Captures = append(eventMatch.Captures, QueryCapture{Node: wrapped, Index: capture.index})
				}
				selectedNode := eventMatch.Captures[selectedIndex].Node
				c.lastCaptureID = raw.match.ID
				c.lastCapturePattern = raw.match.PatternIndex
				c.lastCaptureOrdinal = selectedIndex
				c.lastCaptureValid = true
				return QueryCapture{
					Node:         selectedNode,
					Index:        selected.index,
					match:        eventMatch,
					matchID:      raw.match.ID,
					patternIndex: raw.match.PatternIndex,
					ordinal:      selectedIndex,
				}, true
			}
			node := c.root.tree.registerNode(selected.node)
			for i, capture := range raw.captures {
				if uint32(i) == selectedIndex || capture.node == 0 {
					continue
				}
				_, _, _ = c.runtime.call(context.Background(), []string{
					"tsw_node_delete", "wasitter_node_delete", "node_delete",
				}, uint64(capture.node))
			}
			if node.IsNull() {
				return QueryCapture{}, false
			}
			// The compact return value carries the selected capture, while the
			// private match fields let QueryCaptures.Next expose its native id,
			// pattern, and ordinal.
			eventCaptures := make([]QueryCapture, int(selectedIndex)+1)
			eventCaptures[selectedIndex] = QueryCapture{Node: node, Index: selected.index}
			eventMatch := &QueryMatch{
				PatternIndex: raw.match.PatternIndex,
				ID:           raw.match.ID,
				Captures:     eventCaptures,
				cursor:       c,
			}
			c.lastCaptureID = raw.match.ID
			c.lastCapturePattern = raw.match.PatternIndex
			c.lastCaptureOrdinal = selectedIndex
			c.lastCaptureValid = true
			return QueryCapture{Node: node, Index: selected.index, match: eventMatch, matchID: raw.match.ID, patternIndex: raw.match.PatternIndex, ordinal: selectedIndex}, true
		}

		for _, capture := range raw.captures {
			if capture.node != 0 {
				_, _, _ = c.runtime.call(context.Background(), []string{
					"tsw_node_delete", "wasitter_node_delete", "node_delete",
				}, uint64(capture.node))
			}
		}

		// Match rejection must remove the native in-progress state, exactly as
		// go-tree-sitter's QueryCaptures iterator does. Keep the query read lock
		// across the call so Query.Close cannot delete its handle concurrently.
		q.mu.RLock()
		if q.closed.Load() {
			q.mu.RUnlock()
			return QueryCapture{}, false
		}
		_, _, removeErr := c.runtime.call(c.runtime.Context(), []string{
			"tsw_query_cursor_remove_match",
			"wasitter_query_cursor_remove_match",
			"ts_query_cursor_remove_match",
			"query_cursor_remove_match",
		}, uint64(c.handle), uint64(raw.match.ID))
		q.mu.RUnlock()
		if removeErr != nil && !isUnsupported(removeErr) {
			return QueryCapture{}, false
		}
	}
}

// Close releases the guest cursor handle and any retained execution state. It
// is safe to call Close more than once.
func (c *QueryCursor) Close() error {
	if c == nil || c.closed.Swap(true) {
		return nil
	}
	runtimepkg.SetFinalizer(c, nil)
	c.mu.Lock()
	handle, r := c.handle, c.runtime
	scratchPtr := c.queryScratchPtr
	c.handle = 0
	c.queryScratchPtr = 0
	c.queryScratchCap = 0
	// Exec retains the execution tree's lifetime gate until the native stream
	// is drained. Delete the native cursor while that gate is still held: the C
	// destructor may inspect pointers into the execution tree. Only after the
	// destructor returns is it safe to let Tree.Close acquire its write lock.
	var closeErr error
	if handle != 0 && r != nil && !r.closedState() {
		_, _, err := r.call(context.Background(), []string{"tsw_query_cursor_delete", "wasitter_query_cursor_delete", "ts_query_cursor_delete", "query_cursor_delete"}, uint64(handle))
		if err != nil && !isUnsupported(err) && !errors.Is(err, ErrClosed) {
			closeErr = err
		}
	}
	if scratchPtr != 0 && r != nil && !r.closedState() {
		r.mu.Lock()
		r.freeLocked(scratchPtr)
		r.mu.Unlock()
	}
	// Release the retained tree gate after native destruction. A cursor created
	// by a legacy/partial bridge may not have retained one; in that case there
	// is nothing to do.
	c.releaseTreeLifeLocked()
	c.matches = nil
	c.captureQueue = nil
	c.lastCaptureID, c.lastCapturePattern, c.lastCaptureOrdinal, c.lastCaptureValid = 0, 0, 0, false
	c.captureIteratorFullMatch = false
	c.predicateText = nil
	c.predicateTextSet = false
	c.progressCallback = nil
	c.removedMatches = nil
	c.query, c.root, c.runtime = nil, Node{}, nil
	c.predicateCapturesPrepared = false
	c.mu.Unlock()
	return closeErr
}

// lastErrorLocked reads the optional bridge diagnostic string.  The caller
// must hold Runtime.mu because the pointer is borrowed guest memory.
func (r *Runtime) lastErrorLocked() string {
	if r == nil || r.mod == nil {
		return ""
	}
	ptrFn, _, err := r.function("tsw_last_error_ptr", "wasitter_last_error_ptr", "last_error_ptr")
	if err != nil {
		return ""
	}
	lenFn, _, err := r.function("tsw_last_error_len", "wasitter_last_error_len", "last_error_len")
	if err != nil {
		return ""
	}
	ptrResult, ptrErr := ptrFn.Call(r.Context())
	lenResult, lenErr := lenFn.Call(r.Context())
	if ptrErr != nil || lenErr != nil || len(ptrResult) == 0 || len(lenResult) == 0 {
		return ""
	}
	ptr, ptrOK := checkedU32(ptrResult[0])
	length, lengthOK := checkedU32(lenResult[0])
	if !ptrOK || !lengthOK {
		return ""
	}
	if ptr == 0 || length == 0 {
		return ""
	}
	mem := r.mod.Memory()
	if mem == nil {
		return ""
	}
	b, ok := mem.Read(ptr, length)
	if !ok {
		return ""
	}
	return string(b)
}
