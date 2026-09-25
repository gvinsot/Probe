package symbols

import (
	"context"
	"fmt"
	"go/token"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// Result is the impact analysis of one change.
type Result struct {
	report  *model.Impact
	signals []model.Signal
	index   *Index
}

// Report returns the impact section. It is never nil.
func (r *Result) Report() *model.Impact { return r.report }

// Signals returns the impacted_caller signals and at most one analysis_limited
// signal, unsorted (callers merge them with linter.Merge).
func (r *Result) Signals() []model.Signal { return r.signals }

// Index returns the symbol index, or nil when none was built (not applicable
// or unavailable). Callers must not store a nil *Index in an interface.
func (r *Result) Index() *Index { return r.index }

// analysis carries the state of one Analyze call.
type analysis struct {
	lim           Limits
	change        model.Change
	impact        *model.Impact
	reasons       []string
	unknown       bool // some changed functions could not be determined
	changed       []changedFunction
	touched       map[string]bool // fnKey of every changed or added function
	fileReason    map[string]string
	firstGoPath   string
	firstGoSide   string
	changedPaths  map[string]bool
	signals       []model.Signal
	remainder     int
	firstUnlisted *model.Signal
}

func (a *analysis) limit(reason string) {
	for _, r := range a.reasons {
		if r == reason {
			return
		}
	}
	a.reasons = append(a.reasons, reason)
}

// Analyze builds the static index of the candidate commit and the impact of
// the change on it. It returns an error only when ctx is cancelled: every
// other failure degrades the section to limited or unavailable with a reason.
// Nothing in the repository is executed.
func Analyze(ctx context.Context, repo *gitrepo.Repository, change model.Change, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := &analysis{
		lim:          opts.Limits.withDefaults(),
		change:       change,
		impact:       &model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{}, Note: model.ImpactNote},
		touched:      map[string]bool{},
		fileReason:   map[string]string{},
		changedPaths: map[string]bool{},
	}
	deadline := time.Now().Add(a.lim.Timeout)
	headPaths, anyGo := changedGoFiles(change, opts.Sensitive)
	if !anyGo {
		a.impact.Status, a.impact.Reason = model.ImpactNotApplicable, "no changed Go file"
		return &Result{report: a.impact}, nil
	}
	for _, f := range change.Files {
		a.changedPaths[f.Path] = true
		if f.OldPath != "" {
			a.changedPaths[f.OldPath] = true
		}
		if a.firstGoPath == "" && strings.HasSuffix(f.Path, ".go") && indexable(f.Path, opts.Sensitive) {
			if f.Status != "D" {
				a.firstGoPath, a.firstGoSide = f.Path, "new"
			}
		}
	}
	if a.firstGoPath == "" {
		for _, f := range change.Files {
			p := f.Path
			if f.OldPath != "" {
				p = f.OldPath
			}
			if strings.HasSuffix(p, ".go") && indexable(p, opts.Sensitive) {
				a.firstGoPath, a.firstGoSide = p, "old"
				break
			}
		}
	}

	headTree, err := repo.Tree(ctx, change.HeadCommit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.unknown = true
		return a.unavailable("the candidate tree could not be listed: " + shortError(err)), nil
	}
	entries := selectEntries(headTree, opts.Sensitive)
	byPath := map[string]gitrepo.TreeEntry{}
	var readable []gitrepo.TreeEntry
	var total int64
	large := 0
	for _, e := range entries {
		byPath[e.Path] = e
		if e.Size > a.lim.MaxFileBytes {
			large++
			a.fileReason[e.Path] = fmt.Sprintf("the file is larger than %d bytes and was not indexed", a.lim.MaxFileBytes)
			continue
		}
		readable = append(readable, e)
		total += e.Size
	}
	tooBig := ""
	switch {
	case len(readable) > a.lim.MaxFiles:
		tooBig = fmt.Sprintf("the candidate tree has more than %d Go and go.mod files", a.lim.MaxFiles)
	case total > a.lim.MaxBytes:
		tooBig = fmt.Sprintf("the Go sources of the candidate tree exceed %d bytes", a.lim.MaxBytes)
	}
	toRead := readable
	wanted := map[string]bool{}
	for _, p := range headPaths {
		wanted[p] = true
	}
	if tooBig != "" {
		// Only the changed files are read, to list the changed functions.
		toRead = nil
		for _, e := range readable {
			if wanted[e.Path] {
				toRead = append(toRead, e)
			}
		}
	}
	head, err := readEntries(ctx, repo, toRead, a.lim.MaxFileBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.unknown = true
		return a.unavailable("the candidate Go sources could not be read: " + shortError(err)), nil
	}

	if err := a.changedFunctions(ctx, repo, head, byPath, opts.Sensitive); err != nil {
		return nil, err
	}
	if tooBig != "" {
		return a.unavailable(tooBig), nil
	}
	mods := discoverModules(head)
	usable := false
	for _, m := range mods.list {
		usable = usable || m.path != ""
	}
	if !usable {
		if len(mods.list) == 0 {
			return a.unavailable("no go.mod file in the committed tree"), nil
		}
		return a.unavailable("no go.mod file of the committed tree has a readable module path"), nil
	}
	if mods.unreadable > 0 {
		a.limit(fmt.Sprintf("%d go.mod files have no readable module path; their packages were not indexed", mods.unreadable))
	}
	for _, dir := range mods.duplicates {
		a.limit(fmt.Sprintf("the module in %q repeats the module path of another go.mod and was not indexed", dir))
	}
	if large > 0 {
		a.limit(fmt.Sprintf("%d Go files larger than %d bytes were not indexed", large, a.lim.MaxFileBytes))
	}

	// Build-constraint selection and parsing.
	ctxt := buildContext(head)
	var goPaths []string
	for p := range head {
		if strings.HasSuffix(p, ".go") {
			goPaths = append(goPaths, p)
		}
	}
	sort.Strings(goPaths)
	var candidates []string
	headerErrors := 0
	for _, p := range goPaths {
		match, err := ctxt.MatchFile(path.Dir(p), path.Base(p))
		switch {
		case err != nil:
			headerErrors++
			a.fileReason[p] = "the file could not be parsed"
		case !match:
			a.fileReason[p] = "the file is excluded by the linux/amd64 build constraints"
		default:
			candidates = append(candidates, p)
		}
	}
	fset := token.NewFileSet()
	asts := parseFiles(ctx, fset, candidates, head)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var files []parsed
	parseErrors := headerErrors
	for i, f := range asts {
		if f == nil {
			parseErrors++
			a.fileReason[candidates[i]] = "the file could not be parsed"
			continue
		}
		files = append(files, parsed{path: candidates[i], file: f})
	}
	if parseErrors > 0 {
		a.limit(fmt.Sprintf("%d Go files could not be parsed and were not indexed", parseErrors))
	}
	g := groupPackages(files, mods)
	for _, f := range files {
		if _, ok := g.indexedPaths[f.path]; ok {
			continue
		}
		if _, ok := mods.importPath(path.Dir(f.path)); !ok {
			a.fileReason[f.path] = "the file is outside every Go module with a readable module path"
		} else {
			a.fileReason[f.path] = "the file's package clause differs from its directory's package"
		}
	}
	if g.clauseSkips > 0 {
		a.limit(fmt.Sprintf("%d Go files were not indexed because their package clause differs from their directory's package", g.clauseSkips))
	}
	if len(g.nodes) > a.lim.MaxPackages {
		return a.unavailable(fmt.Sprintf("the candidate tree has more than %d Go packages", a.lim.MaxPackages)), nil
	}

	// Type checking in dependency order.
	b := newBuilder(fset, a.lim, deadline)
	for _, n := range order(g.nodes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			b.timedOut++
			for _, p := range n.paths {
				a.fileReason[p] = "the index time limit was reached before its package"
			}
			n.files = nil
			continue
		}
		b.check(n)
		switch {
		case n.timedOut:
			for _, p := range n.paths {
				a.fileReason[p] = "the index time limit was reached during the type check of its package"
			}
		case n.failed:
			for _, p := range n.paths {
				a.fileReason[p] = "the type check of its package stopped unexpectedly"
			}
		}
	}
	if b.failed > 0 {
		a.limit(fmt.Sprintf("the type check of %d packages stopped unexpectedly; their declarations may be missing", b.failed))
	}
	if b.timedOut > 0 {
		a.limit(fmt.Sprintf("the index time limit (%s) was reached; %d packages were not indexed", a.lim.Timeout, b.timedOut))
	}
	if b.edgeCap {
		a.limit(fmt.Sprintf("the reference limit (%d) was reached; later references are not recorded", a.lim.MaxEdges))
	}
	if b.nameCap {
		a.limit(fmt.Sprintf("the limit of unresolved method calls (%d) was reached", a.lim.MaxNameSites))
	}
	content := map[string][]byte{}
	indexed := 0
	for _, n := range g.nodes {
		if !n.checked || n.failed {
			continue
		}
		for _, p := range n.paths {
			content[p] = head[p]
			indexed++
		}
	}
	x := b.finish(content, indexed)
	a.impact.IndexedFiles = indexed
	a.describe(x, mods)
	a.finishStatus()
	return &Result{report: a.impact, signals: a.limitedSignal(), index: x}, nil
}

