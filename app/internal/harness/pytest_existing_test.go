package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

func TestPytestTestOutcome(t *testing.T) {
	const p = "tests/test_cart.py"
	report := func(cases ...[3]string) string { return normalizedJUnit(t, junit(cases...)) }
	for _, tc := range []struct {
		name, results, test, want string
	}{
		{"passed", report([3]string{"tests.test_cart", "test_total", "passed"}), "test_total", "pass"},
		{"failed", report([3]string{"test_cart", "test_total", "failed"}), "test_total", "fail"},
		{"setup error", report([3]string{"tests.test_cart", "test_total", "error"}), "test_total", "fail"},
		{"skipped", report([3]string{"tests.test_cart", "test_total", "skipped"}), "test_total", "skip"},
		{"class test", report([3]string{"tests.test_cart.TestCart", "test_empty", "passed"}), "TestCart::test_empty", "pass"},
		{"not the class test", report([3]string{"tests.test_cart.TestCart", "test_empty", "passed"}), "test_empty", ""},
		{"variants passed", report([3]string{"tests.test_cart", "test_d[0]", "passed"}, [3]string{"tests.test_cart", "test_d[0.5]", "passed"}), "test_d", "pass"},
		{"a variant failed", report([3]string{"tests.test_cart", "test_d[0]", "passed"}, [3]string{"tests.test_cart", "test_d[0.5]", "failed"}), "test_d", "fail"},
		{"variants skipped", report([3]string{"tests.test_cart", "test_d[0]", "skipped"}, [3]string{"tests.test_cart", "test_d[1]", "skipped"}), "test_d", "skip"},
		{"a variant skipped", report([3]string{"tests.test_cart", "test_d[0]", "passed"}, [3]string{"tests.test_cart", "test_d[1]", "skipped"}), "test_d", ""},
		{"a variant failed, one skipped", report([3]string{"tests.test_cart", "test_d[0]", "failed"}, [3]string{"tests.test_cart", "test_d[1]", "skipped"}), "test_d", "fail"},
		{"a longer name", report([3]string{"tests.test_cart", "test_d_more", "passed"}), "test_d", ""},
		{"twice", report([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"test_cart", "test_total", "passed"}), "test_total", ""},
		{"another module", report([3]string{"tests.test_other", "test_total", "passed"}), "test_total", ""},
		{"absent", report([3]string{"tests.test_cart", "test_other", "passed"}), "test_total", ""},
		{"jest report", jestResults(p, map[string]string{"test_total": "passed"}), "test_total", ""},
	} {
		if got := PytestTestOutcome(tc.results, p, tc.test); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClassifyExistingPytestTest(t *testing.T) {
	const p = "tests/test_cart.py"
	command := []string{"pytest", p + "::test_total", "--junitxml=" + ResultsPath}
	pass := normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "passed"}))
	fail := normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "failed"}))
	broken := normalizedJUnit(t, `<testsuites><testsuite><testcase classname="" name="tests.test_cart"><error message="collection failure"/></testcase></testsuite></testsuites>`)
	base := model.Check{Status: "PASS", Command: command, Results: pass}
	for _, tc := range []struct {
		name      string
		candidate model.Check
		status    string
	}{
		{"fails", model.Check{Status: "FAIL", ExitCode: 1, Command: command, Results: fail}, model.StatusFailsOnCandidate},
		{"passes", model.Check{Status: "PASS", Command: command, Results: pass}, model.StatusPassesOnCandidate},
		{"collection error", model.Check{Status: "FAIL", ExitCode: 2, Command: command, Results: broken}, model.StatusUnverified},
		{"interrupted", model.Check{Status: "FAIL", ExitCode: 2, Command: command, Results: fail}, model.StatusFailsOnCandidate},
		{"crash exit", model.Check{Status: "FAIL", ExitCode: 137, Command: command, Results: fail}, model.StatusUnverified},
		{"other command", model.Check{Status: "FAIL", ExitCode: 1, Command: append([]string{"python3"}, command...), Results: fail}, model.StatusUnverified},
	} {
		status, reason := ClassifyExistingPytestTest(base, tc.candidate, p, "test_total")
		if status != tc.status {
			t.Errorf("%s: %s (%s), want %s", tc.name, status, reason, tc.status)
		}
		if tc.name == "collection error" && reason != reasonJestCandidateSuite {
			t.Errorf("collection error reason %q", reason)
		}
	}
}

