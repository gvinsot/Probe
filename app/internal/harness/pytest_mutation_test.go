package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/mutation"
)

// TestPythonMutationWithRealPytest runs the mutation stage over a Python
// change with the real pytest: the candidate adds discount, whose test pins
// only the result above the threshold, so mutants of the boundary survive and
// mutants of the arithmetic are killed.
func TestPythonMutationWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	cart := "def total(prices):\n    return sum(prices)\n\n\ndef discount(amount):\n    if amount >= 100:\n        return amount - 10\n    return amount\n"
	test := "from shop.cart import discount\n\n\ndef test_discount_above():\n    assert discount(150) == 140\n\n\ndef test_no_discount_below():\n    assert discount(20) == 20\n"
	files := map[string]string{"shop/__init__.py": "", "shop/cart.py": cart, "tests/test_cart.py": test, "pyproject.toml": "[tool.pytest.ini_options]\npythonpath = [\".\"]\n"}
	h := itHarness(t, files, files, pytestTemplate)
	var argv [][]string
	h.executeCapture = realPytest(t, python, &argv)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var added []model.DiffLine
	for n, line := range strings.Split(cart, "\n") {
		if n >= 4 && line != "" {
			added = append(added, model.DiffLine{Kind: "add", NewLine: n + 1, Content: line})
		}
	}
	change := model.Change{Files: []model.ChangedFile{{Path: "shop/cart.py", Status: "M", Hunks: []model.Hunk{{Lines: added}}}}}
	command := []string{"python3", "-m", "pytest", "-p", "no:cacheprovider", "-q", "tests", "--junitxml={results_out}"}
	res := mutation.Run(context.Background(), w, mutation.Config{Command: command, Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 300}}, change)
	s := res.Section
	if res.Operational || s.Status != model.MutationRan {
		t.Fatalf("section %+v", s)
	}
	outcomes := map[string]string{}
	for _, m := range s.Mutants {
		outcomes[m.Operator+" "+m.Original+"->"+m.Mutated] = m.Status
	}
	want := map[string]string{
		"negate_condition amount >= 100->not (amount >= 100)": model.MutantKilled,
		"boundary >=->>":              model.MutantSurvived, // no test at 100
		"increment_constant 100->101": model.MutantSurvived,
		"swap_arithmetic -->+":        model.MutantKilled,
	}
	for k, status := range want {
		if outcomes[k] != status {
			t.Errorf("%s: %s, want %s (all: %v)", k, outcomes[k], status, outcomes)
		}
	}
	if s.Invalid != 0 || s.Inconclusive != 0 || s.Killed != 2 || s.Survived != 2 {
		t.Fatalf("counts killed %d survived %d invalid %d inconclusive %d", s.Killed, s.Survived, s.Invalid, s.Inconclusive)
	}
	if len(res.Signals) != 2 || !strings.Contains(res.Signals[0].Evidence, "tests passed in the run for source file shop/cart.py") || res.Signals[0].Summary != "With a single-change mutant of this added line, no test that the mutation command ran failed" {
		t.Fatalf("signals %+v", res.Signals)
	}
	for _, c := range h.MutationChecks() {
		if c.Results == "" || !strings.Contains(c.Results, `"format":"pytest_junit"`) {
			t.Fatalf("check %s recorded no pytest report: %+v", c.ID, c)
		}
	}
	if len(argv) != 5 || !strings.Contains(strings.Join(argv[0], " "), "-q tests --junitxml="+ResultsPath) {
		t.Fatalf("a control and four mutant runs expected: %q", argv)
	}
}
