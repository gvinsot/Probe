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
	feedbackJSON := []byte(`{"topics":[{"topic":"signal:no_test_change","useful":0,"not_useful":4,"changed":0,"unchanged":5}],"comments":[{"topic":"issue","vote":"down","comment":"Rounding is intentional."}]}`)
	feedback := filepath.Join(t.TempDir(), "feedback.json")
	if err := os.WriteFile(feedback, feedbackJSON, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--out", "report", "--rules-file", rules, "--feedback-file", feedback}
	if code := Run(context.Background(), args, &out, &out, "test"); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	const want = "- Never log credentials.\n- Wrap returned errors."
	if len(systems) == 0 || !strings.Contains(systems[0], "<<<CODING_RULES\n"+want+"\nCODING_RULES>>>") {
		t.Fatalf("rules missing from the system prompt: %q", systems)
	}
	if !strings.Contains(systems[0], "- topic signal:no_test_change: useful 0, not useful 4") || !strings.Contains(systems[0], "voted not useful: Rounding is intentional.") {
		t.Fatalf("feedback missing from the system prompt: %q", systems[0])
	}
	r := readReviewerReport(t, dir)
	if r.TeamFeedback == nil || len(r.TeamFeedback.Topics) != 1 || r.TeamFeedbackSHA256 != fmt.Sprintf("%x", sha256.Sum256(feedbackJSON)) {
		t.Fatalf("recorded feedback %+v / %q", r.TeamFeedback, r.TeamFeedbackSHA256)
	}
	if r.CodingRules != want || r.CodingRulesSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
		t.Fatalf("recorded rules %q / %q", r.CodingRules, r.CodingRulesSHA256)
	}
	markdown, err := os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if err != nil || !strings.Contains(string(markdown), "> - Never log credentials.") {
		t.Fatalf("markdown lacks the rules: %v", err)
	}

	out.Reset()
	args = []string{"lint", "--repo", dir, "--config", policy, "--out", "report", "--rules", "Never log credentials.", "--feedback-file", feedback}
	if code := Run(context.Background(), args, &out, &out, "test"); code != 0 || !strings.Contains(out.String(), "Coding rules not applied") || !strings.Contains(out.String(), "Team feedback not applied") {
		t.Fatalf("lint exit %d: %s", code, out.String())
	}
	if r := readReviewerReport(t, dir); r.CodingRules != "" || r.CodingRulesSHA256 != "" || r.TeamFeedback != nil {
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

func TestLoadTeamFeedback(t *testing.T) {
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "feedback.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if f, sum, err := loadTeamFeedback(""); f != nil || sum != "" || err != nil {
		t.Fatal("unset flag")
	}
	if f, _, err := loadTeamFeedback(write(`{"topics":[],"comments":[]}`)); f != nil || err != nil {
		t.Fatalf("empty feedback: %+v, %v", f, err)
	}
	f, sum, err := loadTeamFeedback(write(`{"topics":[{"topic":"issue","useful":2,"not_useful":0,"changed":1,"unchanged":0}]}`))
	if err != nil || f == nil || f.Comments == nil || len(sum) != 64 {
		t.Fatalf("valid feedback: %+v, %q, %v", f, sum, err)
	}
	for name, content := range map[string]string{
		"unknown field":  `{"topics":[],"extra":1}`,
		"negative count": `{"topics":[{"topic":"issue","useful":-1,"not_useful":0,"changed":0,"unchanged":0}]}`,
		"unnamed topic":  `{"topics":[{"topic":" ","useful":1,"not_useful":0,"changed":0,"unchanged":0}]}`,
		"bad vote":       `{"comments":[{"topic":"issue","vote":"meh","comment":"x"}]}`,
		"empty comment":  `{"comments":[{"topic":"issue","comment":" "}]}`,
		"two objects":    `{"topics":[]} {"topics":[]}`,
		"too large":      `{"comments":[{"topic":"issue","comment":"` + strings.Repeat("x", 64*1024) + `"}]}`,
		"not an object":  `[]`,
	} {
		if _, _, err := loadTeamFeedback(write(content)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
