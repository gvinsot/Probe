package report

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// changedFile returns a modified file whose diff adds the given candidate
// lines, with the line before each added line as context.
func changedFile(path string, added ...int) model.ChangedFile {
	f := model.ChangedFile{Path: path, Status: "M", Hunks: []model.Hunk{}}
	for _, n := range added {
		h := model.Hunk{OldStart: n - 1, NewStart: n - 1, Lines: []model.DiffLine{}}
		if n > 1 {
			h.Lines = append(h.Lines, model.DiffLine{Kind: "context", OldLine: n - 1, NewLine: n - 1, Content: "// context"})
		}
		h.Lines = append(h.Lines, model.DiffLine{Kind: "delete", OldLine: n, Content: "old"}, model.DiffLine{Kind: "add", NewLine: n, Content: "new"})
		f.Hunks = append(f.Hunks, h)
		f.Additions++
		f.Deletions++
	}
	return f
}

// exportFixture is proofReport with a recorded change, a model-chosen location
// on the changed line, a test path and the retained generated test. Finalize
// makes its hypothesis REPRODUCED with exit 1.
func exportFixture() *model.Report {
	r := proofReport()
	r.ToolVersion = "v1.2.3"
	r.GeneratedAt = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	r.Change = model.Change{BaseRef: "main", HeadRef: "HEAD", BaseCommit: strings.Repeat("b", 40), HeadCommit: strings.Repeat("c", 40), BaseRefCommit: strings.Repeat("b", 40),
		Files: []model.ChangedFile{changedFile("auth.go", 3)}, Additions: 1, Deletions: 1}
	r.Policy = model.Policy{Source: model.PolicyExplicit, Path: "policy.json"}
	r.Evidence[0].Path = "guest_test.go"
	r.Evidence[0].Description = "Guest authorization must remain rejected"
	r.Hypotheses[0].Path, r.Hypotheses[0].Line = "auth.go", 3
	r.Hypotheses[0].Rationale = "The generated named test passes on the baseline and fails on the candidate."
	r.Artifacts = []model.Artifact{{Path: "artifacts/0a1b2c3d-generated-test-1-guest_test.go", Kind: model.ArtifactGeneratedTest, SHA256: strings.Repeat("a", 64)}}
	return r
}

func finalized(r *model.Report, ci bool) *model.Report {
	Finalize(r, ci)
	return Sanitize(r)
}

func classes(set findingSet) []string {
	var out []string
	for _, f := range set.findings {
		out = append(out, f.Class)
	}
	return out
}

// Exactly one reproduced finding exists only when Finalize accepted the
// hypothesis; every mutation that makes Finalize refuse it leaves no finding.
func TestFindingsFollowFinalize(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Report)
		want   int
	}{
		{"valid", func(*model.Report) {}, 1},
		{"invented ID", func(r *model.Report) { r.Hypotheses[0].EvidenceIDs = []string{"invented"} }, 0},
		{"no evidence", func(r *model.Report) { r.Hypotheses[0].EvidenceIDs = nil }, 0},
		{"baseline fails", func(r *model.Report) { r.Checks[0].Status = "FAIL"; r.Checks[0].ExitCode = 1 }, 0},
		{"candidate timeout", func(r *model.Report) { r.Checks[1].Status = "TIMEOUT" }, 0},
		{"different commands", func(r *model.Report) { r.Checks[0].Command = []string{"true"} }, 0},
		{"missing check", func(r *model.Report) { r.Checks = r.Checks[:1] }, 0},
		{"ordinary test", func(r *model.Report) { r.Evidence[0].Kind = "existing_test" }, 0},
		{"duplicate evidence", func(r *model.Report) { r.Evidence = append(r.Evidence, r.Evidence[0]) }, 0},
		{"duplicate check", func(r *model.Report) { r.Checks = append(r.Checks, r.Checks[0]) }, 0},
		{"container error", func(r *model.Report) { r.Checks[1].ExitCode = 125 }, 0},
		{"unsupported runner", func(r *model.Report) { r.Evidence[0].Runner = "generic" }, 0},
		{"truncated transcript", func(r *model.Report) { r.Checks[1].Truncated = true }, 0},
		{"replayed baseline", func(r *model.Report) {
			r.Checks[0].Cache = &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("d", 64), LiveRuns: 5}
		}, 0},
		{"invented negative proof", func(r *model.Report) { r.Hypotheses[0].Status = "NOT_REPRODUCED" }, 0},
		{"invented dismissal", func(r *model.Report) { r.Hypotheses[0].Status = "DISMISSED"; r.Hypotheses[0].Rationale = "fine" }, 0},
		{"negative experiment", func(r *model.Report) {
			r.Hypotheses[0].Status, r.Evidence[0].Status = "NOT_REPRODUCED", "NOT_REPRODUCED"
			r.Checks[1].Status, r.Checks[1].ExitCode, r.Checks[1].Output = "PASS", 0, r.Checks[0].Output
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := exportFixture()
			tt.mutate(r)
			r = finalized(r, true)
			set := collectFindings(r, verifyExports(r))
			if len(set.findings) != tt.want {
				t.Fatalf("%d findings, want %d: %+v", len(set.findings), tt.want, set.findings)
			}
			if tt.want == 1 {
				f := set.findings[0]
				if f.Class != ClassReproduced || f.Severity != "high" || f.Title != "Regression" || strings.Join(f.EvidenceIDs, ",") != "experiment" || strings.Join(f.CheckIDs, ",") != "base,candidate" || strings.Join(f.HypothesisIDs, ",") != "h1" {
					t.Fatalf("unexpected finding %+v", f)
				}
				if f.anchor == nil || f.anchor.Path != "auth.go" || f.anchor.Line != 3 || f.anchor.FileLevel || f.anchor.Source != locModel {
					t.Fatalf("anchor %+v", f.anchor)
				}
				if len(f.Artifacts) != 1 || f.Artifacts[0].SHA256 != strings.Repeat("a", 64) {
					t.Fatalf("retained test not linked: %+v", f.Artifacts)
				}
				if len(f.fingerprint) != 32 {
					t.Fatalf("fingerprint %q", f.fingerprint)
				}
			}
		})
	}
}