// changedFunctions reads the baseline versions of the changed files and
// classifies their functions. A changed file that cannot be compared (too
// large on a side, or not parseable) is left out as a whole and makes the
// section limited.
func (a *analysis) changedFunctions(ctx context.Context, repo *gitrepo.Repository, head map[string][]byte, headEntries map[string]gitrepo.TreeEntry, sensitive func(string) bool) error {
	var pairs []sourcePair
	needBase := false
	for _, f := range a.change.Files {
		oldPath := f.Path
		if f.OldPath != "" {
			oldPath = f.OldPath
		}
		var p sourcePair
		if f.Status != "D" && goSource(f.Path) && indexable(f.Path, sensitive) {
			p.headPath = f.Path
		}
		if f.Status != "A" && goSource(oldPath) && indexable(oldPath, sensitive) {
			p.basePath = oldPath
			needBase = true
		}
		if p.headPath != "" || p.basePath != "" {
			pairs = append(pairs, p)
		}
	}
	baseEntries := map[string]gitrepo.TreeEntry{}
	if needBase {
		tree, err := repo.Tree(ctx, a.change.BaseCommit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.unknown = true
			a.limit("the baseline versions of changed Go files could not be listed: " + shortError(err))
			return nil
		}
		for _, e := range selectEntries(tree, sensitive) {
			baseEntries[e.Path] = e
		}
	}
	failed := 0
	var kept []sourcePair
	var read []gitrepo.TreeEntry
	for _, p := range pairs {
		headEntry, inHead := headEntries[p.headPath]
		baseEntry, inBase := baseEntries[p.basePath]
		if inHead && headEntry.Size > a.lim.MaxFileBytes || inBase && baseEntry.Size > a.lim.MaxFileBytes {
			failed++
			continue
		}
		// A side that is not a regular file there (a symlink after a type
		// change, or absent) counts as deleted or added.
		if data, ok := head[p.headPath]; inHead && ok {
			p.head = data
		} else {
			p.headPath = ""
		}
		if inBase {
			read = append(read, baseEntry)
		} else {
			p.basePath = ""
		}
		kept = append(kept, p)
	}
	sort.Slice(read, func(i, j int) bool { return read[i].Path < read[j].Path })
	baseSources, err := readEntries(ctx, repo, read, a.lim.MaxFileBytes)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.unknown = true
		a.limit("the baseline versions of changed Go files could not be read: " + shortError(err))
		return nil
	}
	for i := range kept {
		if kept[i].basePath != "" {
			kept[i].base = baseSources[kept[i].basePath]
		}
	}
	changed, touched, parseFailed := compareFunctions(kept)
	failed += parseFailed
	if failed > 0 {
		a.unknown = true
		a.limit(fmt.Sprintf("%d changed Go files could not be compared (too large or not parseable); their functions are not listed", failed))
	}
	if len(changed) > a.lim.MaxChangedFunctions {
		a.limit(fmt.Sprintf("%d further changed functions are not listed (at most %d)", len(changed)-a.lim.MaxChangedFunctions, a.lim.MaxChangedFunctions))
		a.unknown = true
		changed = changed[:a.lim.MaxChangedFunctions]
	}
	a.changed, a.touched = changed, touched
	return nil
}

