package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// impactedTestFindings returns one finding per impacted test (by test file,
// package and name) whose Finalize status is FAILS_ON_CANDIDATE and whose
// impacted_test_differential record, naming that test, re-derives to
// FAILS_ON_CANDIDATE now. The test is an existing, unchanged declaration, so
// the finding has no location in the changed files: its declaration is a
// detail, and the changed functions the approximate static index links it to
// are listed.
func impactedTestFindings(r *model.Report, x *exportIndex) []finding {
	if r.Impact == nil {
		return nil
	}
	type entry struct {
		f       finding
		reaches []string
	}
	var order []string
	byTest := map[string]*entry{}
	for _, fn := range r.Impact.ChangedFunctions {
		for _, t := range fn.Tests {
			if t.Status != model.StatusFailsOnCandidate {
				continue
			}
			e, ok := x.accepted(t.EvidenceID, model.EvidenceImpactedTestDifferential, model.StatusFailsOnCandidate)
			if !ok || !contains(e.TestNames, t.Name) {
				continue
			}
			key := identity(t.Path, t.Package, t.Name)
			current := byTest[key]
			if current == nil {
				current = &entry{f: finding{
					Class: ClassImpactedTestFailsOnCandidate, Status: model.StatusFailsOnCandidate,
					Identity: key,
					Details: []detail{
						{"test", t.Name},
						{"test declaration", lineRange(t.Path, t.Line, 0)},
						{"package", t.Package},
					},
				}}
				byTest[key] = current
				order = append(order, key)
			}
			current.f.EvidenceIDs = append(current.f.EvidenceIDs, e.ID)
			current.f.CheckIDs = append(current.f.CheckIDs, e.BaseCheckID, e.CheckID)
			current.reaches = append(current.reaches, fmt.Sprintf("%s (%s, depth %d, %s link)", fn.Symbol, lineRange(fn.Path, fn.Line, 0), t.Depth, t.Resolution))
		}
	}
	out := make([]finding, 0, len(order))
	for _, key := range order {
		current := byTest[key]
		reaches := unique(current.reaches)
		sort.Strings(reaches)
		current.f.Details = append(current.f.Details, detail{"linked changed functions (approximate)", strings.Join(reaches, "; ")})
		for _, id := range unique(current.f.EvidenceIDs) {
			if e, ok := x.item(id); ok {
				current.f.Details = append(current.f.Details, detail{"runs", "baseline check " + e.BaseCheckID + " PASS; candidate check " + e.CheckID + " FAIL"})
			}
		}
		out = append(out, current.f)
	}
	return out
}
