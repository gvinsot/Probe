// Package mutation mutates added lines of changed non-test Go files and
// classifies each single-change mutant from recorded sandbox runs of the
// policy's per-package go test -json command.
//
// Mutant generation is host-side go/parser work over bounded snapshot bytes;
// repository code is never executed or type-checked here. Execution goes
// through a Workspace, which the harness implements over a private copy of the
// candidate snapshot inside the unchanged sandbox. Mutation creates no evidence
// record and no hypothesis, never computes a score, and may only add review
// signals: a surviving mutant is an observation about the recorded runs, not a
// defect.
package mutation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// Workspace executes the control and mutant runs. The harness implements it
// with a private copy of the candidate snapshot, the mutation check ledger and
// the unchanged sandbox; tests use a fake.
type Workspace interface {
	Source
	// RunControl runs command unchanged in the workspace (check kind
	// mutation_control) and returns the recorded check. An error means the
	// workspace can no longer be trusted and nothing more may run.
	RunControl(ctx context.Context, pkg string, command []string, timeout time.Duration) (model.Check, error)
	// RunMutant replaces the content of rel, which must still equal original,
	// by mutated, runs command (check kind mutant), and restores and re-checks
	// the original before it returns. It also returns the time limit the run
	// actually had: timeout, or less when the shared budget or the reviewer
	// reserve left less (0 when nothing ran). An error means the workspace
	// could not be verified or restored; a check recorded before that is
	// returned too.
	RunMutant(ctx context.Context, id, rel string, original, mutated []byte, command []string, timeout time.Duration) (model.Check, time.Duration, error)
	// SavePatch retains a redacted mutant_patch artifact and returns its sha256.
	SavePatch(name string, data []byte) (string, error)
}

// Config is the trusted mutation policy of one review.
type Config struct {
	Command []string // policy argv with one standalone {package}
	Limits  model.MutationLimits
	// NotExecuted, when set, returns the added lines of a file that a passing,
	// measured coverage run reported as not executed; their sites are skipped.
	NotExecuted func(path string) []int
}

// Result is what one mutation stage produced. Section.Checks is left empty:
// the caller copies the harness's mutation ledger into it.
type Result struct {
	Section     model.Mutation
	Signals     []model.Signal
	Unverified  string // "" unless the section is not_run or incomplete
	Operational bool   // an infrastructure error or a workspace failure: exit 4
}

// Fixed texts.
const (
	reasonNoCandidates   = "no candidate mutant on added lines of changed non-test Go files"
	reasonCoverageOnly   = "every candidate mutant was on an added line that the coverage run did not execute"
	reasonBudget         = "mutation.max_runtime_seconds does not leave room for another run of this package"
	reasonBudgetCut      = "a runtime budget (mutation.max_runtime_seconds, the sandbox budget or the reviewer reserve) left this run less than its own time limit and the run used all of it; this is not a timeout of the mutant"
	reasonBudgetSpent    = "a runtime budget ended an earlier mutant run before its own time limit; no further run was attempted"
	reasonCancelled      = "the time limit of the review or a cancellation stopped mutation before this run"
	reasonStoppedRun     = "the time limit of the review or a cancellation stopped this run before it finished"
	reasonAborted        = "the mutation workspace could not be verified or restored; no further run was attempted"
	reasonWorkspaceError = "the mutation workspace reported an error before the run"
	reasonPatchFailed    = "the mutant survived, but its patch artifact could not be retained, so the outcome cannot be re-verified"
	reasonApplyFailed    = "the mutant could not be applied: "

	survivorSummary = "With a single-change mutant of this added line, no test that the mutation command ran for its package failed"
	survivorText    = "Mutant %s (%s) replaced %q with %q. The unmutated control run %s and the mutant run %s of %q both passed; %d named tests passed in package %s and none failed (skipped tests are not counted). Only the tests this command ran for this package directory were run. A surviving mutant can be semantically equivalent to the original code; this is not evidence of a defect, of a missing test, or that the line was not executed. Patch artifact sha256 %s."
)

// displayLimit is the display cut of Original and Mutated (UTF-8 safe).
const displayLimit = 256