// unavailable finishes a section whose index could not be built. The changed
// functions found so far are listed as not indexed.
func (a *analysis) unavailable(reason string) *Result {
	a.impact.Status = model.ImpactUnavailable
	a.impact.Reason = joinReasons(append([]string{reason}, a.reasons...))
	for _, cf := range a.changed {
		a.impact.ChangedFunctions = append(a.impact.ChangedFunctions, model.ImpactFunction{
			Path: cf.path, Line: cf.line, EndLine: cf.end, Symbol: cf.pkgName + "." + cf.name, Change: cf.change,
			Callers: []model.ImpactCaller{}, Tests: []model.ImpactTest{},
			Reason: "the static Go index is unavailable; callers and tests were not searched",
		})
	}
	return &Result{report: a.impact, signals: a.limitedSignal()}
}

func (a *analysis) finishStatus() {
	if len(a.reasons) > 0 {
		a.impact.Status = model.ImpactLimited
		a.impact.Reason = joinReasons(a.reasons)
	}
}

func joinReasons(reasons []string) string { return strings.Join(reasons, "; ") }

// shortError is the first line of an error, redacted and cut to 240 bytes.
func shortError(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return redact.TruncateUTF8(redact.Redact(strings.TrimSpace(s)), 240)
}

// site is one reference to a changed function in unchanged, non-test code.
type site struct {
	file, line int32
	caller     int32
	via        int32 // the referenced declaration: the function itself, or an interface method it may be dispatched from
	iface      bool
	call       bool
}

