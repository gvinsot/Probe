package report

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func assessedReport() *model.Report {
	return &model.Report{
		Signals: []model.Signal{
			{ID: "signal-1", Kind: "control_flow", Path: "cart.go", Line: 4, Severity: "high", Summary: "Control flow changed"},
			{ID: "signal-2", Kind: "no_test_change", Path: "cart.go", Line: 1, Severity: "medium", Summary: "No test changed"},
			{ID: "signal-3", Kind: "comment", Path: "cart.go", Line: 9, Severity: "low", Summary: "Comment changed"},
		},
		Evidence: []model.Evidence{
			{ID: "evidence-1", Kind: model.EvidenceSourceObservation, Description: "Candidate source lines 1-9", Path: "cart.go", Output: "9: // total", Status: model.StatusObserved},
			{ID: "evidence-2", Kind: model.EvidenceSourceObservation, Path: "cart.go", Status: model.StatusObserved}, // no observed text: not verified
		},
		SignalAssessments: []model.SignalAssessment{
			{SignalID: "signal-3", Title: "Only a comment changed", Explanation: "The diff rewords a comment.", Judgment: model.AssessmentNoRisk, Rationale: "Line 9 is a comment.", EvidenceIDs: []string{"evidence-1"}},
			{SignalID: "signal-2", Title: "Nothing tests the new branch", Explanation: "No test was added.", Judgment: model.AssessmentNoRisk, Rationale: "Trust me.", EvidenceIDs: []string{"evidence-2"}},
			{SignalID: "signal-1", Title: "Retry bound is off by one", Explanation: "Three attempts became two.", Judgment: "RISK"},
			{SignalID: "signal-1", Title: "Duplicate", Explanation: "Dropped.", Judgment: model.AssessmentNoRisk},
			{SignalID: "signal-9", Title: "Unknown signal", Explanation: "Dropped.", Judgment: model.AssessmentRisk},
			{SignalID: "signal-3", Title: "Weird", Explanation: "Dropped as a duplicate.", Judgment: "maybe"},
		},
	}
}

// no_risk survives only with a rationale citing a verified source
// observation; unknown and repeated signals are dropped; the list follows
// the signal order; and Finalize stays idempotent.
func TestFinalizeAssessments(t *testing.T) {
	r := assessedReport()
	Finalize(r, true)
	got := map[string]string{}
	var order []string
	for _, a := range r.SignalAssessments {
		got[a.SignalID] = a.Judgment
		order = append(order, a.SignalID)
		if a.EvidenceIDs == nil {
			t.Fatalf("%s: nil evidence IDs", a.SignalID)
		}
	}
	want := map[string]string{"signal-1": model.AssessmentRisk, "signal-2": model.AssessmentUncertain, "signal-3": model.AssessmentNoRisk}
	if !reflect.DeepEqual(got, want) || strings.Join(order, ",") != "signal-1,signal-2,signal-3" {
		t.Fatalf("assessments = %+v", r.SignalAssessments)
	}
	if r.SignalAssessments[0].Title != "Retry bound is off by one" {
		t.Fatalf("the first assessment of a signal must be kept: %+v", r.SignalAssessments[0])
	}
	exit := r.ExitCode
	before := append([]model.SignalAssessment(nil), r.SignalAssessments...)
	Finalize(r, true)
	if !reflect.DeepEqual(before, r.SignalAssessments) || r.ExitCode != exit {
		t.Fatal("Finalize is not idempotent over assessments")
	}
}

// Without --ai-impacts-criticality, assessments change no exit code or review
// target.
func TestAssessmentsChangeNoConclusion(t *testing.T) {
	plain := assessedReport()
	plain.SignalAssessments = nil
	Finalize(plain, true)
	assessed := assessedReport()
	Finalize(assessed, true)
	if plain.ExitCode != assessed.ExitCode || !reflect.DeepEqual(plain.ReviewTargets, assessed.ReviewTargets) || !reflect.DeepEqual(plain.ReviewSurface, assessed.ReviewSurface) {
		t.Fatalf("assessments changed a conclusion: exit %d vs %d", plain.ExitCode, assessed.ExitCode)
	}
}