// Records that are not evidence of a finding class never become findings,
// whatever their severity, and neither does a hypothesis that is not
// REPRODUCED or INTENT_TEST_FAILED.
func TestFindingsExcludeNonEvidence(t *testing.T) {
	r := exportFixture()
	r.Hypotheses = []model.Hypothesis{
		{ID: "unverified", Title: "Guess", Severity: "critical", Status: "REPRODUCED", EvidenceIDs: []string{"invented"}, Path: "auth.go", Line: 3},
		{ID: "dismissed", Title: "Fine", Severity: "high", Status: "DISMISSED", Rationale: "checked", EvidenceIDs: []string{"source"}},
		{ID: "judged", Title: "Expected", Severity: "high", Status: "DIVERGED", EvidenceIDs: []string{"experiment"}, CriterionID: "AC-1", IntentJudgment: model.JudgmentUnexpectedChange},
	}
	r.Evidence = append(r.Evidence,
		model.Evidence{ID: "source", Kind: model.EvidenceSourceObservation, Status: model.StatusObserved, Output: "if user == \"admin\""},
		model.Evidence{ID: "obs", Kind: model.EvidenceDifferentialObservation, Status: model.StatusNotDiverged, Path: "x_test.go", CheckID: "candidate", BaseCheckID: "base", RepeatCheckID: "base", Runner: "go_test_json", TestNames: []string{"TestX"}},
		model.Evidence{ID: "passes", Kind: model.EvidenceBaseTestDifferential, Status: model.StatusPassesOnCandidate, Path: "x_test.go", CheckID: "candidate", BaseCheckID: "base", Runner: "go_test_json", TestNames: []string{"TestY"}},
		model.Evidence{ID: "intent-pass", Kind: model.EvidenceIntentTest, Status: model.StatusIntentTestPassed, Path: "x_test.go", CheckID: "candidate", CriterionID: "AC-1", Runner: "go_test_json", TestNames: []string{"TestZ"}},
	)
	r.IntentCriteria = []model.IntentCriterion{{ID: "AC-1", Text: "Guests are rejected.", Line: 1}}
	r.Signals = []model.Signal{
		{ID: "s1", Kind: "security_sensitive_change", Path: "auth.go", Line: 3, Severity: "critical", Summary: "Authorization changed"},
		{ID: "s2", Kind: "uncovered_change", Path: "auth.go", Line: 3, Severity: "medium", Summary: "Not executed"},
		{ID: "s3", Kind: model.SignalImpactedCaller, Path: "auth.go", Line: 3, Severity: "low", Summary: "Caller"},
		{ID: "s4", Kind: model.SignalTestAssertionRemoved, Path: "auth_test.go", Line: 3, Severity: "medium", Summary: "Assertion removed"},
		{ID: "s5", Kind: model.SignalSurvivingMutant, Path: "auth.go", Line: 3, Severity: "medium", Summary: "Survivor signal"},
	}
	r.Checks = append(r.Checks, model.Check{ID: "test", Kind: "test", Status: "FAIL", ExitCode: 1, Output: "FAIL"})
	r.BaseTests = &model.BaseTests{Status: model.BaseTestsRan, Tests: []model.BaseTest{{Name: "TestY", Path: "x_test.go", Line: 1, Change: model.BaseTestModified, Status: model.StatusPassesOnCandidate, EvidenceID: "passes"}}}
	r.Mutation = &model.Mutation{Status: model.MutationRan, Mutants: []model.Mutant{{ID: "mutant-1", Path: "auth.go", Line: 3, Status: model.MutantKilled, CheckID: "mutation-check-2", ControlCheckID: "mutation-check-1", FailedTests: []string{"TestA"}}},
		Checks: []model.Check{{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS"}, {ID: "mutation-check-2", Kind: model.CheckMutant, Status: "FAIL", ExitCode: 1}}}
	r.Prepare = &model.Prepare{Status: model.PrepareBuilt, SourceCommit: strings.Repeat("b", 40), Command: []string{"go", "mod", "download"}, User: "sandbox"}
	r.Impact = &model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{{Path: "auth.go", Line: 3, Symbol: "Allowed", Change: model.ChangeBodyChanged,
		Callers: []model.ImpactCaller{{Path: "main.go", Line: 9, Symbol: "main", Depth: 1, Resolution: model.ResolutionStatic}}}}}
	r = finalized(r, true)
	// Every verifier and finalizer ran, and nothing above is a finding class
	// whose evidence re-derives: the one listed finding would be a reproduced
	// hypothesis, and none of these hypotheses is reproduced any more.
	for _, h := range r.Hypotheses {
		if h.Status == model.StatusReproduced || h.Status == model.StatusIntentTestFailed {
			t.Fatalf("fixture hypothesis %s finalized %s", h.ID, h.Status)
		}
	}
	set := collectFindings(r, verifyExports(r))
	if len(set.findings) != 0 || set.omitted != 0 {
		t.Fatalf("non-evidence became findings: %v", classes(set))
	}
	// The same holds when a verifier claims every stored status: none of these
	// records belongs to a finding class.
	claimed := exportVerification{evidence: map[string]string{}, mutants: map[string]string{"mutant-1": model.MutantKilled}}
	for _, e := range r.Evidence {
		claimed.evidence[e.ID] = e.Status
	}
	if set := collectFindings(r, claimed); len(set.findings) != 0 {
		t.Fatalf("non-evidence became findings with claimed statuses: %v", classes(set))
	}
}

// classFixture returns a finalized-looking report holding one finding of its
// class, and the verification its verifier would return. Checks and evidence
// are recorded as the harness records them; the verification is injected
// because these tests exercise the collectors, not the feature verifiers.
type classFixture func() (*model.Report, exportVerification)

func baseTestFixture() (*model.Report, exportVerification) {
	r := &model.Report{
		Change: model.Change{Files: []model.ChangedFile{changedFile("clamp.go", 7), changedFile("clamp_test.go", 12)}},
		Checks: []model.Check{
			{ID: "check-1", Kind: model.CheckBaseTestBase, Status: "PASS", Command: []string{"go", "test", "-json", "."}},
			{ID: "check-2", Kind: model.CheckBaseTestHybrid, Status: "FAIL", ExitCode: 1, Command: []string{"go", "test", "-json", "."}},
		},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceBaseTestDifferential, Status: model.StatusFailsOnCandidate, Path: "clamp_test.go", CheckID: "check-2", BaseCheckID: "check-1", Runner: "go_test_json", TestNames: []string{"TestClampUpper"}}},
		BaseTests: &model.BaseTests{Status: model.BaseTestsRan, Tests: []model.BaseTest{{Name: "TestClampUpper", Path: "clamp_test.go", Line: 10, EndLine: 14,
			CandidatePath: "clamp_test.go", CandidateLine: 10, CandidateEndLine: 13, Change: model.BaseTestModified, Status: model.StatusFailsOnCandidate, EvidenceID: "evidence-1"}}},
	}
	return r, exportVerification{evidence: map[string]string{"evidence-1": model.StatusFailsOnCandidate}}
}

