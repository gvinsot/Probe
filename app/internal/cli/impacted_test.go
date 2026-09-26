package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Unit tests of the impacted-test stage in cli (F6b): selection from the
// impact section, recording of the harness outcome, and the stage without
// Docker.

func impactedSection() *model.Impact {
	return &model.Impact{
		Status: model.ImpactIndexed,
		ChangedFunctions: []model.ImpactFunction{
			{Path: "price/price.go", Line: 4, Symbol: "example.test/shop/price.Total", Change: model.ChangeBodyChanged, Indexed: true, TestsTotal: 23, Tests: []model.ImpactTest{
				{Name: "TestTotal", Path: "price/price_test.go", Line: 5, Package: "example.test/shop/price", Depth: 1, Resolution: model.ResolutionStatic},
				{Name: "TestCheckout", Path: "api/handler_test.go", Line: 5, Package: "example.test/shop/api", Depth: 2, Resolution: model.ResolutionStatic},
				{Name: "TestEdited", Path: "price/edited_test.go", Line: 3, Package: "example.test/shop/price", Depth: 1, Resolution: model.ResolutionStatic, FileChanged: true},
			}},
			{Path: "cart/cart.go", Line: 7, Symbol: "example.test/shop/cart.Cart.Total", Change: model.ChangeBodyChanged, Indexed: true, TestsTotal: 2, Tests: []model.ImpactTest{
				// Listed again for another function, closer: the smaller depth is kept.
				{Name: "TestCheckout", Path: "api/handler_test.go", Line: 5, Package: "example.test/shop/api", Depth: 1, Resolution: model.ResolutionInterface},
				{Name: "TestMessage", Path: "notify/notify_test.go", Line: 5, Package: "example.test/shop/notify", Depth: 1, Resolution: model.ResolutionInterface,
					EvidenceID: "evidence-9", Status: model.StatusFailsOnCandidate, Reason: "stored"},
			}},
			{Path: "os/os_windows.go", Line: 3, Symbol: "example.test/shop/os.OS", Change: model.ChangeBodyChanged, Indexed: false, Reason: "the declaration is not in the static Go index", Tests: []model.ImpactTest{}},
		},
		Note: model.ImpactNote,
	}
}

func TestPlanImpactedTests(t *testing.T) {
	plan := planImpactedTests(impactedSection())
	var got []string
	for _, tt := range plan.tests {
		got = append(got, tt.Path+":"+tt.Name+":"+tt.Resolution)
		if tt.EvidenceID != "" || tt.Status != "" || tt.Reason != "" || tt.FileChanged || tt.Line == 0 || tt.Package == "" {
			t.Fatalf("selection carries a stored result or lost its identity: %+v", tt)
		}
	}
	// Smallest depth first, then path and name; one entry per test; changed
	// test files are left out.
	if strings.Join(got, ",") != "api/handler_test.go:TestCheckout:interface,notify/notify_test.go:TestMessage:interface,price/price_test.go:TestTotal:static" {
		t.Fatalf("selection %v", got)
	}
	if plan.changedFile != 1 || plan.unlisted != 20 || plan.status != "" {
		t.Fatalf("plan %+v", plan)
	}

	for _, tc := range []struct {
		name           string
		impact         *model.Impact
		status, reason string
	}{
		{"not applicable", &model.Impact{Status: model.ImpactNotApplicable}, model.ImpactTestsNoCandidates, "no indexable Go file changed"},
		{"unavailable", &model.Impact{Status: model.ImpactUnavailable, Reason: "no go.mod"}, model.ImpactTestsNotRun, "the static Go index is unavailable"},
		{"unknown status", &model.Impact{Status: "complete"}, model.ImpactTestsNotRun, "no usable index"},
		{"no reaching test", &model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{{Indexed: true, Tests: []model.ImpactTest{}}}}, model.ImpactTestsNoCandidates, "lists no existing Go test that reaches a changed function within 3 references, which is not proof that none exists"},
		{"only changed test files", &model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{{Indexed: true, Tests: []model.ImpactTest{{Name: "TestA", Path: "a_test.go", Depth: 1, FileChanged: true}}}}}, model.ImpactTestsNoCandidates, "declared in a test file the change modified"},
		{"limited without tests", &model.Impact{Status: model.ImpactLimited, ChangedFunctions: []model.ImpactFunction{}}, model.ImpactTestsNoCandidates, "; the index is limited, so reaching tests may be missing"},
	} {
		plan := planImpactedTests(tc.impact)
		if len(plan.tests) != 0 || plan.status != tc.status || !strings.Contains(plan.reason, tc.reason) {
			t.Errorf("%s: plan %+v", tc.name, plan)
		}
	}
}

