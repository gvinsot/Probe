package report

import (
	"bytes"
	"fmt"

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
	if !d.BaseMatches {
		r.Unverified = append(r.Unverified, planBaseNote)
		human = true
	}
	return human
}

// writePlanDrift renders "## Plan Conformance".
func writePlanDrift(b *bytes.Buffer, r *model.Report) {
	d := r.PlanDrift
	line(b, "\n## Plan Conformance\n")
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
