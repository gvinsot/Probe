package fuzz

import (
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

var scriptCommand = []string{"vitest", "run", "web/swiftproof-fuzz-abcdef12.test.ts", "--reporter=json", "--outputFile=/tmp/swiftproof-test-results.json"}

func scriptResults(t *testing.T, fns ...FunctionStream) string {
	t.Helper()
	out, err := encodeStream(Stream{Version: StreamVersion, Scheme: model.FuzzSeedScheme, Display: 64, Runner: harness.RunnerJest, Functions: fns})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseResults(string(out)); err != nil {
		t.Fatalf("test stream invalid: %v", err)
	}
	return string(out)
}

// scriptCheck is a PASS run of a TS/JS harness. Its log is cut and says
// nothing about the tests: the comparison never reads it.
func scriptCheck(id, kind, results string) *model.Check {
	return &model.Check{ID: id, Kind: kind, Status: "PASS", Command: append([]string(nil), scriptCommand...), Output: "vitest output (cut)", Truncated: true, Results: results}
}

func scriptPair(t *testing.T, b1, c1, b2, c2 *FunctionStream) Checks {
	t.Helper()
	c := Checks{Runner: harness.RunnerJest}
	if b1 != nil {
		c.Base = scriptCheck("check-1", model.CheckFuzzBase, scriptResults(t, *b1))
	}
	if c1 != nil {
		c.Candidate = scriptCheck("check-2", model.CheckFuzzCandidate, scriptResults(t, *c1))
	}
	if b2 != nil {
		c.BaseConfirm = scriptCheck("check-3", model.CheckFuzzBaseConfirm, scriptResults(t, *b2))
	}
	if c2 != nil {
		c.CandidateConfirm = scriptCheck("check-4", model.CheckFuzzCandidateConfirm, scriptResults(t, *c2))
	}
	return c
}

// The outcomes of a TS/JS harness are those of a Go harness: the same
// comparison over the same stream records; only the validation of the named
// execution differs.
func TestEvaluateScriptOutcomes(t *testing.T) {
	same := completeFn(3, encodings("0", "1", "2"))
	other := completeFn(3, encodings("0", "9", "2"))
	ev := Evaluate(testName, 3, scriptPair(t, &same, &same, nil, nil))
	checkInvariant(t, ev)
	if ev.Outcome != model.FuzzNotDiverged || ev.Compared != 3 {
		t.Fatalf("not diverged: %+v", ev)
	}
	ev = Evaluate(testName, 3, scriptPair(t, &same, &other, &same, &other))
	checkInvariant(t, ev)
	if ev.Outcome != model.FuzzDiverged || ev.Diverged != 1 || ev.Counterexample.Input != "F(1)" || ev.Counterexample.Candidate != "9" {
		t.Fatalf("diverged: %+v", ev)
	}
	ev = Evaluate(testName, 3, scriptPair(t, &same, &other, nil, nil))
	checkInvariant(t, ev)
	if ev.Outcome != model.FuzzInconclusive || ev.Unconfirmed != 1 || !ev.Differs {
		t.Fatalf("unconfirmed: %+v", ev)
	}
	// Go and TS/JS verdicts over the same records agree.
	goEv := Evaluate(testName, 3, pair(t, &same, &other, &same, &other))
	scriptEv := Evaluate(testName, 3, scriptPair(t, &same, &other, &same, &other))
	if goEv.Outcome != scriptEv.Outcome || goEv.Diverged != scriptEv.Diverged || *goEv.Counterexample != *scriptEv.Counterexample {
		t.Fatalf("go %+v, script %+v", goEv, scriptEv)
	}
}

// A TS/JS run is usable only when it passed with exit code 0 and its stream
// is a TS/JS stream whose test finished (head and done records); the runner
// must match the stream.
func TestEvaluateScriptRequiresPassingFramedRuns(t *testing.T) {
	same := completeFn(3, encodings("0", "1", "2"))
	interrupted := FunctionStream{Test: testName, Planned: 3, State: StateInterrupted, At: 1, AtCall: "F(1)", Records: encodings("0")}
	cases := map[string]struct {
		checks func() Checks
		reason string
	}{
		"failing candidate": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Candidate.Status, c.Candidate.ExitCode = "FAIL", 1
			return c
		}, "the candidate run ended with exit code 1; a TS/JS run supports a comparison only when it passes with exit code 0"},
		"candidate process ended": {func() Checks {
			c := scriptPair(t, &same, &interrupted, nil, nil)
			c.Candidate.Status, c.Candidate.ExitCode = "FAIL", 1
			return c
		}, "the candidate process ended while evaluating input 1: F(1)"},
		// A passing run whose stream ended inside the test: the process ended
		// while it evaluated an input (a process.exit(0) under Jest).
		"passing run ended inside the test": {func() Checks { return scriptPair(t, &same, &interrupted, nil, nil) },
			"the candidate process ended while evaluating input 1: F(1)"},
		"passing baseline ended inside the test": {func() Checks { return scriptPair(t, &interrupted, &same, nil, nil) },
			"the baseline process ended while evaluating input 1: F(1)"},
		// A later test of a stream that ended inside an earlier one did not start.
		"passing run ended inside an earlier test": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			earlier := interrupted
			earlier.Test = "TestSwiftProofFuzz_abcdef12_2"
			c.Candidate.Results = scriptResults(t, earlier, FunctionStream{Test: testName, Planned: 3, State: StateNotStarted, At: -1, Records: []Record{}})
			return c
		}, "the candidate process ended before this function was evaluated"},
		// A stream that ended between two tests: nothing says the process ended.
		"passing run ended after an earlier test": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			earlier := completeFn(3, encodings("0", "1", "2"))
			earlier.Test = "TestSwiftProofFuzz_abcdef12_2"
			c.Candidate.Results = scriptResults(t, earlier, FunctionStream{Test: testName, Planned: 3, State: StateNotStarted, At: -1, Records: []Record{}})
			return c
		}, "the candidate run observation stream has no done record of the fuzz test although the run passed"},
		"failing candidate without stream": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Candidate.Status, c.Candidate.ExitCode, c.Candidate.Results = "FAIL", 1, ""
			return c
		}, "the candidate run failed without recording an observation stream (for example, the module or the harness could not be loaded; see the check log)"},
		"go stream under the jest runner": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Candidate.Results = results(t, same)
			return c
		}, "the candidate run observation stream is not the stream of a TS/JS harness"},
		"script stream under the go runner": {func() Checks {
			c := pair(t, &same, &same, nil, nil)
			c.Candidate.Results = scriptResults(t, same)
			return c
		}, "the candidate run observation stream is not the stream of a Go harness"},
		"unknown runner": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Runner = "mocha_json"
			return c
		}, "the evidence names an unknown runner"},
		"replayed candidate": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Candidate.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 3}
			return c
		}, "the candidate run was replayed from the execution cache; only live runs are accepted for this check"},
		"baseline replayed once": {func() Checks {
			c := scriptPair(t, &same, &same, nil, nil)
			c.Base.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 1}
			return c
		}, "the baseline run was replayed from the execution cache without two agreeing live runs"},
	}
	for name, tc := range cases {
		ev := Evaluate(testName, 3, tc.checks())
		checkInvariant(t, ev)
		if ev.Outcome != model.FuzzInconclusive || ev.Reason != tc.reason {
			t.Errorf("%s: %s %q", name, ev.Outcome, ev.Reason)
		}
	}
	// A replayed baseline that two live runs agreed on still supports
	// not_diverged; a truncated log never matters.
	c := scriptPair(t, &same, &same, nil, nil)
	c.Base.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 2}
	if ev := Evaluate(testName, 3, c); ev.Outcome != model.FuzzNotDiverged {
		t.Fatalf("agreed replay: %+v", ev)
	}
	for name, tc := range cases {
		if m := forbidden.FindString(tc.reason); m != "" {
			t.Errorf("%s: %q contains %q", name, tc.reason, m)
		}
	}
}

// The TS/JS reasons and the harness texts make no claim either.
func TestScriptTextsMakeNoClaims(t *testing.T) {
	texts := []string{
		ReasonScriptTemplate, ReasonGoTemplate, ReasonScriptJestPath, ReasonScriptVitestExcluded, ReasonNoRunnable, GoTemplateUnverified(2),
		ReasonScriptGenerator, ReasonScriptCommonJS, ReasonScriptRenamed, ReasonScriptModuleName,
		ReasonScriptExportName, ReasonScriptThis, ReasonScriptDestructured, ReasonScriptJSDocDiffers, reasonScriptUntyped("a", true),
		reasonScriptUntyped("a", false), scriptHeader,
	}
	for _, line := range strings.Split(scriptRuntime, "\n") {
		if c := strings.TrimSpace(line); strings.HasPrefix(c, "//") {
			texts = append(texts, c)
		}
	}
	for _, text := range texts {
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("%q contains %q", text, m)
		}
	}
}
