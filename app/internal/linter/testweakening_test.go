package linter

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// hunk builds one hunk from lines prefixed with "+", "-" or " ".
func hunk(oldStart, newStart int, lines ...string) model.Hunk {
	h := model.Hunk{OldStart: oldStart, NewStart: newStart}
	oldLine, newLine := oldStart, newStart
	for _, l := range lines {
		d := model.DiffLine{Content: l[1:]}
		switch l[0] {
		case '+':
			d.Kind, d.NewLine = "add", newLine
			newLine++
		case '-':
			d.Kind, d.OldLine = "delete", oldLine
			oldLine++
		default:
			d.Kind, d.OldLine, d.NewLine = "context", oldLine, newLine
			oldLine++
			newLine++
		}
		h.Lines = append(h.Lines, d)
	}
	return h
}

func changed(path string, hunks ...model.Hunk) model.ChangedFile {
	return model.ChangedFile{Path: path, Status: "M", Hunks: hunks}
}

// kinds returns "kind@side:line" for each signal.
func kinds(signals []model.Signal) string {
	var out []string
	for _, s := range signals {
		out = append(out, fmt.Sprintf("%s@%s:%d", s.Kind, s.Side, s.Line))
	}
	return strings.Join(out, ",")
}

func TestTestWeakeningSignals(t *testing.T) {
	cases := []struct {
		name string
		file model.ChangedFile
		want string
	}{
		{"rust_assertion_removed_and_ignored", changed("tests/cart.rs", hunk(3, 3,
			"+#[ignore]",
			" #[test]",
			" fn totals() {",
			"-    assert_eq!(total(&[1, 2]), 3);",
			"+    assert!(total(&[1, 2]) > 0);",
			" }",
		)), "test_skip_added@new:3,test_expectation_relaxed@new:6"},
		{"js_assertions_removed", changed("src/cart.test.ts", hunk(10, 10,
			" it(\"totals\", () => {",
			"-  expect(total([1, 2])).toBe(3);",
			"-  expect(total([])).toBe(0);",
			"+  total([1, 2]);",
			" });",
		)), "test_assertion_removed@old:11"},
		{"js_assertion_moved", changed("src/cart.test.ts", hunk(10, 10,
			"-  expect(total([1, 2])).toBe(3);",
			" const x = 1;",
			"+  expect(total([1, 2])).toBe(3);",
		)), ""},
		{"go_test_case_removed", changed("cart/cart_test.go", hunk(20, 20,
			"-func TestTotal(t *testing.T) {",
			"-\tif Total(nil) != 0 {",
			"-\t\tt.Fatal(\"empty\")",
			"-\t}",
			"-}",
		)), "test_assertion_removed@old:22,test_case_removed@old:20"},
		{"python_case_removed", changed("tests/test_cart.py", hunk(5, 5,
			"-def test_total():",
			"-    assert total([1]) == 1",
		)), "test_assertion_removed@old:6,test_case_removed@old:5"},
		{"go_skip_added", changed("cart/cart_test.go", hunk(3, 3,
			" func TestTotal(t *testing.T) {",
			"+\tt.Skip(\"later\")",
		)), "test_skip_added@new:4"},
		{"go_skip_reindented_is_not_added", changed("cart/cart_test.go", hunk(3, 3,
			"-\tt.Skip(\"later\")",
			"+\t\tt.Skip(\"later\")",
		)), ""},
		{"js_skip_forms", changed("src/a.spec.js", hunk(1, 1,
			"+it.skip(\"a\", () => {});",
			"+xit(\"b\", () => {});",
			"+describe.todo(\"c\");",
			"+test.fixme(\"d\", () => {});",
			"+model.fit(data);",
		)), "test_skip_added@new:1,test_skip_added@new:2,test_skip_added@new:3,test_skip_added@new:4"},
		{"python_skip_forms", changed("tests/test_a.py", hunk(1, 1,
			"+@pytest.mark.skip(reason=\"flaky\")",
			"+@unittest.skip(\"x\")",
			"+    pytest.skip(\"y\")",
			"+    self.skipTest(\"z\")",
		)), "test_skip_added@new:1,test_skip_added@new:2,test_skip_added@new:3,test_skip_added@new:4"},
		{"js_focus", changed("src/a.test.tsx", hunk(1, 1,
			"+describe.only(\"cart\", () => {",
			"+  fit(\"x\", () => {});",
			"+  obj.fit(\"not a focus\");",
		)), "test_focus_added@new:1,test_focus_added@new:2"},
		{"js_expectation_relaxed", changed("src/a.test.ts", hunk(4, 4,
			"-  expect(total([1, 2])).toEqual(3);",
			"+  expect(total([1, 2])).toBeDefined();",
		)), "test_expectation_relaxed@new:4"},
		{"go_fatal_to_log", changed("a/a_test.go", hunk(7, 7,
			"-\t\tt.Fatalf(\"got %d\", got)",
			"+\t\tt.Logf(\"got %d\", got)",
		)), "test_assertion_removed@old:7,test_expectation_relaxed@new:7"},
		{"go_equality_to_ordering", changed("a/a_test.go", hunk(6, 6,
			"-\tif got := Clamp(50); got != 10 {",
			"+\tif got := Clamp(50); got < 10 {",
			" \t\tt.Fatalf(\"Clamp(50) = %d\", got)",
		)), "test_expectation_relaxed@new:6"},
		{"go_nil_check_is_not_exact", changed("a/a_test.go", hunk(6, 6,
			"-\tif err != nil {",
			"+\tif len(got) > 0 {",
		)), ""},
		{"go_channel_and_shift_are_not_orderings", changed("a/a_test.go", hunk(6, 6,
			"-\tif got != want {",
			"+\tif <-ch && v<<1 {",
		)), ""},
		{"python_equality_to_truthiness", changed("tests/test_a.py", hunk(3, 3,
			"-    self.assertEqual(total([1]), 1)",
			"+    self.assertTrue(total([1]))",
		)), "test_expectation_relaxed@new:3"},
		{"python_assert_ordering", changed("tests/test_a.py", hunk(3, 3,
			"-    assert total([1]) == 1",
			"+    assert total([1]) >= 1",
		)), "test_expectation_relaxed@new:3"},
		{"python_is_not_none", changed("tests/test_a.py", hunk(3, 3,
			"-    assert result == 42",
			"+    assert result is not None",
		)), "test_expectation_relaxed@new:3"},
		{"stricter_edit_is_quiet", changed("src/a.test.ts", hunk(4, 4,
			"-  expect(x).toBeTruthy();",
			"+  expect(x).toBe(true);",
		)), ""},
		{"unsupported_language", changed("src/test/CartTest.java", hunk(1, 1,
			"-    assertEquals(3, total());",
		)), ""},
		{"binary", model.ChangedFile{Path: "a_test.go", Status: "M", Binary: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signals := testWeakeningSignals(tc.file)
			if got := kinds(signals); got != tc.want {
				t.Fatalf("signals %s, want %s\n%+v", got, tc.want, signals)
			}
			for _, s := range signals {
				if !strings.HasPrefix(s.Evidence, weakeningPrefix) || s.Path != tc.file.Path || s.Summary == "" {
					t.Fatalf("signal %+v", s)
				}
				want := "medium"
				if s.Kind == model.SignalTestFocusAdded {
					want = "high"
				}
				if s.Severity != want {
					t.Fatalf("%s severity %s", s.Kind, s.Severity)
				}
				for _, word := range []string{"verified", "proof", "regression", "masked", "bug"} {
					if strings.Contains(strings.ToLower(s.Summary+" "+s.Evidence), word) && !strings.Contains(tc.file.Path, word) {
						t.Fatalf("signal wording uses %q: %+v", word, s)
					}
				}
			}
		})
	}
}

