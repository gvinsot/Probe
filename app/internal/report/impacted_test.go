package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Tests of the impacted-test verifier (F6b) and of what Finalize derives from
// it through the impact section.

// itEvents renders go test -json events of package example.test/shop/<dir>:
// name, action pairs.
func itEvents(dir string, pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "{\"Action\":\"run\",\"Package\":\"example.test/shop/%s\",\"Test\":%q}\n", dir, pairs[i])
		fmt.Fprintf(&b, "{\"Action\":%q,\"Package\":\"example.test/shop/%s\",\"Test\":%q}\n", pairs[i+1], dir, pairs[i])
	}
	return b.String()
}

func itCommand(target string, names ...string) []string {
	return []string{"go", "test", target, "-json", "-count=1", "-run", "^(" + strings.Join(names, "|") + ")$"}
}

// impactedReport is the report a review with --impacted-tests records for the
// shop fixture: TestTotal fails on the candidate (its sibling TestTotalZero
// passed inside that failed run and got a pair of its own), and TestCheckout
// passes on both.
func impactedReport() *model.Report {
	r := impactReport()
	r.Impact.TestsStatus = model.ImpactTestsRan
	tests := &r.Impact.ChangedFunctions[0].Tests
	*tests = append(*tests, model.ImpactTest{Name: "TestTotalZero", Path: "price/price_test.go", Line: 11, Package: "example.test/shop/price", Depth: 1, Resolution: model.ResolutionStatic})
	r.Impact.ChangedFunctions[0].TestsTotal = 3
	both, zero, checkout := itCommand("./price", "TestTotal", "TestTotalZero"), itCommand("./price", "TestTotalZero"), itCommand("./api", "TestCheckout")
	r.Checks = []model.Check{
		{ID: "check-1", Kind: "test", Status: "FAIL", ExitCode: 1, Command: []string{"go", "test", "./..."}},
		{ID: "check-2", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: both, Output: itEvents("price", "TestTotal", "pass", "TestTotalZero", "pass")},
		{ID: "check-3", Kind: model.CheckImpactedTestCandidate, Status: "FAIL", ExitCode: 1, Command: both, Output: itEvents("price", "TestTotal", "fail", "TestTotalZero", "pass")},
		{ID: "check-4", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: zero, Output: itEvents("price", "TestTotalZero", "pass")},
		{ID: "check-5", Kind: model.CheckImpactedTestCandidate, Status: "PASS", Command: zero, Output: itEvents("price", "TestTotalZero", "pass")},
		{ID: "check-6", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: checkout, Output: itEvents("api", "TestCheckout", "pass")},
		{ID: "check-7", Kind: model.CheckImpactedTestCandidate, Status: "PASS", Command: checkout, Output: itEvents("api", "TestCheckout", "pass")},
	}
	evidence := func(id, name, path, base, candidate, status string) model.Evidence {
		return model.Evidence{ID: id, Kind: model.EvidenceImpactedTestDifferential, Description: "Existing test " + name, Path: path, CheckID: candidate, BaseCheckID: base, Status: status, Runner: "go_test_json", TestNames: []string{name}}
	}
	r.Evidence = []model.Evidence{
		evidence("evidence-1", "TestTotal", "price/price_test.go", "check-2", "check-3", model.StatusFailsOnCandidate),
		evidence("evidence-2", "TestTotalZero", "price/price_test.go", "check-4", "check-5", model.StatusPassesOnCandidate),
		evidence("evidence-3", "TestCheckout", "api/handler_test.go", "check-6", "check-7", model.StatusPassesOnCandidate),
	}
	for i, id := range map[int]string{0: "evidence-1", 1: "evidence-3", 2: "evidence-2"} {
		(*tests)[i].EvidenceID = id
	}
	return r
}

func TestVerifyImpactedTestsReDerivesStatuses(t *testing.T) {
	r := impactedReport()
	got := verifyImpactedTests(r, newLedger(r))
	want := map[string]string{"evidence-1": model.StatusFailsOnCandidate, "evidence-2": model.StatusPassesOnCandidate, "evidence-3": model.StatusPassesOnCandidate}
	if len(got) != len(want) {
		t.Fatalf("verified %v", got)
	}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("verified %v, want %v", got, want)
		}
	}
	// A pass event inside the failed candidate run supports nothing on its
	// own: the same evidence citing check-2/check-3 for TestTotalZero is not
	// verified.
	r.Evidence[1].BaseCheckID, r.Evidence[1].CheckID = "check-2", "check-3"
	if got := verifyImpactedTests(r, newLedger(r)); got["evidence-2"] != "" {
		t.Fatalf("a pass inside a failed run was verified: %v", got)
	}
}

