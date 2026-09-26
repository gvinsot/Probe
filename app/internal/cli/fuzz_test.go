package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/coverage"
	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func fuzzPolicy() config.Config {
	cfg := config.Default("go")
	cfg.Fuzz = &config.Fuzz{}
	return cfg
}

// snapshots writes baseline and candidate trees and returns their roots.
func snapshots(t *testing.T, base, candidate map[string]string) (string, string) {
	t.Helper()
	b, c := t.TempDir(), t.TempDir()
	for root, files := range map[string]map[string]string{b: base, c: candidate} {
		for name, content := range files {
			write(t, root, filepath.FromSlash(name), content)
		}
	}
	return b, c
}

// A change without an eligible Go function records no_candidates and lists
// the changed exported TS/JS functions, without a harness and without any
// progress line or Unverified entry.
func TestRunFuzzWithoutEligibleGoFunction(t *testing.T) {
	base, candidate := snapshots(t,
		map[string]string{"web/price.ts": "export function price(n: number): number {\n  return n;\n}\n", "go.mod": "module example.test/m\n\ngo 1.23\n", "a.go": "package m\n\nfunc (T) M() int { return 1 }\n\ntype T struct{}\n"},
		map[string]string{"web/price.ts": "export function price(n: number): number {\n  return n + 1;\n}\n", "go.mod": "module example.test/m\n\ngo 1.23\n", "a.go": "package m\n\nfunc (T) M() int { return 2 }\n\ntype T struct{}\n"})
	change := model.Change{Files: []model.ChangedFile{{Path: "web/price.ts", Status: "M"}, {Path: "a.go", Status: "M"}}}
	r := &model.Report{}
	var errOut bytes.Buffer
	runFuzz(context.Background(), nil, fuzzPolicy(), change, base, candidate, r, true, &errOut)
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzNoCandidates || f.Reason != fuzz.ReasonNoCandidates || len(f.Functions) != 0 || f.SkippedTotal != 2 {
		t.Fatalf("fuzz %+v", f)
	}
	if f.Skipped[0].Path != "a.go" || f.Skipped[0].Reason != fuzz.ReasonMethod || f.Skipped[1].Path != "web/price.ts" || f.Skipped[1].Symbol != "price" || f.Skipped[1].Reason != fuzz.ReasonScriptNotImplemented {
		t.Fatalf("skipped %+v", f.Skipped)
	}
	if errOut.Len() != 0 || len(r.Unverified) != 0 {
		t.Fatalf("stderr %q, unverified %q", errOut.String(), r.Unverified)
	}
	if line := fuzzLine(f); line != "Differential fuzzing: no function ran (no changed Go function is eligible for differential fuzzing; 2 skipped)." {
		t.Fatalf("line %q", line)
	}
}

// Eligible functions with a template that cannot run a fuzz harness record
// not_run with the reason and an Unverified line, and keep the skipped list.
func TestRunFuzzUnsupportedTemplate(t *testing.T) {
	base, candidate := snapshots(t,
		map[string]string{"go.mod": "module example.test/m\n\ngo 1.23\n", "a.go": "package m\n\nfunc F(n int) int { return n }\n\nfunc (T) M() int { return 1 }\n\ntype T struct{}\n"},
		map[string]string{"go.mod": "module example.test/m\n\ngo 1.23\n", "a.go": "package m\n\nfunc F(n int) int { return n + 1 }\n\nfunc (T) M() int { return 2 }\n\ntype T struct{}\n"})
	change := model.Change{Files: []model.ChangedFile{{Path: "a.go", Status: "M"}}}
	for _, template := range [][]string{{"go", "test", "{file}"}, {"go", "test", "./..."}, {"go", "test", "-exec=wrap", "{package}"}} {
		cfg := fuzzPolicy()
		cfg.Commands["generated_test"] = template
		r := &model.Report{}
		runFuzz(context.Background(), nil, cfg, change, base, candidate, r, true, &bytes.Buffer{})
		if r.Fuzz == nil || r.Fuzz.Status != model.FuzzNotRun || r.Fuzz.Reason != fuzzTemplateReason || r.Fuzz.SkippedTotal != 1 || r.Fuzz.Skipped[0].Reason != fuzz.ReasonMethod {
			t.Fatalf("%q: fuzz %+v", template, r.Fuzz)
		}
		if len(r.Unverified) != 1 || r.Unverified[0] != "Differential fuzzing did not run: "+fuzzTemplateReason {
			t.Fatalf("%q: unverified %q", template, r.Unverified)
		}
	}
	// --fuzz=false and an absent policy leave the section to recordFuzzSkipped.
	r := &model.Report{}
	runFuzz(context.Background(), nil, fuzzPolicy(), change, base, candidate, r, false, &bytes.Buffer{})
	runFuzz(context.Background(), nil, config.Default("go"), change, base, candidate, r, true, &bytes.Buffer{})
	if r.Fuzz != nil {
		t.Fatalf("fuzz %+v", r.Fuzz)
	}
}

