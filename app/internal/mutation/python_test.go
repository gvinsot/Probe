package mutation

import (
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const pyCart = `"""Cart helpers."""
LIMIT = 100 + 1


def total(prices, discount=-1, *args, **kw):
    if discount > 0 and prices:
        return sum(prices) * (1 - discount)
    elif (n := len(prices)) == 0:
        return 0
    while discount < 10:
        discount = discount + 1
    label = "a < b" if prices else "x == y"  # a < b in a comment
    evens = [p for p in prices if p % 2 == 0]
    key = sorted(prices, key=lambda p: -p)
    return sum(prices) // 2 + 2 ** 3


def is_free(amount):
    if amount >= 50 or amount == 0:
        return True
    return False


def ratio(a, b):
    return a / b if b != 0 else None
`

func pySites(t *testing.T, added ...int) []Site {
	t.Helper()
	set := map[int]bool{}
	if len(added) == 0 {
		for i := 1; i <= strings.Count(pyCart, "\n"); i++ {
			set[i] = true
		}
	} else {
		for _, l := range added {
			set[l] = true
		}
	}
	sites, capped, skip := PythonFileSites("shop/cart.py", []byte(pyCart), set)
	if capped || skip != "" {
		t.Fatalf("capped %v skip %q", capped, skip)
	}
	return sites
}

func TestPythonFileSites(t *testing.T) {
	var got []string
	for _, s := range pySites(t) {
		got = append(got, s.Operator+" "+s.Original+" -> "+s.Replacement+" @"+strconv.Itoa(s.Line))
	}
	sort.Strings(got)
	want := []string{
		"boundary < -> <= @10",
		"boundary > -> >= @6",
		"boundary >= -> > @19",
		"flip_boolean False -> True @21",
		"flip_boolean True -> False @20",
		"increment_constant 0 -> 1 @13", // the comparison inside a comprehension
		"increment_constant 0 -> 1 @19",
		"increment_constant 0 -> 1 @25", // inside a conditional expression
		"increment_constant 0 -> 1 @6",
		"increment_constant 0 -> 1 @8",
		"increment_constant 10 -> 11 @10",
		"increment_constant 2 -> 3 @13",
		"increment_constant 50 -> 51 @19",
		"negate_comparison != -> == @25",
		"negate_comparison == -> != @13",
		"negate_comparison == -> != @19",
		"negate_comparison == -> != @8",
		"negate_condition (n := len(prices)) == 0 -> not ((n := len(prices)) == 0) @8",
		"negate_condition amount >= 50 or amount == 0 -> not (amount >= 50 or amount == 0) @19",
		"negate_condition discount < 10 -> not (discount < 10) @10",
		"negate_condition discount > 0 and prices -> not (discount > 0 and prices) @6",
		"swap_arithmetic * -> / @7",
		"swap_arithmetic + -> - @11",
		"swap_arithmetic + -> - @15",
		"swap_arithmetic - -> + @7",
		"swap_arithmetic / -> * @25",
		"swap_logical and -> or @6",
		"swap_logical or -> and @19",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sites:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Only sites whose whole span lies on added lines.
	for _, s := range pySites(t, 19) {
		if s.Line != 19 || s.EndLine != 19 {
			t.Fatalf("site outside the added line: %+v", s)
		}
	}
	if len(pySites(t, 2)) != 0 {
		t.Fatal("module-level code was mutated")
	}
}

// TestPythonMutantsParse applies every site and asks the real Python whether
// each mutant still parses, and that it differs from the original.
func TestPythonMutantsParse(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on the host")
	}
	for _, s := range pySites(t) {
		mutant, err := s.Apply([]byte(pyCart))
		if err != nil {
			t.Fatalf("%s at %d: %v", s.Operator, s.Line, err)
		}
		if string(mutant) == pyCart {
			t.Fatalf("%s at %d changed nothing", s.Operator, s.Line)
		}
		cmd := exec.Command(python, "-c", "import ast, sys; ast.parse(sys.stdin.read())")
		cmd.Stdin = strings.NewReader(string(mutant))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s %q -> %q at line %d does not parse: %s", s.Operator, s.Original, s.Replacement, s.Line, out)
		}
	}
}

