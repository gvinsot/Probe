package report

import (
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// baseTestFindings returns one finding per base_tests item whose Finalize
// status is FAILS_ON_CANDIDATE and whose base_test_differential record, naming
// that test, re-derives to FAILS_ON_CANDIDATE now. The location is the test's
// candidate range when there is one; the baseline declaration, which may no
// longer exist on the candidate side, is a detail only.
func baseTestFindings(r *model.Report, x *exportIndex) []finding {
	if r.BaseTests == nil {
		return nil
	}
	var out []finding
	for _, t := range r.BaseTests.Tests {
		if t.Status != model.StatusFailsOnCandidate {
			continue
		}
		e, ok := x.accepted(t.EvidenceID, model.EvidenceBaseTestDifferential, model.StatusFailsOnCandidate)
		if !ok || !contains(e.TestNames, t.Name) {
			continue
		}
		f := finding{
			Class: ClassBaseTestFailsOnCandidate, Status: model.StatusFailsOnCandidate,
			Identity:    identity(t.Path, t.Name),
			EvidenceIDs: []string{e.ID},
			CheckIDs:    []string{e.BaseCheckID, e.CheckID},
			Details: []detail{
				{"test", t.Name},
				{"baseline declaration", lineRange(t.Path, t.Line, t.EndLine)},
				{"change to the test", t.Change},
				{"runs", "baseline check " + e.BaseCheckID + " PASS; hybrid-tree check " + e.CheckID + " FAIL"},
			},
		}
		if t.CandidatePath != "" {
			f.Location = &findingLocation{Path: t.CandidatePath, Line: t.CandidateLine, EndLine: t.CandidateEndLine, Source: locCandidateTest}
		}
		out = append(out, f)
	}
	return out
}

func contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
