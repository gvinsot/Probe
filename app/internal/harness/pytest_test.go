package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

var pytestTemplate = []string{"python3", "-m", "pytest", "-p", "no:cacheprovider", "{file}", "--junitxml={results_out}"}

// recordedJUnit is the report pytest 9.1.1 wrote for tests/test_mod.py (one
// passing, failing, erroring, skipped, xfailed, parametrized, class and nested
// class test), with the rootdir at the repository root. Only the timestamp and
// host name were shortened.
const recordedJUnit = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="1" failures="1" skipped="2" tests="9" time="0.034" timestamp="2026-10-01T22:14:00" hostname="h"><testcase classname="tests.test_mod" name="test_pass" time="0.000" /><testcase classname="tests.test_mod" name="test_fail" time="0.007"><failure message="AssertionError: boom&#10;assert 1 == 2">&gt;   def test_fail(): assert 1 == 2, "boom"
E   AssertionError: boom
E   assert 1 == 2

tests/test_mod.py:3: AssertionError</failure></testcase><testcase classname="tests.test_mod" name="test_error" time="0.000"><error message="failed on setup with &quot;RuntimeError: fixture broke&quot;">E   RuntimeError: fixture broke

tests/test_mod.py:5: RuntimeError</error></testcase><testcase classname="tests.test_mod" name="test_skip" time="0.000"><skipped type="pytest.skip" message="nope">/tmp/pyj/tests/test_mod.py:7: nope</skipped></testcase><testcase classname="tests.test_mod" name="test_xfail" time="0.000"><skipped type="pytest.xfail" message="" /></testcase><testcase classname="tests.test_mod" name="test_param[1]" time="0.000" /><testcase classname="tests.test_mod" name="test_param[a b]" time="0.000" /><testcase classname="tests.test_mod.TestGroup" name="test_in_class" time="0.000" /><testcase classname="tests.test_mod.TestGroup.TestInner" name="test_nested" time="0.000" /></testsuite></testsuites>`

// recordedCollectionError is the report pytest 9.1.1 wrote for a module with a
// syntax error (trace shortened).
const recordedCollectionError = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="1" failures="0" skipped="0" tests="1" time="0.112" timestamp="2026-10-01T22:14:01" hostname="h"><testcase classname="" name="tests.sub.test_broken" time="0.000"><error message="collection failure">E     File "/tmp/pyj/tests/sub/test_broken.py", line 1
E       def test_x(: pass
E   SyntaxError: invalid syntax</error></testcase></testsuite></testsuites>`

// junit renders a minimal pytest JUnit report: classname -> name -> status
// ("passed", "failed", "error" or "skipped").
func junit(cases ...[3]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest">`)
	for _, c := range cases {
		fmt.Fprintf(&b, `<testcase classname=%q name=%q time="0.001">`, c[0], c[1])
		switch c[2] {
		case "failed":
			b.WriteString(`<failure message="AssertionError: assert 1 == 2">E   assert 1 == 2</failure>`)
		case "error":
			b.WriteString(`<error message="failed on setup">E   RuntimeError</error>`)
		case "skipped":
			b.WriteString(`<skipped type="pytest.skip" message="skip" />`)
		}
		b.WriteString(`</testcase>`)
	}
	b.WriteString(`</testsuite></testsuites>`)
	return b.String()
}

