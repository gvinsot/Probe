// Package plan evaluates a model-written change plan deterministically and
// checks a later diff against it. The model proposes; this package measures
// what the proposal names at the base commit and never reads a model claim
// about risk. Nothing here executes repository code.
package plan

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
)

// Proposal bounds.
const (
	MaxFiles        = 500
	MaxSymbols      = 500
	MaxDependencies = 100
	MaxSteps        = 50
	MaxAssumptions  = 50
	maxSummaryBytes = 4000
	maxTextBytes    = 1000
	maxNameBytes    = 200
)

var symbolName = regexp.MustCompile(`^[\p{L}_$][\p{L}\p{N}_$]*(\.[\p{L}_$][\p{L}\p{N}_$]*)?$`)

// Normalize validates a proposal and returns it with sorted, trimmed lists.
// Errors are addressed to the model, which may submit a corrected plan.
func Normalize(p model.PlanProposal) (model.PlanProposal, error) {
	p.Summary = strings.TrimSpace(p.Summary)
	if p.Summary == "" || len(p.Summary) > maxSummaryBytes {
		return p, fmt.Errorf("summary is required and must be at most %d bytes", maxSummaryBytes)
	}
	if len(p.Files) == 0 || len(p.Files) > MaxFiles {
		return p, fmt.Errorf("files must list 1 to %d files", MaxFiles)
	}
	if len(p.Symbols) > MaxSymbols || len(p.Dependencies) > MaxDependencies || len(p.Steps) > MaxSteps || len(p.Assumptions) > MaxAssumptions {
		return p, fmt.Errorf("at most %d symbols, %d dependencies, %d steps and %d assumptions", MaxSymbols, MaxDependencies, MaxSteps, MaxAssumptions)
	}
	var err error
	if p.Steps, err = texts("steps", p.Steps); err != nil {
		return p, err
	}
	if p.Assumptions, err = texts("assumptions", p.Assumptions); err != nil {
		return p, err
	}
	planned := map[string]string{}
	for i := range p.Files {
		f := &p.Files[i]
		f.Path, f.OldPath, f.Reason = strings.TrimSpace(f.Path), strings.TrimSpace(f.OldPath), strings.TrimSpace(f.Reason)
		if err := validPath(f.Path); err != nil {
			return p, fmt.Errorf("files[%d].path: %w", i, err)
		}
		switch f.Change {
		case model.PlanFileAdd, model.PlanFileModify, model.PlanFileDelete:
			if f.OldPath != "" {
				return p, fmt.Errorf("files[%d]: old_path is accepted only with change rename", i)
			}
		case model.PlanFileRename:
			if err := validPath(f.OldPath); err != nil {
				return p, fmt.Errorf("files[%d].old_path: %w", i, err)
			}
			if f.OldPath == f.Path {
				return p, fmt.Errorf("files[%d]: a rename needs a different old_path", i)
			}
		default:
			return p, fmt.Errorf("files[%d].change must be add, modify, delete or rename", i)
		}
		if len(f.Reason) > maxTextBytes || !utf8.ValidString(f.Reason) {
			return p, fmt.Errorf("files[%d].reason must be UTF-8 of at most %d bytes", i, maxTextBytes)
		}
		for _, q := range []string{f.Path, f.OldPath} {
			if q == "" {
				continue
			}
			if _, dup := planned[q]; dup {
				return p, fmt.Errorf("path %q is planned more than once", q)
			}
			planned[q] = f.Change
		}
	}
	seen := map[[2]string]bool{}
	for i := range p.Symbols {
		s := &p.Symbols[i]
		s.Path, s.Name, s.Reason = strings.TrimSpace(s.Path), normalizeName(s.Name), strings.TrimSpace(s.Reason)
		change, ok := planned[s.Path]
		if !ok {
			return p, fmt.Errorf("symbols[%d].path must be one of the planned files", i)
		}
		if len(s.Name) > maxNameBytes || !symbolName.MatchString(s.Name) {
			return p, fmt.Errorf("symbols[%d].name must be a declaration name such as F or T.M", i)
		}
		switch s.Change {
		case model.PlanSymbolAdd, model.PlanSymbolBody, model.PlanSymbolSignature, model.PlanSymbolRemove:
		default:
			return p, fmt.Errorf("symbols[%d].change must be add, body, signature or remove", i)
		}
		if change == model.PlanFileAdd && s.Change != model.PlanSymbolAdd {
			return p, fmt.Errorf("symbols[%d]: a file the plan adds can only receive added symbols", i)
		}
		if change == model.PlanFileDelete && s.Change != model.PlanSymbolRemove {
			return p, fmt.Errorf("symbols[%d]: a file the plan deletes can only lose symbols", i)
		}
		if len(s.Reason) > maxTextBytes || !utf8.ValidString(s.Reason) {
			return p, fmt.Errorf("symbols[%d].reason must be UTF-8 of at most %d bytes", i, maxTextBytes)
		}
		k := [2]string{s.Path, s.Name}
		if seen[k] {
			return p, fmt.Errorf("symbol %s in %s is planned more than once", s.Name, s.Path)
		}
		seen[k] = true
	}
	for i := range p.Dependencies {
		d := &p.Dependencies[i]
		d.Manifest, d.Name, d.Version = strings.TrimSpace(d.Manifest), strings.TrimSpace(d.Name), strings.TrimSpace(d.Version)
		if _, ok := planned[d.Manifest]; !ok {
			return p, fmt.Errorf("dependencies[%d].manifest must be one of the planned files", i)
		}
		if d.Name == "" || len(d.Name) > maxNameBytes || len(d.Version) > maxNameBytes || strings.ContainsAny(d.Name+d.Version, "\x00\r\n") {
			return p, fmt.Errorf("dependencies[%d]: name is required; name and version are single lines of at most %d bytes", i, maxNameBytes)
		}
		switch d.Change {
		case model.PlanDependencyAdd, model.PlanDependencyUpgrade, model.PlanDependencyRemove:
		default:
			return p, fmt.Errorf("dependencies[%d].change must be add, upgrade or remove", i)
		}
	}
	sort.SliceStable(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	sort.SliceStable(p.Symbols, func(i, j int) bool {
		if p.Symbols[i].Path != p.Symbols[j].Path {
			return p.Symbols[i].Path < p.Symbols[j].Path
		}
		return p.Symbols[i].Name < p.Symbols[j].Name
	})
	sort.SliceStable(p.Dependencies, func(i, j int) bool {
		if p.Dependencies[i].Manifest != p.Dependencies[j].Manifest {
			return p.Dependencies[i].Manifest < p.Dependencies[j].Manifest
		}
		return p.Dependencies[i].Name < p.Dependencies[j].Name
	})
	if p.Steps == nil {
		p.Steps = []string{}
	}
	if p.Assumptions == nil {
		p.Assumptions = []string{}
	}
	if p.Symbols == nil {
		p.Symbols = []model.PlannedSymbol{}
	}
	if p.Dependencies == nil {
		p.Dependencies = []model.PlannedDependency{}
	}
	return p, nil
}

func texts(field string, values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for i, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > maxTextBytes || !utf8.ValidString(v) {
			return nil, fmt.Errorf("%s[%d] must be non-empty UTF-8 of at most %d bytes", field, i, maxTextBytes)
		}
		out = append(out, v)
	}
	return out, nil
}

func validPath(p string) error {
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\r\n\\") {
		return errors.New("a repository-relative path is required")
	}
	if err := gitrepo.SafePath(p); err != nil {
		return err
	}
	if path.Clean(p) != p {
		return errors.New("the path must be clean (no ./, trailing / or duplicate separators)")
	}
	return nil
}

// normalizeName drops the pointer notation of a method: "(*T).M" is "T.M".
func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	return strings.NewReplacer("(*", "", "(", "", ")", "", "*", "").Replace(name)
}
