package fuzz

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// scriptFakeRunner simulates Vitest or Jest runs of a TS/JS harness: it
// writes the framed raw stream the harness writes, and a run exits 1 when a
// behavior ends the process.
type scriptFakeRunner struct {
	fakeRunner
}

func (f *scriptFakeRunner) Observe(ctx context.Context, req Request) (Side, Side, error) {
	f.requests = append(f.requests, req)
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return f.scriptSide("base", baseKind, req), f.scriptSide("candidate", candidateKind, req), nil
}

func (f *scriptFakeRunner) scriptSide(name, kind string, req Request) Side {
	h := req.Harness
	var lines []string
	exited, poisoned := false, false
	for n, test := range h.Tests {
		if exited {
			break
		}
		fi := n + 1
		lines = append(lines, scriptHeadLine(fi, h.Suffix, test.Name), beginLine(fi, len(test.Inputs)))
		observed := 0
		stopped := poisoned
		if poisoned {
			lines = append(lines, stopLine(fi, 0, StopPoisoned))
		}
		for i := range test.Inputs {
			if stopped {
				break
			}
			enc, unstable, stop := f.behave(name, req.Confirm, test, i)
			if stop == "exit" {
				exited = true
				break
			}
			if stop != "" {
				lines = append(lines, stopLine(fi, i, stop))
				poisoned = poisoned || stop == StopTimeout
				stopped = true
				break
			}
			lines = append(lines, obsLine(fi, i, enc, unstable))
			observed++
		}
		if exited {
			break
		}
		if !stopped {
			lines = append(lines, endLine(fi, len(test.Inputs)))
		}
		lines = append(lines, scriptDoneLine(fi, test.Name, observed))
	}
	results, err := h.Normalize(rawStream(lines...))
	if err != nil {
		f.t.Fatalf("fake stream rejected: %v", err)
	}
	c := model.Check{
		ID: "check-" + strconv.Itoa(len(f.checks)+1), Kind: kind, Status: "PASS",
		Command: []string{"vitest", "run", h.Path, "--reporter=json", "--outputFile=/tmp/swiftproof-test-results.json"},
		Output:  "vitest log", Results: results,
	}
	if exited {
		c.Status, c.ExitCode = "FAIL", 1
	}
	f.checks = append(f.checks, c)
	return Side{Check: c}
}

// fuzz.Run renders a TS/JS module's harness, runs it with the confirmation
// pair when the first pair differs, and records jest_json evidence whose
// outcomes are derived like those of Go functions.
func TestRunScriptModule(t *testing.T) {
	price := scriptTargetFor("price", scalar("number"))
	price.Inputs = 4
	tax := scriptTargetFor("tax", scalar("number"))
	tax.Inputs = 4
	plan := Plan{Packages: []PackagePlan{scriptPlan("web/price.ts", price, tax)}, Skipped: []model.FuzzSkip{}}
	r := &scriptFakeRunner{fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"price": func(side string, _ bool, test HarnessTest, i int) (string, bool, string) {
			if side == "candidate" && i == 1 {
				return "changed", false, ""
			}
			return "v" + strconv.Itoa(i), false, ""
		},
	})}}
	o := options()
	o.ScriptFamily = FamilyJest
	o.NewSuffix = func() (string, error) { return "abcdef0123456789", nil }
	rep := Run(context.Background(), r, plan, o)
	if len(r.requests) != 2 || !r.requests[1].Confirm || r.requests[0].Harness.Runner != harness.RunnerJest || strings.Contains(r.requests[0].Harness.Content, "vitest") {
		t.Fatalf("requests %+v", r.requests)
	}
	outcomes := map[string]model.FuzzFunction{}
	for _, fn := range rep.Functions {
		outcomes[fn.Symbol] = fn
	}
	if outcomes["price"].Outcome != model.FuzzDiverged || outcomes["price"].Counterexample.Candidate != "changed" || outcomes["tax"].Outcome != model.FuzzNotDiverged {
		t.Fatalf("price %+v, tax %+v", outcomes["price"], outcomes["tax"])
	}
	if len(r.evidence) != 2 {
		t.Fatalf("evidence %+v", r.evidence)
	}
	for _, e := range r.evidence {
		if e.Runner != harness.RunnerJest || e.Path != "web/swiftproof-fuzz-abcdef0123456789.test.ts" || len(e.TestNames) != 1 {
			t.Fatalf("evidence %+v", e)
		}
	}
}