func TestTestWeakeningCaps(t *testing.T) {
	var lines []string
	for i := 0; i < maxSkipSignals+2; i++ {
		lines = append(lines, fmt.Sprintf("+\tt.Skip(\"reason %d\")", i))
	}
	for i := 0; i < maxFocusSignals+2; i++ {
		lines = append(lines, fmt.Sprintf("+it.only(\"case %d\", () => {});", i))
	}
	go1 := testWeakeningSignals(changed("a/a_test.go", hunk(1, 1, lines...)))
	skips := 0
	for _, s := range go1 {
		if s.Kind == model.SignalTestSkipAdded {
			skips++
		}
	}
	if skips != maxSkipSignals || !strings.Contains(go1[len(go1)-1].Evidence, "(and 2 more in this file)") {
		t.Fatalf("skip cap: %d signals, last %+v", skips, go1[len(go1)-1])
	}
	js := testWeakeningSignals(changed("a/a.test.js", hunk(1, 1, lines...)))
	focus := 0
	for _, s := range js {
		if s.Kind == model.SignalTestFocusAdded {
			focus++
		}
	}
	if focus != maxFocusSignals {
		t.Fatalf("focus cap: %d", focus)
	}
	var hunks []model.Hunk
	for i := 0; i < maxRelaxedSignals+3; i++ {
		hunks = append(hunks, hunk(10*i+1, 10*i+1, "-  expect(a).toBe(1);", "+  expect(a).toBeTruthy();"))
	}
	relaxed := testWeakeningSignals(changed("a/a.test.js", hunks...))
	if n := strings.Count(kinds(relaxed), model.SignalTestExpectationRelaxed); n != maxRelaxedSignals {
		t.Fatalf("relaxed cap: %d", n)
	}
	long := testWeakeningSignals(changed("a/a_test.go", hunk(1, 1, "+\tt.Skip(\""+strings.Repeat("é", 300)+"\")")))
	if len(long) != 1 || len(long[0].Evidence) > len(weakeningPrefix)+260 || !strings.HasSuffix(long[0].Evidence, "…") {
		t.Fatalf("long evidence %q", long[0].Evidence)
	}
}