func impactFixture() (*model.Report, exportVerification) {
	test := model.ImpactTest{Name: "TestTotal", Path: "cart/cart_test.go", Line: 20, Package: "example.test/shop/cart", Depth: 1, Resolution: model.ResolutionStatic, EvidenceID: "evidence-1", Status: model.StatusFailsOnCandidate}
	other := test
	other.Depth, other.Resolution = 2, model.ResolutionInterface
	r := &model.Report{
		Change: model.Change{Files: []model.ChangedFile{changedFile("cart/price.go", 5, 30)}},
		Checks: []model.Check{
			{ID: "check-1", Kind: model.CheckImpactedTestBase, Status: "PASS"},
			{ID: "check-2", Kind: model.CheckImpactedTestCandidate, Status: "FAIL", ExitCode: 1},
		},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceImpactedTestDifferential, Status: model.StatusFailsOnCandidate, Path: "cart/cart_test.go", CheckID: "check-2", BaseCheckID: "check-1", Runner: "go_test_json", TestNames: []string{"TestTotal"}}},
		Impact: &model.Impact{Status: model.ImpactIndexed, TestsStatus: model.ImpactTestsRan, ChangedFunctions: []model.ImpactFunction{
			{Path: "cart/price.go", Line: 5, EndLine: 9, Symbol: "Price", Change: model.ChangeBodyChanged, Tests: []model.ImpactTest{test}},
			{Path: "cart/price.go", Line: 30, EndLine: 34, Symbol: "Discount", Change: model.ChangeBodyChanged, Tests: []model.ImpactTest{other}},
		}},
	}
	return r, exportVerification{evidence: map[string]string{"evidence-1": model.StatusFailsOnCandidate}}
}

func fuzzFixture() (*model.Report, exportVerification) {
	r := &model.Report{
		Change: model.Change{Files: []model.ChangedFile{changedFile("calc/calc.go", 12)}},
		Checks: []model.Check{
			{ID: "check-1", Kind: model.CheckFuzzBase, Status: "PASS"}, {ID: "check-2", Kind: model.CheckFuzzCandidate, Status: "PASS"},
			{ID: "check-3", Kind: model.CheckFuzzBaseConfirm, Status: "PASS"}, {ID: "check-4", Kind: model.CheckFuzzCandidateConfirm, Status: "PASS"},
		},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceDifferentialFuzz, Status: model.StatusDiverged, Path: "calc/swiftproof_fuzz_x_test.go", CheckID: "check-2", BaseCheckID: "check-1", Runner: "go_test_json", TestNames: []string{"TestSwiftProofFuzzPercent_x"}}},
		Divergences: []model.Divergence{{EvidenceID: "evidence-1", Kind: model.EvidenceDifferentialFuzz, Path: "calc/calc.go", Line: 10, Symbol: "Percent", AnchorSource: anchorChangedFunction,
			TestPath: "calc/swiftproof_fuzz_x_test.go", TestNames: []string{"TestSwiftProofFuzzPercent_x"}, CheckIDs: []string{"check-1", "check-2", "check-3", "check-4"}, HypothesisIDs: []string{},
			Observations: []model.Observation{
				{Test: "TestSwiftProofFuzzPercent_x", Key: "Percent(1, 3)", Status: model.ObservationDiverged, Base: "33", Candidate: "34", BaseRecorded: true, CandidateRecorded: true},
				{Test: "TestSwiftProofFuzzPercent_x", Key: "Percent(2, 3)", Status: model.ObservationDiverged, Base: "66", Candidate: "67", BaseRecorded: true, CandidateRecorded: true},
				{Test: "TestSwiftProofFuzzPercent_x", Key: "Percent(5, 3)", Status: model.ObservationDiverged, Base: "166", Candidate: "167", BaseRecorded: true, CandidateRecorded: true},
			}, Note: model.DivergenceNote}},
	}
	return r, exportVerification{evidence: map[string]string{"evidence-1": model.StatusDiverged}}
}

func observationFixture() (*model.Report, exportVerification) {
	r := &model.Report{
		Change: model.Change{Files: []model.ChangedFile{changedFile("discount.go", 4)}},
		Checks: []model.Check{
			{ID: "check-1", Kind: model.CheckGeneratedBase, Status: "PASS"}, {ID: "check-2", Kind: model.CheckGeneratedCandidate, Status: "PASS"},
			{ID: "check-3", Kind: model.CheckGeneratedBaseRepeat, Status: "PASS"},
		},
		Evidence:   []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceDifferentialObservation, Status: model.StatusDiverged, Path: "discount_obs_test.go", CheckID: "check-2", BaseCheckID: "check-1", RepeatCheckID: "check-3", Runner: "go_test_json", TestNames: []string{"TestObserveDiscount"}}},
		Hypotheses: []model.Hypothesis{{ID: "h1", Title: "Discount rounding changed", Severity: "medium", Status: model.StatusDiverged, EvidenceIDs: []string{"evidence-1"}, Path: "discount.go", Line: 4}},
		Divergences: []model.Divergence{{EvidenceID: "evidence-1", Kind: model.EvidenceDifferentialObservation, Path: "discount.go", Line: 4, AnchorSource: anchorHypothesis,
			TestPath: "discount_obs_test.go", TestNames: []string{"TestObserveDiscount"}, CheckIDs: []string{"check-1", "check-2", "check-3"}, HypothesisIDs: []string{"h1"},
			Observations: []model.Observation{{Test: "TestObserveDiscount", Key: "Discount(5,33)", Status: model.ObservationDiverged, Base: "4", Candidate: "3", BaseRecorded: true, CandidateRecorded: true}}, Note: model.DivergenceNote}},
		Artifacts: []model.Artifact{{Path: "artifacts/0a1b-generated-test-2-discount_obs_test.go", Kind: model.ArtifactGeneratedTest, SHA256: strings.Repeat("e", 64)}},
	}
	return r, exportVerification{evidence: map[string]string{"evidence-1": model.StatusDiverged}}
}

