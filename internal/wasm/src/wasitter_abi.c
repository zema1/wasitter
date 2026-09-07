#include "wasitter_abi.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "tree_sitter/api.h"

/* A grammar build may override this with -DWASITTER_LANGUAGE_FN=foo. */
#ifndef WASITTER_LANGUAGE_FN
#define WASITTER_LANGUAGE_FN tree_sitter_json
#endif

#ifndef WASITTER_LANGUAGE_NAME
#define WASITTER_LANGUAGE_NAME "json"
#endif

extern const TSLanguage *WASITTER_LANGUAGE_FN(void);

/* WASI's startup object expects a main symbol even though the module is
 * consumed as a library.  Keeping this tiny entry point also makes the
 * artifact usable with runtimes that insist on an exported _start. */
int main(void) { return 0; }

typedef struct {
  TSNode value;
} SWNode;

/* TSTreeCursor contains pointers and is not ABI-safe to pass by value from a
 * wasm host.  Keep it behind an opaque allocation just like SWNode. */
typedef struct {
  TSTreeCursor value;
} SWCursor;

/* Query cursors also carry a small amount of host-side state.  In particular,
 * retaining a pending match lets the Go host first ask for the required output
 * size and then retry without losing that match. */
typedef struct {
  TSQueryCursor *value;
  TSQueryMatch pending_match;
  TSQueryCapture *pending_captures;
  uint32_t pending_capture_count;
  uint32_t pending_capture_index;
  bool has_pending_match;
  /*
   * A query cursor has two native iteration APIs (next_match and
   * next_capture).  The host ABI needs a size probe for both APIs, so a
   * capture returned by the native next_capture call may have to be retained
   * until the caller supplies an output buffer.  `pending_is_capture` tells
   * the state machine whether pending_match was obtained from next_capture
   * (true) or next_match (false).
   */
  bool pending_is_capture;
} SWQueryCursor;

static void tsw_query_cursor_clear_pending(SWQueryCursor *wrapper);

typedef struct {
  uint32_t start_byte;
  uint32_t old_end_byte;
  uint32_t new_end_byte;
  TSPoint start_point;
  TSPoint old_end_point;
  TSPoint new_end_point;
} SWInputEdit;

typedef struct {
  uint32_t start_byte;
  uint32_t end_byte;
  TSPoint start_point;
  TSPoint end_point;
} SWRange;

static char tsw_error[192];
static char *tsw_sexp_buffer;
static size_t tsw_sexp_capacity;

static void tsw_set_error(const char *message) {
  if (!message) message = "unknown error";
  size_t n = strlen(message);
  if (n >= sizeof(tsw_error)) n = sizeof(tsw_error) - 1;
  memcpy(tsw_error, message, n);
  tsw_error[n] = '\0';
}

static void tsw_ok(void) { tsw_error[0] = '\0'; }

static void tsw_write_u16(uint8_t *dst, uint16_t value) {
  dst[0] = (uint8_t)(value & 0xffu);
  dst[1] = (uint8_t)(value >> 8);
}

static void tsw_write_u32(uint8_t *dst, uint32_t value) {
  dst[0] = (uint8_t)(value & 0xffu);
  dst[1] = (uint8_t)((value >> 8) & 0xffu);
  dst[2] = (uint8_t)((value >> 16) & 0xffu);
  dst[3] = (uint8_t)(value >> 24);
}

static uint32_t tsw_read_u32(const uint8_t *src) {
  return ((uint32_t)src[0]) |
         ((uint32_t)src[1] << 8) |
         ((uint32_t)src[2] << 16) |
         ((uint32_t)src[3] << 24);
}

/* Validate a guest pointer before converting it to a C pointer.  Every pointer
 * crossing this ABI is a wasm32 linear-memory offset, not a native host
 * pointer.  Checking both arithmetic overflow and the current linear-memory
 * extent turns malformed calls into a regular ABI error instead of a wasm
 * out-of-bounds trap.  The memory-size builtin is available in the wasm32
 * builds used by the build script; native builds retain the arithmetic check
 * so the C shim can still be compiled for smoke tests. */
static bool tsw_guest_range_fits(uint32_t ptr, uint64_t length) {
  if (length > ((uint64_t)UINT32_MAX + 1u) - (uint64_t)ptr) return false;
#if defined(__wasm32__) || defined(__wasm32)
  uint64_t memory_bytes = (uint64_t)__builtin_wasm_memory_size(0) * 65536u;
  if ((uint64_t)ptr > memory_bytes || length > memory_bytes - (uint64_t)ptr) {
    return false;
  }
#endif
  return true;
}

static bool tsw_require_guest_range(uint32_t ptr, uint64_t length, const char *message) {
  /* A null pointer is only valid for an empty range (used by size probes). */
  if ((ptr == 0 && length != 0) || !tsw_guest_range_fits(ptr, length)) {
    tsw_set_error(message ? message : "guest memory range is invalid");
    return false;
  }
  return true;
}

/* Return the length of a guest NUL-terminated string without reading past the
 * current linear-memory extent.  Accessors that return borrowed C strings are
 * expected to provide a terminator; malformed guest pointers are reported as
 * an ABI error rather than allowing strlen to run into an unmapped page. */
static bool tsw_guest_cstr_len(uint32_t ptr, uint32_t *out_length) {
  if (!ptr || !out_length) return false;
#if defined(__wasm32__) || defined(__wasm32)
  uint64_t memory_bytes = (uint64_t)__builtin_wasm_memory_size(0) * 65536u;
  if ((uint64_t)ptr >= memory_bytes) return false;
  size_t available = (size_t)(memory_bytes - (uint64_t)ptr);
  const char *source = (const char *)(uintptr_t)ptr;
  const char *end = (const char *)memchr(source, '\0', available);
  if (!end) return false;
  uint64_t length = (uint64_t)(end - source);
  if (length > UINT32_MAX) return false;
  *out_length = (uint32_t)length;
  return true;
#else
  *out_length = (uint32_t)strlen((const char *)(uintptr_t)ptr);
  return true;
#endif
}

/* Check a count/element-size product before converting it to size_t.  On
 * wasm32 this rejects products that would wrap the allocator size; on native
 * 64-bit builds every uint32_t count fits, so avoid a compiler "always false"
 * diagnostic while retaining the same behavior. */
static bool tsw_size_product_fits(uint64_t count, size_t element_size) {
#if SIZE_MAX < UINT64_MAX
  return count <= (uint64_t)(SIZE_MAX / element_size);
#else
  (void)count;
  (void)element_size;
  return true;
#endif
}

static bool tsw_size_header_product_fits(
  uint64_t count, size_t header_size, size_t element_size
) {
#if SIZE_MAX < UINT64_MAX
  return count <= (uint64_t)((SIZE_MAX - header_size) / element_size);
#else
  (void)count;
  (void)header_size;
  (void)element_size;
  return true;
#endif
}

static inline TSParser *tsw_parser(uint32_t handle) {
  return (TSParser *)(uintptr_t)handle;
}

static inline TSTree *tsw_tree(uint32_t handle) {
  return (TSTree *)(uintptr_t)handle;
}

static inline const TSLanguage *tsw_lang(uint32_t handle) {
  return (const TSLanguage *)(uintptr_t)handle;
}

static inline SWNode *tsw_node(uint32_t handle) {
  return (SWNode *)(uintptr_t)handle;
}

static inline SWCursor *tsw_cursor(uint32_t handle) {
  return (SWCursor *)(uintptr_t)handle;
}

static inline SWQueryCursor *tsw_query_cursor(uint32_t handle) {
  return (SWQueryCursor *)(uintptr_t)handle;
}

static uint32_t tsw_wrap_node(TSNode value) {
  if (ts_node_is_null(value)) return 0;
  SWNode *result = (SWNode *)malloc(sizeof(*result));
  if (!result) {
    tsw_set_error("out of memory");
    return 0;
  }
  result->value = value;
  return (uint32_t)(uintptr_t)result;
}

static TSNode tsw_unwrap_node(uint32_t handle) {
  if (!handle) return (TSNode){{0, 0, 0, 0}, NULL, NULL};
  SWNode *node = tsw_node(handle);
  return node ? node->value : (TSNode){{0, 0, 0, 0}, NULL, NULL};
}

uint32_t tsw_abi_version(void) { return WASITTER_ABI_VERSION; }

uint32_t tsw_alloc(uint32_t size) {
  void *ptr = malloc(size ? size : 1);
  if (!ptr) {
    tsw_set_error("out of memory");
  } else {
    tsw_ok();
  }
  return (uint32_t)(uintptr_t)ptr;
}

