package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// intentCart is the candidate's pkg/cart.go: Discount is new.
const intentCart = "package pkg\n\nfunc Total(xs []int) int { return len(xs) }\n\nfunc Discount(total int) int {\n\tif total > 100 {\n\t\treturn total - 10\n\t}\n\treturn total\n}\n"

const intentTestPath = "pkg/probe_intent_ac1_test.go"

// intentGoTest references the changed Discount and fails through t.Errorf.
const intentGoTest = `package pkg

import "testing"

func TestIntentAC1(t *testing.T) {
	if got := Discount(100); got != 90 {
		t.Errorf("Discount(100) = %d, want 90", got)
	}
}
`

var intentCriteria = []model.IntentCriterion{{ID: "AC-1", Text: "Orders of 100 or more get 10 off", Line: 3}, {ID: "AC-2", Text: "Orders of 50 or more ship free", Line: 4}}

// cartChange is the recorded change of pkg/cart.go with the given candidate
// lines added.
func cartChange(added ...int) model.Change {
	lines := strings.Split(intentCart, "\n")
	f := model.ChangedFile{Path: "pkg/cart.go", Status: "M"}
	hunk := model.Hunk{}
	for _, n := range added {
		hunk.Lines = append(hunk.Lines, model.DiffLine{Kind: "add", NewLine: n, Content: lines[n-1]})
	}
	f.Hunks = []model.Hunk{hunk}
	return model.Change{Files: []model.ChangedFile{f}}
}