func normalizedJUnit(t *testing.T, xml string) string {
	t.Helper()
	got, err := normalizePytestReport([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestVerifiablePytestTemplate(t *testing.T) {
	for _, tc := range []struct {
		command []string
		want    bool
	}{
		{[]string{"pytest", "{file}", "--junitxml={results_out}"}, true},
		{[]string{"py.test", "-q", "{file}", "--junit-xml={results_out}"}, true},
		{[]string{"python3", "-m", "pytest", "{file}", "--junitxml", "{results_out}"}, true},
		{[]string{"/usr/local/bin/python3.12", "-m", "pytest", "-x", "{file}", "--junitxml={results_out}"}, true},
		{[]string{"python", "-m", "pytest", "{file}", "--junitxml={results_out}"}, true},
		{[]string{"pytest", "{file}"}, false},                                                         // no report
		{[]string{"pytest", "{file}", "--junitxml=/tmp/r.xml"}, false},                                // report elsewhere
		{[]string{"pytest", "{file}", "--junitxml={results_out}", "--junitxml={results_out}"}, false}, // twice
		{[]string{"pytest", "{file}", "--resultlog={results_out}"}, false},                            // not JUnit
		{[]string{"pytest", "{file}", "{results_out}"}, false},                                        // not after --junitxml
		{[]string{"pytest", "{file}", "{file}", "--junitxml={results_out}"}, false},                   // two targets
		{[]string{"pytest", "tests/{file}", "--junitxml={results_out}"}, false},                       // not standalone
		{[]string{"pytest", "{package}", "--junitxml={results_out}"}, false},
		{[]string{"pytest", "{file}", "--junitxml={results_out}", "--junit-prefix=x"}, false}, // rewrites classnames
		{[]string{"python3", "pytest", "{file}", "--junitxml={results_out}"}, false},          // a script, not -m pytest
		{[]string{"python3", "-m", "unittest", "{file}", "--junitxml={results_out}"}, false},
		{[]string{"python3.x", "-m", "pytest", "{file}", "--junitxml={results_out}"}, false},
		{[]string{"uv", "run", "pytest", "{file}", "--junitxml={results_out}"}, false},
		{[]string{"poetry", "run", "pytest", "{file}", "--junitxml={results_out}"}, false},
		{[]string{"sh", "-c", "pytest {file} --junitxml={results_out}"}, false},
		{[]string{"tox", "--", "{file}", "--junitxml={results_out}"}, false},
	} {
		if got := verifiablePytestTemplate(tc.command); got != tc.want {
			t.Errorf("verifiablePytestTemplate(%q) = %v, want %v", tc.command, got, tc.want)
		}
		if isPytestCommand(tc.command) && verifiableJSTemplate(tc.command) {
			t.Errorf("verifiableJSTemplate accepted the pytest command %q", tc.command)
		}
	}
}

func TestGeneratedPyTests(t *testing.T) {
	names, err := generatedPyTests("import pytest\nfrom cart import total\n\ndef test_total():\n    assert total([1]) == 1\n\nasync def test_async():\n    pass\n\ndef helper():\n    pass\n\nclass TestNested:\n    def test_inside(self):\n        pass\n\ndef  test_spaced (x):\n    pass\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"test_total", "test_async", "test_spaced"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	for _, source := range []string{
		"def helper():\n    pass\n",
		"class TestOnly:\n    def test_x(self):\n        pass\n",
		"def test_x():\n    pass\ndef test_x():\n    pass\n",
	} {
		if _, err := generatedPyTests(source); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
}

func TestIsPyTestPath(t *testing.T) {
	for p, want := range map[string]bool{
		"tests/test_cart.py": true, "cart_test.py": true, "Test_Cart.py": true,
		"tests/cart.py": false, "conftest.py": false, "tests/test_cart.pyc": false, "test_cart.ts": false,
	} {
		if got := isPyTestPath(p); got != want {
			t.Errorf("isPyTestPath(%q) = %v", p, got)
		}
	}
}

func TestNormalizeRecordedPytestReport(t *testing.T) {
	file, ok := pytestFile(normalizedJUnit(t, recordedJUnit), "tests/test_mod.py")
	if !ok || file.LoadFailed {
		t.Fatalf("file %+v, %v", file, ok)
	}
	got := map[string]string{}
	top := map[string]bool{}
	for _, c := range file.Cases {
		got[c.Name] = c.Status
		top[c.Name] = c.TopLevel
	}
	want := map[string]string{
		"test_pass": "passed", "test_fail": "failed", "test_error": "failed", "test_skip": "skipped", "test_xfail": "skipped",
		"test_param[1]": "passed", "test_param[a b]": "passed",
		"TestGroup::test_in_class": "passed", "TestGroup::TestInner::test_nested": "passed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cases %v\nwant  %v", got, want)
	}
	if top["TestGroup::test_in_class"] || !top["test_pass"] {
		t.Fatalf("top-level flags %v", top)
	}
	for _, c := range file.Cases {
		if c.Name == "test_fail" && (len(c.Messages) != 1 || !strings.Contains(c.Messages[0], "AssertionError: boom")) {
			t.Fatalf("failure message %q", c.Messages)
		}
	}

	broken, ok := pytestFile(normalizedJUnit(t, recordedCollectionError), "tests/sub/test_broken.py")
	if !ok || !broken.LoadFailed || len(broken.Cases) != 0 {
		t.Fatalf("collection error read as %+v, %v", broken, ok)
	}
}

// TestPytestFileMatchesEveryRootdir reads the same module reported from three
// rootdirs and never attributes another module's tests to it.
func TestPytestFileMatchesEveryRootdir(t *testing.T) {
	for _, classname := range []string{"tests.unit.test_cart", "unit.test_cart", "test_cart"} {
		results := normalizedJUnit(t, junit(
			[3]string{classname, "test_total", "passed"},
			[3]string{classname + ".TestCart", "test_empty", "failed"},
		))
		file, ok := pytestFile(results, "tests/unit/test_cart.py")
		if !ok || len(file.Cases) != 2 || file.Cases[0].Name != "test_total" || file.Cases[1].Name != "TestCart::test_empty" || file.Cases[1].TopLevel {
			t.Errorf("%s: %+v %v", classname, file, ok)
		}
	}
	for _, classname := range []string{"tests.other.test_cart", "other.test_cart", "tests.unit.test_cart_extra", "test_cart_extra", "xtest_cart", ""} {
		results := normalizedJUnit(t, junit([3]string{classname, "test_total", "passed"}))
		if file, ok := pytestFile(results, "tests/unit/test_cart.py"); ok {
			t.Errorf("%q attributed to tests/unit/test_cart.py: %+v", classname, file)
		}
	}
}

func TestValidatePytestExecution(t *testing.T) {
	path, names := "tests/test_cart.py", []string{"test_total", "test_empty"}
	pass := normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"tests.test_cart", "test_empty", "passed"}))
	if c := ValidatePytestExecution(model.Check{Status: "PASS", Results: pass}, path, names); c.Status != "PASS" {
		t.Fatalf("pass rejected: %+v", c)
	}
	fail := normalizedJUnit(t, junit([3]string{"test_cart", "test_total", "passed"}, [3]string{"test_cart", "test_empty", "failed"}))
	if c := ValidatePytestExecution(model.Check{Status: "FAIL", ExitCode: 1, Results: fail}, path, names); c.Status != "FAIL" {
		t.Fatalf("failure rejected: %+v", c)
	}
	setup := normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "error"}, [3]string{"tests.test_cart", "test_empty", "passed"}))
	if c := ValidatePytestExecution(model.Check{Status: "FAIL", ExitCode: 1, Results: setup}, path, names); c.Status != "FAIL" {
		t.Fatalf("a setup error is a failure of the test, as a failing Jest hook: %+v", c)
	}
	for _, tc := range []struct {
		name  string
		check model.Check
	}{
		{"no_results", model.Check{Status: "PASS"}},
		{"jest_report", model.Check{Status: "PASS", Results: jestResults(path, map[string]string{"test_total": "passed", "test_empty": "passed"})}},
		{"malformed", model.Check{Status: "PASS", Results: "{"}},
		{"other_file", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_other", "test_total", "passed"}, [3]string{"tests.test_other", "test_empty", "passed"}))}},
		{"skipped", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "skipped"}, [3]string{"tests.test_cart", "test_empty", "passed"}))}},
		{"missing", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "passed"}))}},
		{"in_class", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_cart.TestX", "test_total", "passed"}, [3]string{"tests.test_cart", "test_empty", "passed"}))}},
		{"parametrized", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total[1]", "passed"}, [3]string{"tests.test_cart", "test_empty", "passed"}))}},
		{"duplicate", model.Check{Status: "PASS", Results: normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"test_cart", "test_total", "passed"}, [3]string{"tests.test_cart", "test_empty", "passed"}))}},
		{"unrelated_failure", model.Check{Status: "FAIL", ExitCode: 1, Results: normalizedJUnit(t, junit([3]string{"tests.test_cart", "test_total", "passed"}, [3]string{"tests.test_cart", "test_empty", "passed"}, [3]string{"tests.test_cart", "test_other", "failed"}))}},
		{"exit_without_failure", model.Check{Status: "FAIL", ExitCode: 1, Results: pass}},
		{"collection_error", model.Check{Status: "FAIL", ExitCode: 2, Results: normalizedJUnit(t, `<testsuites><testsuite><testcase classname="" name="tests.test_cart"><error message="collection failure"/></testcase></testsuite></testsuites>`)}},
		{"truncated", model.Check{Status: "PASS", Results: pass, Truncated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := ValidatePytestExecution(tc.check, path, names); c.Status != "ERROR" {
				t.Fatalf("false execution proof: %+v", c)
			}
		})
	}
	if c, known := ValidateExecution(RunnerPytest, model.Check{Status: "PASS", Results: pass}, path, names); !known || c.Status != "PASS" {
		t.Fatalf("ValidateExecution does not dispatch pytest_junit: %+v %v", c, known)
	}
}