void tsw_free(uint32_t ptr) {
  if (!ptr) {
    tsw_ok();
    return;
  }
  /* Keep malformed host offsets from reaching the libc allocator.  A wasm
   * `free` implementation commonly reads a header immediately before the
   * supplied pointer, so even an in-bounds-looking bad offset can otherwise
   * turn into an out-of-bounds trap.  We cannot identify ownership here, but
   * checking the linear-memory extent handles the dangerous class cheaply. */
  if (!tsw_require_guest_range(ptr, 1u, "allocator pointer is invalid")) return;
  free((void *)(uintptr_t)ptr);
  tsw_ok();
}

uint32_t tsw_realloc(uint32_t ptr, uint32_t size) {
  if (ptr && !tsw_require_guest_range(ptr, 1u, "allocator pointer is invalid")) {
    return 0;
  }
  void *result = realloc((void *)(uintptr_t)ptr, size ? size : 1);
  if (!result) {
    tsw_set_error("out of memory");
  } else {
    tsw_ok();
  }
  return (uint32_t)(uintptr_t)result;
}

uint32_t tsw_language(void) {
  return (uint32_t)(uintptr_t)WASITTER_LANGUAGE_FN();
}

uint32_t tsw_language_name(uint32_t language) {
  const char *name = language ? ts_language_name(tsw_lang(language)) : NULL;
  if (!name || !name[0]) name = WASITTER_LANGUAGE_NAME;
  return (uint32_t)(uintptr_t)name;
}

uint32_t tsw_language_name_ptr(void) {
  return tsw_language_name(tsw_language());
}

uint32_t tsw_language_name_len(void) {
  const char *name = (const char *)(uintptr_t)tsw_language_name(tsw_language());
  return name ? (uint32_t)strlen(name) : 0;
}

uint32_t tsw_language_abi_version(void) {
  return ts_language_abi_version(WASITTER_LANGUAGE_FN());
}

uint32_t tsw_language_abi_version_for(uint32_t language) {
  return language ? ts_language_abi_version(tsw_lang(language)) : 0;
}

uint32_t tsw_language_metadata(uint32_t language) {
  if (!language) return 0;
  return (uint32_t)(uintptr_t)ts_language_metadata(tsw_lang(language));
}

uint32_t tsw_language_symbol_count(uint32_t language) {
  return language ? ts_language_symbol_count(tsw_lang(language)) : 0;
}

uint32_t tsw_language_state_count(uint32_t language) {
  return language ? ts_language_state_count(tsw_lang(language)) : 0;
}

uint32_t tsw_language_field_count(uint32_t language) {
  return language ? ts_language_field_count(tsw_lang(language)) : 0;
}

uint32_t tsw_language_field_id_for_name(uint32_t language, uint32_t name_ptr, uint32_t name_len) {
  if (!language) return 0;
  if ((name_len && !name_ptr) ||
      (name_ptr && !tsw_require_guest_range(
        name_ptr, name_len, "language field-name range is invalid"))) {
    return 0;
  }
  const char *name = name_ptr ? (const char *)(uintptr_t)name_ptr : "";
  return ts_language_field_id_for_name(
    tsw_lang(language), name, name_len);
}

uint32_t tsw_language_symbol_type(uint32_t language, uint32_t symbol) {
  return language ? (uint32_t)ts_language_symbol_type(tsw_lang(language), (TSSymbol)symbol) : 0;
}

uint32_t tsw_language_next_state(uint32_t language, uint32_t state, uint32_t symbol) {
  return language ? (uint32_t)ts_language_next_state(
    tsw_lang(language), (TSStateId)state, (TSSymbol)symbol) : 0;
}

uint32_t tsw_language_version(uint32_t language) {
  return language ? ts_language_version(tsw_lang(language)) : 0;
}

uint32_t tsw_language_symbol_name(uint32_t language, uint32_t symbol) {
  if (!language) return 0;
  return (uint32_t)(uintptr_t)ts_language_symbol_name(tsw_lang(language), (TSSymbol)symbol);
}

uint32_t tsw_language_field_name(uint32_t language, uint32_t field) {
  if (!language) return 0;
  return (uint32_t)(uintptr_t)ts_language_field_name_for_id(tsw_lang(language), (TSFieldId)field);
}

uint32_t tsw_language_symbol_for_name(uint32_t language, uint32_t name_ptr, uint32_t name_len) {
  return tsw_language_symbol_for_name_named(language, name_ptr, name_len, 1u);
}

uint32_t tsw_language_symbol_for_name_named(
  uint32_t language,
  uint32_t name_ptr,
  uint32_t name_len,
  uint32_t is_named
) {
  if (!language) return 0;
  if ((name_len && !name_ptr) ||
      (name_ptr && !tsw_require_guest_range(
        name_ptr, name_len, "language symbol-name range is invalid"))) {
    return 0;
  }
  const char *name = name_ptr ? (const char *)(uintptr_t)name_ptr : "";
  return ts_language_symbol_for_name(
    tsw_lang(language), name, name_len, is_named != 0);
}

/* ----------------------- Lookahead iterator ABI ----------------------- */

uint32_t tsw_lookahead_iterator_new(uint32_t language, uint32_t state) {
  if (!language) {
    tsw_set_error("language is required");
    return 0;
  }
  TSLookaheadIterator *iterator = ts_lookahead_iterator_new(
    tsw_lang(language), (TSStateId)state);
  if (!iterator) {
    tsw_set_error("invalid lookahead iterator state");
    return 0;
  }
  tsw_ok();
  return (uint32_t)(uintptr_t)iterator;
}

void tsw_lookahead_iterator_delete(uint32_t iterator) {
  if (iterator) {
    ts_lookahead_iterator_delete((TSLookaheadIterator *)(uintptr_t)iterator);
  }
}

uint32_t tsw_lookahead_iterator_reset_state(uint32_t iterator, uint32_t state) {
  if (!iterator) return 0;
  bool ok = ts_lookahead_iterator_reset_state(
    (TSLookaheadIterator *)(uintptr_t)iterator, (TSStateId)state);
  if (ok) tsw_ok(); else tsw_set_error("invalid lookahead iterator state");
  return ok ? 1u : 0u;
}

uint32_t tsw_lookahead_iterator_reset(
  uint32_t iterator,
  uint32_t language,
  uint32_t state
) {
  if (!iterator || !language) return 0;
  bool ok = ts_lookahead_iterator_reset(
    (TSLookaheadIterator *)(uintptr_t)iterator,
    tsw_lang(language),
    (TSStateId)state);
  if (ok) tsw_ok(); else tsw_set_error("invalid lookahead iterator language/state");
  return ok ? 1u : 0u;
}

uint32_t tsw_lookahead_iterator_language(uint32_t iterator) {
  if (!iterator) return 0;
  return (uint32_t)(uintptr_t)ts_lookahead_iterator_language(
    (const TSLookaheadIterator *)(uintptr_t)iterator);
}

uint32_t tsw_lookahead_iterator_next(uint32_t iterator) {
  if (!iterator) return 0;
  return ts_lookahead_iterator_next(
    (TSLookaheadIterator *)(uintptr_t)iterator) ? 1u : 0u;
}

uint32_t tsw_lookahead_iterator_current_symbol(uint32_t iterator) {
  if (!iterator) return 0;
  return (uint32_t)ts_lookahead_iterator_current_symbol(
    (const TSLookaheadIterator *)(uintptr_t)iterator);
}

uint32_t tsw_lookahead_iterator_current_symbol_name(uint32_t iterator) {
  if (!iterator) return 0;
  return (uint32_t)(uintptr_t)ts_lookahead_iterator_current_symbol_name(
    (const TSLookaheadIterator *)(uintptr_t)iterator);
}

uint32_t tsw_parser_new(void) {
  TSParser *parser = ts_parser_new();
  if (!parser) tsw_set_error("unable to allocate parser"); else tsw_ok();
  return (uint32_t)(uintptr_t)parser;
}

void tsw_parser_delete(uint32_t parser) {
  if (parser) ts_parser_delete(tsw_parser(parser));
}

uint32_t tsw_parser_set_language(uint32_t parser, uint32_t language) {
  if (!parser || !language) {
    tsw_set_error("parser and language are required");
    return 0;
  }
  bool ok = ts_parser_set_language(tsw_parser(parser), tsw_lang(language));
  if (ok) tsw_ok(); else tsw_set_error("incompatible language ABI version");
  return ok ? 1 : 0;
}

uint32_t tsw_parser_language(uint32_t parser) {
  if (!parser) return 0;
  return (uint32_t)(uintptr_t)ts_parser_language(tsw_parser(parser));
}