// describe fills the changed functions, their callers and reaching tests, and
// the capped impacted_caller signals.
func (a *analysis) describe(x *Index, mods modules) {
	added := addedLines(a.change)
	touchedKeys := map[string]bool{}
	for key := range a.touched {
		parts := strings.SplitN(key, "\x00", 3)
		if ip, ok := mods.importPath(parts[0]); ok && len(parts) == 3 {
			touchedKeys[ip+"."+parts[2]] = true
		}
	}
	signalled := 0
	for _, cf := range a.changed {
		fn := model.ImpactFunction{Path: cf.path, Line: cf.line, EndLine: cf.end, Symbol: cf.pkgName + "." + cf.name, Change: cf.change, Callers: []model.ImpactCaller{}, Tests: []model.ImpactTest{}}
		importPath, inModule := mods.importPath(cf.dir)
		id, found := int32(-1), false
		if inModule {
			fn.Symbol = importPath + "." + cf.name
			id, found = x.byKey[fn.Symbol]
		}
		switch {
		case a.fileReason[cf.path] != "":
			fn.Reason = a.fileReason[cf.path]
		case !inModule:
			fn.Reason = "the file is outside every Go module with a readable module path"
		case !found || x.decls[id].Path != cf.path:
			fn.Reason = "the declaration is not in the static Go index"
		default:
			fn.Indexed = true
		}
		if !fn.Indexed {
			a.impact.ChangedFunctions = append(a.impact.ChangedFunctions, fn)
			continue
		}
		x.changed[id] = cf.change
		sites := x.sites(id, touchedKeys, added)
		fn.CallersTotal = len(sites)
		for i, s := range sites {
			if i < MaxCallersListed {
				resolution := model.ResolutionStatic
				if s.iface {
					resolution = model.ResolutionInterface
				}
				fn.Callers = append(fn.Callers, model.ImpactCaller{Path: x.files[s.file], Line: int(s.line), Symbol: x.decls[s.caller].Key, Depth: 1, Resolution: resolution})
			}
			if i < MaxSignalsPerFunction && signalled < MaxSignalsTotal {
				a.signals = append(a.signals, callerSignal(x, fn, s))
				signalled++
				continue
			}
			a.remainder++
			if a.firstUnlisted == nil {
				a.firstUnlisted = &model.Signal{Path: x.files[s.file], Line: int(s.line), Side: "new"}
			}
		}
		fn.Tests, fn.TestsTotal = x.reachingTests(id, mods, a.changedPaths)
		a.impact.ChangedFunctions = append(a.impact.ChangedFunctions, fn)
	}
}

