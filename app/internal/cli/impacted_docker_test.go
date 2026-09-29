package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
)

const (
	// impactedPriceTests are the unchanged tests of the price package: TestTotal
	// fails on the candidate, TestTotalZero passes on both revisions.
	impactedPriceTests = "package price\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total([]int{1, 2, 3}) != 6 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n\nfunc TestTotalZero(t *testing.T) {\n\tif Total([]int{0}) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n"
	// impactedBrokenTest fails on the baseline too.
	impactedBrokenTest = "\nfunc TestBroken(t *testing.T) {\n\tif Total([]int{1}) != 99 {\n\t\tt.Fatal(\"fails on the baseline too\")\n\t}\n}\n"
)

// impactedShopFixture is the shop fixture with the given price tests, which
// the candidate does not modify: the candidate's Total skips the first item.
func impactedShopFixture(t *testing.T, priceTests string) string {
	t.Helper()
	dir := shopFixture(t)
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "price/price_test.go", priceTests)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "price tests")
	git(t, dir, "checkout", "-q", "candidate")
	git(t, dir, "rebase", "-q", "main")
	return dir
}

// impactedReview runs review --impacted-tests on a fixture and returns the
// exit code, the report, the output directory and the console output.
func impactedReview(t *testing.T, dir, policy string, extra ...string) (int, model.Report, string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report")
	args := append([]string{"review", "--repo", dir, "--base", "main", "--head", "candidate", "--config", policy, "--impacted-tests", "--reviewer=false", "--out", out}, extra...)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr, "integration")
	data, err := os.ReadFile(filepath.Join(out, "confidence-report.json"))
	if err != nil {
		t.Fatalf("exit %d, no report: %v\n%s", code, err, stderr.String())
	}
	var r model.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return code, r, out, stdout.String() + stderr.String()
}

// impactedTestsByName returns the listed impacted tests by name; every entry
// of one test must carry the same result.
func impactedTestsByName(t *testing.T, im *model.Impact) map[string]model.ImpactTest {
	t.Helper()
	out := map[string]model.ImpactTest{}
	for _, f := range im.ChangedFunctions {
		for _, tt := range f.Tests {
			if prev, ok := out[tt.Name]; ok && (prev.Status != tt.Status || prev.EvidenceID != tt.EvidenceID) {
				t.Fatalf("entries of %s disagree: %+v %+v", tt.Name, prev, tt)
			}
			out[tt.Name] = tt
		}
	}
	return out
}

// impactedRetryCutUnderLoad reports whether reason is one a retry pair that
// ran out of time can give (harness texts): the retry candidate run timed out,
// or the retry pair gave no result because a run timed out, the stage's
// sub-cap was used up or the shared budget left no room for a run.
func impactedRetryCutUnderLoad(reason string) bool {
	if reason == "the candidate-side run timed out" {
		return true
	}
	inner, ok := strings.CutPrefix(reason, "the test passed inside a candidate run that failed as a whole, which supports no result on its own; its own run pair gave no result (")
	if !ok {
		return false
	}
	for _, cut := range []string{"timed out", "time limit of this stage was used up", "did not start (Sandbox runtime budget exhausted", "did not start (Sandbox runtime reserved for reviewer experiments"} {
		if strings.Contains(inner, cut) {
			return true
		}
	}
	return false
}