// criticalityReport has one kept no_risk reading per severity, plus a no_risk
// reading without a verified observation, which stays uncertain.
func criticalityReport() *model.Report {
	r := &model.Report{
		AIImpactsCriticality: true,
		Change: model.Change{Files: []model.ChangedFile{{Path: "a.go", Status: "M", Additions: 5, Hunks: []model.Hunk{{NewStart: 1, NewLines: 5, Lines: []model.DiffLine{
			{Kind: "add", NewLine: 1, Content: "a"}, {Kind: "add", NewLine: 2, Content: "b"}, {Kind: "add", NewLine: 3, Content: "c"}, {Kind: "add", NewLine: 4, Content: "d"}, {Kind: "add", NewLine: 5, Content: "e"},
		}}}}}},
		Checks:   []model.Check{{ID: "check-1", Kind: model.CheckTest, Status: "PASS"}},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceSourceObservation, Path: "a.go", Output: "1: a", Status: model.StatusObserved}},
	}
	for i, sev := range []string{"critical", "high", "medium", "low", "high"} {
		id := fmt.Sprintf("signal-%d", i+1)
		r.Signals = append(r.Signals, model.Signal{ID: id, Kind: "k", Path: "a.go", Line: i + 1, Side: "new", Severity: sev, Summary: "summary " + id})
		evidence := []string{"evidence-1"}
		if i == 4 {
			evidence = []string{"evidence-9"} // unknown: the reading stays uncertain
		}
		r.SignalAssessments = append(r.SignalAssessments, model.SignalAssessment{SignalID: id, Title: "t", Explanation: "e", Judgment: model.AssessmentNoRisk, Rationale: "harmless", EvidenceIDs: evidence})
	}
	return r
}

func TestAIImpactsCriticalityLowersSeverities(t *testing.T) {
	r := criticalityReport()
	Finalize(r, true)
	type got struct {
		adjusted string
		aside    bool
	}
	want := []got{{"high", false}, {"medium", false}, {"low", false}, {"", true}, {"", false}}
	check := func(when string) {
		for i, a := range r.SignalAssessments {
			if g := (got{a.AdjustedSeverity, a.SetAside}); g != want[i] {
				t.Errorf("%s: %s = %+v, want %+v", when, a.SignalID, g, want[i])
			}
			if r.Signals[i].Severity != criticalityReport().Signals[i].Severity {
				t.Errorf("%s: the linter severity of %s changed", when, a.SignalID)
			}
		}
	}
	check("first Finalize")
	targets := fmt.Sprint(r.ReviewTargets)
	exit := r.ExitCode
	Finalize(r, true)
	check("second Finalize")
	if fmt.Sprint(r.ReviewTargets) != targets || r.ExitCode != exit {
		t.Fatal("Finalize is not idempotent over adjusted severities")
	}
	for _, target := range r.ReviewTargets {
		for _, id := range target.SignalIDs {
			if id == "signal-4" {
				t.Fatal("a set-aside signal must be no review target")
			}
		}
		if target.StartLine == 1 && target.Severity != "high" {
			t.Fatalf("the target of signal-1 keeps severity %s, want the adjusted high", target.Severity)
		}
	}
	if r.ReviewSurface.FocusedLines != 4 {
		t.Fatalf("focused lines = %d, want 4 (line 4 was set aside)", r.ReviewSurface.FocusedLines)
	}
	md := string(Markdown(r))
	for _, want := range []string{"--ai-impacts-criticality)", "signal-1; linter: summary signal-1; severity high, lowered from critical", "signal-4; linter: summary signal-4; set aside"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}

	// Turning the flag off on the persisted report clears every adjustment.
	r.AIImpactsCriticality = false
	Finalize(r, true)
	for _, a := range r.SignalAssessments {
		if a.AdjustedSeverity != "" || a.SetAside {
			t.Errorf("%s not cleared: %+v", a.SignalID, a)
		}
	}
}

// A high signal read as harmless no longer requests human review on its own.
func TestAIImpactsCriticalityChangesTheExitCode(t *testing.T) {
	for _, impacts := range []bool{false, true} {
		r := criticalityReport()
		r.Signals, r.SignalAssessments = r.Signals[1:2], r.SignalAssessments[1:2]
		r.AIImpactsCriticality = impacts
		Finalize(r, true)
		want := 2
		if impacts {
			want = 0
		}
		if r.ExitCode != want {
			t.Errorf("ai_impacts_criticality=%v: exit %d, want %d", impacts, r.ExitCode, want)
		}
	}
}

func TestMarkdownRendersReviewerReading(t *testing.T) {
	r := assessedReport()
	r.ReviewerSummary = "The retry change is the main risk.\n\n<script>alert(1)</script>"
	Finalize(r, true)
	md := string(Markdown(r))
	for _, want := range []string{
		"## Reviewer Summary",
		"> The retry change is the main risk.",
		"&lt;script&gt;",
		"## Linter Signals Read by the Reviewer",
		"- **risk** Retry bound is off by one (signal-1; linter: Control flow changed): Three attempts became two.",
		"- **uncertain** Nothing tests the new branch",
		"- **no risk** Only a comment changed",
		"  Rationale: Line 9 is a comment.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	if strings.Contains(md, "<script>") {
		t.Fatal("raw HTML rendered")
	}
	if strings.Index(md, "## Linter Signals Read by the Reviewer") > strings.Index(md, "## Reproduced Issues") {
		t.Fatal("reviewer reading must follow the investigation summary")
	}

	bare := assessedReport()
	bare.SignalAssessments = nil
	Finalize(bare, true)
	if md := string(Markdown(bare)); strings.Contains(md, "Read by the Reviewer") || strings.Contains(md, "## Reviewer Summary") {
		t.Fatal("reviewer sections rendered without a reviewer")
	}
}
