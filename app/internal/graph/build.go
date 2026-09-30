package graph

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// Limits bound the construction of a graph.
type Limits struct {
	MaxFiles int   // source files and manifests read
	MaxBytes int64 // total bytes read
	MaxNodes int
	MaxEdges int
	// ImplementsTimeout bounds the interface-implementation search.
	ImplementsTimeout time.Duration
}

// DefaultLimits keeps a graph build within the budget of the impact analysis.
func DefaultLimits() Limits {
	return Limits{MaxFiles: 20000, MaxBytes: 64 << 20, MaxNodes: 300_000, MaxEdges: 2_000_000, ImplementsTimeout: 20 * time.Second}
}

// Options configure Build.
type Options struct {
	// Index is the symbol index of the same commit. Without it the graph is
	// structural: components, packages, files, types, imports and
	// dependencies, but no functions nor calls.
	Index *symbols.Index
	// Sensitive paths are neither read nor listed.
	Sensitive func(string) bool
	Limits    Limits
}

// Tree lists the files of a commit and reads blobs; *gitrepo.Repository
// implements it.
type Tree interface {
	Tree(ctx context.Context, commit string) ([]gitrepo.TreeEntry, error)
	ReadBlobs(ctx context.Context, oids []string, limit int64, fn func(oid string, data []byte) error) error
}

// builder accumulates the graph.
type builder struct {
	g       *Graph
	limits  Limits
	nodes   map[string]int // id -> index in g.Nodes
	edges   map[[3]string]int
	dropped map[string]int // edge kind -> edges left out by MaxEdges
	full    bool           // MaxNodes reached
}

func (b *builder) node(n Node) bool {
	if _, ok := b.nodes[n.ID]; ok {
		return true
	}
	if len(b.g.Nodes) >= b.limits.MaxNodes {
		b.full = true
		return false
	}
	b.nodes[n.ID] = len(b.g.Nodes)
	b.g.Nodes = append(b.g.Nodes, n)
	return true
}

func (b *builder) has(id string) bool { _, ok := b.nodes[id]; return ok }

// edge adds an edge between existing nodes, or counts one more site of an
// existing edge.
func (b *builder) edge(e Edge) {
	if e.From == e.To || !b.has(e.From) || !b.has(e.To) {
		return
	}
	key := [3]string{e.From, e.Kind, e.To}
	if i, ok := b.edges[key]; ok {
		if b.g.Edges[i].Count == 0 {
			b.g.Edges[i].Count = 1
		}
		n := e.Count
		if n == 0 {
			n = 1
		}
		b.g.Edges[i].Count += n
		return
	}
	if len(b.g.Edges) >= b.limits.MaxEdges {
		b.dropped[e.Kind]++
		return
	}
	b.edges[key] = len(b.g.Edges)
	b.g.Edges = append(b.g.Edges, e)
}

func (b *builder) limit(format string, args ...any) {
	b.g.Complete = false
	b.g.Limitations = append(b.g.Limitations, fmt.Sprintf(format, args...))
}

// source is one file of the commit the graph reads.
type source struct {
	path, oid, language string
	manifest            bool
}

