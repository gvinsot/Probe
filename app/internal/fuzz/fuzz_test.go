package fuzz

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// behavior says what one revision records for input i of a test: an
// encoding, an instability flag, or a stop ("timeout", "goexit", "abort", or
// "exit" when the process ends).
type behavior func(side string, confirm bool, test HarnessTest, i int) (enc string, unstable bool, stop string)

// fakeRunner simulates the sandbox: it writes the raw stream the harness
// would write for each revision and records checks and evidence like the
// harness does.
type fakeRunner struct {
	t           *testing.T
	behave      behavior
	observeErr  error
	confirmErr  error
	evidenceErr error
	overflow    map[string]string // check kind -> SHA-256 of a stream kept only as an artifact
	onObserve   func(Request)
	requests    []Request
	checks      []model.Check
	evidence    []model.Evidence
}

func (f *fakeRunner) Observe(ctx context.Context, req Request) (Side, Side, error) {
	f.requests = append(f.requests, req)
	if f.onObserve != nil {
		f.onObserve(req)
	}
	if req.Confirm && f.confirmErr != nil {
		return Side{}, Side{}, f.confirmErr
	}
	if !req.Confirm && f.observeErr != nil {
		return Side{}, Side{}, f.observeErr
	}
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return f.side("base", baseKind, req), f.side("candidate", candidateKind, req), nil
}

func (f *fakeRunner) side(name, kind string, req Request) Side {
	h := req.Harness
	var lines, passed []string
	exited, poisoned := false, false
	for n, test := range h.Tests {
		if exited {
			break
		}
		fi := n + 1
		lines = append(lines, beginLine(fi, len(test.Inputs)))
		if poisoned {
			lines = append(lines, stopLine(fi, 0, StopPoisoned))
			passed = append(passed, test.Name)
			continue
		}
		stopped := false
		for i := range test.Inputs {
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
		}
		if exited {
			break
		}
		if !stopped {
			lines = append(lines, endLine(fi, len(test.Inputs)))
		}
		passed = append(passed, test.Name)
	}
	results, err := h.Normalize(rawStream(lines...))
	if err != nil {
		f.t.Fatalf("fake stream rejected: %v", err)
	}
	c := model.Check{
		ID: "check-" + strconv.Itoa(len(f.checks)+1), Kind: kind, Status: "PASS",
		Command: []string{"go", "test", "./" + h.Tests[0].Target.Dir, "-json", "-count=1", "-run", "^(" + strings.Join(h.TestNames(), "|") + ")$"},
		Output:  goLog("example.test/"+h.Tests[0].Target.Dir, passed...), Results: results,
	}
	if exited {
		c.Status, c.ExitCode = "FAIL", 1
	}
	s := Side{Check: c}
	if sum := f.overflow[kind]; sum != "" {
		s.Check.Results, s.OverflowSHA256 = "", sum
	}
	f.checks = append(f.checks, s.Check)
	return s
}

func (f *fakeRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	if f.evidenceErr != nil {
		return model.Evidence{}, f.evidenceErr
	}
	e.ID = "evidence-" + strconv.Itoa(len(f.evidence)+1)
	f.evidence = append(f.evidence, e)
	return e, nil
}

// behaviorByName applies one behavior per target name; unknown names record
// the same value on both revisions.
func behaviorByName(m map[string]behavior) behavior {
	return func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
		if b, ok := m[test.Target.Name]; ok {
			return b(side, confirm, test, i)
		}
		return "v(" + test.Inputs[i].Call + ")", false, ""
	}
}

func options() Options {
	return Options{
		Limits:           Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 8, CallTimeout: time.Second, MaxRuntime: 240 * time.Second},
		ObservationsPath: "/tmp/swiftproof-observations.jsonl",
		PayloadLimit:     harness.PayloadLimit(32 * 1024),
		NewSuffix:        func() (string, error) { return "abcdef12", nil },
	}
}

