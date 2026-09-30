package mutation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const priceScript = `export function price(total: number, percent: number): number {
  if (percent > 100 || total <= 0) {
    return 0;
  }
  const rows: Array<number> = [1, 2];
  return total - Math.floor((total * percent) / 100);
}

export const isFree = (total: number): boolean => total === 0;

export function ok(): boolean {
  return true;
}

let i = 0;
i++;
const top = 1 + 2;
`

func scriptSites(t *testing.T, p, src string, added map[int]bool) []Site {
	t.Helper()
	if added == nil {
		added = allLines(src)
	}
	sites, capped, skip := ScriptFileSites(p, []byte(src), added)
	if skip != "" || capped {
		t.Fatalf("skip %q capped %v", skip, capped)
	}
	return sites
}

func TestScriptFileSites(t *testing.T) {
	sites := scriptSites(t, "web/price.ts", priceScript, nil)
	var got []string
	for _, s := range sites {
		got = append(got, s.Operator+" "+s.Original+" -> "+s.Replacement+" @"+itoa(s.Line)+" "+s.Symbol)
	}
	want := []string{
		"negate_condition (percent > 100 || total <= 0) -> (!(percent > 100 || total <= 0)) @2 price",
		"boundary > -> >= @2 price",
		"boundary <= -> < @2 price",
		"negate_comparison === -> !== @9 isFree",
		"swap_logical || -> && @2 price",
		"increment_constant 100 -> 101 @2 price",
		"increment_constant 0 -> 1 @2 price",
		"increment_constant 0 -> 1 @9 isFree",
		"swap_arithmetic - -> + @6 price",
		"swap_arithmetic * -> / @6 price",
		"swap_arithmetic / -> * @6 price",
		"flip_boolean true -> false @12 ok",
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing site %q in\n%s", w, strings.Join(got, "\n"))
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d sites, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for _, s := range sites {
		// Generics, ++ and module-level code are never mutated.
		if s.Line == 5 || s.Line >= 15 {
			t.Errorf("site outside a function body or on a type: %+v", s)
		}
		mutated, err := s.Apply([]byte(priceScript))
		if err != nil {
			t.Errorf("%s at %d: %v", s.Operator, s.Line, err)
			continue
		}
		if strings.Count(string(mutated), "\n") != strings.Count(priceScript, "\n") {
			t.Errorf("line count changed")
		}
	}
	// Only sites whose whole span is on added lines.
	if sites := scriptSites(t, "web/price.ts", priceScript, map[int]bool{6: true}); len(sites) != 3 {
		t.Fatalf("line 6 only: %+v", sites)
	}
}

func TestScriptFileSitesSkips(t *testing.T) {
	if _, _, skip := ScriptFileSites("web/gen.ts", []byte("// @generated\nexport function f(a: number) { return a + 1; }\n"), map[int]bool{2: true}); skip != skipScriptGenerated {
		t.Fatalf("generated: %q", skip)
	}
	if _, _, skip := ScriptFileSites("web/min.js", []byte(strings.Repeat("a", 1200)+"\n"), map[int]bool{1: true}); skip != skipScriptMinified {
		t.Fatalf("minified: %q", skip)
	}
	// In TSX, < and > may be tags and are not mutated; <= still is.
	src := "export function C(n: number) {\n  return n < 2 ? <div>{n}</div> : n >= 3;\n}\n"
	for _, s := range scriptSites(t, "web/c.tsx", src, nil) {
		if s.Original == "<" || s.Original == ">" {
			t.Fatalf("tag-like operator mutated in TSX: %+v", s)
		}
	}
}

func TestApplyScriptRefusesJoins(t *testing.T) {
	src := []byte("export function f(a: number) {\n  return a - -1;\n}\n")
	s := Site{Path: "f.ts", Line: 2, Start: bytesIndex(src, "- -"), End: bytesIndex(src, "- -") + 1, Operator: OpSwapArithmetic, Original: "-", Replacement: "+"}
	if _, err := s.Apply(src); err != nil {
		t.Fatalf("a spaced replacement was refused: %v", err)
	}
	joined := []byte("export function f(a: number) {\n  return a +-1;\n}\n")
	j := Site{Path: "f.ts", Line: 2, Start: bytesIndex(joined, "+-"), End: bytesIndex(joined, "+-") + 1, Operator: OpSwapArithmetic, Original: "+", Replacement: "-"}
	if _, err := j.Apply(joined); err == nil || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("a joining replacement was applied: %v", err)
	}
	s.Original = "+"
	if _, err := s.Apply(src); err == nil {
		t.Fatal("a stale site was applied")
	}
}

