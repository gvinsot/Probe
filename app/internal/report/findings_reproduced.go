package report

import (
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// reproducedFindings returns one finding per reproduced hypothesis (Finalize's
// r.ReproducedIssues) that cites at least one differential_test record which
// re-derives to REPRODUCED now. Only those records and their baseline and
// candidate checks are cited. The title and the severity are the reviewer
// model's, and the location is model-chosen.
func reproducedFindings(r *model.Report, x *exportIndex) []finding {
	var out []finding
	for _, h := range r.ReproducedIssues {
		if h.Status != model.StatusReproduced {
			continue
		}
		f := finding{
			Class: ClassReproduced, Status: model.StatusReproduced,
			Severity: h.Severity, Title: h.Title, HypothesisIDs: []string{h.ID},
		}
		if h.Path != "" {
			f.Location = &findingLocation{Path: h.Path, Line: h.Line, EndLine: h.Line, Source: locModel}
		}
		var identities []string
		for _, id := range unique(h.EvidenceIDs) {
			e, ok := x.accepted(id, model.EvidenceDifferentialTest, model.StatusReproduced)
			if !ok {
				continue
			}
			f.EvidenceIDs = append(f.EvidenceIDs, e.ID)
			f.CheckIDs = append(f.CheckIDs, e.BaseCheckID, e.CheckID)
			names := append([]string{}, e.TestNames...)
			sort.Strings(names)
			identities = append(identities, identity(e.Runner, e.Path, strings.Join(names, "\x00")))
			f.Details = append(f.Details,
				detail{"generated test", testLabel(e.Path, e.TestNames)},
				detail{"runs", "baseline check " + e.BaseCheckID + " PASS; candidate check " + e.CheckID + " FAIL (" + e.Runner + ")"},
			)
			f.Artifacts = append(f.Artifacts, x.retainedTestArtifacts(model.ArtifactGeneratedTest, e.Path)...)
		}
		if len(f.EvidenceIDs) == 0 {
			continue
		}
		sort.Strings(identities)
		f.Identity = strings.Join(identities, "\x00\x00")
		out = append(out, f)
	}
	return out
}

// testLabel renders a test file with its test names.
func testLabel(path string, names []string) string {
	joined := strings.Join(names, ", ")
	switch {
	case path == "":
		return joined
	case joined == "":
		return path
	}
	return path + " (" + joined + ")"
}
