package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// pyIntentCart is the candidate's shop/cart.py: discount (lines 5-8) is new,
// and its threshold is wrong for the criterion "orders of 100 or more get 10
// off".
const pyIntentCart = `def total(prices):
    return sum(prices)


def discount(amount):
    if amount > 100:
        return amount - 10
    return amount
`

// pyIntentFixture is a harness over a Python project whose candidate adds
// discount to shop/cart.py, with a verifiable pytest template.
func pyIntentFixture(t *testing.T) *Harness {
	t.Helper()
	src, base := t.TempDir(), t.TempDir()
	files := map[string]map[string]string{
		src:  {"shop/__init__.py": "", "shop/cart.py": pyIntentCart, "tests/helpers.py": "def check(value, want):\n    assert value == want\n"},
		base: {"shop/__init__.py": "", "shop/cart.py": "def total(prices):\n    return sum(prices)\n"},
	}
	for dir, fs := range files {
		fs["pyproject.toml"] = "[tool.pytest.ini_options]\npythonpath = [\".\"]\n"
		for name, content := range fs {
			full := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	lines := strings.Split(pyIntentCart, "\n")
	var added []model.DiffLine
	for n := 5; n <= 8; n++ {
		added = append(added, model.DiffLine{Kind: "add", NewLine: n, Content: lines[n-1]})
	}
	diff, _ := json.Marshal(model.Change{Files: []model.ChangedFile{{Path: "shop/cart.py", Status: "M", Hunks: []model.Hunk{{Lines: added}}}}})
	h, err := New(Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxGeneratedTests: 10, Diff: string(diff), IntentCriteria: intentCriteria,
		Commands: map[string][]string{"generated_test": pytestTemplate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// TestIntentPytestRunnerWithRealPytest runs intent tests for AC-1 with the
// real pytest on the candidate snapshot.
func TestIntentPytestRunnerWithRealPytest(t *testing.T) {
	python := requirePytest(t)
	for _, tc := range []struct {
		name, content, status, check string
		symbols                      []string
	}{
		{"assertion", "from shop.cart import discount\n\n\ndef test_orders_of_100_get_10_off():\n    assert discount(100) == 90\n", model.StatusIntentTestFailed, "FAIL", []string{"discount"}},
		{"pytest.fail", "import pytest\nfrom shop.cart import discount\n\n\ndef test_orders_of_100_get_10_off():\n    if discount(100) != 90:\n        pytest.fail(\"no discount at 100\")\n", model.StatusIntentTestFailed, "FAIL", []string{"discount"}},
		{"pass", "from shop.cart import discount\n\n\ndef test_orders_of_150_get_10_off():\n    assert discount(150) == 140\n", model.StatusIntentTestPassed, "PASS", []string{"discount"}},
		{"code raises", "from shop.cart import discount\n\n\ndef test_orders_of_100_get_10_off():\n    assert discount(None) == 90\n", model.StatusUnverified, "FAIL", []string{"discount"}},
		{"helper assertion", "from shop.cart import discount\nfrom tests.helpers import check\n\n\ndef test_orders_of_100_get_10_off():\n    check(discount(100), 90)\n", model.StatusUnverified, "FAIL", []string{"discount"}},
		{"import error", "from shop.cart import discount, missing\n\n\ndef test_orders_of_100_get_10_off():\n    assert discount(100) == 90\n", model.StatusUnverified, "ERROR", []string{"discount"}},
		{"unchanged code", "from shop.cart import total\n\n\ndef test_total():\n    assert total([1]) == 2\n", model.StatusUnverified, "FAIL", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := pyIntentFixture(t)
			var argv [][]string
			h.executeCapture = realPytest(t, python, &argv)
			call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": "tests/test_intent_ac1.py", "content": tc.content})
			result := runIntent(t, h, "intent-test-1")
			e := result.Evidence
			if e.Status != tc.status || result.CandidateCheck.Status != tc.check || e.Runner != RunnerPytest || len(argv) != 1 {
				t.Fatalf("evidence %+v check %+v argv %q", e, result.CandidateCheck, argv)
			}
			if !reflect.DeepEqual(e.ReferencedSymbols, tc.symbols) || !strings.Contains(e.Description, noteLexicalPython) {
				t.Fatalf("symbols %q description %q", e.ReferencedSymbols, e.Description)
			}
			// The report re-derives the same status from the recorded check.
			status, _ := IntentOutcome(e.Runner, result.CandidateCheck, e.Path, e.TestNames, e.ReferencedSymbols, h.intent.words)
			if status != tc.status {
				t.Fatalf("re-derived %s", status)
			}
		})
	}
}

func TestPytestAssertionMessage(t *testing.T) {
	const p = "tests/test_kinds.py"
	for m, want := range map[string]bool{
		// Recorded from pytest 9.1.1 (message attribute, then the end of the traceback).
		"assert 1 == 2\n>   def test_plain(): assert 1 == 2\nE   assert 1 == 2\n\ntests/test_kinds.py:4: AssertionError": true,
		"AssertionError: 1 != 2\nE   AssertionError: 1 != 2\n\ntests/test_kinds.py:13: AssertionError":                   true,
		"Failed: DID NOT RAISE ValueError\nE   Failed: DID NOT RAISE ValueError\n\ntests/test_kinds.py:10: Failed":       true,
		"assert 1 == 2\n\ntest_kinds.py:4: AssertionError":                                                               true, // rootdir tests/
		"assert 1 == 2\n\n/workspace/tests/test_kinds.py:4: AssertionError":                                              true,
		"ValueError: bad input\nE   ValueError: bad input\n\ntests/test_kinds.py:3: ValueError":                          false,
		"AssertionError: helper says no\n\ntests/helper.py:2: AssertionError":                                            false,
		"assert 1 == 2\n\nother/test_kinds.py:4: AssertionError":                                                         false,
		"assert 1 == 2\n\nxtests/test_kinds.py:4: AssertionError":                                                        false,
		"assert 1 == 2":        false, // --tb=no
		"AssertionError: boom": false,
		"":                     false,
	} {
		if got := pytestAssertionMessage(m, p); got != want {
			t.Errorf("pytestAssertionMessage(%q) = %v", m, got)
		}
	}
}

// A long traceback keeps its message and its last line, which names where the
// failure was raised.
func TestJUnitMessageKeepsTheTracebackEnd(t *testing.T) {
	text := strings.Repeat("E   long line of the traceback\n", 2000) + "\ntests/test_x.py:9: AssertionError"
	raw := `<testsuites><testsuite><testcase classname="tests.test_x" name="test_long"><failure message="assert 1 == 2">` + text + `</failure></testcase></testsuite></testsuites>`
	file, ok := pytestFile(normalizedJUnit(t, raw), "tests/test_x.py")
	if !ok || len(file.Cases) != 1 || len(file.Cases[0].Messages) != 1 {
		t.Fatalf("file %+v", file)
	}
	m := file.Cases[0].Messages[0]
	if len(m) > junitMessageLimit || !strings.HasPrefix(m, "assert 1 == 2\n") || !strings.HasSuffix(m, "tests/test_x.py:9: AssertionError") || !pytestAssertionMessage(m, "tests/test_x.py") {
		t.Fatalf("message (%d bytes) %q…%q", len(m), m[:40], m[len(m)-40:])
	}
}

// A setup error fails the test (it did not pass) but is not an assertion of
// the test.
func TestPytestSetupErrorIsNotAnAssertion(t *testing.T) {
	raw := `<testsuites><testsuite><testcase classname="tests.test_x" name="test_a"><error message="failed on setup">E   RuntimeError

tests/test_x.py:3: AssertionError</error></testcase></testsuite></testsuites>`
	if pytestAssertionFailed(normalizedJUnit(t, raw), "tests/test_x.py", []string{"test_a"}) {
		t.Fatal("a setup error read as an assertion")
	}
}
