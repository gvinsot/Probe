package fuzz

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const testName = "TestSwiftProofFuzz_abcdef12_1"

var fuzzCommand = []string{"go", "test", "./p", "-json", "-count=1", "-run", "^(" + testName + ")$"}

// rec is one record of input i whose call is F(i) and whose encoding is enc.
func rec(i int, enc string) Record {
	return Record{Index: i, Call: "F(" + strconv.Itoa(i) + ")", SHA256: sha(enc), Length: len(enc), Display: enc}
}

func unstableRec(i int, enc string) Record {
	r := rec(i, enc)
	r.Unstable = true
	return r
}

// encodings builds complete records from one encoding per input.
func encodings(values ...string) []Record {
	out := make([]Record, len(values))
	for i, v := range values {
		out[i] = rec(i, v)
	}
	return out
}

func completeFn(planned int, records []Record) FunctionStream {
	return FunctionStream{Test: testName, Planned: planned, State: StateComplete, At: -1, Records: records}
}

func results(t *testing.T, fns ...FunctionStream) string {
	t.Helper()
	out, err := encodeStream(Stream{Version: StreamVersion, Scheme: model.FuzzSeedScheme, Display: 64, Functions: fns})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseResults(string(out)); err != nil {
		t.Fatalf("test stream invalid: %v", err)
	}
	return string(out)
}

// goLog is a go test -json log recording one run and one pass of each name.
func goLog(pkg string, names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", pkg)
	for _, n := range names {
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", pkg, n)
		fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Test":%q,"Output":"=== RUN   %s\n"}`+"\n", pkg, n, n)
		fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Test":%q,"Elapsed":0}`+"\n", pkg, n)
	}
	fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Elapsed":0}`+"\n", pkg)
	return b.String()
}

func fuzzCheck(id, kind, results string) *model.Check {
	return &model.Check{ID: id, Kind: kind, Status: "PASS", Command: append([]string(nil), fuzzCommand...), Output: goLog("example.test/p", testName), Results: results}
}

// pair builds the four checks from four function streams (nil: no check).
func pair(t *testing.T, b1, c1, b2, c2 *FunctionStream) Checks {
	t.Helper()
	var c Checks
	if b1 != nil {
		c.Base = fuzzCheck("check-1", model.CheckFuzzBase, results(t, *b1))
	}
	if c1 != nil {
		c.Candidate = fuzzCheck("check-2", model.CheckFuzzCandidate, results(t, *c1))
	}
	if b2 != nil {
		c.BaseConfirm = fuzzCheck("check-3", model.CheckFuzzBaseConfirm, results(t, *b2))
	}
	if c2 != nil {
		c.CandidateConfirm = fuzzCheck("check-4", model.CheckFuzzCandidateConfirm, results(t, *c2))
	}
	return c
}

func fnp(f FunctionStream) *FunctionStream { return &f }

func checkInvariant(t *testing.T, ev Evaluation) {
	t.Helper()
	if ev.Inputs != ev.Compared+ev.Unstable+ev.Unconfirmed+ev.NotRecorded || ev.Diverged > ev.Compared {
		t.Fatalf("count invariant broken: %+v", ev)
	}
	if (ev.Outcome == model.FuzzDiverged) != (ev.Counterexample != nil) {
		t.Fatalf("counterexample iff diverged: %+v", ev)
	}
	if (ev.Outcome == model.FuzzInconclusive) != (ev.Reason != "") {
		t.Fatalf("reason iff inconclusive: %+v", ev)
	}
}

