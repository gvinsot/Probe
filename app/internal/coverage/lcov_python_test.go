package coverage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// coveragePyLCOV is the report coverage.py 7.16 wrote through pytest-cov
// (--cov=. --cov-report=lcov:<dir>/lcov.info) for shop/cart.py, whose total
// spans lines 4-9 (a two-line signature, an if/else) and whose unused
// function never ran: one DA record per statement, none for the continuation
// of the signature or the else line.
const coveragePyLCOV = `SF:shop/__init__.py
end_of_record
SF:shop/cart.py
DA:4,1
DA:6,1
DA:7,0
DA:9,1
DA:12,1
DA:13,0
LF:6
LH:4
FN:4,9,total
FNDA:1,total
FN:12,13,unused
FNDA:0,unused
FNF:2
FNH:1
end_of_record
SF:tests/test_cart.py
DA:1,1
DA:2,1
DA:3,1
LF:3
LH:3
FN:2,3,test_total
FNDA:1,test_total
FNF:1
FNH:1
end_of_record
`

func pytestCovRun(status string) Run {
	return Run{CheckID: "check-4", Status: status, Command: []string{"python", "-m", "pytest", "--cov=.", "--cov-report=lcov:/tmp/probe-coverage/lcov.info"}, SHA256: strings.Repeat("b", 64)}
}

func TestAnalyzeLCOVClassifiesPythonSources(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{
		file("shop/cart.py", []int{5, 7, 8, 13}, []int{2}),
		file("shop/tax.py", []int{1}, nil),
		file("tests/test_cart.py", []int{3}, nil),
		file("tests/conftest.py", []int{1}, nil),
		file("web/app.ts", []int{1}, nil),
	}}
	result := Analyze(mustLCOV(t, coveragePyLCOV), pytestCovRun("PASS"), change)
	c := result.Report()
	if c.Status != StatusMeasured || c.Format != FormatLCOV || c.Note != NoteLCOV || Languages(c) != "Python" {
		t.Fatalf("report header %+v (%s)", c, Languages(c))
	}
	// cart.py: 5 (continuation) and 8 (else) have no entry, 7 and 13 never
	// ran; tax.py was not loaded by any test; test modules and the
	// TypeScript file the report does not name are outside the measurement.
	if c.AddedLines != 5 || c.ExecutedLines != 0 || c.NotExecutedLines != 2 || c.NoBlockLines != 2 || c.NotMeasuredLines != 1 || len(c.Files) != 2 {
		t.Fatalf("counters %+v", c)
	}
	signals := result.Signals()
	if len(signals) != 2 || signals[0].Path != "shop/cart.py" || signals[0].Line != 7 || signals[1].Line != 13 {
		t.Fatalf("signals %+v", signals)
	}
}

// A report measures the languages it names: a Vitest report never makes the
// Python changes of the same repository "not measured", nor the reverse, and
// a report naming both measures both.
func TestAnalyzeLCOVScopesByReportedLanguages(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{file("web/app.ts", []int{1}, nil), file("shop/cart.py", []int{7}, nil)}}
	for _, tc := range []struct {
		name, report string
		files        []string
		languages    string
	}{
		{"vitest", "SF:web/app.ts\nDA:1,1\nend_of_record\n", []string{"web/app.ts"}, "TypeScript/JavaScript"},
		{"coverage.py", coveragePyLCOV, []string{"shop/cart.py"}, "Python"},
		{"merged", "SF:web/app.ts\nDA:1,1\nend_of_record\n" + coveragePyLCOV, []string{"shop/cart.py", "web/app.ts"}, "TypeScript/JavaScript and Python"},
		{"tests only", "SF:tests/test_cart.py\nDA:1,1\nend_of_record\n", []string{"shop/cart.py"}, "Python"},
	} {
		c := Analyze(mustLCOV(t, tc.report), pytestCovRun("PASS"), change).Report()
		var got []string
		for _, f := range c.Files {
			got = append(got, f.Path)
		}
		if strings.Join(got, ",") != strings.Join(tc.files, ",") || Languages(c) != tc.languages {
			t.Errorf("%s: files %v (%s), want %v (%s)", tc.name, got, Languages(c), tc.files, tc.languages)
		}
	}
}

func TestPythonSource(t *testing.T) {
	for p, want := range map[string]bool{
		"shop/cart.py": true, "cart.py": true, "src/pkg/__init__.py": true, "testing/helpers.py": true,
		"tests/helpers.py": false, "pkg/test/util.py": false, "test_cart.py": false, "shop/cart_test.py": false, "conftest.py": false,
		".venv/lib/x.py": false, "lib/python3.12/site-packages/x.py": false, "shop/cart.pyi": false, "shop/cart.ts": false,
	} {
		if PythonSource(p) != want {
			t.Errorf("PythonSource(%q) = %v, want %v", p, !want, want)
		}
	}
}

// TestPytestCovEndToEnd runs the documented command with the real pytest and
// pytest-cov on the host, as the sandbox would in /workspace, and analyzes
// the report it writes at the captured path.
func TestPytestCovEndToEnd(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import pytest, pytest_cov").Run() != nil {
		t.Skip("pytest and pytest-cov are not installed on the host")
	}
	workspace := t.TempDir()
	for name, content := range map[string]string{
		"shop/__init__.py":   "",
		"shop/cart.py":       "def total(prices, discount=0):\n    if discount:\n        return sum(prices) * (1 - discount)\n    return sum(prices)\n",
		"tests/test_cart.py": "from shop.cart import total\n\n\ndef test_total():\n    assert total([1, 2]) == 3\n",
		"pyproject.toml":     "[tool.pytest.ini_options]\npythonpath = [\".\"]\n",
	} {
		full := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	command, capture := Expand([]string{"python3", "-m", "pytest", "-p", "no:cacheprovider", "--cov=.", "--cov-report=lcov:" + DirPlaceholder + "/lcov.info"})
	if capture != LCOVPath {
		t.Fatalf("capture %s", capture)
	}
	// The sandbox's /tmp/probe-coverage is a private directory here.
	dir := filepath.Join(t.TempDir(), "probe-coverage")
	for i, arg := range command {
		command[i] = strings.ReplaceAll(arg, ReportDir, dir)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "lcov.info"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := Parse(raw)
	if err != nil || profile.Format != FormatLCOV {
		t.Fatalf("parse: %v %+v", err, profile)
	}
	change := model.Change{Files: []model.ChangedFile{file("shop/cart.py", []int{2, 3, 4}, nil)}}
	c := Analyze(profile, pytestCovRun("PASS"), change).Report()
	// Lines 2 and 4 ran; line 3 (the discount branch) never did.
	if c.AddedLines != 3 || c.ExecutedLines != 2 || c.NotExecutedLines != 1 || Languages(c) != "Python" {
		t.Fatalf("analysis %+v\n%s", c, raw)
	}
}