// Sections without a selectable test record their status without any run; a
// stage that cannot select requests review through an Unverified entry.
func TestRunImpactedStageWithoutSelection(t *testing.T) {
	for _, tc := range []struct {
		impact     *model.Impact
		status     string
		unverified bool
	}{
		{&model.Impact{Status: model.ImpactNotApplicable, ChangedFunctions: []model.ImpactFunction{}}, model.ImpactTestsNoCandidates, false},
		{&model.Impact{Status: model.ImpactUnavailable, ChangedFunctions: []model.ImpactFunction{}}, model.ImpactTestsNotRun, true},
		{&model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{{Indexed: true, Tests: []model.ImpactTest{{Name: "TestA", Path: "a_test.go", Depth: 1, FileChanged: true}}}}}, model.ImpactTestsNoCandidates, false},
	} {
		r := &model.Report{Impact: tc.impact}
		var errOut bytes.Buffer
		// No harness is needed: nothing is selected, so nothing runs.
		if runImpactedTests(context.Background(), nil, r, impactResult{report: tc.impact}, &errOut) {
			t.Fatal("reported an operational failure")
		}
		if r.Impact.TestsStatus != tc.status || r.Impact.TestsReason == "" || errOut.Len() != 0 {
			t.Fatalf("section %+v, stderr %q", r.Impact, errOut.String())
		}
		if got := len(r.Unverified) == 1 && strings.HasPrefix(r.Unverified[0], impactedUnverifiedPrefix); got != tc.unverified {
			t.Fatalf("unverified %q", r.Unverified)
		}
		for _, f := range r.Impact.ChangedFunctions {
			for _, tt := range f.Tests {
				if tt.Reason != impactedChangedFileReason || tt.Status != "" {
					t.Fatalf("test %+v", tt)
				}
			}
		}
	}
	// Without an impact section there is nothing to record into.
	r := &model.Report{}
	if runImpactedTests(context.Background(), nil, r, impactResult{}, &bytes.Buffer{}) || r.Impact != nil || len(r.Unverified) != 0 {
		t.Fatalf("report %+v", r)
	}
}

// The harness outcome lands on every listed entry of the same test; the
// changed-file entries get their reason; limits and unlisted tests are stated
// in tests_reason; a selected test without a result adds the Unverified entry.
func TestRecordImpactedOutcome(t *testing.T) {
	r := &model.Report{Impact: impactedSection()}
	plan := planImpactedTests(r.Impact)
	out := harness.ImpactedTests{Status: model.ImpactTestsRan, Capped: 1, Tests: []model.ImpactTest{
		{Name: "TestCheckout", Path: "api/handler_test.go", EvidenceID: "evidence-2", Status: model.StatusPassesOnCandidate},
		{Name: "TestMessage", Path: "notify/notify_test.go", Reason: "not run: the stage runs at most 16 tests from at most 4 packages per review"},
		{Name: "TestTotal", Path: "price/price_test.go", EvidenceID: "evidence-1", Status: model.StatusFailsOnCandidate},
	}}
	recordImpactedOutcome(r, plan, out)
	fns := r.Impact.ChangedFunctions
	if a, b := fns[0].Tests[1], fns[1].Tests[0]; a.EvidenceID != "evidence-2" || b.EvidenceID != "evidence-2" || a.Status != model.StatusPassesOnCandidate || b.Status != model.StatusPassesOnCandidate {
		t.Fatalf("TestCheckout entries %+v %+v", a, b)
	}
	if tt := fns[0].Tests[0]; tt.EvidenceID != "evidence-1" || tt.Status != model.StatusFailsOnCandidate || tt.Reason != "" {
		t.Fatalf("TestTotal %+v", tt)
	}
	if tt := fns[0].Tests[2]; tt.Reason != impactedChangedFileReason || tt.EvidenceID != "" {
		t.Fatalf("changed-file test %+v", tt)
	}
	// A stored result of a previous stage is replaced by this run's.
	if tt := fns[1].Tests[1]; tt.EvidenceID != "" || tt.Status != "" || !strings.HasPrefix(tt.Reason, "not run: the stage runs at most 16 tests") {
		t.Fatalf("capped test %+v", tt)
	}
	want := "1 of 3 selected tests were not run: the stage runs at most 16 tests from at most 4 packages (test files, with a {file} template) per review; 1 reaching tests declared in test files the change modified were not run (--base-tests runs the baseline versions of changed tests); 20 reaching tests beyond the 20 listed per changed function were not considered"
	if r.Impact.TestsStatus != model.ImpactTestsRan || r.Impact.TestsReason != want {
		t.Fatalf("tests_status %q, tests_reason %q", r.Impact.TestsStatus, r.Impact.TestsReason)
	}
	// Every selected test got a result: the capped one does not add an entry.
	if len(r.Unverified) != 0 {
		t.Fatalf("unverified %q", r.Unverified)
	}
	// A selected test without a result does.
	out.Tests[0].Status, out.Tests[0].Reason = model.StatusUnverified, "the candidate-side run timed out"
	r = &model.Report{Impact: impactedSection()}
	recordImpactedOutcome(r, plan, out)
	if len(r.Unverified) != 1 || r.Unverified[0] != impactedUnverifiedLine {
		t.Fatalf("unverified %q", r.Unverified)
	}
	// A stage that did not run says why, in the section and as an Unverified entry.
	r = &model.Report{Impact: impactedSection()}
	recordImpactedOutcome(r, plan, harness.ImpactedTests{Status: model.ImpactTestsNotRun, Reason: "no run started: Sandbox runtime budget exhausted.", Tests: out.Tests})
	if r.Impact.TestsStatus != model.ImpactTestsNotRun || r.Impact.TestsReason != "no run started: Sandbox runtime budget exhausted." || len(r.Unverified) != 1 || r.Unverified[0] != impactedUnverifiedPrefix+r.Impact.TestsReason {
		t.Fatalf("not run: %+v %q", r.Impact, r.Unverified)
	}
}