// intentFixture is a harness whose candidate adds Discount (lines 5-10 of
// pkg/cart.go, or the lines given) and whose intent has two criteria.
func intentFixture(t *testing.T, added ...int) *Harness {
	t.Helper()
	if len(added) == 0 {
		added = []int{5, 6, 7, 8, 9, 10}
	}
	src, base := t.TempDir(), t.TempDir()
	for dir, cart := range map[string]string{src: intentCart, base: "package pkg\n\nfunc Total(xs []int) int { return len(xs) }\n"} {
		if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pkg", "cart.go"), []byte(cart), 0644); err != nil {
			t.Fatal(err)
		}
	}
	diff, _ := json.Marshal(cartChange(added...))
	h, err := New(Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxGeneratedTests: 10, Diff: string(diff), IntentCriteria: intentCriteria,
		Commands: map[string][]string{"test": {"go", "test", "./..."}, "generated_test": {"go", "test", "{package}"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// goIntentEvents writes the go test -json events of one named test.
func goIntentEvents(w io.Writer, test, action string, outputs ...string) {
	event := func(fields map[string]string) {
		fields["Package"] = "example.test/pkg"
		b, _ := json.Marshal(fields)
		fmt.Fprintln(w, string(b))
	}
	event(map[string]string{"Action": "run", "Test": test})
	for _, o := range outputs {
		event(map[string]string{"Action": "output", "Test": test, "Output": o})
	}
	event(map[string]string{"Action": action, "Test": test})
}

type intentResult struct {
	Evidence       model.Evidence `json:"evidence"`
	CandidateCheck model.Check    `json:"candidate_check"`
	Note           string         `json:"note"`
}

func runIntent(t *testing.T, h *Harness, id string) intentResult {
	t.Helper()
	var result intentResult
	if err := json.Unmarshal(call(t, h, IntentRunTool, map[string]any{"test_id": id}), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIntentToolsRequireKnownCriterion(t *testing.T) {
	h := intentFixture(t)
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("nothing may run")
		return execution{}
	}
	for _, tc := range []struct{ args, want string }{
		{`{"criterion_id":"AC-9","path":"pkg/a_test.go","content":"x"}`, "unknown criterion_id"},
		{`{"criterion_id":"ac-1","path":"pkg/a_test.go","content":"x"}`, "unknown criterion_id"},
		{`{"criterion_id":"","path":"pkg/a_test.go","content":"x"}`, "unknown criterion_id"},
		{`{"criterion_id":"AC-1","path":"pkg/cart.go","content":"x"}`, "supported test filename"},
		{`{"criterion_id":"AC-1","path":"pkg/a_test.go","content":"package pkg\nfunc helper() {}\n"}`, "at least one runnable"},
	} {
		if _, err := h.Call(context.Background(), IntentCreateTool, json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.args, err, tc.want)
		}
	}
	if h.generated != 0 || h.intent.created != 0 || len(h.tests) != 0 {
		t.Fatalf("a refused call created a test: %d %d %d", h.generated, h.intent.created, len(h.tests))
	}
	// A criterion ID that occurs twice identifies nothing.
	h.opts.IntentCriteria = append(h.opts.IntentCriteria, model.IntentCriterion{ID: "AC-1", Text: "again", Line: 9})
	if _, err := h.Call(context.Background(), IntentCreateTool, json.RawMessage(`{"criterion_id":"AC-1","path":"pkg/a_test.go","content":"x"}`)); err == nil || !strings.Contains(err.Error(), "unknown criterion_id") {
		t.Fatalf("duplicated criterion accepted: %v", err)
	}
	if _, err := h.Call(context.Background(), IntentRunTool, json.RawMessage(`{"test_id":"intent-test-1"}`)); err == nil || !strings.Contains(err.Error(), "unknown intent test") {
		t.Fatalf("unknown test: %v", err)
	}
}

func TestIntentTestRunsOnCandidateOnly(t *testing.T) {
	h := intentFixture(t)
	calls := 0
	h.execute = func(_ context.Context, _ string, args []string, out io.Writer) execution {
		calls++
		if !contains(args, "type=bind,src="+h.candidate+",dst=/source,readonly") {
			t.Errorf("not the candidate snapshot: %q", args)
		}
		if _, err := os.Stat(filepath.Join(h.candidate, filepath.FromSlash(intentTestPath))); err != nil {
			t.Errorf("intent test not staged in the candidate: %v", err)
		}
		if _, err := os.Stat(filepath.Join(h.base, filepath.FromSlash(intentTestPath))); !os.IsNotExist(err) {
			t.Errorf("intent test staged in the base snapshot: %v", err)
		}
		want := []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestIntentAC1)$"}
		if got := args[len(args)-len(want):]; !reflect.DeepEqual(got, want) {
			t.Errorf("argv tail %q", got)
		}
		goIntentEvents(out, "TestIntentAC1", "fail", "=== RUN   TestIntentAC1\n", "    probe_intent_ac1_test.go:7: Discount(100) = 100, want 90\n", "--- FAIL: TestIntentAC1 (0.00s)\n")
		return execution{ExitCode: 1}
	}
	var created map[string]any
	if err := json.Unmarshal(call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": intentTestPath, "content": intentGoTest, "description": "100 gets 10 off"}), &created); err != nil {
		t.Fatal(err)
	}
	if created["test_id"] != "intent-test-1" || created["criterion_id"] != "AC-1" || created["criterion"] != intentCriteria[0].Text || created["runs_on"] != "candidate" {
		t.Fatalf("created %+v", created)
	}
	result := runIntent(t, h, "intent-test-1")
	if calls != 1 {
		t.Fatalf("%d executions, want exactly one candidate run", calls)
	}
	e := result.Evidence
	if e.Kind != model.EvidenceIntentTest || e.Status != model.StatusIntentTestFailed || e.BaseCheckID != "" || e.CriterionID != "AC-1" || e.Runner != RunnerGo ||
		!reflect.DeepEqual(e.TestNames, []string{"TestIntentAC1"}) || !reflect.DeepEqual(e.ReferencedSymbols, []string{"Discount"}) || e.Path != intentTestPath || e.CheckID != result.CandidateCheck.ID {
		t.Fatalf("evidence %+v", e)
	}
	if !strings.Contains(e.Description, "candidate-only intent test for AC-1; no baseline control") || result.Note != IntentNote {
		t.Fatalf("description %q note %q", e.Description, result.Note)
	}
	checks := h.Checks()
	if len(checks) != 1 || checks[0].Kind != model.CheckGeneratedIntent || checks[0].Status != "FAIL" {
		t.Fatalf("checks %+v", checks)
	}
	for _, root := range []string{h.candidate, h.base} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(intentTestPath))); !os.IsNotExist(err) {
			t.Fatalf("intent test left behind in %s", root)
		}
	}
	a, ok := artifactByKind(h, model.ArtifactIntentTest)
	sum := sha256.Sum256([]byte(intentGoTest))
	if !ok || a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("intent test not retained: %+v", h.Artifacts())
	}
	if _, err := h.Call(context.Background(), "delete_generated_test", json.RawMessage(`{"test_id":"intent-test-1"}`)); err == nil || !strings.Contains(err.Error(), "failed as an intent test") {
		t.Fatalf("a failing intent test was deleted: %v", err)
	}
	// A second run records a second record but retains the source once.
	runIntent(t, h, "intent-test-1")
	retained := 0
	for _, a := range h.Artifacts() {
		if a.Kind == model.ArtifactIntentTest {
			retained++
		}
	}
	if retained != 1 || len(h.Evidence()) != 2 {
		t.Fatalf("%d retained sources, %d records", retained, len(h.Evidence()))
	}
}