func TestNormalizePytestReportRejectsAndBounds(t *testing.T) {
	for name, raw := range map[string]string{
		"json":     `{"testResults":[]}`,
		"html":     `<html><body/></html>`,
		"nested":   `<testsuites><testcase classname="a" name="t"><testcase classname="a" name="u"/></testcase></testsuites>`,
		"entity":   `<?xml version="1.0"?><!DOCTYPE t [<!ENTITY x "expanded">]><testsuites><testsuite><testcase classname="a" name="&x;"/></testsuite></testsuites>`,
		"unclosed": `<testsuites><testsuite><testcase classname="a" name="t">`,
		"latin1":   "<testsuites name=\"\xe9\"/>",
		"empty":    ``,
	} {
		if got, err := normalizePytestReport([]byte(raw)); err == nil {
			t.Errorf("%s accepted: %s", name, got)
		}
	}
	secret := "Bearer abcdefghijklmnopqrstuvwxyz0123456789"
	raw := `<testsuites><testsuite><testcase classname="tests.test_a" name="test_leak"><failure message="` + secret + `">` + strings.Repeat("x", 10000) + `</failure><system-out>` + strings.Repeat("y", 100000) + `</system-out></testcase></testsuite></testsuites>`
	got := normalizedJUnit(t, raw)
	if strings.Contains(got, "abcdefghijklmnop") || strings.Contains(got, "yyyy") || len(got) > 5000 || !redact.IsFixedPoint(got) {
		t.Fatalf("normalized report keeps secrets, captured output or unbounded text (%d bytes)", len(got))
	}
}

