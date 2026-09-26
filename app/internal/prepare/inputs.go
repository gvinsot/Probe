package prepare

import (
	"path"
	"sort"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Matching returns the first prepare.inputs pattern that matches the
// repository path p, or "" when none does. Patterns use path.Match: '*' never
// crosses '/', and a pattern without metacharacters is an exact path.
func Matching(patterns []string, p string) string {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, p); err == nil && ok {
			return pattern
		}
	}
	return ""
}

// ChangedInputs returns the changed files whose new or old path matches a
// declared input, sorted and unique. A nil spec has none.
func ChangedInputs(spec *config.Prepare, change model.Change) []string {
	if spec == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range change.Files {
		if Matching(spec.Inputs, f.Path) == "" && (f.OldPath == "" || Matching(spec.Inputs, f.OldPath) == "") {
			continue
		}
		if !seen[f.Path] {
			seen[f.Path] = true
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Signals returns one medium prepare_input_changed signal per changed file
// whose new or old path matches a declared input (lint and review). It is
// anchored at the file's first changed line, like dependency_change, so that a
// long lockfile hunk does not inflate the focused lines. IDs are assigned by
// linter.Merge. A nil spec has none.
func Signals(spec *config.Prepare, change model.Change) []model.Signal {
	if spec == nil {
		return nil
	}
	var out []model.Signal
	for _, f := range change.Files {
		pattern := Matching(spec.Inputs, f.Path)
		if pattern == "" && f.OldPath != "" {
			pattern = Matching(spec.Inputs, f.OldPath)
		}
		if pattern == "" {
			continue
		}
		line, side := linter.FirstChangedLine(f)
		out = append(out, model.Signal{
			Kind: model.SignalPrepareInputChanged, Path: f.Path, Line: line, Side: side, Severity: "medium",
			Summary:  "Dependency-preparation input changed",
			Evidence: "Matches prepare.inputs pattern " + pattern + ". Sandbox checks use dependencies prepared from the base commit's version of the declared inputs only; the candidate's version is never installed, so checks may fail or behave differently for that reason alone.",
		})
	}
	return out
}
