package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const btPkg = "example.test/clamp"

func btEvents(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n", btPkg, pairs[i])
		fmt.Fprintf(&b, "{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", pairs[i+1], btPkg, pairs[i])
	}
	return b.String()
}

// baseTestsReport is a recorded run pair of TestClampUpper: it passed on the
// baseline tree (check-1) and failed on the hybrid tree (check-2).
func baseTestsReport() *model.Report {
	command := []string{"go", "test", ".", "-json", "-count=1", "-run", "^(TestClampUpper)$"}
	return &model.Report{
		Version: 1,
		Change: model.Change{Files: []model.ChangedFile{{Path: "clamp_test.go", Status: "M", Hunks: []model.Hunk{{OldStart: 14, OldLines: 5, NewStart: 14, NewLines: 5, Lines: []model.DiffLine{
			{Kind: "delete", OldLine: 15, Content: "\tif got := Clamp(50); got != 10 {"},
			{Kind: "add", NewLine: 15, Content: "\tif got := Clamp(50); got < 10 {"},
		}}}}}},
		Checks: []model.Check{
			{ID: "check-1", Kind: model.CheckBaseTestBase, Status: "PASS", ExitCode: 0, Command: command, Output: btEvents("TestClampUpper", "pass")},
			{ID: "check-2", Kind: model.CheckBaseTestHybrid, Status: "FAIL", ExitCode: 1, Command: append([]string(nil), command...), Output: btEvents("TestClampUpper", "fail")},
		},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceBaseTestDifferential, Description: "Baseline version of TestClampUpper", Path: "clamp_test.go",
			CheckID: "check-2", BaseCheckID: "check-1", Status: model.StatusFailsOnCandidate, Runner: "go_test_json", TestNames: []string{"TestClampUpper"}}},
		BaseTests: &model.BaseTests{Status: model.BaseTestsRan, Note: model.BaseTestsNote, Tests: []model.BaseTest{{
			Name: "TestClampUpper", Path: "clamp_test.go", Line: 14, EndLine: 18, CandidatePath: "clamp_test.go", CandidateLine: 14, CandidateEndLine: 18,
			Change: model.BaseTestModified, Status: model.StatusFailsOnCandidate, EvidenceID: "evidence-1"}}},
	}
}

func TestBaseTestsFailsRequestsReviewAndNeverExitOne(t *testing.T) {
	for _, ci := range []bool{false, true} {
		r := baseTestsReport()
		// A model citing the record as a reproduction or dismissal gains nothing.
		r.Hypotheses = []model.Hypothesis{
			{ID: "h1", Title: "Upper bound dropped", Severity: "critical", Status: model.StatusReproduced, EvidenceIDs: []string{"evidence-1"}},
			{ID: "h2", Title: "Fine", Severity: "high", Status: model.StatusDismissed, Rationale: "expected", EvidenceIDs: []string{"evidence-1"}},
			{ID: "h3", Title: "Same", Severity: "high", Status: model.StatusNotReproduced, EvidenceIDs: []string{"evidence-1"}},
		}
		Finalize(r, ci)
		if got := r.BaseTests.Tests[0]; got.Status != model.StatusFailsOnCandidate || got.Reason != "" {
			t.Fatalf("ci=%v: test %+v", ci, got)
		}
		for _, h := range r.Hypotheses {
			if h.Status != model.StatusUnverified {
				t.Fatalf("ci=%v: hypothesis %s supported by base_test_differential: %s", ci, h.ID, h.Status)
			}
		}
		want := 0
		if ci {
			want = 2
		}
		if r.ExitCode != want || len(r.ReproducedIssues) != 0 {
			t.Fatalf("ci=%v: exit %d, reproduced %v", ci, r.ExitCode, r.ReproducedIssues)
		}
	}
	// Without hypotheses, the FAILS item alone requests review.
	r := baseTestsReport()
	r.Checks[1].Output = btEvents("TestClampUpper", "fail")
	Finalize(r, true)
	if r.ExitCode != 2 {
		t.Fatalf("exit %d, want 2", r.ExitCode)
	}
}

