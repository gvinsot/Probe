package reviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const validSummary = `{"title":"Allow every user through the admin check","overview":"The admin check now returns true for any user.\nThis removes the role test.",
"changes":[{"area":"Simplify the admin check","summary":"Allowed no longer compares the user.","refs":[{"path":"auth.go","start_line":3},{"path":"/etc/passwd"},{"path":"auth.go","start_line":3}],"signal_ids":["signal-1","signal-9"],"hypothesis_ids":["hypothesis-1"]},{"area":"Cover the admin check","summary":"A test follows.","signal_ids":["signal-1","signal-2"]},{"area":"","summary":"dropped","signal_ids":["signal-2"]}],
"behavior_changes":[{"text":"Non-admin users are allowed","refs":[{"path":"auth.go","start_line":2,"end_line":40}]}],
"risks":[{"text":"The role test is gone","severity":" Medium","refs":[{"path":"auth.go","start_line":9,"side":"old","quote":"if u.Role  != \"admin\" {"}]},{"text":"Unverified: every user becomes admin","severity":"low","hypothesis_ids":["hypothesis-1","hypothesis-9"]},{"text":"Nothing backs this","signal_ids":["signal-9"]},{"text":"Invented code","refs":[{"path":"auth.go","start_line":3,"quote":"return isAdmin(u)"}]}],
"review_focus":[{"text":"The return statement","severity":"low","refs":[{"path":"auth.go","start_line":3,"quote":"return true"}]},{"text":"No lines","refs":[{"path":"auth.go"}]}],
"testing":[]}`

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

// authHunk replaces the role test of Allowed by an unconditional true.
var authHunk = model.Hunk{OldStart: 1, OldLines: 4, NewStart: 1, NewLines: 4, Lines: []model.DiffLine{
	{Kind: "context", OldLine: 1, NewLine: 1, Content: "func Allowed(u User) bool {"},
	{Kind: "delete", OldLine: 2, Content: "\tif u.Role != \"admin\" {"},
	{Kind: "delete", OldLine: 3, Content: "\t\treturn false"},
	{Kind: "add", NewLine: 2, Content: "\t// every user passes"},
	{Kind: "add", NewLine: 3, Content: "\treturn true"},
	{Kind: "context", OldLine: 4, NewLine: 4, Content: "}"},
}}

