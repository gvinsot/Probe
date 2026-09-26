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
// and nothing runs. With a Vitest template and eligible Go functions too, the
// stage cannot run the Go functions: it records not_run, and the TS/JS
// function is listed as not fuzzed because the stage did not run.
func TestRunFuzzScriptTemplateRules(t *testing.T) {
	ts := "export function price(n: number): number {\n  return n;\n}\n"
	goMod := "module example.test/m\n\ngo 1.23\n"
	base, candidate := snapshots(t,
		map[string]string{"web/price.ts": ts, "go.mod": goMod, "a.go": "package m\n\nfunc F(n int) int { return n }\n"},
		map[string]string{"web/price.ts": strings.Replace(ts, "return n;", "return n + 1;", 1), "go.mod": goMod, "a.go": "package m\n\nfunc F(n int) int { return n + 1 }\n"})
	tsOnly := model.Change{Files: []model.ChangedFile{{Path: "web/price.ts", Status: "M"}}}
	r := &model.Report{}
	var errOut bytes.Buffer
	runFuzz(context.Background(), nil, fuzzPolicy(), tsOnly, base, candidate, r, true, &errOut)
	if f := r.Fuzz; f == nil || f.Status != model.FuzzNoCandidates || f.SkippedTotal != 1 || f.Skipped[0].Symbol != "price" || f.Skipped[0].Reason != fuzz.ReasonScriptTemplate {
		t.Fatalf("fuzz %+v", r.Fuzz)
	}
	if errOut.Len() != 0 || len(r.Unverified) != 0 {
		t.Fatalf("stderr %q, unverified %q", errOut.String(), r.Unverified)
	}
	cfg := fuzzPolicy()
	cfg.Commands["generated_test"] = vitestGeneratedTest
	both := model.Change{Files: []model.ChangedFile{{Path: "web/price.ts", Status: "M"}, {Path: "a.go", Status: "M"}}}
	r = &model.Report{}
	runFuzz(context.Background(), nil, cfg, both, base, candidate, r, true, &bytes.Buffer{})
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzNotRun || f.Reason != fuzzTemplateReason || f.SkippedTotal != 1 || f.Skipped[0].Reason != fuzz.ReasonScriptStageNotRun {
		t.Fatalf("fuzz %+v", f)
	}
	if len(r.Unverified) != 1 || r.Unverified[0] != "Differential fuzzing did not run: "+fuzzTemplateReason {
		t.Fatalf("unverified %q", r.Unverified)
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
