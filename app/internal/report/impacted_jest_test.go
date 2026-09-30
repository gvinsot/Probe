package report

import (
	"encoding/json"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Tests of the impacted-test verifier for TS/JS tests run by a Vitest or Jest
// template (runner jest_json): the statuses come from the recorded JSON
// reports of the two checks.

// jestResults renders the normalized report of one file: title, status pairs
// of tests inside describe("price").
func jestResults(file string, pairs ...string) string {
	var assertions []map[string]any
	for i := 0; i+1 < len(pairs); i += 2 {
		assertions = append(assertions, map[string]any{"ancestorTitles": []string{"price"}, "title": pairs[i], "status": pairs[i+1]})
	}
	b, _ := json.Marshal(map[string]any{"testResults": []map[string]any{{"name": "/workspace/" + file, "assertionResults": assertions}}})
	return string(b)
}

// jestImpactedReport records one TS test that fails on the candidate and one
// that passes on both, each with its own pair of Vitest runs.
func jestImpactedReport() *model.Report {
	const file = "web/price.test.ts"
	r := impactReport()
	r.Impact.TestsStatus = model.ImpactTestsRan
	r.Impact.Languages = []string{"typescript"}
	fn := &r.Impact.ChangedFunctions[0]
	fn.Path, fn.Symbol = "web/price.ts", "web.price"
	fn.Tests = []model.ImpactTest{
		{Name: "price > discounts", Path: file, Line: 4, Package: "web", Depth: 1, Resolution: model.ResolutionName, EvidenceID: "evidence-1"},
		{Name: "price > rounds", Path: file, Line: 7, Package: "web", Depth: 1, Resolution: model.ResolutionName, EvidenceID: "evidence-2"},
	}
	fn.TestsTotal = 2
	command := func(filter string) []string {
		return []string{"vitest", "run", file, "--reporter=json", "--outputFile=/tmp/probe-test-results.json", "-t", filter}
	}
	discounts, rounds := command(`^(?:price\s+discounts)$`), command(`^(?:price\s+rounds)$`)
	r.Checks = []model.Check{
		{ID: "check-1", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: discounts, Results: jestResults(file, "discounts", "passed", "rounds", "skipped")},
		{ID: "check-2", Kind: model.CheckImpactedTestCandidate, Status: "FAIL", ExitCode: 1, Command: discounts, Results: jestResults(file, "discounts", "failed", "rounds", "skipped")},
		{ID: "check-3", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: rounds, Results: jestResults(file, "discounts", "skipped", "rounds", "passed")},
		{ID: "check-4", Kind: model.CheckImpactedTestCandidate, Status: "PASS", Command: rounds, Results: jestResults(file, "discounts", "skipped", "rounds", "passed")},
	}
	evidence := func(id, name, base, candidate, status string) model.Evidence {
		return model.Evidence{ID: id, Kind: model.EvidenceImpactedTestDifferential, Description: "Existing test " + name, Path: file, CheckID: candidate, BaseCheckID: base, Status: status, Runner: "jest_json", TestNames: []string{name}}
	}
	r.Evidence = []model.Evidence{
		evidence("evidence-1", "price > discounts", "check-1", "check-2", model.StatusFailsOnCandidate),
		evidence("evidence-2", "price > rounds", "check-3", "check-4", model.StatusPassesOnCandidate),
	}
	return r
}

func TestVerifyImpactedJestTests(t *testing.T) {
	r := jestImpactedReport()
	got := verifyImpactedTests(r, newLedger(r))
	if got["evidence-1"] != model.StatusFailsOnCandidate || got["evidence-2"] != model.StatusPassesOnCandidate || len(got) != 2 {
		t.Fatalf("verified %v", got)
	}
	cases := []struct {
		name   string
		id     string
		mutate func(r *model.Report)
	}{
		{"go runner", "evidence-1", func(r *model.Report) { r.Evidence[0].Runner = "go_test_json" }},
		{"not a test file", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "web/price.ts" }},
		{"another file", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "web/other.test.ts" }},
		{"another test", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"price > rounds"} }},
		{"empty title", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"price > "} }},
		{"report without the test", "evidence-1", func(r *model.Report) { r.Checks[1].Results = jestResults("web/price.test.ts", "rounds", "skipped") }},
		{"report of another file", "evidence-1", func(r *model.Report) { r.Checks[1].Results = jestResults("web/other.test.ts", "discounts", "failed") }},
		{"test skipped on the baseline", "evidence-1", func(r *model.Report) { r.Checks[0].Results = jestResults("web/price.test.ts", "discounts", "pending") }},
		{"duplicated result", "evidence-1", func(r *model.Report) {
			r.Checks[1].Results = jestResults("web/price.test.ts", "discounts", "failed", "discounts", "passed")
		}},
		{"replayed baseline for FAILS", "evidence-1", func(r *model.Report) { r.Checks[0].Cache = cacheRecord(model.CacheHit, 2) }},
		{"pass inside a failed run", "evidence-2", func(r *model.Report) { r.Checks[3].Status, r.Checks[3].ExitCode = "FAIL", 1 }},
		{"command without the file", "evidence-2", func(r *model.Report) {
			r.Checks[2].Command[2], r.Checks[3].Command[2] = "web", "web"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := jestImpactedReport()
			tc.mutate(r)
			if _, ok := verifyImpactedTests(r, newLedger(r))[tc.id]; ok {
				t.Fatal("verified")
			}
		})
	}
	// Through Finalize: the failing TS test requests review and gets a high
	// target at its declaration; nothing produces exit 1.
	r = jestImpactedReport()
	Finalize(r, true)
	if r.ExitCode != 2 || len(r.ReproducedIssues) != 0 {
		t.Fatalf("exit %d, reproduced %d", r.ExitCode, len(r.ReproducedIssues))
	}
	tests := r.Impact.ChangedFunctions[0].Tests
	if tests[0].Status != model.StatusFailsOnCandidate || tests[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("finalized tests %+v", tests)
	}
	found := false
	for _, target := range r.ReviewTargets {
		found = found || target.Path == "web/price.test.ts" && target.StartLine == 4 && target.Severity == "high"
	}
	if !found {
		t.Fatalf("no high target at the failing test: %+v", r.ReviewTargets)
	}
}