func summaryReport() *model.Report {
	return &model.Report{
		Intent:     "Simplify the admin check",
		ExitCode:   2,
		Change:     model.Change{Files: []model.ChangedFile{{Path: "auth.go", Status: "M", Additions: 1, Deletions: 1, Hunks: []model.Hunk{authHunk}}}},
		Hypotheses: []model.Hypothesis{{ID: "hypothesis-1", Title: "Every user is admin", Severity: "critical", Status: "UNVERIFIED", Path: "auth.go", Line: 3}},
		Signals: []model.Signal{
			{ID: "signal-1", Kind: "branch_growth", Path: "auth.go", Line: 1, Scope: model.SignalScopeFile, Severity: "low", Summary: "More branching constructs appear in the diff"},
			{ID: "signal-2", Kind: "test_suppression", Path: "auth_test.go", Line: 7, Severity: "medium", Summary: "Type or safety checking suppression added"},
		},
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
	if s.Title != "Allow every user through the admin check" || s.Model != "test-model" || len(s.Changes) != 2 {
		t.Fatalf("summary %+v", s)
	}
	// Unknown files and duplicates are dropped; lines are clamped to the hunks.
	if want := []model.CodeRef{{Path: "auth.go", StartLine: 3}}; !reflect.DeepEqual(s.Changes[0].Refs, want) {
		t.Fatalf("change refs %+v", s.Changes[0].Refs)
	}
	if want := []model.CodeRef{{Path: "auth.go", StartLine: 2, EndLine: 4}}; len(s.BehaviorChanges) != 1 || !reflect.DeepEqual(s.BehaviorChanges[0].Refs, want) {
		t.Fatalf("behavior changes %+v", s.BehaviorChanges)
	}
	// A risk keeps its recorded IDs or the code it quotes, at the lines where
	// the quote really is; one with neither, or quoting code the diff does not
	// hold, is dropped.
	// Its severity is the model's, raised to the findings it cites, and the
	// most severe risks come first.
	wantRisks := []model.PRSummaryRisk{
		{Text: "Unverified: every user becomes admin", Severity: "critical", SignalIDs: []string{}, HypothesisIDs: []string{"hypothesis-1"}, Refs: []model.CodeRef{}},
		{Text: "The role test is gone", Severity: "medium", SignalIDs: []string{}, HypothesisIDs: []string{}, Refs: []model.CodeRef{{Path: "auth.go", StartLine: 2, Side: "old", Quote: `if u.Role  != "admin" {`}}},
	}
	if !reflect.DeepEqual(s.Risks, wantRisks) {
		t.Fatalf("risks %+v", s.Risks)
	}
	// A focus point needs a verified quote.
	if want := []model.CodeRef{{Path: "auth.go", StartLine: 3, Quote: "return true"}}; len(s.ReviewFocus) != 1 || !reflect.DeepEqual(s.ReviewFocus[0].Refs, want) || s.Testing == nil || len(s.Testing) != 0 {
		t.Fatalf("focus %+v testing %+v", s.ReviewFocus, s.Testing)
	}
	// It is rated like a risk: the critical hypothesis on its line raises the
	// model's "low".
	if s.ReviewFocus[0].Severity != "critical" {
		t.Fatalf("focus severity %q", s.ReviewFocus[0].Severity)
	}
	if s.RejectedCitations != 1 {
		t.Fatalf("rejected citations %d", s.RejectedCitations)
	}
	// The dropped citations are sent back once; the second answer stands.
	if len(events) != 2 || events[0].Tool != "pr_summary_completion" || events[0].Status != "OK" {
		t.Fatalf("events %+v", events)
	}
	retry := (*requests)[1]["messages"].([]any)
	correction := retry[len(retry)-1].(map[string]any)["content"].(string)
	for _, want := range []string{"did not match the diff", `the quote "return isAdmin(u)" is not in the new lines of auth.go`, `"/etc/passwd" is not a file of the change`, `the risk "Nothing backs this" cites no recorded ID`, `the review_focus point "No lines" cites no quote`} {
		if !strings.Contains(correction, want) {
			t.Fatalf("correction lacks %q:\n%s", want, correction)
		}
	}
	body := (*requests)[0]
	if _, ok := body["tools"]; ok {
		t.Fatal("a summary offers no tools")
	}
	if _, ok := body["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls sent without tools")
	}
	user := body["messages"].([]any)[1].(map[string]any)["content"].(string)
	// Unknown IDs are dropped, and an ID joins the first area citing it.
	if c := s.Changes; !reflect.DeepEqual(c[0].SignalIDs, []string{"signal-1"}) || !reflect.DeepEqual(c[0].HypothesisIDs, []string{"hypothesis-1"}) ||
		!reflect.DeepEqual(c[1].SignalIDs, []string{"signal-2"}) || !reflect.DeepEqual(c[1].HypothesisIDs, []string{}) || !reflect.DeepEqual(c[1].Refs, []model.CodeRef{}) {
		t.Fatalf("areas %+v", c)
	}
	for _, want := range []string{"Simplify admin check", "Every user is admin", "human review required", `"kind":"test","status":"PASS"`, `"id":"hypothesis-1"`, `"id":"signal-2","kind":"test_suppression","severity":"medium","path":"auth_test.go","line":7`, `"id":"signal-1","kind":"branch_growth","severity":"low","path":"auth.go","summary"`} {
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
	risk := func(text string) map[string]any {
		return map[string]any{"text": text, "severity": "high", "refs": []map[string]any{{"path": "auth.go", "start_line": 3, "quote": "return true"}}}
	}
	risks := []map[string]any{risk(long), risk("")}
	for _, text := range strings.Fields("a b c d e f g h i") {
		risks = append(risks, risk(text))
	}
	answer, _ := json.Marshal(map[string]any{"title": long, "overview": long, "risks": risks})
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
	if len(s.Title) > maxSummaryTitle || len(s.Overview) > maxSummaryOverview || len(s.Risks) != maxSummaryItems || len(s.Risks[0].Text) > maxSummaryItem {
		t.Fatalf("bounds: title %d overview %d risks %d", len(s.Title), len(s.Overview), len(s.Risks))
	}
	user := (*requests)[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, `"hunks_omitted":true`) || strings.Contains(user, "xxxxxxxx") {
		t.Fatal("hunks were not omitted from a large change")
	}
}

func TestSummaryRefsFollowTheDiff(t *testing.T) {
	known := summaryRefs{files: map[string]model.ChangedFile{
		"a.go":    {Path: "a.go", Hunks: []model.Hunk{{OldStart: 10, OldLines: 3, NewStart: 10, NewLines: 5}, {OldStart: 40, OldLines: 2, NewStart: 42, NewLines: 0}}},
		"bin.png": {Path: "bin.png", Binary: true},
	}}
	cases := []struct {
		in   rawRef
		want model.CodeRef
		ok   bool
	}{
		{rawRef{Path: "a.go", StartLine: 11, EndLine: 12}, model.CodeRef{Path: "a.go", StartLine: 11, EndLine: 12}, true},
		{rawRef{Path: "a.go", StartLine: 5, EndLine: 100}, model.CodeRef{Path: "a.go", StartLine: 10, EndLine: 14}, true},
		{rawRef{Path: "a.go", StartLine: 14, EndLine: 3}, model.CodeRef{Path: "a.go", StartLine: 14}, true},
		{rawRef{Path: "a.go", StartLine: 41, Side: "old"}, model.CodeRef{Path: "a.go", StartLine: 41, Side: "old"}, true},
		{rawRef{Path: "a.go", StartLine: 42}, model.CodeRef{Path: "a.go"}, true}, // the hunk adds no line: whole file
		{rawRef{Path: "a.go", StartLine: 12, Side: "left"}, model.CodeRef{Path: "a.go", StartLine: 12}, true},
		{rawRef{Path: "bin.png", StartLine: 1}, model.CodeRef{Path: "bin.png"}, true},
		{rawRef{Path: "b.go", StartLine: 1}, model.CodeRef{}, false},
	}
	for _, c := range cases {
		if got, ok := known.ref(c.in); ok != c.ok || got != c.want {
			t.Errorf("ref(%+v) = %+v %v, want %+v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSummaryQuotesAnchorRefs(t *testing.T) {
	line := func(n int, content string) model.DiffLine {
		return model.DiffLine{Kind: "add", NewLine: n, Content: content}
	}
	file := model.ChangedFile{Path: "a.go", Hunks: []model.Hunk{
		{NewStart: 10, NewLines: 3, Lines: []model.DiffLine{line(10, "\tclose(done)"), line(11, "\treturn nil"), line(12, "}")}},
		{NewStart: 50, NewLines: 2, OldStart: 48, OldLines: 1, Lines: []model.DiffLine{line(50, "\tclose(done)"), line(51, "\tlog.Print(err)"), {Kind: "delete", OldLine: 48, Content: "\treturn err"}}},
	}}
	cases := []struct {
		in       rawRef
		want     model.CodeRef
		rejected bool
	}{
		// The occurrence nearest to the given line wins, and sets the lines.
		{rawRef{Path: "a.go", StartLine: 45, Quote: "close(done)"}, model.CodeRef{Path: "a.go", StartLine: 50, Quote: "close(done)"}, false},
		{rawRef{Path: "a.go", StartLine: 2, Quote: "close(done)"}, model.CodeRef{Path: "a.go", StartLine: 10, Quote: "close(done)"}, false},
		// Whitespace aside, a quote may span lines.
		{rawRef{Path: "a.go", Quote: "close(done)\n    return nil"}, model.CodeRef{Path: "a.go", StartLine: 10, EndLine: 11, Quote: "close(done)\n    return nil"}, false},
		{rawRef{Path: "a.go", StartLine: 48, Side: "old", Quote: "return err"}, model.CodeRef{Path: "a.go", StartLine: 48, Side: "old", Quote: "return err"}, false},
		// Code the diff does not hold, on that side or across hunks, and
		// quotes too short to identify anything are rejected.
		{rawRef{Path: "a.go", Quote: "return err"}, model.CodeRef{}, true},
		{rawRef{Path: "a.go", Quote: "} close(done)"}, model.CodeRef{}, true},
		{rawRef{Path: "a.go", Quote: "close(dome)"}, model.CodeRef{}, true},
		{rawRef{Path: "a.go", Quote: "}"}, model.CodeRef{}, true},
		{rawRef{Path: "a.go", Quote: "{ } ( ) ;;"}, model.CodeRef{}, true},
		{rawRef{Path: "a.go", Quote: strings.Repeat("close(done) ", 30)}, model.CodeRef{}, true},
	}
	for _, c := range cases {
		known := summaryRefs{files: map[string]model.ChangedFile{"a.go": file}}
		got, ok := known.ref(c.in)
		if ok == c.rejected || got != c.want || (known.rejected == 1) != c.rejected || len(known.misses) != known.rejected {
			t.Errorf("ref(%+v) = %+v %v (rejected %d, misses %v), want %+v rejected %v", c.in, got, ok, known.rejected, known.misses, c.want, c.rejected)
		}
	}
}