// jestResults renders a normalized report with one file: title, status pairs.
func jestResults(t *testing.T, fileStatus string, pairs ...string) string {
	t.Helper()
	var assertions []map[string]any
	for i := 0; i+1 < len(pairs); i += 2 {
		assertions = append(assertions, map[string]any{"ancestorTitles": []string{}, "title": pairs[i], "status": pairs[i+1]})
	}
	if assertions == nil {
		assertions = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"testResults": []map[string]any{{"name": "/workspace/web/price.test.ts", "status": fileStatus, "assertionResults": assertions}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClassifyScript(t *testing.T) {
	command := []string{"vitest", "related", "web/price.ts", "--run", "--reporter=json", "--outputFile=" + ResultsPath}
	control := model.Check{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS", Command: command, Results: jestResults(t, "passed", "a", "passed", "b", "passed")}
	mutant := func(status string, exit int, results string) model.Check {
		return model.Check{ID: "mutation-check-2", Kind: model.CheckMutant, Status: status, ExitCode: exit, Command: command, Results: results}
	}
	cases := []struct {
		name string
		run  model.Check
		want string
	}{
		{"killed", mutant("FAIL", 1, jestResults(t, "failed", "a", "failed", "b", "passed")), model.MutantKilled},
		{"survived", mutant("PASS", 0, jestResults(t, "passed", "a", "passed", "b", "passed")), model.MutantSurvived},
		{"did not load", mutant("FAIL", 1, jestResults(t, "failed")), model.MutantInvalid},
		{"no report", mutant("FAIL", 1, ""), model.MutantInconclusive},
		{"failed without a failing test", mutant("FAIL", 1, jestResults(t, "passed", "a", "passed")), model.MutantInconclusive},
		{"passed without a passing test", mutant("PASS", 0, jestResults(t, "passed", "a", "skipped")), model.MutantInconclusive},
		{"exit outside the failure range", mutant("FAIL", 125, jestResults(t, "failed", "a", "failed")), model.MutantInconclusive},
		{"timeout", mutant("TIMEOUT", -1, ""), model.MutantTimeout},
		{"another command", model.Check{ID: "x", Kind: model.CheckMutant, Status: "PASS", Command: command[:3]}, model.MutantInconclusive},
	}
	for _, tc := range cases {
		v := ClassifyFor(true, control, tc.run)
		if v.Status != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, v.Status, v.Reason, tc.want)
		}
	}
	if v := ClassifyFor(true, control, cases[0].run); len(v.FailedTests) != 1 || v.FailedTests[0] != "web/price.test.ts: a" {
		t.Fatalf("failed tests %q", v.FailedTests)
	}
	if v := ClassifyFor(true, control, cases[1].run); v.TestsRun != 2 {
		t.Fatalf("tests run %d", v.TestsRun)
	}
	for name, bad := range map[string]model.Check{
		"failing control":  {ID: "c", Kind: model.CheckMutationControl, Status: "PASS", Command: command, Results: jestResults(t, "failed", "a", "failed")},
		"no passing test":  {ID: "c", Kind: model.CheckMutationControl, Status: "PASS", Command: command, Results: jestResults(t, "passed", "a", "skipped")},
		"file not loaded":  {ID: "c", Kind: model.CheckMutationControl, Status: "PASS", Command: command, Results: jestResults(t, "failed")},
		"no report":        {ID: "c", Kind: model.CheckMutationControl, Status: "PASS", Command: command},
		"control not PASS": {ID: "c", Kind: model.CheckMutationControl, Status: "FAIL", ExitCode: 1, Command: command, Results: jestResults(t, "passed", "a", "passed")},
	} {
		if NewControlFor(true, bad).Reason() == "" {
			t.Errorf("%s: control accepted", name)
		}
	}
}

func TestExpandScriptCommand(t *testing.T) {
	command := []string{"npx", "--no", "jest", "--findRelatedTests", FilePlaceholder, "--json", "--outputFile=" + ResultsPlaceholder}
	if !ScriptCommand(command) || ScriptCommand([]string{"go", "test", "-json", "{package}"}) {
		t.Fatal("ScriptCommand")
	}
	got := ExpandCommand(command, PackageArg("web/price.ts"))
	want := []string{"npx", "--no", "jest", "--findRelatedTests", "web/price.ts", "--json", "--outputFile=" + ResultsPath}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("expanded %q, want %q", got, want)
	}
	if PackageArg("web/price.go") != "./web" {
		t.Fatal("Go package argument changed")
	}
	if terms := TermsFor(command); terms.Invalid != "did not load" || TermsFor([]string{"go"}).Invalid != "did not build or pass go vet" {
		t.Fatal("terms")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func bytesIndex(b []byte, s string) int { return strings.Index(string(b), s) }
