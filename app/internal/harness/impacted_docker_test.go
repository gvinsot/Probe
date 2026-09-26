package harness

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Real sandboxes: the unchanged cart tests run on the baseline and on the
// candidate, whose Total skips the first price. TestTotal fails on the
// candidate; TestCount passed inside that failed run and gets a run pair of
// its own. No container survives.
func TestDockerImpactedTestsRealGo(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
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
	// On a loaded host the stage's 180 s sub-cap can run out before the retry
	// pair; the product limit stays, and the test then records why.
	switch count := res.Tests[1]; {
	case count.Status == model.StatusPassesOnCandidate && got == "impacted_test_base:PASS,impacted_test_candidate:FAIL,impacted_test_base:PASS,impacted_test_candidate:PASS":
	case count.Status == model.StatusUnverified && (strings.HasPrefix(count.Reason, impactedPassedInsideFailure) || count.Reason == reasonCandidateTimeout):
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
