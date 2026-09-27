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
	touched       map[string]bool // fnKey of every changed or added Go function
	touchedKeys   map[string]bool // index key of every changed or added lexical function
	multi         bool            // lexical sources changed: texts name no single language
	fileReason    map[string]string
	firstGoPath   string
	firstGoSide   string
	changedPaths  map[string]bool
	signals       []model.Signal
	remainder     int
	firstUnlisted *model.Signal
}

// indexWord and fnWord name the index and the functions in texts: Go-only
// wording unless lexical sources changed.
func (a *analysis) indexWord() string {
	if a.multi {
		return "static index"
	}
	return "static Go index"
}

func (a *analysis) fnWord() string {
	if a.multi {
		return "functions"
	}
	return "Go functions"
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
// the change on it. Go packages are parsed and type-checked; TypeScript and
// JavaScript, Python and Rust sources are scanned lexically (lexindex.go),
// and both parts share one index. It returns an error only when ctx is
// cancelled: every other failure degrades the section to limited or
// unavailable with a reason. Nothing in the repository is executed.
func Analyze(ctx context.Context, repo *gitrepo.Repository, change model.Change, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := &analysis{
		lim:          opts.Limits.withDefaults(),
		change:       change,
		impact:       &model.Impact{Status: model.ImpactIndexed, ChangedFunctions: []model.ImpactFunction{}, Note: model.ImpactNote},
		touched:      map[string]bool{},
		touchedKeys:  map[string]bool{},
		fileReason:   map[string]string{},
		changedPaths: map[string]bool{},
	}
	deadline := time.Now().Add(a.lim.Timeout)
	headPaths, anyGo := changedGoFiles(change, opts.Sensitive)
	anyLex := changedLexicalFiles(change, opts.Sensitive)
	a.multi = anyLex
	if !anyGo && !anyLex {
		a.impact.Status, a.impact.Reason = model.ImpactNotApplicable, NotApplicableReason
		return &Result{report: a.impact}, nil
	}
	for _, f := range change.Files {
		a.changedPaths[f.Path] = true
		if f.OldPath != "" {
			a.changedPaths[f.OldPath] = true
		}
		if a.firstGoPath == "" && (strings.HasSuffix(f.Path, ".go") && indexable(f.Path, opts.Sensitive) || lexIndexable(f.Path, opts.Sensitive)) {
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
			if strings.HasSuffix(p, ".go") && indexable(p, opts.Sensitive) || lexIndexable(p, opts.Sensitive) {
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
	var g *goBuild
	if anyGo {
		var reason string
		g, reason, err = a.buildGo(ctx, repo, headTree, headPaths, opts.Sensitive, deadline)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			if !anyLex {
				return a.unavailable(reason), nil
			}
			// The lexical part is still built: the Go functions are listed
			// as not indexed, and the section is limited.
			a.limit("the static Go index is unavailable: " + reason)
			for _, cf := range a.changed {
				if a.fileReason[cf.path] == "" {
					a.fileReason[cf.path] = "the static Go index is unavailable (" + reason + ")"
				}
			}
			g = nil
		}
	}
	if g == nil {
		g = &goBuild{b: newBuilder(token.NewFileSet(), a.lim, deadline), content: map[string][]byte{}}
	}
	languages := map[string]bool{}
	if g.indexed > 0 {
		languages[LangGo] = true
	}
	indexed := g.indexed
	if anyLex {
		lb, err := a.buildLexical(ctx, repo, headTree, g.b, g.content, deadline, opts.Sensitive)
		if err != nil {
			return nil, err
		}
		indexed += lb.indexed
		for l := range lb.languages {
			languages[l] = true
		}
		sort.SliceStable(a.changed, func(i, j int) bool {
			x, y := a.changed[i], a.changed[j]
			if x.path != y.path {
				return x.path < y.path
			}
			if x.line != y.line {
				return x.line < y.line
			}
			return x.name < y.name
		})
		if len(a.changed) > a.lim.MaxChangedFunctions {
			a.limit(fmt.Sprintf("%d further changed functions are not listed (at most %d)", len(a.changed)-a.lim.MaxChangedFunctions, a.lim.MaxChangedFunctions))
			a.unknown = true
			a.changed = a.changed[:a.lim.MaxChangedFunctions]
		}
		for _, l := range []string{LangGo, LangTypeScript, LangPython, LangRust} {
			if languages[l] {
				a.impact.Languages = append(a.impact.Languages, l)
			}
		}
		a.impact.Note = model.ImpactNoteFor(a.impact.Languages)
	}
	x := g.b.finish(g.content, indexed)
	a.impact.IndexedFiles = indexed
	if err := a.describe(ctx, x, g.mods, deadline); err != nil {
		return nil, err
	}
	a.finishStatus()
	return &Result{report: a.impact, signals: a.limitedSignal(), index: x}, nil
}

// goBuild is the Go part of the index under construction.
type goBuild struct {
	b       *builder
	mods    modules
	content map[string][]byte
	indexed int
}

// buildGo reads, parses and type-checks the Go packages of the candidate
// tree and lists the changed Go functions. It returns the reason why the Go
// index is unavailable instead of a build, and an error only when ctx is
// cancelled.
func (a *analysis) buildGo(ctx context.Context, repo *gitrepo.Repository, headTree []gitrepo.TreeEntry, headPaths []string, sensitive func(string) bool, deadline time.Time) (*goBuild, string, error) {
	opts := Options{Sensitive: sensitive}
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
			return nil, "", ctx.Err()
		}
		a.unknown = true
		return nil, "the candidate Go sources could not be read: " + shortError(err), nil
	}

	if err := a.changedFunctions(ctx, repo, head, byPath, opts.Sensitive); err != nil {
		return nil, "", err
	}
	if tooBig != "" {
		return nil, tooBig, nil
	}
	mods := discoverModules(head)
	usable := false
	for _, m := range mods.list {
		usable = usable || m.path != ""
	}
	if !usable {
		if len(mods.list) == 0 {
			return nil, "no go.mod file in the committed tree", nil
		}
		return nil, "no go.mod file of the committed tree has a readable module path", nil
	}
	if mods.unreadable > 0 {
		a.limit(fmt.Sprintf("%d go.mod files have no readable module path; their packages were not indexed", mods.unreadable))
	}
	if n := len(mods.duplicates); n > 0 {
		// One counted reason, whatever the number of go.mod files.
		var first []string
		for _, dir := range mods.duplicates {
			if len(first) == 3 {
				break
			}
			first = append(first, fmt.Sprintf("%q", redact.TruncateUTF8(dir, 100)))
		}
		a.limit(fmt.Sprintf("%d go.mod files repeat the module path of another go.mod and were not indexed (first: %s)", n, strings.Join(first, ", ")))
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
		return nil, "", err
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
		return nil, fmt.Sprintf("the candidate tree has more than %d Go packages", a.lim.MaxPackages), nil
	}

	// Type checking in dependency order.
	b := newBuilder(fset, a.lim, deadline)
	for _, n := range order(g.nodes) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
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
	return &goBuild{b: b, mods: mods, content: content, indexed: indexed}, "", nil
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
			Reason: "the " + a.indexWord() + " is unavailable; callers and tests were not searched",
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

// maxReasonBytes bounds impact.reason; every reason is a bounded, counted
// text, so the cut only guards against their sum.
const maxReasonBytes = 4096

func joinReasons(reasons []string) string {
	return redact.TruncateUTF8(strings.Join(reasons, "; "), maxReasonBytes)
}

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

// searchDeadlineHook, when set by a test, replaces the deadline the impact
// searches receive, so that a test can reach it after the type check.
var searchDeadlineHook func(deadline time.Time) time.Time

// Per-function reasons of an indexed changed function whose search met a bound.
const (
	reasonCallersStopped = "the caller search stopped at its time or visit limit; callers of this function may be missing"
	reasonTestsStopped   = "the reaching-test search stopped at its time or visit limit; reaching tests of this function may be missing"
	reasonReachCapped    = "the reaching-test search stopped at its declaration limit; reaching tests of this function may be missing"
	reasonImplCapped     = "interface methods beyond the first ones with the same name were not checked; interface calls reaching this function may be missing"
	reasonImplCostly     = "a receiver type met by the search is too large to check against some interfaces (a wide embedding, or many fields or methods); interface calls reaching this function may be missing"
	reasonImplGeneric    = "a generic receiver type met by the search may implement some interfaces only once instantiated, which is not checked; interface calls reaching this function may be missing"
)

// describe fills the changed functions, their callers and reaching tests, and
// the capped impacted_caller signals. The callers of every changed function
// are searched first, then the reaching tests; each of the two passes has its
// own budget of Limits.MaxSearchVisits units, and both stop at the analysis
// deadline. A function whose search met a bound keeps what was found and gets
// a reason, and the section becomes limited. It returns an error only when
// ctx is cancelled.
func (a *analysis) describe(ctx context.Context, x *Index, mods modules, deadline time.Time) error {
	if searchDeadlineHook != nil {
		deadline = searchDeadlineHook(deadline)
	}
	added := addedLines(a.change)
	touchedKeys := map[string]bool{}
	for key := range a.touched {
		parts := strings.SplitN(key, "\x00", 3)
		if ip, ok := mods.importPath(parts[0]); ok && len(parts) == 3 {
			touchedKeys[ip+"."+parts[2]] = true
		}
	}
	for key := range a.touchedKeys {
		touchedKeys[key] = true
	}
	fns := make([]model.ImpactFunction, len(a.changed))
	ids := make([]int32, len(a.changed))
	var byPos map[declPos]int32
	for i, cf := range a.changed {
		fn := model.ImpactFunction{Path: cf.path, Line: cf.line, EndLine: cf.end, Symbol: cf.pkgName + "." + cf.name, Change: cf.change, Callers: []model.ImpactCaller{}, Tests: []model.ImpactTest{}}
		ids[i] = -1
		importPath, inModule := mods.importPath(cf.dir)
		id, found := int32(-1), false
		if cf.key != "" {
			// A lexical function: its key is known.
			inModule = true
			fn.Symbol = cf.key
			id, found = x.byKey[cf.key]
		} else if inModule {
			fn.Symbol = importPath + "." + cf.name
			id, found = x.byKey[fn.Symbol]
			if (!found || x.decls[id].Path != cf.path) && a.fileReason[cf.path] == "" {
				// The syntactic name can differ from the index key, for
				// example for a method declared on an alias receiver (type C
				// = Cart), which the index keys by the aliased type. The
				// declaration is then found at its position.
				if byPos == nil {
					byPos = x.declsByPosition()
				}
				if alt, ok := byPos[declPos{path: cf.path, line: cf.line, name: lastName(cf.name)}]; ok {
					id, found = alt, true
					fn.Symbol = x.decls[alt].Key
					// References inside the function are part of the change
					// under its index key too.
					touchedKeys[fn.Symbol] = true
				}
			}
		}
		switch {
		case a.fileReason[cf.path] != "":
			fn.Reason = a.fileReason[cf.path]
		case !inModule:
			fn.Reason = "the file is outside every Go module with a readable module path"
		case (!found || x.decls[id].Path != cf.path) && cf.key != "":
			fn.Reason = "the declaration is not in the lexical index"
		case !found || x.decls[id].Path != cf.path:
			fn.Reason = "the declaration is not in the static Go index"
		default:
			fn.Indexed = true
			ids[i] = id
			x.changed[id] = cf.change
		}
		fns[i] = fn
	}

	reasons := make([][]string, len(fns))
	callersCut, testsCut, reachCapped := 0, 0, 0
	implCapped, implCostly, implGeneric := 0, 0, 0
	// implReasons adds the reasons of the interface methods an implementers
	// lookup left unchecked during the search of function i, once each.
	implReasons := func(i int, b *budget) {
		for _, gap := range []struct {
			on     bool
			reason string
			count  *int
		}{
			{b.implCapped, reasonImplCapped, &implCapped},
			{b.implCostly, reasonImplCostly, &implCostly},
			{b.implGeneric, reasonImplGeneric, &implGeneric},
		} {
			if gap.on && !contains(reasons[i], gap.reason) {
				*gap.count++
				reasons[i] = append(reasons[i], gap.reason)
			}
		}
	}
	signalled := 0
	callerBudget := newBudget(ctx, deadline, a.lim.MaxSearchVisits)
	for i := range fns {
		if ids[i] < 0 {
			continue
		}
		fn := &fns[i]
		callerBudget.resetImpl()
		sites, ok := x.sites(ids[i], touchedKeys, added, callerBudget)
		implReasons(i, callerBudget)
		if !ok {
			callersCut++
			reasons[i] = append(reasons[i], reasonCallersStopped)
		}
		fn.CallersTotal = len(sites)
		for j, s := range sites {
			if j < MaxCallersListed {
				resolution := model.ResolutionStatic
				switch {
				case x.decls[ids[i]].lexical():
					resolution = model.ResolutionName
				case s.iface:
					resolution = model.ResolutionInterface
				}
				fn.Callers = append(fn.Callers, model.ImpactCaller{Path: x.files[s.file], Line: int(s.line), Symbol: x.decls[s.caller].Key, Depth: 1, Resolution: resolution})
			}
			if j < MaxSignalsPerFunction && signalled < MaxSignalsTotal {
				a.signals = append(a.signals, callerSignal(x, *fn, s))
				signalled++
				continue
			}
			a.remainder++
			if a.firstUnlisted == nil {
				a.firstUnlisted = &model.Signal{Path: x.files[s.file], Line: int(s.line), Side: "new"}
			}
		}
	}
	testBudget := newBudget(ctx, deadline, a.lim.MaxSearchVisits)
	for i := range fns {
		if ids[i] < 0 {
			continue
		}
		fn := &fns[i]
		testBudget.resetImpl()
		tests, total, ok, capped := x.reachingTests(ids[i], mods, a.changedPaths, testBudget)
		fn.Tests, fn.TestsTotal = tests, total
		implReasons(i, testBudget)
		if capped {
			reachCapped++
			reasons[i] = append(reasons[i], reasonReachCapped)
		}
		if !ok {
			testsCut++
			reasons[i] = append(reasons[i], reasonTestsStopped)
		}
	}
	if callerBudget.cancelled || testBudget.cancelled {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	for i := range fns {
		if len(reasons[i]) > 0 {
			fns[i].Reason = strings.Join(reasons[i], "; ")
		}
	}
	a.impact.ChangedFunctions = append(a.impact.ChangedFunctions, fns...)
	stoppedBy := func(b *budget) string {
		if b.timedOut {
			return fmt.Sprintf("the index time limit (%s)", a.lim.Timeout)
		}
		return fmt.Sprintf("its limit of %d visits", a.lim.MaxSearchVisits)
	}
	if callersCut > 0 {
		a.limit(fmt.Sprintf("the caller search stopped at %s; callers of %d changed functions may be missing", stoppedBy(callerBudget), callersCut))
	}
	if testsCut > 0 {
		a.limit(fmt.Sprintf("the reaching-test search stopped at %s; reaching tests of %d changed functions may be missing", stoppedBy(testBudget), testsCut))
	}
	if reachCapped > 0 {
		a.limit(fmt.Sprintf("the reaching-test search of %d changed functions stopped after %d declarations; more reaching tests may exist", reachCapped, maxReachVisits))
	}
	if implCapped > 0 {
		a.limit(fmt.Sprintf("more than %d interface methods share a method name the search met; the others were not checked, so interface calls reaching %d changed functions may be missing", maxImplementCandidates, implCapped))
	}
	if implCostly > 0 {
		a.limit(fmt.Sprintf("receiver types whose check against an interface would exceed %d estimated type-checker steps (a wide embedding, or many fields or methods) were not checked, so interface calls reaching %d changed functions may be missing", maxImplementSteps, implCostly))
	}
	if implGeneric > 0 {
		a.limit(fmt.Sprintf("interfaces that a generic receiver type may implement only once instantiated were not checked, so interface calls reaching %d changed functions may be missing", implGeneric))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// declPos locates a function or method declaration: its file, its line and
// its final name (the method name for a method).
type declPos struct {
	path string
	line int
	name string
}

// declsByPosition maps the position of every indexed function and method to
// its declaration.
func (x *Index) declsByPosition() map[declPos]int32 {
	out := map[declPos]int32{}
	for i, d := range x.decls {
		if d.Kind != KindFunc && d.Kind != KindMethod {
			continue
		}
		k := declPos{path: d.Path, line: d.Line, name: lastName(d.Name)}
		if _, dup := out[k]; !dup {
			out[k] = int32(i)
		}
	}
	return out
}

// lastName is the final identifier of "F" or "T.M".
func lastName(name string) string { return name[strings.LastIndex(name, ".")+1:] }

// sites lists the references to a changed function in unchanged, non-test
// code: static references, and calls of interface methods its receiver type
// implements. A reference inside a changed or added function, on an added
// line, or in a test file is part of the change or of the tests and is not
// listed. One site is kept per line, preferring a static reference. Every
// reference visited costs one unit of bud; ok is false when bud stopped
// before the search ended, and the sites found so far are returned.
func (x *Index) sites(id int32, touched map[string]bool, added map[string]map[int]bool, bud *budget) ([]site, bool) {
	var out []site
	byLine := map[[2]int32]int{}
	consider := func(e edge, via int32, iface bool) {
		file := x.files[e.file]
		if testFile(file) || x.decls[e.caller].testCode || touched[x.decls[e.caller].Key] || added[file][int(e.line)] {
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
	finish := func(ok bool) ([]site, bool) {
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if x.files[a.file] != x.files[b.file] {
				return x.files[a.file] < x.files[b.file]
			}
			return a.line < b.line
		})
		return out, ok
	}
	if !bud.ok() {
		return finish(false)
	}
	for _, e := range x.references(id) {
		if !bud.spend(1) {
			return finish(false)
		}
		consider(e, id, false)
	}
	for _, im := range x.implementers(id, bud) {
		for _, e := range x.references(im) {
			if !bud.spend(1) {
				return finish(false)
			}
			consider(e, im, true)
		}
	}
	// implementers may have stopped the budget before any reference.
	return finish(!bud.stopped)
}

// reachingTests finds the TestX functions that reach id within MaxDepth
// references, breadth first in index order, so that each test is reported at
// its smallest depth through the first path found. The resolution is
// "interface" when any step of that path is an interface call.
//
// The references of an interface method are followed once per search: every
// caller they lead to is seen from the first declaration that expands them,
// so expanding them again from another implementing declaration finds
// nothing new. Every reference visited costs one unit of bud. It returns the
// listed tests, the number found, ok false when bud stopped before the search
// ended, and capped true when the search stopped at maxReachVisits
// declarations; the tests found so far are returned in both cases.
func (x *Index) reachingTests(id int32, mods modules, changedPaths map[string]bool, bud *budget) (_ []model.ImpactTest, total int, ok, capped bool) {
	type node struct {
		id     int32
		depth  int
		iface  bool
		parent int
	}
	nodes := []node{{id: id, parent: -1}}
	seen := map[int32]bool{id: true}
	expanded := map[int32]bool{}
	var tests []model.ImpactTest
	ok = bud.ok()
	// visit follows one reference from the declaration of nodes[i] back to
	// caller; it returns false when the search must stop.
	visit := func(i int, caller int32, iface bool) bool {
		if !bud.spend(1) {
			ok = false
			return false
		}
		if seen[caller] {
			return true
		}
		if len(nodes) >= maxReachVisits {
			capped = true
			return false
		}
		seen[caller] = true
		child := node{id: caller, depth: nodes[i].depth + 1, iface: nodes[i].iface || iface, parent: i}
		nodes = append(nodes, child)
		d := x.decls[caller]
		if !d.Test {
			return true
		}
		var via []string
		for j := len(nodes) - 1; j >= 0; j = nodes[j].parent {
			via = append(via, x.decls[nodes[j].id].Key)
		}
		resolution := model.ResolutionStatic
		switch {
		case d.lexical():
			resolution = model.ResolutionName
		case child.iface:
			resolution = model.ResolutionInterface
		}
		pkg, _ := mods.importPath(dirOf(d.Path))
		if d.lexical() {
			pkg = dirOf(d.Path)
		}
		tests = append(tests, model.ImpactTest{Name: d.Name, Path: d.Path, Line: d.Line, Package: pkg, Depth: child.depth, Resolution: resolution, Via: via, FileChanged: changedPaths[d.Path]})
		return true
	}
search:
	for i := 0; ok && i < len(nodes); i++ {
		n := nodes[i]
		if n.depth >= MaxDepth || i > 0 && x.decls[n.id].Test {
			continue
		}
		for _, e := range x.references(n.id) {
			if !visit(i, e.caller, false) {
				break search
			}
		}
		for _, im := range x.implementers(n.id, bud) {
			if expanded[im] {
				continue
			}
			expanded[im] = true
			for _, e := range x.references(im) {
				if !visit(i, e.caller, true) {
					break search
				}
			}
		}
		if bud.stopped {
			ok = false
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
	total = len(tests)
	if len(tests) > MaxTestsListed {
		tests = tests[:MaxTestsListed]
	}
	if tests == nil {
		tests = []model.ImpactTest{}
	}
	return tests, total, ok, capped
}

// callerSignal is the low impacted_caller signal of one site.
func callerSignal(x *Index, fn model.ImpactFunction, s site) model.Signal {
	what := "body"
	if fn.Change == model.ChangeSignatureChanged {
		what = "signature"
	}
	caller := x.decls[s.caller].Key
	sig := model.Signal{Kind: model.SignalImpactedCaller, Path: x.files[s.file], Line: int(s.line), Side: "new", Symbol: fn.Symbol, Severity: "low"}
	switch lang := x.decls[s.via].Language; {
	case lang != "":
		sig.Summary = "Unchanged caller of a changed " + langName(lang) + " function"
		sig.Evidence = fmt.Sprintf("Lexical index (approximate, linked by name only): %s calls a function or method named %s here, which may be %s. Its %s changed at %s:%d. A place to review, not a defect.", caller, lastName(x.decls[s.via].Name), fn.Symbol, what, fn.Path, fn.Line)
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
		parts = append(parts, fmt.Sprintf("%d further caller sites of changed "+a.fnWord()+" in unchanged code are not listed as signals (at most %d per changed function and %d in total); impact.changed_functions records callers_total for each function.", a.remainder, MaxSignalsPerFunction, MaxSignalsTotal))
	}
	concerned := len(a.changed) > 0 || a.unknown
	switch {
	case a.impact.Status == model.ImpactUnavailable && concerned:
		parts = append(parts, "The "+a.indexWord()+" is unavailable ("+a.impact.Reason+"), so callers of changed "+a.fnWord()+" were not searched.")
	case len(a.reasons) > 0 && concerned:
		parts = append(parts, "The "+a.indexWord()+" is limited ("+joinReasons(a.reasons)+"), so callers of changed "+a.fnWord()+" may be missing from it.")
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
