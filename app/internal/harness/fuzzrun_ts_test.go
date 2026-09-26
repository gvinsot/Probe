package harness

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const scriptFuzzPath = "pkg/swiftproof-fuzz-abcdef12.test.ts"

// scriptFuzzSource stands for a rendered TS/JS fuzz harness: the harness side
// needs only its top-level test declarations.
const scriptFuzzSource = `// @ts-nocheck
import { test } from "vitest";

test("TestSwiftProofFuzz_abcdef12_1", async function () {
}, 60000);

test("TestSwiftProofFuzz_abcdef12_2", async function () {
}, 60000);
`

var scriptVitestTemplate = []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}

// scriptFixture is fixture with a Vitest generated_test template.
func scriptFixture(t *testing.T) *Harness {
	t.Helper()
	h := fixture(t)
	h.opts.Commands["generated_test"] = append([]string(nil), scriptVitestTemplate...)
	return h
}

// testStarted counts the "started:" markers of a test stream; an "inside"
// marker says that the stream ended inside the last started test.
func testStarted(results string) (int, bool) {
	return strings.Count(results, "started:"), strings.Contains(results, "inside")
}

func scriptRun(confirm bool) ObservedRun {
	return ObservedRun{Path: scriptFuzzPath, Content: scriptFuzzSource, TestNames: fuzzNames, Confirm: confirm, SaveSource: true,
		Deadline: time.Now().Add(time.Minute), Normalize: testNormalize, Runner: RunnerJest, Started: testStarted}
}

// A TS/JS run executes the reviewed template with the harness as its {file}
// target and the report path in place of {results_out}, adds no test-name
// filter, captures only the observation stream, and records the runner.
func TestRunObservedScriptCommandAndChannel(t *testing.T) {
	h := scriptFixture(t)
	argvs := fakeFuzz(t, h,
		fuzzSideRun{log: "vitest log", payload: coverageFrame("stream:started:started:")},
		fuzzSideRun{log: "vitest log", payload: coverageFrame("stream:started:started:")})
	base, candidate, err := h.RunObserved(context.Background(), scriptRun(false))
	if err != nil {
		t.Fatal(err)
	}
	command := []string{"vitest", "run", scriptFuzzPath, "--reporter=json", "--outputFile=" + ResultsPath}
	if !reflect.DeepEqual(base.Check.Command, command) || !reflect.DeepEqual(candidate.Check.Command, command) {
		t.Fatalf("commands %q %q", base.Check.Command, candidate.Check.Command)
	}
	for _, args := range *argvs {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, FuzzObservationsPath) || strings.Contains(joined, "-run") {
			t.Fatalf("argv %q", args)
		}
		if !reflect.DeepEqual(args[len(args)-len(command):], command) {
			t.Fatalf("argv tail %q", args[len(args)-len(command):])
		}
	}
	if base.Check.Status != "PASS" || candidate.Check.Status != "PASS" || base.Check.Results == "" {
		t.Fatalf("checks %+v %+v", base.Check, candidate.Check)
	}
	if h.fuzz.runners[base.Check.ID] != RunnerJest || h.fuzz.runners[candidate.Check.ID] != RunnerJest {
		t.Fatalf("runners %v", h.fuzz.runners)
	}
	audit := h.Audit()
	if len(audit) != 1 || audit[0].Tool != "stage:run_fuzz" || !strings.Contains(audit[0].Arguments, `"runner":"jest_json"`) {
		t.Fatalf("audit %+v", audit)
	}
}

// Preconditions of a TS/JS run: no check is recorded when one fails.
func TestRunObservedScriptPreconditions(t *testing.T) {
	for name, edit := range map[string]func(*Harness, *ObservedRun){
		"go template": func(h *Harness, _ *ObservedRun) {
			h.opts.Commands["generated_test"] = []string{"go", "test", "{package}"}
		},
		"npm template": func(h *Harness, _ *ObservedRun) {
			h.opts.Commands["generated_test"] = []string{"npm", "test", "{file}", "{results_out}"}
		},
		"go path":          func(_ *Harness, r *ObservedRun) { r.Path = "pkg/x_test.go" },
		"unclean path":     func(_ *Harness, r *ObservedRun) { r.Path = "pkg/../pkg/x.test.ts" },
		"escaping path":    func(_ *Harness, r *ObservedRun) { r.Path = "../x.test.ts" },
		"other test names": func(_ *Harness, r *ObservedRun) { r.TestNames = fuzzNames[:1] },
		"no test":          func(_ *Harness, r *ObservedRun) { r.Content = "// nothing\n" },
		"described test": func(_ *Harness, r *ObservedRun) {
			r.Content = "describe(\"x\", () => {\n  test(\"a\", () => {});\n});\n"
		},
		"no started counter": func(_ *Harness, r *ObservedRun) { r.Started = nil },
		"unknown runner":     func(_ *Harness, r *ObservedRun) { r.Runner = "mocha_json" },
		"existing path": func(h *Harness, r *ObservedRun) {
			r.Path = "pkg/main.go.test.ts"
			cleanup, err := stageEphemeral(h.base, r.Path, "x")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
		},
	} {
		h := scriptFixture(t)
		fakeFuzz(t, h, fuzzSideRun{}, fuzzSideRun{})
		run := scriptRun(false)
		edit(h, &run)
		if _, _, err := h.RunObserved(context.Background(), run); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(h.Checks()) != 0 {
			t.Errorf("%s: a check was recorded", name)
		}
	}
}

