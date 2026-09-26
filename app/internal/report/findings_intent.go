package report

import (
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// intentStatement is the fixed text of an intent_test_failed finding; %s is the
// criterion ID. It is followed by the retained intent test's sha256.
const intentStatement = "A model-written test for %s failed on the candidate; there is no baseline control, and the test or its reading of the criterion may be wrong."

// intentFindings returns one finding per hypothesis of r.IntentTestFailures
// that cites at least one intent_test record for the same known criterion
// which re-derives to INTENT_TEST_FAILED now. The finding carries the fixed
// statement, the criterion's verbatim text, and the retained intent test's
// sha256 when exactly one retained intent_test artifact matches the test file.
// It never reads intent_judgment.
func intentFindings(r *model.Report, x *exportIndex) []finding {
	var out []finding
	for _, h := range r.IntentTestFailures {
		if h.Status != model.StatusIntentTestFailed || !model.ValidCriterionID(h.CriterionID) {
			continue
		}
		criterion, known := x.criteria[h.CriterionID]
		if !known {
			continue
		}
		f := finding{
			Class: ClassIntentTestFailed, Status: model.StatusIntentTestFailed,
			Title: h.Title, HypothesisIDs: []string{h.ID}, CriterionID: h.CriterionID,
			Statement: strings.Replace(intentStatement, "%s", h.CriterionID, 1),
			Details:   []detail{{"criterion " + h.CriterionID, criterion.Text}},
		}
		if h.Path != "" {
			f.Location = &findingLocation{Path: h.Path, Line: h.Line, EndLine: h.Line, Source: locModel}
		}
		var identities []string
		for _, id := range unique(h.EvidenceIDs) {
			e, ok := x.accepted(id, model.EvidenceIntentTest, model.StatusIntentTestFailed)
			if !ok || e.CriterionID != h.CriterionID {
				continue
			}
			f.EvidenceIDs = append(f.EvidenceIDs, e.ID)
			f.CheckIDs = append(f.CheckIDs, e.CheckID)
			names := append([]string{}, e.TestNames...)
			sort.Strings(names)
			identities = append(identities, identity(e.Path, strings.Join(names, "\x00")))
			f.Details = append(f.Details,
				detail{"intent test", testLabel(e.Path, e.TestNames)},
				detail{"run", "candidate check " + e.CheckID + " FAIL (candidate only; no baseline control)"},
			)
			if len(e.ReferencedSymbols) > 0 {
				f.Details = append(f.Details, detail{"changed symbols the test references", strings.Join(e.ReferencedSymbols, ", ")})
			}
			f.Artifacts = append(f.Artifacts, x.retainedTestArtifacts(model.ArtifactIntentTest, e.Path)...)
		}
		if len(f.EvidenceIDs) == 0 {
			continue
		}
		sort.Strings(identities)
		f.Identity = identity(h.CriterionID, strings.Join(identities, "\x00\x00"))
		out = append(out, f)
	}
	return out
}
