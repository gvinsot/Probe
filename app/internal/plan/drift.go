package plan

import (
	"fmt"
	"sort"

	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Summaries of the linter's public_api_change signals that change or remove
// an exported declaration. An added exported declaration is not drift.
const (
	exportedChanged = "Exported Go declaration changed"
	exportedRemoved = "Exported Go declaration removed"
)

// Drift compares a recorded change with a plan contract. It is a pure
// function of its inputs, so report.Finalize recomputes it from the report:
//
//   - a changed path the contract does not list is unplanned (a test file is
//     low, anything else medium);
//   - a planned path the change does not touch is untouched (low);
//   - a changed or removed exported Go declaration that the contract does not
//     announce as a signature change or removal is unannounced (high); it is
//     read from the linter's public_api_change signals;
//   - a changed path matching a critical glob that the contract did not list
//     as critical is an unannounced critical path (high);
//   - a changed dependency manifest the contract does not list is an
//     unannounced dependency change (medium).
//
// An invalid glob is skipped: the policy validation rejects it upstream.
func Drift(contract model.PlanContract, globs []string, change model.Change, signals []model.Signal) []model.PlanDriftItem {
	planned := set(contract.Files)
	critical := set(contract.CriticalFiles)
	manifests := set(contract.Manifests)
	announced := map[[2]string]bool{}
	for _, s := range contract.Symbols {
		if s.Change == model.PlanSymbolSignature || s.Change == model.PlanSymbolRemove {
			announced[[2]string{s.Path, s.Name}] = true
		}
	}
	items := []model.PlanDriftItem{}
	touched := map[string]bool{}
	oldPath := map[string]string{}
	for _, f := range change.Files {
		touched[f.Path] = true
		if f.OldPath != "" {
			touched[f.OldPath] = true
			oldPath[f.Path] = f.OldPath
		}
		paths := []string{f.Path}
		if f.OldPath != "" && f.OldPath != f.Path {
			paths = append(paths, f.OldPath)
		}
		inPlan := false
		for _, p := range paths {
			inPlan = inPlan || planned[p]
		}
		if !inPlan {
			if linter.IsTestPath(f.Path) {
				items = append(items, model.PlanDriftItem{Kind: model.DriftUnplannedTestFile, Severity: "low", Path: f.Path, Summary: "Test file changed outside the plan"})
			} else {
				items = append(items, model.PlanDriftItem{Kind: model.DriftUnplannedFile, Severity: "medium", Path: f.Path, Summary: "File changed outside the plan"})
			}
		}
		for _, p := range paths {
			if glob, ok, err := linter.MatchSensitive(globs, p); err == nil && ok && !critical[p] {
				items = append(items, model.PlanDriftItem{Kind: model.DriftUnannouncedCritical, Severity: "high", Path: p, Summary: "Critical path changed although the plan did not announce it (pattern " + glob + ")"})
				break
			}
		}
		for _, p := range paths {
			if IsManifest(p) && !manifests[p] {
				items = append(items, model.PlanDriftItem{Kind: model.DriftUnannouncedManifest, Severity: "medium", Path: p, Summary: "Dependency manifest changed although the plan did not announce it"})
				break
			}
		}
	}
	for _, s := range signals {
		if s.Kind != "public_api_change" || (s.Summary != exportedChanged && s.Summary != exportedRemoved) {
			continue
		}
		if announced[[2]string{s.Path, s.Symbol}] || oldPath[s.Path] != "" && announced[[2]string{oldPath[s.Path], s.Symbol}] {
			continue
		}
		what := "changed"
		if s.Summary == exportedRemoved {
			what = "removed"
		}
		items = append(items, model.PlanDriftItem{Kind: model.DriftUnannouncedExported, Severity: "high", Path: s.Path, Symbol: s.Symbol, Summary: fmt.Sprintf("Exported declaration %s although the plan did not announce it", what)})
	}
	for _, p := range contract.Files {
		if !touched[p] {
			items = append(items, model.PlanDriftItem{Kind: model.DriftPlannedUntouched, Severity: "low", Path: p, Summary: "Planned file not changed"})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Symbol < b.Symbol
	})
	return items
}

// DriftStatus is drifted when an item is medium or higher.
func DriftStatus(items []model.PlanDriftItem) string {
	for _, it := range items {
		if it.Severity == "medium" || it.Severity == "high" || it.Severity == "critical" {
			return model.PlanDriftDrifted
		}
	}
	return model.PlanDriftConforming
}

// DriftSignals turns the items that point into the diff into plan_drift
// signals, located on the first changed line of their file, so that any
// consumer rendering signals shows them. Untouched planned files are not in
// the diff and stay in the section only. IDs are assigned by the caller.
func DriftSignals(items []model.PlanDriftItem, change model.Change) []model.Signal {
	first := map[string][2]any{}
	for _, f := range change.Files {
		line, side := firstLine(f)
		first[f.Path] = [2]any{line, side}
		if f.OldPath != "" {
			if _, ok := first[f.OldPath]; !ok {
				first[f.OldPath] = [2]any{line, side}
			}
		}
	}
	var out []model.Signal
	for _, it := range items {
		loc, ok := first[it.Path]
		if !ok || it.Kind == model.DriftPlannedUntouched {
			continue
		}
		out = append(out, model.Signal{Kind: model.SignalPlanDrift, Path: it.Path, Line: loc[0].(int), Side: loc[1].(string), Symbol: it.Symbol, Severity: it.Severity, Summary: it.Summary, Evidence: "Plan contract check (" + it.Kind + "): deterministic comparison of the diff with the plan"})
	}
	return out
}

func firstLine(f model.ChangedFile) (int, string) {
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			if d.Kind == "add" && d.NewLine > 0 {
				return d.NewLine, "new"
			}
			if d.Kind == "delete" && d.OldLine > 0 {
				return d.OldLine, "old"
			}
		}
	}
	if f.Status == "D" {
		return 1, "old"
	}
	return 1, "new"
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