func onePackagePlan(targets ...Target) Plan {
	return Plan{Packages: []PackagePlan{obsPlan(targets...)}, Skipped: []model.FuzzSkip{}}
}

func TestRunConfirmsADivergenceOnce(t *testing.T) {
	f := &fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"A": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			if side == "candidate" && i == 1 {
				return "int(-1)", false, ""
			}
			return "int(" + strconv.Itoa(i) + ")", false, ""
		},
	})}
	plan := onePackagePlan(target("A", 1, 4, scalar("int")), target("B", 1, 4, scalar("string")))
	rep := Run(context.Background(), f, plan, options())
	if len(f.requests) != 2 || f.requests[0].Confirm || !f.requests[0].SaveSource || !f.requests[1].Confirm || f.requests[1].SaveSource {
		t.Fatalf("requests %+v", f.requests)
	}
	if f.requests[0].Harness.Content != f.requests[1].Harness.Content || f.requests[0].Deadline.IsZero() {
		t.Fatal("the confirmation must run the identical harness within the sub-cap")
	}
	if rep.Status != model.FuzzRan || rep.SeedScheme != model.FuzzSeedScheme || rep.Note != model.FuzzNote || rep.Skipped == nil || rep.SkippedTotal != 0 {
		t.Fatalf("report %+v", rep)
	}
	if rep.Limits != (model.FuzzLimits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 8, CallTimeoutMS: 1000, MaxRuntimeSeconds: 240}) {
		t.Fatalf("limits %+v", rep.Limits)
	}
	a, b := rep.Functions[0], rep.Functions[1]
	h := f.requests[0].Harness
	wantChecks := model.FuzzChecks{Base: "check-1", Candidate: "check-2", BaseConfirm: "check-3", CandidateConfirm: "check-4"}
	if a.Outcome != model.FuzzDiverged || a.EvidenceID != "evidence-1" || *a.Checks != wantChecks || a.TestName != h.Tests[0].Name ||
		a.Inputs != 4 || a.Compared != 4 || a.Diverged != 1 || a.Symbol != "obs.A" || a.Path != "obs/obs.go" || a.Signature != "func(int)" {
		t.Fatalf("A = %+v", a)
	}
	if *a.Counterexample != (model.FuzzCounterexample{Index: 1, Input: "A(1)", Base: "int(1)", Candidate: "int(-1)"}) {
		t.Fatalf("counterexample %+v", a.Counterexample)
	}
	if b.Outcome != model.FuzzNotDiverged || b.EvidenceID != "evidence-2" || b.Counterexample != nil || b.Reason != "" {
		t.Fatalf("B = %+v", b)
	}
	e := f.evidence[0]
	if e.Kind != model.EvidenceDifferentialFuzz || e.Status != model.StatusDiverged || e.Runner != harness.RunnerGo || e.CheckID != "check-2" ||
		e.BaseCheckID != "check-1" || e.Path != h.Path || len(e.TestNames) != 1 || e.TestNames[0] != h.Tests[0].Name {
		t.Fatalf("evidence %+v", e)
	}
	if e.Output != "Input: A(1)\nBaseline: int(1)\nCandidate: int(-1)\n1 of 4 compared inputs recorded different values on baseline and candidate; each revision repeated its own value in a second run." {
		t.Fatalf("output %q", e.Output)
	}
	if e.Description != "Differential fuzzing of obs.A on 4 seeded inputs (swiftproof-fuzz/v1)" {
		t.Fatalf("description %q", e.Description)
	}
	if f.evidence[1].Status != model.StatusNotDiverged || f.evidence[1].Output != "4 of 4 inputs compared; baseline and candidate recorded equal encodings for each compared input." {
		t.Fatalf("evidence %+v", f.evidence[1])
	}
	// The recorded outcome is exactly what the checks re-derive.
	checks := Checks{Base: &f.checks[0], Candidate: &f.checks[1], BaseConfirm: &f.checks[2], CandidateConfirm: &f.checks[3]}
	if ev := Evaluate(a.TestName, a.Inputs, checks); ev.Outcome != a.Outcome || *ev.Counterexample != *a.Counterexample || ev.Diverged != a.Diverged {
		t.Fatalf("re-derived %+v", ev)
	}
	if lines := Unverified(rep, 0); len(lines) != 0 {
		t.Fatalf("unverified %v", lines)
	}
}