// Selection and recording without Docker: the shop fixture's reaching tests
// are selected and, with no image, every run is SKIPPED; the stage is not_run
// with that reason, every selected test says why, and nothing is operational.
func TestRunImpactedTestsWithoutDocker(t *testing.T) {
	dir := shopFixture(t)
	repo, change := openChange(t, dir, "main", "candidate")
	res, err := analyzeImpact(context.Background(), repo, change, true)
	if err != nil {
		t.Fatal(err)
	}
	h := stageHarness(t, repo, change, []string{"go", "test", "{package}"})
	r := &model.Report{Impact: res.report}
	var errOut bytes.Buffer
	if runImpactedTests(context.Background(), h, r, res, &errOut) {
		t.Fatal("reported an operational failure")
	}
	if !strings.Contains(errOut.String(), "Running 2 unchanged Go tests that statically reach changed code on baseline and candidate") {
		t.Fatalf("progress line %q", errOut.String())
	}
	const skipped = "no run started: No Docker image configured; repository code was not executed."
	if r.Impact.TestsStatus != model.ImpactTestsNotRun || r.Impact.TestsReason != skipped || len(r.Unverified) != 1 || r.Unverified[0] != impactedUnverifiedPrefix+skipped {
		t.Fatalf("section %q %q, unverified %q", r.Impact.TestsStatus, r.Impact.TestsReason, r.Unverified)
	}
	seen := 0
	for _, f := range r.Impact.ChangedFunctions {
		for _, tt := range f.Tests {
			seen++
			if tt.Status != "" || tt.EvidenceID != "" || !strings.Contains(tt.Reason, "did not start (No Docker image configured") {
				t.Fatalf("test %+v", tt)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("%d listed tests", seen)
	}
	for _, c := range h.Checks() {
		if c.Kind != model.CheckImpactedTestBase || c.Status != "SKIPPED" {
			t.Fatalf("check %+v", c)
		}
	}
	if line := impactLine(r.Impact); !strings.HasSuffix(line, "Impacted tests: not_run.") {
		t.Fatalf("stdout line %q", line)
	}
}

// The stage's own texts make no claim that the §5 non-claims forbid.
func TestImpactedTextsMakeNoClaim(t *testing.T) {
	plan := planImpactedTests(impactedSection())
	texts := []string{impactedUnverifiedPrefix, impactedUnverifiedLine, impactedChangedFileReason,
		impactedRanReason(plan, harness.ImpactedTests{Capped: 2, Tests: make([]model.ImpactTest, 5)})}
	for _, im := range []*model.Impact{{Status: model.ImpactNotApplicable}, {Status: model.ImpactUnavailable}, {Status: model.ImpactLimited}, {Status: "x"},
		{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{{Indexed: true, Tests: []model.ImpactTest{{Name: "TestA", Path: "a_test.go", FileChanged: true}}}}}} {
		texts = append(texts, planImpactedTests(im).reason)
	}
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range []string{"regression", "bug", "defect", "caused", "tested", "verified", "covers", "safe", "correct", "complete", "all tests", "%"} {
			if strings.Contains(lower, word) {
				t.Errorf("%q uses %q", text, word)
			}
		}
	}
}