// Every way a record can fail to be supported by its recorded checks.
func TestVerifyImpactedTestsFailsClosed(t *testing.T) {
	replayed := func(liveRuns int) func(r *model.Report) {
		return func(r *model.Report) { r.Checks[1].Cache = cacheRecord(model.CacheHit, liveRuns) }
	}
	cases := []struct {
		name   string
		id     string // the record under test
		mutate func(r *model.Report)
		ok     bool
	}{
		{"unchanged FAILS", "evidence-1", func(*model.Report) {}, true},
		{"jest runner", "evidence-1", func(r *model.Report) { r.Evidence[0].Runner = "jest_json" }, false},
		{"two test names", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"TestTotal", "TestTotalZero"} }, false},
		{"subtest name", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"TestTotal/sub"} }, false},
		{"secret-shaped name", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"TestAKIAABCDEFGHIJKLMNOP"} }, false},
		{"not a test file", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "price/price.go" }, false},
		{"redacted path", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "price/[REDACTED]_test.go" }, false},
		{"another package", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "api/price_test.go" }, false},
		{"base kind", "evidence-1", func(r *model.Report) { r.Checks[1].Kind = model.CheckBaseTestBase }, false},
		{"candidate kind", "evidence-1", func(r *model.Report) { r.Checks[2].Kind = model.CheckBaseTestHybrid }, false},
		{"swapped checks", "evidence-1", func(r *model.Report) { r.Evidence[0].BaseCheckID, r.Evidence[0].CheckID = "check-3", "check-2" }, false},
		{"missing check", "evidence-1", func(r *model.Report) { r.Evidence[0].CheckID = "check-9" }, false},
		{"duplicated check", "evidence-1", func(r *model.Report) { r.Checks = append(r.Checks, r.Checks[2]) }, false},
		{"command mismatch", "evidence-1", func(r *model.Report) { r.Checks[2].Command = itCommand("./price", "TestTotal") }, false},
		{"stored status differs", "evidence-1", func(r *model.Report) { r.Evidence[0].Status = model.StatusPassesOnCandidate }, false},
		{"stored UNVERIFIED", "evidence-1", func(r *model.Report) { r.Evidence[0].Status = model.StatusUnverified }, false},
		{"replayed candidate", "evidence-1", func(r *model.Report) { r.Checks[2].Cache = cacheRecord(model.CacheHit, 2) }, false},
		{"replayed baseline for FAILS", "evidence-1", replayed(2), false},
		{"live baseline written through", "evidence-1", func(r *model.Report) { r.Checks[1].Cache = cacheRecord(model.CacheStored, 1) }, true},
		{"truncated candidate", "evidence-1", func(r *model.Report) { r.Checks[2].Truncated = true }, false},
		{"candidate exit outside the failure range", "evidence-1", func(r *model.Report) { r.Checks[2].ExitCode = 125 }, false},
		{"forged pass after the fail", "evidence-1", func(r *model.Report) {
			r.Checks[2].Output += "{\"Action\":\"pass\",\"Package\":\"example.test/shop/price\",\"Test\":\"TestTotal\"}\n"
		}, false},
		{"baseline failed", "evidence-1", func(r *model.Report) { r.Checks[1].Status, r.Checks[1].ExitCode = "FAIL", 1 }, false},
		{"PASSES on a replayed baseline of two live runs", "evidence-3", func(r *model.Report) { r.Checks[5].Cache = cacheRecord(model.CacheHit, 2) }, true},
		{"PASSES on a replayed baseline of one live run", "evidence-3", func(r *model.Report) { r.Checks[5].Cache = cacheRecord(model.CacheHit, 1) }, false},
		{"PASSES with a skipped test", "evidence-3", func(r *model.Report) { r.Checks[6].Output = itEvents("api", "TestCheckout", "skip") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := impactedReport()
			tc.mutate(r)
			_, ok := verifyImpactedTests(r, newLedger(r))[tc.id]
			if ok != tc.ok {
				t.Fatalf("verified %v, want %v", ok, tc.ok)
			}
		})
	}
	// The verifier handles only its own kind.
	r := impactedReport()
	r.Evidence[0].Kind = model.EvidenceBaseTestDifferential
	if _, ok := verifyImpactedTests(r, newLedger(r))["evidence-1"]; ok {
		t.Fatal("verified another kind")
	}
}