func TestRunWithoutDifferenceRunsOnce(t *testing.T) {
	f := &fakeRunner{t: t, behave: behaviorByName(nil)}
	rep := Run(context.Background(), f, onePackagePlan(target("A", 1, 4, scalar("int"))), options())
	if len(f.requests) != 1 || rep.Functions[0].Outcome != model.FuzzNotDiverged {
		t.Fatalf("requests %d, %+v", len(f.requests), rep.Functions)
	}
	if c := rep.Functions[0].Checks; c.BaseConfirm != "" || c.CandidateConfirm != "" {
		t.Fatalf("checks %+v", c)
	}
}

func TestRunRecordsInconclusiveOutcomes(t *testing.T) {
	f := &fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"Stamp": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			return "t", true, ""
		},
		"Halt": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			if side == "candidate" && i == 2 {
				return "", false, "exit"
			}
			return "int(" + strconv.Itoa(i) + ")", false, ""
		},
	})}
	plan := onePackagePlan(target("Stamp", 1, 3, scalar("string")), target("Halt", 1, 4, scalar("int")), target("After", 1, 2, scalar("bool")))
	rep := Run(context.Background(), f, plan, options())
	stamp, halt, after := rep.Functions[0], rep.Functions[1], rep.Functions[2]
	if stamp.Outcome != model.FuzzInconclusive || stamp.Reason != "all 3 inputs gave different observations on repeated evaluation" || stamp.Unstable != 3 {
		t.Fatalf("Stamp = %+v", stamp)
	}
	if halt.Outcome != model.FuzzInconclusive || halt.Reason != "the candidate process ended while evaluating input 2: Halt(-1)" {
		t.Fatalf("Halt = %+v", halt)
	}
	if after.Outcome != model.FuzzInconclusive || after.Reason != "the candidate process ended before this function was evaluated" {
		t.Fatalf("After = %+v", after)
	}
	for i, e := range f.evidence {
		if e.Status != model.StatusUnverified || !strings.HasPrefix(e.Output, "Inconclusive: ") || e.CheckID != "check-2" {
			t.Fatalf("evidence %d %+v", i, e)
		}
	}
	if len(f.requests) != 1 {
		t.Fatal("no difference: no confirmation")
	}
	lines := Unverified(rep, 0)
	want := []string{
		"Differential fuzzing of obs.Stamp is inconclusive: all 3 inputs gave different observations on repeated evaluation",
		"Differential fuzzing of obs.Halt is inconclusive: the candidate process ended while evaluating input 2: Halt(-1)",
		"Differential fuzzing of obs.After is inconclusive: the candidate process ended before this function was evaluated",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unverified:\n%s", strings.Join(lines, "\n"))
	}
}

func TestRunTimeoutPoisonsLaterFunctions(t *testing.T) {
	f := &fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"Loop": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			if i == 1 {
				return "", false, StopTimeout
			}
			return "x", false, ""
		},
	})}
	rep := Run(context.Background(), f, onePackagePlan(target("Loop", 1, 3, scalar("uint8")), target("After", 1, 2, scalar("bool"))), options())
	if r := rep.Functions[0].Reason; r != "the baseline stopped (timeout) while evaluating input 1: Loop(1)" {
		t.Fatalf("Loop reason %q", r)
	}
	if r := rep.Functions[1].Reason; r != "the baseline skipped this function because an earlier call in the same process exceeded call_timeout_ms" {
		t.Fatalf("After reason %q", r)
	}
}

