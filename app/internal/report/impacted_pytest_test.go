package report

import (
	"encoding/json"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Tests of the impacted-test and base-test verifiers for Python tests run by
// a pytest template (runner pytest_junit): the statuses come from the
// recorded, normalized JUnit reports of the two checks.

// pytestResults renders the normalized pytest report of one module: name,
// status pairs ("passed", "failed", "error" or "skipped"), with the classes
// of a "Class::name" in the classname.
func pytestResults(module string, pairs ...string) string {
	cases := []map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		classname, name := module, pairs[i]
		for j := len(name) - 1; j > 0; j-- {
			if name[j] == ':' && name[j-1] == ':' {
				classname, name = module+"."+name[:j-1], name[j+1:]
				break
			}
		}
		cases = append(cases, map[string]any{"classname": classname, "name": name, "status": pairs[i+1]})
	}
	b, _ := json.Marshal(map[string]any{"format": "pytest_junit", "testcases": cases})
	return string(b)
}

// pytestImpactedReport records one Python test that fails on the candidate
// and one class test that passes on both, each with its own pair of runs.
func pytestImpactedReport() *model.Report {
	const file = "tests/test_cart.py"
	r := impactReport()
	r.Impact.TestsStatus = model.ImpactTestsRan
	r.Impact.Languages = []string{"python"}
	fn := &r.Impact.ChangedFunctions[0]
	fn.Path, fn.Symbol = "shop/cart.py", "shop.total"
	fn.Tests = []model.ImpactTest{
		{Name: "test_total", Path: file, Line: 6, Package: "tests", Depth: 1, Resolution: model.ResolutionName, EvidenceID: "evidence-1"},
		{Name: "TestCart::test_empty", Path: file, Line: 16, Package: "tests", Depth: 1, Resolution: model.ResolutionName, EvidenceID: "evidence-2"},
	}
	fn.TestsTotal = 2
	command := func(name string) []string {
		return []string{"python", "-m", "pytest", "-p", "no:cacheprovider", file + "::" + name, "--junitxml=/tmp/probe-test-results.json"}
	}
	total, empty := command("test_total"), command("TestCart::test_empty")
	r.Checks = []model.Check{
		{ID: "check-1", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: total, Results: pytestResults("tests.test_cart", "test_total", "passed")},
		{ID: "check-2", Kind: model.CheckImpactedTestCandidate, Status: "FAIL", ExitCode: 1, Command: total, Results: pytestResults("tests.test_cart", "test_total", "failed")},
		{ID: "check-3", Kind: model.CheckImpactedTestBase, Status: "PASS", Command: empty, Results: pytestResults("test_cart", "TestCart::test_empty", "passed")},
		{ID: "check-4", Kind: model.CheckImpactedTestCandidate, Status: "PASS", Command: empty, Results: pytestResults("test_cart", "TestCart::test_empty", "passed")},
	}
	evidence := func(id, name, base, candidate, status string) model.Evidence {
		return model.Evidence{ID: id, Kind: model.EvidenceImpactedTestDifferential, Description: "Existing test " + name, Path: file, CheckID: candidate, BaseCheckID: base, Status: status, Runner: "pytest_junit", TestNames: []string{name}}
	}
	r.Evidence = []model.Evidence{
		evidence("evidence-1", "test_total", "check-1", "check-2", model.StatusFailsOnCandidate),
		evidence("evidence-2", "TestCart::test_empty", "check-3", "check-4", model.StatusPassesOnCandidate),
	}
	return r
}

