package plan

import (
	"context"
	"fmt"
	"go/ast"
	"path"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/linter"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// Assessment thresholds. They are constants, recorded in every assessment,
// so that a plan's categories can be recomputed from its document.
const (
	// WideImpactCallers is the number of caller sites in unchanged non-test
	// code from which a planned change of an existing symbol is wide.
	WideImpactCallers = 10
	// LargeScopeFiles is the number of planned files from which a plan is a
	// large change on its own.
	LargeScopeFiles = 20
)

// manifestNames are the dependency manifests and lock files whose change is a
// dependency change, whatever the language.
var manifestNames = map[string]bool{
	"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true,
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"cargo.toml": true, "cargo.lock": true,
	"pyproject.toml": true, "poetry.lock": true, "pipfile": true, "pipfile.lock": true, "setup.py": true, "setup.cfg": true, "uv.lock": true,
}

// IsManifest reports a dependency manifest or lock file by its base name;
// requirements*.txt counts too.
func IsManifest(p string) bool {
	base := strings.ToLower(path.Base(p))
	return manifestNames[base] || strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")
}

// Tree is what Evaluate reads of the base commit: every path it records.
type Tree map[string]bool

// Indexer returns the static index of the base commit, or nil when none
// applies or can be built, with the section describing why. cli passes the
// index built once by symbols.IndexCommit.
type Indexer func(ctx context.Context) (*symbols.Index, *model.Impact, error)

// Input is the evaluation of one plan.
type Input struct {
	BaseCommit     string
	Proposal       model.PlanProposal // already normalized
	SensitiveGlobs []string
	Tree           Tree
	Index          Indexer
}

// Evaluate applies the fixed rules to a normalized proposal. It returns the
// assessment, the contract review --plan checks, and Unverified notes. It
// returns an error only for an invalid sensitive-path glob or a cancelled ctx.
func Evaluate(ctx context.Context, in Input) (model.PlanAssessment, model.PlanContract, []string, error) {
	a := model.PlanAssessment{
		Categories: []model.PlanCategory{}, Signals: []model.Signal{}, CriticalPaths: []model.PlanCriticalPath{},
		Symbols: []model.PlanSymbolImpact{}, Manifests: []string{}, NewPackages: []string{}, Inconsistencies: []string{},
		Thresholds: model.PlanAssessmentBoundary{WideImpactCallers: WideImpactCallers, LargeScopeFiles: LargeScopeFiles},
	}
	var notes []string
	var signals []model.Signal
	add := func(kind, severity, p string, line int, side, symbol, summary, evidence string) {
		if line <= 0 {
			line = 1
		}
		signals = append(signals, model.Signal{Kind: kind, Path: p, Line: line, Side: side, Symbol: symbol, Severity: severity, Summary: summary, Evidence: evidence})
	}
	goDirs := map[string]bool{}
	for p := range in.Tree {
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			goDirs[path.Dir(p)] = true
		}
	}
	inconsistent := func(p, text string) {
		a.Inconsistencies = append(a.Inconsistencies, text)
		add(model.PlanSignalInconsistent, "low", p, 1, "new", "", "The plan does not match the base commit", text)
	}
	newPackages := map[string]bool{}
	manifests := map[string]bool{}
	critical := map[string]string{}
	for _, f := range in.Proposal.Files {
		side := "old"
		switch f.Change {
		case model.PlanFileAdd:
			side = "new"
			if in.Tree[f.Path] {
				inconsistent(f.Path, fmt.Sprintf("%s is planned as added but exists at the base commit", f.Path))
			}
		case model.PlanFileModify, model.PlanFileDelete:
			if !in.Tree[f.Path] {
				inconsistent(f.Path, fmt.Sprintf("%s is planned as %s but does not exist at the base commit", f.Path, map[string]string{model.PlanFileModify: "modified", model.PlanFileDelete: "deleted"}[f.Change]))
			}
		case model.PlanFileRename:
			side = "new"
			if !in.Tree[f.OldPath] {
				inconsistent(f.OldPath, fmt.Sprintf("%s is planned to be renamed but does not exist at the base commit", f.OldPath))
			}
			if in.Tree[f.Path] {
				inconsistent(f.Path, fmt.Sprintf("%s is the target of a planned rename but exists at the base commit", f.Path))
			}
		}
		for _, p := range []string{f.Path, f.OldPath} {
			if p == "" {
				continue
			}
			glob, ok, err := linter.MatchSensitive(in.SensitiveGlobs, p)
			if err != nil {
				return a, model.PlanContract{}, nil, err
			}
			if ok {
				critical[p] = glob
				add(model.PlanSignalCriticalPath, "high", p, 1, side, "", "The plan touches a configured sensitive path", "Path matches configured pattern "+glob)
			}
			if IsManifest(p) && !manifests[p] {
				manifests[p] = true
				add(model.PlanSignalDependency, "medium", p, 1, side, "", "The plan changes a dependency manifest", fmt.Sprintf("%s is a dependency manifest or lock file; the plan %s it", p, verb(f.Change)))
			}
		}
		if f.Change == model.PlanFileDelete {
			add(model.PlanSignalDeletion, "medium", f.Path, 1, "old", "", "The plan deletes a file", f.Path+" exists at the base commit and the plan deletes it")
		}
		if (f.Change == model.PlanFileAdd || f.Change == model.PlanFileRename) && strings.HasSuffix(f.Path, ".go") && !strings.HasSuffix(f.Path, "_test.go") {
			if dir := path.Dir(f.Path); !goDirs[dir] && !newPackages[dir] {
				newPackages[dir] = true
				add(model.PlanSignalNewPackage, "medium", f.Path, 1, "new", "", "The plan creates a Go package", fmt.Sprintf("%s has no non-test Go file at the base commit; the plan adds %s", dir, f.Path))
			}
		}
	}
	for _, d := range in.Proposal.Dependencies {
		manifests[d.Manifest] = true
	}
	if n := len(in.Proposal.Files); n >= LargeScopeFiles {
		add(model.PlanSignalLargeScope, "medium", in.Proposal.Files[0].Path, 1, "new", "", "The plan changes many files", fmt.Sprintf("the plan names %d files; the rule flags %d or more", n, LargeScopeFiles))
	}

	// Symbols: measured on the static index of the base commit.
	var index *symbols.Index
	a.Index = model.PlanIndex{Status: model.ImpactNotApplicable, Reason: "the plan changes no existing symbol"}
	measured := false
	for _, s := range in.Proposal.Symbols {
		if s.Change != model.PlanSymbolAdd {
			measured = true
		}
	}
	if measured && in.Index != nil {
		x, section, err := in.Index(ctx)
		if err != nil {
			return a, model.PlanContract{}, nil, err
		}
		index = x
		if section != nil {
			a.Index = model.PlanIndex{Status: section.Status, Reason: section.Reason}
		}
		switch a.Index.Status {
		case model.ImpactLimited, model.ImpactUnavailable:
			notes = append(notes, "The static index of the base commit is "+a.Index.Status+": callers and reaching tests of planned symbols may be missing ("+a.Index.Reason+").")
		}
	}
	for _, s := range in.Proposal.Symbols {
		impact := model.PlanSymbolImpact{Path: s.Path, Name: s.Name, Change: s.Change, Callers: []model.ImpactCaller{}, Tests: []model.ImpactTest{}}
		if strings.HasSuffix(s.Path, ".go") {
			impact.Exported = ast.IsExported(lastName(s.Name))
		}
		side := "old"
		if s.Change == model.PlanSymbolAdd {
			side = "new"
			impact.Reason = "the plan adds this symbol; nothing exists to measure at the base commit"
		} else if index == nil {
			impact.Reason = "no static index covers this file at the base commit"
			if a.Index.Reason != "" && a.Index.Status != model.ImpactNotApplicable {
				impact.Reason += " (" + a.Index.Reason + ")"
			}
			add(model.PlanSignalUnmeasured, "low", s.Path, 1, side, s.Name, "Planned symbol not measured", impact.Reason)
		} else {
			reach, found, err := index.ReachOf(ctx, s.Path, s.Name)
			if err != nil {
				return a, model.PlanContract{}, nil, err
			}
			if !found {
				impact.Reason = "the static index holds no such function or method in this file at the base commit"
				if indexedSource(s.Path) {
					inconsistent(s.Path, fmt.Sprintf("%s is planned as changed in %s, but the static index holds no such function or method there (types, variables and constants are not indexed)", s.Name, s.Path))
				} else {
					add(model.PlanSignalUnmeasured, "low", s.Path, 1, side, s.Name, "Planned symbol not measured", "the static index does not read this kind of file")
				}
			} else {
				impact.Found, impact.Symbol, impact.Line, impact.Signature = true, reach.Symbol, reach.Line, reach.Signature
				impact.Exported = impact.Exported || reach.Exported
				impact.CallersTotal, impact.Callers, impact.TestsTotal, impact.Tests, impact.Complete = reach.CallersTotal, reach.Callers, reach.TestsTotal, reach.Tests, reach.Complete
				if !reach.Complete {
					impact.Reason = "a search stopped at its bound; more callers or tests may exist"
				}
				if impact.CallersTotal >= WideImpactCallers {
					add(model.PlanSignalWideImpact, "medium", s.Path, impact.Line, side, s.Name, "The plan changes a widely referenced symbol", fmt.Sprintf("%d reference sites in unchanged non-test code (static index, approximate); the rule flags %d or more", impact.CallersTotal, WideImpactCallers))
				}
				if impact.CallersTotal > 0 && impact.TestsTotal == 0 {
					add(model.PlanSignalUntestedImpact, "medium", s.Path, impact.Line, side, s.Name, "The plan changes a referenced symbol that no test reaches", fmt.Sprintf("%d reference sites and no test reaching it within %d references (static index, approximate)", impact.CallersTotal, symbols.MaxDepth))
				}
			}
		}
		if impact.Exported && (s.Change == model.PlanSymbolSignature || s.Change == model.PlanSymbolRemove) {
			add(model.PlanSignalExportedSignature, "high", s.Path, impact.Line, side, s.Name, "The plan changes an exported API", fmt.Sprintf("exported %s is planned to %s", s.Name, map[string]string{model.PlanSymbolSignature: "change its signature", model.PlanSymbolRemove: "be removed"}[s.Change]))
		}
		if kind := linter.FunctionRisk(s.Name); kind != "" && s.Change != model.PlanSymbolAdd {
			domain := "authentication or authorization"
			if kind == "sensitive_function_change" {
				domain = "payment"
			}
			add(model.PlanSignalSensitiveSymbol, "high", s.Path, impact.Line, side, s.Name, "The plan changes a sensitive function", "the name suggests "+domain+" behavior (name heuristic of the linter, not a verified property)")
		}
		a.Symbols = append(a.Symbols, impact)
	}

	a.Signals = linter.Merge(nil, signals)
	if a.Signals == nil {
		a.Signals = []model.Signal{}
	}
	a.Categories, a.Major = Categorize(a.Signals)
	for p, glob := range critical {
		a.CriticalPaths = append(a.CriticalPaths, model.PlanCriticalPath{Path: p, Pattern: glob})
	}
	sort.Slice(a.CriticalPaths, func(i, j int) bool { return a.CriticalPaths[i].Path < a.CriticalPaths[j].Path })
	a.Manifests = sortedKeys(manifests)
	a.NewPackages = sortedKeys(newPackages)

	contract := model.PlanContract{BaseCommit: in.BaseCommit, Files: []string{}, Symbols: []model.PlanContractSymbol{}, CriticalFiles: []string{}, Manifests: a.Manifests, NewPackages: a.NewPackages}
	files := map[string]bool{}
	for _, f := range in.Proposal.Files {
		files[f.Path] = true
		if f.OldPath != "" {
			files[f.OldPath] = true
		}
	}
	contract.Files = sortedKeys(files)
	for _, s := range in.Proposal.Symbols {
		contract.Symbols = append(contract.Symbols, model.PlanContractSymbol{Path: s.Path, Name: s.Name, Change: s.Change})
	}
	for _, c := range a.CriticalPaths {
		contract.CriticalFiles = append(contract.CriticalFiles, c.Path)
	}
	contract.Dependencies = len(in.Proposal.Dependencies) > 0 || len(a.Manifests) > 0
	return a, contract, notes, nil
}