func TestRunObserveErrorAndBudget(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	o := options()
	o.Now = func() time.Time { return now }
	other := obsPlan(target("C", 1, 2, scalar("int")))
	other.Dir = "other"
	other.Targets[0].Dir, other.Targets[0].Path, other.Targets[0].Symbol = "other", "other/other.go", "other.C"
	third := obsPlan(target("D", 1, 2, scalar("int")))
	third.Dir = "third"
	third.Targets[0].Dir, third.Targets[0].Path, third.Targets[0].Symbol = "third", "third/third.go", "third.D"
	plan := Plan{Packages: []PackagePlan{obsPlan(target("A", 1, 2, scalar("int")), target("B", 1, 2, scalar("int"))), other, third}}
	calls := 0
	f := &fakeRunner{t: t, behave: behaviorByName(nil), onObserve: func(Request) {
		calls++
		if calls == 2 {
			now = now.Add(241 * time.Second) // the second package uses up the sub-cap
		}
	}}
	f.observeErr = errors.New("staging failed: password=hunter2")
	rep := Run(context.Background(), f, plan, o)
	if len(rep.Functions) != 4 || len(f.evidence) != 0 {
		t.Fatalf("functions %+v, evidence %+v", rep.Functions, f.evidence)
	}
	for _, fn := range rep.Functions[:3] {
		if fn.Outcome != model.FuzzInconclusive || fn.Reason != "the fuzz harness could not run: staging failed: [REDACTED]" || fn.Checks != nil || fn.EvidenceID != "" || fn.TestName == "" || fn.NotRecorded != fn.Inputs {
			t.Fatalf("function %+v", fn)
		}
	}
	d := rep.Functions[3]
	if d.Outcome != model.FuzzInconclusive || d.Reason != ReasonRuntimeBudget || d.TestName != "" || d.Checks != nil || d.Symbol != "third.D" {
		t.Fatalf("budget function %+v", d)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests %d", len(f.requests))
	}
	// A cancelled context runs nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &fakeRunner{t: t, behave: behaviorByName(nil)}
	rep = Run(ctx, f, plan, options())
	if len(f.requests) != 0 || rep.Functions[0].Reason != ReasonRuntimeBudget {
		t.Fatalf("cancelled: %+v", rep.Functions)
	}
}

func TestRunSkipsTheConfirmationPastTheDeadline(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	o := options()
	o.Now = func() time.Time { return now }
	f := &fakeRunner{t: t, behave: behaviorByName(map[string]behavior{
		"A": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			return side, false, ""
		},
	}), onObserve: func(Request) { now = now.Add(300 * time.Second) }}
	rep := Run(context.Background(), f, onePackagePlan(target("A", 1, 2, scalar("int"))), o)
	a := rep.Functions[0]
	if len(f.requests) != 1 || a.Outcome != model.FuzzInconclusive || a.Unconfirmed != 2 ||
		a.Reason != "a difference seen in one run per revision could not be confirmed: the confirmation runs did not take place" {
		t.Fatalf("A = %+v (requests %d)", a, len(f.requests))
	}
	// A confirmation error has the same effect.
	f = &fakeRunner{t: t, behave: f.behave, confirmErr: errors.New("docker unavailable")}
	rep = Run(context.Background(), f, onePackagePlan(target("A", 1, 2, scalar("int"))), options())
	if len(f.requests) != 2 || rep.Functions[0].Unconfirmed != 2 || rep.Functions[0].Checks.BaseConfirm != "" {
		t.Fatalf("confirmation error: %+v", rep.Functions[0])
	}
}