func TestPythonApplyRefusesMerges(t *testing.T) {
	src := []byte("def f(x):\n    if(x):\n        return 1\n")
	sites, _, _ := PythonFileSites("a.py", src, map[int]bool{2: true, 3: true})
	for _, s := range sites {
		if s.Operator == OpNegateCondition {
			if _, err := s.Apply(src); err == nil {
				t.Fatal("if(x) became ifnot (x)")
			}
		}
	}
	s := Site{Path: "a.py", Start: 4, End: 5, Original: "1", Replacement: "2"}
	if _, err := s.Apply([]byte("abc 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply([]byte("abc 9\n")); err == nil {
		t.Fatal("a stale span was applied")
	}
}

func TestPythonModeAndScope(t *testing.T) {
	command := []string{"python", "-m", "pytest", "-x", "tests", "--junitxml={results_out}"}
	if CommandMode(command) != ModePython || CommandMode([]string{"vitest", "related", "{file}"}) != ModeScript || CommandMode([]string{"go", "test", "-json", "{package}"}) != ModeGo {
		t.Fatal("CommandMode")
	}
	if got := ExpandCommand(command, PackageArg("shop/cart.py")); strings.Join(got, " ") != "python -m pytest -x tests --junitxml="+ResultsPath {
		t.Fatalf("expanded %q", got)
	}
	if PackageArg("shop/cart.py") != "shop/cart.py" {
		t.Fatal("a Python unit is its source file")
	}
	for p, want := range map[string]bool{"shop/cart.py": true, "tests/test_cart.py": false, "shop/conftest.py": false, "shop/cart.ts": false} {
		if inScope(ModePython, model.ChangedFile{Path: p, Status: "M"}) != want {
			t.Errorf("inScope(python, %s) != %v", p, want)
		}
	}
	terms := TermsFor(command)
	if terms.Invalid != "did not import (a collection error)" || !strings.Contains(model.MutationNoteFor(command), "JUnit XML report") {
		t.Fatalf("terms %+v", terms)
	}
}

// pyReport renders a normalized pytest report.
func pyReport(cases ...[3]string) string {
	var b strings.Builder
	b.WriteString(`{"format":"pytest_junit","testcases":[`)
	for i, c := range cases {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"classname":"` + c[0] + `","name":"` + c[1] + `","status":"` + c[2] + `"}`)
	}
	b.WriteString("]}")
	return b.String()
}

func TestClassifyPythonMutants(t *testing.T) {
	command := []string{"python", "-m", "pytest", "tests", "--junitxml=" + ResultsPath}
	control := model.Check{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS", Command: command,
		Results: pyReport([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"tests.test_cart", "test_skip", "skipped"})}
	run := func(status string, code int, results string) model.Check {
		return model.Check{ID: "mutation-check-2", Kind: model.CheckMutant, Status: status, ExitCode: code, Command: command, Results: results}
	}
	for _, tc := range []struct {
		name string
		run  model.Check
		want string
	}{
		{"killed", run("FAIL", 1, pyReport([3]string{"tests.test_cart", "test_total", "failed"})), model.MutantKilled},
		{"killed by a fixture", run("FAIL", 1, pyReport([3]string{"tests.test_cart", "test_total", "error"})), model.MutantKilled},
		{"survived", run("PASS", 0, pyReport([3]string{"tests.test_cart", "test_total", "passed"})), model.MutantSurvived},
		{"does not import", run("FAIL", 2, pyReport([3]string{"", "tests.test_cart", "error"})), model.MutantInvalid},
		{"no report", run("FAIL", 1, ""), model.MutantInconclusive},
		{"failed without a failing test", run("FAIL", 1, pyReport([3]string{"tests.test_cart", "test_total", "passed"})), model.MutantInconclusive},
		{"passed without a passing test", run("PASS", 0, pyReport([3]string{"tests.test_cart", "test_total", "skipped"})), model.MutantInconclusive},
		{"usage error", run("FAIL", 4, pyReport([3]string{"tests.test_cart", "test_total", "failed"})), model.MutantKilled},
		{"crash", run("FAIL", 137, pyReport([3]string{"tests.test_cart", "test_total", "failed"})), model.MutantInconclusive},
	} {
		if v := ClassifyFor(true, control, tc.run); v.Status != tc.want {
			t.Errorf("%s: %+v, want %s", tc.name, v, tc.want)
		}
	}
	if v := ClassifyFor(true, control, run("FAIL", 1, pyReport([3]string{"tests.test_cart", "test_total", "failed"}))); len(v.FailedTests) != 1 || v.FailedTests[0] != "tests.test_cart::test_total" {
		t.Fatalf("failed tests %q", v.FailedTests)
	}
	for name, bad := range map[string]string{
		"failing control":     pyReport([3]string{"tests.test_cart", "test_total", "failed"}),
		"no passing test":     pyReport([3]string{"tests.test_cart", "test_total", "skipped"}),
		"collection error":    pyReport([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"", "tests.test_other", "error"}),
		"not a pytest report": `{"format":"other","testcases":[]}`,
	} {
		c := control
		c.Results = bad
		if NewControlFor(true, c).Reason() == "" {
			t.Errorf("%s accepted as a control", name)
		}
	}
}