// Categorize derives the four categories from plan signals: a category is
// flagged when one of its signals is medium or higher, and the plan is major
// when a category is flagged.
func Categorize(signals []model.Signal) ([]model.PlanCategory, bool) {
	order := []string{model.PlanCategoryCriticalParts, model.PlanCategoryArchitecture, model.PlanCategoryRegressionRisk, model.PlanCategoryOtherMajor}
	byName := map[string]*model.PlanCategory{}
	out := make([]model.PlanCategory, len(order))
	for i, name := range order {
		out[i] = model.PlanCategory{Name: name, SignalIDs: []string{}}
		byName[name] = &out[i]
	}
	major := false
	for _, s := range signals {
		c := byName[model.PlanSignalCategory[s.Kind]]
		if c == nil {
			continue
		}
		c.SignalIDs = append(c.SignalIDs, s.ID)
		if s.Severity == "medium" || s.Severity == "high" || s.Severity == "critical" {
			c.Flagged = true
			major = true
		}
	}
	return out, major
}

func verb(change string) string {
	switch change {
	case model.PlanFileAdd:
		return "adds"
	case model.PlanFileDelete:
		return "deletes"
	case model.PlanFileRename:
		return "renames"
	default:
		return "modifies"
	}
}

// indexedSource reports a file kind the static index reads.
func indexedSource(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".go", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs":
		return true
	}
	return false
}

func lastName(name string) string { return name[strings.LastIndex(name, ".")+1:] }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
