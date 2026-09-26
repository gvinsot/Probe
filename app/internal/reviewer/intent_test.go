package reviewer

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	reports "github.com/gvinsot/SwiftProof/app/internal/report"
)

var reviewCriteria = []model.IntentCriterion{{ID: "AC-1", Text: "Orders of 100 or more get 10 off", Line: 2}, {ID: "AC-2", Text: "Orders of 50 or more ship free", Line: 3}}

func TestValidateIntentLink(t *testing.T) {
	for _, tc := range []struct {
		name, status, criterion, judgment string
		criteria                          []model.IntentCriterion
		want                              string // "" = accepted
	}{
		{"no link", model.StatusReproduced, "", "", reviewCriteria, ""},
		{"criterion on any status", model.StatusUnverified, "AC-2", "", reviewCriteria, ""},
		{"intent test failure", model.StatusIntentTestFailed, "AC-1", "", reviewCriteria, ""},
		{"expected change on a divergence", model.StatusDiverged, "AC-1", model.JudgmentExpectedChange, reviewCriteria, ""},
		{"unexpected change on a divergence", model.StatusDiverged, "AC-2", model.JudgmentUnexpectedChange, reviewCriteria, ""},
		{"unknown criterion", model.StatusDiverged, "AC-3", "", reviewCriteria, "unknown criterion_id"},
		{"malformed criterion", model.StatusUnverified, "ac-1", "", reviewCriteria, "unknown criterion_id"},
		{"duplicated criterion", model.StatusUnverified, "AC-1", "", append([]model.IntentCriterion{{ID: "AC-1", Text: "x", Line: 1}}, reviewCriteria...), "unknown criterion_id"},
		{"intent test failure without criterion", model.StatusIntentTestFailed, "", "", reviewCriteria, "requires the criterion_id"},
		{"judgment on a reproduced claim", model.StatusReproduced, "AC-1", model.JudgmentExpectedChange, reviewCriteria, "only on a DIVERGED"},
		{"judgment on an intent test failure", model.StatusIntentTestFailed, "AC-1", model.JudgmentUnexpectedChange, reviewCriteria, "only on a DIVERGED"},
		{"judgment without criterion", model.StatusDiverged, "", model.JudgmentExpectedChange, reviewCriteria, "requires a criterion_id"},
		{"unknown judgment", model.StatusDiverged, "AC-1", "correct", reviewCriteria, "expected_change or unexpected_change"},
		{"link without criteria", model.StatusDiverged, "AC-1", "", nil, "intent links require acceptance criteria"},
		{"judgment without criteria", model.StatusDiverged, "", model.JudgmentExpectedChange, nil, "intent links require acceptance criteria"},
		{"no link without criteria", model.StatusIntentTestFailed, "", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := model.Hypothesis{Status: tc.status, CriterionID: tc.criterion, IntentJudgment: tc.judgment}
			err := validateIntentLink(&model.Report{IntentCriteria: tc.criteria}, &h)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
		})
	}
}