func TestRunEvidenceErrorAndOverflow(t *testing.T) {
	differ := behaviorByName(map[string]behavior{
		"A": func(side string, confirm bool, test HarnessTest, i int) (string, bool, string) {
			return side, false, ""
		},
	})
	f := &fakeRunner{t: t, behave: differ, evidenceErr: errors.New("check kinds do not match")}
	rep := Run(context.Background(), f, onePackagePlan(target("A", 1, 2, scalar("int"))), options())
	a := rep.Functions[0]
	if a.Outcome != model.FuzzInconclusive || a.Counterexample != nil || a.EvidenceID != "" || a.Reason != "the evidence record could not be stored: check kinds do not match" {
		t.Fatalf("A = %+v", a)
	}
	sum := sha("stream")
	f = &fakeRunner{t: t, behave: behaviorByName(nil), overflow: map[string]string{model.CheckFuzzCandidate: sum}}
	rep = Run(context.Background(), f, onePackagePlan(target("A", 1, 2, scalar("int"))), options())
	a = rep.Functions[0]
	if a.Outcome != model.FuzzInconclusive || a.Reason != "observation stream exceeded the report budget" || a.ResultsSHA256 != sum || a.EvidenceID != "evidence-1" {
		t.Fatalf("A = %+v", a)
	}
	if f.evidence[0].Output != "Inconclusive: observation stream exceeded the report budget" {
		t.Fatalf("output %q", f.evidence[0].Output)
	}
	// An overflow of a stream the outcome did not need changes nothing.
	f = &fakeRunner{t: t, behave: behaviorByName(nil), overflow: map[string]string{model.CheckFuzzCandidateConfirm: sum}}
	if rep = Run(context.Background(), f, onePackagePlan(target("A", 1, 2, scalar("int"))), options()); rep.Functions[0].Outcome != model.FuzzNotDiverged {
		t.Fatalf("A = %+v", rep.Functions[0])
	}
}