func TestBaseTestsVerifierRefusesUnsupportedRecords(t *testing.T) {
	hit := func(liveRuns int) *model.CheckCache {
		return &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("a", 64), LiveRuns: liveRuns}
	}
	cases := []struct {
		name   string
		mutate func(*model.Report)
		status string
	}{
		{"valid", func(*model.Report) {}, model.StatusFailsOnCandidate},
		{"passes", func(r *model.Report) {
			r.Checks[1].Status, r.Checks[1].ExitCode, r.Checks[1].Output = "PASS", 0, btEvents("TestClampUpper", "pass")
			r.Evidence[0].Status = model.StatusPassesOnCandidate
		}, model.StatusPassesOnCandidate},
		{"item status is derived, not stored", func(r *model.Report) { r.BaseTests.Tests[0].Status = model.StatusPassesOnCandidate }, model.StatusFailsOnCandidate},
		{"missing evidence", func(r *model.Report) { r.Evidence = nil }, model.StatusUnverified},
		{"no evidence id", func(r *model.Report) { r.BaseTests.Tests[0].EvidenceID = "" }, model.StatusUnverified},
		{"duplicate evidence", func(r *model.Report) { r.Evidence = append(r.Evidence, r.Evidence[0]) }, model.StatusUnverified},
		{"other kind", func(r *model.Report) { r.Evidence[0].Kind = model.EvidenceDifferentialTest }, model.StatusUnverified},
		{"impacted kind", func(r *model.Report) { r.Evidence[0].Kind = model.EvidenceImpactedTestDifferential }, model.StatusUnverified},
		{"name mismatch", func(r *model.Report) { r.BaseTests.Tests[0].Name = "TestClampLower" }, model.StatusUnverified},
		{"path mismatch", func(r *model.Report) { r.BaseTests.Tests[0].Path = "other_test.go" }, model.StatusUnverified},
		{"two names", func(r *model.Report) { r.Evidence[0].TestNames = []string{"TestClampUpper", "TestClampUpper"} }, model.StatusUnverified},
		{"other runner", func(r *model.Report) { r.Evidence[0].Runner = "jest_json" }, model.StatusUnverified},
		{"secret-shaped name", func(r *model.Report) {
			name := "TestAKIA" + strings.Repeat("A", 16)
			r.Evidence[0].TestNames = []string{name}
			r.BaseTests.Tests[0].Name = name
		}, model.StatusUnverified},
		{"check kinds swapped", func(r *model.Report) { r.Checks[0].Kind, r.Checks[1].Kind = r.Checks[1].Kind, r.Checks[0].Kind }, model.StatusUnverified},
		{"generated kinds", func(r *model.Report) {
			r.Checks[0].Kind, r.Checks[1].Kind = model.CheckGeneratedBase, model.CheckGeneratedCandidate
		}, model.StatusUnverified},
		{"commands differ", func(r *model.Report) { r.Checks[1].Command[3] = "-count=2" }, model.StatusUnverified},
		{"command targets another package", func(r *model.Report) {
			for i := range r.Checks {
				r.Checks[i].Command[2] = "./other"
			}
		}, model.StatusUnverified},
		{"duplicate check", func(r *model.Report) { r.Checks = append(r.Checks, r.Checks[1]) }, model.StatusUnverified},
		{"missing check", func(r *model.Report) { r.Checks = r.Checks[:1] }, model.StatusUnverified},
		{"baseline failed", func(r *model.Report) { r.Checks[0].Status, r.Checks[0].ExitCode = "FAIL", 1 }, model.StatusUnverified},
		{"outputs contradict the stored status", func(r *model.Report) {
			r.Checks[1].Output = btEvents("TestClampUpper", "pass")
		}, model.StatusUnverified},
		{"forged pass on the hybrid tree", func(r *model.Report) {
			r.Checks[1].Output = btEvents("TestClampUpper", "fail") + "{\"Action\":\"pass\",\"Package\":\"" + btPkg + "\",\"Test\":\"TestClampUpper\"}\n"
			r.Checks[1].Status, r.Checks[1].ExitCode = "PASS", 0
			r.Evidence[0].Status = model.StatusPassesOnCandidate
		}, model.StatusUnverified},
		{"truncated hybrid log", func(r *model.Report) { r.Checks[1].Truncated = true }, model.StatusUnverified},
		{"infrastructure error", func(r *model.Report) { r.Checks[1].ExitCode = 125 }, model.StatusUnverified},
		{"replayed baseline for FAILS", func(r *model.Report) { r.Checks[0].Cache = hit(5) }, model.StatusUnverified},
		{"replayed hybrid", func(r *model.Report) { r.Checks[1].Cache = hit(5) }, model.StatusUnverified},
		{"replayed baseline of two live runs for PASSES", func(r *model.Report) {
			r.Checks[0].Cache = hit(2)
			r.Checks[1].Status, r.Checks[1].ExitCode, r.Checks[1].Output = "PASS", 0, btEvents("TestClampUpper", "pass")
			r.Evidence[0].Status = model.StatusPassesOnCandidate
		}, model.StatusPassesOnCandidate},
		{"replayed baseline of one live run for PASSES", func(r *model.Report) {
			r.Checks[0].Cache = hit(1)
			r.Checks[1].Status, r.Checks[1].ExitCode, r.Checks[1].Output = "PASS", 0, btEvents("TestClampUpper", "pass")
			r.Evidence[0].Status = model.StatusPassesOnCandidate
		}, model.StatusUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := baseTestsReport()
			tc.mutate(r)
			Finalize(r, true)
			got := r.BaseTests.Tests[0]
			if got.Status != tc.status {
				t.Fatalf("status %s, want %s (%+v)", got.Status, tc.status, got)
			}
			if tc.status == model.StatusUnverified && got.Reason == "" {
				t.Fatal("an UNVERIFIED item has no reason")
			}
			if r.ExitCode != 2 && tc.status != model.StatusPassesOnCandidate {
				t.Fatalf("exit %d, want 2", r.ExitCode)
			}
		})
	}
}

