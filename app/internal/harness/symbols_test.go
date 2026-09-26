package harness

import (
	"context"
	"encoding/json"
	"testing"
)

// A lexical answer says why the index did not answer; an index answer is
// returned as the index gave it. Every call is audited under its tool name.
func TestSymbolToolLexicalNotes(t *testing.T) {
	h := fixture(t)
	decode := func(raw json.RawMessage) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := decode(call(t, h, "find_references", map[string]any{"symbol": "Value"}))
	if m["index"] != noIndexNote || m["method"] != "lexical (not semantic symbol resolution)" || len(m["matches"].([]any)) != 1 {
		t.Fatalf("without an index: %+v", m)
	}
	index := &fakeIndex{found: false}
	h.opts.Symbols = index
	m = decode(call(t, h, "inspect_symbol", map[string]any{"symbol": "Value"}))
	if m["index"] != indexMissNote || index.callCount != 1 {
		t.Fatalf("index miss: %+v", m)
	}
	index.found, index.value = true, map[string]any{"method": "go_static_index", "callers": []string{"pkg.Caller"}}
	m = decode(call(t, h, "find_callers", map[string]any{"symbol": "Value", "depth": 2}))
	if _, ok := m["index"]; ok || m["method"] != "go_static_index" || index.depth != 2 {
		t.Fatalf("index answer altered: %+v", m)
	}
	var audited []string
	for _, e := range h.Audit() {
		if e.Status != "OK" {
			t.Fatalf("audit %+v", e)
		}
		audited = append(audited, e.Tool)
	}
	if len(audited) != 3 || audited[0] != "find_references" || audited[1] != "inspect_symbol" || audited[2] != "find_callers" {
		t.Fatalf("audit %q", audited)
	}
	// Index answers never create evidence.
	if len(h.Evidence()) != 0 {
		t.Fatalf("symbol tools created evidence: %+v", h.Evidence())
	}
}

// A cancelled context stops the lexical search and the index alike.
func TestSymbolToolHonorsCancellation(t *testing.T) {
	h := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.symbolTool(ctx, "find_references", "Value", 0); err == nil {
		t.Fatal("cancelled search answered")
	}
}