// A stage that could not run records its reason as an Unverified line too.
func TestRecordFuzzSkippedNotRunAddsUnverified(t *testing.T) {
	r := &model.Report{}
	recordFuzzSkipped(fuzzPolicy(), stageContext{mode: "review", checks: true, reason: reasonPrepareFailed}, true, r)
	if r.Fuzz == nil || r.Fuzz.Status != model.FuzzNotRun || len(r.Unverified) != 1 || r.Unverified[0] != "Differential fuzzing did not run: "+reasonPrepareFailed {
		t.Fatalf("fuzz %+v, unverified %q", r.Fuzz, r.Unverified)
	}
	r = &model.Report{}
	recordFuzzSkipped(fuzzPolicy(), stageContext{mode: "review", checks: false, reason: reasonExecutionDisabled}, true, r)
	if r.Fuzz == nil || r.Fuzz.Status != model.FuzzDisabled || len(r.Unverified) != 0 {
		t.Fatalf("fuzz %+v, unverified %q", r.Fuzz, r.Unverified)
	}
}

func TestFuzzLine(t *testing.T) {
	for want, f := range map[string]*model.FuzzReport{
		"": nil,
		"Differential fuzzing: disabled for this run (--fuzz=false).":                        {Status: model.FuzzDisabled, Reason: "--fuzz=false"},
		"Differential fuzzing did not run: dependency preparation did not produce an image.": {Status: model.FuzzNotRun, Reason: reasonPrepareFailed},
		"Differential fuzzing did not run: no reason was recorded.":                          {Status: model.FuzzNotRun},
		"Differential fuzzing: no function ran (no changed files; 0 skipped).":               {Status: model.FuzzNoCandidates, Reason: "no changed files"},
		"Differential fuzzing: 3 changed functions ran on seeded inputs; 1 diverged, 1 not diverged, 1 inconclusive; 4 skipped.": {Status: model.FuzzRan, SkippedTotal: 4, Functions: []model.FuzzFunction{
			{Outcome: model.FuzzDiverged}, {Outcome: model.FuzzNotDiverged}, {Outcome: model.FuzzInconclusive},
		}},
	} {
		if got := fuzzLine(f); got != want {
			t.Errorf("fuzzLine = %q, want %q", got, want)
		}
		if strings.Contains(want, "%") {
			t.Errorf("a percentage in %q", want)
		}
	}
}

// Through the whole CLI, with a sandbox image that does not exist: fuzzing
// runs after the initial checks and coverage, records one fuzz_base and one
// fuzz_candidate check, one UNVERIFIED differential_fuzz record and an
// inconclusive function with an Unverified line, and prints its stdout line.
// No repository code runs, whether or not Docker is installed.
func TestFuzzStageThroughTheCLI(t *testing.T) {
	dir := fixture(t)
	policy := v04Policy(t, absentImage, func(p map[string]any) {
		delete(p, "prepare")
		delete(p, "mutation")
	})
	code, r, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (the sandbox image does not exist)", code)
	}
	kinds := checkKinds(r)
	want := []string{"test", "typecheck", "build", coverage.CommandKey, model.CheckFuzzBase, model.CheckFuzzCandidate}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("check order %v, want %v", kinds, want)
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 1 {
		t.Fatalf("fuzz %+v", f)
	}
	fn := f.Functions[0]
	if fn.Symbol != "fixture.Allowed" || fn.Outcome != model.FuzzInconclusive || fn.EvidenceID == "" || fn.Checks == nil || fn.Checks.BaseConfirm != "" {
		t.Fatalf("function %+v", fn)
	}
	if len(r.Evidence) != 1 || r.Evidence[0].Kind != model.EvidenceDifferentialFuzz || r.Evidence[0].Status != model.StatusUnverified {
		t.Fatalf("evidence %+v", r.Evidence)
	}
	found := false
	for _, u := range r.Unverified {
		found = found || strings.HasPrefix(u, "Differential fuzzing of fixture.Allowed is inconclusive: ")
	}
	if !found {
		t.Fatalf("unverified %q", r.Unverified)
	}
	for _, want := range []string{
		"Running differential fuzzing of 1 changed Go function in 1 package in isolated Docker sandboxes...",
		"Differential fuzzing: 1 changed function ran on seeded inputs; 0 diverged, 0 not diverged, 1 inconclusive; 0 skipped.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	for _, a := range r.Audit {
		if a.Tool == "run_fuzz" || strings.HasPrefix(a.Tool, "stage:run_fuzz") && a.Tool != "stage:run_fuzz" {
			t.Fatalf("audit tool %q", a.Tool)
		}
	}
	// The harness file never reaches the checkout.
	matches, _ := filepath.Glob(filepath.Join(dir, "swiftproof_fuzz_*"))
	if len(matches) != 0 {
		t.Fatalf("harness file in the checkout: %v", matches)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
	if _, err := os.Stat(filepath.Join(dir, ".swiftproof")); err == nil {
		t.Fatal("the report was written into the checkout")
	}
}