func TestFinalizeBaseTestsSectionRules(t *testing.T) {
	// A report without the section keeps none and asks nothing.
	r := &model.Report{Version: 1}
	Finalize(r, true)
	if r.BaseTests != nil || r.ExitCode != 0 {
		t.Fatalf("absent section: %+v exit %d", r.BaseTests, r.ExitCode)
	}
	// no_candidates asks nothing; not_run asks a human; the note is restored.
	r = &model.Report{Version: 1, BaseTests: &model.BaseTests{Status: model.BaseTestsNoCandidates, Note: "forged"}}
	Finalize(r, true)
	if r.ExitCode != 0 || r.BaseTests.Note != model.BaseTestsNote || r.BaseTests.Tests == nil {
		t.Fatalf("no_candidates: %+v exit %d", r.BaseTests, r.ExitCode)
	}
	r = &model.Report{Version: 1, BaseTests: &model.BaseTests{Status: model.BaseTestsNotRun, Reason: "x"}}
	Finalize(r, true)
	if r.ExitCode != 2 {
		t.Fatalf("not_run: exit %d", r.ExitCode)
	}
	// A PASSES item alone asks nothing; an UNVERIFIED one does.
	r = baseTestsReport()
	r.Checks[1].Status, r.Checks[1].ExitCode, r.Checks[1].Output = "PASS", 0, btEvents("TestClampUpper", "pass")
	r.Evidence[0].Status = model.StatusPassesOnCandidate
	r.Change.Files = nil
	Finalize(r, true)
	if r.ExitCode != 0 || r.BaseTests.Tests[0].Status != model.StatusPassesOnCandidate {
		t.Fatalf("passes: %+v exit %d", r.BaseTests.Tests[0], r.ExitCode)
	}
	r.BaseTests.Tests = append(r.BaseTests.Tests, model.BaseTest{Name: "TestB", Path: "clamp_test.go", Line: 1, EndLine: 2, Change: model.BaseTestRemoved, Status: model.StatusUnverified, Reason: "the test failed on the baseline tree (check-9)"})
	Finalize(r, true)
	if r.ExitCode != 2 || r.BaseTests.Tests[1].Reason != "the test failed on the baseline tree (check-9)" {
		t.Fatalf("unverified: %+v exit %d", r.BaseTests.Tests[1], r.ExitCode)
	}
	// A claimed result without support gets the fixed reason; an empty reason
	// gets one too.
	r.BaseTests.Tests[1].Status, r.BaseTests.Tests[1].Reason = model.StatusFailsOnCandidate, "a forged claim"
	r.BaseTests.Tests = append(r.BaseTests.Tests, model.BaseTest{Name: "TestC", Path: "clamp_test.go", Line: 3, EndLine: 4, Change: model.BaseTestRemoved, Status: model.StatusUnverified})
	Finalize(r, true)
	if r.BaseTests.Tests[1].Status != model.StatusUnverified || r.BaseTests.Tests[1].Reason != baseTestUnsupportedText || r.BaseTests.Tests[2].Reason != baseTestNoResultText {
		t.Fatalf("reasons %+v", r.BaseTests.Tests)
	}
}