func TestIntentOutcomes(t *testing.T) {
	noReference := "package pkg\n\nimport \"testing\"\n\nfunc TestIntentAC1(t *testing.T) { t.Fatal(\"x\") }\n"
	assertion := "    probe_intent_ac1_test.go:7: Discount(100) = 100, want 90\n"
	for _, tc := range []struct {
		name    string
		content string
		added   []int
		emit    func(io.Writer) execution
		status  string
		check   string
		reason  string
	}{
		{"assertion failure", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", assertion)
			return execution{ExitCode: 1}
		}, model.StatusIntentTestFailed, "FAIL", ""},
		{"assertion in a subtest", intentGoTest, nil, func(w io.Writer) execution {
			fmt.Fprintln(w, `{"Action":"run","Package":"p","Test":"TestIntentAC1"}`)
			fmt.Fprintln(w, `{"Action":"output","Package":"p","Test":"TestIntentAC1/at_100","Output":"    probe_intent_ac1_test.go:9: got 100\n"}`)
			fmt.Fprintln(w, `{"Action":"fail","Package":"p","Test":"TestIntentAC1/at_100"}`)
			fmt.Fprintln(w, `{"Action":"fail","Package":"p","Test":"TestIntentAC1"}`)
			return execution{ExitCode: 1}
		}, model.StatusIntentTestFailed, "FAIL", ""},
		{"pass", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "pass")
			return execution{}
		}, model.StatusIntentTestPassed, "PASS", notePassed},
		{"panic", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", assertion, "panic: runtime error: index out of range [3] with length 0\n")
			return execution{ExitCode: 2}
		}, model.StatusUnverified, "FAIL", reasonNotAssertion},
		{"no message", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", "    probe_intent_ac1_test.go:7: \n")
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "FAIL", reasonNotAssertion},
		{"assertion line of another file", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", "    cart_test.go:7: from elsewhere\n")
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "FAIL", reasonNotAssertion},
		{"no changed symbol referenced", noReference, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", "    probe_intent_ac1_test.go:5: x\n")
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "FAIL", reasonNoReference},
		{"name not on an added line", intentGoTest, []int{7}, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", assertion)
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "FAIL", reasonNotOnAdded},
		{"setup failure", intentGoTest, nil, func(w io.Writer) execution {
			fmt.Fprintln(w, "# example.test/pkg\npkg/probe_intent_ac1_test.go:6:12: undefined: Discount\nFAIL\texample.test/pkg [build failed]")
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "ERROR", reasonInconclusive},
		{"pass without a changed symbol", "package pkg\n\nimport \"testing\"\n\nfunc TestIntentAC1(t *testing.T) { _ = Total(nil) }\n", nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "pass")
			return execution{}
		}, model.StatusUnverified, "PASS", reasonNoReference},
		{"skip", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "skip")
			return execution{}
		}, model.StatusUnverified, "ERROR", reasonInconclusive},
		{"unrelated test", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestOther", "fail", "    probe_intent_ac1_test.go:7: x\n")
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "ERROR", reasonInconclusive},
		{"timeout", intentGoTest, nil, func(w io.Writer) execution {
			return execution{ExitCode: -1, TimedOut: true}
		}, model.StatusUnverified, "TIMEOUT", reasonInconclusive},
		{"truncated", intentGoTest, nil, func(w io.Writer) execution {
			goIntentEvents(w, "TestIntentAC1", "fail", assertion)
			fmt.Fprint(w, strings.Repeat("x", 40*1024))
			return execution{ExitCode: 1}
		}, model.StatusUnverified, "ERROR", reasonInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := intentFixture(t, tc.added...)
			h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution { return tc.emit(out) }
			call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": intentTestPath, "content": tc.content})
			result := runIntent(t, h, "intent-test-1")
			e := result.Evidence
			if e.Status != tc.status || result.CandidateCheck.Status != tc.check || e.CheckID == "" || e.Runner != RunnerGo || len(e.TestNames) != 1 || e.CriterionID != "AC-1" || e.BaseCheckID != "" {
				t.Fatalf("evidence %+v, check %s", e, result.CandidateCheck.Status)
			}
			if tc.reason != "" && !strings.Contains(e.Description, tc.reason) || !strings.HasPrefix(e.Description, "(candidate-only intent test for AC-1; no baseline control)") {
				t.Fatalf("description %q lacks %q", e.Description, tc.reason)
			}
			if tc.status == model.StatusIntentTestFailed && len(e.ReferencedSymbols) == 0 {
				t.Fatal("a failure without referenced symbols")
			}
			_, retained := artifactByKind(h, model.ArtifactIntentTest)
			if retained != (tc.status == model.StatusIntentTestFailed) {
				t.Fatalf("retained %v for %s", retained, tc.status)
			}
			if tc.status != model.StatusIntentTestFailed {
				call(t, h, "delete_generated_test", map[string]any{"test_id": "intent-test-1"})
			}
		})
	}
}