// Through Finalize: statuses come from the verifier, a failing impacted test
// gets a high target at its declaration and requests review (exit 2 with --ci,
// 0 without), and nothing produces exit 1 or a reproduced issue, even with a
// hypothesis that claims one on this evidence.
func TestFinalizeImpactedTests(t *testing.T) {
	for _, ci := range []bool{true, false} {
		r := impactedReport()
		r.Checks[0].Status, r.Checks[0].ExitCode = "PASS", 0
		r.Hypotheses = []model.Hypothesis{{ID: "h1", Title: "Total skips the first item", Severity: "critical", Status: model.StatusReproduced, EvidenceIDs: []string{"evidence-1"}}}
		Finalize(r, ci)
		want := map[bool]int{true: 2, false: 0}[ci]
		if r.ExitCode != want || len(r.ReproducedIssues) != 0 || r.Hypotheses[0].Status != model.StatusUnverified {
			t.Fatalf("ci=%v: exit %d, reproduced %d, hypothesis %s", ci, r.ExitCode, len(r.ReproducedIssues), r.Hypotheses[0].Status)
		}
		tests := r.Impact.ChangedFunctions[0].Tests
		if tests[0].Status != model.StatusFailsOnCandidate || tests[1].Status != model.StatusPassesOnCandidate || tests[2].Status != model.StatusPassesOnCandidate {
			t.Fatalf("statuses %+v", tests)
		}
		high := 0
		for _, target := range r.ReviewTargets {
			if target.Severity == "high" {
				high++
				if target.Path != "price/price_test.go" || target.StartLine != 5 {
					t.Fatalf("target %+v", target)
				}
			}
		}
		if high != 1 {
			t.Fatalf("%d high targets: %+v", high, r.ReviewTargets)
		}
	}
	// Only PASSES_ON_CANDIDATE results request nothing: without TestTotal
	// and its failed candidate run, the exit code stays 0.
	r := impactedReport()
	r.Checks[0].Status, r.Checks[0].ExitCode = "PASS", 0
	r.Checks = append(r.Checks[:1], r.Checks[3:]...)
	r.Evidence = r.Evidence[1:]
	r.Impact.ChangedFunctions[0].Tests = r.Impact.ChangedFunctions[0].Tests[1:]
	Finalize(r, true)
	if r.ExitCode != 0 || len(r.Impact.ChangedFunctions[0].Tests) != 2 || r.Impact.ChangedFunctions[0].Tests[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("passing impacted tests: exit %d (%v), tests %+v", r.ExitCode, r.Unverified, r.Impact.ChangedFunctions[0].Tests)
	}
}

// Write, re-read and re-finalize: byte-identical JSON and Markdown, the
// statuses survive, and a stored FAILS that the checks do not support is
// UNVERIFIED after re-rendering.
func TestImpactedTestsRoundTrip(t *testing.T) {
	r := impactedReport()
	Finalize(r, true)
	dir := t.TempDir()
	if err := Write(dir, r, []string{"markdown", "json"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var again model.Report
	if err := json.Unmarshal(data, &again); err != nil {
		t.Fatal(err)
	}
	Finalize(&again, true)
	second := t.TempDir()
	if err := Write(second, &again, []string{"markdown", "json"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
		a, _ := os.ReadFile(filepath.Join(dir, name))
		b, _ := os.ReadFile(filepath.Join(second, name))
		if string(a) != string(b) {
			t.Fatalf("%s differs after a round trip", name)
		}
	}
	md, _ := os.ReadFile(filepath.Join(dir, "CONFIDENCE_REPORT.md"))
	for _, want := range []string{"test TestTotal at price/price\\_test.go:5 (depth 1, static; FAILS\\_ON\\_CANDIDATE (evidence-1))", "Impacted tests: ran."} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	lower := strings.ToLower(string(md))
	for _, word := range []string{"regression", "bug"} {
		if strings.Contains(lower, word) {
			t.Fatalf("markdown uses %q", word)
		}
	}
	// A saved report whose candidate check is edited to pass no longer
	// supports FAILS_ON_CANDIDATE.
	again.Checks[2].Status, again.Checks[2].ExitCode = "PASS", 0
	Finalize(&again, true)
	if s := again.Impact.ChangedFunctions[0].Tests[0].Status; s != model.StatusUnverified || again.ExitCode != 2 {
		t.Fatalf("edited report: status %s exit %d", s, again.ExitCode)
	}
}