// sites lists the references to a changed function in unchanged, non-test
// code: static references, and calls of interface methods its receiver type
// implements. A reference inside a changed or added function, on an added
// line, or in a test file is part of the change or of the tests and is not
// listed. One site is kept per line, preferring a static reference.
func (x *Index) sites(id int32, touched map[string]bool, added map[string]map[int]bool) []site {
	var out []site
	byLine := map[[2]int32]int{}
	consider := func(e edge, via int32, iface bool) {
		file := x.files[e.file]
		if strings.HasSuffix(file, "_test.go") || touched[x.decls[e.caller].Key] || added[file][int(e.line)] {
			return
		}
		k := [2]int32{e.file, e.line}
		if i, ok := byLine[k]; ok {
			if out[i].iface && !iface {
				out[i] = site{file: e.file, line: e.line, caller: e.caller, via: via, iface: false, call: e.call}
			}
			return
		}
		byLine[k] = len(out)
		out = append(out, site{file: e.file, line: e.line, caller: e.caller, via: via, iface: iface, call: e.call})
	}
	for _, e := range x.references(id) {
		consider(e, id, false)
	}
	for _, im := range x.implementers(id) {
		for _, e := range x.references(im) {
			consider(e, im, true)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if x.files[a.file] != x.files[b.file] {
			return x.files[a.file] < x.files[b.file]
		}
		return a.line < b.line
	})
	return out
}

// pred is one step backwards along a reference.
type pred struct {
	caller int32
	iface  bool
}

// predecessors lists the declarations that reference id, directly or through
// an interface method id's receiver type implements, in index order.
func (x *Index) predecessors(id int32) []pred {
	var out []pred
	for _, e := range x.references(id) {
		out = append(out, pred{e.caller, false})
	}
	for _, im := range x.implementers(id) {
		for _, e := range x.references(im) {
			out = append(out, pred{e.caller, true})
		}
	}
	return out
}

