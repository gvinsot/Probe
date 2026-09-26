// Package fuzz is the host side of deterministic differential fuzzing (F2).
// It selects changed Go functions whose signature is unchanged, derives a
// seeded input corpus from each function's identity, renders one observation
// harness per package, validates and normalizes the observation streams that
// sandbox runs return, and compares baseline and candidate streams.
//
// Nothing in this package executes repository code or involves a model.
// Selection and rendering parse and print Go source on the host (go/parser,
// go/format, go/build.MatchFile with in-memory files). Execution goes through
// a Runner, which the harness implements with the ordinary Docker sandbox; the
// harness never imports this package.
//
// A divergence is a difference between bounded canonical encodings recorded
// for the same seeded input, reproduced by a second run on each revision. It
// never says which revision is right. The absence of a divergence never
// establishes equivalent behavior, not even for the inputs that were tried:
// only results, recovered panics and slice arguments after the call are
// encoded, within fixed bounds, and code under test can write the stream of
// its own revision.
package fuzz

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// Report bounds.
const (
	// MaxSkipped bounds fuzz.skipped; fuzz.skipped_total counts every skip.
	MaxSkipped = 200
	// maxUnverified bounds the Unverified lines of one fuzz stage.
	maxUnverified = 20
	// maxEvidenceOutput bounds the readable output of one evidence record.
	maxEvidenceOutput = 1024
	// maxErrorText bounds a runner error quoted in a reason.
	maxErrorText = 200
)

// Reasons Run records itself.
const (
	ReasonRuntimeBudget = "fuzz.max_runtime_seconds or the overall deadline was reached before this package ran"
	ReasonNoCandidates  = "no changed Go function is eligible for differential fuzzing"
)

// CounterexampleCutNote accompanies a counterexample whose displays are not
// the whole recorded encodings (Evaluation.CounterexampleCut).
const CounterexampleCutNote = "(The values shown are cut or redacted displays; the recorded encodings differ.)"

// Limits are the effective limits of one fuzz stage.
type Limits struct {
	MaxFunctions int
	MaxPackages  int
	MaxInputs    int
	CallTimeout  time.Duration // bound of one evaluation of one input
	MaxRuntime   time.Duration // sub-cap of the whole stage inside the sandbox budget
}

// NewLimits converts effective policy values (config.Fuzz.Effective).
func NewLimits(f config.Fuzz) Limits {
	return Limits{
		MaxFunctions: f.MaxFunctions,
		MaxPackages:  f.MaxPackages,
		MaxInputs:    f.MaxInputs,
		CallTimeout:  time.Duration(f.CallTimeoutMS) * time.Millisecond,
		MaxRuntime:   time.Duration(f.MaxRuntimeSeconds) * time.Second,
	}
}

// Model returns the limits as the report records them.
func (l Limits) Model() model.FuzzLimits {
	return model.FuzzLimits{
		MaxFunctions:      l.MaxFunctions,
		MaxPackages:       l.MaxPackages,
		MaxInputs:         l.MaxInputs,
		CallTimeoutMS:     int(l.CallTimeout / time.Millisecond),
		MaxRuntimeSeconds: int(l.MaxRuntime / time.Second),
	}
}

// CommandSupported reports whether a generated_test template can run a fuzz
// harness: a verifiable single-package go test template whose target is
// {package}. A {file} target would compile the harness file alone, without
// the package it observes.
func CommandSupported(cmd []string) bool {
	if !harness.VerifiableGoTemplate(cmd) {
		return false
	}
	for _, arg := range cmd[2:] {
		if arg == "{package}" {
			return true
		}
	}
	return false
}

// Request asks a Runner to execute one rendered harness on both revisions:
// stage Harness.Content at Harness.Path in both snapshots (never overwriting a
// file), run the generated_test template for its package with -json -count=1
// -run ^(Harness.TestNames())$ and the payload channel on the observation
// path, pass the returned stream through Harness.Normalize into
// Check.Results, and remove the file again.
type Request struct {
	Harness    Harness
	Confirm    bool // false: fuzz_base then fuzz_candidate; true: fuzz_base_confirm then fuzz_candidate_confirm
	SaveSource bool // retain Harness.Content as the fuzz_harness artifact
	// Deadline is the end of the stage sub-cap: no run starts after it and
	// none may run past it.
	Deadline time.Time
}