func intentFixture() (*model.Report, exportVerification) {
	h := model.Hypothesis{ID: "h1", Title: "Empty cart total", Severity: "medium", Status: model.StatusIntentTestFailed, EvidenceIDs: []string{"evidence-1"}, Path: "cart.go", Line: 8, CriterionID: "AC-2"}
	r := &model.Report{
		Intent:         "- Empty carts cost nothing.\n- A total is never negative.",
		IntentCriteria: []model.IntentCriterion{{ID: "AC-1", Text: "Empty carts cost nothing.", Line: 1}, {ID: "AC-2", Text: "A total is never negative.", Line: 2}},
		Change:         model.Change{Files: []model.ChangedFile{changedFile("cart.go", 8)}},
		Checks:         []model.Check{{ID: "check-1", Kind: model.CheckGeneratedIntent, Status: "FAIL", ExitCode: 1}},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceIntentTest, Status: model.StatusIntentTestFailed, Path: "cart_intent_test.go", CheckID: "check-1", CriterionID: "AC-2",
			ReferencedSymbols: []string{"Total"}, Runner: "go_test_json", TestNames: []string{"TestIntentNonNegative"}}},
		Hypotheses:         []model.Hypothesis{h},
		IntentTestFailures: []model.Hypothesis{h},
		Artifacts:          []model.Artifact{{Path: "artifacts/0a1b-intent-test-1-cart_intent_test.go", Kind: model.ArtifactIntentTest, SHA256: strings.Repeat("f", 64)}},
	}
	return r, exportVerification{evidence: map[string]string{"evidence-1": model.StatusIntentTestFailed}}
}

func mutantFixture() (*model.Report, exportVerification) {
	r := &model.Report{
		Change: model.Change{Files: []model.ChangedFile{changedFile("discount.go", 6)}},
		Mutation: &model.Mutation{Status: model.MutationRan, Command: []string{"go", "test", "-json", "{package}"}, Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 120},
			Mutants: []model.Mutant{
				{ID: "mutant-1", Path: "discount.go", Line: 6, Package: "./", Operator: "comparison", Original: "if pct > 50 {", Mutated: "if pct >= 50 {", Status: model.MutantSurvived,
					CheckID: "mutation-check-2", ControlCheckID: "mutation-check-1", PatchSHA256: strings.Repeat("9", 64), TestsRun: 2},
				{ID: "mutant-2", Path: "discount.go", Line: 6, Package: "./", Operator: "arithmetic", Original: "p * 2", Mutated: "p / 2", Status: model.MutantKilled,
					CheckID: "mutation-check-3", ControlCheckID: "mutation-check-1", FailedTests: []string{"TestDiscount"}},
			},
			Checks: []model.Check{{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS"}, {ID: "mutation-check-2", Kind: model.CheckMutant, Status: "PASS"}, {ID: "mutation-check-3", Kind: model.CheckMutant, Status: "FAIL", ExitCode: 1}},
		},
		Artifacts: []model.Artifact{{Path: "artifacts/0a1b-mutant-1.patch", Kind: model.ArtifactMutantPatch, SHA256: strings.Repeat("9", 64)}},
	}
	return r, exportVerification{evidence: map[string]string{}, mutants: map[string]string{"mutant-1": model.MutantSurvived, "mutant-2": model.MutantKilled}}
}

func reproducedFixture() (*model.Report, exportVerification) {
	r := finalized(exportFixture(), true)
	return r, verifyExports(r)
}

var classFixtures = map[string]classFixture{
	ClassReproduced:                   reproducedFixture,
	ClassBaseTestFailsOnCandidate:     baseTestFixture,
	ClassImpactedTestFailsOnCandidate: impactFixture,
	ClassFuzzDivergence:               fuzzFixture,
	ClassObservedDivergence:           observationFixture,
	ClassIntentTestFailed:             intentFixture,
	ClassSurvivingMutant:              mutantFixture,
}

func TestClassTableIsComplete(t *testing.T) {
	rules, ranks := map[string]bool{}, map[int]bool{}
	want := []string{ClassReproduced, ClassBaseTestFailsOnCandidate, ClassImpactedTestFailsOnCandidate, ClassFuzzDivergence, ClassObservedDivergence, ClassIntentTestFailed, ClassSurvivingMutant}
	wantRules := []string{"swiftproof/reproduced", "swiftproof/base-test-fails-on-candidate", "swiftproof/impacted-test-fails-on-candidate", "swiftproof/fuzz-divergence", "swiftproof/observed-divergence", "swiftproof/intent-test-failed", "swiftproof/surviving-mutant"}
	if len(findingClasses) != len(want) {
		t.Fatalf("%d classes", len(findingClasses))
	}
	for i, c := range findingClasses {
		if c.Class != want[i] || c.RuleID != wantRules[i] || c.Rank != i+1 {
			t.Errorf("class %d: %+v", i, c)
		}
		if rules[c.RuleID] || ranks[c.Rank] {
			t.Errorf("duplicate rule or rank: %+v", c)
		}
		rules[c.RuleID], ranks[c.Rank] = true, true
		for _, text := range []string{c.Short, c.Full, c.Help, c.Heading, c.Caveat, c.RuleName, c.Status} {
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s has an empty text", c.Class)
			}
		}
		if _, ok := classFixtures[c.Class]; !ok {
			t.Errorf("%s has no fixture", c.Class)
		}
	}
	levels := map[string]string{ClassBaseTestFailsOnCandidate: levelWarning, ClassImpactedTestFailsOnCandidate: levelWarning, ClassFuzzDivergence: levelWarning,
		ClassObservedDivergence: levelWarning, ClassIntentTestFailed: levelNote, ClassSurvivingMutant: levelNote}
	for class, level := range levels {
		c, _ := classByName(class)
		if got := c.level(finding{Severity: "critical"}); got != level {
			t.Errorf("%s level %s, want %s", class, got, level)
		}
	}
	reproduced, _ := classByName(ClassReproduced)
	for sev, level := range map[string]string{"critical": levelError, "high": levelError, "medium": levelWarning, "low": levelWarning} {
		if got := reproduced.level(finding{Severity: sev}); got != level {
			t.Errorf("reproduced %s: level %s, want %s", sev, got, level)
		}
	}
}

