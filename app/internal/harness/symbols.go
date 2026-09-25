package harness

// The symbol tools (F6a): find_references, inspect_symbol and find_callers.
// A configured SymbolIndex (the static Go index built by package symbols)
// answers first; the lexical search answers otherwise, with a note saying why.

import (
	"context"
	"errors"
)

// SymbolIndex answers find_references, inspect_symbol and find_callers from a
// static index. found is false when the index has no answer for the symbol,
// in which case the harness falls back to a lexical search. The value must be
// JSON-encodable. package symbols satisfies it structurally, without importing
// harness.
type SymbolIndex interface {
	Query(ctx context.Context, tool, symbol string, depth int) (any, bool, error)
}

// Notes added to a lexical answer of a symbol tool.
const (
	indexMissNote = "The symbol does not name a Go function or method in the static Go index; this answer is lexical."
	noIndexNote   = "No static Go index exists for this run (no changed Go file, the index is unavailable, or --impact=false); this answer is lexical."
)

// symbolTool serves find_references, inspect_symbol and find_callers. depth is
// accepted from 0 to 3; for find_callers 0 means 1, and Call refuses a non-zero
// depth for the other tools. Index answers are observations from static
// analysis and never create evidence.
func (h *Harness) symbolTool(ctx context.Context, tool, symbol string, depth int) (any, error) {
	if depth < 0 || depth > 3 {
		return nil, errors.New("depth must be between 1 and 3")
	}
	if tool == "find_callers" && depth == 0 {
		depth = 1
	}
	if symbol == "" || len(symbol) > 256 {
		return nil, errors.New("search query must contain 1 to 256 bytes")
	}
	note := noIndexNote
	if h.opts.Symbols != nil {
		value, found, err := h.opts.Symbols.Query(ctx, tool, symbol, depth)
		if err != nil {
			return nil, err
		}
		if found {
			return value, nil
		}
		note = indexMissNote
	}
	value, err := h.search(ctx, symbol, true)
	if err != nil {
		return nil, err
	}
	if m, ok := value.(map[string]any); ok {
		m["index"] = note
	}
	return value, nil
}
