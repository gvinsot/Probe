package report

import (
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// divergenceFindings returns one finding per entry of r.Divergences, which only
// Finalize fills with validated entries, whose evidence record has the entry's
// kind and re-derives to DIVERGED now. A differential_fuzz entry becomes a
// fuzz_divergence anchored at its changed function; a differential_observation
// entry becomes an observed_divergence, whose only possible anchor is the
// model-chosen location of a citing hypothesis. Both show recorded values and
// never say which revision is right.
func divergenceFindings(r *model.Report, x *exportIndex) []finding {
	var out []finding
	for _, d := range r.Divergences {
		e, ok := x.accepted(d.EvidenceID, d.Kind, model.StatusDiverged)
		if !ok || len(d.CheckIDs) < minDivergenceChecks(d.Kind) {
			continue
		}
		var rows []model.Observation
		for _, o := range d.Observations {
			if o.Status == model.ObservationDiverged && o.Key != "" {
				rows = append(rows, o)
			}
		}
		if len(rows) == 0 {
			continue
		}
		f := finding{
			Status:        model.StatusDiverged,
			EvidenceIDs:   []string{e.ID},
			CheckIDs:      append([]string{}, d.CheckIDs...),
			HypothesisIDs: append([]string{}, d.HypothesisIDs...),
		}
		switch d.Kind {
		case model.EvidenceDifferentialFuzz:
			f.Class = ClassFuzzDivergence
			if d.AnchorSource == anchorChangedFunction && d.Path != "" {
				f.Location = &findingLocation{Path: d.Path, Line: d.Line, EndLine: d.Line, Source: locChangedFunction}
			}
			key := d.Path + "\x00" + d.Symbol
			if d.Path == "" && d.Symbol == "" {
				key = d.TestPath + "\x00" + rows[0].Key
			}
			f.Identity = identity("fuzz", key)
			if d.Symbol != "" {
				f.Details = append(f.Details, detail{"function", d.Symbol})
			}
		case model.EvidenceDifferentialObservation:
			f.Class = ClassObservedDivergence
			if d.AnchorSource == anchorHypothesis && d.Path != "" {
				f.Location = &findingLocation{Path: d.Path, Line: d.Line, EndLine: d.Line, Source: locModel}
			}
			names := append([]string{}, d.TestNames...)
			sort.Strings(names)
			keys := make([]string, 0, len(rows))
			for _, o := range rows {
				keys = append(keys, o.Test+"\x00"+o.Key)
			}
			sort.Strings(keys)
			f.Identity = identity("observation", d.TestPath, strings.Join(names, "\x00"), strings.Join(keys, "\x00"))
			f.Details = append(f.Details, detail{"generated test", testLabel(d.TestPath, d.TestNames)})
			f.Artifacts = x.retainedTestArtifacts(model.ArtifactGeneratedTest, d.TestPath)
		default:
			continue
		}
		label := "recorded key"
		if d.Kind == model.EvidenceDifferentialFuzz {
			label = "input"
		}
		for i, o := range rows {
			if i == maxRowsInDetails {
				f.Details = append(f.Details, detail{"more rows", plural(len(rows)-maxRowsInDetails, "further diverging row is", "further diverging rows are") + " in confidence-report.json"})
				break
			}
			key := o.Key
			if o.Truncated {
				key += " (values cut for display)"
			}
			f.Details = append(f.Details,
				detail{label, key},
				detail{"baseline value", recordedValue(o.Base)},
				detail{"candidate value", recordedValue(o.Candidate)},
			)
		}
		out = append(out, f)
	}
	return out
}

// recordedValue renders one side of a DIVERGED row, whose two sides are both
// recorded.
func recordedValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}
