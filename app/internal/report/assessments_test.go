package report

import (
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

// Assessments are model judgment: they change no exit code or review target.
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