func TestEvaluateOutcomes(t *testing.T) {
	same := completeFn(3, encodings("int(0)", "int(1)", "int(2)"))
	differs := completeFn(3, encodings("int(0)", "int(9)", "int(8)"))
	stopped := FunctionStream{Test: testName, Planned: 3, State: StateStopped, Stop: StopTimeout, At: 2, AtCall: "F(2)", Records: encodings("int(0)", "int(1)")}
	stoppedDiff := FunctionStream{Test: testName, Planned: 3, State: StateStopped, Stop: StopTimeout, At: 2, AtCall: "F(2)", Records: encodings("int(7)", "int(1)")}
	unstableAll := completeFn(3, []Record{unstableRec(0, "a"), unstableRec(1, "b"), unstableRec(2, "c")})
	firstUnstable := completeFn(3, []Record{unstableRec(0, "int(0)"), rec(1, "int(1)"), rec(2, "int(2)")})
	baseMoves := completeFn(3, encodings("int(0)", "int(5)", "int(2)"))
	confirmUnstable := completeFn(3, []Record{rec(0, "int(0)"), unstableRec(1, "int(9)"), rec(2, "int(8)")})
	cases := []struct {
		name           string
		checks         Checks
		outcome        string
		reason         string
		compared, div  int
		unstable       int
		unconfirmed    int
		notRecorded    int
		counterexample int
	}{
		{"equal", pair(t, &same, &same, nil, nil), model.FuzzNotDiverged, "", 3, 0, 0, 0, 0, 0},
		{"confirmed divergence", pair(t, &same, &differs, &same, &differs), model.FuzzDiverged, "", 3, 2, 0, 0, 0, 1},
		{"no confirmation pair", pair(t, &same, &differs, nil, nil), model.FuzzInconclusive,
			"a difference seen in one run per revision could not be confirmed: the confirmation runs did not take place", 1, 0, 0, 2, 0, 0},
		{"baseline unstable across runs", pair(t, &same, &differs, &baseMoves, &differs), model.FuzzDiverged, "", 2, 1, 1, 0, 0, 2},
		{"candidate unstable across runs", pair(t, &same, &differs, &same, &same), model.FuzzInconclusive,
			"2 of 3 inputs were unstable on the candidate only, or differed between the revisions without repeating in the confirmation runs", 1, 0, 2, 0, 0, 0},
		{"instability flag in confirmation", pair(t, &same, &differs, &same, &confirmUnstable), model.FuzzDiverged, "", 2, 1, 1, 0, 0, 2},
		{"baseline instability flag in first pair", pair(t, &firstUnstable, &same, nil, nil), model.FuzzNotDiverged, "", 2, 0, 1, 0, 0, 0},
		{"instability flag on both sides", pair(t, &firstUnstable, &firstUnstable, nil, nil), model.FuzzNotDiverged, "", 2, 0, 1, 0, 0, 0},
		{"candidate-only instability flag", pair(t, &same, &firstUnstable, nil, nil), model.FuzzInconclusive,
			"1 of 3 inputs were unstable on the candidate only, or differed between the revisions without repeating in the confirmation runs", 2, 0, 1, 0, 0, 0},
		{"all unstable", pair(t, &unstableAll, &unstableAll, nil, nil), model.FuzzInconclusive, "all 3 inputs gave different observations on repeated evaluation", 0, 0, 3, 0, 0, 0},
		{"candidate stopped without difference", pair(t, &same, &stopped, nil, nil), model.FuzzInconclusive,
			"the candidate stopped (timeout) while evaluating input 2: F(2)", 2, 0, 0, 0, 1, 0},
		{"baseline stopped", pair(t, &stopped, &same, nil, nil), model.FuzzInconclusive,
			"the baseline stopped (timeout) while evaluating input 2: F(2)", 2, 0, 0, 0, 1, 0},
		{"divergence before a stop", pair(t, &same, &stoppedDiff, &same, &stoppedDiff), model.FuzzDiverged, "", 2, 1, 0, 0, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Evaluate(testName, 3, tc.checks)
			checkInvariant(t, ev)
			if ev.Outcome != tc.outcome || ev.Reason != tc.reason || ev.Compared != tc.compared || ev.Diverged != tc.div ||
				ev.Unstable != tc.unstable || ev.Unconfirmed != tc.unconfirmed || ev.NotRecorded != tc.notRecorded {
				t.Fatalf("got %+v", ev)
			}
			if tc.outcome == model.FuzzDiverged && ev.Counterexample.Index != tc.counterexample {
				t.Fatalf("counterexample %+v, want index %d", ev.Counterexample, tc.counterexample)
			}
		})
	}
}

