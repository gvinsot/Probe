package harness

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Real sandboxes: the unchanged cart tests run on the baseline and on the
// candidate, whose Total skips the first price. TestTotal fails on the
// candidate; TestCount passed inside that failed run and gets a run pair of
// its own. No container survives.
func TestDockerImpactedTestsRealGo(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	h, names := dockerRunFixture(t, image, brokenTotal)
	h.opts.Commands["generated_test"] = []string{"go", "test", "{package}"}
	selected := []model.ImpactTest{
		{Name: "TestTotal", Path: "cart/cart_test.go", Line: 5, Package: "example.test/shop/cart", Depth: 1, Resolution: model.ResolutionStatic},
		{Name: "TestCount", Path: "cart/cart_test.go", Line: 11, Package: "example.test/shop/cart", Depth: 1, Resolution: model.ResolutionStatic},
	}
	res := h.RunImpactedTests(context.Background(), selected)
	if res.Status != model.ImpactTestsRan || res.Errors != 0 || res.Tests[0].Status != model.StatusFailsOnCandidate {
		t.Fatalf("result %+v\nchecks %+v", res, h.Checks())
	}
	var kinds []string
	for _, c := range h.Checks() {
		kinds = append(kinds, c.Kind+":"+c.Status)
		if c.Status == "ERROR" {
			t.Fatalf("check %s ERROR: %s", c.ID, c.Output)
		}
	}
	got := strings.Join(kinds, ",")
	if !strings.HasPrefix(got, "impacted_test_base:PASS,impacted_test_candidate:FAIL") {
		t.Fatalf("checks %s", got)
	}
	// On a loaded host the stage's 180 s sub-cap or a run timeout can end the
	// retry pair; the product limit stays, and the test then records why. Any
	// other reason, such as a retry pair that was never attempted, fails.
	switch count := res.Tests[1]; {
	case count.Status == model.StatusPassesOnCandidate && got == "impacted_test_base:PASS,impacted_test_candidate:FAIL,impacted_test_base:PASS,impacted_test_candidate:PASS":
	case count.Status == model.StatusUnverified && impactedRetryCutUnderLoad(count.Reason):
		t.Logf("the retry pair gave no result within the sub-cap: %s", count.Reason)
	default:
		t.Fatalf("TestCount %+v; checks %s", count, got)
	}
	for _, tt := range res.Tests {
		e := itEvidence(t, h, tt.EvidenceID)
		if e.Kind != model.EvidenceImpactedTestDifferential || e.Path != "cart/cart_test.go" || strings.Join(e.TestNames, ",") != tt.Name {
			t.Fatalf("evidence %+v", e)
		}
		if status, reason := ClassifyExistingTest(itCheck(t, h, e.BaseCheckID), itCheck(t, h, e.CheckID), tt.Name); status != tt.Status {
			t.Fatalf("%s: recorded checks classify as %s (%s)", tt.Name, status, reason)
		}
	}
	assertNoContainers(t, *names)
}

// impactedRetryCutUnderLoad reports whether reason is one a retry pair that
// ran out of time can give: the retry candidate run timed out, or the retry
// pair gave no result because a run timed out, the stage's sub-cap was used up
// or the shared budget left no room for a run.
func impactedRetryCutUnderLoad(reason string) bool {
	if reason == reasonCandidateTimeout {
		return true
	}
	inner, ok := strings.CutPrefix(reason, impactedPassedInsideFailure+"; its own run pair gave no result (")
	if !ok {
		return false
	}
	for _, cut := range []string{"timed out", "time limit of this stage was used up", "did not start (" + strings.TrimSuffix(budgetExhaustedText, "."), "did not start (" + strings.TrimSuffix(budgetReservedText, ".")} {
		if strings.Contains(inner, cut) {
			return true
		}
	}
	return false
}

func TestImpactedRetryCutUnderLoad(t *testing.T) {
	for reason, want := range map[string]bool{
		reasonCandidateTimeout: true,
		impactedPassedInsideFailure + "; its own run pair gave no result (the 180 s time limit of this stage was used up)":                                                     true,
		impactedPassedInsideFailure + "; its own run pair gave no result (the baseline run check-3 timed out, so it was not run on candidate code)":                            true,
		impactedPassedInsideFailure + "; its own run pair gave no result (the candidate run check-4 did not start (Sandbox runtime budget exhausted), so no result was drawn)": true,
		impactedPassedInsideFailure + "; no run pair of its own was attempted because the failed run held only this test":                                                      false,
		impactedPassedInsideFailure + "; no run pair of its own was attempted because every test of the failed run passed in it":                                               false,
		impactedPassedInsideFailure + "; its own run pair gave no result (the test failed on the baseline (check-3), so it was not run on candidate code)":                     false,
		reasonCandidateOutcome: false,
	} {
		if got := impactedRetryCutUnderLoad(reason); got != want {
			t.Errorf("%q: %v, want %v", reason, got, want)
		}
	}
}
