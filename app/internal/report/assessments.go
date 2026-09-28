package report

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// finalizeAssessments normalizes the reviewer's reading of linter signals. An
// assessment of an unknown or already assessed signal is dropped, an unknown
// judgment becomes uncertain, and no_risk is kept only when a non-blank
// rationale cites a source observation the ledger verified as OBSERVED. The
// list follows the order of the signals. Assessments are model judgment:
// nothing here changes a signal, a status, a review target or the exit code.
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
	summaries := make(map[string]string, len(r.Signals))
	for _, s := range r.Signals {
		if _, dup := summaries[s.ID]; !dup {
			summaries[s.ID] = s.Summary
		}
	}
	line(b, "\n## Linter Signals Read by the Reviewer\n")
	line(b, "Model judgment, not evidence: it changes no signal, status, review target or exit code. A no-risk reading cites a recorded source observation.\n")
	for _, a := range r.SignalAssessments {
		fmt.Fprintf(b, "- **%s** %s (%s; linter: %s): %s\n", assessmentLabel(a.Judgment), inline(a.Title), inline(a.SignalID), inline(summaries[a.SignalID]), inline(a.Explanation))
		if strings.TrimSpace(a.Rationale) != "" {
			line(b, "  Rationale: "+inline(a.Rationale))
		}
	}
}