func TestSelectPytestTests(t *testing.T) {
	got := selectPytestTests([]string{"pytest", "-q", "tests/test_cart.py", "--junitxml=" + ResultsPath}, "tests/test_cart.py", []string{"test_total", "TestCart::test_empty[a b]"})
	want := []string{"pytest", "-q", "tests/test_cart.py::test_total", "tests/test_cart.py::TestCart::test_empty[a b]", "--junitxml=" + ResultsPath}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	for name, ok := range map[string]bool{"test_x": true, "TestA::test_x[1-a b]": true, "": false, "::test_x": false, "TestA::": false, "-k": false, "test\nx": false} {
		if ValidPytestTestName(name) != ok {
			t.Errorf("ValidPytestTestName(%q) != %v", name, ok)
		}
	}
}

// requirePytest returns the Python interpreter that has pytest, or skips.
func requirePytest(t *testing.T) string {
	t.Helper()
	for _, python := range []string{"python3", "python"} {
		if path, err := exec.LookPath(python); err == nil && exec.Command(path, "-c", "import pytest").Run() == nil {
			return path
		}
	}
	t.Skip("pytest is not installed on the host")
	return ""
}

// realPytest returns an executeCapture that does what the sandbox does with
// the docker argv it receives: it copies the read-only /source mount into a
// fresh /workspace, runs the command there with the real pytest, and returns
// the JUnit report written to ResultsPath on the payload channel.
func realPytest(t *testing.T, python string, argv *[][]string) func(context.Context, string, []string, io.Writer, io.Writer) execution {
	return func(ctx context.Context, _ string, args []string, log, payload io.Writer) execution {
		source, command := "", []string(nil)
		for i, arg := range args {
			if src, ok := strings.CutPrefix(arg, "type=bind,src="); ok {
				source = strings.TrimSuffix(src, ",dst=/source,readonly")
			}
			if arg == "-c" && i+2 < len(args) && args[i+2] == "probe" {
				command = args[i+3:]
				break
			}
		}
		if source == "" || len(command) < 4 || command[1] != "-m" || command[2] != "pytest" {
			t.Fatalf("unexpected docker argv %q", args)
		}
		*argv = append(*argv, command)
		workspace := t.TempDir()
		if err := os.CopyFS(workspace, os.DirFS(source)); err != nil {
			t.Fatal(err)
		}
		report := filepath.Join(t.TempDir(), "junit.xml")
		local := []string{"-m", "pytest"}
		for _, arg := range command[3:] {
			local = append(local, strings.ReplaceAll(arg, ResultsPath, report))
		}
		cmd := exec.CommandContext(ctx, python, local...)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		cmd.Stdout, cmd.Stderr = log, log
		err := cmd.Run()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if b, readErr := os.ReadFile(report); readErr == nil {
			fmt.Fprint(payload, coverageFrame(string(b)))
		}
		return execution{ExitCode: code}
	}
}

