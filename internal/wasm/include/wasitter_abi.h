#ifndef WASITTER_ABI_H_
#define WASITTER_ABI_H_

/*
 * A deliberately small, C-compatible ABI used by the Go host.  Every handle
 * is a 32-bit offset in the wasm linear memory (wasm32 pointers fit exactly in
 * uint32_t).  Handles are opaque to callers; in particular, TSNode is never
 * passed across the boundary by value because its layout is an implementation
 * detail of Tree-sitter.
 */

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#if defined(__GNUC__) || defined(__clang__)
#define WASITTER_EXPORT __attribute__((visibility("default")))
#else
#define WASITTER_EXPORT
#endif

#define WASITTER_ABI_VERSION 1u

WASITTER_EXPORT uint32_t tsw_abi_version(void);
WASITTER_EXPORT uint32_t tsw_alloc(uint32_t size);
WASITTER_EXPORT void tsw_free(uint32_t ptr);
WASITTER_EXPORT uint32_t tsw_realloc(uint32_t ptr, uint32_t size);

/* The module's statically linked grammar. */
WASITTER_EXPORT uint32_t tsw_language(void);
WASITTER_EXPORT uint32_t tsw_language_name(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_name_ptr(void);
WASITTER_EXPORT uint32_t tsw_language_name_len(void);
/* The no-argument form is the stable built-in-language API. */
WASITTER_EXPORT uint32_t tsw_language_abi_version(void);
WASITTER_EXPORT uint32_t tsw_language_abi_version_for(uint32_t language);
/* Returns a pointer to the optional three-byte TSLanguageMetadata record. */
WASITTER_EXPORT uint32_t tsw_language_metadata(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_symbol_count(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_state_count(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_field_count(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_field_id_for_name(
  uint32_t language,
  uint32_t name_ptr,
  uint32_t name_len
);
WASITTER_EXPORT uint32_t tsw_language_symbol_type(uint32_t language, uint32_t symbol);
WASITTER_EXPORT uint32_t tsw_language_next_state(uint32_t language, uint32_t state, uint32_t symbol);
WASITTER_EXPORT uint32_t tsw_language_version(uint32_t language);
WASITTER_EXPORT uint32_t tsw_language_symbol_name(uint32_t language, uint32_t symbol);
WASITTER_EXPORT uint32_t tsw_language_field_name(uint32_t language, uint32_t field);
WASITTER_EXPORT uint32_t tsw_language_symbol_for_name(
  uint32_t language,
  uint32_t name_ptr,
  uint32_t name_len
);
/* Variant that preserves Tree-sitter's is_named selector.  The historical
 * three-argument form above is equivalent to is_named=true. */
WASITTER_EXPORT uint32_t tsw_language_symbol_for_name_named(
  uint32_t language,
  uint32_t name_ptr,
  uint32_t name_len,
  uint32_t is_named
);

/* Lookahead iterators expose the symbols accepted by a grammar parse state.
 * Like nodes and cursors, the native TSLookaheadIterator is kept behind an
 * opaque wasm32 handle. */
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_new(uint32_t language, uint32_t state);
WASITTER_EXPORT void tsw_lookahead_iterator_delete(uint32_t iterator);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_reset_state(uint32_t iterator, uint32_t state);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_reset(
  uint32_t iterator,
  uint32_t language,
  uint32_t state
);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_language(uint32_t iterator);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_next(uint32_t iterator);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_current_symbol(uint32_t iterator);
WASITTER_EXPORT uint32_t tsw_lookahead_iterator_current_symbol_name(uint32_t iterator);

WASITTER_EXPORT uint32_t tsw_parser_new(void);
WASITTER_EXPORT void tsw_parser_delete(uint32_t parser);
WASITTER_EXPORT uint32_t tsw_parser_set_language(uint32_t parser, uint32_t language);
WASITTER_EXPORT uint32_t tsw_parser_language(uint32_t parser);
WASITTER_EXPORT uint32_t tsw_parser_set_included_ranges(
  uint32_t parser,
  uint32_t ranges_ptr,
  uint32_t range_count
);
WASITTER_EXPORT uint32_t tsw_parser_included_ranges_into(
  uint32_t parser,
  uint32_t output_ptr,
  uint32_t output_capacity
);
WASITTER_EXPORT uint32_t tsw_parser_parse(
  uint32_t parser,
  uint32_t old_tree,
  uint32_t input_ptr,
  uint32_t input_len
);
WASITTER_EXPORT void tsw_parser_reset(uint32_t parser);
WASITTER_EXPORT void tsw_parser_set_timeout_micros(uint32_t parser, uint64_t timeout);
WASITTER_EXPORT uint32_t tsw_parser_set_timeout(uint32_t parser, uint64_t timeout);
WASITTER_EXPORT uint64_t tsw_parser_timeout_micros(uint32_t parser);
/* Optional cancellation flag bridge.  The pointer addresses a wasm32
 * `size_t` cell that the host may set to a non-zero value while parsing. */
WASITTER_EXPORT uint32_t tsw_parser_set_cancellation_flag(
  uint32_t parser,
  uint32_t flag_ptr
);
WASITTER_EXPORT uint32_t tsw_parser_cancellation_flag(uint32_t parser);

WASITTER_EXPORT void tsw_tree_delete(uint32_t tree);
WASITTER_EXPORT uint32_t tsw_tree_copy(uint32_t tree);
WASITTER_EXPORT uint32_t tsw_tree_root(uint32_t tree);
WASITTER_EXPORT uint32_t tsw_tree_root_with_offset(
  uint32_t tree,
  uint32_t offset_bytes,
  uint64_t offset_point
);
WASITTER_EXPORT uint32_t tsw_tree_language(uint32_t tree);
WASITTER_EXPORT uint32_t tsw_tree_included_ranges_into(
  uint32_t tree,
  uint32_t output_ptr,
  uint32_t output_capacity
);
WASITTER_EXPORT void tsw_tree_edit(uint32_t tree, uint32_t edit_ptr);
WASITTER_EXPORT uint32_t tsw_tree_changed_ranges_into(
  uint32_t old_tree,
  uint32_t new_tree,
  uint32_t output_ptr,
  uint32_t output_capacity
);
/* Returns a pointer to a 24-byte-per-range allocation.  The first uint32 is
 * the count; callers should use tsw_ranges_free after consuming it.  This
 * optional helper is intentionally separate from the _into form so that the
 * two-argument Go ABI remains arity-safe. */
WASITTER_EXPORT uint32_t tsw_tree_get_changed_ranges(uint32_t old_tree, uint32_t new_tree);
WASITTER_EXPORT void tsw_ranges_free(uint32_t ranges, uint32_t count);

/* Node handles own only the small TSNode value, not the underlying tree. */
WASITTER_EXPORT void tsw_node_delete(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_type_ptr(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_type_len(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_type(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_string(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_to_sexp(uint32_t node);
/* Return Tree-sitter's stable TSNode identity (the underlying node pointer).
 * This is distinct from the ABI wrapper handle returned by tsw_wrap_node. */
WASITTER_EXPORT uint32_t tsw_node_id(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_kind_id(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_grammar_type(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_symbol(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_grammar_symbol(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_language(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_start_byte(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_end_byte(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_start_row(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_start_column(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_end_row(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_end_column(uint32_t node);
WASITTER_EXPORT uint64_t tsw_node_start_point(uint32_t node);
WASITTER_EXPORT uint64_t tsw_node_end_point(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_is_null(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_is_named(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_is_missing(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_is_extra(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_has_changes(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_has_error(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_is_error(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_parse_state(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_next_parse_state(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_parent(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_eq(uint32_t node, uint32_t other);
WASITTER_EXPORT uint32_t tsw_node_child_with_descendant(uint32_t node, uint32_t descendant);
WASITTER_EXPORT uint32_t tsw_node_child(uint32_t node, uint32_t index);
WASITTER_EXPORT uint32_t tsw_node_named_child(uint32_t node, uint32_t index);
WASITTER_EXPORT uint32_t tsw_node_child_count(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_named_child_count(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_next_sibling(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_prev_sibling(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_next_named_sibling(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_prev_named_sibling(uint32_t node);
WASITTER_EXPORT uint32_t tsw_node_child_by_field_name(
  uint32_t node,
  uint32_t name_ptr,
  uint32_t name_len
);
WASITTER_EXPORT uint32_t tsw_node_child_by_field_id(uint32_t node, uint32_t field);
WASITTER_EXPORT uint32_t tsw_node_field_name_for_child_ptr(
  uint32_t node,
  uint32_t index
);
WASITTER_EXPORT uint32_t tsw_node_field_name_for_child(uint32_t node, uint32_t index);
WASITTER_EXPORT uint32_t tsw_node_field_name_for_named_child(uint32_t node, uint32_t index);
WASITTER_EXPORT uint32_t tsw_node_first_child_for_byte(uint32_t node, uint32_t byte);
WASITTER_EXPORT uint32_t tsw_node_first_named_child_for_byte(uint32_t node, uint32_t byte);
WASITTER_EXPORT uint32_t tsw_node_descendant_for_byte_range(
  uint32_t node,
  uint32_t start,
  uint32_t end
);
WASITTER_EXPORT uint32_t tsw_node_named_descendant_for_byte_range(
  uint32_t node,
  uint32_t start,
  uint32_t end
);
WASITTER_EXPORT uint32_t tsw_node_descendant_for_point_range(
  uint32_t node,
  uint64_t start,
  uint64_t end
);
WASITTER_EXPORT uint32_t tsw_node_named_descendant_for_point_range(
  uint32_t node,
  uint64_t start,
  uint64_t end
);
WASITTER_EXPORT uint32_t tsw_node_descendant_count(uint32_t node);
WASITTER_EXPORT void tsw_node_edit(uint32_t node, uint32_t edit_ptr);

/*
 * Tree cursors are stateful C structs.  They are therefore represented by an
 * allocated opaque handle instead of passing TSTreeCursor by value over the
 * wasm ABI.  Functions whose names begin with tsw_cursor_ are canonical;
 * tsw_tree_cursor_* aliases are exported by the bridge for callers that use
 * Tree-sitter's original terminology.
 */
WASITTER_EXPORT uint32_t tsw_cursor_new(uint32_t node);
WASITTER_EXPORT void tsw_cursor_delete(uint32_t cursor);
WASITTER_EXPORT void tsw_cursor_reset(uint32_t cursor, uint32_t node);
WASITTER_EXPORT void tsw_cursor_reset_to(uint32_t cursor, uint32_t other);
WASITTER_EXPORT uint32_t tsw_cursor_current_node(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_current_field_name_ptr(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_current_field_name_len(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_current_field_name(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_current_field_id(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_goto_parent(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_goto_next_sibling(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_goto_previous_sibling(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_goto_first_child(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_goto_last_child(uint32_t cursor);
WASITTER_EXPORT void tsw_cursor_goto_descendant(uint32_t cursor, uint32_t index);
WASITTER_EXPORT uint32_t tsw_cursor_current_descendant_index(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_cursor_current_depth(uint32_t cursor);
WASITTER_EXPORT int64_t tsw_cursor_goto_first_child_for_byte(uint32_t cursor, uint32_t byte);
WASITTER_EXPORT int64_t tsw_cursor_goto_first_child_for_point(uint32_t cursor, uint64_t point);
WASITTER_EXPORT uint32_t tsw_cursor_copy(uint32_t cursor);
/* Tree-sitter terminology aliases for the cursor functions above. */
WASITTER_EXPORT uint32_t tsw_tree_cursor_new(uint32_t node);
WASITTER_EXPORT void tsw_tree_cursor_delete(uint32_t cursor);
WASITTER_EXPORT void tsw_tree_cursor_reset(uint32_t cursor, uint32_t node);
WASITTER_EXPORT void tsw_tree_cursor_reset_to(uint32_t cursor, uint32_t other);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_node(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_field_name_ptr(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_field_name_len(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_field_name(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_field_id(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_parent(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_next_sibling(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_previous_sibling(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_prev_sibling(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_first_child(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_goto_last_child(uint32_t cursor);
WASITTER_EXPORT void tsw_tree_cursor_goto_descendant(uint32_t cursor, uint32_t index);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_descendant_index(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_tree_cursor_current_depth(uint32_t cursor);
WASITTER_EXPORT int64_t tsw_tree_cursor_goto_first_child_for_byte(uint32_t cursor, uint32_t byte);
WASITTER_EXPORT int64_t tsw_tree_cursor_goto_first_child_for_point(uint32_t cursor, uint64_t point);
WASITTER_EXPORT uint32_t tsw_tree_cursor_copy(uint32_t cursor);

/* Native Tree-sitter queries.  tsw_query_new writes an 8-byte little-endian
 * error record (error byte offset, then TSQueryError) to error_out_ptr when
 * that pointer is non-zero. */
WASITTER_EXPORT uint32_t tsw_query_new(
  uint32_t language,
  uint32_t source_ptr,
  uint32_t source_len,
  uint32_t error_out_ptr
);
WASITTER_EXPORT void tsw_query_delete(uint32_t query);
WASITTER_EXPORT uint32_t tsw_query_pattern_count(uint32_t query);
WASITTER_EXPORT uint32_t tsw_query_capture_count(uint32_t query);
WASITTER_EXPORT uint32_t tsw_query_string_count(uint32_t query);
WASITTER_EXPORT uint32_t tsw_query_start_byte_for_pattern(uint32_t query, uint32_t pattern);
WASITTER_EXPORT uint32_t tsw_query_end_byte_for_pattern(uint32_t query, uint32_t pattern);
/* Writes one 8-byte LE record per predicate step (type u32, value_id u32)
 * and returns the required byte count. */
WASITTER_EXPORT uint32_t tsw_query_predicates_for_pattern_into(
  uint32_t query,
  uint32_t pattern,
  uint32_t output_ptr,
  uint32_t output_capacity
);
WASITTER_EXPORT uint32_t tsw_query_is_pattern_rooted(uint32_t query, uint32_t pattern);
WASITTER_EXPORT uint32_t tsw_query_is_pattern_non_local(uint32_t query, uint32_t pattern);
WASITTER_EXPORT uint32_t tsw_query_is_pattern_guaranteed_at_step(uint32_t query, uint32_t offset);
WASITTER_EXPORT uint32_t tsw_query_capture_name_ptr(uint32_t query, uint32_t index);
WASITTER_EXPORT uint32_t tsw_query_capture_name_len(uint32_t query, uint32_t index);
WASITTER_EXPORT uint32_t tsw_query_capture_name(uint32_t query, uint32_t index);
WASITTER_EXPORT uint32_t tsw_query_capture_quantifier_for_id(
  uint32_t query,
  uint32_t pattern,
  uint32_t capture
);
WASITTER_EXPORT uint32_t tsw_query_string_value_ptr(uint32_t query, uint32_t index);
WASITTER_EXPORT uint32_t tsw_query_string_value_len(uint32_t query, uint32_t index);
WASITTER_EXPORT uint32_t tsw_query_string_value(uint32_t query, uint32_t index);
WASITTER_EXPORT void tsw_query_disable_capture(uint32_t query, uint32_t name_ptr, uint32_t name_len);
WASITTER_EXPORT void tsw_query_disable_pattern(uint32_t query, uint32_t pattern);

WASITTER_EXPORT uint32_t tsw_query_cursor_new(void);
WASITTER_EXPORT void tsw_query_cursor_delete(uint32_t cursor);
WASITTER_EXPORT void tsw_query_cursor_exec(uint32_t cursor, uint32_t query, uint32_t node);
/* Return zero when exhausted; otherwise return the required output size.  The
 * output starts with an 8-byte LE header (id u32, pattern_index u16,
 * capture_count u16), followed by capture_count records of 8 bytes each
 * (node handle u32, capture index u32). */
WASITTER_EXPORT uint32_t tsw_query_cursor_next_match(
  uint32_t cursor,
  uint32_t output_ptr,
  uint32_t output_capacity
);
/* Return zero when exhausted.  Otherwise write a 16-byte LE record:
 * id u32, pattern_index u16, capture ordinal u16, node handle u32,
 * capture index u32.  Passing a nil/short output buffer performs a size
 * probe (returns 16) and retains the capture for the next call. */
WASITTER_EXPORT uint32_t tsw_query_cursor_next_capture(
  uint32_t cursor,
  uint32_t output_ptr,
  uint32_t output_capacity
);
/* Optional predicate-aware capture form.  It follows the native
 * ts_query_cursor_next_capture ordering while also returning the complete
 * match needed by host-side predicate evaluation.  The output starts with a
 * 16-byte LE header: match id u32, pattern index u16, capture count u16,
 * selected capture ordinal u32, and a reserved u32.  It is followed by
 * capture_count records of 8 bytes (node handle u32, capture index u32).
 * A nil/short output buffer performs a size probe and retains the pending
 * native capture for the next call. */
WASITTER_EXPORT uint32_t tsw_query_cursor_next_capture_match(
  uint32_t cursor,
  uint32_t output_ptr,
  uint32_t output_capacity
);
WASITTER_EXPORT void tsw_query_cursor_remove_match(uint32_t cursor, uint32_t match_id);
WASITTER_EXPORT uint32_t tsw_query_cursor_did_exceed_match_limit(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_query_cursor_match_limit(uint32_t cursor);
WASITTER_EXPORT void tsw_query_cursor_set_match_limit(uint32_t cursor, uint32_t limit);
WASITTER_EXPORT void tsw_query_cursor_set_timeout_micros(uint32_t cursor, uint64_t timeout);
WASITTER_EXPORT uint64_t tsw_query_cursor_timeout_micros(uint32_t cursor);
WASITTER_EXPORT uint32_t tsw_query_cursor_set_byte_range(uint32_t cursor, uint32_t start, uint32_t end);
WASITTER_EXPORT uint32_t tsw_query_cursor_set_point_range(uint32_t cursor, uint64_t start, uint64_t end);
WASITTER_EXPORT void tsw_query_cursor_set_max_start_depth(uint32_t cursor, uint32_t depth);

/* Copy a NUL-terminated Tree-sitter string into caller memory.
 * Returns the required byte count (excluding NUL); truncation is allowed. */
WASITTER_EXPORT uint32_t tsw_copy_string(
  uint32_t string_ptr,
  uint32_t output_ptr,
  uint32_t output_capacity
);

WASITTER_EXPORT uint32_t tsw_last_error_ptr(void);
WASITTER_EXPORT uint32_t tsw_last_error_len(void);
WASITTER_EXPORT void tsw_clear_error(void);

#ifdef __cplusplus
}
#endif

#endif /* WASITTER_ABI_H_ */
