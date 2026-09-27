package symbols

import (
	"context"
	"go/ast"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// IndexCommit builds the static index of a whole commit, for a pre-change
// plan: the Go packages and the lexical sources of the tree, exactly as
// Analyze builds them for a change, with nothing compared since no change
// exists yet. The returned section carries the index status and reason
// (not_applicable when the tree holds no indexable source, indexed, limited
// or unavailable); its changed-function list is empty. It returns an error
// only when ctx is cancelled.
func IndexCommit(ctx context.Context, repo *gitrepo.Repository, commit string, opts Options) (*Index, *model.Impact, error) {
	tree, err := repo.Tree(ctx, commit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, &model.Impact{Status: model.ImpactUnavailable, Reason: "the base tree could not be listed: " + shortError(err), ChangedFunctions: []model.ImpactFunction{}, Note: model.ImpactNote}, nil
	}
	// One indexable file of each kind makes Analyze build that part of the
	// index over the whole tree; comparing it with itself finds no change.
	change := model.Change{BaseCommit: commit, HeadCommit: commit}
	var goFile, lexFile bool
	for _, e := range tree {
		if !regularBlob(e) {
			continue
		}
		switch {
		case !goFile && strings.HasSuffix(e.Path, ".go") && indexable(e.Path, opts.Sensitive):
			goFile = true
		case !lexFile && lexIndexable(e.Path, opts.Sensitive):
			lexFile = true
		default:
			continue
		}
		change.Files = append(change.Files, model.ChangedFile{Path: e.Path, Status: "M", Hunks: []model.Hunk{}})
	}
	res, err := Analyze(ctx, repo, change, opts)
	if err != nil {
		return nil, nil, err
	}
	return res.Index(), res.Report(), nil
}

// Reach is the static reach of one declaration: its reference sites in
// non-test code and the tests reaching it within MaxDepth references. It is
// approximate like every index answer, and an empty list is not proof of
// absence.
type Reach struct {
	Symbol       string
	Kind         string
	Line         int
	Signature    string
	Exported     bool
	Callers      []model.ImpactCaller
	CallersTotal int
	Tests        []model.ImpactTest
	TestsTotal   int
	// Complete is false when a search stopped at one of its bounds; what was
	// found is still listed.
	Complete bool
}

// ReachOf measures the function or method name ("F" or "T.M") declared in
// path. found is false when the index holds no such declaration. A Go name is
// exported when its final identifier is; a lexical declaration is never
// reported as exported, since the lexical index does not read visibility.
func (x *Index) ReachOf(ctx context.Context, path, name string) (Reach, bool, error) {
	if x == nil {
		return Reach{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return Reach{}, false, err
	}
	id := int32(-1)
	for i, d := range x.decls {
		if d.Path == path && (d.Name == name || d.Kind == KindMethod && trimPointer(d.Name) == trimPointer(name)) && d.Kind != KindPackageInit && d.Kind != KindInterfaceMethod {
			id = int32(i)
			break
		}
	}
	if id < 0 {
		return Reach{}, false, nil
	}
	d := x.decls[id]
	out := Reach{Symbol: d.Key, Kind: d.Kind, Line: d.Line, Signature: d.Signature, Exported: !d.lexical() && ast.IsExported(lastName(d.Name)), Complete: true}
	bud := newBudget(ctx, time.Now().Add(queryTimeout), maxQueryWork)
	sites, ok := x.sites(id, map[string]bool{}, map[string]map[int]bool{}, bud)
	out.Complete = out.Complete && ok
	out.CallersTotal = len(sites)
	for i, s := range sites {
		if i == MaxCallersListed {
			break
		}
		resolution := model.ResolutionStatic
		switch {
		case d.lexical():
			resolution = model.ResolutionName
		case s.iface:
			resolution = model.ResolutionInterface
		}
		out.Callers = append(out.Callers, model.ImpactCaller{Path: x.files[s.file], Line: int(s.line), Symbol: x.decls[s.caller].Key, Depth: 1, Resolution: resolution})
	}
	bud = newBudget(ctx, time.Now().Add(queryTimeout), maxQueryWork)
	tests, total, ok, capped := x.reachingTests(id, x.mods, map[string]bool{}, bud)
	out.Complete = out.Complete && ok && !capped
	out.Tests, out.TestsTotal = tests, total
	if bud.cancelled {
		if err := ctx.Err(); err != nil {
			return Reach{}, false, err
		}
		return Reach{}, false, context.Canceled
	}
	if out.Callers == nil {
		out.Callers = []model.ImpactCaller{}
	}
	return out, true, nil
}

// trimPointer drops the pointer receiver notation of "(*T).M" or "*T.M".
func trimPointer(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		if name[i] != '(' && name[i] != ')' && name[i] != '*' {
			out = append(out, name[i])
		}
	}
	return string(out)
}