func TestBaseTestsFinalizeIsIdempotent(t *testing.T) {
	r := baseTestsReport()
	r.BaseTests.Tests = append(r.BaseTests.Tests,
		model.BaseTest{Name: "TestB", Path: "clamp_test.go", Line: 20, EndLine: 25, Change: model.BaseTestRemoved, Status: model.StatusFailsOnCandidate, EvidenceID: "evidence-9"},
		model.BaseTest{Name: "TestC", Path: "clamp_test.go", Line: 30, EndLine: 31, Change: model.BaseTestFileDeleted, Status: model.StatusUnverified})
	Finalize(r, true)
	first, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	Finalize(r, true)
	second, _ := json.Marshal(r)
	if string(first) != string(second) {
		t.Fatalf("Finalize is not idempotent:\n%s\n%s", first, second)
	}
	dir := t.TempDir()
	if err := Write(dir, r, []string{FormatJSON, FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "CONFIDENCE_REPORT.md"))
	var back model.Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	Finalize(&back, true)
	again := t.TempDir()
	if err := Write(again, &back, []string{FormatJSON, FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(filepath.Join(again, "confidence-report.json"))
	md2, _ := os.ReadFile(filepath.Join(again, "CONFIDENCE_REPORT.md"))
	if string(data) != string(data2) || string(md) != string(md2) {
		t.Fatal("a re-rendered report differs")
	}
}

func TestBaseTestTargets(t *testing.T) {
	r := baseTestsReport()
	r.BaseTests.Tests = append(r.BaseTests.Tests, model.BaseTest{Name: "TestGone", Path: "gone_test.go", Line: 3, EndLine: 9, Change: model.BaseTestFileDeleted, Status: model.StatusUnverified})
	Finalize(r, false)
	extra := baseTestTargets(r)
	if len(extra) != 2 || extra[0].side != "old" || extra[0].start != 14 || extra[0].end != 18 || extra[0].severity != "high" || extra[1].side != "new" || extra[1].path != "clamp_test.go" {
		t.Fatalf("targets %+v", extra)
	}
	for _, e := range extra {
		for _, word := range []string{"regression", "bug", "masked", "defect"} {
			if strings.Contains(strings.ToLower(e.reason), word) {
				t.Fatalf("target reason uses %q: %s", word, e.reason)
			}
		}
	}
	high := 0
	for _, target := range r.ReviewTargets {
		if target.Severity == "high" && target.Path == "clamp_test.go" {
			high++
		}
	}
	if high != 2 {
		t.Fatalf("review targets %+v", r.ReviewTargets)
	}
	// Only FAILS items are targets.
	r.Checks[1].Output = btEvents("TestClampUpper", "pass")
	r.Checks[1].Status, r.Checks[1].ExitCode = "PASS", 0
	r.Evidence[0].Status = model.StatusPassesOnCandidate
	Finalize(r, false)
	if extra := baseTestTargets(r); len(extra) != 0 {
		t.Fatalf("PASSES produced targets %+v", extra)
	}
}

func TestWriteBaseTestsSection(t *testing.T) {
	r := baseTestsReport()
	hostile := "Test`*[x](y)<b>_!"
	r.BaseTests.Tests = append(r.BaseTests.Tests,
		model.BaseTest{Name: hostile, Path: "a_[b]_test.go", Line: 3, EndLine: 9, Change: model.BaseTestRemoved, Status: model.StatusUnverified, Reason: "the test failed on the baseline tree (check-7) <script>"})
	for i := 0; i < maxPassesShown+2; i++ {
		id := fmt.Sprintf("evidence-p%d", i)
		name := fmt.Sprintf("TestPass%02d", i)
		checkBase, checkHybrid := fmt.Sprintf("check-p%da", i), fmt.Sprintf("check-p%db", i)
		command := []string{"go", "test", ".", "-json", "-count=1", "-run", "^(" + name + ")$"}
		r.Checks = append(r.Checks,
			model.Check{ID: checkBase, Kind: model.CheckBaseTestBase, Status: "PASS", Command: command, Output: btEvents(name, "pass")},
			model.Check{ID: checkHybrid, Kind: model.CheckBaseTestHybrid, Status: "PASS", Command: command, Output: btEvents(name, "pass")})
		r.Evidence = append(r.Evidence, model.Evidence{ID: id, Kind: model.EvidenceBaseTestDifferential, Path: "clamp_test.go", CheckID: checkHybrid, BaseCheckID: checkBase, Status: model.StatusPassesOnCandidate, Runner: "go_test_json", TestNames: []string{name}})
		r.BaseTests.Tests = append(r.BaseTests.Tests, model.BaseTest{Name: name, Path: "clamp_test.go", Line: 40 + i, EndLine: 40 + i, Change: model.BaseTestSharedCodeChanged, Status: model.StatusPassesOnCandidate, EvidenceID: id})
	}
	Finalize(r, true)
	body := section(t, string(Markdown(r)), "## Changed Baseline Tests on Candidate Code")
	fails := strings.Index(body, "FAILS\\_ON\\_CANDIDATE** TestClampUpper")
	unverified := strings.Index(body, "**UNVERIFIED**")
	passes := strings.Index(body, "PASSES\\_ON\\_CANDIDATE** TestPass00")
	if fails < 0 || unverified < fails || passes < unverified {
		t.Fatalf("order FAILS %d, UNVERIFIED %d, PASSES %d:\n%s", fails, unverified, passes, body)
	}
	for _, want := range []string{
		"24 baseline versions of changed Go tests selected: 1 FAILS\\_ON\\_CANDIDATE, 22 PASSES\\_ON\\_CANDIDATE, 1 UNVERIFIED.",
		"TestClampUpper — clamp\\_test.go:14–18 (modified by the change); edited version at clamp\\_test.go:14–18. It passed on the baseline tree and failed on the candidate tree with its package's test files reverted to the baseline (baseline check check-1, hybrid-tree check check-2); evidence-1. One recorded run each: possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge.",
		"- … 2 more PASSES\\_ON\\_CANDIDATE results in confidence-report.json",
		inline(model.BaseTestsNote),
		"Test\\`\\*\\[x\\]\\(y\\)&lt;b&gt;\\_\\! — a\\_\\[b\\]\\_test.go:3–9 (removed by the change): the test failed on the baseline tree \\(check-7\\) &lt;script&gt;.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("section lacks %q:\n%s", want, body)
		}
	}
	// The fixed texts carry no character that inline() would turn into an
	// escaped HTML entity (an apostrophe would render as "&#39;").
	if strings.Contains(body, "&\\#") {
		t.Fatalf("an escaped entity in the fixed texts:\n%s", body)
	}
	if strings.Contains(body, "TestPass21") || strings.Contains(body, "[x](y)") || strings.Contains(body, "<script>") {
		t.Fatalf("collapsed or unescaped content rendered:\n%s", body)
	}
	if strings.Contains(body, baseTestsIntentText) {
		t.Fatal("intent sentence without an intent")
	}
	for _, word := range []string{"regression", "bug", "masked", "defect", "verified", "tested"} {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(body, "UNVERIFIED", "")), word) {
			t.Errorf("section uses %q", word)
		}
	}
	r.Intent = "Clamp keeps its bounds."
	if body := section(t, string(Markdown(r)), "## Changed Baseline Tests on Candidate Code"); !strings.Contains(body, baseTestsIntentText) {
		t.Fatalf("intent sentence missing:\n%s", body)
	}
	// The Recorded Evidence line names both checks.
	if ev := section(t, string(Markdown(r)), "## Recorded Evidence"); !strings.Contains(ev, "Hybrid-tree check: check-2; baseline check: check-1") {
		t.Fatalf("recorded evidence:\n%s", ev)
	}
}