// pyFixture is a harness over a Python project whose candidate changes
// total(): the discount is applied twice.
func pyFixture(t *testing.T) *Harness {
	t.Helper()
	src, base := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{
		base: "def total(prices, discount):\n    return sum(prices) * (1 - discount)\n",
		src:  "def total(prices, discount):\n    return sum(prices) * (1 - discount) * (1 - discount)\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, "shop"), 0755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{"shop/__init__.py": "", "shop/cart.py": body, "pyproject.toml": "[tool.pytest.ini_options]\npythonpath = [\".\"]\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	h, err := New(Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxGeneratedTests: 10, Commands: map[string][]string{"generated_test": pytestTemplate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

const generatedPySource = `from shop.cart import total


def test_discount_applied_once():
    assert total([10], 0.5) == 5


def test_no_discount():
    assert total([10], 0) == 10
`

func runPython(t *testing.T, h *Harness, content string) map[string]any {
	t.Helper()
	call(t, h, "create_test", map[string]any{"path": "tests/test_probe_discount.py", "content": content, "description": "discount applied once"})
	var result map[string]any
	if err := json.Unmarshal(call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"}), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestPythonDifferentialWithRealPytest runs a generated pytest module through
// the harness against the real pytest on both snapshots.
func TestPythonDifferentialWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	h := pyFixture(t)
	var argv [][]string
	h.executeCapture = realPytest(t, python, &argv)
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("verifiable experiment ran without the results channel")
		return execution{}
	}
	e := runPython(t, h, generatedPySource)["evidence"].(map[string]any)
	if e["status"] != model.StatusReproduced || e["runner"] != RunnerPytest || !reflect.DeepEqual(e["test_names"], []any{"test_discount_applied_once", "test_no_discount"}) {
		t.Fatalf("evidence %+v", e)
	}
	if len(argv) != 2 || !contains(argv[0], "tests/test_probe_discount.py") || !contains(argv[0], "--junitxml="+ResultsPath) {
		t.Fatalf("argv %q", argv)
	}
	checks := h.Checks()
	if len(checks) != 2 || checks[0].Status != "PASS" || checks[1].Status != "FAIL" || checks[1].Results == "" {
		t.Fatalf("checks %+v", checks)
	}
	if file, ok := pytestFile(checks[1].Results, "tests/test_probe_discount.py"); !ok || len(file.Cases) != 2 {
		t.Fatalf("candidate report %s", checks[1].Results)
	}
	kinds := map[string]bool{}
	for _, a := range h.Artifacts() {
		kinds[a.Kind] = true
	}
	if !kinds["test_results"] || !kinds["generated_test"] {
		t.Fatalf("results and reproducer must be retained: %+v", h.Artifacts())
	}
}

func TestPythonDifferentialInconclusiveWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	for name, content := range map[string]string{
		// Both revisions fail: nothing is reproduced.
		"baseline_fails": "from shop.cart import total\n\n\ndef test_wrong():\n    assert total([10], 0.5) == 1\n",
		// The module does not import: a collection error, not a test failure.
		"collection_error": "from shop.cart import missing\n\n\ndef test_x():\n    assert missing()\n",
		// A skipped test never ran.
		"skipped": "import pytest\nfrom shop.cart import total\n\n\n@pytest.mark.skip\ndef test_skipped():\n    assert total([10], 0.5) == 5\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := pyFixture(t)
			var argv [][]string
			h.executeCapture = realPytest(t, python, &argv)
			e := runPython(t, h, content)["evidence"].(map[string]any)
			if e["status"] != model.StatusUnverified || e["runner"] != RunnerPytest {
				t.Fatalf("evidence %+v", e)
			}
		})
	}
}

