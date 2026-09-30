package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/mutation"
)

// jsResult is one assertion result of a fake Jest-compatible report.
type jsResult struct {
	ancestors []string
	title     string
	status    string
}

// jsReportFor renders a Jest-compatible report with one file entry.
func jsReportFor(file string, results ...jsResult) string {
	assertions := []map[string]any{}
	for _, r := range results {
		ancestors := r.ancestors
		if ancestors == nil {
			ancestors = []string{}
		}
		assertions = append(assertions, map[string]any{"ancestorTitles": ancestors, "title": r.title, "status": r.status, "failureMessages": []string{}})
	}
	b, _ := json.Marshal(map[string]any{"testResults": []map[string]any{{"name": "/workspace/" + file, "status": "passed", "assertionResults": assertions}}})
	return string(b)
}

// jsFrame wraps a report as the capture wrapper frames it.
func jsFrame(report string) string {
	return fmt.Sprintf("%s%d\n%s%s", coverage.FrameHeader, len(report), report, coverage.FrameFooter)
}

func TestNormalizeJSTitleMatchesTheIndex(t *testing.T) {
	for in, want := range map[string]string{
		"discounts":              "discounts",
		"  two   spaces\there ":  "two spaces here",
		"$5 off":                 "5 off",
		"":                       "(untitled)",
		strings.Repeat("x", 250): strings.Repeat("x", 200),
	} {
		if got := NormalizeJSTitle(in); got != want {
			t.Errorf("NormalizeJSTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := JSTestName([]string{"price", " nested "}, "zero"); got != "price > nested > zero" {
		t.Fatalf("JSTestName = %q", got)
	}
}

func TestJestTestOutcome(t *testing.T) {
	report := jsReportFor("src/price.test.ts",
		jsResult{[]string{"price"}, "discounts", "passed"},
		jsResult{[]string{"price", "nested"}, "zero", "failed"},
		jsResult{nil, "top", "pending"},
		jsResult{nil, "todo", "todo"},
		jsResult{nil, "twice", "passed"},
		jsResult{nil, "twice", "passed"},
		jsResult{[]string{"spaced   out"}, "a  b", "passed"},
	)
	for name, want := range map[string]string{
		"price > discounts":     "pass",
		"price > nested > zero": "fail",
		"top":                   "skip",
		"todo":                  "skip",
		"twice":                 "",
		"spaced out > a b":      "pass",
		"discounts":             "",
		"price > missing":       "",
	} {
		if got := JestTestOutcome(report, "src/price.test.ts", name); got != want {
			t.Errorf("outcome of %q = %q, want %q", name, got, want)
		}
	}
	if got := JestTestOutcome(report, "src/other.test.ts", "top"); got != "" {
		t.Fatalf("a result of another file was read: %q", got)
	}
	twice := `{"testResults":[` + strings.TrimSuffix(strings.TrimPrefix(report, `{"testResults":[`), `]}`) + "," + strings.TrimSuffix(strings.TrimPrefix(report, `{"testResults":[`), `]}`) + `]}`
	if got := JestTestOutcome(twice, "src/price.test.ts", "top"); got != "" {
		t.Fatalf("a file with two report entries gave %q", got)
	}
	if JestTestOutcome("not json", "src/price.test.ts", "top") != "" || JestTestOutcome("", "src/price.test.ts", "top") != "" {
		t.Fatal("an unreadable report gave an outcome")
	}
}

func TestClassifyExistingJestTest(t *testing.T) {
	const file = "src/price.test.ts"
	command := []string{"vitest", "run", file, "--reporter=json", "--outputFile=" + ResultsPath, "-t", "^(?:top)$"}
	check := func(status string, exit int, results string) model.Check {
		return model.Check{Status: status, ExitCode: exit, Command: command, Results: results}
	}
	pass := jsReportFor(file, jsResult{nil, "top", "passed"})
	fail := jsReportFor(file, jsResult{nil, "top", "failed"})
	cases := []struct {
		name            string
		base, candidate model.Check
		want            string
	}{
		{"fails", check("PASS", 0, pass), check("FAIL", 1, fail), model.StatusFailsOnCandidate},
		{"passes", check("PASS", 0, pass), check("PASS", 0, pass), model.StatusPassesOnCandidate},
		{"pass inside a failed run", check("PASS", 0, pass), check("FAIL", 1, pass), model.StatusUnverified},
		{"skipped on baseline", check("PASS", 0, jsReportFor(file, jsResult{nil, "top", "skipped"})), check("FAIL", 1, fail), model.StatusUnverified},
		{"baseline failed", check("FAIL", 1, fail), check("FAIL", 1, fail), model.StatusUnverified},
		{"candidate exit outside the failure range", check("PASS", 0, pass), check("FAIL", 125, fail), model.StatusUnverified},
		{"candidate timeout", check("PASS", 0, pass), check("TIMEOUT", -1, ""), model.StatusUnverified},
		{"candidate without report", check("PASS", 0, pass), check("FAIL", 1, ""), model.StatusUnverified},
		{"truncated candidate", check("PASS", 0, pass), model.Check{Status: "FAIL", ExitCode: 1, Command: command, Results: fail, Truncated: true}, model.StatusUnverified},
		{"command mismatch", check("PASS", 0, pass), model.Check{Status: "FAIL", ExitCode: 1, Command: command[:3], Results: fail}, model.StatusUnverified},
	}
	for _, tc := range cases {
		if got, _ := ClassifyExistingJestTest(tc.base, tc.candidate, file, "top"); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	suite := `{"testResults":[{"name":"/workspace/src/price.test.ts","status":"failed","message":"Cannot find module","assertionResults":[]}]}`
	if _, reason := ClassifyExistingJestTest(check("PASS", 0, pass), check("FAIL", 1, suite), file, "top"); reason != reasonJestCandidateSuite {
		t.Fatalf("suite failure reason %q", reason)
	}
}

// The -t filter selects a test by its titles joined by white space; it is a
// JavaScript regular expression, anchored, with every title quoted.
func TestSelectJSTests(t *testing.T) {
	got := selectJSTests([]string{"vitest", "run", "a.test.ts"}, []string{"price > nested > zero", "costs (x+1) $"})
	want := `^(?:price\s+nested\s+zero|costs\s+\(x\+1\)\s+\$)$`
	if len(got) != 5 || got[3] != "-t" || got[4] != want {
		t.Fatalf("command %q, want the filter %s", got, want)
	}
	re := regexp.MustCompile(want)
	for _, full := range []string{"price nested zero", "costs (x+1) $", "price  nested\tzero"} {
		if !re.MatchString(full) {
			t.Errorf("filter does not select %q", full)
		}
	}
	if re.MatchString("price nested zero extra") || re.MatchString("xprice nested zero") {
		t.Fatal("filter is not anchored")
	}
}

func TestScriptTestPathAndName(t *testing.T) {
	for p, want := range map[string]bool{
		"src/a.test.ts": true, "src/a.spec.jsx": true, "src/__tests__/a.ts": true, "lib/a.test.mjs": true,
		"src/a.ts": false, "a_test.go": false, "node_modules/x/a.test.js": false, "types/a.test.d.ts": false,
	} {
		if ScriptTestPath(p) != want {
			t.Errorf("ScriptTestPath(%q) = %v", p, !want)
		}
	}
	for name, want := range map[string]bool{"price > zero": true, "top": true, "": false, "a >  > b": false, "a\nb": false, " > b": false} {
		if ValidJSTestName(name) != want {
			t.Errorf("ValidJSTestName(%q) = %v", name, !want)
		}
	}
}

// jsImpactedTrees: src/price.ts changed, the test file unchanged.
func jsImpactedTrees() (base, candidate map[string]string) {
	test := "import { test, expect, describe } from \"vitest\";\nimport { price } from \"./price\";\ndescribe(\"price\", () => {\n  test(\"discounts\", () => { expect(price(100, 10)).toBe(90); });\n});\ntest(\"top\", () => { expect(price(1, 0)).toBe(1); });\n"
	base = map[string]string{"src/price.ts": "export const price = (t: number, p: number) => t - (t * p) / 100;\n", "src/price.test.ts": test, "package.json": "{}\n"}
	candidate = map[string]string{"src/price.ts": "export const price = (t: number, p: number) => t;\n", "src/price.test.ts": test, "package.json": "{}\n"}
	return base, candidate
}

var jsImpactedTemplate = []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}

// fakeJSImpacted answers each run with a report that holds the tests its -t
// filter selects: on the baseline every test passes; on the candidate the
// tests in failing fail. A run fails when one of its tests fails.
func fakeJSImpacted(t *testing.T, h *Harness, failing map[string]bool) *[][]string {
	t.Helper()
	var argvs [][]string
	tests := []jsResult{{[]string{"price"}, "discounts", ""}, {nil, "top", ""}}
	h.executeCapture = func(_ context.Context, _ string, args []string, log, payload io.Writer) execution {
		argvs = append(argvs, append([]string(nil), args...))
		filter := ""
		for i, a := range args {
			if a == "-t" && i+1 < len(args) {
				filter = args[i+1]
			}
		}
		re := regexp.MustCompile(filter)
		var results []jsResult
		exit := 0
		for _, r := range tests {
			full := strings.Join(append(append([]string{}, r.ancestors...), r.title), " ")
			if !re.MatchString(full) {
				results = append(results, jsResult{r.ancestors, r.title, "skipped"})
				continue
			}
			status := "passed"
			if sideOf(h, args) == "candidate" && failing[JSTestName(r.ancestors, r.title)] {
				status, exit = "failed", 1
			}
			results = append(results, jsResult{r.ancestors, r.title, status})
		}
		fmt.Fprint(log, "vitest log\n")
		fmt.Fprint(payload, jsFrame(jsReportFor("src/price.test.ts", results...)))
		return execution{ExitCode: exit}
	}
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a TS/JS impacted run went without the payload channel")
		return execution{}
	}
	return &argvs
}

func jsSelected() []model.ImpactTest {
	return []model.ImpactTest{
		{Name: "price > discounts", Path: "src/price.test.ts", Line: 4, Package: "src", Depth: 1, Resolution: model.ResolutionName},
		{Name: "top", Path: "src/price.test.ts", Line: 6, Package: "src", Depth: 1, Resolution: model.ResolutionName},
	}
}

// One test fails on the candidate; its sibling passed inside that failed run
// and gets a pair of its own, as with go test.
func TestRunImpactedTestsJest(t *testing.T) {
	base, candidate := jsImpactedTrees()
	h := itHarness(t, base, candidate, jsImpactedTemplate)
	argvs := fakeJSImpacted(t, h, map[string]bool{"price > discounts": true})
	res := h.RunImpactedTests(context.Background(), jsSelected())
	if res.Status != model.ImpactTestsRan || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if res.Tests[0].Status != model.StatusFailsOnCandidate || res.Tests[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("tests %+v", res.Tests)
	}
	if len(*argvs) != 4 {
		t.Fatalf("%d runs, want a pair for both tests and a pair for top alone", len(*argvs))
	}
	first := strings.Join((*argvs)[0], " ")
	if !strings.Contains(first, "vitest run src/price.test.ts --reporter=json --outputFile="+ResultsPath+` -t ^(?:price\s+discounts|top)$`) {
		t.Fatalf("first command %s", first)
	}
	if last := (*argvs)[3]; last[len(last)-1] != `^(?:top)$` {
		t.Fatalf("retry filter %q", last)
	}
	for i, test := range res.Tests {
		var e *model.Evidence
		for _, ev := range h.Evidence() {
			if ev.ID == test.EvidenceID {
				e = &ev
			}
		}
		if e == nil || e.Kind != model.EvidenceImpactedTestDifferential || e.Runner != RunnerJest || e.Path != "src/price.test.ts" || len(e.TestNames) != 1 || e.TestNames[0] != test.Name {
			t.Fatalf("test %d evidence %+v", i, e)
		}
		if !strings.Contains(e.Description, "test file src/price.test.ts") {
			t.Fatalf("description %q", e.Description)
		}
		for _, id := range []string{e.BaseCheckID, e.CheckID} {
			if c := itCheck(t, h, id); c.Results == "" {
				t.Fatalf("check %s holds no report", id)
			}
		}
	}
}

// A Go test with a Vitest template, and a TS test with a go test template,
// are not run; a command that verifies nothing runs nothing.
func TestRunImpactedTestsRunnerMismatch(t *testing.T) {
	base, candidate := jsImpactedTrees()
	h := itHarness(t, base, candidate, jsImpactedTemplate)
	fakeJSImpacted(t, h, nil)
	res := h.RunImpactedTests(context.Background(), []model.ImpactTest{{Name: "TestA", Path: "pkg/a_test.go", Depth: 1}})
	if res.Tests[0].Reason != "not run: not a TypeScript or JavaScript test of a TypeScript or JavaScript test file" {
		t.Fatalf("Go test with a Vitest template: %+v", res.Tests[0])
	}
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	res = h.RunImpactedTests(context.Background(), jsSelected())
	if res.Tests[0].Reason != "not run: not a Go test function of a Go test file" {
		t.Fatalf("TS test with a go test template: %+v", res.Tests[0])
	}
	h = itHarness(t, base, candidate, []string{"npm", "test", "{file}", "--outputFile={results_out}"})
	res = h.RunImpactedTests(context.Background(), jsSelected())
	if res.Status != model.ImpactTestsNotRun || res.Reason != impactedTemplateReason {
		t.Fatalf("npm template: %+v", res)
	}
}

// A test file that differs between the snapshots is the subject of
// --base-tests, never of this stage.
func TestRunImpactedTestsJestChangedFile(t *testing.T) {
	base, candidate := jsImpactedTrees()
	candidate["src/price.test.ts"] += "// edited\n"
	h := itHarness(t, base, candidate, jsImpactedTemplate)
	fakeJSImpacted(t, h, nil)
	res := h.RunImpactedTests(context.Background(), jsSelected())
	if res.Status != model.ImpactTestsNotRun || !strings.Contains(res.Tests[0].Reason, "differs between the baseline and candidate snapshots") {
		t.Fatalf("result %+v", res)
	}
}

// The baseline version of a modified TS test runs on the baseline tree and on
// the candidate tree with that test file (and its snapshot file) reverted:
// the hybrid run sees the baseline test and the candidate code.
func TestRunBaseTestsJest(t *testing.T) {
	baseTest := "import { test, expect } from \"vitest\";\nimport { price } from \"./price\";\ntest(\"top\", () => { expect(price(1, 0)).toBe(1); });\n"
	candTest := strings.Replace(baseTest, "toBe(1)", "toBe(2)", 1)
	base := map[string]string{"src/price.ts": "export const price = (t: number, p: number) => t;\n", "src/price.test.ts": baseTest, "src/__snapshots__/price.test.ts.snap": "base snapshot\n"}
	candidate := map[string]string{"src/price.ts": "export const price = (t: number, p: number) => t + 1;\n", "src/price.test.ts": candTest, "src/__snapshots__/price.test.ts.snap": "candidate snapshot\n", "src/other.test.ts": "x\n"}
	h := itHarness(t, base, candidate, jsImpactedTemplate)
	var hybridSeen map[string]string
	h.executeCapture = func(_ context.Context, _ string, args []string, log, payload io.Writer) execution {
		side := sideOf(h, args)
		status, exit := "passed", 0
		if side == "unknown" {
			// The hybrid tree: read what the sandbox would see.
			dir := itMountDir(args)
			hybridSeen = map[string]string{}
			for _, p := range []string{"src/price.test.ts", "src/__snapshots__/price.test.ts.snap", "src/price.ts", "src/other.test.ts"} {
				b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
				if err == nil {
					hybridSeen[p] = string(b)
				}
			}
			status, exit = "failed", 1
		}
		fmt.Fprint(log, "vitest log\n")
		fmt.Fprint(payload, jsFrame(jsReportFor("src/price.test.ts", jsResult{nil, "top", status})))
		return execution{ExitCode: exit}
	}
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a TS/JS base-test run went without the payload channel")
		return execution{}
	}
	selected := []model.BaseTest{{Name: "top", Path: "src/price.test.ts", Line: 3, EndLine: 3, Change: model.BaseTestModified, Status: model.StatusUnverified}}
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil || res.Status != model.BaseTestsRan || res.Tests[0].Status != model.StatusFailsOnCandidate {
		t.Fatalf("result %+v, %v", res, err)
	}
	want := map[string]string{"src/price.test.ts": baseTest, "src/__snapshots__/price.test.ts.snap": "base snapshot\n", "src/price.ts": candidate["src/price.ts"], "src/other.test.ts": "x\n"}
	for p, content := range want {
		if hybridSeen[p] != content {
			t.Fatalf("hybrid tree %s = %q, want %q", p, hybridSeen[p], content)
		}
	}
	var e model.Evidence
	for _, ev := range h.Evidence() {
		if ev.ID == res.Tests[0].EvidenceID {
			e = ev
		}
	}
	if e.Runner != RunnerJest || e.Kind != model.EvidenceBaseTestDifferential || !strings.Contains(e.Description, "test file and its snapshot file reverted") {
		t.Fatalf("evidence %+v", e)
	}
	manifest := artifactsOfKind(h, model.ArtifactBaseTestHybridManifest)
	if len(manifest) != 1 {
		t.Fatalf("manifests %+v", manifest)
	}
	b, _ := os.ReadFile(manifest[0].Path)
	if !strings.Contains(string(b), `"files":["src/price.test.ts"]`) || !strings.Contains(string(b), `"dirs":[]`) {
		t.Fatalf("manifest %s", b)
	}
	// A candidate snapshot with no baseline counterpart is removed.
	delete(base, "src/__snapshots__/price.test.ts.snap")
	h = itHarness(t, base, candidate, jsImpactedTemplate)
	hybridSeen = nil
	h.executeCapture = func(_ context.Context, _ string, args []string, log, payload io.Writer) execution {
		if sideOf(h, args) == "unknown" {
			if _, err := os.Stat(filepath.Join(itMountDir(args), "src", "__snapshots__", "price.test.ts.snap")); err == nil {
				t.Error("the candidate snapshot stayed in the hybrid tree")
			}
		}
		fmt.Fprint(payload, jsFrame(jsReportFor("src/price.test.ts", jsResult{nil, "top", "passed"})))
		return execution{}
	}
	if res, err := h.RunBaseTests(context.Background(), selected); err != nil || res.Tests[0].Status != model.StatusPassesOnCandidate {
		t.Fatalf("result %+v, %v", res, err)
	}
}

// The mutation package mirrors the fixed report path; the two must agree or a
// TS mutation run would capture nothing.
func TestMutationResultsPathMirrorsHarness(t *testing.T) {
	if mutation.ResultsPath != ResultsPath || mutation.ResultsPlaceholder != config.ResultsPlaceholder {
		t.Fatalf("mutation mirrors %q %q, harness %q %q", mutation.ResultsPath, mutation.ResultsPlaceholder, ResultsPath, config.ResultsPlaceholder)
	}
}