func TestExistingRunnerForPytest(t *testing.T) {
	runner := existingRunnerFor(pytestTemplate)
	if runner != RunnerPytest || !runner.script() || runner.jest() || runner.evidenceRunner() != RunnerPytest {
		t.Fatalf("runner %q", runner)
	}
	if ok, _ := runner.validName("TestCart::test_empty", "tests/test_cart.py"); !ok {
		t.Fatal("a pytest class test was refused")
	}
	for _, tc := range [][2]string{{"test_x", "tests/helpers.py"}, {"price > discounts", "src/price.test.ts"}, {"TestA", "pkg/a_test.go"}, {"-x", "tests/test_cart.py"}} {
		if ok, reason := runner.validName(tc[0], tc[1]); ok || !strings.Contains(reason, "pytest") {
			t.Errorf("%q in %s accepted (%s)", tc[0], tc[1], reason)
		}
	}
}

// pyImpactedTrees: shop/cart.py changed (the discount applies twice), the
// test module unchanged.
func pyImpactedTrees() (base, candidate map[string]string) {
	test := `import pytest

from shop.cart import total


def test_total():
    assert total([10], 0.5) == 5


@pytest.mark.parametrize("discount", [0, 0.5])
def test_discount(discount):
    assert total([10], discount) == 10 * (1 - discount)


class TestCart:
    def test_empty(self):
        assert total([], 0.5) == 0
`
	common := map[string]string{"shop/__init__.py": "", "tests/test_cart.py": test, "pyproject.toml": "[tool.pytest.ini_options]\npythonpath = [\".\"]\n"}
	base, candidate = map[string]string{}, map[string]string{}
	for p, c := range common {
		base[p], candidate[p] = c, c
	}
	base["shop/cart.py"] = "def total(prices, discount):\n    return sum(prices) * (1 - discount)\n"
	candidate["shop/cart.py"] = "def total(prices, discount):\n    return sum(prices) * (1 - discount) * (1 - discount)\n"
	return base, candidate
}

func pySelected() []model.ImpactTest {
	return []model.ImpactTest{
		{Name: "test_total", Path: "tests/test_cart.py", Line: 6, Package: "tests", Depth: 1, Resolution: model.ResolutionName},
		{Name: "test_discount", Path: "tests/test_cart.py", Line: 10, Package: "tests", Depth: 1, Resolution: model.ResolutionName},
		{Name: "TestCart::test_empty", Path: "tests/test_cart.py", Line: 16, Package: "tests", Depth: 1, Resolution: model.ResolutionName},
	}
}

// TestRunImpactedTestsWithRealPytest runs three unchanged tests that reach
// the changed function with the real pytest: two fail on the candidate (one
// of them through one parametrized variant), and the class test, which passed
// inside the failed run, gets a run pair of its own and passes.
func TestRunImpactedTestsWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	base, candidate := pyImpactedTrees()
	h := itHarness(t, base, candidate, pytestTemplate)
	var argv [][]string
	h.executeCapture = realPytest(t, python, &argv)
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a pytest impacted run went without the payload channel")
		return execution{}
	}
	res := h.RunImpactedTests(context.Background(), pySelected())
	if res.Status != model.ImpactTestsRan || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	want := []string{model.StatusFailsOnCandidate, model.StatusFailsOnCandidate, model.StatusPassesOnCandidate}
	for i, test := range res.Tests {
		if test.Status != want[i] {
			t.Fatalf("test %s: %s (%s), want %s", test.Name, test.Status, test.Reason, want[i])
		}
		e := itEvidence(t, h, test.EvidenceID)
		if e.Runner != RunnerPytest || e.Path != "tests/test_cart.py" || !reflect.DeepEqual(e.TestNames, []string{test.Name}) || !strings.Contains(e.Description, "test file tests/test_cart.py") {
			t.Fatalf("evidence %+v", e)
		}
	}
	if len(argv) != 4 {
		t.Fatalf("%d runs, want a pair for the three tests and a pair for the class test alone: %q", len(argv), argv)
	}
	first := strings.Join(argv[0], " ")
	if !strings.Contains(first, "tests/test_cart.py::TestCart::test_empty tests/test_cart.py::test_discount tests/test_cart.py::test_total --junitxml="+ResultsPath) {
		t.Fatalf("first command %s", first)
	}
	if last := strings.Join(argv[3], " "); !strings.Contains(last, "tests/test_cart.py::TestCart::test_empty --junitxml") || strings.Contains(last, "::test_total") {
		t.Fatalf("retry command %s", last)
	}
}