static bool tsw_decode_ranges(uint32_t ranges_ptr, uint32_t range_count, TSRange **out) {
  *out = NULL;
  if (range_count == 0) return true;
  if (!ranges_ptr || !tsw_size_product_fits(range_count, 24u) ||
      !tsw_guest_range_fits(ranges_ptr, (uint64_t)range_count * 24u)) return false;
  const uint8_t *src = (const uint8_t *)(uintptr_t)ranges_ptr;
  if (!tsw_size_product_fits(range_count, sizeof(TSRange))) return false;
  TSRange *ranges = (TSRange *)malloc((size_t)range_count * sizeof(TSRange));
  if (!ranges) return false;
  for (uint32_t i = 0; i < range_count; i++) {
    const uint8_t *p = src + (size_t)i * 24u;
    ranges[i].start_byte = tsw_read_u32(p + 0);
    ranges[i].end_byte = tsw_read_u32(p + 4);
    ranges[i].start_point.row = tsw_read_u32(p + 8);
    ranges[i].start_point.column = tsw_read_u32(p + 12);
    ranges[i].end_point.row = tsw_read_u32(p + 16);
    ranges[i].end_point.column = tsw_read_u32(p + 20);
  }
  *out = ranges;
  return true;
}

static void tsw_encode_range(uint8_t *dst, const TSRange *range) {
  tsw_write_u32(dst + 0, range->start_byte);
  tsw_write_u32(dst + 4, range->end_byte);
  tsw_write_u32(dst + 8, range->start_point.row);
  tsw_write_u32(dst + 12, range->start_point.column);
  tsw_write_u32(dst + 16, range->end_point.row);
  tsw_write_u32(dst + 20, range->end_point.column);
}

uint32_t tsw_parser_set_included_ranges(uint32_t parser, uint32_t ranges_ptr, uint32_t range_count) {
  if (!parser) return 0;
  TSRange *ranges = NULL;
  if (!tsw_decode_ranges(ranges_ptr, range_count, &ranges)) {
    tsw_set_error("invalid included ranges");
    return 0;
  }
  bool ok = ts_parser_set_included_ranges(tsw_parser(parser), ranges, range_count);
  free(ranges);
  if (!ok) {
    tsw_set_error("invalid included ranges");
    return 0;
  }
  tsw_ok();
  return 1;
}

uint32_t tsw_parser_included_ranges_into(uint32_t parser, uint32_t output_ptr, uint32_t output_capacity) {
  if (!parser) return 0;
  uint32_t count = 0;
  const TSRange *ranges = ts_parser_included_ranges(tsw_parser(parser), &count);
  if (!ranges) return 0;
  uint32_t copy_count = output_capacity < count ? output_capacity : count;
  if (output_ptr && copy_count) {
    if (!tsw_require_guest_range(
          output_ptr, (uint64_t)copy_count * 24u,
          "included-ranges output range is invalid")) {
      return 0;
    }
    uint8_t *dst = (uint8_t *)(uintptr_t)output_ptr;
    for (uint32_t i = 0; i < copy_count; i++) {
      tsw_encode_range(dst + (size_t)i * 24u, &ranges[i]);
    }
  }
  return count;
}

uint32_t tsw_parser_parse(uint32_t parser, uint32_t old_tree, uint32_t input_ptr, uint32_t input_len) {
  if (!parser) {
    tsw_set_error("parser is required");
    return 0;
  }
  if ((input_len && !input_ptr) ||
      (input_ptr && !tsw_require_guest_range(
        input_ptr, input_len, "parser input range is invalid"))) {
    return 0;
  }
  const char *input = (const char *)(uintptr_t)input_ptr;
  TSTree *tree = ts_parser_parse_string(
    tsw_parser(parser),
    tsw_tree(old_tree),
    input ? input : "",
    input_len
  );
  if (!tree) tsw_set_error("parse failed"); else tsw_ok();
  return (uint32_t)(uintptr_t)tree;
}

void tsw_parser_reset(uint32_t parser) {
  if (parser) ts_parser_reset(tsw_parser(parser));
}

void tsw_parser_set_timeout_micros(uint32_t parser, uint64_t timeout) {
  if (parser) ts_parser_set_timeout_micros(tsw_parser(parser), timeout);
}

uint32_t tsw_parser_set_timeout(uint32_t parser, uint64_t timeout) {
  if (!parser) {
    tsw_set_error("parser is required");
    return 0;
  }
  ts_parser_set_timeout_micros(tsw_parser(parser), timeout);
  tsw_ok();
  return 1;
}

uint64_t tsw_parser_timeout_micros(uint32_t parser) {
  return parser ? ts_parser_timeout_micros(tsw_parser(parser)) : 0;
}

uint32_t tsw_parser_set_cancellation_flag(uint32_t parser, uint32_t flag_ptr) {
  if (!parser) {
    tsw_set_error("parser is required");
    return 0;
  }
  /* Tree-sitter stores a pointer to size_t.  On wasm32 this is four bytes;
   * use the actual type in the bounds check so the bridge remains correct if
   * the function is compiled for a non-wasm smoke-test target as well. */
  if (flag_ptr && !tsw_require_guest_range(
        flag_ptr, sizeof(size_t), "cancellation flag range is invalid")) {
    return 0;
  }
  ts_parser_set_cancellation_flag(
    tsw_parser(parser), flag_ptr ? (const size_t *)(uintptr_t)flag_ptr : NULL);
  tsw_ok();
  return 1;
}

uint32_t tsw_parser_cancellation_flag(uint32_t parser) {
  if (!parser) return 0;
  const size_t *flag = ts_parser_cancellation_flag(tsw_parser(parser));
  return (uint32_t)(uintptr_t)flag;
}

void tsw_tree_delete(uint32_t tree) {
  if (tree) ts_tree_delete(tsw_tree(tree));
}

uint32_t tsw_tree_copy(uint32_t tree) {
  if (!tree) return 0;
  return (uint32_t)(uintptr_t)ts_tree_copy(tsw_tree(tree));
}

uint32_t tsw_tree_root(uint32_t tree) {
  if (!tree) return 0;
  return tsw_wrap_node(ts_tree_root_node(tsw_tree(tree)));
}

uint32_t tsw_tree_root_with_offset(uint32_t tree, uint32_t offset_bytes, uint64_t offset_point) {
  if (!tree) return 0;
  TSPoint point = {(uint32_t)(offset_point >> 32), (uint32_t)offset_point};
  return tsw_wrap_node(ts_tree_root_node_with_offset(tsw_tree(tree), offset_bytes, point));
}

uint32_t tsw_tree_language(uint32_t tree) {
  if (!tree) return 0;
  return (uint32_t)(uintptr_t)ts_tree_language(tsw_tree(tree));
}

uint32_t tsw_tree_included_ranges_into(uint32_t tree, uint32_t output_ptr, uint32_t output_capacity) {
  if (!tree) return 0;
  uint32_t count = 0;
  TSRange *ranges = ts_tree_included_ranges(tsw_tree(tree), &count);
  if (!ranges) return 0;
  uint32_t copy_count = output_capacity < count ? output_capacity : count;
  if (output_ptr && copy_count) {
    if (!tsw_require_guest_range(
          output_ptr, (uint64_t)copy_count * 24u,
          "included-ranges output range is invalid")) {
      free(ranges);
      return 0;
    }
    uint8_t *dst = (uint8_t *)(uintptr_t)output_ptr;
    for (uint32_t i = 0; i < copy_count; i++) {
      tsw_encode_range(dst + (size_t)i * 24u, &ranges[i]);
    }
  }
  free(ranges);
  return count;
}

void tsw_tree_edit(uint32_t tree, uint32_t edit_ptr) {
  if (!tree || !edit_ptr) return;
  if (!tsw_require_guest_range(edit_ptr, 36u, "tree edit range is invalid")) return;
  const SWInputEdit *input = (const SWInputEdit *)(uintptr_t)edit_ptr;
  TSInputEdit edit = {
    .start_byte = input->start_byte,
    .old_end_byte = input->old_end_byte,
    .new_end_byte = input->new_end_byte,
    .start_point = input->start_point,
    .old_end_point = input->old_end_point,
    .new_end_point = input->new_end_point,
  };
  ts_tree_edit(tsw_tree(tree), &edit);
}

uint32_t tsw_tree_changed_ranges_into(uint32_t old_tree, uint32_t new_tree, uint32_t output_ptr, uint32_t output_capacity) {
  if (!old_tree || !new_tree) return 0;
  uint32_t count = 0;
  TSRange *ranges = ts_tree_get_changed_ranges(tsw_tree(old_tree), tsw_tree(new_tree), &count);
  if (!ranges) return 0;
  uint32_t copy_count = count < output_capacity ? count : output_capacity;
  if (output_ptr && copy_count) {
    if (!tsw_require_guest_range(
          output_ptr, (uint64_t)copy_count * 24u,
          "changed-ranges output range is invalid")) {
      free(ranges);
      return 0;
    }
    uint8_t *out = (uint8_t *)(uintptr_t)output_ptr;
    for (uint32_t i = 0; i < copy_count; i++) {
      tsw_encode_range(out + (size_t)i * 24u, &ranges[i]);
    }
  }
  free(ranges);
  return count;
}

