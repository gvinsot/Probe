package report

// The observation oracle's report side (F1). Every differential_observation
// record is re-derived here from its recorded checks with the same function the
// harness used (harness.EvaluateObservations), and a stored status counts only
// when the recomputation agrees with it.

import (
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/observe"
)

// verifyObservations re-derives the status of every differential_observation
// evidence record from its recorded checks. It returns an ID only when the
// re-derived status is DIVERGED or NOT_DIVERGED and equals the stored one.
//
// Requirements (all must hold, see observationOutcome): the base check has kind
// generated_test_base and the candidate check generated_test_candidate, both
// resolve once in the ledger and use the same command, the named executions
// pass on both, and a repeat check, when cited, has kind
// generated_test_base_repeat and the base command. Live-baseline rule: DIVERGED
// needs a cited repeat that was executed, never replayed; NOT_DIVERGED may rest
// on a replayed base only when two agreeing live runs recorded it.
func verifyObservations(r *model.Report, l *ledger) map[string]string {
	out := map[string]string{}
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceDifferentialObservation {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok {
			continue
		}
		o, ok := observationOutcome(e, l)
		if !ok || o.Status != e.Status {
			continue
		}
		if o.Status == model.StatusDiverged || o.Status == model.StatusNotDiverged {
			out[e.ID] = o.Status
		}
	}
	return out
}

// observationOutcome re-derives the outcome of one differential_observation
// record, or returns false when its recorded checks cannot support any status.
// It is pure: it never modifies the report or the ledger.
func observationOutcome(e model.Evidence, l *ledger) (observe.Outcome, bool) {
	if e.Kind != model.EvidenceDifferentialObservation || e.Path == "" || !verifiableText(e.Path) || !verifiableNames(e.TestNames) {
		return observe.Outcome{}, false
	}
	base, baseOK := l.check(e.BaseCheckID)
	candidate, candidateOK := l.check(e.CheckID)
	if !baseOK || !candidateOK || base.Kind != model.CheckGeneratedBase || candidate.Kind != model.CheckGeneratedCandidate || candidate.Replayed() {
		return observe.Outcome{}, false
	}
	var repeat *model.Check
	if e.RepeatCheckID != "" {
		c, ok := l.check(e.RepeatCheckID)
		if !ok || c.Kind != model.CheckGeneratedBaseRepeat || c.Replayed() {
			return observe.Outcome{}, false
		}
		repeat = &c
	}
	o, known := harness.EvaluateObservations(e.Runner, base, candidate, repeat, e.Path, e.TestNames)
	if !known {
		return observe.Outcome{}, false
	}
	switch o.Status {
	case model.StatusDiverged:
		if repeat == nil || !positiveBaseline(*repeat) {
			return observe.Outcome{}, false
		}
	case model.StatusNotDiverged:
		if !negativeBaseline(base) {
			return observe.Outcome{}, false
		}
	}
	return o, true
}

// observationDivergences returns one candidate divergence per verified
// DIVERGED differential_observation record: Kind, TestPath, TestNames,
// CheckIDs [base, candidate, repeat] and the DIVERGED rows sorted by (test,
// key), re-derived from the recorded checks. Path stays empty; Finalize
// anchors it through a citing hypothesis.
func observationDivergences(r *model.Report, l *ledger) []model.Divergence {
	var out []model.Divergence
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceDifferentialObservation || l.verified[recorded.ID] != model.StatusDiverged {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok {
			continue
		}
		o, ok := observationOutcome(e, l)
		if !ok || o.Status != model.StatusDiverged {
			continue
		}
		out = append(out, model.Divergence{
			EvidenceID:    e.ID,
			Kind:          model.EvidenceDifferentialObservation,
			TestPath:      e.Path,
			TestNames:     append([]string{}, e.TestNames...),
			CheckIDs:      []string{e.BaseCheckID, e.CheckID, e.RepeatCheckID},
			HypothesisIDs: []string{},
			Observations:  o.Diverged(),
			Note:          model.DivergenceNote,
		})
	}
	return out
}

// observationMasks returns every candidate check ID whose observation record,
// whatever its verified status, recorded a key on both the baseline and the
// candidate side with different full values. Finalize then withdraws a
// both-pass NOT_REPRODUCED generated test that shares that candidate check, so
// an observed difference is never hidden behind it.
//
// Two extensions make the rule fail closed: a key recorded on one side only
// also counts as a difference, and the values are also read from the checks of
// every differential_test record itself, so removing the observation record
// from a saved report does not lift the mask. Validity rules (duplicates,
// redaction, bounds, passing runs) are deliberately ignored here: they decide
// what may support a divergence, never what may hide one.
func observationMasks(r *model.Report, l *ledger) map[string]bool {
	masked := map[string]bool{}
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceDifferentialObservation && recorded.Kind != model.EvidenceDifferentialTest {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok || e.CheckID == "" {
			continue
		}
		base, baseOK := l.check(e.BaseCheckID)
		candidate, candidateOK := l.check(e.CheckID)
		if !baseOK || !candidateOK {
			continue
		}
		baseSet, known := harness.ObservationSet(e.Runner, base, e.Path, e.TestNames)
		if !known {
			continue
		}
		candidateSet, _ := harness.ObservationSet(e.Runner, candidate, e.Path, e.TestNames)
		if observe.Differ(baseSet, candidateSet) {
			masked[e.CheckID] = true
		}
	}
	return masked
}