// Side is one recorded run of a Request.
type Side struct {
	Check model.Check
	// OverflowSHA256 is set when the normalized stream exceeded what remained
	// of the report's results budget: the stream was kept only as the
	// fuzz_observations artifact with this SHA-256, and Check.Results is empty.
	OverflowSHA256 string
}

// Runner executes harnesses in the sandbox and records evidence. The harness
// satisfies it through a thin adapter in cli; models can call neither method.
type Runner interface {
	// Observe runs the request on the baseline and then on the candidate.
	// An error means no check was recorded.
	Observe(ctx context.Context, req Request) (base, candidate Side, err error)
	// AddEvidence records one differential_fuzz evidence record and returns it
	// as stored, with its ID.
	AddEvidence(e model.Evidence) (model.Evidence, error)
}

// Options are the per-run inputs of Run.
type Options struct {
	Limits           Limits
	ObservationsPath string // in-container path of the observation stream
	PayloadLimit     int    // harness.PayloadLimit(sandbox.max_output_bytes)
	// NewSuffix and Now are replaceable for tests; nil means NewSuffix and
	// time.Now.
	NewSuffix func() (string, error)
	Now       func() time.Time
}

// Run executes the plan package by package, sequentially and in plan order,
// so evidence IDs are deterministic. For each package it renders a harness
// with a fresh suffix and runs it on both revisions; when a first-pair
// difference appears and the sub-cap allows, it runs one confirmation pair.
// Every function then gets its Evaluate outcome and, when checks exist, one
// differential_fuzz evidence record. A package that the stage sub-cap or the
// context stops before it starts, or whose harness cannot be rendered or run,
// gives inconclusive functions without checks. Run never sets an exit code
// and never fails: problems become inconclusive outcomes with a reason.
func Run(ctx context.Context, runner Runner, plan Plan, o Options) model.FuzzReport {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	suffix := o.NewSuffix
	if suffix == nil {
		suffix = NewSuffix
	}
	rep := model.FuzzReport{
		Status: model.FuzzRan, SeedScheme: model.FuzzSeedScheme, Limits: o.Limits.Model(),
		Functions: []model.FuzzFunction{}, Skipped: []model.FuzzSkip{}, Note: model.FuzzNote,
	}
	skipped := append([]model.FuzzSkip(nil), plan.Skipped...)
	if plan.Targets() == 0 {
		rep.Status, rep.Reason = model.FuzzNoCandidates, ReasonNoCandidates
	}
	deadline := now().Add(o.Limits.MaxRuntime)
	for _, pkg := range plan.Packages {
		if ctx.Err() != nil || !now().Before(deadline) {
			for _, t := range pkg.Targets {
				rep.Functions = append(rep.Functions, notRun(t, ReasonRuntimeBudget))
			}
			continue
		}
		h, err := renderPackage(pkg, o, suffix)
		if err != nil {
			// A planned function without a recorded run is inconclusive, never
			// a silent skip: it gets an Unverified line like any other.
			for _, t := range pkg.Targets {
				rep.Functions = append(rep.Functions, notRun(t, "the fuzz harness could not be rendered: "+errorText(err)))
			}
			continue
		}
		rep.Functions = append(rep.Functions, runPackage(ctx, runner, h, deadline, now)...)
	}
	sort.SliceStable(skipped, func(i, j int) bool {
		if skipped[i].Path != skipped[j].Path {
			return skipped[i].Path < skipped[j].Path
		}
		return skipped[i].Line < skipped[j].Line
	})
	rep.SkippedTotal = len(skipped)
	if len(skipped) > MaxSkipped {
		skipped = skipped[:MaxSkipped]
	}
	rep.Skipped = append(rep.Skipped, skipped...)
	return rep
}

