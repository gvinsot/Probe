package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

var vitestGeneratedTest = []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}

// An eligible TS/JS function runs only with a verifiable Vitest or Jest
// template: with the Go template it is listed as not fuzzed with the reason,
// nothing runs, and the section reason says that no changed function can
// run with this template. With a Vitest template and only eligible Go
// functions, the stage cannot run them: it records not_run with an
// Unverified line and lists them with the Go template reason. With a Jest
// template and a module whose path Jest reads as a regular expression, the
// TS/JS function is skipped with that reason and nothing runs.
func TestRunFuzzScriptTemplateRules(t *testing.T) {
	ts := "export function price(n: number): number {\n  return n;\n}\n"
	goMod := "module example.test/m\n\ngo 1.23\n"
	base, candidate := snapshots(t,
		map[string]string{"web/price.ts": ts, "app/[id]/page.ts": ts, "go.mod": goMod, "a.go": "package m\n\nfunc F(n int) int { return n }\n"},
		map[string]string{"web/price.ts": strings.Replace(ts, "return n;", "return n + 1;", 1), "app/[id]/page.ts": strings.Replace(ts, "return n;", "return n + 1;", 1), "go.mod": goMod, "a.go": "package m\n\nfunc F(n int) int { return n + 1 }\n"})
	tsOnly := model.Change{Files: []model.ChangedFile{{Path: "web/price.ts", Status: "M"}}}
	r := &model.Report{}
	var errOut bytes.Buffer
	runFuzz(context.Background(), nil, fuzzPolicy(), tsOnly, base, candidate, r, true, &errOut)
	if f := r.Fuzz; f == nil || f.Status != model.FuzzNoCandidates || f.Reason != fuzz.ReasonNoRunnable || f.SkippedTotal != 1 || f.Skipped[0].Symbol != "price" || f.Skipped[0].Reason != fuzz.ReasonScriptTemplate {
		t.Fatalf("fuzz %+v", r.Fuzz)
	}
	if errOut.Len() != 0 || len(r.Unverified) != 0 {
		t.Fatalf("stderr %q, unverified %q", errOut.String(), r.Unverified)
	}
	if line := fuzzLine(r.Fuzz); line != "Differential fuzzing: no function ran (no changed function can run with this generated_test template; 1 skipped)." {
		t.Fatalf("stdout line %q", line)
	}
	cfg := fuzzPolicy()
	cfg.Commands["generated_test"] = vitestGeneratedTest
	goOnly := model.Change{Files: []model.ChangedFile{{Path: "a.go", Status: "M"}}}
	r = &model.Report{}
	runFuzz(context.Background(), nil, cfg, goOnly, base, candidate, r, true, &bytes.Buffer{})
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzNotRun || f.Reason != fuzzTemplateReason || f.SkippedTotal != 1 || f.Skipped[0].Symbol != "m.F" || f.Skipped[0].Reason != fuzz.ReasonGoTemplate {
		t.Fatalf("fuzz %+v", f)
	}
	if len(r.Unverified) != 1 || r.Unverified[0] != "Differential fuzzing did not run: "+fuzzTemplateReason {
		t.Fatalf("unverified %q", r.Unverified)
	}
	cfg.Commands["generated_test"] = []string{"jest", "{file}", "--json", "--outputFile={results_out}"}
	bracket := model.Change{Files: []model.ChangedFile{{Path: "app/[id]/page.ts", Status: "M"}}}
	r = &model.Report{}
	errOut.Reset()
	runFuzz(context.Background(), nil, cfg, bracket, base, candidate, r, true, &errOut)
	if f := r.Fuzz; f == nil || f.Status != model.FuzzNoCandidates || f.Reason != fuzz.ReasonNoRunnable || f.SkippedTotal != 1 || f.Skipped[0].Reason != fuzz.ReasonScriptJestPath {
		t.Fatalf("fuzz %+v", r.Fuzz)
	}
	if errOut.Len() != 0 || len(r.Unverified) != 0 {
		t.Fatalf("stderr %q, unverified %q", errOut.String(), r.Unverified)
	}
}