// Every reason text ClassifyExistingTest can return, the change texts, the
// note and the review-target reasons render without an escaped HTML entity:
// inline() turns an apostrophe or a quote into one.
func TestBaseTestReasonsRenderWithoutEntities(t *testing.T) {
	command := []string{"go", "test", ".", "-json", "-count=1", "-run", "^(TestA)$"}
	pass := model.Check{Status: "PASS", Command: command, Output: btEvents("TestA", "pass")}
	candidates := []model.Check{
		pass,
		{Status: "FAIL", ExitCode: 1, Command: command, Output: btEvents("TestA", "fail")},
		{Status: "TIMEOUT", ExitCode: -1, Command: command},
		{Status: "ERROR", ExitCode: 125, Command: command},
		{Status: "PASS", Truncated: true, Command: command, Output: btEvents("TestA", "pass")},
		{Status: "FAIL", ExitCode: 200, Command: command, Output: btEvents("TestA", "fail")},
		{Status: "FAIL", ExitCode: 1, Command: command, Output: "FAIL example.test/clamp [build failed]"},
		{Status: "FAIL", ExitCode: 1, Command: command, Output: btEvents("TestA", "pass")},
		{Status: "PASS", Command: command, Output: btEvents("TestA", "skip")},
		{Status: "PASS", Command: []string{"go", "test"}, Output: btEvents("TestA", "pass")},
	}
	bases := []model.Check{pass, {Status: "FAIL", ExitCode: 1, Command: command}, {Status: "PASS", Command: command, Output: btEvents("TestA", "skip")}}
	reasons := map[string]bool{}
	for _, base := range bases {
		for _, candidate := range candidates {
			_, reason := harness.ClassifyExistingTest(base, candidate, "TestA")
			reasons[reason] = true
		}
	}
	if len(reasons) < 10 {
		t.Fatalf("only %d distinct reasons reached: %v", len(reasons), reasons)
	}
	r := baseTestsReport()
	i := 0
	for reason := range reasons {
		i++
		r.BaseTests.Tests = append(r.BaseTests.Tests, model.BaseTest{Name: fmt.Sprintf("TestR%02d", i), Path: "clamp_test.go", Line: i, EndLine: i, Change: []string{model.BaseTestRemoved, model.BaseTestModified, model.BaseTestSharedCodeChanged, model.BaseTestFileDeleted}[i%4], Status: model.StatusUnverified, Reason: reason})
	}
	r.Intent = "Clamp keeps its bounds."
	Finalize(r, true)
	md := string(Markdown(r))
	for _, heading := range []string{"## Changed Baseline Tests on Candidate Code", "## Suggested Human Review"} {
		if body := section(t, md, heading); strings.Contains(body, "&\\#") || strings.Contains(body, "&amp;") || strings.Contains(body, "&quot;") {
			t.Errorf("%s holds an escaped entity:\n%s", heading, body)
		}
	}
	for reason := range reasons {
		if !strings.Contains(md, inline(reason)) {
			t.Errorf("reason %q not rendered", reason)
		}
	}
}