func TestPythonGeneratedTestNeedsTopLevelFunctions(t *testing.T) {
	h := pyFixture(t)
	if _, err := h.createTest("tests/test_probe.py", "class TestX:\n    def test_x(self):\n        pass\n", ""); err == nil {
		t.Fatal("a verifiable pytest template accepted a module without top-level test functions")
	}
	// Without a verifiable template, free-form content is still accepted.
	h.opts.Commands["generated_test"] = []string{"pytest", "{file}"}
	if _, err := h.createTest("tests/test_probe.py", "class TestX:\n    def test_x(self):\n        pass\n", ""); err != nil {
		t.Fatal(err)
	}
}

// TestNormalizeLivePytestReport normalizes what the installed pytest writes,
// so a change in its JUnit output is noticed.
func TestNormalizeLivePytestReport(t *testing.T) {
	python := requirePytest(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0755); err != nil {
		t.Fatal(err)
	}
	module := "import pytest\n\ndef test_pass():\n    assert True\n\ndef test_fail():\n    assert 1 == 2\n\n@pytest.mark.skip\ndef test_skip():\n    pass\n\nclass TestGroup:\n    def test_in_class(self):\n        pass\n"
	if err := os.WriteFile(filepath.Join(dir, "tests", "test_live.py"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "junit.xml")
	cmd := exec.Command(python, "-m", "pytest", "-p", "no:cacheprovider", "tests/test_live.py", "--junitxml="+report)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("pytest wrote no report: %s", out.String())
	}
	file, ok := pytestFile(normalizedJUnit(t, string(raw)), "tests/test_live.py")
	got := map[string]string{}
	for _, c := range file.Cases {
		got[c.Name] = c.Status
	}
	want := map[string]string{"test_pass": "passed", "test_fail": "failed", "test_skip": "skipped", "TestGroup::test_in_class": "passed"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("live report read as %v, want %v\n%s", got, want, raw)
	}
}