// renderPackage renders a harness, drawing a fresh suffix again in the
// (improbable) case of an identifier collision.
func renderPackage(pkg PackagePlan, o Options, suffix func() (string, error)) (Harness, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		s, err := suffix()
		if err != nil {
			return Harness{}, err
		}
		h, err := Render(pkg, RenderOptions{Suffix: s, ObservationsPath: o.ObservationsPath, PayloadLimit: o.PayloadLimit, CallTimeout: o.Limits.CallTimeout})
		if err == nil {
			return h, nil
		}
		last = err
		if !errors.Is(err, ErrCollision) {
			break
		}
	}
	return Harness{}, last
}

func runPackage(ctx context.Context, runner Runner, h Harness, deadline time.Time, now func() time.Time) []model.FuzzFunction {
	req := Request{Harness: h, SaveSource: true, Deadline: deadline}
	b1, c1, err := runner.Observe(ctx, req)
	if err == nil && (b1.Check.ID == "" || c1.Check.ID == "") {
		err = errors.New("the runner recorded no check")
	}
	if err != nil {
		out := make([]model.FuzzFunction, 0, len(h.Tests))
		for _, t := range h.Tests {
			fn := notRun(t.Target, "the fuzz harness could not run: "+errorText(err))
			fn.TestName = t.Name
			out = append(out, fn)
		}
		return out
	}
	differs := false
	first := ParseChecks(Checks{Base: &b1.Check, Candidate: &c1.Check})
	for _, t := range h.Tests {
		if first.Evaluate(t.Name, len(t.Inputs)).Differs {
			differs = true
		}
	}
	var b2, c2 *Side
	if differs && ctx.Err() == nil && now().Before(deadline) {
		req.Confirm, req.SaveSource = true, false
		base, candidate, err := runner.Observe(ctx, req)
		if err == nil && base.Check.ID != "" && candidate.Check.ID != "" {
			b2, c2 = &base, &candidate
		}
	}
	checks := Checks{Base: &b1.Check, Candidate: &c1.Check}
	ids := model.FuzzChecks{Base: b1.Check.ID, Candidate: c1.Check.ID}
	if b2 != nil {
		checks.BaseConfirm, checks.CandidateConfirm = &b2.Check, &c2.Check
		ids.BaseConfirm, ids.CandidateConfirm = b2.Check.ID, c2.Check.ID
	}
	recorded := ParseChecks(checks)
	out := make([]model.FuzzFunction, 0, len(h.Tests))
	for _, t := range h.Tests {
		ev := recorded.Evaluate(t.Name, len(t.Inputs))
		fn := function(t, ev)
		fnChecks := ids
		fn.Checks = &fnChecks
		if ev.Outcome == model.FuzzInconclusive {
			if sum := overflow(ev, b1, c1, b2, c2); sum != "" {
				fn.Reason, fn.ResultsSHA256 = "observation stream exceeded the report budget", sum
			}
		}
		e := model.Evidence{
			Kind:        model.EvidenceDifferentialFuzz,
			Description: fmt.Sprintf("Differential fuzzing of %s on %d seeded inputs (%s)", t.Target.Symbol, len(t.Inputs), model.FuzzSeedScheme),
			Path:        h.Path,
			Output:      evidenceOutput(ev, fn.Reason),
			CheckID:     c1.Check.ID,
			BaseCheckID: b1.Check.ID,
			Status:      ev.EvidenceStatus(),
			Runner:      harness.RunnerGo,
			TestNames:   []string{t.Name},
		}
		stored, err := runner.AddEvidence(e)
		if err != nil {
			fn.Outcome, fn.Counterexample = model.FuzzInconclusive, nil
			fn.Reason = "the evidence record could not be stored: " + errorText(err)
		} else {
			fn.EvidenceID = stored.ID
		}
		out = append(out, fn)
	}
	return out
}