func TestIntentRequiresVerifiedRunner(t *testing.T) {
	for _, template := range [][]string{{"go", "test", "./..."}, {"go", "test", "-C", "x", "{package}"}, {"make", "test"}} {
		h := intentFixture(t)
		h.opts.Commands["generated_test"] = template
		if _, err := h.Call(context.Background(), IntentCreateTool, json.RawMessage(`{"criterion_id":"AC-1","path":"pkg/probe_intent_ac1_test.go","content":`+strconvQuote(intentGoTest)+`}`)); err == nil || !strings.Contains(err.Error(), "named-test runner") {
			t.Fatalf("%q: %v", template, err)
		}
		if h.generated != 0 || h.intent.created != 0 || len(h.tests) != 0 {
			t.Fatalf("%q: a refused intent test was created", template)
		}
		out := call(t, h, "create_test", map[string]any{"path": "pkg/other_test.go", "content": generatedSource})
		if !strings.Contains(string(out), `"generated-test-1"`) {
			t.Fatalf("the refused intent test consumed an ID: %s", out)
		}
	}
	// A JavaScript test needs a Jest-compatible template.
	h := intentFixture(t)
	if _, err := h.Call(context.Background(), IntentCreateTool, json.RawMessage(`{"criterion_id":"AC-1","path":"src/a.test.ts","content":"test(\"x\", () => {})\n"}`)); err == nil || !strings.Contains(err.Error(), "named-test runner") {
		t.Fatalf("JS test without a verifiable template: %v", err)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestIntentBudgetSharedAndCapped(t *testing.T) {
	h := intentFixture(t)
	h.opts.MaxGeneratedTests = 4
	create := func(i int) error {
		content := strings.ReplaceAll(intentGoTest, "TestIntentAC1", fmt.Sprintf("TestIntent%d", i))
		_, err := h.Call(context.Background(), IntentCreateTool, json.RawMessage(fmt.Sprintf(`{"criterion_id":"AC-1","path":"pkg/intent%d_test.go","content":%s}`, i, strconvQuote(content))))
		return err
	}
	if err := create(1); err != nil {
		t.Fatal(err)
	}
	if err := create(2); err != nil {
		t.Fatal(err)
	}
	if err := create(3); err == nil || !strings.Contains(err.Error(), "intent test budget exhausted") {
		t.Fatalf("third intent test: %v", err)
	}
	// Deleting an intent test does not return its slot.
	call(t, h, "delete_generated_test", map[string]any{"test_id": "intent-test-2"})
	if err := create(4); err == nil || !strings.Contains(err.Error(), "intent test budget exhausted") {
		t.Fatalf("slot returned after deletion: %v", err)
	}
	// Intent tests used two of the four shared slots.
	call(t, h, "create_test", map[string]any{"path": "pkg/a_test.go", "content": strings.ReplaceAll(generatedSource, "TestProbe", "TestA")})
	call(t, h, "create_test", map[string]any{"path": "pkg/b_test.go", "content": strings.ReplaceAll(generatedSource, "TestProbe", "TestB")})
	if _, err := h.Call(context.Background(), "create_test", json.RawMessage(`{"path":"pkg/c_test.go","content":"package pkg\nimport \"testing\"\nfunc TestC(t *testing.T) {}\n"}`)); err == nil || !strings.Contains(err.Error(), "generated test budget exhausted") {
		t.Fatalf("shared budget: %v", err)
	}
	if maxIntentTests(10) != 5 || maxIntentTests(3) != 2 || maxIntentTests(1) != 1 || maxIntentTests(0) != 0 {
		t.Fatal("maxIntentTests")
	}
	// With a budget of 1, generated tests leave no room for an intent test.
	h = intentFixture(t)
	h.opts.MaxGeneratedTests = 1
	call(t, h, "create_test", map[string]any{"path": "pkg/a_test.go", "content": strings.ReplaceAll(generatedSource, "TestProbe", "TestA")})
	if err := create(5); err == nil || !strings.Contains(err.Error(), "generated test budget exhausted") {
		t.Fatalf("shared budget first: %v", err)
	}
}

func TestRunKindsAreNotInterchangeable(t *testing.T) {
	h := intentFixture(t)
	runs := 0
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		runs++
		return execution{}
	}
	call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": intentTestPath, "content": intentGoTest})
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	if _, err := h.Call(context.Background(), "run_generated_test", json.RawMessage(`{"test_id":"intent-test-1"}`)); err == nil || !strings.Contains(err.Error(), "use run_intent_test") {
		t.Fatalf("intent test ran differentially: %v", err)
	}
	if _, err := h.Call(context.Background(), IntentRunTool, json.RawMessage(`{"test_id":"generated-test-2"}`)); err == nil || !strings.Contains(err.Error(), "created by create_intent_test") {
		t.Fatalf("generated test ran as an intent test: %v", err)
	}
	if runs != 0 {
		t.Fatalf("%d runs", runs)
	}
}

// tsIntentFixture is a TypeScript candidate whose new discount function the
// intent test references, run through a Vitest template.
func tsIntentFixture(t *testing.T) *Harness {
	t.Helper()
	cart := "export function total(xs: number[]): number {\n  return xs.length;\n}\n\nexport function discount(total: number): number {\n  return total > 100 ? total - 10 : total;\n}\n"
	src, base := t.TempDir(), t.TempDir()
	for _, dir := range []string{src, base} {
		if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "src", "cart.ts"), []byte(cart), 0644); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(cart, "\n")
	change := model.Change{Files: []model.ChangedFile{{Path: "src/cart.ts", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{
		{Kind: "add", NewLine: 5, Content: lines[4]}, {Kind: "add", NewLine: 6, Content: lines[5]}, {Kind: "add", NewLine: 7, Content: lines[6]},
	}}}}}}
	diff, _ := json.Marshal(change)
	h, err := New(Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxGeneratedTests: 10, Diff: string(diff), IntentCriteria: intentCriteria,
		Commands: map[string][]string{"generated_test": vitestTemplate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

const tsIntentTest = `import { expect, test } from "vitest";
import { discount } from "./cart";

test("orders of 100 get 10 off", () => {
  expect(discount(100)).toBe(90);
});
`

func jestIntentResults(status, fileMessage string, failures ...string) string {
	assertion := map[string]any{"ancestorTitles": []string{}, "title": "orders of 100 get 10 off", "status": status}
	if len(failures) > 0 {
		assertion["failureMessages"] = failures
	}
	b, _ := json.Marshal(map[string]any{"testResults": []map[string]any{{"name": "/workspace/src/cart.intent.test.ts", "status": status, "message": fileMessage, "assertionResults": []any{assertion}}}})
	return string(b)
}

func TestIntentJestRunner(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		code          int
		status, check string
	}{
		{"assertion", jestIntentResults("failed", "", "AssertionError: expected 100 to be 90 // Object.is equality"), 1, model.StatusIntentTestFailed, "FAIL"},
		{"runtime error", jestIntentResults("failed", "", "TypeError: discount is not a function"), 1, model.StatusUnverified, "FAIL"},
		{"file-level error", jestIntentResults("failed", "Cannot find module ./cart", "Error: x"), 1, model.StatusUnverified, "FAIL"},
		{"empty message", jestIntentResults("failed", "", ""), 1, model.StatusUnverified, "FAIL"},
		{"pass", jestIntentResults("passed", ""), 0, model.StatusIntentTestPassed, "PASS"},
		{"missing report", "", 1, model.StatusUnverified, "ERROR"},
		// Only an assertion-error header counts as an assertion failure.
		{"thrown Error", jestIntentResults("failed", "", "Error: not implemented\n    at discount (/workspace/src/cart.ts:6:9)"), 1, model.StatusUnverified, "FAIL"},
		{"custom error class", jestIntentResults("failed", "", "CartError: negative total"), 1, model.StatusUnverified, "FAIL"},
		{"timeout", jestIntentResults("failed", "", "Error: Test timed out in 5000ms."), 1, model.StatusUnverified, "FAIL"},
		{"thrown value", jestIntentResults("failed", "", "thrown: \"x\""), 1, model.StatusUnverified, "FAIL"},
		{"assertion and a thrown error", jestIntentResults("failed", "", "AssertionError: expected 100 to be 90", "Error: cleanup failed"), 1, model.StatusUnverified, "FAIL"},
		{"Jest matcher", jestIntentResults("failed", "", "Error: expect(received).toBe(expected) // Object.is equality\n\nExpected: 90\nReceived: 100"), 1, model.StatusIntentTestFailed, "FAIL"},
		{"colored Jest matcher", jestIntentResults("failed", "", "Error: \x1b[2mexpect(\x1b[22m\x1b[31mreceived\x1b[39m\x1b[2m).\x1b[22mtoBe\x1b[2m(\x1b[22m\x1b[32mexpected\x1b[39m\x1b[2m)\x1b[22m"), 1, model.StatusIntentTestFailed, "FAIL"},
		{"expect.assertions", jestIntentResults("failed", "", "Error: expect.assertions(1)\n\nExpected one assertion to be called but received zero assertion calls."), 1, model.StatusIntentTestFailed, "FAIL"},
		{"node assert", jestIntentResults("failed", "", "AssertionError [ERR_ASSERTION]: 100 == 90"), 1, model.StatusIntentTestFailed, "FAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tsIntentFixture(t)
			runs := 0
			h.executeCapture = func(_ context.Context, _ string, args []string, _, payload io.Writer) execution {
				runs++
				if !contains(args, "type=bind,src="+h.candidate+",dst=/source,readonly") || !contains(args, "src/cart.intent.test.ts") || !contains(args, "--outputFile="+ResultsPath) {
					t.Errorf("argv %q", args)
				}
				if tc.payload != "" {
					fmt.Fprint(payload, coverageFrame(tc.payload))
				}
				return execution{ExitCode: tc.code}
			}
			h.execute = func(context.Context, string, []string, io.Writer) execution {
				t.Fatal("a verifiable JS intent test ran without the results channel")
				return execution{}
			}
			call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": "src/cart.intent.test.ts", "content": tsIntentTest})
			result := runIntent(t, h, "intent-test-1")
			e := result.Evidence
			if runs != 1 || e.Status != tc.status || result.CandidateCheck.Status != tc.check || e.Runner != RunnerJest || !reflect.DeepEqual(e.TestNames, []string{"orders of 100 get 10 off"}) {
				t.Fatalf("runs %d evidence %+v check %s", runs, e, result.CandidateCheck.Status)
			}
			if !reflect.DeepEqual(e.ReferencedSymbols, []string{"discount"}) || !strings.Contains(e.Description, noteLexicalSymbols) {
				t.Fatalf("symbols %q description %q", e.ReferencedSymbols, e.Description)
			}
		})
	}
}