// TestRunBaseTestsWithRealPytest runs the baseline version of a test the
// change weakened on the baseline tree and on the candidate tree with the
// test module reverted, with the real pytest.
func TestRunBaseTestsWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	base, candidate := pyImpactedTrees()
	// The change also loosens the test so that it still passes.
	candidate["tests/test_cart.py"] = strings.Replace(candidate["tests/test_cart.py"], "assert total([10], 0.5) == 5", "assert total([10], 0.5) <= 5", 1)
	h := itHarness(t, base, candidate, pytestTemplate)
	var argv [][]string
	var hybridTest string
	run := realPytest(t, python, &argv)
	h.executeCapture = func(ctx context.Context, name string, args []string, log, payload io.Writer) execution {
		if sideOf(h, args) == "unknown" {
			b, _ := os.ReadFile(filepath.Join(itMountDir(args), "tests", "test_cart.py"))
			hybridTest = string(b)
		}
		return run(ctx, name, args, log, payload)
	}
	selected := []model.BaseTest{{Name: "test_total", Path: "tests/test_cart.py", Line: 6, EndLine: 7, Change: model.BaseTestModified, Status: model.StatusUnverified}}
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil || res.Status != model.BaseTestsRan || res.Tests[0].Status != model.StatusFailsOnCandidate {
		t.Fatalf("result %+v, %v", res, err)
	}
	if hybridTest != base["tests/test_cart.py"] {
		t.Fatalf("the hybrid tree held %q, not the baseline test module", hybridTest)
	}
	e := itEvidence(t, h, res.Tests[0].EvidenceID)
	if e.Runner != RunnerPytest || e.Kind != model.EvidenceBaseTestDifferential || !reflect.DeepEqual(e.TestNames, []string{"test_total"}) || !strings.Contains(e.Description, "test module reverted to the baseline") {
		t.Fatalf("evidence %+v", e)
	}
	if len(argv) != 2 || !contains(argv[0], "tests/test_cart.py::test_total") {
		t.Fatalf("argv %q", argv)
	}
}

// A pytest test with a Vitest template, a Go template or an unverifiable
// pytest template is not run.
func TestRunImpactedTestsPytestRunnerMismatch(t *testing.T) {
	base, candidate := pyImpactedTrees()
	h := itHarness(t, base, candidate, jsImpactedTemplate)
	res := h.RunImpactedTests(context.Background(), pySelected())
	if !strings.Contains(res.Tests[0].Reason, "not a TypeScript or JavaScript test") {
		t.Fatalf("Vitest template: %+v", res.Tests[0])
	}
	h = itHarness(t, base, candidate, []string{"pytest", "{file}"})
	res = h.RunImpactedTests(context.Background(), pySelected())
	if res.Status != model.ImpactTestsNotRun || res.Reason != impactedTemplateReason {
		t.Fatalf("pytest without a report: %+v", res)
	}
	h = itHarness(t, base, candidate, pytestTemplate)
	res = h.RunImpactedTests(context.Background(), []model.ImpactTest{{Name: "test_x", Path: "tests/helpers.py", Depth: 1}})
	if !strings.Contains(res.Tests[0].Reason, "not a pytest test of a Python test module") {
		t.Fatalf("helper module: %+v", res.Tests[0])
	}
}
