package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/plan"
)

// planBaseNote is the Unverified note of a review whose base differs from the
// commit the plan was made against.
const planBaseNote = "The plan was made against another base commit than the one this review compares from; files and symbols it named may have moved since."

// finalizePlanDrift recomputes the plan_drift section and its plan_drift
// signals from the recorded contract, critical globs, change and
// public_api_change signals: the stored items and status are never trusted.
// It reports whether the section requests human review (a drifted change, or
// a plan made against another base).
func finalizePlanDrift(r *model.Report) bool {
	kept := r.Signals[:0:0]
	for _, s := range r.Signals {
		if s.Kind != model.SignalPlanDrift {
			kept = append(kept, s)
		}
	}
	r.Signals = kept
	d := r.PlanDrift
	if d == nil {
		return false
	}
	if d.Contract.Files == nil {
		d.Contract.Files = []string{}
	}
	if d.Contract.Symbols == nil {
		d.Contract.Symbols = []model.PlanContractSymbol{}
	}
	if d.Contract.CriticalFiles == nil {
		d.Contract.CriticalFiles = []string{}
	}
	if d.Contract.Manifests == nil {
		d.Contract.Manifests = []string{}
	}
	if d.Contract.NewPackages == nil {
		d.Contract.NewPackages = []string{}
	}
	if d.CriticalGlobs == nil {
		d.CriticalGlobs = []string{}
	}
	d.Items = plan.Drift(d.Contract, d.CriticalGlobs, r.Change, r.Signals)
	d.Status = plan.DriftStatus(d.Items)
	d.BaseMatches = d.Contract.BaseCommit != "" && d.Contract.BaseCommit == r.Change.BaseCommit
	d.Note = model.PlanDriftNote
	if extra := plan.DriftSignals(d.Items, r.Change); len(extra) > 0 {
		r.Signals = linter.Merge(r.Signals, extra)
	}
	human := d.Status == model.PlanDriftDrifted
	if d.Assessment.Status != model.PlanAssessed || d.Assessment.Major {
		// A plan that raised a category, or that could not be re-assessed,
		// needs a human whatever the implementation does.
		human = true
	}
	if d.Assessment.Status != model.PlanAssessed {
		d.Assessment.Status, d.Assessment.Major = model.PlanUnassessed, false
		if d.Assessment.Reason == "" {
			d.Assessment.Reason = "the plan was not re-assessed by this review"
		}
	}
	if d.Assessment.FlaggedCategories == nil {
		d.Assessment.FlaggedCategories = []string{}
	}
	if d.Assessment.Gaps == nil {
		d.Assessment.Gaps = []string{}
	}
	if len(d.Assessment.Gaps) > 0 {
		human = true
	}
	d.Assessment.Major = len(d.Assessment.FlaggedCategories) > 0
	if !d.BaseMatches {
		r.Unverified = append(r.Unverified, planBaseNote)
		human = true
	}
	return human
}

// decidePlanGate sets the plan gate of a report with a plan_drift section, at
// the end of Finalize, and reports whether the gate adds a request for human
// review. The decision is no_human_review_required only when every condition
// of the plan-driven process holds; otherwise every reason is listed.
// needsHuman is what the rest of the report already concluded.
func decidePlanGate(r *model.Report, needsHuman bool) bool {
	d := r.PlanDrift
	if d == nil {
		return false
	}
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	switch {
	case d.Assessment.Status != model.PlanAssessed:
		add("the plan could not be re-assessed by this review (%s)", d.Assessment.Reason)
	case d.Assessment.Major:
		add("the plan raised risk categories: %s", strings.Join(d.Assessment.FlaggedCategories, ", "))
	}
	for _, gap := range d.Assessment.Gaps {
		add("the plan's risk is not fully measured: %s", gap)
	}
	if d.Status == model.PlanDriftDrifted {
		n := 0
		for _, it := range d.Items {
			if rank(it.Severity) >= rank("medium") {
				n++
			}
		}
		add("the change drifted from the plan (%d medium or higher differences)", n)
	}
	if !d.BaseMatches {
		add("the plan was made against another base commit than the one this review compares from")
	}
	if r.ExitCode == 1 {
		add("a high or critical issue was reproduced")
	}
	if len(r.Change.Files) > 0 && len(r.Checks) == 0 {
		add("no check ran (lint, --checks=false, or no command configured)")
	}
	failed := 0
	for _, c := range r.Checks {
		if c.Status != "PASS" || c.ExitCode != 0 {
			failed++
		}
	}
	if failed > 0 {
		add("%d checks did not pass", failed)
	}
	high := 0
	for _, s := range r.Signals {
		if rank(s.Severity) >= rank("high") && s.Kind != model.SignalPlanDrift {
			high++
		}
	}
	if high > 0 {
		add("%d high or critical risk signals", high)
	}
	if len(r.Unverified) > 0 {
		add("%d unverified areas", len(r.Unverified))
	}
	if needsHuman && len(reasons) == 0 {
		add("other sections of the report request human review")
	}
	d.DecisionReasons = reasons
	if d.DecisionReasons == nil {
		d.DecisionReasons = []string{}
	}
	if len(reasons) > 0 {
		d.Decision = model.PlanDecisionReviewRequired
		return true
	}
	d.Decision = model.PlanDecisionNoReview
	return false
}

// writePlanDrift renders "## Plan Conformance".
func writePlanDrift(b *bytes.Buffer, r *model.Report) {
	d := r.PlanDrift
	line(b, "\n## Plan Conformance\n")
	if d.Decision == model.PlanDecisionNoReview {
		line(b, "Plan gate: **no human review required**. The plan raised no risk category, the change conforms to it, the checks passed, and nothing else in this report requests review.\n")
	} else {
		line(b, "Plan gate: **human review required**.\n")
		for _, reason := range d.DecisionReasons {
			fmt.Fprintf(b, "- %s\n", inline(reason))
		}
		line(b, "")
	}
	switch {
	case d.Assessment.Status != model.PlanAssessed:
		fmt.Fprintf(b, "Plan assessment: unavailable (%s).\n\n", inline(orNone(d.Assessment.Reason)))
	case d.Assessment.Major:
		fmt.Fprintf(b, "Plan assessment (re-computed by this review): flagged categories %s.\n\n", inline(strings.Join(d.Assessment.FlaggedCategories, ", ")))
	default:
		line(b, "Plan assessment (re-computed by this review): no category flagged.\n")
	}
	fmt.Fprintf(b, "Status: **%s** against plan %s (%d planned files, %d planned symbols).\n\n", inline(d.Status), inline(shortHash(d.PlanSHA256)), len(d.Contract.Files), len(d.Contract.Symbols))
	if !d.BaseMatches {
		fmt.Fprintf(b, "The plan was made against base %s; this review compares from %s.\n\n", inline(d.Contract.BaseCommit), inline(r.Change.BaseCommit))
	}
	if len(d.Items) == 0 {
		line(b, "The change stays within the plan: every changed file was planned, and no unannounced exported change, critical path or dependency manifest was found.\n")
	}
	for _, it := range d.Items {
		where := it.Path
		if it.Symbol != "" {
			where += " (" + it.Symbol + ")"
		}
		fmt.Fprintf(b, "- **%s** %s — %s [%s]\n", inline(it.Severity), inline(it.Summary), inline(where), inline(it.Kind))
	}
	line(b, "\n"+inline(d.Note)+"\n")
	line(b, inline(model.PlanGateNote)+"\n")
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(unknown)"
	}
	return h
}