func TestIntentAuditStripsContent(t *testing.T) {
	h := intentFixture(t)
	call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": intentTestPath, "content": intentGoTest + "// SECRETMARKER\n"})
	found := false
	for _, e := range h.Audit() {
		if e.Tool == IntentCreateTool {
			found = true
			if strings.Contains(e.Arguments, "SECRETMARKER") || strings.Contains(e.Arguments, "content") || !strings.Contains(e.Arguments, "AC-1") {
				t.Fatalf("audit %q", e.Arguments)
			}
		}
	}
	if !found {
		t.Fatal("create_intent_test not audited")
	}
}

func TestIntentToolDefinitions(t *testing.T) {
	defs := intentToolDefinitions()
	var names []string
	for _, d := range defs {
		fn := d["function"].(map[string]any)
		names = append(names, fn["name"].(string))
		params := fn["parameters"].(map[string]any)
		required := params["required"].([]string)
		if !sort.StringsAreSorted(required) || params["additionalProperties"] != false {
			t.Fatalf("%s parameters %+v", fn["name"], params)
		}
		if word := regexp.MustCompile(`(?i)\b(satisfied|verified|contradicts?|met|correct|tested)\b`).FindString(fn["description"].(string)); word != "" {
			t.Errorf("%s description says %q", fn["name"], word)
		}
	}
	if !reflect.DeepEqual(names, []string{IntentCreateTool, IntentRunTool}) {
		t.Fatalf("names %q", names)
	}
	create := defs[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if fmt.Sprint(create["required"]) != "[content criterion_id path]" || create["properties"].(map[string]any)["criterion_id"].(map[string]any)["pattern"] != `^AC-[1-9][0-9]{0,2}$` {
		t.Fatalf("create_intent_test parameters %+v", create)
	}
	for _, name := range names {
		if !IsIntentTool(name) || !knownTools[name] {
			t.Fatalf("%s not dispatched", name)
		}
	}
}

func TestIntentOutcomeRules(t *testing.T) {
	fail := model.Check{ID: "check-1", Kind: model.CheckGeneratedIntent, Status: "FAIL", ExitCode: 1}
	var b strings.Builder
	goIntentEvents(&b, "TestIntentAC1", "fail", "    probe_intent_ac1_test.go:7: Discount(100) = 100, want 90\n")
	fail.Output = b.String()
	names := []string{"TestIntentAC1"}
	change := cartChange(5)
	if status, _ := IntentOutcome(RunnerGo, fail, intentTestPath, names, []string{"Discount"}, NewIntentWords(change)); status != model.StatusIntentTestFailed {
		t.Fatalf("baseline case %s", status)
	}
	for _, tc := range []struct {
		name    string
		check   func(model.Check) model.Check
		symbols []string
		change  func(model.Change) model.Change
		runner  string
	}{
		{name: "exit 125", check: func(c model.Check) model.Check { c.ExitCode = 125; return c }},
		{name: "exit 0 FAIL", check: func(c model.Check) model.Check { c.ExitCode = 0; return c }},
		{name: "truncated", check: func(c model.Check) model.Check { c.Truncated = true; return c }},
		{name: "unknown runner", runner: "pytest"},
		{name: "no symbols", symbols: []string{}},
		{name: "non-identifier symbol", symbols: []string{"Discount", "1bad"}},
		{name: "redaction-shaped symbol", symbols: []string{"Discount", "ghp_abcdefgh1234"}},
		{name: "too many symbols", symbols: append([]string{"Discount"}, strings.Split(strings.Repeat("x,", MaxReferencedSymbols), ",")...)},
		{name: "symbol not on an added line", symbols: []string{"Total"}},
		{name: "test file", change: func(c model.Change) model.Change { c.Files[0].Path = "pkg/cart_test.go"; return c }},
		{name: "deleted file", change: func(c model.Change) model.Change { c.Files[0].Status = "D"; return c }},
		{name: "binary file", change: func(c model.Change) model.Change { c.Files[0].Binary = true; return c }},
		{name: "secret-bearing file", change: func(c model.Change) model.Change { c.Files[0].Path = "config/.env.go"; return c }},
		{name: "path altered by redaction", change: func(c model.Change) model.Change { c.Files[0].Path = "password=hunter22/cart.go"; return c }},
		{name: "removed line", change: func(c model.Change) model.Change { c.Files[0].Hunks[0].Lines[0].Kind = "delete"; return c }},
	} {
		check, symbols, ch, runner := fail, []string{"Discount"}, cartChange(5), RunnerGo
		if tc.check != nil {
			check = tc.check(check)
		}
		if tc.symbols != nil {
			symbols = tc.symbols
		}
		if tc.change != nil {
			ch = tc.change(ch)
		}
		if tc.runner != "" {
			runner = tc.runner
		}
		if status, reason := IntentOutcome(runner, check, intentTestPath, names, symbols, NewIntentWords(ch)); status != model.StatusUnverified || reason == "" {
			t.Errorf("%s: %s %q", tc.name, status, reason)
		}
	}
	// A pass needs a referenced changed declaration, but not one named on an
	// added line (the word set is not read).
	pass := model.Check{Status: "PASS"}
	if status, _ := IntentOutcome(RunnerGo, pass, intentTestPath, names, []string{"Total"}, nil); status != model.StatusIntentTestPassed {
		t.Fatal("pass")
	}
	for _, symbols := range [][]string{nil, {}, {"1bad"}, {"Total", "ghp_abcdefgh1234"}} {
		if status, reason := IntentOutcome(RunnerGo, pass, intentTestPath, names, symbols, nil); status != model.StatusUnverified || reason != reasonNoReference {
			t.Fatalf("pass with symbols %q: %s %q", symbols, status, reason)
		}
	}
	if status, _ := IntentOutcome(RunnerGo, model.Check{Status: "PASS", Truncated: true}, intentTestPath, names, []string{"Total"}, nil); status != model.StatusUnverified {
		t.Fatal("truncated pass")
	}
	// A whole-word match only.
	renamed := cartChange(5)
	renamed.Files[0].Hunks[0].Lines[0].Content = "func DiscountRate(total int) int {"
	if IntentSymbolsNamed(renamed, []string{"Discount"}) {
		t.Fatal("a prefix matched")
	}
}

// The changed declarations and the linking rule's word set are computed once
// per harness, so the host-side cost of any number of intent-test runs stays
// linear in the size of the diff, and an ended context computes nothing.
func TestIntentLinkingIsComputedOnce(t *testing.T) {
	h := intentFixture(t)
	h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
		goIntentEvents(out, "TestIntentAC1", "fail", "    probe_intent_ac1_test.go:7: Discount(100) = 100, want 90\n")
		return execution{ExitCode: 1}
	}
	call(t, h, IntentCreateTool, map[string]any{"criterion_id": "AC-1", "path": intentTestPath, "content": intentGoTest})
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	h.mu.Lock()
	symbols, _ := h.referencedSymbols(ended, h.tests["intent-test-1"])
	words, decls := h.intent.words, h.intent.decls
	h.mu.Unlock()
	if len(symbols) != 0 || words != nil || len(decls) != 0 {
		t.Fatalf("an ended context computed symbols %q, words %v, declarations %v", symbols, words != nil, decls)
	}
	if e := runIntent(t, h, "intent-test-1").Evidence; e.Status != model.StatusIntentTestFailed {
		t.Fatalf("first run %+v", e)
	}
	h.mu.Lock()
	words, decls = h.intent.words, h.intent.decls
	h.mu.Unlock()
	if words == nil || words.words == nil || !reflect.DeepEqual(decls["go"], []string{"Discount"}) {
		t.Fatalf("words %v, declarations %v", words, decls)
	}
	if e := runIntent(t, h, "intent-test-1").Evidence; e.Status != model.StatusIntentTestFailed {
		t.Fatalf("second run %+v", e)
	}
	h.mu.Lock()
	reused := h.intent.words == words
	h.mu.Unlock()
	if !reused {
		t.Fatal("the word set was rebuilt")
	}
}

