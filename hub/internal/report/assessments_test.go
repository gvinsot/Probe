package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// assessed is the sample report after a reviewer read its signals: s1 is a
// risk, s2 was set aside, and one hypothesis was dismissed.
func assessed(t *testing.T) *Report {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(sample), &raw); err != nil {
		t.Fatal(err)
	}
	raw["signal_assessments"] = []map[string]any{
		{"signal_id": "s1", "title": "Refunds are no longer checked", "explanation": "The amount guard was removed.", "judgment": "risk", "evidence_ids": []string{}},
		{"signal_id": "s2", "title": "Only a comment", "explanation": "The line is a comment.", "judgment": "no_risk", "rationale": "Line 12 is a comment.", "evidence_ids": []string{"e9"}, "set_aside": true},
	}
	raw["hypotheses"] = append(raw["hypotheses"].([]any), map[string]any{"id": "h3", "title": "Loop may not end", "severity": "high", "status": "DISMISSED", "rationale": "The loop is bounded.", "evidence_ids": []string{"e9"}, "path": "pay/refund.go", "line": 12})
	raw["reviewer_summary"] = "  The refund guard removal is the main risk.  "
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAssessedSignalsShowTheReviewerReading(t *testing.T) {
	r := assessed(t)
	var s1 *Alert
	alerts := r.Alerts()
	for i := range alerts {
		if strings.HasPrefix(alerts[i].ID, "signal:s1") {
			s1 = &alerts[i]
		}
		if alerts[i].ID == "signal:s2" || alerts[i].ID == "issue:h3" {
			t.Errorf("%s was set aside by the reviewer but is still an alert", alerts[i].ID)
		}
	}
	if s1 == nil {
		t.Fatal("signal s1 missing")
	}
	if s1.Title != "Refunds are no longer checked" || s1.OriginalTitle != "validation removed" || s1.Explanation != "The amount guard was removed." || s1.Judgment != JudgmentRisk {
		t.Errorf("s1 = %+v", s1)
	}
	if s1.Detail != "the guard disappeared" || s1.Severity != SeverityHigh {
		t.Errorf("the linter detail and severity must stay: %+v", s1)
	}
}

func TestDismissedAlertsAreListedApartAndChangeNoVerdict(t *testing.T) {
	plain := decodeSample(t)
	r := assessed(t)
	dismissed := r.Dismissed()
	var ids []string
	for _, a := range dismissed {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "issue:h3,signal:s2" {
		t.Fatalf("dismissed = %v", ids)
	}
	if dismissed[1].Rationale != "Line 12 is a comment." || dismissed[1].OriginalTitle != "comment only" {
		t.Errorf("s2 = %+v", dismissed[1])
	}
	s, p := r.Summarize(), plain.Summarize()
	if s.Verdict != p.Verdict || s.ExitCode != p.ExitCode {
		t.Errorf("verdict changed: %+v vs %+v", s, p)
	}
	if s.Dismissed != 2 || s.Suspicions != 1 || s.Counts.Low != 0 {
		t.Errorf("summary = %+v", s)
	}
	v := r.BuildView()
	if len(v.Dismissed) != 2 || v.ReviewerSummary != "The refund guard removal is the main risk." {
		t.Errorf("view dismissed=%d summary=%q", len(v.Dismissed), v.ReviewerSummary)
	}
	if v := plain.BuildView(); v.Dismissed == nil || len(v.Dismissed) != 0 {
		t.Errorf("a report without a reviewer must carry an empty dismissed list, got %v", v.Dismissed)
	}
}

// A dismissed alert hides no line of the active list.
func TestDismissedAlertsHideNoLine(t *testing.T) {
	r := &Report{
		Signals: []Signal{
			{ID: "s1", Kind: "sensitive", Path: "a.go", Line: 5, Side: "new", Severity: "low", Summary: "looks sensitive"},
			{ID: "s2", Kind: "style", Path: "a.go", Line: 5, Side: "new", Severity: "low", Summary: "trailing space"},
		},
		SignalAssessments: []SignalAssessment{
			{SignalID: "s1", Title: "A log message changed", Explanation: "x", Judgment: JudgmentNoRisk, Rationale: "Only the message text changed.", SetAside: true},
			{SignalID: "s2", Title: "Trailing space", Explanation: "y", Judgment: "odd"},
		},
	}
	alerts := r.Alerts()
	if len(alerts) != 1 || alerts[0].ID != "signal:s2" || alerts[0].Line != 5 {
		t.Fatalf("the dismissed s1 must not hide s2: %+v", alerts)
	}
	if alerts[0].Judgment != JudgmentUncertain {
		t.Errorf("an unknown judgment must read as uncertain: %+v", alerts[0])
	}
}

// An issue is never dropped or narrowed by a more severe alert on its line.
func TestDedupeLinesKeepsEveryIssue(t *testing.T) {
	alerts := dedupeLines([]Alert{
		{ID: "signal:s", Kind: KindSignal, Severity: SeverityHigh, Path: "a.go", Line: 1, EndLine: 5, Side: "new"},
		{ID: "issue:u", Kind: KindIssue, Severity: SeverityMedium, Status: "UNVERIFIED", Path: "a.go", Line: 3, EndLine: 3, Side: "new"},
		{ID: "focus:0", Kind: KindFocus, Severity: SeverityLow, Path: "a.go", Line: 3, EndLine: 3, Side: "new"},
	})
	if len(alerts) != 2 || alerts[1].ID != "issue:u" || alerts[1].Line != 3 {
		t.Fatalf("got %+v", alerts)
	}
}

// The hub shows the severity the CLI adjusted, with the linter's beside it,
// and keeps a no_risk signal that was not set aside among the alerts.
func TestAdjustedSeverityComesFromTheCLI(t *testing.T) {
	r := &Report{
		Signals: []Signal{
			{ID: "s1", Kind: "k", Path: "a.go", Line: 1, Side: "new", Severity: "high", Summary: "one"},
			{ID: "s2", Kind: "k", Path: "a.go", Line: 2, Side: "new", Severity: "high", Summary: "two"},
		},
		SignalAssessments: []SignalAssessment{
			{SignalID: "s1", Title: "Harmless rename", Explanation: "x", Judgment: JudgmentNoRisk, Rationale: "r", AdjustedSeverity: "medium"},
			{SignalID: "s2", Title: "Harmless too", Explanation: "y", Judgment: JudgmentNoRisk, Rationale: "r"}, // ai_impacts_criticity=false
		},
	}
	alerts := r.Alerts()
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	byID := map[string]Alert{}
	for _, a := range alerts {
		byID[a.ID] = a
	}
	if a := byID["signal:s1"]; a.Severity != SeverityMedium || a.OriginalSeverity != SeverityHigh {
		t.Errorf("s1 = %+v", a)
	}
	if a := byID["signal:s2"]; a.Severity != SeverityHigh || a.OriginalSeverity != "" || a.Judgment != JudgmentNoRisk {
		t.Errorf("s2 = %+v", a)
	}
	if s := r.Summarize(); s.Counts.High != 1 || s.Counts.Medium != 1 || s.Dismissed != 0 {
		t.Errorf("counts = %+v", s.Counts)
	}
}
