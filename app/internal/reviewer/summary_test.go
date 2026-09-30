package reviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const validSummary = `{"title":"Allow every user through the admin check","overview":"The admin check now returns true for any user.\nThis removes the role test.",
"changes":[{"area":"Authorization","summary":"Allowed no longer compares the user.","files":["auth.go","/etc/passwd"]},{"area":"","summary":"dropped"}],
"behavior_changes":["Non-admin users are allowed"],"risks":["Unverified: every user becomes admin"],"review_focus":["auth.go line 3"],"testing":"No test was executed."}`

// summaryProvider answers with the given contents in turn and records the
// requests.
func summaryProvider(t *testing.T, answers ...string) (string, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		answer := answers[len(answers)-1]
		if n <= len(answers) {
			answer = answers[n-1]
		}
		m := message{Role: "assistant", Content: answer}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m, "finish_reason": "stop"}}})
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1", &requests
}

func summaryReport() *model.Report {
	return &model.Report{
		Intent:     "Simplify the admin check",
		ExitCode:   2,
		Change:     model.Change{Files: []model.ChangedFile{{Path: "auth.go", Status: "M", Additions: 1, Deletions: 1}}},
		Hypotheses: []model.Hypothesis{{ID: "hypothesis-1", Title: "Every user is admin", Severity: "critical", Status: "UNVERIFIED", Path: "auth.go", Line: 3}},
		Unverified: []string{"Reviewer iteration budget exhausted"},
		Checks:     []model.Check{{ID: "check-1", Kind: "test", Status: "PASS"}},
	}
}

func TestSummarizeValidatesTheAnswer(t *testing.T) {
	endpoint, requests := summaryProvider(t, "```json\n"+validSummary+"\n```")
	s, events, err := Summarize(context.Background(), Options{Endpoint: endpoint, Model: "test-model", APIKey: "sk-test-summary-key"}, summaryReport(), SummaryInput{CommitMessages: []string{"Simplify admin check\n\nRefs SHOP-7"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Allow every user through the admin check" || s.Model != "test-model" || len(s.Changes) != 1 || strings.Join(s.Changes[0].Files, ",") != "auth.go" {
		t.Fatalf("summary %+v", s)
	}
	if len(s.Risks) != 1 || len(s.BehaviorChanges) != 1 || s.Testing != "No test was executed." {
		t.Fatalf("summary %+v", s)
	}
	if len(events) != 1 || events[0].Tool != "pr_summary_completion" || events[0].Status != "OK" {
		t.Fatalf("events %+v", events)
	}
	body := (*requests)[0]
	if _, ok := body["tools"]; ok {
		t.Fatal("a summary offers no tools")
	}
	if _, ok := body["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls sent without tools")
	}
	user := body["messages"].([]any)[1].(map[string]any)["content"].(string)
	for _, want := range []string{"Simplify admin check", "Every user is admin", "human review required", `"kind":"test","status":"PASS"`} {
		if !strings.Contains(user, want) {
			t.Fatalf("input lacks %q:\n%s", want, user)
		}
	}
}

func TestSummarizeRetriesOnceThenFails(t *testing.T) {
	endpoint, requests := summaryProvider(t, "Here is a summary: it is fine.", validSummary)
	s, events, err := Summarize(context.Background(), Options{Endpoint: endpoint, Model: "m"}, summaryReport(), SummaryInput{})
	if err != nil || s == nil || len(events) != 2 || len(*requests) != 2 {
		t.Fatalf("retry: %+v %v %d", s, err, len(*requests))
	}
	last := (*requests)[1]["messages"].([]any)
	if !strings.Contains(last[len(last)-1].(map[string]any)["content"].(string), "rejected: no JSON object") {
		t.Fatalf("correction %v", last[len(last)-1])
	}

	endpoint, _ = summaryProvider(t, `{"title":"","overview":""}`)
	if _, events, err := Summarize(context.Background(), Options{Endpoint: endpoint, Model: "m"}, summaryReport(), SummaryInput{}); err == nil || len(events) != 2 {
		t.Fatalf("invalid twice: %v %d", err, len(events))
	}
}

func TestSummarizeBoundsAndLargeChanges(t *testing.T) {
	long := strings.Repeat("word ", 400)
	answer, _ := json.Marshal(map[string]any{"title": long, "overview": long, "risks": []string{long, "", "a", "b", "c", "d", "e", "f", "g", "h", "i"}})
	endpoint, requests := summaryProvider(t, string(answer))
	r := summaryReport()
	hunk := model.Hunk{Lines: []model.DiffLine{{Kind: "add", NewLine: 1, Content: strings.Repeat("x", 4000)}}}
	for i := 0; i < 60; i++ {
		r.Change.Files = append(r.Change.Files, model.ChangedFile{Path: "big.go", Hunks: []model.Hunk{hunk}})
	}
	s, _, err := Summarize(context.Background(), Options{Endpoint: endpoint, Model: "m", MaxInputBytes: 64 * 1024}, r, SummaryInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Title) > maxSummaryTitle || len(s.Overview) > maxSummaryOverview || len(s.Risks) != maxSummaryItems || len(s.Risks[0]) > maxSummaryItem {
		t.Fatalf("bounds: title %d overview %d risks %d", len(s.Title), len(s.Overview), len(s.Risks))
	}
	user := (*requests)[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, `"hunks_omitted":true`) || strings.Contains(user, "xxxxxxxx") {
		t.Fatal("hunks were not omitted from a large change")
	}
}