uint32_t tsw_tree_get_changed_ranges(uint32_t old_tree, uint32_t new_tree) {
  if (!old_tree || !new_tree) return 0;
  uint32_t count = 0;
  TSRange *ranges = ts_tree_get_changed_ranges(tsw_tree(old_tree), tsw_tree(new_tree), &count);
  if (!ranges) return 0;
  /* The returned block is a wire-format record (u32 count followed by
   * 24-byte ranges), not an array whose size may silently wrap on wasm32. */
  if (!tsw_size_header_product_fits(count, sizeof(uint32_t), 24u)) {
    free(ranges);
    tsw_set_error("changed-ranges allocation is too large");
    return 0;
  }
  size_t bytes = sizeof(uint32_t) + (size_t)count * 24u;
  uint8_t *block = (uint8_t *)malloc(bytes ? bytes : sizeof(uint32_t));
  if (!block) {
    free(ranges);
    tsw_set_error("out of memory");
    return 0;
  }
  tsw_write_u32(block, count);
  for (uint32_t i = 0; i < count; i++) {
    tsw_encode_range(block + sizeof(count) + (size_t)i * 24u, &ranges[i]);
  }
  free(ranges);
  return (uint32_t)(uintptr_t)block;
}

void tsw_ranges_free(uint32_t ranges, uint32_t count) {
  if (ranges && !tsw_require_guest_range(ranges, 1u, "ranges pointer is invalid")) return;
  (void)count;
  free((void *)(uintptr_t)ranges);
}

void tsw_node_delete(uint32_t node) { free(tsw_node(node)); }

uint32_t tsw_node_type_ptr(uint32_t node) {
  TSNode value = tsw_unwrap_node(node);
  return (uint32_t)(uintptr_t)ts_node_type(value);
}

uint32_t tsw_node_type_len(uint32_t node) {
  const char *type = (const char *)(uintptr_t)tsw_node_type_ptr(node);
  return type ? (uint32_t)strlen(type) : 0;
}

static uint32_t tsw_node_sexp_ptr(uint32_t node) {
  TSNode value = tsw_unwrap_node(node);
  char *owned = ts_node_string(value);
  if (!owned) {
    tsw_set_error("unable to serialize node");
    return 0;
  }
  size_t raw_length = strlen(owned);
  if (raw_length == SIZE_MAX) {
    free(owned);
    tsw_set_error("serialized node is too large");
    return 0;
  }
  size_t length = raw_length + 1;
  if (length > tsw_sexp_capacity) {
    size_t capacity = tsw_sexp_capacity ? tsw_sexp_capacity : 256;
    while (capacity < length) {
      if (capacity > SIZE_MAX / 2u) {
        capacity = length;
        break;
      }
      capacity *= 2;
    }
    char *grown = (char *)realloc(tsw_sexp_buffer, capacity);
    if (!grown) {
      free(owned);
      tsw_set_error("out of memory");
      return 0;
    }
    tsw_sexp_buffer = grown;
    tsw_sexp_capacity = capacity;
  }
  memcpy(tsw_sexp_buffer, owned, length);
  free(owned);
  return (uint32_t)(uintptr_t)tsw_sexp_buffer;
}

uint32_t tsw_node_string(uint32_t node) { return tsw_node_sexp_ptr(node); }
uint32_t tsw_node_to_sexp(uint32_t node) { return tsw_node_sexp_ptr(node); }
uint32_t tsw_node_id(uint32_t node) {
  TSNode value = tsw_unwrap_node(node);
  return (uint32_t)(uintptr_t)value.id;
}
uint32_t tsw_node_type(uint32_t node) { return tsw_node_type_ptr(node); }
uint32_t tsw_node_kind_id(uint32_t node) { return ts_node_symbol(tsw_unwrap_node(node)); }
uint32_t tsw_node_grammar_type(uint32_t node) {
  return (uint32_t)(uintptr_t)ts_node_grammar_type(tsw_unwrap_node(node));
}

uint32_t tsw_node_symbol(uint32_t node) { return ts_node_symbol(tsw_unwrap_node(node)); }
uint32_t tsw_node_grammar_symbol(uint32_t node) { return ts_node_grammar_symbol(tsw_unwrap_node(node)); }
uint32_t tsw_node_language(uint32_t node) {
  return (uint32_t)(uintptr_t)ts_node_language(tsw_unwrap_node(node));
}
uint32_t tsw_node_start_byte(uint32_t node) { return ts_node_start_byte(tsw_unwrap_node(node)); }
uint32_t tsw_node_end_byte(uint32_t node) { return ts_node_end_byte(tsw_unwrap_node(node)); }
uint32_t tsw_node_start_row(uint32_t node) { return ts_node_start_point(tsw_unwrap_node(node)).row; }
uint32_t tsw_node_start_column(uint32_t node) { return ts_node_start_point(tsw_unwrap_node(node)).column; }
uint32_t tsw_node_end_row(uint32_t node) { return ts_node_end_point(tsw_unwrap_node(node)).row; }
uint32_t tsw_node_end_column(uint32_t node) { return ts_node_end_point(tsw_unwrap_node(node)).column; }
uint64_t tsw_node_start_point(uint32_t node) {
  TSPoint point = ts_node_start_point(tsw_unwrap_node(node));
  return ((uint64_t)point.row << 32) | point.column;
}
uint64_t tsw_node_end_point(uint32_t node) {
  TSPoint point = ts_node_end_point(tsw_unwrap_node(node));
  return ((uint64_t)point.row << 32) | point.column;
}
uint32_t tsw_node_is_null(uint32_t node) { return node ? ts_node_is_null(tsw_unwrap_node(node)) : 1; }
uint32_t tsw_node_is_named(uint32_t node) { return ts_node_is_named(tsw_unwrap_node(node)); }
uint32_t tsw_node_is_missing(uint32_t node) { return ts_node_is_missing(tsw_unwrap_node(node)); }
uint32_t tsw_node_is_extra(uint32_t node) { return ts_node_is_extra(tsw_unwrap_node(node)); }
uint32_t tsw_node_has_changes(uint32_t node) { return ts_node_has_changes(tsw_unwrap_node(node)); }
uint32_t tsw_node_has_error(uint32_t node) { return ts_node_has_error(tsw_unwrap_node(node)); }
uint32_t tsw_node_is_error(uint32_t node) { return ts_node_is_error(tsw_unwrap_node(node)); }
uint32_t tsw_node_parse_state(uint32_t node) { return ts_node_parse_state(tsw_unwrap_node(node)); }
uint32_t tsw_node_next_parse_state(uint32_t node) { return ts_node_next_parse_state(tsw_unwrap_node(node)); }
uint32_t tsw_node_parent(uint32_t node) { return tsw_wrap_node(ts_node_parent(tsw_unwrap_node(node))); }
uint32_t tsw_node_eq(uint32_t node, uint32_t other) {
  return ts_node_eq(tsw_unwrap_node(node), tsw_unwrap_node(other)) ? 1 : 0;
}
uint32_t tsw_node_child_with_descendant(uint32_t node, uint32_t descendant) {
  return tsw_wrap_node(ts_node_child_with_descendant(
    tsw_unwrap_node(node), tsw_unwrap_node(descendant)));
}
uint32_t tsw_node_child(uint32_t node, uint32_t index) { return tsw_wrap_node(ts_node_child(tsw_unwrap_node(node), index)); }
uint32_t tsw_node_named_child(uint32_t node, uint32_t index) { return tsw_wrap_node(ts_node_named_child(tsw_unwrap_node(node), index)); }
uint32_t tsw_node_child_count(uint32_t node) { return ts_node_child_count(tsw_unwrap_node(node)); }
uint32_t tsw_node_named_child_count(uint32_t node) { return ts_node_named_child_count(tsw_unwrap_node(node)); }
uint32_t tsw_node_next_sibling(uint32_t node) { return tsw_wrap_node(ts_node_next_sibling(tsw_unwrap_node(node))); }
uint32_t tsw_node_prev_sibling(uint32_t node) { return tsw_wrap_node(ts_node_prev_sibling(tsw_unwrap_node(node))); }
uint32_t tsw_node_next_named_sibling(uint32_t node) { return tsw_wrap_node(ts_node_next_named_sibling(tsw_unwrap_node(node))); }
uint32_t tsw_node_prev_named_sibling(uint32_t node) { return tsw_wrap_node(ts_node_prev_named_sibling(tsw_unwrap_node(node))); }

uint32_t tsw_node_child_by_field_name(uint32_t node, uint32_t name_ptr, uint32_t name_len) {
  if ((name_len && !name_ptr) ||
      (name_ptr && !tsw_require_guest_range(
        name_ptr, name_len, "node field-name range is invalid"))) {
    return 0;
  }
  const char *name = name_ptr ? (const char *)(uintptr_t)name_ptr : "";
  return tsw_wrap_node(ts_node_child_by_field_name(
    tsw_unwrap_node(node), name, name_len));
}