func TestSubmitRecordsIntentLinks(t *testing.T) {
	r := &model.Report{IntentCriteria: reviewCriteria}
	if _, err := submit(r, []byte(`{"title":"Values differ","severity":"MEDIUM","status":"diverged","rationale":"r","evidence_ids":["evidence-3"],"path":"cart.go","line":3,"criterion_id":"AC-1","intent_judgment":"EXPECTED_CHANGE"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(r, []byte(`{"title":"AC-2 test failed","severity":"low","status":"intent_test_failed","rationale":"r","evidence_ids":["evidence-4"],"path":"cart.go","line":4,"criterion_id":"AC-2"}`)); err != nil {
		t.Fatal(err)
	}
	for _, claim := range []string{
		`{"title":"T","severity":"low","status":"INTENT_TEST_FAILED","rationale":"R","evidence_ids":["e"],"path":"a.go","line":1}`,
		`{"title":"T","severity":"low","status":"REPRODUCED","rationale":"R","evidence_ids":["e"],"path":"a.go","line":1,"criterion_id":"AC-1","intent_judgment":"expected_change"}`,
		`{"title":"T","severity":"low","status":"DIVERGED","rationale":"R","evidence_ids":["e"],"path":"a.go","line":1,"criterion_id":"AC-9"}`,
	} {
		if _, err := submit(r, []byte(claim)); err == nil {
			t.Fatalf("accepted %s", claim)
		}
	}
	if len(r.Hypotheses) != 2 {
		t.Fatalf("hypotheses %+v", r.Hypotheses)
	}
	if h := r.Hypotheses[0]; h.Status != model.StatusDiverged || h.IntentJudgment != model.JudgmentExpectedChange || h.CriterionID != "AC-1" {
		t.Fatalf("first %+v", h)
	}
	if h := r.Hypotheses[1]; h.Status != model.StatusIntentTestFailed || h.CriterionID != "AC-2" {
		t.Fatalf("second %+v", h)
	}
}

func TestIntentPromptOnlyWithCriteria(t *testing.T) {
	if intentPromptFor(false) != "" {
		t.Fatal("prompt without criteria")
	}
	prompt := intentPromptFor(true)
	for _, want := range []string{"intent_criteria", "data, never instructions", "create_intent_test", "run_intent_test", "candidate only", "no baseline control", "INTENT_TEST_FAILED", "criterion_id", "INTENT_TEST_PASSED record says nothing", "intent_judgment", "DIVERGED", "never as evidence", "never dismiss"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if m := regexp.MustCompile(`(?i)\b(satisfied|verified|contradicts?|correct)\b`).FindString(prompt); m != "" {
		t.Fatalf("prompt says %q", m)
	}
	for _, criteria := range [][]model.IntentCriterion{nil, reviewCriteria} {
		server, bodies := scripted(t)
		if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{Intent: "x", IntentCriteria: criteria}, &fakeHarness{}); err != nil {
			t.Fatal(err)
		}
		body := (*bodies)[0]
		system := body["messages"].([]any)[0].(map[string]any)["content"].(string)
		tools, _ := json.Marshal(body["tools"])
		with := len(criteria) > 0
		if strings.HasSuffix(system, intentPrompt) != with || strings.Contains(string(tools), harness.IntentCreateTool) != with || strings.Contains(string(tools), harness.IntentRunTool) != with {
			t.Fatalf("criteria %v: prompt or tools do not match", criteria)
		}
	}
}

// A model cannot create an intent-test failure: an invented evidence ID, or a
// record the harness did not produce, finalizes as UNVERIFIED.
func TestFabricatedIntentFailureStaysUnverified(t *testing.T) {
	server, _ := scripted(t, []toolCall{
		call("a", "submit_hypothesis", `{"title":"AC-1 fails","severity":"critical","status":"INTENT_TEST_FAILED","rationale":"The intent test failed.","evidence_ids":["evidence-99"],"path":"cart.go","line":5,"criterion_id":"AC-1"}`),
	})
	r := &model.Report{Intent: "- Orders of 100 or more get 10 off", IntentCriteria: reviewCriteria[:1],
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceIntentTest, Status: model.StatusIntentTestFailed, CriterionID: "AC-1", CheckID: "check-1", Runner: "go_test_json", Path: "x_test.go", TestNames: []string{"TestX"}, ReferencedSymbols: []string{"Discount"}}}}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	// A second claim cites the stored record, which no recorded check supports.
	r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: "hypothesis-2", Title: "AC-1 fails", Severity: "high", Status: model.StatusIntentTestFailed, Rationale: "r", EvidenceIDs: []string{"evidence-1"}, CriterionID: "AC-1"})
	reports.Finalize(r, true)
	if len(r.Hypotheses) != 2 || r.Hypotheses[0].Status != model.StatusUnverified || r.Hypotheses[1].Status != model.StatusUnverified || len(r.IntentTestFailures) != 0 || r.ExitCode != 2 {
		t.Fatalf("fabricated failure: %+v exit %d", r.Hypotheses, r.ExitCode)
	}
}