// Baseline-side ERROR rules of a TS/JS harness come from the payload and the
// stream, never from the log; the candidate side never becomes ERROR.
func TestRunObservedScriptStatusRules(t *testing.T) {
	both := coverageFrame("stream:started:started:")
	one := coverageFrame("stream:started:")
	inside := coverageFrame("stream:started:inside")
	for name, tc := range map[string]struct {
		base, candidate        fuzzSideRun
		baseStatus, candStatus string
		baseCause              string
	}{
		"both pass":                         {fuzzSideRun{payload: both}, fuzzSideRun{payload: both}, "PASS", "PASS", ""},
		"baseline fails without a payload":  {fuzzSideRun{exit: 1, log: `{"Action":"run","Test":"TestSwiftProofFuzz_abcdef12_1"}`}, fuzzSideRun{exit: 1}, "ERROR", "FAIL", fuzzScriptNotStarted},
		"baseline passes without a payload": {fuzzSideRun{}, fuzzSideRun{payload: both}, "ERROR", "PASS", fuzzBaseNoStream},
		// The stream ended between two tests: the runner did not run the second
		// one although the run passed.
		"baseline passes skipping a test": {fuzzSideRun{payload: one}, fuzzSideRun{payload: both}, "ERROR", "PASS", fuzzScriptTestsMissed},
		// The stream ended inside the first test: the baseline process ended
		// while it evaluated an input (a process.exit(0) under Jest), which is
		// behavior of the baseline code; the check stays PASS and its functions
		// are inconclusive.
		"baseline passes ending inside a test":   {fuzzSideRun{payload: inside}, fuzzSideRun{payload: both}, "PASS", "PASS", ""},
		"baseline fails ending inside a test":    {fuzzSideRun{exit: 1, payload: inside}, fuzzSideRun{payload: both}, "FAIL", "PASS", ""},
		"baseline fails after starting":          {fuzzSideRun{exit: 1, payload: one}, fuzzSideRun{payload: both}, "FAIL", "PASS", ""},
		"baseline fails with a rejected stream":  {fuzzSideRun{exit: 1, payload: coverageFrame("garbage")}, fuzzSideRun{payload: both}, "FAIL", "PASS", ""},
		"baseline passes with a rejected stream": {fuzzSideRun{payload: coverageFrame("garbage")}, fuzzSideRun{payload: both}, "ERROR", "PASS", fuzzBaseBadStream},
		"candidate fails without a payload":      {fuzzSideRun{payload: both}, fuzzSideRun{exit: 1, log: "SyntaxError: nothing"}, "PASS", "FAIL", ""},
		"candidate passes skipping a test":       {fuzzSideRun{payload: both}, fuzzSideRun{payload: one}, "PASS", "PASS", ""},
		// A log that claims a failure decides nothing for a passing run.
		"log text is ignored": {fuzzSideRun{log: "FAIL swiftproof: the harness did not start", payload: both}, fuzzSideRun{log: "Error: x", payload: both}, "PASS", "PASS", ""},
	} {
		h := scriptFixture(t)
		fakeFuzz(t, h, tc.base, tc.candidate)
		base, candidate, err := h.RunObserved(context.Background(), scriptRun(false))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if base.Check.Status != tc.baseStatus || candidate.Check.Status != tc.candStatus {
			t.Errorf("%s: %s / %s", name, base.Check.Status, candidate.Check.Status)
		}
		if tc.baseCause != "" && !strings.Contains(base.Check.Output, fuzzCausePrefix+tc.baseCause) {
			t.Errorf("%s: baseline output %q", name, base.Check.Output)
		}
		if tc.baseCause == "" && strings.Contains(base.Check.Output, fuzzCausePrefix) {
			t.Errorf("%s: unexpected cause in %q", name, base.Check.Output)
		}
	}
}

// AddFuzzEvidence accepts a record only with the runner its checks ran with.
func TestAddFuzzEvidenceRunnerMustMatchTheChecks(t *testing.T) {
	h := scriptFixture(t)
	fakeFuzz(t, h, fuzzSideRun{payload: coverageFrame("stream:started:started:")}, fuzzSideRun{payload: coverageFrame("stream:started:started:")})
	base, candidate, err := h.RunObserved(context.Background(), scriptRun(false))
	if err != nil {
		t.Fatal(err)
	}
	e := model.Evidence{Kind: model.EvidenceDifferentialFuzz, Description: "d", Path: scriptFuzzPath, CheckID: candidate.Check.ID, BaseCheckID: base.Check.ID,
		Status: model.StatusNotDiverged, Runner: RunnerJest, TestNames: []string{fuzzNames[0]}}
	if _, err := h.AddFuzzEvidence(e); err != nil {
		t.Fatal(err)
	}
	e.Runner = RunnerGo
	if _, err := h.AddFuzzEvidence(e); err == nil {
		t.Fatal("a go_test_json record of TS/JS checks was accepted")
	}
	// A Go run's checks refuse a jest_json record.
	g := fixture(t)
	fakeFuzz(t, g, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:b")}, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:c")})
	gb, gc, err := g.RunObserved(context.Background(), fuzzRun(false, false))
	if err != nil {
		t.Fatal(err)
	}
	e.CheckID, e.BaseCheckID, e.Path, e.Runner = gc.Check.ID, gb.Check.ID, fuzzPath, RunnerJest
	if _, err := g.AddFuzzEvidence(e); err == nil {
		t.Fatal("a jest_json record of Go checks was accepted")
	}
	e.Runner = RunnerGo
	if _, err := g.AddFuzzEvidence(e); err != nil {
		t.Fatal(err)
	}
	if !VerifiableJSTemplate(scriptVitestTemplate) || VerifiableJSTemplate([]string{"go", "test", "{package}"}) {
		t.Fatal("VerifiableJSTemplate")
	}
}