uint32_t tsw_node_child_by_field_id(uint32_t node, uint32_t field) {
  return tsw_wrap_node(ts_node_child_by_field_id(tsw_unwrap_node(node), (TSFieldId)field));
}

uint32_t tsw_node_field_name_for_child_ptr(uint32_t node, uint32_t index) {
  return (uint32_t)(uintptr_t)ts_node_field_name_for_child(tsw_unwrap_node(node), index);
}

uint32_t tsw_node_field_name_for_child(uint32_t node, uint32_t index) {
  return tsw_node_field_name_for_child_ptr(node, index);
}

uint32_t tsw_node_field_name_for_named_child(uint32_t node, uint32_t index) {
  return (uint32_t)(uintptr_t)ts_node_field_name_for_named_child(tsw_unwrap_node(node), index);
}

uint32_t tsw_node_first_child_for_byte(uint32_t node, uint32_t byte) {
  return tsw_wrap_node(ts_node_first_child_for_byte(tsw_unwrap_node(node), byte));
}

uint32_t tsw_node_first_named_child_for_byte(uint32_t node, uint32_t byte) {
  return tsw_wrap_node(ts_node_first_named_child_for_byte(tsw_unwrap_node(node), byte));
}

uint32_t tsw_node_descendant_for_byte_range(uint32_t node, uint32_t start, uint32_t end) {
  return tsw_wrap_node(ts_node_descendant_for_byte_range(tsw_unwrap_node(node), start, end));
}

uint32_t tsw_node_named_descendant_for_byte_range(uint32_t node, uint32_t start, uint32_t end) {
  return tsw_wrap_node(ts_node_named_descendant_for_byte_range(tsw_unwrap_node(node), start, end));
}

uint32_t tsw_node_descendant_for_point_range(uint32_t node, uint64_t start, uint64_t end) {
  TSPoint start_point = {(uint32_t)(start >> 32), (uint32_t)start};
  TSPoint end_point = {(uint32_t)(end >> 32), (uint32_t)end};
  return tsw_wrap_node(ts_node_descendant_for_point_range(tsw_unwrap_node(node), start_point, end_point));
}

uint32_t tsw_node_named_descendant_for_point_range(uint32_t node, uint64_t start, uint64_t end) {
  TSPoint start_point = {(uint32_t)(start >> 32), (uint32_t)start};
  TSPoint end_point = {(uint32_t)(end >> 32), (uint32_t)end};
  return tsw_wrap_node(ts_node_named_descendant_for_point_range(tsw_unwrap_node(node), start_point, end_point));
}

uint32_t tsw_node_descendant_count(uint32_t node) {
  return ts_node_descendant_count(tsw_unwrap_node(node));
}

void tsw_node_edit(uint32_t node, uint32_t edit_ptr) {
  if (!node || !edit_ptr) return;
  if (!tsw_require_guest_range(edit_ptr, 36u, "node edit range is invalid")) return;
  SWNode *wrapper = tsw_node(node);
  if (!wrapper) return;
  const SWInputEdit *input = (const SWInputEdit *)(uintptr_t)edit_ptr;
  TSInputEdit edit = {
    .start_byte = input->start_byte,
    .old_end_byte = input->old_end_byte,
    .new_end_byte = input->new_end_byte,
    .start_point = input->start_point,
    .old_end_point = input->old_end_point,
    .new_end_point = input->new_end_point,
  };
  ts_node_edit(&wrapper->value, &edit);
}

/* ------------------------- Tree cursor ABI ------------------------- */

uint32_t tsw_cursor_new(uint32_t node) {
  if (!node) return 0;
  SWCursor *cursor = (SWCursor *)malloc(sizeof(*cursor));
  if (!cursor) {
    tsw_set_error("out of memory");
    return 0;
  }
  cursor->value = ts_tree_cursor_new(tsw_unwrap_node(node));
  tsw_ok();
  return (uint32_t)(uintptr_t)cursor;
}

void tsw_cursor_delete(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper) return;
  ts_tree_cursor_delete(&wrapper->value);
  free(wrapper);
}

void tsw_cursor_reset(uint32_t cursor, uint32_t node) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper || !node) return;
  ts_tree_cursor_reset(&wrapper->value, tsw_unwrap_node(node));
}

void tsw_cursor_reset_to(uint32_t cursor, uint32_t other) {
  SWCursor *dst = tsw_cursor(cursor);
  SWCursor *src = tsw_cursor(other);
  if (!dst || !src) return;
  ts_tree_cursor_reset_to(&dst->value, &src->value);
}

uint32_t tsw_cursor_current_node(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper) return 0;
  return tsw_wrap_node(ts_tree_cursor_current_node(&wrapper->value));
}

uint32_t tsw_cursor_current_field_name_ptr(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper) return 0;
  return (uint32_t)(uintptr_t)ts_tree_cursor_current_field_name(&wrapper->value);
}

uint32_t tsw_cursor_current_field_name_len(uint32_t cursor) {
  const char *name = (const char *)(uintptr_t)tsw_cursor_current_field_name_ptr(cursor);
  return name ? (uint32_t)strlen(name) : 0;
}

uint32_t tsw_cursor_current_field_name(uint32_t cursor) {
  return tsw_cursor_current_field_name_ptr(cursor);
}

uint32_t tsw_cursor_current_field_id(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper) return 0;
  return ts_tree_cursor_current_field_id(&wrapper->value);
}

uint32_t tsw_cursor_goto_parent(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper && ts_tree_cursor_goto_parent(&wrapper->value) ? 1u : 0u;
}

uint32_t tsw_cursor_goto_next_sibling(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper && ts_tree_cursor_goto_next_sibling(&wrapper->value) ? 1u : 0u;
}

uint32_t tsw_cursor_goto_previous_sibling(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper && ts_tree_cursor_goto_previous_sibling(&wrapper->value) ? 1u : 0u;
}

uint32_t tsw_cursor_goto_first_child(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper && ts_tree_cursor_goto_first_child(&wrapper->value) ? 1u : 0u;
}

uint32_t tsw_cursor_goto_last_child(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper && ts_tree_cursor_goto_last_child(&wrapper->value) ? 1u : 0u;
}

void tsw_cursor_goto_descendant(uint32_t cursor, uint32_t index) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (wrapper) ts_tree_cursor_goto_descendant(&wrapper->value, index);
}

uint32_t tsw_cursor_current_descendant_index(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper ? ts_tree_cursor_current_descendant_index(&wrapper->value) : 0;
}

uint32_t tsw_cursor_current_depth(uint32_t cursor) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper ? ts_tree_cursor_current_depth(&wrapper->value) : 0;
}

int64_t tsw_cursor_goto_first_child_for_byte(uint32_t cursor, uint32_t byte) {
  SWCursor *wrapper = tsw_cursor(cursor);
  return wrapper ? ts_tree_cursor_goto_first_child_for_byte(&wrapper->value, byte) : -1;
}

int64_t tsw_cursor_goto_first_child_for_point(uint32_t cursor, uint64_t point) {
  SWCursor *wrapper = tsw_cursor(cursor);
  if (!wrapper) return -1;
  TSPoint goal = {(uint32_t)(point >> 32), (uint32_t)point};
  return ts_tree_cursor_goto_first_child_for_point(&wrapper->value, goal);
}

uint32_t tsw_cursor_copy(uint32_t cursor) {
  SWCursor *src = tsw_cursor(cursor);
  if (!src) return 0;
  SWCursor *dst = (SWCursor *)malloc(sizeof(*dst));
  if (!dst) {
    tsw_set_error("out of memory");
    return 0;
  }
  dst->value = ts_tree_cursor_copy(&src->value);
  return (uint32_t)(uintptr_t)dst;
}

/* Keep aliases as real exports (rather than preprocessor macros), since wasm
 * hosts resolve function names dynamically. */
