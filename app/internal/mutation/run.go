package mutation

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// mutantSlack is added to three times the control duration to bound one
// mutant run: a mutant that makes a loop run forever stops well before the
// policy timeout, while a mutant does the same compile and test work as its
// control. The policy timeout and the remaining sub-cap still apply.
const mutantSlack = 30 * time.Second

// executor runs one plan sequentially: per package, one control run, then its
// mutants. It tracks the mutation.max_runtime_seconds sub-cap from the
// recorded run durations; the harness additionally applies the shared budget,
// the reviewer reserve and the policy timeout.
type executor struct {
	ctx     context.Context
	w       Workspace
	cfg     Config
	plan    Plan
	mutants []model.Mutant
	signals []model.Signal

	timeout, maxRuntime, spent, lastControl time.Duration

	executed    bool   // the sandbox ran at least one control or mutant command (not SKIPPED, not ERROR)
	operational bool   // an infrastructure error or a workspace failure
	stopped     string // the first reason that stopped the whole stage
}

func (e *executor) execute() {
	e.timeout = time.Duration(e.cfg.Limits.TimeoutSeconds) * time.Second
	e.maxRuntime = time.Duration(e.cfg.Limits.MaxRuntimeSeconds) * time.Second
	for from := 0; from < len(e.plan.Selected); {
		pkg := PackageArg(e.plan.Selected[from].Path)
		to := from
		for to < len(e.plan.Selected) && PackageArg(e.plan.Selected[to].Path) == pkg {
			to++
		}
		if !e.runPackage(pkg, from, to) {
			return
		}
		from = to
	}
}

// stop records reason on every mutant from index k on that has not run, and
// ends the stage.
func (e *executor) stop(k int, reason string) bool {
	if e.stopped == "" {
		e.stopped = reason
	}
	for ; k < len(e.mutants); k++ {
		if e.mutants[k].Status == model.MutantNotRun && e.mutants[k].Reason == "" {
			e.mutants[k].Reason = reason
		}
	}
	return false
}

// firstReason is the reason of a stage that executed nothing.
func (e *executor) firstReason() string {
	if e.stopped != "" {
		return e.stopped
	}
	for _, m := range e.mutants {
		if m.Reason != "" {
			return m.Reason
		}
	}
	return "no control or mutant run was executed"
}

// runPackage runs the control of pkg and then its mutants [from, to). It
// returns false when the whole stage must stop.
func (e *executor) runPackage(pkg string, from, to int) bool {
	if e.ctx.Err() != nil {
		return e.stop(from, reasonCancelled)
	}
	remaining := e.maxRuntime - e.spent
	if remaining <= 0 || (e.lastControl > 0 && remaining < e.lastControl) {
		return e.stop(from, reasonBudget)
	}
	command := ExpandCommand(e.cfg.Command, pkg)
	control, err := e.w.RunControl(e.ctx, pkg, command, minDuration(e.timeout, remaining))
	e.spent += duration(control)
	if err != nil {
		e.operational = true
		return e.stop(from, reasonAborted)
	}
	if control.Status == "SKIPPED" {
		return e.stop(from, fmt.Sprintf("the control run %s was not executed: %s", control.ID, cut(control.Output)))
	}
	e.record(control)
	e.lastControl = duration(control)
	// The control log is read once; every mutant of the package is classified
	// against it.
	ctl := NewControlFor(ScriptCommand(e.cfg.Command), control)
	if why := ctl.Reason(); why != "" {
		for k := from; k < to; k++ {
			e.mutants[k].ControlCheckID, e.mutants[k].Reason = control.ID, why
		}
		return true
	}
	for k := from; k < to; k++ {
		if !e.runMutant(k, command, control, ctl) {
			return false
		}
	}
	return true
}