// Every class: its fixture yields exactly one finding of that class; removing
// the checks and evidence yields none even when a verifier would still claim
// the status; and statuses forced into the report without evidence, then
// finalized and re-derived, yield none.
func TestEveryClassRequiresRecordedEvidence(t *testing.T) {
	for class, fixture := range classFixtures {
		t.Run(class, func(t *testing.T) {
			r, v := fixture()
			set := collectFindings(r, v)
			if len(set.findings) != 1 || set.findings[0].Class != class {
				t.Fatalf("valid fixture: %v", classes(set))
			}
			f := set.findings[0]
			if f.Identity == "" || len(f.CheckIDs) == 0 || len(f.fingerprint) != 32 {
				t.Fatalf("incomplete finding %+v", f)
			}

			stripped, v := fixture()
			stripped.Checks, stripped.Evidence = nil, nil
			if stripped.Mutation != nil {
				stripped.Mutation.Checks = nil
			}
			if set := collectFindings(stripped, v); len(set.findings) != 0 {
				t.Fatalf("findings without recorded checks: %v", classes(set))
			}

			forged, _ := fixture()
			forged.Checks, forged.Evidence = nil, nil
			if forged.Mutation != nil {
				forged.Mutation.Checks = nil
			}
			forceStatuses(forged)
			forged = finalized(forged, true)
			if set := collectFindings(forged, verifyExports(forged)); len(set.findings) != 0 {
				t.Fatalf("forged statuses became findings: %v", classes(set))
			}
		})
	}
}

// forceStatuses writes every class status into the report without evidence.
func forceStatuses(r *model.Report) {
	for i := range r.Hypotheses {
		r.Hypotheses[i].Status = model.StatusReproduced
	}
	if r.BaseTests != nil {
		for i := range r.BaseTests.Tests {
			r.BaseTests.Tests[i].Status = model.StatusFailsOnCandidate
		}
	}
	if r.Impact != nil {
		for i := range r.Impact.ChangedFunctions {
			for j := range r.Impact.ChangedFunctions[i].Tests {
				r.Impact.ChangedFunctions[i].Tests[j].Status = model.StatusFailsOnCandidate
			}
		}
	}
	if r.Mutation != nil {
		for i := range r.Mutation.Mutants {
			r.Mutation.Mutants[i].Status = model.MutantSurvived
		}
	}
}

// Each collector rejects a record whose evidence does not resolve, has another
// kind or status, is not re-derived now, or names another test.
func TestCollectorsRejectTamperedRecords(t *testing.T) {
	type tamper struct {
		name   string
		mutate func(*model.Report, *exportVerification)
	}
	common := []tamper{
		{"evidence not re-derived", func(r *model.Report, v *exportVerification) { delete(v.evidence, "evidence-1") }},
		{"evidence re-derived to another status", func(r *model.Report, v *exportVerification) { v.evidence["evidence-1"] = model.StatusUnverified }},
		{"stored status differs", func(r *model.Report, v *exportVerification) { r.Evidence[0].Status = model.StatusUnverified }},
		{"evidence of another kind", func(r *model.Report, v *exportVerification) { r.Evidence[0].Kind = model.EvidenceDifferentialTest }},
		{"duplicate evidence", func(r *model.Report, v *exportVerification) { r.Evidence = append(r.Evidence, r.Evidence[0]) }},
		{"duplicate check", func(r *model.Report, v *exportVerification) { r.Checks = append(r.Checks, r.Checks[0]) }},
		{"missing check", func(r *model.Report, v *exportVerification) { r.Checks = r.Checks[1:] }},
	}
	cases := map[string][]tamper{
		ClassBaseTestFailsOnCandidate: append(common,
			tamper{"passes", func(r *model.Report, v *exportVerification) {
				r.BaseTests.Tests[0].Status = model.StatusPassesOnCandidate
			}},
			tamper{"other test name", func(r *model.Report, v *exportVerification) { r.BaseTests.Tests[0].Name = "TestClampLower" }},
			tamper{"no evidence ID", func(r *model.Report, v *exportVerification) { r.BaseTests.Tests[0].EvidenceID = "" }},
		),
		ClassImpactedTestFailsOnCandidate: append(common,
			tamper{"unverified", func(r *model.Report, v *exportVerification) {
				for i := range r.Impact.ChangedFunctions {
					r.Impact.ChangedFunctions[i].Tests[0].Status = model.StatusUnverified
				}
			}},
			tamper{"other test name", func(r *model.Report, v *exportVerification) { r.Evidence[0].TestNames = []string{"TestOther"} }},
		),
		ClassFuzzDivergence: append(common,
			tamper{"three checks", func(r *model.Report, v *exportVerification) {
				r.Divergences[0].CheckIDs = r.Divergences[0].CheckIDs[:3]
			}},
			tamper{"confirmation check missing", func(r *model.Report, v *exportVerification) { r.Checks = r.Checks[:3] }},
			tamper{"no diverged row", func(r *model.Report, v *exportVerification) {
				for i := range r.Divergences[0].Observations {
					r.Divergences[0].Observations[i].Status = model.ObservationEqual
				}
			}},
			tamper{"kind mismatch", func(r *model.Report, v *exportVerification) {
				r.Divergences[0].Kind = model.EvidenceDifferentialObservation
			}},
		),
		ClassObservedDivergence: append(common,
			tamper{"repeat check missing", func(r *model.Report, v *exportVerification) { r.Checks = r.Checks[:2] }},
			tamper{"not diverged", func(r *model.Report, v *exportVerification) { v.evidence["evidence-1"] = model.StatusNotDiverged }},
		),
		ClassIntentTestFailed: append(append([]tamper{}, common[:6]...),
			tamper{"missing check", func(r *model.Report, v *exportVerification) { r.Checks = nil }},
			tamper{"unknown criterion", func(r *model.Report, v *exportVerification) { r.IntentCriteria = r.IntentCriteria[:1] }},
			tamper{"duplicated criterion", func(r *model.Report, v *exportVerification) {
				r.IntentCriteria = append(r.IntentCriteria, r.IntentCriteria[1])
			}},
			tamper{"other criterion", func(r *model.Report, v *exportVerification) { r.Evidence[0].CriterionID = "AC-1" }},
			tamper{"passed", func(r *model.Report, v *exportVerification) {
				r.Evidence[0].Status, v.evidence["evidence-1"] = model.StatusIntentTestPassed, model.StatusIntentTestPassed
			}},
			tamper{"hypothesis not in the failures list", func(r *model.Report, v *exportVerification) { r.IntentTestFailures = nil }},
		),
		ClassSurvivingMutant: {
			{"not re-derived", func(r *model.Report, v *exportVerification) { delete(v.mutants, "mutant-1") }},
			{"re-derived as killed", func(r *model.Report, v *exportVerification) { v.mutants["mutant-1"] = model.MutantKilled }},
			{"inconclusive", func(r *model.Report, v *exportVerification) { r.Mutation.Mutants[0].Status = model.MutantInconclusive }},
			{"control check missing", func(r *model.Report, v *exportVerification) { r.Mutation.Checks = r.Mutation.Checks[1:] }},
			{"check ID also in the main ledger", func(r *model.Report, v *exportVerification) {
				r.Checks = append(r.Checks, model.Check{ID: "mutation-check-2", Kind: "test", Status: "PASS"})
			}},
			{"duplicate mutation check", func(r *model.Report, v *exportVerification) {
				r.Mutation.Checks = append(r.Mutation.Checks, r.Mutation.Checks[1])
			}},
		},
	}
	for class, tampers := range cases {
		for _, tc := range tampers {
			t.Run(class+"/"+tc.name, func(t *testing.T) {
				r, v := classFixtures[class]()
				tc.mutate(r, &v)
				if set := collectFindings(r, v); len(set.findings) != 0 {
					t.Fatalf("tampered record exported: %+v", set.findings)
				}
			})
		}
	}
}