uint32_t tsw_tree_cursor_new(uint32_t node) { return tsw_cursor_new(node); }
void tsw_tree_cursor_delete(uint32_t cursor) { tsw_cursor_delete(cursor); }
void tsw_tree_cursor_reset(uint32_t cursor, uint32_t node) { tsw_cursor_reset(cursor, node); }
void tsw_tree_cursor_reset_to(uint32_t cursor, uint32_t other) { tsw_cursor_reset_to(cursor, other); }
uint32_t tsw_tree_cursor_current_node(uint32_t cursor) { return tsw_cursor_current_node(cursor); }
uint32_t tsw_tree_cursor_current_field_name_ptr(uint32_t cursor) { return tsw_cursor_current_field_name_ptr(cursor); }
uint32_t tsw_tree_cursor_current_field_name_len(uint32_t cursor) { return tsw_cursor_current_field_name_len(cursor); }
uint32_t tsw_tree_cursor_current_field_name(uint32_t cursor) { return tsw_cursor_current_field_name(cursor); }
uint32_t tsw_tree_cursor_current_field_id(uint32_t cursor) { return tsw_cursor_current_field_id(cursor); }
uint32_t tsw_tree_cursor_goto_parent(uint32_t cursor) { return tsw_cursor_goto_parent(cursor); }
uint32_t tsw_tree_cursor_goto_next_sibling(uint32_t cursor) { return tsw_cursor_goto_next_sibling(cursor); }
uint32_t tsw_tree_cursor_goto_previous_sibling(uint32_t cursor) { return tsw_cursor_goto_previous_sibling(cursor); }
uint32_t tsw_tree_cursor_goto_prev_sibling(uint32_t cursor) { return tsw_cursor_goto_previous_sibling(cursor); }
uint32_t tsw_tree_cursor_goto_first_child(uint32_t cursor) { return tsw_cursor_goto_first_child(cursor); }
uint32_t tsw_tree_cursor_goto_last_child(uint32_t cursor) { return tsw_cursor_goto_last_child(cursor); }
void tsw_tree_cursor_goto_descendant(uint32_t cursor, uint32_t index) { tsw_cursor_goto_descendant(cursor, index); }
uint32_t tsw_tree_cursor_current_descendant_index(uint32_t cursor) { return tsw_cursor_current_descendant_index(cursor); }
uint32_t tsw_tree_cursor_current_depth(uint32_t cursor) { return tsw_cursor_current_depth(cursor); }
int64_t tsw_tree_cursor_goto_first_child_for_byte(uint32_t cursor, uint32_t byte) { return tsw_cursor_goto_first_child_for_byte(cursor, byte); }
int64_t tsw_tree_cursor_goto_first_child_for_point(uint32_t cursor, uint64_t point) { return tsw_cursor_goto_first_child_for_point(cursor, point); }
uint32_t tsw_tree_cursor_copy(uint32_t cursor) { return tsw_cursor_copy(cursor); }

/* ---------------------------- Query ABI ---------------------------- */

static const char *tsw_query_error_string(TSQueryError error) {
  switch (error) {
    case TSQueryErrorSyntax: return "query syntax error";
    case TSQueryErrorNodeType: return "unknown node type";
    case TSQueryErrorField: return "unknown field name";
    case TSQueryErrorCapture: return "invalid capture name";
    case TSQueryErrorStructure: return "invalid query structure";
    case TSQueryErrorLanguage: return "query language mismatch";
    default: return "query error";
  }
}

uint32_t tsw_query_new(uint32_t language, uint32_t source_ptr, uint32_t source_len, uint32_t error_out_ptr) {
  uint32_t error_offset = 0;
  TSQueryError error_type = TSQueryErrorNone;
  bool error_output_valid = true;
  const char *source = source_ptr ? (const char *)(uintptr_t)source_ptr : "";
  TSQuery *query = NULL;
  if (language &&
      (source_ptr || source_len == 0) &&
      (!source_ptr || tsw_require_guest_range(
        source_ptr, source_len, "query source range is invalid"))) {
    query = ts_query_new(tsw_lang(language), source, source_len, &error_offset, &error_type);
  } else {
    error_type = language ? TSQueryErrorSyntax : TSQueryErrorLanguage;
  }
  if (error_out_ptr) {
    error_output_valid = tsw_require_guest_range(
      error_out_ptr, 8u, "query error output range is invalid");
    if (error_output_valid) {
      uint8_t *out = (uint8_t *)(uintptr_t)error_out_ptr;
      tsw_write_u32(out + 0, error_offset);
      tsw_write_u32(out + 4, (uint32_t)error_type);
    }
  }
  if (!error_output_valid) {
    if (query) ts_query_delete(query);
    return 0;
  }
  if (!query) {
    tsw_set_error(tsw_query_error_string(error_type));
    return 0;
  }
  tsw_ok();
  return (uint32_t)(uintptr_t)query;
}

void tsw_query_delete(uint32_t query) {
  if (query) ts_query_delete((TSQuery *)(uintptr_t)query);
}

uint32_t tsw_query_pattern_count(uint32_t query) {
  return query ? ts_query_pattern_count((const TSQuery *)(uintptr_t)query) : 0;
}

uint32_t tsw_query_capture_count(uint32_t query) {
  return query ? ts_query_capture_count((const TSQuery *)(uintptr_t)query) : 0;
}

uint32_t tsw_query_string_count(uint32_t query) {
  return query ? ts_query_string_count((const TSQuery *)(uintptr_t)query) : 0;
}

uint32_t tsw_query_start_byte_for_pattern(uint32_t query, uint32_t pattern) {
  if (!query || pattern >= ts_query_pattern_count((const TSQuery *)(uintptr_t)query)) return 0;
  return ts_query_start_byte_for_pattern((const TSQuery *)(uintptr_t)query, pattern);
}

uint32_t tsw_query_end_byte_for_pattern(uint32_t query, uint32_t pattern) {
  if (!query || pattern >= ts_query_pattern_count((const TSQuery *)(uintptr_t)query)) return 0;
  return ts_query_end_byte_for_pattern((const TSQuery *)(uintptr_t)query, pattern);
}

uint32_t tsw_query_predicates_for_pattern_into(
  uint32_t query,
  uint32_t pattern,
  uint32_t output_ptr,
  uint32_t output_capacity
) {
  if (!query) return 0;
  if (pattern >= ts_query_pattern_count((const TSQuery *)(uintptr_t)query)) return 0;
  uint32_t count = 0;
  const TSQueryPredicateStep *steps = ts_query_predicates_for_pattern(
    (const TSQuery *)(uintptr_t)query, pattern, &count);
  if (!steps) return 0;
  uint64_t required64 = (uint64_t)count * 8u;
  uint32_t required = required64 > UINT32_MAX ? UINT32_MAX : (uint32_t)required64;
  uint32_t copy_count = output_capacity / 8u;
  if (copy_count > count) copy_count = count;
  if (output_ptr && copy_count &&
      !tsw_require_guest_range(
        output_ptr, (uint64_t)copy_count * 8u,
        "query predicate output range is invalid")) {
    return 0;
  }
  if (output_ptr && copy_count) {
    uint8_t *out = (uint8_t *)(uintptr_t)output_ptr;
    for (uint32_t i = 0; i < copy_count; i++) {
      tsw_write_u32(out + (size_t)i * 8u, (uint32_t)steps[i].type);
      tsw_write_u32(out + (size_t)i * 8u + 4u, steps[i].value_id);
    }
  }
  return required;
}

uint32_t tsw_query_is_pattern_rooted(uint32_t query, uint32_t pattern) {
  return query && pattern < ts_query_pattern_count((const TSQuery *)(uintptr_t)query) &&
    ts_query_is_pattern_rooted((const TSQuery *)(uintptr_t)query, pattern) ? 1u : 0u;
}

uint32_t tsw_query_is_pattern_non_local(uint32_t query, uint32_t pattern) {
  return query && pattern < ts_query_pattern_count((const TSQuery *)(uintptr_t)query) &&
    ts_query_is_pattern_non_local((const TSQuery *)(uintptr_t)query, pattern) ? 1u : 0u;
}

uint32_t tsw_query_is_pattern_guaranteed_at_step(uint32_t query, uint32_t offset) {
  return query && ts_query_is_pattern_guaranteed_at_step((const TSQuery *)(uintptr_t)query, offset) ? 1u : 0u;
}

uint32_t tsw_query_capture_name_ptr(uint32_t query, uint32_t index) {
  if (!query || index >= ts_query_capture_count((const TSQuery *)(uintptr_t)query)) return 0;
  uint32_t length = 0;
  return (uint32_t)(uintptr_t)ts_query_capture_name_for_id(
    (const TSQuery *)(uintptr_t)query, index, &length);
}

uint32_t tsw_query_capture_name_len(uint32_t query, uint32_t index) {
  if (!query || index >= ts_query_capture_count((const TSQuery *)(uintptr_t)query)) return 0;
  uint32_t length = 0;
  (void)ts_query_capture_name_for_id((const TSQuery *)(uintptr_t)query, index, &length);
  return length;
}

uint32_t tsw_query_capture_name(uint32_t query, uint32_t index) {
  return tsw_query_capture_name_ptr(query, index);
}

uint32_t tsw_query_capture_quantifier_for_id(uint32_t query, uint32_t pattern, uint32_t capture) {
  if (!query || pattern >= ts_query_pattern_count((const TSQuery *)(uintptr_t)query) ||
      capture >= ts_query_capture_count((const TSQuery *)(uintptr_t)query)) return 0;
  return (uint32_t)ts_query_capture_quantifier_for_id(
    (const TSQuery *)(uintptr_t)query, pattern, capture);
}

