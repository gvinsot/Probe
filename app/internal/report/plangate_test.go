package report

import (
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// gateReport is a report of a change that stays within a low-risk plan and
// whose checks passed: the plan gate lets it through.
func gateReport() *model.Report {
	base := strings.Repeat("c", 40)
	return &model.Report{
		Change: model.Change{BaseCommit: base, Files: []model.ChangedFile{{Path: "calc/calc.go", Status: "M", Hunks: []model.Hunk{{OldStart: 5, NewStart: 5, OldLines: 1, NewLines: 1, Lines: []model.DiffLine{
			{Kind: "delete", OldLine: 5, Content: "\treturn total - 10"},
			{Kind: "add", NewLine: 5, Content: "\treturn total - 11"},
		}}}}}},
		Checks: []model.Check{{ID: "check-1", Kind: "test", Status: "PASS", Command: []string{"go", "test", "./..."}}},
		PlanDrift: &model.PlanDrift{
			PlanSHA256: strings.Repeat("a", 64),
			Contract:   model.PlanContract{BaseCommit: base, Files: []string{"calc/calc.go"}},
			Assessment: model.PlanGateAssessment{Status: model.PlanAssessed, FlaggedCategories: []string{}, Gaps: []string{}},
		},
	}
}

func TestPlanGateLetsALowRiskConformingChangeThrough(t *testing.T) {
	r := gateReport()
	Finalize(r, true)
	d := r.PlanDrift
	if d.Status != model.PlanDriftConforming || d.Decision != model.PlanDecisionNoReview || len(d.DecisionReasons) != 0 {
		t.Fatalf("gate = %s %s %v", d.Status, d.Decision, d.DecisionReasons)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.ExitCode)
	}
	md := string(Markdown(r))
	for _, want := range []string{"Plan gate: **no human review required**", "no category flagged", "not a verdict on correctness"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestPlanGateRequiresReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.Report)
		reason string
	}{
		{"major plan", func(r *model.Report) {
			r.PlanDrift.Assessment.FlaggedCategories = []string{model.PlanCategoryRegressionRisk}
		}, "the plan raised risk categories: regression_risk"},
		{"stored major flag without categories is recomputed, gap still blocks", func(r *model.Report) {
			r.PlanDrift.Assessment.Gaps = []string{"1 planned symbols could not be measured on the static index"}
		}, "the plan's risk is not fully measured: 1 planned symbols could not be measured"},
		{"unassessed plan", func(r *model.Report) {
			r.PlanDrift.Assessment = model.PlanGateAssessment{}
		}, "the plan could not be re-assessed by this review"},
		{"drift", func(r *model.Report) {
			r.PlanDrift.Contract.Files = []string{"other.go"}
		}, "the change drifted from the plan"},
		{"other base", func(r *model.Report) {
			r.PlanDrift.Contract.BaseCommit = strings.Repeat("d", 40)
		}, "another base commit"},
		{"no checks", func(r *model.Report) { r.Checks = nil }, "no check ran"},
		{"failed check", func(r *model.Report) {
			r.Checks[0].Status, r.Checks[0].ExitCode = "FAIL", 1
		}, "1 checks did not pass"},
		{"high signal", func(r *model.Report) {
			r.Signals = []model.Signal{{Kind: "validation_removed", Path: "calc/calc.go", Line: 5, Side: "old", Severity: "high", Summary: "s"}}
		}, "1 high or critical risk signals"},
		{"unverified", func(r *model.Report) { r.Unverified = []string{"something was not checked"} }, "unverified areas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gateReport()
			tc.mutate(r)
			Finalize(r, true)
			d := r.PlanDrift
			if d.Decision != model.PlanDecisionReviewRequired {
				t.Fatalf("decision = %s, want review required", d.Decision)
			}
			if !strings.Contains(strings.Join(d.DecisionReasons, "|"), tc.reason) {
				t.Errorf("reasons %q lack %q", d.DecisionReasons, tc.reason)
			}
			if r.ExitCode != 2 {
				t.Errorf("exit code = %d, want 2 with --ci", r.ExitCode)
			}
			if !strings.Contains(string(Markdown(r)), "Plan gate: **human review required**") {
				t.Error("markdown does not state that review is required")
			}
		})
	}
}

func TestPlanGateIsRecomputedFromAForgedDecision(t *testing.T) {
	r := gateReport()
	r.Checks = nil
	r.PlanDrift.Decision, r.PlanDrift.DecisionReasons = model.PlanDecisionNoReview, nil
	r.PlanDrift.Assessment.Major = true // a stored flag without categories is recomputed
	Finalize(r, false)
	if r.PlanDrift.Decision != model.PlanDecisionReviewRequired || r.PlanDrift.Assessment.Major {
		t.Errorf("forged gate survived: %+v", r.PlanDrift)
	}
	if r.ExitCode != 0 {
		t.Errorf("without --ci the exit code stays 0, got %d", r.ExitCode)
	}
}