// runMutant applies, runs and classifies mutant k. It returns false when the
// whole stage must stop.
func (e *executor) runMutant(k int, command []string, control model.Check, ctl Control) bool {
	m, site := &e.mutants[k], e.plan.Selected[k]
	m.ControlCheckID = control.ID
	if e.ctx.Err() != nil {
		return e.stop(k, reasonCancelled)
	}
	remaining := e.maxRuntime - e.spent
	controlTime := duration(control)
	if remaining <= 0 || remaining < controlTime {
		return e.stop(k, reasonBudget)
	}
	src := e.plan.Sources[site.Path]
	mutated, err := site.Apply(src)
	if err != nil {
		m.Status, m.Reason = model.MutantInconclusive, reasonApplyFailed+err.Error()
		return true
	}
	// own is the mutant's own time limit; the sub-cap, the shared budget or the
	// reviewer reserve may leave the run less.
	own := minDuration(e.timeout, 3*controlTime+mutantSlack)
	timeout := minDuration(own, remaining)
	c, limit, err := e.w.RunMutant(e.ctx, m.ID, site.Path, src, mutated, command, timeout)
	if limit <= 0 || limit > timeout {
		limit = timeout
	}
	e.spent += duration(c)
	if c.ID != "" {
		m.CheckID = c.ID
	}
	if err != nil {
		e.operational = true
		if c.ID == "" {
			// Nothing ran: the workspace refused before writing or running.
			m.Reason = reasonWorkspaceError
			return e.stop(k+1, reasonAborted)
		}
		m.Status, m.Reason = model.MutantInconclusive, reasonAborted
		return e.stop(k+1, reasonAborted)
	}
	if c.Status == "SKIPPED" {
		m.Reason = fmt.Sprintf("the mutant run %s was not executed: %s", c.ID, cut(c.Output))
		return e.stop(k+1, m.Reason)
	}
	e.record(c)
	if c.Status == "TIMEOUT" && e.ctx.Err() != nil {
		// The review's time limit, not the mutant, ended this run: a TIMEOUT
		// outcome would describe the mutant.
		m.Status, m.Reason = model.MutantInconclusive, reasonStoppedRun
		return e.stop(k+1, reasonCancelled)
	}
	if c.Status == "TIMEOUT" && limit < own {
		// A budget left this run less than its own time limit and the run used
		// all of it: the budget, not the mutant, ended the run.
		m.Status, m.Reason = model.MutantInconclusive, reasonBudgetCut
		return e.stop(k+1, reasonBudgetSpent)
	}
	v := ctl.Classify(c)
	m.Status, m.Reason = v.Status, v.Reason
	switch v.Status {
	case model.MutantKilled:
		m.FailedTests = append([]string(nil), v.FailedTests...)
	case model.MutantSurvived:
		m.TestsRun = v.TestsRun
		sha, err := e.w.SavePatch(m.ID+".patch", patchText(*m, site, src, mutated, c.Command))
		if err != nil || sha == "" {
			m.Status, m.Reason, m.TestsRun = model.MutantInconclusive, reasonPatchFailed, 0
			return true
		}
		m.PatchSHA256 = sha
		e.signals = append(e.signals, survivorSignal(*m, control.ID, c))
	}
	return true
}

// record notes what one recorded, not skipped, run says about the stage: an
// ERROR is an infrastructure failure (for these kinds the harness never turns
// candidate output into ERROR) and makes the stage an operational failure; any
// other status means the sandbox ran the command.
func (e *executor) record(c model.Check) {
	if c.Status == "ERROR" {
		e.operational = true
		return
	}
	e.executed = true
}

// patchText is the retained record of one mutant: a header naming the mutant
// and the executed command, then a one-hunk unified diff of the lines it
// changes. It records what ran; it is not a proposed change.
func patchText(m model.Mutant, s Site, src, mutated []byte, command []string) []byte {
	var b bytes.Buffer
	symbol := ""
	if m.Symbol != "" {
		symbol = " in " + m.Symbol
	}
	fmt.Fprintf(&b, "Probe mutant %s (%s) of %s:%d%s\n", m.ID, m.Operator, m.Path, m.Line, symbol)
	fmt.Fprintf(&b, "Command: %s\n", strings.Join(command, " "))
	fmt.Fprintf(&b, "This patch records one single-change mutant that was run; it is not a proposed change.\n")
	original, changed := lines(src, s.Line, s.EndLine), lines(mutated, s.Line, s.EndLine)
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", s.Path, s.Path, s.Line, len(original), s.Line, len(changed))
	for _, l := range original {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range changed {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.Bytes()
}

// lines returns the 1-based lines first..last of src without their newline.
func lines(src []byte, first, last int) []string {
	all := strings.Split(string(src), "\n")
	if first < 1 || last < first || last > len(all) {
		return nil
	}
	return all[first-1 : last]
}

// survivorSignal is the medium review signal of one SURVIVED mutant.
func survivorSignal(m model.Mutant, controlID string, c model.Check) model.Signal {
	summary, text := survivorSummary, survivorText
	if scriptPath(m.Path) {
		summary, text = survivorScriptSummary, survivorScriptText
	}
	s := model.Signal{
		Kind: model.SignalSurvivingMutant, Path: m.Path, Line: m.Line, Side: "new", Symbol: m.Symbol, Severity: "medium",
		Summary:  summary,
		Evidence: redact.Redact(fmt.Sprintf(text, m.ID, m.Operator, m.Original, m.Mutated, controlID, c.ID, strings.Join(c.Command, " "), m.TestsRun, m.Package, m.PatchSHA256)),
	}
	if m.EndLine > m.Line {
		s.EndLine = m.EndLine
	}
	return s
}

func duration(c model.Check) time.Duration {
	if c.DurationMS <= 0 {
		return 0
	}
	return time.Duration(c.DurationMS) * time.Millisecond
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// cut is a bounded one-line form of a recorded output.
func cut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "no reason was recorded"
	}
	return redact.TruncateUTF8(s, 200)
}