// TestDockerImpactedTestsEndToEnd runs the unchanged tests that reach changed
// functions in real sandboxes (design scenarios (e) and (f) of F6).
func TestDockerImpactedTestsEndToEnd(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Commands = map[string][]string{"test": {"go", "test", "./..."}, "generated_test": {"go", "test", "{package}"}}
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)

	t.Run("e_fails_and_passing_sibling", func(t *testing.T) {
		dir := impactedShopFixture(t, impactedPriceTests)
		code, r, out, console := impactedReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2 (never 1): checks %+v\n%s", code, r.Checks, console)
		}
		if len(r.Checks) == 0 || r.Checks[0].Kind != "test" || r.Checks[0].Status != "FAIL" {
			t.Fatalf("the candidate suite should fail: %+v", r.Checks)
		}
		if len(r.ReproducedIssues) != 0 {
			t.Fatalf("reproduced issues %+v", r.ReproducedIssues)
		}
		im := r.Impact
		if im == nil || im.Status != model.ImpactIndexed || im.TestsStatus != model.ImpactTestsRan {
			t.Fatalf("impact %+v", im)
		}
		tests := impactedTestsByName(t, im)
		if tests["TestTotal"].Status != model.StatusFailsOnCandidate || tests["TestCheckout"].Status != model.StatusFailsOnCandidate {
			t.Fatalf("tests %+v\n%s", tests, console)
		}
		// TestTotalZero passed inside the failed candidate run of its package,
		// so it gets a run pair of its own. On a loaded host the stage's 180 s
		// sub-cap or a run timeout can end that pair first; the product limit
		// stays, and the test then records why. Any other reason, such as a
		// retry pair that was never attempted, fails.
		passes := 1
		switch zero := tests["TestTotalZero"]; {
		case zero.Status == model.StatusPassesOnCandidate:
		case zero.Status == model.StatusUnverified && impactedRetryCutUnderLoad(zero.Reason):
			t.Logf("the retry pair gave no result within the sub-cap: %s", zero.Reason)
			passes = 0
		default:
			t.Fatalf("TestTotalZero %+v", zero)
		}
		for name, tt := range tests {
			var e model.Evidence
			for _, candidate := range r.Evidence {
				if candidate.ID == tt.EvidenceID {
					e = candidate
				}
			}
			base, cand := findCheck(r, e.BaseCheckID), findCheck(r, e.CheckID)
			if e.Kind != model.EvidenceImpactedTestDifferential || e.Runner != "go_test_json" || strings.Join(e.TestNames, ",") != name || e.Path != tt.Path ||
				base.Kind != model.CheckImpactedTestBase || cand.Kind != model.CheckImpactedTestCandidate || strings.Join(base.Command, " ") != strings.Join(cand.Command, " ") || base.Status != "PASS" {
				t.Fatalf("%s: evidence %+v base %+v candidate %+v", name, e, base, cand)
			}
		}
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("check %s is ERROR: %s", c.ID, c.Output)
			}
		}
		// No signal kind is added for impacted tests.
		for _, s := range r.Signals {
			if strings.Contains(s.Kind, "impacted_test") || strings.Contains(s.Kind, "regression") {
				t.Fatalf("signal %+v", s)
			}
		}
		targets := map[string]bool{}
		for _, rt := range r.ReviewTargets {
			if rt.Severity == "high" {
				targets[rt.Path] = true
			}
		}
		if !targets["price/price_test.go"] || !targets["api/handler_test.go"] {
			t.Fatalf("high targets %+v", r.ReviewTargets)
		}
		if want := "Impacted tests: ran; 2 FAILS_ON_CANDIDATE, " + map[int]string{1: "1 PASSES_ON_CANDIDATE, 0 UNVERIFIED", 0: "0 PASSES_ON_CANDIDATE, 1 UNVERIFIED"}[passes]; !strings.Contains(console, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, console)
		}
		md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
		if err != nil || !strings.Contains(string(md), "## Impact Analysis") || !strings.Contains(string(md), "Impacted tests: ran") || !strings.Contains(string(md), "FAILS\\_ON\\_CANDIDATE") {
			t.Fatalf("markdown: %v\n%s", err, md)
		}
		// Re-rendering gives the same files.
		again := filepath.Join(t.TempDir(), "again")
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{"report", "--input", filepath.Join(out, "confidence-report.json"), "--out", again}, &stdout, &stderr, "integration"); code != 0 {
			t.Fatalf("report exit %d: %s", code, stderr.String())
		}
		for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
			first, _ := os.ReadFile(filepath.Join(out, name))
			second, _ := os.ReadFile(filepath.Join(again, name))
			if !bytes.Equal(first, second) {
				t.Fatalf("%s differs after re-rendering", name)
			}
		}
		if status := git(t, dir, "status", "--porcelain"); status != "" {
			t.Fatalf("the checkout changed: %s", status)
		}
	})

	t.Run("f_fails_on_the_baseline_too", func(t *testing.T) {
		dir := impactedShopFixture(t, impactedPriceTests+impactedBrokenTest)
		code, r, _, console := impactedReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2: checks %+v\n%s", code, r.Checks, console)
		}
		tests := impactedTestsByName(t, r.Impact)
		broken := tests["TestBroken"]
		if broken.Status != "" || broken.EvidenceID != "" || !strings.Contains(broken.Reason, "the test failed on the baseline") {
			t.Fatalf("TestBroken %+v\n%s", broken, console)
		}
		if tests["TestTotal"].Status != model.StatusFailsOnCandidate {
			t.Fatalf("TestTotal %+v", tests["TestTotal"])
		}
		// The failed baseline run was narrowed to the tests it records as passed.
		narrowed := false
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("check %s is ERROR: %s", c.ID, c.Output)
			}
			narrowed = narrowed || c.Kind == model.CheckImpactedTestBase && c.Status == "PASS" && strings.Contains(strings.Join(c.Command, " "), "^(TestTotal|TestTotalZero)$")
		}
		if !narrowed {
			t.Fatalf("no narrowed baseline run: %+v", r.Checks)
		}
		found := false
		for _, u := range r.Unverified {
			found = found || u == impactedUnverifiedLine
		}
		if !found {
			t.Fatalf("unverified %q", r.Unverified)
		}
	})
}
