package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// intentDockerFixture builds a harness whose candidate adds the lines of
// candidate over files, records the change as added lines of the files that
// differ, and records the name of every container it launches.
func intentDockerFixture(t *testing.T, image string, files, candidate map[string]string, template []string) (*Harness, *[]string) {
	t.Helper()
	base, head := t.TempDir(), t.TempDir()
	write := func(dir string, set map[string]string) {
		for path, content := range set {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(base, files)
	write(head, files)
	write(head, candidate)
	change := model.Change{}
	for path, content := range candidate {
		f := model.ChangedFile{Path: path, Status: "M"}
		hunk := model.Hunk{}
		old := strings.Split(files[path], "\n")
		for i, l := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
			if i >= len(old) || old[i] != l {
				hunk.Lines = append(hunk.Lines, model.DiffLine{Kind: "add", NewLine: i + 1, Content: l})
			}
		}
		f.Hunks = []model.Hunk{hunk}
		change.Files = append(change.Files, f)
	}
	diff, _ := json.Marshal(change)
	h, err := New(Options{BaseDir: base, CandidateDir: head, ArtifactDir: t.TempDir(), Image: image, Timeout: 3 * time.Minute, MaxRuntime: 20 * time.Minute, MaxOutputBytes: 64 * 1024, MaxGeneratedTests: 12, Diff: string(diff),
		IntentCriteria: intentCriteria, Commands: map[string][]string{"generated_test": template}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	var names []string
	execute, capture := h.execute, h.executeCapture
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		names = append(names, name)
		if !contains(args, "type=bind,src="+h.candidate+",dst=/source,readonly") {
			t.Errorf("an intent test ran outside the candidate snapshot: %q", args)
		}
		return execute(ctx, name, args, out)
	}
	h.executeCapture = func(ctx context.Context, name string, args []string, log, payload io.Writer) execution {
		names = append(names, name)
		if !contains(args, "type=bind,src="+h.candidate+",dst=/source,readonly") {
			t.Errorf("an intent test ran outside the candidate snapshot: %q", args)
		}
		return capture(ctx, name, args, log, payload)
	}
	return h, &names
}

// noContainersLeft checks that none of the named containers still exists.
func noContainersLeft(t *testing.T, names []string) {
	t.Helper()
	for _, name := range names {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}").Output()
		if err != nil {
			t.Fatalf("docker ps: %v", err)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("container %s remains", name)
		}
	}
}

// createAndRunIntent creates one intent test for criterion and runs it.
func createAndRunIntent(t *testing.T, h *Harness, criterion, path, content string) intentResult {
	t.Helper()
	var created map[string]any
	if err := json.Unmarshal(call(t, h, IntentCreateTool, map[string]any{"criterion_id": criterion, "path": path, "content": content, "description": "intent test for " + criterion}), &created); err != nil {
		t.Fatal(err)
	}
	return runIntent(t, h, created["test_id"].(string))
}

const (
	goShopBase      = "package shop\n\nfunc Total(xs []int) int {\n\tsum := 0\n\tfor _, x := range xs {\n\t\tsum += x\n\t}\n\treturn sum\n}\n"
	goShopCandidate = goShopBase + "\nfunc Discount(total int) int {\n\tif total > 100 {\n\t\treturn total - 10\n\t}\n\treturn total\n}\n"
)

// A real go test -json run: an assertion failure of the intent test's own file
// that names the new Discount is INTENT_TEST_FAILED; a pass, a panic, a
// compile error, and a failure or a pass of a test that references no changed
// symbol are not.
func TestDockerIntentGoRunner(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	files := map[string]string{"go.mod": "module example.test/shop\n\ngo 1.23.0\n", "shop.go": goShopBase}
	h, names := intentDockerFixture(t, image, files, map[string]string{"shop.go": goShopCandidate}, []string{"go", "test", "{package}"})
	test := func(name, body string) string {
		return "package shop\n\nimport \"testing\"\n\nfunc " + name + "(t *testing.T) {\n" + body + "}\n"
	}
	for _, tc := range []struct {
		name, path, content, status, check, reason string
		symbols                                    []string
	}{
		{"assertion", "swiftproof_intent_ac1_test.go", test("TestIntentAC1", "\tif got := Discount(100); got != 90 {\n\t\tt.Errorf(\"Discount(100) = %d, want 90\", got)\n\t}\n"), model.StatusIntentTestFailed, "FAIL", "", []string{"Discount"}},
		{"pass", "swiftproof_intent_ac1b_test.go", test("TestIntentAC1Above", "\tif got := Discount(200); got != 190 {\n\t\tt.Fatalf(\"Discount(200) = %d\", got)\n\t}\n"), model.StatusIntentTestPassed, "PASS", notePassed, []string{"Discount"}},
		{"panic", "swiftproof_intent_panic_test.go", test("TestIntentPanic", "\tvar xs []int\n\t_ = Discount(xs[3])\n"), model.StatusUnverified, "FAIL", reasonNotAssertion, []string{"Discount"}},
		{"compile error", "swiftproof_intent_build_test.go", test("TestIntentBuild", "\t_ = Discount(100, 2)\n"), model.StatusUnverified, "ERROR", reasonInconclusive, []string{"Discount"}},
		{"no changed symbol", "swiftproof_intent_noref_test.go", test("TestIntentNoRef", "\tif got := Total([]int{1}); got != 2 {\n\t\tt.Fatalf(\"Total = %d\", got)\n\t}\n"), model.StatusUnverified, "FAIL", reasonNoReference, nil},
		{"pass without a changed symbol", "swiftproof_intent_norefpass_test.go", test("TestIntentNoRefPass", "\tif got := Total([]int{1}); got != 1 {\n\t\tt.Fatalf(\"Total = %d\", got)\n\t}\n"), model.StatusUnverified, "PASS", reasonNoReference, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := createAndRunIntent(t, h, "AC-1", tc.path, tc.content)
			e := result.Evidence
			if e.Status != tc.status || result.CandidateCheck.Status != tc.check || result.CandidateCheck.Kind != model.CheckGeneratedIntent || e.Runner != RunnerGo {
				t.Fatalf("evidence %+v\ncheck %s exit %d\n%s", e, result.CandidateCheck.Status, result.CandidateCheck.ExitCode, result.CandidateCheck.Output)
			}
			if tc.reason != "" && !strings.Contains(e.Description, tc.reason) {
				t.Fatalf("description %q lacks %q", e.Description, tc.reason)
			}
			if !reflect.DeepEqual(e.ReferencedSymbols, tc.symbols) && !(len(e.ReferencedSymbols) == 0 && len(tc.symbols) == 0) {
				t.Fatalf("symbols %q, want %q", e.ReferencedSymbols, tc.symbols)
			}
			// The recorded check re-derives the same status.
			c, _ := ValidateExecution(e.Runner, result.CandidateCheck, e.Path, e.TestNames)
			var change model.Change
			_ = json.Unmarshal([]byte(h.opts.Diff), &change)
			if status, _ := IntentOutcome(e.Runner, c, e.Path, e.TestNames, e.ReferencedSymbols, NewIntentWords(change)); status != e.Status {
				t.Fatalf("re-derived %s, stored %s", status, e.Status)
			}
		})
	}
	if _, retained := artifactByKind(h, model.ArtifactIntentTest); !retained {
		t.Fatal("the failing intent test was not retained")
	}
	for _, root := range []string{h.candidate, h.base} {
		matches, _ := filepath.Glob(filepath.Join(root, "swiftproof_intent*"))
		if len(matches) != 0 {
			t.Fatalf("intent tests left in %s: %q", root, matches)
		}
	}
	if len(*names) != 6 {
		t.Fatalf("%d containers, want one per intent test", len(*names))
	}
	noContainersLeft(t, *names)
}

