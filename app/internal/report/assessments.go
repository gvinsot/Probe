package report

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// finalizeAssessments normalizes the reviewer's reading of linter signals. An
// assessment of an unknown or already assessed signal is dropped, an unknown
// judgment becomes uncertain, and no_risk is kept only when a non-blank
// rationale cites a source observation the ledger verified as OBSERVED. The
// list follows the order of the signals. Assessments are model judgment and
// change no hypothesis status; only adjustSeverities, when the run allowed it,
// lets a kept no_risk lower its signal's severity.
func finalizeAssessments(r *model.Report, l *ledger) {
	order := make(map[string]int, len(r.Signals))
	for i, s := range r.Signals {
		if _, dup := order[s.ID]; !dup {
			order[s.ID] = i
		}
	}
	seen := map[string]bool{}
	kept := make([]model.SignalAssessment, 0, len(r.SignalAssessments))
	for _, a := range r.SignalAssessments {
		if _, known := order[a.SignalID]; !known || seen[a.SignalID] || strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Explanation) == "" {
			continue
		}
		seen[a.SignalID] = true
		if a.EvidenceIDs == nil {
			a.EvidenceIDs = []string{}
		}
		a.Judgment = strings.ToLower(a.Judgment)
		switch a.Judgment {
		case model.AssessmentRisk, model.AssessmentUncertain:
		case model.AssessmentNoRisk:
			if strings.TrimSpace(a.Rationale) == "" || !citesObservation(a.EvidenceIDs, l) {
				a.Judgment = model.AssessmentUncertain
			}
		default:
			a.Judgment = model.AssessmentUncertain
		}
		kept = append(kept, a)
	}
	sort.SliceStable(kept, func(i, j int) bool { return order[kept[i].SignalID] < order[kept[j].SignalID] })
	r.SignalAssessments = kept
	adjustSeverities(r)
}

// adjustSeverities applies Report.AIImpactsCriticality. Each assessment is first
// cleared, so that a re-rendered report is adjusted once. Then, when the run
// allowed it, a kept no_risk reading lowers its signal's severity by one level
// (critical to high, high to medium, medium to low) in AdjustedSeverity, and
// sets a low signal aside. The signal keeps the linter's severity; review
// targets, the review surface and the exit code use effectiveSeverity.
func adjustSeverities(r *model.Report) {
	linter := make(map[string]string, len(r.Signals))
	for _, s := range r.Signals {
		if _, dup := linter[s.ID]; !dup {
			linter[s.ID] = s.Severity
		}
	}
	for i := range r.SignalAssessments {
		a := &r.SignalAssessments[i]
		a.AdjustedSeverity, a.SetAside = "", false
		if !r.AIImpactsCriticality || a.Judgment != model.AssessmentNoRisk {
			continue
		}
		switch severity(linter[a.SignalID]) {
		case "critical":
			a.AdjustedSeverity = "high"
		case "high":
			a.AdjustedSeverity = "medium"
		case "medium":
			a.AdjustedSeverity = "low"
		default:
			a.SetAside = true
		}
	}
}

// signalAdjustment is how a no_risk reading changed one signal.
type signalAdjustment struct {
	severity string // "" when unchanged
	setAside bool
}

// signalAdjustments indexes the adjustments of a finalized report by signal ID.
func signalAdjustments(r *model.Report) map[string]signalAdjustment {
	out := map[string]signalAdjustment{}
	for _, a := range r.SignalAssessments {
		if a.AdjustedSeverity != "" || a.SetAside {
			out[a.SignalID] = signalAdjustment{a.AdjustedSeverity, a.SetAside}
		}
	}
	return out
}

// effectiveSeverity is the severity conclusions use for s, and whether s was
// set aside.
func effectiveSeverity(s model.Signal, adjustments map[string]signalAdjustment) (string, bool) {
	a := adjustments[s.ID]
	if a.severity != "" {
		return a.severity, a.setAside
	}
	return s.Severity, a.setAside
}

// citesObservation reports whether one cited ID is a source observation that
// the ledger verified.
func citesObservation(ids []string, l *ledger) bool {
	for _, id := range ids {
		e, ok := l.item(id)
		if ok && e.Kind == model.EvidenceSourceObservation && l.verified[id] == model.StatusObserved {
			return true
		}
	}
	return false
}

// assessmentLabel is the fixed wording of a judgment.
func assessmentLabel(judgment string) string {
	switch judgment {
	case model.AssessmentRisk:
		return "risk"
	case model.AssessmentNoRisk:
		return "no risk"
	default:
		return "uncertain"
	}
}

// writeReviewerReading renders the reviewer's closing text and its reading of
// the linter signals. It writes nothing for a report that has neither, so that
// reports without a reviewer keep their exact rendering.
func writeReviewerReading(b *bytes.Buffer, r *model.Report) {
	if strings.TrimSpace(r.ReviewerSummary) != "" {
		line(b, "\n## Reviewer Summary\n")
		line(b, "Model output, not evidence.\n")
		for _, l := range strings.Split(strings.TrimSpace(r.ReviewerSummary), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				line(b, "> "+inline(l))
			}
		}
	}
	if len(r.SignalAssessments) == 0 {
		return
	}
	signals := make(map[string]model.Signal, len(r.Signals))
	for _, s := range r.Signals {
		if _, dup := signals[s.ID]; !dup {
			signals[s.ID] = s
		}
	}
	line(b, "\n## Linter Signals Read by the Reviewer\n")
	if r.AIImpactsCriticality {
		line(b, "Model judgment, not evidence. A no-risk reading cites a recorded source observation; it lowered its signal's severity by one level, and set a low signal aside, before review targets and the exit code were derived (--ai-impacts-criticality).\n")
	} else {
		line(b, "Model judgment, not evidence: it changes no signal, status, review target or exit code (--ai-impacts-criticality=false). A no-risk reading cites a recorded source observation.\n")
	}
	for _, a := range r.SignalAssessments {
		s := signals[a.SignalID]
		effect := ""
		switch {
		case a.SetAside:
			effect = "; set aside"
		case a.AdjustedSeverity != "":
			effect = fmt.Sprintf("; severity %s, lowered from %s", inline(a.AdjustedSeverity), inline(s.Severity))
		}
		fmt.Fprintf(b, "- **%s** %s (%s; linter: %s%s): %s\n", assessmentLabel(a.Judgment), inline(a.Title), inline(a.SignalID), inline(s.Summary), effect, inline(a.Explanation))
		if strings.TrimSpace(a.Rationale) != "" {
			line(b, "  Rationale: "+inline(a.Rationale))
		}
	}
}