func TestEvaluateCounterexampleAndRows(t *testing.T) {
	// 40 inputs; every input from 3 on differs. Calls F(3)..F(9) are the
	// shortest, and among them the lowest index wins.
	n := 40
	base, cand := make([]Record, n), make([]Record, n)
	for i := 0; i < n; i++ {
		base[i] = rec(i, "int("+strconv.Itoa(i)+")")
		cand[i] = rec(i, "int("+strconv.Itoa(i)+")")
		if i >= 3 {
			cand[i] = rec(i, "int(-"+strconv.Itoa(i)+")")
		}
	}
	b, c := completeFn(n, base), completeFn(n, cand)
	ev := Evaluate(testName, n, pair(t, &b, &c, &b, &c))
	checkInvariant(t, ev)
	if ev.Outcome != model.FuzzDiverged || ev.Diverged != 37 {
		t.Fatalf("ev %+v", ev)
	}
	want := model.FuzzCounterexample{Index: 3, Input: "F(3)", Base: "int(3)", Candidate: "int(-3)"}
	if *ev.Counterexample != want {
		t.Fatalf("counterexample %+v", ev.Counterexample)
	}
	rows := ev.Observations(testName)
	if len(rows) != MaxDivergenceRows || rows[0].Key != "F(3)" || rows[6].Key != "F(9)" || rows[7].Key != "F(10)" {
		t.Fatalf("rows %d, first %+v", len(rows), rows[:8])
	}
	for _, r := range rows {
		if r.Status != model.ObservationDiverged || r.Test != testName || !r.BaseRecorded || !r.CandidateRecorded || r.Truncated {
			t.Fatalf("row %+v", r)
		}
	}
	if got := ev.EvidenceStatus(); got != model.StatusDiverged {
		t.Fatalf("status %s", got)
	}
	// A display shorter than the recorded encoding marks the row truncated.
	long := rec(3, strings.Repeat("x", 100))
	long.Display = strings.Repeat("x", 64)
	cand[3] = long
	c = completeFn(n, cand)
	if rows := Evaluate(testName, n, pair(t, &b, &c, &b, &c)).Observations(testName); !rows[0].Truncated {
		t.Fatalf("row %+v", rows[0])
	}
	if (Evaluation{Outcome: model.FuzzNotDiverged}).EvidenceStatus() != model.StatusNotDiverged || (Evaluation{Outcome: model.FuzzInconclusive}).EvidenceStatus() != model.StatusUnverified {
		t.Fatal("status mapping")
	}
}

// TestEvaluatePrefersAVisibleCounterexample: a difference that lies in a cut
// or redacted part of the displays shows two identical values; such an input
// is not chosen as the counterexample while another divergent input shows its
// difference, and its row is marked as not showing the whole values.
func TestEvaluatePrefersAVisibleCounterexample(t *testing.T) {
	cutPair := func(i int, shown, baseTail, candTail string) (Record, Record) {
		b, c := rec(i, shown+baseTail), rec(i, shown+candTail)
		b.Display, c.Display = shown, shown
		return b, c
	}
	b0, c0 := cutPair(0, `string("x`, `a")`, `b")`)
	b1, c1 := rec(1, "int(1)"), rec(1, "int(2)")
	b2, c2 := rec(2, `string("x[REDACTED]h")`), rec(2, `string("x[REDACTED]h")`)
	b2.SHA256, b2.Length = sha(`string("x://a:b@h")`), len(`string("x://a:b@h")`)
	c2.SHA256, c2.Length = sha(`string("x://a:c@h")`), len(`string("x://a:c@h")`)
	base := completeFn(3, []Record{b0, b1, b2})
	cand := completeFn(3, []Record{c0, c1, c2})
	ev := Evaluate(testName, 3, pair(t, &base, &cand, &base, &cand))
	checkInvariant(t, ev)
	if ev.Outcome != model.FuzzDiverged || ev.Diverged != 3 || ev.Counterexample.Index != 1 || ev.CounterexampleCut {
		t.Fatalf("got %+v %+v", ev, ev.Counterexample)
	}
	rows := ev.Observations(testName)
	if len(rows) != 3 || rows[0].Key != "F(1)" || rows[0].Truncated || rows[1].Key != "F(0)" || !rows[1].Truncated || rows[2].Key != "F(2)" || !rows[2].Truncated {
		t.Fatalf("rows %+v", rows)
	}
	// Only invisible differences: the shortest call, marked as cut.
	base = completeFn(3, []Record{b0, rec(1, "int(1)"), b2})
	cand = completeFn(3, []Record{c0, rec(1, "int(1)"), c2})
	ev = Evaluate(testName, 3, pair(t, &base, &cand, &base, &cand))
	if ev.Outcome != model.FuzzDiverged || ev.Counterexample.Index != 0 || !ev.CounterexampleCut {
		t.Fatalf("got %+v %+v", ev, ev.Counterexample)
	}
	if out := evidenceOutput(ev, ""); !strings.Contains(out, "\n"+CounterexampleCutNote+"\n") {
		t.Fatalf("output lacks the cut note: %q", out)
	}
}

