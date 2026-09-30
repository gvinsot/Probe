package server

import (
	"net/http"
	"strings"
	"testing"
)

// The owner saves, replaces and clears a repository's coding rules; the
// browser projection carries them and an oversized text is refused.
func TestCodingRulesAreSavedPerRepository(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(nil)
	path := "/api/repos/" + repo.Key + "/rules"

	saved := h.do(http.MethodPut, path, map[string]any{"rules": "  - Never log credentials.\r\n- Wrap errors.\n"})
	if saved.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", saved.Code, saved.Body)
	}
	const want = "- Never log credentials.\n- Wrap errors."
	if got := h.decode(saved)["repo"].(map[string]any)["coding_rules"]; got != want {
		t.Fatalf("public rules = %q", got)
	}
	if stored, _ := h.store.Repo(h.userKey, repo.Key); stored.CodingRules != want {
		t.Fatalf("stored rules = %q", stored.CodingRules)
	}

	for name, body := range map[string]map[string]any{
		"too large":     {"rules": strings.Repeat("x", maxCodingRulesBytes+1)},
		"nul":           {"rules": "a\x00b"},
		"unknown field": {"rules": "x", "extra": true},
	} {
		if res := h.do(http.MethodPut, path, body); res.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, res.Code)
		}
	}
	if stored, _ := h.store.Repo(h.userKey, repo.Key); stored.CodingRules != want {
		t.Fatalf("a refused save changed the rules: %q", stored.CodingRules)
	}

	cleared := h.do(http.MethodPut, path, map[string]any{"rules": "  "})
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear = %d", cleared.Code)
	}
	if _, present := h.decode(cleared)["repo"].(map[string]any)["coding_rules"]; present {
		t.Fatal("cleared rules still projected")
	}
	if res := h.do(http.MethodPut, "/api/repos/unknown/rules", map[string]any{"rules": "x"}); res.Code != http.StatusNotFound {
		t.Errorf("unknown repository: %d", res.Code)
	}
}