// The details and anchors of each class come from its own records.
func TestCollectorDetails(t *testing.T) {
	r, v := baseTestFixture()
	f := collectFindings(r, v).findings[0]
	if f.anchor == nil || f.anchor.Path != "clamp_test.go" || f.anchor.Line != 10 || f.anchor.EndLine != 13 || f.anchor.Source != locCandidateTest {
		t.Fatalf("base test anchor %+v", f.anchor)
	}
	if !hasDetail(f, "baseline declaration", "clamp_test.go:10–14") || !hasDetail(f, "change to the test", "modified") {
		t.Fatalf("base test details %+v", f.Details)
	}
	// Without a candidate range the test is unanchored (old side only).
	r, v = baseTestFixture()
	r.BaseTests.Tests[0].CandidatePath, r.BaseTests.Tests[0].CandidateLine, r.BaseTests.Tests[0].Change = "", 0, model.BaseTestRemoved
	if f := collectFindings(r, v).findings[0]; f.anchor != nil {
		t.Fatalf("removed test anchored: %+v", f.anchor)
	}

	r, v = impactFixture()
	set := collectFindings(r, v)
	if len(set.findings) != 1 {
		t.Fatalf("an impacted test reached from two functions must be one finding: %d", len(set.findings))
	}
	f = set.findings[0]
	if f.anchor != nil || strings.Join(f.CheckIDs, ",") != "check-1,check-2" || !hasDetail(f, "test declaration", "cart/cart_test.go:20") {
		t.Fatalf("impacted test finding %+v", f)
	}
	if !hasDetail(f, "linked changed functions (approximate)", "Discount (cart/price.go:30, depth 2, interface link); Price (cart/price.go:5, depth 1, static link)") {
		t.Fatalf("linked functions %+v", f.Details)
	}

	r, v = fuzzFixture()
	f = collectFindings(r, v).findings[0]
	if f.anchor == nil || f.anchor.Path != "calc/calc.go" || f.anchor.Line != 10 || f.anchor.Source != locChangedFunction {
		t.Fatalf("fuzz anchor %+v", f.anchor)
	}
	if !hasDetail(f, "input", "Percent(1, 3)") || !hasDetail(f, "baseline value", "33") || !hasDetail(f, "candidate value", "34") || !hasDetail(f, "more rows", "1 further diverging rows are in confidence-report.json") {
		t.Fatalf("fuzz details %+v", f.Details)
	}
	if len(f.Details) > maxDetails {
		t.Fatalf("%d details", len(f.Details))
	}

	r, v = observationFixture()
	f = collectFindings(r, v).findings[0]
	if f.anchor == nil || f.anchor.Source != locModel || f.anchor.Line != 4 || len(f.Artifacts) != 1 || strings.Join(f.HypothesisIDs, ",") != "h1" {
		t.Fatalf("observation finding %+v", f)
	}
	if !hasDetail(f, "recorded key", "Discount(5,33)") || !hasDetail(f, "baseline value", "4") || !hasDetail(f, "candidate value", "3") {
		t.Fatalf("observation details %+v", f.Details)
	}

	r, v = intentFixture()
	f = collectFindings(r, v).findings[0]
	if f.Statement != "A model-written test for AC-2 failed on the candidate; there is no baseline control, and the test or its reading of the criterion may be wrong." {
		t.Fatalf("intent statement %q", f.Statement)
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].SHA256 != strings.Repeat("f", 64) || !hasDetail(f, "criterion AC-2", "A total is never negative.") || f.CriterionID != "AC-2" {
		t.Fatalf("intent finding %+v", f)
	}
	// Two retained intent tests with the same file name: nothing is linked.
	r, v = intentFixture()
	r.Artifacts = append(r.Artifacts, model.Artifact{Path: "artifacts/0a1b-intent-test-2-cart_intent_test.go", Kind: model.ArtifactIntentTest, SHA256: strings.Repeat("1", 64)})
	if f := collectFindings(r, v).findings[0]; len(f.Artifacts) != 0 {
		t.Fatalf("ambiguous artifact linked: %+v", f.Artifacts)
	}

	r, v = mutantFixture()
	set = collectFindings(r, v)
	if len(set.findings) != 1 {
		t.Fatalf("killed mutant listed: %v", classes(set))
	}
	f = set.findings[0]
	if f.MutantID != "mutant-1" || len(f.EvidenceIDs) != 0 || strings.Join(f.CheckIDs, ",") != "mutation-check-1,mutation-check-2" || len(f.Artifacts) != 1 || f.anchor == nil || f.anchor.Source != locMutatedLine {
		t.Fatalf("mutant finding %+v", f)
	}
}

func hasDetail(f finding, label, value string) bool {
	for _, d := range f.Details {
		if d.Label == label && d.Value == value {
			return true
		}
	}
	return false
}