// Through the whole CLI, with a Vitest template, a change to an eligible Go
// function and to an eligible TS/JS function, and a sandbox image that does
// not exist: the TS/JS function runs (it has fuzz checks), and the Go
// function is listed with the Go template reason and one Unverified line; the
// Go function takes no share of the budget. No repository code runs, whether
// or not Docker is installed.
func TestScriptFuzzRunsBesideGoFunctionsThroughTheCLI(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/m\n\ngo 1.23\n")
	write(t, dir, "a.go", "package m\n\nfunc F(n int) int { return n }\n")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "a.go", "package m\n\nfunc F(n int) int { return n + 1 }\n")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n + 1;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	policy := v04Policy(t, absentImage, func(p map[string]any) {
		delete(p, "prepare")
		delete(p, "mutation")
		p["commands"] = map[string]any{"generated_test": vitestGeneratedTest}
		p["fuzz"] = map[string]any{"max_packages": 1, "max_functions": 1}
	})
	_, r, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 1 || f.Functions[0].Symbol != "price" || f.Functions[0].Checks == nil {
		t.Fatalf("fuzz %+v", f)
	}
	if f.SkippedTotal != 1 || f.Skipped[0].Symbol != "m.F" || f.Skipped[0].Reason != fuzz.ReasonGoTemplate {
		t.Fatalf("skipped %+v", f.Skipped)
	}
	found := false
	for _, u := range r.Unverified {
		found = found || u == fuzz.GoTemplateUnverified(1)
		if strings.Contains(u, "fuzz.max_functions or fuzz.max_packages was reached") {
			t.Fatalf("the Go function took a share of the budget: %q", u)
		}
	}
	if !found {
		t.Fatalf("unverified %q", r.Unverified)
	}
	if !strings.Contains(output, "Running differential fuzzing of 1 changed TS/JS function in 1 module in isolated Docker sandboxes...") {
		t.Fatalf("output:\n%s", output)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
}

// Through the whole CLI, with a Vitest template and a sandbox image that does
// not exist: the TS/JS function runs as a module (a fuzz_base and a
// fuzz_candidate check, one UNVERIFIED jest_json record, an inconclusive
// function), the progress line names the module, and no harness file reaches
// the checkout. No repository code runs, whether or not Docker is installed.
func TestScriptFuzzStageThroughTheCLI(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n + 1;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	policy := v04Policy(t, absentImage, func(p map[string]any) {
		delete(p, "prepare")
		delete(p, "mutation")
		p["commands"] = map[string]any{"generated_test": vitestGeneratedTest}
	})
	code, r, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (the sandbox image does not exist)", code)
	}
	if kinds := strings.Join(checkKinds(r), ","); kinds != model.CheckFuzzBase+","+model.CheckFuzzCandidate {
		t.Fatalf("checks %s", kinds)
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 1 || f.Functions[0].Symbol != "price" || f.Functions[0].Outcome != model.FuzzInconclusive || f.Functions[0].Checks == nil {
		t.Fatalf("fuzz %+v", f)
	}
	if len(r.Evidence) != 1 || r.Evidence[0].Runner != harness.RunnerJest || r.Evidence[0].Status != model.StatusUnverified || !strings.HasSuffix(r.Evidence[0].Path, ".test.ts") {
		t.Fatalf("evidence %+v", r.Evidence)
	}
	for _, c := range r.Checks {
		if c.Command[0] != "vitest" || c.Command[2] != r.Evidence[0].Path {
			t.Fatalf("command %q", c.Command)
		}
	}
	for _, want := range []string{
		"Running differential fuzzing of 1 changed TS/JS function in 1 module in isolated Docker sandboxes...",
		"Differential fuzzing: 1 changed function planned, 1 with recorded fuzz checks; 0 diverged, 0 not diverged, 1 inconclusive; 0 skipped.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	found := false
	for _, u := range r.Unverified {
		found = found || strings.HasPrefix(u, "Differential fuzzing of price (web/price.ts) is inconclusive: ")
	}
	if !found {
		t.Fatalf("unverified %q", r.Unverified)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "web", "swiftproof-fuzz-*"))
	if len(matches) != 0 {
		t.Fatalf("harness file in the checkout: %v", matches)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
}

func TestFuzzPlanText(t *testing.T) {
	goPkg := func(n int) fuzz.PackagePlan { return fuzz.PackagePlan{Targets: make([]fuzz.Target, n)} }
	module := func(n int) fuzz.PackagePlan {
		return fuzz.PackagePlan{Targets: make([]fuzz.Target, n), Script: &fuzz.ScriptModule{Path: "m.ts", Import: "./m", Ext: "ts"}}
	}
	for want, plan := range map[string]fuzz.Plan{
		"7 changed Go functions in 2 packages":                                          {Packages: []fuzz.PackagePlan{goPkg(3), goPkg(4)}},
		"1 changed TS/JS function in 1 module":                                          {Packages: []fuzz.PackagePlan{module(1)}},
		"1 changed Go function in 1 package and 3 changed TS/JS functions in 2 modules": {Packages: []fuzz.PackagePlan{goPkg(1), module(1), module(2)}},
	} {
		if got := fuzzPlanText(plan); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