const (
	tsShopBase      = "export function total(xs: number[]): number {\n  return xs.reduce((a, b) => a + b, 0);\n}\n"
	tsShopCandidate = tsShopBase + "\nexport function discount(total: number): number {\n  return total > 100 ? total - 10 : total;\n}\n"
)

// A real Vitest run (image SWIFTPROOF_TEST_TS_IMAGE): an expect() failure is
// INTENT_TEST_FAILED through the Jest-compatible report; a runtime error and a
// thrown Error are not.
func TestDockerTSIntentVitestRunner(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_TS_IMAGE to a preloaded image with vitest on PATH (for example swiftproof-ts-test:local)")
	}
	h, names := intentDockerFixture(t, image, map[string]string{"src/shop.ts": tsShopBase}, map[string]string{"src/shop.ts": tsShopCandidate}, []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"})
	assertion := "import { expect, test } from \"vitest\";\nimport { discount } from \"./shop\";\n\ntest(\"orders of 100 get 10 off\", () => {\n  expect(discount(100)).toBe(90);\n});\n"
	runtime := "import { expect, test } from \"vitest\";\nimport { discount } from \"./shop\";\n\ntest(\"calls a missing method\", () => {\n  const d = discount as any;\n  expect(d.missing()).toBe(1);\n});\n"
	result := createAndRunIntent(t, h, "AC-1", "src/shop.intent.test.ts", assertion)
	if e := result.Evidence; e.Status != model.StatusIntentTestFailed || e.Runner != RunnerJest || !reflect.DeepEqual(e.ReferencedSymbols, []string{"discount"}) || result.CandidateCheck.Status != "FAIL" {
		t.Fatalf("assertion: %+v\n%s\n%s", e, result.CandidateCheck.Output, result.CandidateCheck.Results)
	}
	result = createAndRunIntent(t, h, "AC-1", "src/shop.runtime.test.ts", runtime)
	if e := result.Evidence; e.Status != model.StatusUnverified || !strings.Contains(e.Description, reasonNotAssertion) || result.CandidateCheck.Status != "FAIL" {
		t.Fatalf("runtime error: %+v\n%s", e, result.CandidateCheck.Results)
	}
	// An Error thrown on the way (here by the test itself, as code under test
	// could) is not an assertion failure either.
	thrown := "import { test } from \"vitest\";\nimport { discount } from \"./shop\";\n\ntest(\"throws instead of asserting\", () => {\n  const got = discount(100);\n  if (got !== 90) throw new Error(\"got \" + got);\n});\n"
	result = createAndRunIntent(t, h, "AC-1", "src/shop.thrown.test.ts", thrown)
	if e := result.Evidence; e.Status != model.StatusUnverified || !strings.Contains(e.Description, reasonNotAssertion) || result.CandidateCheck.Status != "FAIL" || !strings.Contains(result.CandidateCheck.Results, "Error: got 100") {
		t.Fatalf("thrown error: %+v\n%s", e, result.CandidateCheck.Results)
	}
	noContainersLeft(t, *names)
}
