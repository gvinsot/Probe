package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
)

// --rules-file reaches the reviewer's system prompt and is recorded in the
// report with its digest; lint records no rules it could not apply.
func TestCodingRulesReachTheReviewer(t *testing.T) {
	dir := fixture(t)
	var mu sync.Mutex
	var systems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
			t.Errorf("request: %v", err)
		} else {
			mu.Lock()
			systems = append(systems, body.Messages[0].Content)
			mu.Unlock()
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Done"}}]}`))
	}))
	defer server.Close()
	cfg := config.Default("go")
	cfg.Reviewer.Endpoint, cfg.Reviewer.Model = server.URL, "test-model"
	cfg.Reviewer.APIKeyEnv = "PROBE_TEST_REVIEWER_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "test-reviewer-key")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	rules := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(rules, []byte("\n- Never log credentials.\n- Wrap returned errors.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--out", "report", "--rules-file", rules}
	if code := Run(context.Background(), args, &out, &out, "test"); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	const want = "- Never log credentials.\n- Wrap returned errors."
	if len(systems) == 0 || !strings.Contains(systems[0], "<<<CODING_RULES\n"+want+"\nCODING_RULES>>>") {
		t.Fatalf("rules missing from the system prompt: %q", systems)
	}
	r := readReviewerReport(t, dir)
	if r.CodingRules != want || r.CodingRulesSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
		t.Fatalf("recorded rules %q / %q", r.CodingRules, r.CodingRulesSHA256)
	}
	markdown, err := os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if err != nil || !strings.Contains(string(markdown), "> - Never log credentials.") {
		t.Fatalf("markdown lacks the rules: %v", err)
	}

	out.Reset()
	args = []string{"lint", "--repo", dir, "--config", policy, "--out", "report", "--rules", "Never log credentials."}
	if code := Run(context.Background(), args, &out, &out, "test"); code != 0 || !strings.Contains(out.String(), "Coding rules not applied") {
		t.Fatalf("lint exit %d: %s", code, out.String())
	}
	if r := readReviewerReport(t, dir); r.CodingRules != "" || r.CodingRulesSHA256 != "" {
		t.Fatalf("lint recorded rules it did not apply: %+v", r.CodingRules)
	}
}

func TestLoadCodingRules(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rules")
	if err := os.WriteFile(file, []byte(" from file \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadCodingRules("", file); err != nil || got != "from file" {
		t.Fatalf("file: %q, %v", got, err)
	}
	if got, err := loadCodingRules("  ", ""); err != nil || got != "" {
		t.Fatalf("blank: %q, %v", got, err)
	}
	for name, tc := range map[string][2]string{
		"both":      {"x", file},
		"too large": {strings.Repeat("x", 32*1024+1), ""},
		"not utf-8": {"\xff", ""},
		"nul":       {"a\x00b", ""},
		"missing":   {"", file + ".absent"},
	} {
		if _, err := loadCodingRules(tc[0], tc[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