// TestEvaluateRequiresValidatedExecutions covers every way a recorded check
// can fail to support a comparison.
func TestEvaluateRequiresValidatedExecutions(t *testing.T) {
	same := completeFn(3, encodings("int(0)", "int(1)", "int(2)"))
	differs := completeFn(3, encodings("int(0)", "int(9)", "int(8)"))
	sameResults := results(t, same)
	edit := func(change func(c *Checks)) Checks {
		c := pair(t, &same, &same, nil, nil)
		change(&c)
		return c
	}
	withConfirm := func(change func(c *Checks)) Checks {
		c := pair(t, &same, &differs, &same, &differs)
		change(&c)
		return c
	}
	cases := []struct {
		name   string
		checks Checks
		reason string
	}{
		{"no baseline", edit(func(c *Checks) { c.Base = nil }), "the baseline run did not run"},
		{"log without pass", edit(func(c *Checks) { c.Base.Output = goLog("example.test/p") }), "the baseline run log does not record exactly one run and one pass of the fuzz test"},
		{"forged second pass", edit(func(c *Checks) { c.Candidate.Output += goLog("example.test/p", testName) }), "the candidate run log does not record exactly one run and one pass of the fuzz test"},
		{"truncated log", edit(func(c *Checks) { c.Base.Truncated = true }), "the baseline run log was truncated"},
		{"error status", edit(func(c *Checks) { c.Base.Status = "ERROR" }), "the baseline run ended with status ERROR"},
		{"timeout status", edit(func(c *Checks) { c.Candidate.Status = "TIMEOUT" }), "the candidate run ended with status TIMEOUT"},
		{"infrastructure exit code", edit(func(c *Checks) { c.Candidate.Status, c.Candidate.ExitCode = "FAIL", 137 }), "the candidate run ended with exit code 137"},
		{"pass with exit code", edit(func(c *Checks) { c.Base.ExitCode = 1 }), "the baseline run ended with exit code 1"},
		{"no stream", edit(func(c *Checks) { c.Candidate.Results = "" }), "the candidate run recorded no observation stream"},
		{"failed without a stream", edit(func(c *Checks) { c.Candidate.Results, c.Candidate.Status, c.Candidate.ExitCode = "", "FAIL", 1 }),
			"the candidate run failed without recording an observation stream (for example, the package did not build; see the check log)"},
		{"rejected stream", edit(func(c *Checks) { c.Candidate.Results = sameResults + " " }), "the candidate run observation stream was rejected"},
		{"swapped kinds", edit(func(c *Checks) { c.Base.Kind, c.Candidate.Kind = c.Candidate.Kind, c.Base.Kind }), "the baseline run is not a fuzz_base check"},
		{"replayed candidate", edit(func(c *Checks) { c.Candidate.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 5} }), "the candidate run was replayed from the execution cache; only live runs are accepted for this check"},
		{"replayed baseline with one live run", edit(func(c *Checks) { c.Base.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 1} }), "the baseline run was replayed from the execution cache without two agreeing live runs"},
		{"different commands", edit(func(c *Checks) { c.Candidate.Command = append(c.Candidate.Command, "-v") }), "the runs did not use one identical recorded command"},
		{"no command", edit(func(c *Checks) { c.Base.Command, c.Candidate.Command = nil, nil }), "a run recorded no command"},
		{"different packages", edit(func(c *Checks) { c.Candidate.Output = goLog("example.test/q", testName) }), "the runs recorded the fuzz test in different packages"},
		{"different planned count", edit(func(c *Checks) {
			c.Candidate.Results = results(t, completeFn(4, encodings("int(0)", "int(1)", "int(2)", "int(3)")))
		}), "the candidate run observation stream planned a different number of inputs"},
		{"other test", edit(func(c *Checks) {
			other := completeFn(3, encodings("a", "b", "c"))
			other.Test = "TestSwiftProofFuzz_abcdef12_2"
			c.Candidate.Results = results(t, other)
		}), "the candidate run observation stream has no records of the fuzz test"},
		{"different inputs", edit(func(c *Checks) {
			moved := completeFn(3, encodings("int(0)", "int(1)", "int(2)"))
			moved.Records[1].Call = "F(99)"
			c.Candidate.Results = results(t, moved)
		}), "the recorded streams do not describe the same inputs"},
		{"interrupted candidate", edit(func(c *Checks) {
			c.Candidate.Status, c.Candidate.ExitCode = "FAIL", 1
			c.Candidate.Output = strings.Replace(c.Candidate.Output, `{"Action":"pass","Package":"example.test/p","Test":"`+testName+`","Elapsed":0}`+"\n", "", 1)
			c.Candidate.Results = results(t, FunctionStream{Test: testName, Planned: 3, State: StateInterrupted, At: 1, AtCall: "F(1)", Records: encodings("int(0)")})
		}), "the candidate process ended while evaluating input 1: F(1)"},
		{"stream ended although the test passed", edit(func(c *Checks) {
			c.Candidate.Results = results(t, FunctionStream{Test: testName, Planned: 3, State: StateInterrupted, At: 1, AtCall: "F(1)", Records: encodings("int(0)")})
		}), "the candidate run observation stream ended although the fuzz test passed"},
		{"replayed confirmation", withConfirm(func(c *Checks) { c.BaseConfirm.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 9} }),
			"a difference seen in one run per revision could not be confirmed: the baseline confirmation run was replayed from the execution cache; only live runs are accepted for this check"},
		{"confirmation of the wrong kind", withConfirm(func(c *Checks) { c.CandidateConfirm.Kind = model.CheckFuzzCandidate }),
			"a difference seen in one run per revision could not be confirmed: the candidate confirmation run is not a fuzz_candidate_confirm check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Evaluate(testName, 3, tc.checks)
			checkInvariant(t, ev)
			if ev.Outcome != model.FuzzInconclusive || ev.Reason != tc.reason {
				t.Fatalf("got %s %q, want %q", ev.Outcome, ev.Reason, tc.reason)
			}
		})
	}
	// A baseline replayed from an entry two live runs agreed on may support
	// a negative outcome (the §1.11 rule); DIVERGED rests on the live
	// confirmation pair.
	agreed := edit(func(c *Checks) { c.Base.Cache = &model.CheckCache{Status: model.CacheHit, LiveRuns: 2} })
	if ev := Evaluate(testName, 3, agreed); ev.Outcome != model.FuzzNotDiverged {
		t.Fatalf("agreed replay: %+v", ev)
	}
	stored := withConfirm(func(c *Checks) { c.Base.Cache = &model.CheckCache{Status: model.CacheStored, LiveRuns: 1} })
	if ev := Evaluate(testName, 3, stored); ev.Outcome != model.FuzzDiverged {
		t.Fatalf("stored live baseline: %+v", ev)
	}
	// A FAIL check stays usable for a test that passed before its process
	// ended.
	failed := edit(func(c *Checks) { c.Candidate.Status, c.Candidate.ExitCode = "FAIL", 1 })
	if ev := Evaluate(testName, 3, failed); ev.Outcome != model.FuzzNotDiverged {
		t.Fatalf("FAIL check with a passing test: %+v", ev)
	}
	if ev := Evaluate(testName, 0, pair(t, &same, &same, nil, nil)); ev.Outcome != model.FuzzInconclusive || ev.Inputs != 0 {
		t.Fatalf("zero planned: %+v", ev)
	}
}