func TestIntentTestFileNames(t *testing.T) {
	for path, want := range map[string]bool{
		"pkg/cart_test.go": true, "src/cart.test.ts": true, "src/cart.spec.tsx": true, "src/cart.test.jsx": true, "src/cart.spec.jsx": true,
		"src/cart.test.mjs": true, "src/cart.test.cjs": true, "src/cart.test.mts": true, "src/cart.spec.mts": true, "src/cart.test.cts": true,
		"src/__tests__/cart.ts": true, "__tests__/helpers.js": true, "SRC/Cart.Test.MJS": true,
		"pkg/cart.go": false, "src/cart.ts": false, "src/cart.mjs": false, "src/testing.ts": false, "src/contest.ts": false, "src/__tests__.ts": false, "docs/cart.test.md": false,
	} {
		if got := intentTestFile(path); got != want {
			t.Errorf("intentTestFile(%q) = %v", path, got)
		}
	}
	// Neither an added line nor a declaration of such a file links an intent
	// test to the change.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	line := "export function discount(total) { return total; }"
	for _, name := range []string{"cart.test.mjs", "cart.mjs"} {
		if err := os.WriteFile(filepath.Join(src, "src", name), []byte(line+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	file := func(path string) model.ChangedFile {
		return model.ChangedFile{Path: path, Status: "A", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "add", NewLine: 1, Content: line}}}}}
	}
	for _, tc := range []struct {
		path string
		want []string
	}{{"src/cart.test.mjs", nil}, {"src/cart.mjs", []string{"discount"}}} {
		change := model.Change{Files: []model.ChangedFile{file(tc.path)}}
		if IntentSymbolsNamed(change, []string{"discount"}) != (tc.want != nil) {
			t.Errorf("%s: linking rule", tc.path)
		}
		diff, _ := json.Marshal(change)
		h, err := New(Options{CandidateDir: src, BaseDir: t.TempDir(), ArtifactDir: t.TempDir(), Image: "test-image:local", MaxGeneratedTests: 10, Diff: string(diff), IntentCriteria: intentCriteria})
		if err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		decls, ok := h.changedDeclarations(context.Background(), "js")
		h.mu.Unlock()
		h.Close()
		if !ok || !reflect.DeepEqual(decls, tc.want) && !(len(decls) == 0 && tc.want == nil) {
			t.Errorf("%s: declarations %q", tc.path, decls)
		}
	}
}
