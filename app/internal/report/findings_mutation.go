package report

import (
	"fmt"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// mutantFindings returns one finding per SURVIVED mutant that verifyMutation
// re-derives as SURVIVED now: Finalize has already turned every other
// SURVIVED mutant into INCONCLUSIVE, and the export does not trust the stored
// status alone either. A mutant cites its mutation-ledger checks (the control
// run and the mutant run) and no evidence record. Killed mutants are never
// listed.
func mutantFindings(r *model.Report, x *exportIndex) []finding {
	if r.Mutation == nil {
		return nil
	}
	var out []finding
	for _, m := range r.Mutation.Mutants {
		if m.Status != model.MutantSurvived || x.v.mutants[m.ID] != model.MutantSurvived {
			continue
		}
		f := finding{
			Class: ClassSurvivingMutant, Status: model.MutantSurvived,
			MutantID: m.ID,
			Identity: identity(m.Path, m.Package, m.Operator, m.Original, m.Mutated),
			CheckIDs: []string{m.ControlCheckID, m.CheckID},
			Location: &findingLocation{Path: m.Path, Line: m.Line, EndLine: m.Line, Source: locMutatedLine},
			Details: []detail{
				{"mutant", m.ID},
				{"operator", m.Operator},
				{"original", m.Original},
				{"mutated", m.Mutated},
				{"package", m.Package},
				{"runs", fmt.Sprintf("control check %s PASS; mutant check %s PASS with %d passing tests", m.ControlCheckID, m.CheckID, m.TestsRun)},
			},
			Artifacts: x.artifactBySHA(model.ArtifactMutantPatch, m.PatchSHA256),
		}
		if len(f.Artifacts) == 0 {
			f.Details = append(f.Details, detail{"patch sha256 (no patch artifact)", m.PatchSHA256})
		}
		out = append(out, f)
	}
	return out
}