// A location is emitted only for a changed, non-deleted, non-binary file; a
// model-chosen line only when the diff recorded it on the candidate side.
func TestAnchorRules(t *testing.T) {
	r := &model.Report{Change: model.Change{Files: []model.ChangedFile{
		changedFile("a.go", 5),
		{Path: "gone.go", Status: "D", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "delete", OldLine: 1}}}}},
		{Path: "logo.png", Status: "M", Binary: true},
		{Path: "removed-lines-only.go", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "delete", OldLine: 3}}}}},
	}}}
	x := newExportIndex(r, exportVerification{})
	cases := []struct {
		loc  *findingLocation
		want *findingAnchor
	}{
		{&findingLocation{Path: "a.go", Line: 5, Source: locModel}, &findingAnchor{Path: "a.go", Line: 5, EndLine: 5, Source: locModel}},
		{&findingLocation{Path: "a.go", Line: 4, Source: locModel}, &findingAnchor{Path: "a.go", Line: 4, EndLine: 4, Source: locModel}}, // context line
		{&findingLocation{Path: "a.go", Line: 40, Source: locModel}, &findingAnchor{Path: "a.go", FileLevel: true, Source: locModel}},
		{&findingLocation{Path: "a.go", Line: 0, Source: locModel}, &findingAnchor{Path: "a.go", FileLevel: true, Source: locModel}},
		{&findingLocation{Path: "a.go", Line: 40, EndLine: 45, Source: locChangedFunction}, &findingAnchor{Path: "a.go", Line: 40, EndLine: 45, Source: locChangedFunction}},
		{&findingLocation{Path: "a.go", Line: 40, EndLine: 2, Source: locMutatedLine}, &findingAnchor{Path: "a.go", Line: 40, EndLine: 40, Source: locMutatedLine}},
		{&findingLocation{Path: "elsewhere.go", Line: 1, Source: locModel}, nil},
		{&findingLocation{Path: "gone.go", Line: 1, Source: locChangedFunction}, nil},
		{&findingLocation{Path: "logo.png", Line: 1, Source: locModel}, nil},
		{&findingLocation{Path: "removed-lines-only.go", Line: 3, Source: locModel}, nil},
		{nil, nil},
	}
	for i, tc := range cases {
		got := anchorFinding(tc.loc, x)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("case %d: got %+v, want %+v", i, got, tc.want)
		}
	}
}