func TestRunWithoutCandidatesAndSkips(t *testing.T) {
	var skipped []model.FuzzSkip
	for i := 0; i < 205; i++ {
		skipped = append(skipped, model.FuzzSkip{Path: fmt.Sprintf("p/f%03d.go", i), Line: 1, Symbol: "p.F", Reason: ReasonMethod})
	}
	f := &fakeRunner{t: t}
	rep := Run(context.Background(), f, Plan{Skipped: skipped}, options())
	if rep.Status != model.FuzzNoCandidates || rep.Reason != ReasonNoCandidates || len(rep.Skipped) != MaxSkipped || rep.SkippedTotal != 205 ||
		rep.Skipped[0].Path != "p/f000.go" || len(rep.Functions) != 0 || rep.Functions == nil || len(f.requests) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestRunRenderFailuresAndSuffixRetry(t *testing.T) {
	bad := obsPlan(target("A", 1, 2, scalar("int")))
	delete(bad.Idents, "A")
	good := obsPlan(target("B", 1, 2, scalar("int")))
	good.Dir = "good"
	good.Targets[0].Dir, good.Targets[0].Path = "good", "good/good.go"
	good.Idents["swiftproofFuzz0123abcd_run"] = true
	suffixes := []string{"0123abcd", "0123abcd", "89abcdef"}
	o := options()
	o.NewSuffix = func() (string, error) {
		s := suffixes[0]
		suffixes = suffixes[1:]
		return s, nil
	}
	f := &fakeRunner{t: t, behave: behaviorByName(nil)}
	rep := Run(context.Background(), f, Plan{Packages: []PackagePlan{bad, good}}, o)
	// A function whose harness cannot be rendered has no recorded run: it is
	// inconclusive, never a silent skip.
	a := rep.Functions[0]
	if rep.SkippedTotal != 0 || len(rep.Functions) != 2 || a.Symbol != "obs.A" || a.Outcome != model.FuzzInconclusive ||
		!strings.HasPrefix(a.Reason, "the fuzz harness could not be rendered: ") || a.Checks != nil || a.EvidenceID != "" || a.NotRecorded != a.Inputs {
		t.Fatalf("functions %+v, skipped %+v", rep.Functions, rep.Skipped)
	}
	if rep.Functions[1].Outcome != model.FuzzNotDiverged || len(f.requests) != 1 || f.requests[0].Harness.Suffix != "89abcdef" {
		t.Fatalf("functions %+v", rep.Functions)
	}
	if lines := Unverified(rep, 0); len(lines) != 1 || !strings.HasPrefix(lines[0], "Differential fuzzing of obs.A is inconclusive: the fuzz harness could not be rendered: ") {
		t.Fatalf("unverified %v", lines)
	}
	o.NewSuffix = func() (string, error) { return "", errors.New("no entropy") }
	rep = Run(context.Background(), &fakeRunner{t: t}, Plan{Packages: []PackagePlan{good}}, o)
	if rep.Status != model.FuzzRan || len(rep.Functions) != 1 || rep.Functions[0].Reason != "the fuzz harness could not be rendered: no entropy" {
		t.Fatalf("functions %+v", rep.Functions)
	}
	// A target without any recordable input (a hand-built plan; Select skips
	// it) makes its package inconclusive instead of crashing.
	empty := obsPlan(target("ghp_abcdefgh1", 1, 8, scalar("int")))
	rep = Run(context.Background(), &fakeRunner{t: t}, Plan{Packages: []PackagePlan{empty}}, options())
	if len(rep.Functions) != 1 || rep.Functions[0].Outcome != model.FuzzInconclusive || !strings.Contains(rep.Functions[0].Reason, ErrNoInput.Error()) {
		t.Fatalf("functions %+v", rep.Functions)
	}
}

func TestUnverifiedIsBounded(t *testing.T) {
	rep := model.FuzzReport{}
	for i := 0; i < 25; i++ {
		rep.Functions = append(rep.Functions, model.FuzzFunction{Symbol: "p.F" + strconv.Itoa(i), Outcome: model.FuzzInconclusive, Reason: "no input was compared"})
	}
	rep.Functions = append(rep.Functions, model.FuzzFunction{Symbol: "p.G", Outcome: model.FuzzDiverged})
	lines := Unverified(rep, 3)
	if len(lines) != maxUnverified {
		t.Fatalf("%d lines", len(lines))
	}
	if lines[17] != "Differential fuzzing of p.F17 is inconclusive: no input was compared" ||
		lines[18] != "Differential fuzzing: 7 more functions are inconclusive (see fuzz.functions in confidence-report.json)." ||
		lines[19] != "Differential fuzzing did not run on 3 changed functions because fuzz.max_functions or fuzz.max_packages was reached (see fuzz.skipped)." {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	if got := Unverified(model.FuzzReport{}, 0); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	rep.Functions = rep.Functions[:19]
	if got := Unverified(rep, 0); len(got) != 19 {
		t.Fatalf("19 fit: %d", len(got))
	}
}

func TestCommandSupportedAndLimits(t *testing.T) {
	for cmd, want := range map[string]bool{
		"go test {package}":                   true,
		"go test -race {package}":             true,
		"go test {file}":                      false,
		"go test ./...":                       false,
		"go test {package} {package}":         false,
		"go test -exec=x {package}":           false,
		"npx vitest run {file}":               false,
		"go vet {package}":                    false,
		"/usr/local/go/bin/go test {package}": true,
	} {
		if got := CommandSupported(strings.Fields(cmd)); got != want {
			t.Errorf("%s: %v", cmd, got)
		}
	}
	effective := (&config.Fuzz{}).Effective(config.Sandbox{MaxRuntimeSeconds: 600, TimeoutSeconds: 120})
	l := NewLimits(effective)
	if l.MaxFunctions != 8 || l.MaxPackages != 4 || l.MaxInputs != 64 || l.CallTimeout != time.Second || l.MaxRuntime != 240*time.Second {
		t.Fatalf("limits %+v", l)
	}
	if l.Model() != (model.FuzzLimits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeoutMS: 1000, MaxRuntimeSeconds: 240}) {
		t.Fatalf("model %+v", l.Model())
	}
}

// forbidden are the words the v0.4 documentation rules forbid about fuzz
// outcomes (§4, §5), and the percent sign.
var forbidden = regexp.MustCompile(`(?i)\b(tested|verified|safe|correct|approved|regression|bug|masked|contradicts|score|equivalen\w*)\b|%`)

// TestTextsMakeNoClaims checks every fixed text of the package and the texts
// the scenarios above produce.
func TestTextsMakeNoClaims(t *testing.T) {
	texts := []string{
		ReasonMethod, ReasonGeneric, ReasonSignature, ReasonConstrained, ReasonCgo, ReasonPackageName, ReasonPackageClause,
		ReasonShadowed, ReasonSensitive, ReasonMovedDir, ReasonNotInSnapshot, ReasonNoBody, ReasonDuplicate, ReasonBudgetPackages,
		ReasonBudgetFunctions, ReasonRuntimeBudget, ReasonNoCandidates, reasonParamType("map[string]int"), reasonNamedDiffers("T"),
		reasonUnreadable("x"), reasonUnparsable("x.go"), ReasonNoInput, ErrNoInput.Error(), CounterexampleCutNote,
		evidenceOutput(Evaluation{Outcome: model.FuzzDiverged, Diverged: 1, Compared: 2, Counterexample: &model.FuzzCounterexample{}}, ""),
		evidenceOutput(Evaluation{Outcome: model.FuzzDiverged, Diverged: 1, Compared: 2, Counterexample: &model.FuzzCounterexample{}, CounterexampleCut: true}, ""),
		evidenceOutput(Evaluation{Outcome: model.FuzzNotDiverged, Compared: 2, Inputs: 2}, ""),
	}
	// Every reason Evaluate can give, from the outcome table and the
	// validation cases.
	for _, ev := range []Evaluation{
		Evaluate(testName, 3, pair(t, fnp(completeFn(3, encodings("int(0)", "int(1)", "int(2)"))), fnp(completeFn(3, []Record{unstableRec(0, "int(0)"), rec(1, "int(1)"), rec(2, "int(2)")})), nil, nil)),
		Evaluate(testName, 3, pair(t, fnp(completeFn(3, encodings("int(0)", "int(1)", "int(2)"))), fnp(completeFn(3, encodings("int(0)", "int(9)", "int(2)"))), nil, nil)),
		Evaluate(testName, 3, pair(t, fnp(completeFn(3, encodings("int(0)", "int(1)", "int(2)"))), nil, nil, nil)),
	} {
		texts = append(texts, ev.Reason)
	}
	for _, c := range []*model.Check{
		{Kind: model.CheckFuzzCandidate, Cache: &model.CheckCache{Status: model.CacheHit, LiveRuns: 5}},
		{Kind: model.CheckFuzzBase, Cache: &model.CheckCache{Status: model.CacheHit, LiveRuns: 1}},
	} {
		texts = append(texts, view(c, nil, "candidate", c.Kind, testName, 3, c.Kind == model.CheckFuzzBase, "").reason())
	}
	texts = append(texts, Unverified(model.FuzzReport{Functions: []model.FuzzFunction{{Symbol: "p.F", Outcome: model.FuzzInconclusive, Reason: "x"}}}, 2)...)
	s := side{name: "candidate", stream: &FunctionStream{At: 1, AtCall: "F(1)"}}
	for _, stop := range []string{StopTimeout, StopGoexit, StopAbort, StopPoisoned} {
		s.stream.Stop = stop
		texts = append(texts, s.stopReason())
	}
	for _, state := range []string{StateInterrupted, StateNotStarted} {
		s.stream.State, s.ended = state, true
		texts = append(texts, s.reason())
	}
	for _, text := range texts {
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("%q contains %q", text, m)
		}
	}
	// The harness template makes no claim either.
	if m := forbidden.FindString(harnessHeader); m != "" {
		t.Errorf("harness header contains %q", m)
	}
}