func TestEvaluateTamperedHashChangesTheOutcome(t *testing.T) {
	same := completeFn(3, encodings("int(0)", "int(1)", "int(2)"))
	differs := completeFn(3, encodings("int(0)", "int(9)", "int(2)"))
	checks := pair(t, &same, &differs, &same, &differs)
	if ev := Evaluate(testName, 3, checks); ev.Outcome != model.FuzzDiverged || ev.Counterexample.Index != 1 {
		t.Fatalf("before tampering: %+v", ev)
	}
	// Setting the candidate hash of the counterexample to the baseline's in
	// the first pair only: the first pair now agrees, so nothing diverges.
	tampered := strings.Replace(checks.Candidate.Results, sha("int(9)"), sha("int(1)"), 1)
	checks.Candidate.Results = tampered
	if ev := Evaluate(testName, 3, checks); ev.Outcome != model.FuzzNotDiverged || ev.Diverged != 0 {
		t.Fatalf("after tampering: %+v", ev)
	}
	// A hash replaced by a malformed one rejects the whole stream.
	checks.Candidate.Results = strings.Replace(tampered, sha("int(0)"), "00", 1)
	if ev := Evaluate(testName, 3, checks); ev.Outcome != model.FuzzInconclusive {
		t.Fatalf("malformed hash: %+v", ev)
	}
}