func TestGuardRejectsMalformedFindings(t *testing.T) {
	r, v := baseTestFixture()
	x := newExportIndex(r, v)
	valid := finding{Class: ClassBaseTestFailsOnCandidate, Status: model.StatusFailsOnCandidate, Identity: "x", EvidenceIDs: []string{"evidence-1"}, CheckIDs: []string{"check-1", "check-2"}}
	if _, ok := normalizeFinding(valid, x); !ok {
		t.Fatal("valid finding refused")
	}
	for name, mutate := range map[string]func(*finding){
		"unknown class":      func(f *finding) { f.Class = "masked_test_change" },
		"status of a class":  func(f *finding) { f.Status = model.StatusReproduced },
		"no identity":        func(f *finding) { f.Identity = "" },
		"no check":           func(f *finding) { f.CheckIDs = nil },
		"no evidence":        func(f *finding) { f.EvidenceIDs = nil },
		"unresolved check":   func(f *finding) { f.CheckIDs = append(f.CheckIDs, "check-9") },
		"unresolved record":  func(f *finding) { f.EvidenceIDs = append(f.EvidenceIDs, "evidence-9") },
		"empty check ID":     func(f *finding) { f.CheckIDs = []string{""} },
		"empty evidence ID":  func(f *finding) { f.EvidenceIDs = []string{""} },
		"judgment is status": func(f *finding) { f.Status = model.JudgmentUnexpectedChange },
	} {
		f := valid
		f.CheckIDs = append([]string{}, valid.CheckIDs...)
		f.EvidenceIDs = append([]string{}, valid.EvidenceIDs...)
		mutate(&f)
		if _, ok := normalizeFinding(f, x); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	// Strings and lists are bounded.
	long := valid
	long.Title = strings.Repeat("é", 400)
	for i := 0; i < 12; i++ {
		long.Details = append(long.Details, detail{"label", strings.Repeat("v", 600)})
	}
	for i := 0; i < 30; i++ {
		long.HypothesisIDs = append(long.HypothesisIDs, fmt.Sprintf("h%02d", i))
	}
	got, ok := normalizeFinding(long, x)
	if !ok || !got.truncated || len(got.Title) > maxTitleBytes+len(ellipsis) || len(got.Details) != maxDetails || len(got.HypothesisIDs) != maxIDsPerList {
		t.Fatalf("not bounded: ok %v truncated %v title %d details %d ids %d", ok, got.truncated, len(got.Title), len(got.Details), len(got.HypothesisIDs))
	}
	for _, d := range got.Details {
		if len(d.Value) > maxValueBytes+len(ellipsis) {
			t.Fatalf("detail of %d bytes", len(d.Value))
		}
	}
}

// Findings are ordered by class rank, then severity, location and identity,
// and the order and fingerprints do not depend on the input order.
func TestFindingOrderAndFingerprints(t *testing.T) {
	r := &model.Report{}
	var v exportVerification
	for _, fixture := range []classFixture{mutantFixture, intentFixture, observationFixture, fuzzFixture, impactFixture, baseTestFixture} {
		part, pv := fixture()
		merged := mergeFixtures(r, v, part, pv)
		r, v = merged.r, merged.v
	}
	set := collectFindings(r, v)
	want := []string{ClassBaseTestFailsOnCandidate, ClassImpactedTestFailsOnCandidate, ClassFuzzDivergence, ClassObservedDivergence, ClassIntentTestFailed, ClassSurvivingMutant}
	if strings.Join(classes(set), ",") != strings.Join(want, ",") {
		t.Fatalf("order %v", classes(set))
	}
	fingerprints := map[string]bool{}
	for _, f := range set.findings {
		if fingerprints[f.fingerprint] {
			t.Fatalf("duplicate fingerprint %s", f.fingerprint)
		}
		fingerprints[f.fingerprint] = true
	}
	// Reversed collections give the same findings.
	reversed := *r
	reversed.Checks = reverseChecks(r.Checks)
	reversed.Evidence = reverseEvidence(r.Evidence)
	again := collectFindings(&reversed, v)
	if describeFindings(set) != describeFindings(again) {
		t.Fatalf("findings depend on record order:\n%s\n%s", describeFindings(set), describeFindings(again))
	}
	// A line shift keeps the fingerprint.
	r2, v2 := mutantFixture()
	r2.Mutation.Mutants[0].Line = 7
	r2.Change.Files[0] = changedFile("discount.go", 7)
	r1, v1 := mutantFixture()
	if collectFindings(r1, v1).findings[0].fingerprint != collectFindings(r2, v2).findings[0].fingerprint {
		t.Fatal("fingerprint depends on the line")
	}
}

// describeFindings renders the exported fields of every finding.
func describeFindings(set findingSet) string {
	var b strings.Builder
	for _, f := range set.findings {
		fmt.Fprintf(&b, "%s|%s|%s|%v|%v|%v|%v|%v|", f.Class, f.Status, f.fingerprint, f.EvidenceIDs, f.CheckIDs, f.HypothesisIDs, f.Details, f.Artifacts)
		if f.anchor != nil {
			fmt.Fprintf(&b, "%+v", *f.anchor)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "omitted %d", set.omitted)
	return b.String()
}

type mergedFixture struct {
	r *model.Report
	v exportVerification
}

// mergeFixtures combines two fixtures whose IDs are disjoint after renaming
// the second one's check and evidence IDs.
func mergeFixtures(a *model.Report, av exportVerification, b *model.Report, bv exportVerification) mergedFixture {
	suffix := fmt.Sprintf("-%d", len(a.Evidence)+len(a.Checks)+1)
	rename := func(id string) string {
		if id == "" {
			return ""
		}
		return id + suffix
	}
	out := *a
	v := exportVerification{evidence: map[string]string{}, mutants: map[string]string{}}
	for k, s := range av.evidence {
		v.evidence[k] = s
	}
	for k, s := range av.mutants {
		v.mutants[k] = s
	}
	for k, s := range bv.evidence {
		v.evidence[rename(k)] = s
	}
	for k, s := range bv.mutants {
		v.mutants[k] = s
	}
	for _, c := range b.Checks {
		c.ID = rename(c.ID)
		out.Checks = append(out.Checks, c)
	}
	for _, e := range b.Evidence {
		e.ID, e.CheckID, e.BaseCheckID, e.RepeatCheckID = rename(e.ID), rename(e.CheckID), rename(e.BaseCheckID), rename(e.RepeatCheckID)
		out.Evidence = append(out.Evidence, e)
	}
	out.Change.Files = append(append([]model.ChangedFile{}, a.Change.Files...), b.Change.Files...)
	out.Artifacts = append(append([]model.Artifact{}, a.Artifacts...), b.Artifacts...)
	out.IntentCriteria = append(append([]model.IntentCriterion{}, a.IntentCriteria...), b.IntentCriteria...)
	for _, h := range b.IntentTestFailures {
		h.EvidenceIDs = []string{rename(h.EvidenceIDs[0])}
		out.IntentTestFailures = append(out.IntentTestFailures, h)
	}
	for _, d := range b.Divergences {
		d.EvidenceID = rename(d.EvidenceID)
		ids := []string{}
		for _, id := range d.CheckIDs {
			ids = append(ids, rename(id))
		}
		d.CheckIDs = ids
		out.Divergences = append(out.Divergences, d)
	}
	if b.BaseTests != nil {
		bt := *b.BaseTests
		bt.Tests = append([]model.BaseTest{}, bt.Tests...)
		for i := range bt.Tests {
			bt.Tests[i].EvidenceID = rename(bt.Tests[i].EvidenceID)
		}
		out.BaseTests = &bt
	}
	if b.Impact != nil {
		im := *b.Impact
		im.ChangedFunctions = append([]model.ImpactFunction{}, im.ChangedFunctions...)
		for i := range im.ChangedFunctions {
			im.ChangedFunctions[i].Tests = append([]model.ImpactTest{}, im.ChangedFunctions[i].Tests...)
			for j := range im.ChangedFunctions[i].Tests {
				im.ChangedFunctions[i].Tests[j].EvidenceID = rename(im.ChangedFunctions[i].Tests[j].EvidenceID)
			}
		}
		out.Impact = &im
	}
	if b.Mutation != nil {
		out.Mutation = b.Mutation
	}
	return mergedFixture{&out, v}
}

func reverseChecks(in []model.Check) []model.Check {
	out := make([]model.Check, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

func reverseEvidence(in []model.Evidence) []model.Evidence {
	out := make([]model.Evidence, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

// The finding cap keeps the strongest findings and counts the rest.
func TestFindingCap(t *testing.T) {
	r := manyMutants(maxFindings + 500)
	set := collectFindings(r, allSurvived(r))
	if len(set.findings) != maxFindings || set.omitted != 500 {
		t.Fatalf("%d findings, %d omitted", len(set.findings), set.omitted)
	}
}

// manyMutants returns a report with n verifiable surviving mutants, one per
// added line, sharing one control check.
func manyMutants(n int) *model.Report {
	lines := make([]int, n)
	for i := range lines {
		lines[i] = i + 1
	}
	r := &model.Report{
		Change:   model.Change{Files: []model.ChangedFile{changedFile("big.go", lines...)}},
		Mutation: &model.Mutation{Status: model.MutationRan, Checks: []model.Check{{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS"}}},
	}
	for i := 1; i <= n; i++ {
		check := fmt.Sprintf("mutation-check-%d", i+1)
		r.Mutation.Checks = append(r.Mutation.Checks, model.Check{ID: check, Kind: model.CheckMutant, Status: "PASS"})
		r.Mutation.Mutants = append(r.Mutation.Mutants, model.Mutant{ID: fmt.Sprintf("mutant-%d", i), Path: "big.go", Line: i, Package: "./", Operator: "comparison",
			Original: fmt.Sprintf("if x%d > 1 {", i), Mutated: fmt.Sprintf("if x%d >= 1 {", i), Status: model.MutantSurvived, CheckID: check, ControlCheckID: "mutation-check-1",
			PatchSHA256: strings.Repeat("9", 64), TestsRun: 1})
	}
	return r
}

func allSurvived(r *model.Report) exportVerification {
	v := exportVerification{evidence: map[string]string{}, mutants: map[string]string{}}
	for _, m := range r.Mutation.Mutants {
		v.mutants[m.ID] = m.Status
	}
	return v
}