// overflow returns the SHA-256 of a stream that was kept only as an artifact
// because of the results budget, when that stream was one the evaluation
// needed.
func overflow(ev Evaluation, b1, c1 Side, b2, c2 *Side) string {
	for _, s := range []Side{b1, c1} {
		if s.OverflowSHA256 != "" {
			return s.OverflowSHA256
		}
	}
	if ev.Unconfirmed > 0 && b2 != nil {
		for _, s := range []*Side{b2, c2} {
			if s.OverflowSHA256 != "" {
				return s.OverflowSHA256
			}
		}
	}
	return ""
}

// function builds the report entry of one evaluated fuzz test.
func function(t HarnessTest, ev Evaluation) model.FuzzFunction {
	return model.FuzzFunction{
		Path: t.Target.Path, Line: t.Target.Line, EndLine: t.Target.EndLine, Symbol: t.Target.Symbol,
		Signature: t.Target.Signature, TestName: t.Name, Outcome: ev.Outcome, Reason: ev.Reason,
		Counterexample: ev.Counterexample, Inputs: ev.Inputs, Compared: ev.Compared, Diverged: ev.Diverged,
		Unstable: ev.Unstable, Unconfirmed: ev.Unconfirmed, NotRecorded: ev.NotRecorded,
	}
}

// notRun is the entry of a function that has no recorded run.
func notRun(t Target, reason string) model.FuzzFunction {
	return model.FuzzFunction{
		Path: t.Path, Line: t.Line, EndLine: t.EndLine, Symbol: t.Symbol, Signature: t.Signature,
		Outcome: model.FuzzInconclusive, Reason: reason, Inputs: t.Inputs, NotRecorded: t.Inputs,
	}
}

// evidenceOutput is the bounded readable output of an evidence record.
func evidenceOutput(ev Evaluation, reason string) string {
	var text string
	switch ev.Outcome {
	case model.FuzzDiverged:
		c := ev.Counterexample
		note := ""
		if ev.CounterexampleCut {
			note = "\n" + CounterexampleCutNote
		}
		text = fmt.Sprintf("Input: %s\nBaseline: %s\nCandidate: %s%s\n%d of %d compared inputs recorded different values on baseline and candidate; each revision repeated its own value in a second run.",
			c.Input, c.Base, c.Candidate, note, ev.Diverged, ev.Compared)
	case model.FuzzNotDiverged:
		text = fmt.Sprintf("%d of %d inputs compared; baseline and candidate recorded equal encodings for each compared input.", ev.Compared, ev.Inputs)
	default:
		text = "Inconclusive: " + reason
	}
	return redact.TruncateUTF8(text, maxEvidenceOutput)
}

func errorText(err error) string {
	return redact.TruncateUTF8(redact.Redact(err.Error()), maxErrorText)
}

// Unverified returns the Unverified lines of a fuzz stage: one per
// inconclusive function and one for the functions cut by max_functions or
// max_packages, at most 20 lines in total.
func Unverified(rep model.FuzzReport, budgetSkipped int) []string {
	var lines []string
	inconclusive := 0
	for _, f := range rep.Functions {
		if f.Outcome == model.FuzzInconclusive {
			inconclusive++
		}
	}
	room := maxUnverified
	if budgetSkipped > 0 {
		room--
	}
	shown := 0
	for _, f := range rep.Functions {
		if f.Outcome != model.FuzzInconclusive {
			continue
		}
		if shown == room-1 && inconclusive > room {
			lines = append(lines, fmt.Sprintf("Differential fuzzing: %d more functions are inconclusive (see fuzz.functions in confidence-report.json).", inconclusive-shown))
			break
		}
		lines = append(lines, fmt.Sprintf("Differential fuzzing of %s is inconclusive: %s", f.Symbol, f.Reason))
		shown++
	}
	if budgetSkipped > 0 {
		lines = append(lines, fmt.Sprintf("Differential fuzzing did not run on %d changed functions because fuzz.max_functions or fuzz.max_packages was reached (see fuzz.skipped).", budgetSkipped))
	}
	return lines
}
