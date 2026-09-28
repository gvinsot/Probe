package reviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func signalReport() *model.Report {
	return &model.Report{Signals: []model.Signal{
		{ID: "signal-1", Kind: "control_flow", Path: "cart.go", Line: 4, Severity: "high", Summary: "Control flow changed"},
		{ID: "signal-2", Kind: "no_test_change", Path: "cart.go", Line: 1, Severity: "medium", Summary: "No test changed"},
	}}
}

// assess_signals is offered with the prompt only when the run has signals,
// in both review modes, and it never reaches the harness.
func TestAssessToolOfferedOnlyWithSignals(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, withSignals := range []bool{false, true} {
			server, bodies := scripted(t)
			r := &model.Report{}
			if withSignals {
				r = signalReport()
			}
			if err := Run(context.Background(), Options{ReadOnly: readOnly, Endpoint: server.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
				t.Fatal(err)
			}
			body := (*bodies)[0]
			encoded, _ := json.Marshal(body["tools"])
			system := body["messages"].([]any)[0].(map[string]any)["content"].(string)
			if strings.Contains(string(encoded), `"`+AssessTool+`"`) != withSignals || strings.Contains(system, assessPrompt) != withSignals {
				t.Fatalf("readOnly=%v signals=%v: assess tool or prompt offered = %v", readOnly, withSignals, !withSignals)
			}
		}
	}
}

func TestAssessRecordsValidAndReportsRejected(t *testing.T) {
	server, bodies := scripted(t, []toolCall{call("a", AssessTool, `{"assessments":[
		{"signal_id":"signal-1","title":"Loop stops one attempt early","explanation":"The retry bound changed from <= to <.","judgment":"risk"},
		{"signal_id":"signal-2","title":"No test","explanation":"Nothing tests the change.","judgment":"no_risk"},
		{"signal_id":"signal-9","title":"Ghost","explanation":"x","judgment":"risk"},
		{"signal_id":"signal-2","title":"Only a comment changed","explanation":"The diff edits a comment.","judgment":"no_risk","rationale":"Line 1 is a comment.","evidence_ids":["evidence-1"]}
	]}`)}, []toolCall{call("b", AssessTool, `{"assessments":[{"signal_id":"signal-1","title":"Retry bound is off by one","explanation":"Three attempts became two.","judgment":"RISK"}]}`)})
	r := signalReport()
	h := &fakeHarness{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, r, h); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("assess_signals reached the harness: %v", h.calls)
	}
	if len(r.SignalAssessments) != 2 {
		t.Fatalf("assessments = %+v", r.SignalAssessments)
	}
	first, second := r.SignalAssessments[0], r.SignalAssessments[1]
	if first.SignalID != "signal-1" || first.Title != "Retry bound is off by one" || first.Judgment != model.AssessmentRisk {
		t.Fatalf("a later assessment must replace the earlier one: %+v", first)
	}
	if second.SignalID != "signal-2" || second.Judgment != model.AssessmentNoRisk || second.EvidenceIDs[0] != "evidence-1" {
		t.Fatalf("second = %+v", second)
	}
	var result map[string]any
	messages := (*bodies)[1]["messages"].([]any)
	if err := json.Unmarshal([]byte(messages[len(messages)-1].(map[string]any)["content"].(string)), &result); err != nil {
		t.Fatal(err)
	}
	rejected := result["rejected"].(map[string]any)
	if !strings.Contains(rejected["signal-9"].(string), "unknown") || len(rejected) != 2 {
		t.Fatalf("rejected = %v", rejected)
	}
}

func TestAssessRejectsMalformedCalls(t *testing.T) {
	r := signalReport()
	for _, args := range []string{`{}`, `{"assessments":[]}`, `{"assessments":[{"signal_id":"signal-1","title":"t","explanation":"e","judgment":"risk","extra":1}]}`, `{"assessments":[` + strings.Repeat(`{"signal_id":"signal-1","title":"t","explanation":"e","judgment":"risk"},`, maxAssessmentsPerCall) + `{"signal_id":"signal-1","title":"t","explanation":"e","judgment":"risk"}]}`} {
		if _, err := assess(r, []byte(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	if len(r.SignalAssessments) != 0 {
		t.Fatalf("recorded %+v", r.SignalAssessments)
	}
}

// finalAnswer serves one final answer with the given text and no tool call.
func finalAnswer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		m := message{Role: "assistant", Content: content}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m, "finish_reason": "stop"}}})
	}))
	t.Cleanup(server.Close)
	return server
}

// The model's closing text is recorded, bounded, as the reviewer summary.
func TestRunRecordsClosingSummary(t *testing.T) {
	server := finalAnswer(t, "  "+strings.Repeat("risk ", 2000)+"  ")
	r := &model.Report{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ReviewerSummary, "risk risk") || len(r.ReviewerSummary) > maxSummaryBytes {
		t.Fatalf("summary: %d bytes", len(r.ReviewerSummary))
	}
}

// A diff that would take more than half of the input budget is sent without
// its hunks, and the model is told to read them with get_diff.
func TestLargeDiffSentWithoutHunks(t *testing.T) {
	lines := make([]model.DiffLine, 400)
	for i := range lines {
		lines[i] = model.DiffLine{Kind: "add", NewLine: i + 1, Content: strings.Repeat("x", 100)}
	}
	for _, large := range []bool{false, true} {
		r := &model.Report{Change: model.Change{Files: []model.ChangedFile{{Path: "big.go", Status: "M", Additions: 1, Hunks: []model.Hunk{{NewStart: 1, NewLines: 1, Lines: lines[:1]}}}}}}
		if large {
			r.Change.Files[0].Hunks[0].Lines = lines
		}
		server, bodies := scripted(t)
		if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", MaxInputBytes: 64 * 1024}, r, &fakeHarness{}); err != nil {
			t.Fatal(err)
		}
		messages := (*bodies)[0]["messages"].([]any)
		user := messages[1].(map[string]any)["content"].(string)
		if strings.Contains(user, `"hunks_omitted":true`) != large || strings.Contains(user, `"big.go"`) != true {
			t.Fatalf("large=%v: user message %.300s", large, user)
		}
		system := messages[0].(map[string]any)["content"].(string)
		if strings.Contains(system, "get_diff and a path") != large {
			t.Fatalf("large=%v: get_diff instruction mismatch", large)
		}
	}
}