// Run plans, executes and classifies the mutants of change. It never returns
// an error: every problem is recorded as a mutant or section status with a
// reason, and Operational reports an infrastructure or workspace failure.
func Run(ctx context.Context, w Workspace, cfg Config, change model.Change) Result {
	section := model.Mutation{Command: redactAll(cfg.Command), Limits: cfg.Limits, Files: []model.MutationFile{}, Mutants: []model.Mutant{}, Checks: []model.Check{}, Note: model.MutationNote}
	plan := NewPlan(w, change, cfg.Limits.MaxMutants, cfg.NotExecuted)
	section.Files = append(section.Files, plan.Files...)
	section.Generated = plan.Generated
	section.CoverageSkipped = plan.CoverageSkipped
	section.Dropped = plan.Generated - len(plan.Selected)
	if len(plan.Selected) == 0 {
		section.Status, section.Reason = model.MutationNoCandidates, reasonNoCandidates
		if plan.Generated == 0 && plan.CoverageSkipped > 0 {
			section.Reason = reasonCoverageOnly
		}
		return Result{Section: section, Signals: []model.Signal{}}
	}
	mutants := make([]model.Mutant, len(plan.Selected))
	for i, s := range plan.Selected {
		mutants[i] = model.Mutant{ID: fmt.Sprintf("mutant-%d", i+1), Path: s.Path, Line: s.Line, Column: s.Column, Symbol: s.Symbol, Package: PackageArg(s.Path), Operator: s.Operator, Original: display(s.Original), Mutated: display(s.Replacement), Status: model.MutantNotRun}
		if s.EndLine > s.Line {
			mutants[i].EndLine = s.EndLine
		}
	}
	e := executor{ctx: ctx, w: w, cfg: cfg, plan: plan, mutants: mutants}
	e.execute()
	section.Mutants = mutants
	Recount(&section)
	switch {
	case !e.executed:
		section.Status, section.Reason = model.MutationNotRun, e.firstReason()
	case section.Inconclusive+section.NotRun == 0 && section.Dropped == 0:
		section.Status = model.MutationRan
	default:
		section.Status, section.Reason = model.MutationIncomplete, IncompleteReason(section)
	}
	result := Result{Section: section, Signals: e.signals, Operational: e.operational}
	if result.Signals == nil {
		result.Signals = []model.Signal{}
	}
	switch section.Status {
	case model.MutationNotRun:
		result.Unverified = "Mutation analysis did not run: " + section.Reason
	case model.MutationIncomplete:
		result.Unverified = "Mutation analysis is incomplete: " + section.Reason
	}
	return result
}

// Recount recomputes the per-status counters from the mutants.
func Recount(m *model.Mutation) {
	m.Killed, m.Survived, m.Invalid, m.TimedOut, m.Inconclusive, m.NotRun = 0, 0, 0, 0, 0, 0
	for _, mu := range m.Mutants {
		switch mu.Status {
		case model.MutantKilled:
			m.Killed++
		case model.MutantSurvived:
			m.Survived++
		case model.MutantInvalid:
			m.Invalid++
		case model.MutantTimeout:
			m.TimedOut++
		case model.MutantInconclusive:
			m.Inconclusive++
		case model.MutantNotRun:
			m.NotRun++
		}
	}
}

// IncompleteReason states why a section that executed is incomplete, with
// counts only.
func IncompleteReason(m model.Mutation) string {
	var parts []string
	if n := m.Inconclusive + m.NotRun; n > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d selected mutants have no outcome (%d not run, %d inconclusive)", n, len(m.Mutants), m.NotRun, m.Inconclusive))
	}
	if m.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d candidate mutants were not run because of max_mutants (%d)", m.Dropped, m.Generated, m.Limits.MaxMutants))
	}
	return strings.Join(parts, "; ")
}

// display is the bounded, redacted display form of a span.
func display(s string) string {
	return redact.TruncateUTF8(redact.Redact(s), displayLimit)
}

func redactAll(argv []string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = redact.Redact(arg)
	}
	return out
}
