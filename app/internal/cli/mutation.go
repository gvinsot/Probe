package cli

// Mutation of added lines (F4). A configured mutation policy runs one control
// and then single-change mutants per package in a private copy of the
// candidate. Survivors become medium review signals; nothing else is added to
// the report outside the mutation section, and nothing is removed.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/linter"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/mutation"
)

// workspaceFailure is the not_run reason of a mutation stage whose private
// candidate copy could not be created. The OS error names host paths and is
// not recorded.
const workspaceFailure = "the mutation workspace could not be prepared"

// runMutation mutates added Go (or, with a Vitest or Jest command, TS/JS) lines and runs the policy's command once per
// mutant. It sets r.Mutation (with the mutation ledger), merges the survivor
// signals, appends the Unverified sentence of a not_run or incomplete section,
// and returns true on an operational failure: an infrastructure error of a
// mutation run, or a workspace that could not be created, verified or
// restored. covered is the coverage result only when a coverage run passed and
// was measured; its not-executed lines are skipped.
func runMutation(ctx context.Context, h *harness.Harness, cfg config.Config, change model.Change, covered coverage.Result, r *model.Report, errOut io.Writer) (operational bool) {
	if cfg.Mutation == nil {
		return false
	}
	lines := "Go"
	if mutation.ScriptCommand(cfg.Mutation.Command) {
		lines = "TypeScript/JavaScript"
	}
	fmt.Fprintf(errOut, "Running mutation analysis of added %s lines in isolated Docker sandboxes...\n", lines)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		r.Mutation = mutationSection(cfg, model.MutationNotRun, workspaceFailure)
		r.Unverified = append(r.Unverified, "Mutation analysis did not run: "+workspaceFailure)
		return true
	}
	defer w.Close()
	res := mutation.Run(ctx, w, mutation.Config{
		Command:     cfg.Mutation.Command,
		Limits:      mutationLimits(cfg),
		NotExecuted: covered.NotExecuted,
	}, change)
	section := res.Section
	section.Checks = h.MutationChecks()
	r.Mutation = &section
	// Survivors may only add review signals; nothing is deleted or lowered.
	r.Signals = linter.Merge(r.Signals, res.Signals)
	if res.Unverified != "" {
		r.Unverified = append(r.Unverified, res.Unverified)
	}
	for _, c := range section.Checks {
		if c.Status == "ERROR" {
			res.Operational = true
		}
	}
	return res.Operational
}

func mutationLimits(cfg config.Config) model.MutationLimits {
	return model.MutationLimits{MaxMutants: cfg.Mutation.MaxMutants, TimeoutSeconds: cfg.Mutation.TimeoutSeconds, MaxRuntimeSeconds: cfg.Mutation.MaxRuntimeSeconds}
}

// mutationSection builds a mutation section with no mutants.
func mutationSection(cfg config.Config, status, reason string) *model.Mutation {
	m := &model.Mutation{Status: status, Reason: reason, Command: []string{}, Files: []model.MutationFile{}, Mutants: []model.Mutant{}, Checks: []model.Check{}, Note: model.MutationNote}
	if cfg.Mutation != nil {
		m.Command = append(m.Command, cfg.Mutation.Command...)
		m.Note = model.MutationNoteFor(m.Command)
		m.Limits = mutationLimits(cfg)
	}
	return m
}

// recordMutationSkipped records why a configured mutation stage did not run
// (§1.7). It is a no-op in lint, without a mutation policy, or when the section
// is already set. No changed files gives no_candidates; --checks=false and any
// other missed execution give not_run with their reason and, like a not_run
// section the stage records itself, one Unverified sentence.
func recordMutationSkipped(cfg config.Config, sc stageContext, r *model.Report) {
	if sc.mode != "review" || cfg.Mutation == nil || r.Mutation != nil {
		return
	}
	switch {
	case sc.reason == reasonNoChangedFiles:
		r.Mutation = mutationSection(cfg, model.MutationNoCandidates, "no changed files")
		return
	case !sc.checks:
		r.Mutation = mutationSection(cfg, model.MutationNotRun, "initial checks disabled (--checks=false)")
	default:
		r.Mutation = mutationSection(cfg, model.MutationNotRun, skippedReason(sc))
	}
	r.Unverified = append(r.Unverified, "Mutation analysis did not run: "+r.Mutation.Reason)
}

// mutationLine is the stdout line of the mutation section: counts only, never
// a percentage or a score, and killed mutants are only counted.
func mutationLine(m *model.Mutation) string {
	if m == nil {
		return ""
	}
	switch m.Status {
	case model.MutationNotRun:
		return "Mutation of added lines did not run: " + orNoReason(m.Reason) + "."
	case model.MutationNoCandidates:
		return "Mutation of added lines: no mutant was run (" + orNoReason(m.Reason) + ")."
	}
	status := "ran"
	if m.Status != model.MutationRan {
		status = m.Status
	}
	return fmt.Sprintf("Mutation of added lines (%s): %d mutants selected of %d generated; %d killed, %d survived, %d %s, %d timed out, %d inconclusive, %d not run.",
		status, len(m.Mutants), m.Generated, m.Killed, m.Survived, m.Invalid, mutation.TermsFor(m.Command).Invalid, m.TimedOut, m.Inconclusive, m.NotRun)
}

// orNoReason is a reason for a stdout sentence its caller ends itself: an
// empty reason gets a fixed text, and a final period is dropped (harness
// texts such as budgetReservedText end with one).
func orNoReason(s string) string {
	if s = strings.TrimSuffix(strings.TrimSpace(s), "."); s == "" {
		return "no reason was recorded"
	}
	return s
}