func TestWriteBaseTestsStatusTexts(t *testing.T) {
	render := func(s *model.BaseTests) string {
		t.Helper()
		r := &model.Report{Version: 1, BaseTests: s}
		Finalize(r, false)
		md := string(Markdown(r))
		if stray := strayHeadings(md); len(stray) > 0 {
			t.Fatalf("unexpected headings %q", stray)
		}
		return section(t, md, "## Changed Baseline Tests on Candidate Code")
	}
	if body := render(&model.BaseTests{Status: model.BaseTestsNoCandidates}); !strings.Contains(body, "No test was selected, so no baseline version was re-run. Only the tests declared in modified, deleted or renamed Go test files are considered; this says nothing about any other test.") || strings.Contains(body, "modified or removed no") {
		t.Fatalf("no_candidates:\n%s", body)
	}
	if body := render(&model.BaseTests{Status: model.BaseTestsNoCandidates, Reason: "no changed files"}); !strings.Contains(body, "Reason: no changed files.") {
		t.Fatalf("no_candidates reason:\n%s", body)
	}
	body := render(&model.BaseTests{Status: model.BaseTestsNotRun, Reason: "no run started: Sandbox runtime budget exhausted.", Tests: []model.BaseTest{{Name: "TestA", Path: "a_test.go", Line: 1, EndLine: 2, Change: model.BaseTestModified, Status: model.StatusUnverified, Reason: "not run: x"}}})
	if !strings.Contains(body, "Not run: no run started: Sandbox runtime budget exhausted.\n") || !strings.Contains(body, "**UNVERIFIED** TestA") {
		t.Fatalf("not_run:\n%s", body)
	}
	if body := render(&model.BaseTests{Status: model.BaseTestsNotRun}); !strings.Contains(body, "Not run: no reason was recorded.") {
		t.Fatalf("not_run without reason:\n%s", body)
	}
	// Absent section, absent heading.
	if md := string(Markdown(&model.Report{Version: 1})); strings.Contains(md, "Changed Baseline Tests") {
		t.Fatal("heading rendered without a section")
	}
}
