package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Cross-feature tests of the integration agent (contract §2 "Agent I"): every
// export class is exercised with the real fixture of its feature, through the
// real verifiers and Finalize, and every class loses its findings when the
// recorded checks or evidence behind it are removed or its statuses are
// forced without them.

type exportClassCase struct {
	class string
	rule  string
	exit  int // with --ci
	build func(t *testing.T) *model.Report
}

func exportClassCases() []exportClassCase {
	return []exportClassCase{
		{ClassReproduced, "probe/reproduced", 1, func(*testing.T) *model.Report { return proofReport() }},
		{ClassBaseTestFailsOnCandidate, "probe/base-test-fails-on-candidate", 2, func(*testing.T) *model.Report { return baseTestsReport() }},
		{ClassImpactedTestFailsOnCandidate, "probe/impacted-test-fails-on-candidate", 2, func(*testing.T) *model.Report { return impactedReport() }},
		{ClassFuzzDivergence, "probe/fuzz-divergence", 2, func(t *testing.T) *model.Report { return fuzzReport(t, divergeAt(discountCall, "w:"+discountCall)) }},
		{ClassObservedDivergence, "probe/observed-divergence", 2, func(*testing.T) *model.Report { return observationReport() }},
		{ClassIntentTestFailed, "probe/intent-test-failed", 2, func(*testing.T) *model.Report { return intentProofReport() }},
		{ClassSurvivingMutant, "probe/surviving-mutant", 0, func(*testing.T) *model.Report { return mutationReport() }},
	}
}

func exportedClasses(r *model.Report) map[string]int {
	classes := map[string]int{}
	for _, f := range collectFindings(r, verifyExports(r)).findings {
		classes[f.Class]++
	}
	return classes
}

func stripChecksAndEvidence(r *model.Report) {
	r.Checks, r.Evidence = nil, nil
	if r.Mutation != nil {
		r.Mutation.Checks = nil
	}
}

// Every class: the real fixture gives findings of that class only, with the
// exit code of §1.17, and both exports render them.
func TestExportClassesFromRealFixtures(t *testing.T) {
	for _, tc := range exportClassCases() {
		t.Run(tc.class, func(t *testing.T) {
			r := tc.build(t)
			Finalize(r, true)
			if r.ExitCode != tc.exit {
				t.Fatalf("exit %d, want %d", r.ExitCode, tc.exit)
			}
			classes := exportedClasses(r)
			if classes[tc.class] == 0 || len(classes) != 1 {
				t.Fatalf("findings %v, want only %s", classes, tc.class)
			}
			dir := t.TempDir()
			if err := Write(dir, r, []string{FormatMarkdown, FormatJSON, FormatSARIF, FormatPRComment}); err != nil {
				t.Fatal(err)
			}
			sarif, err := os.ReadFile(filepath.Join(dir, "confidence-report.sarif"))
			if err != nil {
				t.Fatal(err)
			}
			var log struct {
				Runs []struct {
					Results []struct {
						RuleID string `json:"ruleId"`
						Level  string `json:"level"`
					} `json:"results"`
				} `json:"runs"`
			}
			if err := json.Unmarshal(sarif, &log); err != nil {
				t.Fatal(err)
			}
			comment, err := os.ReadFile(filepath.Join(dir, "PR_COMMENT.md"))
			if err != nil {
				t.Fatal(err)
			}
			// A finding without a location in a changed file is not a SARIF
			// result; the comment lists every finding.
			for _, res := range log.Runs[0].Results {
				if res.RuleID != tc.rule {
					t.Errorf("SARIF result with rule %s", res.RuleID)
				}
				if res.Level == levelError && (tc.class != ClassReproduced || r.ExitCode != 1) {
					t.Errorf("level error for %s", tc.class)
				}
			}
			want := "## Probe: 1 evidence-backed finding\n"
			if n := classes[tc.class]; n != 1 {
				want = fmt.Sprintf("## Probe: %d evidence-backed findings\n", n)
			}
			if !strings.Contains(string(comment), want) || !strings.Contains(string(comment), notApprovalText) {
				t.Errorf("the comment does not list the finding (want %q):\n%s", want, comment)
			}
		})
	}
}

// Every class: removing the recorded checks and evidence, before or after
// Finalize, leaves no finding and never exit 1.
func TestExportClassesNeedRecordedChecksAndEvidence(t *testing.T) {
	for _, tc := range exportClassCases() {
		t.Run(tc.class, func(t *testing.T) {
			before := tc.build(t)
			stripChecksAndEvidence(before)
			Finalize(before, true)
			if classes := exportedClasses(before); len(classes) != 0 {
				t.Errorf("stripped before Finalize: findings %v", classes)
			}
			if before.ExitCode == 1 {
				t.Error("stripped before Finalize: exit 1")
			}

			after := tc.build(t)
			Finalize(after, true)
			stripChecksAndEvidence(after)
			// Not finalized again: the export guard alone must refuse the
			// statuses Finalize left behind.
			if classes := exportedClasses(after); len(classes) != 0 {
				t.Errorf("stripped after Finalize, not finalized again: findings %v", classes)
			}
			Finalize(after, true)
			if classes := exportedClasses(after); len(classes) != 0 || after.ExitCode == 1 {
				t.Errorf("stripped after Finalize, finalized again: findings %v, exit %d", classes, after.ExitCode)
			}
		})
	}
}

// Every class: statuses forced without the evidence behind them (the checks
// kept) give no finding. For surviving mutants, which rest on the mutation
// ledger rather than on evidence records, every mutant is forced SURVIVED with
// its ledger removed.
func TestExportClassesRefuseForcedStatuses(t *testing.T) {
	for _, tc := range exportClassCases() {
		t.Run(tc.class, func(t *testing.T) {
			r := tc.build(t)
			Finalize(r, true)
			if tc.class == ClassSurvivingMutant {
				for i := range r.Mutation.Mutants {
					m := &r.Mutation.Mutants[i]
					m.Status, m.TestsRun, m.PatchSHA256 = model.MutantSurvived, 3, strings.Repeat("a", 64)
					if m.CheckID == "" {
						m.CheckID, m.ControlCheckID = "mutation-check-9", "mutation-check-1"
					}
				}
				r.Mutation.Checks = nil
			} else {
				r.Evidence = nil
			}
			forceStatuses(r, tc.class) // findings_test.go (F9)
			if classes := exportedClasses(r); len(classes) != 0 {
				t.Errorf("forced, not finalized again: findings %v", classes)
			}
			Finalize(r, true)
			if classes := exportedClasses(r); len(classes) != 0 || r.ExitCode == 1 {
				t.Errorf("forced, finalized again: findings %v, exit %d", classes, r.ExitCode)
			}
		})
	}
}
