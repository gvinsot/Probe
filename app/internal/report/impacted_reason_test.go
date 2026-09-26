package report

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Integration agent I (F6b handoff items B and C): a stored impacted-test
// result that the evidence does not support becomes UNVERIFIED with a fixed
// reason, idempotently, and the Markdown shows the reason of every listed test
// without a FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE result.
func TestImpactedTestReasonsAreShownAndUnsupportedResultsExplained(t *testing.T) {
	r := impactedReport()
	tests := r.Impact.ChangedFunctions[0].Tests
	if tests[0].Status == "" {
		// The fixture stores results as the stage records them.
		for i, status := range []string{model.StatusFailsOnCandidate, model.StatusPassesOnCandidate, model.StatusPassesOnCandidate} {
			r.Impact.ChangedFunctions[0].Tests[i].Status = status
		}
	}
	// Break the candidate check of TestTotal's record: its stored
	// FAILS_ON_CANDIDATE no longer re-derives.
	r.Checks[2].Truncated = true
	r.Impact.ChangedFunctions[0].Tests[0].Status = model.StatusFailsOnCandidate
	r.Impact.ChangedFunctions[0].Tests[0].Reason = ""
	// A test the stage selected but could not run carries its own reason.
	r.Impact.ChangedFunctions[0].Tests = append(r.Impact.ChangedFunctions[0].Tests, model.ImpactTest{Name: "TestLater", Path: "price/later_test.go", Line: 3, Package: "example.test/shop/price", Depth: 2, Resolution: model.ResolutionStatic, Reason: "not run: the 180 s sub-cap was reached"})
	Finalize(r, true)
	first := r.Impact.ChangedFunctions[0].Tests
	if first[0].Status != model.StatusUnverified || first[0].Reason != impactTestUnsupportedText {
		t.Fatalf("unsupported stored result: %+v", first[0])
	}
	if first[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("a supported result changed: %+v", first[1])
	}
	md := string(renderMarkdown(r))
	for _, want := range []string{
		"test TestTotal at price/price\\_test.go:" + strconv.Itoa(first[0].Line) + " (depth 1, static; UNVERIFIED (evidence-1); the recorded evidence does not support this result)",
		"; not run: the 180 s sub-cap was reached)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "PASSES\\_ON\\_CANDIDATE (evidence-3); ") {
		t.Error("a supported result shows a reason")
	}
	// Idempotent: a second Finalize keeps the reason and the status.
	Finalize(r, true)
	if got := r.Impact.ChangedFunctions[0].Tests[0]; got.Status != model.StatusUnverified || got.Reason != impactTestUnsupportedText {
		t.Fatalf("second Finalize: %+v", got)
	}
}