// Build builds the graph of a resolved commit from its Git objects.
func Build(ctx context.Context, repo Tree, commit string, opts Options) (*Graph, error) {
	limits := opts.Limits
	if limits.MaxFiles == 0 {
		limits = DefaultLimits()
	}
	entries, err := repo.Tree(ctx, commit)
	if err != nil {
		return nil, err
	}
	b := &builder{
		g:      &Graph{Schema: Schema, Commit: commit, Complete: true, Calls: opts.Index != nil, Nodes: []Node{}, Edges: []Edge{}},
		limits: limits, nodes: map[string]int{}, edges: map[[3]string]int{}, dropped: map[string]int{},
	}

	// The files the graph reads: indexed source languages and manifests.
	var files []source
	var bytes int64
	skipped := 0
	for _, e := range entries {
		if e.Type != "blob" || e.Mode == "120000" || (opts.Sensitive != nil && opts.Sensitive(e.Path)) {
			continue
		}
		lang := symbols.LanguageOf(e.Path)
		manifest := isManifest(e.Path) || isRequirements(e.Path)
		if lang == "" && !manifest {
			continue
		}
		if e.Size > gitrepo.MaxFileBytes || len(files) >= limits.MaxFiles || bytes+e.Size > limits.MaxBytes {
			skipped++
			continue
		}
		bytes += e.Size
		files = append(files, source{path: e.Path, oid: e.OID, language: lang, manifest: manifest})
	}
	if skipped > 0 {
		b.limit("%d files were not read: larger than %d bytes, or beyond %d files or %d bytes in total", skipped, gitrepo.MaxFileBytes, limits.MaxFiles, limits.MaxBytes)
	}

	// Read every blob once, and keep only the extracted facts.
	byOID := map[string][]int{}
	var oids []string
	for i, f := range files {
		if _, ok := byOID[f.oid]; !ok {
			oids = append(oids, f.oid)
		}
		byOID[f.oid] = append(byOID[f.oid], i)
	}
	facts := make([]fileFacts, len(files))
	manifests := map[string]manifestInfo{} // path -> info
	err = repo.ReadBlobs(ctx, oids, gitrepo.MaxFileBytes, func(oid string, data []byte) error {
		for _, i := range byOID[oid] {
			f := files[i]
			if f.manifest {
				info := manifestInfo{}
				info.ecosystem, info.deps = manifestDeps(f.path, data)
				if path.Base(f.path) == "go.mod" {
					info.module = goModule(data)
				}
				manifests[f.path] = info
			}
			if f.language != "" {
				facts[i] = extract(f.language, f.path, data)
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}

	r := newResolver(files, manifests)
	b.addStructure(files, facts, r)
	if opts.Index != nil {
		b.addFunctions(ctx, opts.Index, r, limits)
	}
	b.addImports(files, facts, r)
	b.addDeclared(r)

	if b.full {
		b.limit("the graph was capped at %d nodes", limits.MaxNodes)
	}
	for kind, n := range b.dropped {
		b.limit("%d %s edges left out beyond %d edges", n, kind, limits.MaxEdges)
	}
	sort.Strings(b.g.Limitations)
	sort.Slice(b.g.Nodes, func(i, j int) bool { return b.g.Nodes[i].ID < b.g.Nodes[j].ID })
	sort.Slice(b.g.Edges, func(i, j int) bool {
		x, y := b.g.Edges[i], b.g.Edges[j]
		if x.From != y.From {
			return x.From < y.From
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.To < y.To
	})
	return b.g, nil
}

func isRequirements(p string) bool {
	base := path.Base(p)
	return strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")
}

// addStructure adds the components, packages, files and types.
func (b *builder) addStructure(files []source, facts []fileFacts, r *resolver) {
	for _, dir := range r.componentDirs() {
		b.node(Node{ID: componentID(dir), Kind: KindComponent, Name: displayDir(dir), Path: dir, Detail: strings.Join(r.manifestsOf[dir], ", ")})
	}
	for i, f := range files {
		if f.language == "" {
			continue
		}
		dir := path.Dir(f.path)
		comp := componentID(r.componentOf(f.path))
		pkg := packageID(dir)
		if !b.has(pkg) {
			b.node(Node{ID: pkg, Kind: KindPackage, Name: r.packageName(dir, f.language), Path: dir, Language: f.language, Component: comp})
			b.edge(Edge{From: comp, To: pkg, Kind: EdgeContains})
		}
		fid := fileID(f.path)
		b.node(Node{ID: fid, Kind: KindFile, Name: f.path, Path: f.path, Language: f.language, Component: comp, Test: isTestFile(f.path, f.language)})
		b.edge(Edge{From: pkg, To: fid, Kind: EdgeContains})
		for _, t := range facts[i].Types {
			tid := typeID(r.typeKey(f.path, f.language, t.Name))
			b.node(Node{ID: tid, Kind: KindType, Name: t.Name, Path: f.path, Line: t.Line, EndLine: t.EndLine, Language: f.language, Component: comp, Detail: t.Kind, Exported: t.Exported})
			b.edge(Edge{From: fid, To: tid, Kind: EdgeContains})
		}
	}
}

// addFunctions adds the functions of the symbol index, their calls, their
// types (member_of) and the interfaces Go types implement.
func (b *builder) addFunctions(ctx context.Context, x *symbols.Index, r *resolver, limits Limits) {
	decls := x.Decls()
	// A package initializer or a lexical module body has no callable name:
	// its calls are attributed to its file.
	nodeOf := map[string]string{}
	for _, d := range decls {
		fid := fileID(d.Path)
		if !b.has(fid) {
			continue
		}
		if d.Kind == symbols.KindPackageInit {
			nodeOf[d.Key] = fid
			continue
		}
		id := functionID(d.Key)
		lang := d.Language
		if lang == "" {
			lang = "go"
		}
		if !b.node(Node{ID: id, Kind: KindFunction, Name: displayFunction(d), Path: d.Path, Line: d.Line, EndLine: d.EndLine, Language: lang,
			Component: componentID(r.componentOf(d.Path)), Detail: d.Signature, Test: d.Test, Exported: exportedFunction(d)}) {
			continue
		}
		nodeOf[d.Key] = id
		b.edge(Edge{From: fid, To: id, Kind: EdgeContains})
		if owner, _, ok := strings.Cut(d.Name, "."); ok && (d.Kind == symbols.KindMethod || d.Kind == symbols.KindInterfaceMethod || d.Language != "") {
			// "T.M" is a method of T (Go), "C.m" a member of class C.
			var tkey string
			if d.Language == "" {
				tkey = typeID(strings.TrimSuffix(d.Package, "_test") + "." + owner)
			} else {
				tkey = typeID(d.Path + "." + owner)
			}
			b.edge(Edge{From: id, To: tkey, Kind: EdgeMemberOf})
		}
	}
	for _, c := range x.Calls() {
		from, to := nodeOf[c.Caller], nodeOf[c.Callee]
		if from == "" || to == "" {
			continue
		}
		b.edge(Edge{From: from, To: to, Kind: EdgeCalls, Path: c.Path, Line: c.Line, Count: countOrZero(c.Count)})
	}
	impls, complete := x.Implementations(ctx, limits.ImplementsTimeout)
	if !complete {
		b.limit("interface implementations are partial: the search was bounded")
	}
	byKey := map[string]symbols.Decl{}
	for _, d := range decls {
		byKey[d.Key] = d
	}
	for _, im := range impls {
		m, i := byKey[im.Method], byKey[im.Interface]
		mt, _, ok1 := strings.Cut(m.Name, ".")
		it, _, ok2 := strings.Cut(i.Name, ".")
		if !ok1 || !ok2 {
			continue
		}
		b.edge(Edge{From: typeID(strings.TrimSuffix(m.Package, "_test") + "." + mt), To: typeID(strings.TrimSuffix(i.Package, "_test") + "." + it), Kind: EdgeImplements})
	}
}

func countOrZero(n int) int {
	if n <= 1 {
		return 0
	}
	return n
}

// addImports resolves the imports of every file, and aggregates them into
// package and component dependencies.
func (b *builder) addImports(files []source, facts []fileFacts, r *resolver) {
	for i, f := range files {
		if f.language == "" {
			continue
		}
		fid := fileID(f.path)
		fromPkg := packageID(path.Dir(f.path))
		fromComp := componentID(r.componentOf(f.path))
		for _, imp := range facts[i].Imports {
			target, kind := r.resolve(f, imp.Spec)
			switch kind {
			case "":
				continue
			case KindDependency:
				eco, name, _ := strings.Cut(target, ":")
				dep := dependencyID(eco, name)
				b.node(Node{ID: dep, Kind: KindDependency, Name: target, Detail: eco})
				b.edge(Edge{From: fid, To: dep, Kind: EdgeImports, Path: f.path, Line: imp.Line})
				b.edge(Edge{From: fromComp, To: dep, Kind: EdgeDependsOn})
			case KindFile, KindPackage:
				to := fileID(target)
				dir := path.Dir(target)
				if kind == KindPackage {
					to, dir = packageID(target), target
				}
				b.edge(Edge{From: fid, To: to, Kind: EdgeImports, Path: f.path, Line: imp.Line})
				if toPkg := packageID(dir); toPkg != fromPkg {
					b.edge(Edge{From: fromPkg, To: toPkg, Kind: EdgeDependsOn})
				}
				if toComp := componentID(r.componentOf(dir + "/x")); toComp != fromComp {
					b.edge(Edge{From: fromComp, To: toComp, Kind: EdgeDependsOn})
				}
			}
		}
	}
}

// addDeclared adds the dependencies components declare in their manifests.
func (b *builder) addDeclared(r *resolver) {
	for _, m := range r.manifestPaths {
		info := r.manifests[m]
		comp := componentID(r.componentOf(m))
		for _, d := range info.deps {
			dep := dependencyID(info.ecosystem, d)
			b.node(Node{ID: dep, Kind: KindDependency, Name: info.ecosystem + ":" + d, Detail: info.ecosystem})
			b.edge(Edge{From: comp, To: dep, Kind: EdgeDeclares, Path: m})
		}
	}
}

func displayDir(dir string) string {
	if dir == "." {
		return "(repository root)"
	}
	return dir
}

func displayFunction(d symbols.Decl) string {
	if d.Language == "" && d.PkgName != "" {
		return d.PkgName + "." + d.Name
	}
	return d.Name
}

func exportedFunction(d symbols.Decl) bool {
	name := d.Name
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if d.Language == "" {
		for _, c := range name {
			return unicode.IsUpper(c)
		}
		return false
	}
	return !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "#")
}

func isTestFile(p, language string) bool {
	base := path.Base(p)
	inTests := strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/") || strings.Contains(p, "/__tests__/") || strings.HasPrefix(p, "__tests__/")
	switch language {
	case "go":
		return strings.HasSuffix(base, "_test.go")
	case "typescript":
		return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || inTests
	case "python":
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py" || inTests
	case "rust":
		return inTests
	}
	return false
}