// reachingTests finds the TestX functions that reach id within MaxDepth
// references, breadth first in index order, so that each test is reported at
// its smallest depth through the first path found. The resolution is
// "interface" when any step of that path is an interface call.
func (x *Index) reachingTests(id int32, mods modules, changedPaths map[string]bool) ([]model.ImpactTest, int) {
	type node struct {
		id     int32
		depth  int
		iface  bool
		parent int
	}
	nodes := []node{{id: id, parent: -1}}
	seen := map[int32]bool{id: true}
	var tests []model.ImpactTest
	for i := 0; i < len(nodes) && len(nodes) < maxReachVisits; i++ {
		n := nodes[i]
		if n.depth >= MaxDepth || i > 0 && x.decls[n.id].Test {
			continue
		}
		for _, p := range x.predecessors(n.id) {
			if seen[p.caller] || len(nodes) >= maxReachVisits {
				continue
			}
			seen[p.caller] = true
			child := node{id: p.caller, depth: n.depth + 1, iface: n.iface || p.iface, parent: i}
			nodes = append(nodes, child)
			d := x.decls[p.caller]
			if !d.Test {
				continue
			}
			var via []string
			for j := len(nodes) - 1; j >= 0; j = nodes[j].parent {
				via = append(via, x.decls[nodes[j].id].Key)
			}
			resolution := model.ResolutionStatic
			if child.iface {
				resolution = model.ResolutionInterface
			}
			pkg, _ := mods.importPath(dirOf(d.Path))
			tests = append(tests, model.ImpactTest{Name: d.Name, Path: d.Path, Line: d.Line, Package: pkg, Depth: child.depth, Resolution: resolution, Via: via, FileChanged: changedPaths[d.Path]})
		}
	}
	sort.SliceStable(tests, func(i, j int) bool {
		a, b := tests[i], tests[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
	total := len(tests)
	if len(tests) > MaxTestsListed {
		tests = tests[:MaxTestsListed]
	}
	if tests == nil {
		tests = []model.ImpactTest{}
	}
	return tests, total
}

// callerSignal is the low impacted_caller signal of one site.
func callerSignal(x *Index, fn model.ImpactFunction, s site) model.Signal {
	what := "body"
	if fn.Change == model.ChangeSignatureChanged {
		what = "signature"
	}
	caller := x.decls[s.caller].Key
	sig := model.Signal{Kind: model.SignalImpactedCaller, Path: x.files[s.file], Line: int(s.line), Side: "new", Symbol: fn.Symbol, Severity: "low"}
	switch {
	case s.iface:
		sig.Summary = "Unchanged interface call that may dispatch to a changed Go method"
		sig.Evidence = fmt.Sprintf("Static Go index (approximate): %s calls the interface method %s here, and the receiver type of %s implements that interface, so this call may dispatch to it; dispatch is possible, not established. The %s of %s changed at %s:%d. A place to review, not a defect.", caller, x.decls[s.via].Key, fn.Symbol, what, fn.Symbol, fn.Path, fn.Line)
	case s.call:
		sig.Summary = "Unchanged caller of a changed Go function"
		sig.Evidence = fmt.Sprintf("Static Go index (approximate): %s calls %s here. Its %s changed at %s:%d. A place to review, not a defect.", caller, fn.Symbol, what, fn.Path, fn.Line)
	default:
		sig.Summary = "Unchanged reference to a changed Go function"
		sig.Evidence = fmt.Sprintf("Static Go index (approximate): %s refers to %s here without calling it, for example as a function value. Its %s changed at %s:%d. A place to review, not a defect.", caller, fn.Symbol, what, fn.Path, fn.Line)
	}
	sig.Evidence = redact.TruncateUTF8(sig.Evidence, 1000)
	return sig
}

// limitedSignal returns at most one medium analysis_limited signal (symbol
// impact_index) that states why callers of changed functions may be missing:
// signals left out by the caps, and a limited or unavailable index when it
// concerns changed functions.
func (a *analysis) limitedSignal() []model.Signal {
	out := append([]model.Signal(nil), a.signals...)
	var parts []string
	if a.remainder > 0 {
		parts = append(parts, fmt.Sprintf("%d further caller sites of changed Go functions in unchanged code are not listed as signals (at most %d per changed function and %d in total); impact.changed_functions records callers_total for each function.", a.remainder, MaxSignalsPerFunction, MaxSignalsTotal))
	}
	concerned := len(a.changed) > 0 || a.unknown
	switch {
	case a.impact.Status == model.ImpactUnavailable && concerned:
		parts = append(parts, "The static Go index is unavailable ("+a.impact.Reason+"), so callers of changed Go functions were not searched.")
	case len(a.reasons) > 0 && concerned:
		parts = append(parts, "The static Go index is limited ("+joinReasons(a.reasons)+"), so callers of changed Go functions may be missing from it.")
	}
	if len(parts) == 0 {
		return out
	}
	s := model.Signal{Kind: model.SignalAnalysisLimited, Symbol: LimitedSymbol, Severity: "medium", Summary: "Static impact analysis limited", Evidence: redact.TruncateUTF8(strings.Join(parts, " "), 2000)}
	switch {
	case a.firstUnlisted != nil:
		s.Path, s.Line, s.Side = a.firstUnlisted.Path, a.firstUnlisted.Line, "new"
	case a.firstGoPath != "":
		s.Path, s.Line, s.Side = a.firstGoPath, 1, a.firstGoSide
	default:
		return out
	}
	return append(out, s)
}