func TestVerifyImpactedPytestTests(t *testing.T) {
	r := pytestImpactedReport()
	got := verifyImpactedTests(r, newLedger(r))
	if got["evidence-1"] != model.StatusFailsOnCandidate || got["evidence-2"] != model.StatusPassesOnCandidate || len(got) != 2 {
		t.Fatalf("verified %v", got)
	}
	cases := []struct {
		name   string
		id     string
		mutate func(r *model.Report)
	}{
		{"jest runner", "evidence-1", func(r *model.Report) { r.Evidence[0].Runner = "jest_json" }},
		{"go runner", "evidence-1", func(r *model.Report) { r.Evidence[0].Runner = "go_test_json" }},
		{"not a test module", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "tests/helpers.py" }},
		{"another module", "evidence-1", func(r *model.Report) { r.Evidence[0].Path = "tests/test_other.py" }},
		{"another test", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"TestCart::test_empty"} }},
		{"an option as name", "evidence-1", func(r *model.Report) { r.Evidence[0].TestNames = []string{"-x"} }},
		{"report without the test", "evidence-1", func(r *model.Report) { r.Checks[1].Results = pytestResults("tests.test_cart", "test_other", "failed") }},
		{"report of another module", "evidence-1", func(r *model.Report) { r.Checks[1].Results = pytestResults("tests.test_other", "test_total", "failed") }},
		{"skipped on the baseline", "evidence-1", func(r *model.Report) { r.Checks[0].Results = pytestResults("tests.test_cart", "test_total", "skipped") }},
		{"a jest report", "evidence-1", func(r *model.Report) { r.Checks[1].Results = jestResults("tests/test_cart.py", "test_total", "failed") }},
		{"duplicated result", "evidence-1", func(r *model.Report) {
			r.Checks[1].Results = pytestResults("tests.test_cart", "test_total", "failed", "test_total", "passed")
		}},
		{"replayed baseline for FAILS", "evidence-1", func(r *model.Report) { r.Checks[0].Cache = cacheRecord(model.CacheHit, 2) }},
		{"pass inside a failed run", "evidence-2", func(r *model.Report) { r.Checks[3].Status, r.Checks[3].ExitCode = "FAIL", 1 }},
		{"command selects the module only", "evidence-2", func(r *model.Report) {
			r.Checks[2].Command[5], r.Checks[3].Command[5] = "tests/test_cart.py", "tests/test_cart.py"
		}},
		{"command narrowed by pytest's -m", "evidence-1", func(r *model.Report) {
			r.Checks[0].Command = append(r.Checks[0].Command, "-m", "slow")
			r.Checks[1].Command = append(r.Checks[1].Command, "-m", "slow")
		}},
		{"command narrowed by -k", "evidence-1", func(r *model.Report) {
			r.Checks[0].Command = append(r.Checks[0].Command, "-k", "nothing")
			r.Checks[1].Command = append(r.Checks[1].Command, "-k", "nothing")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pytestImpactedReport()
			tc.mutate(r)
			if _, ok := verifyImpactedTests(r, newLedger(r))[tc.id]; ok {
				t.Fatal("verified")
			}
		})
	}
	r = pytestImpactedReport()
	Finalize(r, true)
	if r.ExitCode != 2 || len(r.ReproducedIssues) != 0 {
		t.Fatalf("exit %d, reproduced %d", r.ExitCode, len(r.ReproducedIssues))
	}
	tests := r.Impact.ChangedFunctions[0].Tests
	if tests[0].Status != model.StatusFailsOnCandidate || tests[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("finalized tests %+v", tests)
	}
}

// A Python base test: the baseline version of test_total passes on the
// baseline tree and fails on the hybrid tree.
func TestVerifyBaseTestsPytest(t *testing.T) {
	const file = "tests/test_cart.py"
	command := []string{"pytest", file + "::test_total", "--junitxml=/tmp/probe-test-results.json"}
	build := func() *model.Report {
		return &model.Report{
			Checks: []model.Check{
				{ID: "check-1", Kind: model.CheckBaseTestBase, Status: "PASS", Command: append([]string(nil), command...), Results: pytestResults("tests.test_cart", "test_total", "passed")},
				{ID: "check-2", Kind: model.CheckBaseTestHybrid, Status: "FAIL", ExitCode: 1, Command: append([]string(nil), command...), Results: pytestResults("tests.test_cart", "test_total", "failed")},
			},
			Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceBaseTestDifferential, Description: "Baseline version", Path: file, CheckID: "check-2", BaseCheckID: "check-1", Status: model.StatusFailsOnCandidate, Runner: "pytest_junit", TestNames: []string{"test_total"}}},
		}
	}
	r := build()
	if got := verifyBaseTests(r, newLedger(r)); got["evidence-1"] != model.StatusFailsOnCandidate {
		t.Fatalf("verified %v", got)
	}
	for name, mutate := range map[string]func(r *model.Report){
		"jest runner":     func(r *model.Report) { r.Evidence[0].Runner = "jest_json" },
		"not a test file": func(r *model.Report) { r.Evidence[0].Path = "shop/cart.py" },
		"another test":    func(r *model.Report) { r.Evidence[0].TestNames = []string{"test_other"} },
		"hybrid passed":   func(r *model.Report) { r.Checks[1].Results = pytestResults("tests.test_cart", "test_total", "passed") },
		"module target":   func(r *model.Report) { r.Checks[0].Command[1], r.Checks[1].Command[1] = file, file },
		"hybrid collection error": func(r *model.Report) {
			r.Checks[1].Results = `{"format":"pytest_junit","testcases":[{"classname":"","name":"tests.test_cart","status":"error"}]}`
		},
	} {
		r := build()
		mutate(r)
		if _, ok := verifyBaseTests(r, newLedger(r))["evidence-1"]; ok {
			t.Errorf("%s: verified", name)
		}
	}
}