uint32_t tsw_query_string_value_ptr(uint32_t query, uint32_t index) {
  if (!query || index >= ts_query_string_count((const TSQuery *)(uintptr_t)query)) return 0;
  uint32_t length = 0;
  return (uint32_t)(uintptr_t)ts_query_string_value_for_id(
    (const TSQuery *)(uintptr_t)query, index, &length);
}

uint32_t tsw_query_string_value_len(uint32_t query, uint32_t index) {
  if (!query || index >= ts_query_string_count((const TSQuery *)(uintptr_t)query)) return 0;
  uint32_t length = 0;
  (void)ts_query_string_value_for_id((const TSQuery *)(uintptr_t)query, index, &length);
  return length;
}

uint32_t tsw_query_string_value(uint32_t query, uint32_t index) {
  return tsw_query_string_value_ptr(query, index);
}

void tsw_query_disable_capture(uint32_t query, uint32_t name_ptr, uint32_t name_len) {
  if (!query) return;
  if ((name_len && !name_ptr) ||
      (name_ptr && !tsw_require_guest_range(
        name_ptr, name_len, "query capture-name range is invalid"))) {
    return;
  }
  const char *name = name_ptr ? (const char *)(uintptr_t)name_ptr : "";
  ts_query_disable_capture((TSQuery *)(uintptr_t)query, name, name_len);
}

void tsw_query_disable_pattern(uint32_t query, uint32_t pattern) {
  if (query) ts_query_disable_pattern((TSQuery *)(uintptr_t)query, pattern);
}

uint32_t tsw_query_cursor_new(void) {
  SWQueryCursor *wrapper = (SWQueryCursor *)calloc(1, sizeof(*wrapper));
  if (!wrapper) {
    tsw_set_error("out of memory");
    return 0;
  }
  wrapper->value = ts_query_cursor_new();
  if (!wrapper->value) {
    free(wrapper);
    tsw_set_error("unable to allocate query cursor");
    return 0;
  }
  wrapper->has_pending_match = false;
  wrapper->pending_is_capture = false;
  wrapper->pending_capture_index = 0;
  tsw_ok();
  return (uint32_t)(uintptr_t)wrapper;
}

void tsw_query_cursor_delete(uint32_t cursor) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!wrapper) return;
  tsw_query_cursor_clear_pending(wrapper);
  if (wrapper->value) ts_query_cursor_delete(wrapper->value);
  free(wrapper);
}

void tsw_query_cursor_exec(uint32_t cursor, uint32_t query, uint32_t node) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!wrapper || !wrapper->value) return;
  tsw_query_cursor_clear_pending(wrapper);
  if (!query || !node) return;
  ts_query_cursor_exec(wrapper->value,
    (const TSQuery *)(uintptr_t)query, tsw_unwrap_node(node));
}

static bool tsw_query_cursor_pending(SWQueryCursor *wrapper) {
  if (!wrapper || !wrapper->value) return false;
  if (wrapper->has_pending_match) {
    /* A capture probe has already advanced the native cursor by one capture.
     * It must not be replayed as a complete match: doing so would duplicate
     * captures (and, before the pending list was copied, could dereference a
     * capture array released by the native cursor).  Discard the probe and
     * continue with the native next-match stream when callers switch modes. */
    if (wrapper->pending_is_capture) {
      tsw_query_cursor_clear_pending(wrapper);
    } else {
      return true;
    }
  }
  TSQueryMatch match;
  tsw_query_cursor_clear_pending(wrapper);
  if (!ts_query_cursor_next_match(wrapper->value, &match)) return false;
  if (match.capture_count != 0 && !match.captures) {
    tsw_set_error("query match has a null capture list");
    return false;
  }
  if (match.capture_count != 0) {
    if (!tsw_size_product_fits(match.capture_count, sizeof(TSQueryCapture))) {
      tsw_set_error("query match capture list is too large");
      return false;
    }
    wrapper->pending_captures = (TSQueryCapture *)malloc(
      (size_t)match.capture_count * sizeof(TSQueryCapture));
    if (!wrapper->pending_captures) {
      tsw_set_error("out of memory while retaining query match");
      return false;
    }
    memcpy(
      wrapper->pending_captures,
      match.captures,
      (size_t)match.capture_count * sizeof(TSQueryCapture));
  }
  wrapper->pending_capture_count = match.capture_count;
  wrapper->pending_match = match;
  wrapper->pending_match.captures = wrapper->pending_captures;
  wrapper->pending_capture_index = 0;
  wrapper->has_pending_match = true;
  wrapper->pending_is_capture = false;
  return true;
}

/*
 * Ensure that a capture is available for tsw_query_cursor_next_capture.
 * There are two kinds of pending state:
 *
 *   - a complete match obtained by next_match (usually by a size probe), in
 *     which case pending_capture_index identifies the next capture to emit;
 *   - one capture obtained by the native next_capture API, in which case the
 *     match and its capture index are retained until the output buffer is
 *     available.
 *
 * Keeping the latter state is important: returning a fixed size for a probe
 * without retaining the native result would silently drop captures.
 */
static bool tsw_query_cursor_pending_capture(SWQueryCursor *wrapper) {
  if (!wrapper || !wrapper->value) return false;

  if (wrapper->has_pending_match) {
    if (wrapper->pending_is_capture) return true;
    if (wrapper->pending_capture_index < wrapper->pending_match.capture_count) {
      return true;
    }
    tsw_query_cursor_clear_pending(wrapper);
  }

  TSQueryMatch match;
  uint32_t capture_index = 0;
  tsw_query_cursor_clear_pending(wrapper);
  if (!ts_query_cursor_next_capture(wrapper->value, &match, &capture_index)) {
    return false;
  }
  if (match.capture_count != 0 && !match.captures) {
    tsw_set_error("query capture has a null capture list");
    return false;
  }
  if (match.capture_count != 0) {
    if (!tsw_size_product_fits(match.capture_count, sizeof(TSQueryCapture))) {
      tsw_set_error("query capture list is too large");
      return false;
    }
    wrapper->pending_captures = (TSQueryCapture *)malloc(
      (size_t)match.capture_count * sizeof(TSQueryCapture));
    if (!wrapper->pending_captures) {
      tsw_set_error("out of memory while retaining query capture");
      return false;
    }
    memcpy(
      wrapper->pending_captures,
      match.captures,
      (size_t)match.capture_count * sizeof(TSQueryCapture));
  }
  wrapper->pending_capture_count = match.capture_count;
  wrapper->pending_match = match;
  wrapper->pending_match.captures = wrapper->pending_captures;
  wrapper->pending_capture_index = capture_index;
  wrapper->has_pending_match = true;
  wrapper->pending_is_capture = true;
  return true;
}

static void tsw_query_cursor_clear_pending(SWQueryCursor *wrapper) {
  if (!wrapper) return;
  free(wrapper->pending_captures);
  wrapper->pending_captures = NULL;
  wrapper->pending_capture_count = 0;
  wrapper->pending_match = (TSQueryMatch){0};
  wrapper->has_pending_match = false;
  wrapper->pending_is_capture = false;
  wrapper->pending_capture_index = 0;
}

uint32_t tsw_query_cursor_next_match(uint32_t cursor, uint32_t output_ptr, uint32_t output_capacity) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!tsw_query_cursor_pending(wrapper)) return 0;
  const TSQueryMatch *match = &wrapper->pending_match;
  uint32_t count = match->capture_count;
  if (count != 0 && !match->captures) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("query match has a null capture list");
    return 0;
  }
  uint64_t required64 = 8u + (uint64_t)count * 8u;
  uint32_t required = required64 > UINT32_MAX ? UINT32_MAX : (uint32_t)required64;
  if (!output_ptr || output_capacity < required) return required;

  /* Check the 32-bit address arithmetic before forming the guest pointer.
   * This is mostly defensive (wazero validates memory accesses too), but it
   * turns malformed host pointers into a clean probe result instead of an
   * integer wrap followed by an out-of-bounds write. */
  if (!tsw_require_guest_range(
        output_ptr, required, "query match output range is invalid")) {
    return 0;
  }

  uint8_t *out = (uint8_t *)(uintptr_t)output_ptr;
  tsw_write_u32(out + 0, match->id);
  tsw_write_u16(out + 4, match->pattern_index);
  /* TSQueryMatch.capture_count is uint16_t in the public C API.  Keep the
   * explicit cast here so strict warning settings (for example
   * -Wconversion -Werror) remain clean while documenting the wire width. */
  tsw_write_u16(out + 6, (uint16_t)count);
  for (uint32_t i = 0; i < count; i++) {
    uint32_t node = tsw_wrap_node(match->captures[i].node);
    if (!node) {
      /* The native match has already been consumed, so there is no safe way
       * to replay it after an allocation failure.  Do make sure that handles
       * produced for earlier captures in this match are reclaimed. */
      for (uint32_t j = 0; j < i; j++) {
        uint32_t previous = tsw_read_u32(out + 8u + (size_t)j * 8u);
        if (previous) free(tsw_node(previous));
      }
      tsw_query_cursor_clear_pending(wrapper);
      tsw_set_error("out of memory while wrapping query capture");
      return 0;
    }
    tsw_write_u32(out + 8u + (size_t)i * 8u, node);
    tsw_write_u32(out + 12u + (size_t)i * 8u, match->captures[i].index);
  }
  tsw_query_cursor_clear_pending(wrapper);
  return required;
}