// A TS base test: the baseline version of "price > discounts" passes on the
// baseline tree and fails on the hybrid tree.
func TestVerifyBaseTestsJest(t *testing.T) {
	const file = "web/price.test.ts"
	command := []string{"vitest", "run", file, "--reporter=json", "--outputFile=/tmp/probe-test-results.json", "-t", `^(?:price\s+discounts)$`}
	build := func() *model.Report {
		return &model.Report{
			Checks: []model.Check{
				{ID: "check-1", Kind: model.CheckBaseTestBase, Status: "PASS", Command: command, Results: jestResults(file, "discounts", "passed")},
				{ID: "check-2", Kind: model.CheckBaseTestHybrid, Status: "FAIL", ExitCode: 1, Command: command, Results: jestResults(file, "discounts", "failed")},
			},
			Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceBaseTestDifferential, Description: "Baseline version", Path: file, CheckID: "check-2", BaseCheckID: "check-1", Status: model.StatusFailsOnCandidate, Runner: "jest_json", TestNames: []string{"price > discounts"}}},
		}
	}
	r := build()
	if got := verifyBaseTests(r, newLedger(r)); got["evidence-1"] != model.StatusFailsOnCandidate {
		t.Fatalf("verified %v", got)
	}
	for name, mutate := range map[string]func(r *model.Report){
		"go runner":       func(r *model.Report) { r.Evidence[0].Runner = "go_test_json" },
		"not a test file": func(r *model.Report) { r.Evidence[0].Path = "web/price.ts" },
		"another test":    func(r *model.Report) { r.Evidence[0].TestNames = []string{"price > rounds"} },
		"hybrid passed":   func(r *model.Report) { r.Checks[1].Results = jestResults(file, "discounts", "passed") },
		"package target":  func(r *model.Report) { r.Checks[0].Command[2], r.Checks[1].Command[2] = "./web", "./web" },
	} {
		r := build()
		r.Checks[0].Command = append([]string(nil), command...)
		r.Checks[1].Command = append([]string(nil), command...)
		mutate(r)
		if _, ok := verifyBaseTests(r, newLedger(r))["evidence-1"]; ok {
			t.Errorf("%s: verified", name)
		}
	}
}