// A candidate run that fails (here the process ends in halt) supports no
// test of its harness: every function of the module is inconclusive, with
// the input being evaluated when the process ended where there is one, and
// no confirmation pair runs.
func TestRunScriptModuleWithAFailingCandidate(t *testing.T) {
	price := scriptTargetFor("price", scalar("number"))
	price.Inputs = 4
	halt := scriptTargetFor("halt", scalar("number"))
	halt.Inputs = 4
	plan := Plan{Packages: []PackagePlan{scriptPlan("web/price.ts", price, halt)}, Skipped: []model.FuzzSkip{}}
	r := &scriptFakeRunner{fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"price": func(side string, _ bool, test HarnessTest, i int) (string, bool, string) {
			if side == "candidate" && i == 1 {
				return "changed", false, ""
			}
			return "v", false, ""
		},
		"halt": func(side string, _ bool, test HarnessTest, i int) (string, bool, string) {
			if side == "candidate" && i == 2 {
				return "", false, "exit"
			}
			return "v", false, ""
		},
	})}}
	o := options()
	o.ScriptFamily = FamilyVitest
	o.NewSuffix = func() (string, error) { return "abcdef0123456789", nil }
	rep := Run(context.Background(), r, plan, o)
	if len(r.requests) != 1 {
		t.Fatalf("%d requests", len(r.requests))
	}
	outcomes := map[string]model.FuzzFunction{}
	for _, fn := range rep.Functions {
		outcomes[fn.Symbol] = fn
	}
	if outcomes["price"].Outcome != model.FuzzInconclusive || outcomes["price"].Reason != "the candidate run ended with exit code 1; a TS/JS run supports a comparison only when it passes with exit code 0" {
		t.Fatalf("price %+v", outcomes["price"])
	}
	if outcomes["halt"].Outcome != model.FuzzInconclusive || outcomes["halt"].Reason != "the candidate process ended while evaluating input 2: "+r.requests[0].Harness.Tests[1].Inputs[2].Call {
		t.Fatalf("halt %+v", outcomes["halt"])
	}
	lines := Unverified(rep, 0)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Differential fuzzing of price (web/price.ts) is inconclusive: ") {
		t.Fatalf("unverified %q", lines)
	}
}

// Without a failing candidate, the functions of a module compare as Go
// functions do.
func TestRunScriptModuleWithoutDifference(t *testing.T) {
	a := scriptTargetFor("a", scalar("string"))
	a.Inputs = 3
	r := &scriptFakeRunner{fakeRunner{t: t, behave: behaviorByName(nil)}}
	o := options()
	o.ScriptFamily = FamilyVitest
	rep := Run(context.Background(), r, Plan{Packages: []PackagePlan{scriptPlan("m.js", a)}, Skipped: []model.FuzzSkip{}}, o)
	if len(r.requests) != 1 || len(rep.Functions) != 1 || rep.Functions[0].Outcome != model.FuzzNotDiverged || rep.Status != model.FuzzRan {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(r.requests[0].Harness.Content, `import { test } from "vitest";`) || r.requests[0].Harness.Path != "swiftproof-fuzz-abcdef12.test.js" {
		t.Fatalf("harness %s", r.requests[0].Harness.Path)
	}
	// Without a runner family the module is not rendered: inconclusive.
	o.ScriptFamily = ""
	rep = Run(context.Background(), r, Plan{Packages: []PackagePlan{scriptPlan("m.js", a)}, Skipped: []model.FuzzSkip{}}, o)
	if rep.Functions[0].Outcome != model.FuzzInconclusive || !strings.HasPrefix(rep.Functions[0].Reason, "the fuzz harness could not be rendered") {
		t.Fatalf("report %+v", rep)
	}
}