uint32_t tsw_query_cursor_next_capture(uint32_t cursor, uint32_t output_ptr, uint32_t output_capacity) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!wrapper || !wrapper->value) return 0;

  /* Probe the native cursor before checking the output buffer.  This makes
   * the documented zero-on-exhaustion behavior hold even when the caller
   * passes a nil/short buffer, while the pending state preserves the capture
   * for the subsequent retry. */
  if (!tsw_query_cursor_pending_capture(wrapper)) return 0;

  const TSQueryMatch *match = &wrapper->pending_match;
  uint32_t capture_index = wrapper->pending_capture_index;
  if (capture_index >= match->capture_count || !match->captures) {
    tsw_query_cursor_clear_pending(wrapper);
    return 0;
  }
  if (!output_ptr || output_capacity < 16) return 16;
  if (!tsw_require_guest_range(
        output_ptr, 16u, "query capture output range is invalid")) {
    return 0;
  }

  const TSQueryCapture *capture = &match->captures[capture_index];
  uint8_t *out = (uint8_t *)(uintptr_t)output_ptr;
  tsw_write_u32(out + 0, match->id);
  tsw_write_u16(out + 4, match->pattern_index);
  tsw_write_u16(out + 6, (uint16_t)capture_index);
  uint32_t node = tsw_wrap_node(capture->node);
  if (!node) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("out of memory while wrapping query capture");
    return 0;
  }
  tsw_write_u32(out + 8, node);
  tsw_write_u32(out + 12, capture->index);
  if (wrapper->pending_is_capture) {
    /* The native next_capture call has already advanced its state. */
    tsw_query_cursor_clear_pending(wrapper);
  } else {
    /* The pending value came from next_match (typically a size probe).  Keep
     * the remaining captures available to subsequent next_capture calls. */
    wrapper->pending_capture_index++;
    if (wrapper->pending_capture_index >= match->capture_count) {
      tsw_query_cursor_clear_pending(wrapper);
    }
  }
  return 16;
}

/*
 * Predicate-aware variant of tsw_query_cursor_next_capture.  The ordinary
 * capture ABI intentionally returns only the selected capture, which is
 * enough for callers that do not evaluate predicates.  Host-side evaluation
 * of Tree-sitter's built-in text predicates needs the complete capture list
 * associated with the selected native match, though.  This optional export
 * preserves the native next_capture stream and carries that list in one
 * size-probed record, avoiding the ordering changes caused by flattening
 * next_match results on the host.
 */
uint32_t tsw_query_cursor_next_capture_match(
  uint32_t cursor,
  uint32_t output_ptr,
  uint32_t output_capacity
) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!tsw_query_cursor_pending_capture(wrapper)) return 0;

  const TSQueryMatch *match = &wrapper->pending_match;
  uint32_t count = match->capture_count;
  uint32_t selected = wrapper->pending_capture_index;
  if (count != 0 && !match->captures) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("query capture has a null capture list");
    return 0;
  }
  if (selected >= count) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("query capture ordinal is out of range");
    return 0;
  }
  if (!tsw_size_header_product_fits(count, 16u, 8u)) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("query capture list is too large");
    return 0;
  }
  uint64_t required64 = 16u + (uint64_t)count * 8u;
  if (required64 > UINT32_MAX) {
    tsw_query_cursor_clear_pending(wrapper);
    tsw_set_error("query capture output is too large");
    return 0;
  }
  uint32_t required = (uint32_t)required64;
  if (!output_ptr || output_capacity < required) return required;
  if (!tsw_require_guest_range(
        output_ptr, required,
        "query capture output range is invalid")) {
    return 0;
  }

  uint8_t *out = (uint8_t *)(uintptr_t)output_ptr;
  tsw_write_u32(out + 0, match->id);
  tsw_write_u16(out + 4, match->pattern_index);
  tsw_write_u16(out + 6, (uint16_t)count);
  tsw_write_u32(out + 8, selected);
  tsw_write_u32(out + 12, 0);
  for (uint32_t i = 0; i < count; i++) {
    uint32_t node = tsw_wrap_node(match->captures[i].node);
    if (!node) {
      /* Reclaim handles emitted for earlier records.  The native match has
       * already been consumed, so replaying it after an allocation failure
       * is impossible; dropping the pending copy is the only safe choice. */
      for (uint32_t j = 0; j < i; j++) {
        uint32_t previous = tsw_read_u32(out + 16u + (size_t)j * 8u);
        if (previous) free(tsw_node(previous));
      }
      tsw_query_cursor_clear_pending(wrapper);
      tsw_set_error("out of memory while wrapping query captures");
      return 0;
    }
    tsw_write_u32(out + 16u + (size_t)i * 8u, node);
    tsw_write_u32(out + 20u + (size_t)i * 8u, match->captures[i].index);
  }
  tsw_query_cursor_clear_pending(wrapper);
  return required;
}

void tsw_query_cursor_remove_match(uint32_t cursor, uint32_t match_id) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (wrapper && wrapper->value) {
    /* A match retained for a size probe has not yet been emitted; remove it
     * from the native cursor only after discarding the pending copy. */
    if (wrapper->has_pending_match && wrapper->pending_match.id == match_id) {
      tsw_query_cursor_clear_pending(wrapper);
    }
    ts_query_cursor_remove_match(wrapper->value, match_id);
  }
}

uint32_t tsw_query_cursor_did_exceed_match_limit(uint32_t cursor) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  return wrapper && wrapper->value && ts_query_cursor_did_exceed_match_limit(wrapper->value) ? 1u : 0u;
}

uint32_t tsw_query_cursor_match_limit(uint32_t cursor) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  return wrapper && wrapper->value ? ts_query_cursor_match_limit(wrapper->value) : 0;
}

void tsw_query_cursor_set_match_limit(uint32_t cursor, uint32_t limit) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (wrapper && wrapper->value) ts_query_cursor_set_match_limit(wrapper->value, limit);
}

void tsw_query_cursor_set_timeout_micros(uint32_t cursor, uint64_t timeout) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (wrapper && wrapper->value) ts_query_cursor_set_timeout_micros(wrapper->value, timeout);
}

uint64_t tsw_query_cursor_timeout_micros(uint32_t cursor) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  return wrapper && wrapper->value ? ts_query_cursor_timeout_micros(wrapper->value) : 0;
}

uint32_t tsw_query_cursor_set_byte_range(uint32_t cursor, uint32_t start, uint32_t end) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  return wrapper && wrapper->value && ts_query_cursor_set_byte_range(wrapper->value, start, end) ? 1u : 0u;
}

uint32_t tsw_query_cursor_set_point_range(uint32_t cursor, uint64_t start, uint64_t end) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (!wrapper || !wrapper->value) return 0;
  TSPoint start_point = {(uint32_t)(start >> 32), (uint32_t)start};
  TSPoint end_point = {(uint32_t)(end >> 32), (uint32_t)end};
  return ts_query_cursor_set_point_range(wrapper->value, start_point, end_point) ? 1u : 0u;
}

void tsw_query_cursor_set_max_start_depth(uint32_t cursor, uint32_t depth) {
  SWQueryCursor *wrapper = tsw_query_cursor(cursor);
  if (wrapper && wrapper->value) ts_query_cursor_set_max_start_depth(wrapper->value, depth);
}

uint32_t tsw_copy_string(uint32_t string_ptr, uint32_t output_ptr, uint32_t output_capacity) {
  if (!string_ptr) return 0;
  uint32_t length = 0;
  if (!tsw_guest_cstr_len(string_ptr, &length)) {
    tsw_set_error("source string is not a valid guest string");
    return 0;
  }
  const char *source = (const char *)(uintptr_t)string_ptr;
  if (output_ptr && output_capacity) {
    uint32_t n = length < output_capacity - 1 ? length : output_capacity - 1;
    if (!tsw_require_guest_range(
          output_ptr, (uint64_t)n + 1u,
          "string output range is invalid")) {
      return 0;
    }
    memcpy((void *)(uintptr_t)output_ptr, source, n);
    ((char *)(uintptr_t)output_ptr)[n] = '\0';
  }
  return length;
}

uint32_t tsw_last_error_ptr(void) { return (uint32_t)(uintptr_t)tsw_error; }
uint32_t tsw_last_error_len(void) { return (uint32_t)strlen(tsw_error); }
void tsw_clear_error(void) { tsw_ok(); }