// Through Analyze on a real repository: the signals appear on test files only,
// with stable IDs, and a focus marker is a high signal.
func TestTestWeakeningThroughAnalyze(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	put(t, dir, "clamp.go", "package clamp\n\nfunc Clamp(n int) int {\n\tif n > 10 {\n\t\treturn 10\n\t}\n\treturn n\n}\n")
	put(t, dir, "clamp_test.go", "package clamp\n\nimport \"testing\"\n\nfunc TestClampUpper(t *testing.T) {\n\tif got := Clamp(50); got != 10 {\n\t\tt.Fatalf(\"Clamp(50) = %d\", got)\n\t}\n}\n\nfunc TestClampInside(t *testing.T) {\n\tif got := Clamp(3); got != 3 {\n\t\tt.Fatalf(\"Clamp(3) = %d\", got)\n\t}\n}\n")
	put(t, dir, "web/cart.test.ts", "describe(\"cart\", () => {\n  it(\"totals\", () => {\n    expect(total([1, 2])).toBe(3);\n  });\n});\n")
	base := commit(t, dir)
	put(t, dir, "clamp.go", "package clamp\n\nfunc Clamp(n int) int {\n\treturn n\n}\n")
	// TestClampInside is removed, TestClampUpper gets a skip and a looser check.
	put(t, dir, "clamp_test.go", "package clamp\n\nimport \"testing\"\n\nfunc TestClampUpper(t *testing.T) {\n\tt.Skip(\"later\")\n\tif got := Clamp(50); got < 10 {\n\t\tt.Fatalf(\"Clamp(50) = %d\", got)\n\t}\n}\n")
	put(t, dir, "web/cart.test.ts", "describe.only(\"cart\", () => {\n  it(\"totals\", () => {\n    expect(total([1, 2])).toBeDefined();\n  });\n});\n")
	head := commit(t, dir)
	r, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	change, err := r.Analyze(context.Background(), base, head, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Analyze(context.Background(), r, change, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Analyze(context.Background(), r, change, nil)
	if kinds(first) != kinds(second) {
		t.Fatal("signals are not deterministic")
	}
	got := map[string][]model.Signal{}
	for i, s := range first {
		if s.ID != second[i].ID || !strings.HasPrefix(s.ID, "sig-") {
			t.Fatalf("unstable ID %s / %s", s.ID, second[i].ID)
		}
		got[s.Kind] = append(got[s.Kind], s)
		if strings.HasPrefix(s.Kind, "test_") && s.Path == "clamp.go" {
			t.Fatalf("test signal on a non-test file: %+v", s)
		}
	}
	for kind, path := range map[string]string{
		model.SignalTestExpectationRelaxed: "clamp_test.go",
		model.SignalTestCaseRemoved:        "clamp_test.go",
		model.SignalTestSkipAdded:          "clamp_test.go",
		model.SignalTestFocusAdded:         "web/cart.test.ts",
	} {
		found := false
		for _, s := range got[kind] {
			found = found || s.Path == path
		}
		if !found {
			t.Errorf("no %s on %s: %s", kind, path, kinds(first))
		}
	}
	for _, s := range got[model.SignalTestFocusAdded] {
		if s.Severity != "high" {
			t.Fatalf("focus severity %s", s.Severity)
		}
	}
	relaxedTS := false
	for _, s := range got[model.SignalTestExpectationRelaxed] {
		relaxedTS = relaxedTS || s.Path == "web/cart.test.ts"
	}
	if !relaxedTS {
		t.Errorf("toBe -> toBeDefined was not flagged: %s", kinds(first))
	}
}
