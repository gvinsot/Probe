package symbols

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// Declaration kinds.
const (
	KindFunc            = "func"
	KindMethod          = "method"
	KindInterfaceMethod = "interface_method"
	// KindPackageInit stands for a package's init functions, blank functions
	// and package-level variable initializers, which have no callable name.
	KindPackageInit = "package_init"
)

// Decl is one indexed declaration.
type Decl struct {
	Key       string // import path "." name, e.g. "example.test/shop/cart.Cart.Total"
	Package   string // types package path; external test packages end in "_test"
	PkgName   string
	Name      string // "F", "T.M", or "init" for KindPackageInit
	Path      string // repository-relative file
	Line      int
	EndLine   int
	Kind      string
	Test      bool // a TestX(t *testing.T) function in a _test.go file, or a lexical test
	Signature string
	// Language is "" for a Go declaration, and the lexical language
	// (LangTypeScript, LangPython or LangRust) otherwise.
	Language string
	// testCode marks lexical test code that is not in a test file: a test,
	// or a helper inside a Rust #[cfg(test)] module.
	testCode bool
}

// lexical reports a declaration of the lexical index.
func (d Decl) lexical() bool { return d.Language != "" }

// edge is one reference to a declaration: a call, or a use as a value.
type edge struct {
	caller, callee int32
	file           int32
	line, col      int32
	call           bool
}

// nameSite is a method call whose receiver type could not be resolved
// (typically a value from a package outside the repository). It is recorded by
// name only and never feeds signals or test selection.
type nameSite struct {
	caller int32
	name   string
	file   int32
	line   int32
}

// Index is the static symbol index of one commit. It is read-only once built,
// except for a lazily filled cache guarded by mu; Query may be called
// concurrently.
type Index struct {
	decls        []Decl
	byKey        map[string]int32
	byName       map[string][]int32 // Name, final identifier and PkgName.Name -> ids
	files        []string
	content      map[string][]byte // indexed file contents, for snippets
	edges        []edge            // sorted by callee, file, line, col, caller
	calleeStart  []int32           // edges[calleeStart[id]:calleeStart[id+1]] reference id
	callerOrder  []int32           // edge indexes sorted by caller, file, line, col
	callerStart  []int32
	nameSites    []nameSite // sorted by name, file, line
	recv         map[int32]*types.Named
	ifaces       map[int32]*types.Interface
	ifaceByName  map[string][]int32
	changed      map[int32]string // declaration -> change class, set by Analyze
	mods         modules          // Go modules of the indexed tree, for test packages
	indexedFiles int

	mu    sync.Mutex
	impl  map[int32]implEntry
	steps map[*types.Named]int64 // lookupSteps of receiver types, guarded by mu

	snipMu        sync.Mutex
	redacted      map[int32][]string // whole-file redaction of indexed files, split in lines
	redactedBytes int
}

// implEntry is the cached result of implementers: the interface methods, and
// which candidates were left unchecked (see the budget flags of the same
// names).
type implEntry struct {
	ids     []int32
	capped  bool
	costly  bool
	generic bool
}

// builder is the state of one index construction.
type builder struct {
	fset     *token.FileSet
	limits   Limits
	deadline time.Time
	checked  map[string]*types.Package
	fakes    map[string]*types.Package

	decls    []Decl
	declID   map[string]int32
	files    []string
	fileID   map[string]int32
	edges    []edge
	names    []nameSite
	recv     map[int32]*types.Named
	ifaces   map[int32]*types.Interface
	edgeCap  bool
	nameCap  bool
	failed   int // packages whose type check panicked
	timedOut int // packages not checked because the time limit was reached
}

// newBuilder starts an index construction whose type checks stop at deadline
// on their next type error (Analyze also checks it between packages).
func newBuilder(fset *token.FileSet, limits Limits, deadline time.Time) *builder {
	return &builder{fset: fset, limits: limits, deadline: deadline, checked: map[string]*types.Package{}, fakes: map[string]*types.Package{}, declID: map[string]int32{}, fileID: map[string]int32{}, recv: map[int32]*types.Named{}, ifaces: map[int32]*types.Interface{}}
}

// errImported is returned with every imported package. go/types then uses the
// returned package as it is but marks it "fake", which makes it skip the
// error report, and the case-insensitive lookup over the imported scope that
// builds the error message, for every name the importer uses and the package
// does not declare. Without it, code referencing many missing names of a
// large package (a skipped file, or code written to exhaust the index) costs
// one sort of that package's names per reference.
var errImported = errors.New("imported by the static index without export data")

// errDeadline stops a type check from its error callback once the index time
// limit is reached.
var errDeadline = errors.New("index time limit reached")

// Import returns a checked repository package, and for every other path
// (imports from outside the repository, and repository packages not checked
// yet because of an import cycle) an empty package named after the path. Both
// come with errImported (see there); only "unsafe" is exact.
func (b *builder) Import(p string) (*types.Package, error) {
	if p == "unsafe" {
		return types.Unsafe, nil
	}
	if pkg, ok := b.checked[p]; ok {
		return pkg, errImported
	}
	pkg, ok := b.fakes[p]
	if !ok {
		pkg = types.NewPackage(p, guessName(p))
		pkg.MarkComplete()
		b.fakes[p] = pkg
	}
	return pkg, errImported
}

// guessName is the conventional package name of an import path: its last
// element without a "go-" prefix or a major-version suffix.
func guessName(p string) string {
	parts := strings.Split(p, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "go-")
	name = strings.Map(func(r rune) rune {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, name)
	if name == "" {
		return "_"
	}
	return name
}

// order returns the nodes in dependency order: repository imports first.
// Cycles are broken by visit state; the package reached again is faked.
func order(nodes []*pkgNode) []*pkgNode {
	byPath := map[string]*pkgNode{}
	for _, n := range nodes {
		byPath[n.path] = n
	}
	state := map[*pkgNode]int{}
	var out []*pkgNode
	var visit func(n *pkgNode)
	visit = func(n *pkgNode) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		for _, imp := range n.imports {
			if dep, ok := byPath[imp]; ok && dep != n {
				visit(dep)
			}
		}
		state[n] = 2
		out = append(out, n)
	}
	for _, n := range nodes {
		visit(n)
	}
	return out
}

// check type-checks one package and extracts its declarations and edges. A
// panic inside go/types marks the package failed; it is then faked for its
// importers.
func (b *builder) check(n *pkgNode) {
	info := &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{
		Importer:    b,
		FakeImportC: true,
		Sizes:       types.SizesFor("gc", "amd64"),
		// Imports from outside the repository are faked, so type errors are
		// expected; checking continues past them. go/types cannot be
		// interrupted otherwise, so the time limit is also checked on each
		// error: a package that produces many errors (each can cost a lookup
		// over an imported scope) stops there.
		Error: func(error) {
			if time.Now().After(b.deadline) {
				panic(errDeadline)
			}
		},
	}
	var pkg *types.Package
	stopped := false
	ok := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				ok, stopped = false, r == errDeadline
			}
		}()
		pkg, _ = conf.Check(n.path, b.fset, n.files, info)
		return true
	}()
	if stopped {
		n.timedOut = true
		b.timedOut++
		n.files = nil
		return
	}
	if !ok || pkg == nil {
		n.failed = true
		b.failed++
		n.files = nil
		return
	}
	if !n.external {
		// An external test package cannot be imported.
		b.checked[n.path] = pkg
	}
	ok = func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		b.extract(n, pkg, info)
		return true
	}()
	if !ok {
		n.failed = true
		b.failed++
	}
	n.checked = true
	// The syntax trees and type information are not needed once edges are
	// extracted; dropping them keeps memory proportional to one package.
	n.files = nil
}

// funcKey is the index key of a function or method object: its package path,
// its receiver's type name for a method, and its name. Generic functions and
// methods of instantiated types map to their origin. It returns "" for an
// object without a package or an abstract method of an unnamed interface.
func funcKey(fn *types.Func) string {
	fn = fn.Origin()
	pkg := fn.Pkg()
	if pkg == nil {
		return ""
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return ""
	}
	if recv := sig.Recv(); recv != nil {
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := types.Unalias(t).(*types.Named)
		if !ok {
			return ""
		}
		return pkg.Path() + "." + named.Origin().Obj().Name() + "." + fn.Name()
	}
	return pkg.Path() + "." + fn.Name()
}

// position returns the line and column of pos in the file as parsed, without
// applying //line directives: the candidate writes those, and they could
// otherwise move a recorded site to any file name and line (for example into
// a _test.go name, which would hide a caller). The file itself is always the
// repository path the index parsed, never a directive's name.
func (b *builder) position(pos token.Pos) (line, col int) {
	p := b.fset.PositionFor(pos, false)
	return p.Line, p.Column
}

// line is the unadjusted line of pos (see position).
func (b *builder) line(pos token.Pos) int {
	l, _ := b.position(pos)
	return l
}

func (b *builder) file(p string) int32 {
	if id, ok := b.fileID[p]; ok {
		return id
	}
	id := int32(len(b.files))
	b.files = append(b.files, p)
	b.fileID[p] = id
	return id
}

// declare records a declaration, keeping the first one of a key.
func (b *builder) declare(d Decl) int32 {
	if id, ok := b.declID[d.Key]; ok {
		return id
	}
	id := int32(len(b.decls))
	if len(d.Signature) > 300 {
		d.Signature = redact.TruncateUTF8(d.Signature, 300)
	}
	b.decls = append(b.decls, d)
	b.declID[d.Key] = id
	return id
}

// extract records the package's declarations first, so that references to a
// function declared later in the same package resolve, then its edges.
func (b *builder) extract(n *pkgNode, pkg *types.Package, info *types.Info) {
	qualifier := func(p *types.Package) string { return p.Name() }
	initKey := pkg.Path() + ".init"
	type body struct {
		caller int32
		node   ast.Node
		path   string // the repository path of the file holding node
	}
	var bodies []body
	for i, f := range n.files {
		filePath := n.paths[i]
		testFile := strings.HasSuffix(filePath, "_test.go")
		initDecl := func(pos token.Pos) int32 {
			return b.declare(Decl{Key: initKey, Package: pkg.Path(), PkgName: pkg.Name(), Name: "init", Path: filePath, Line: b.line(pos), EndLine: b.line(pos), Kind: KindPackageInit})
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				fn, _ := info.Defs[decl.Name].(*types.Func)
				key := ""
				if fn != nil && decl.Name.Name != "_" && !(decl.Recv == nil && decl.Name.Name == "init") {
					key = funcKey(fn)
				}
				var id int32
				if key == "" {
					id = initDecl(decl.Pos())
				} else {
					name, kind := fn.Name(), KindFunc
					if decl.Recv != nil {
						kind = KindMethod
						name = strings.TrimPrefix(key, pkg.Path()+".")
					}
					id = b.declare(Decl{
						Key: key, Package: pkg.Path(), PkgName: pkg.Name(), Name: name, Path: filePath,
						Line: b.line(decl.Pos()), EndLine: b.line(decl.End()),
						Kind: kind, Test: testFile && isTestFunc(decl, f), Signature: types.ObjectString(fn, qualifier),
					})
					if kind == KindMethod {
						if named := receiverNamed(fn); named != nil {
							b.recv[id] = named
						}
					}
				}
				if decl.Body != nil {
					bodies = append(bodies, body{id, decl.Body, filePath})
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						b.declareInterface(s, pkg, info, filePath)
					case *ast.ValueSpec:
						if decl.Tok == token.VAR && len(s.Values) > 0 {
							id := initDecl(s.Pos())
							for _, v := range s.Values {
								bodies = append(bodies, body{id, v, filePath})
							}
						}
					}
				}
			}
		}
	}
	for _, bd := range bodies {
		if b.edgeCap && b.nameCap {
			break
		}
		b.edgesOf(bd.caller, bd.node, bd.path, info)
	}
}

// receiverNamed returns the origin named type of a concrete method's receiver.
func receiverNamed(fn *types.Func) *types.Named {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return nil
	}
	return named.Origin()
}

// declareInterface records the explicit methods of a named, non-generic
// interface type, for interface dispatch.
func (b *builder) declareInterface(s *ast.TypeSpec, pkg *types.Package, info *types.Info, filePath string) {
	obj, ok := info.Defs[s.Name].(*types.TypeName)
	if !ok || obj.IsAlias() {
		return
	}
	named, ok := obj.Type().(*types.Named)
	if !ok || named.TypeParams().Len() > 0 {
		return
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return
	}
	for i := 0; i < iface.NumExplicitMethods(); i++ {
		m := iface.ExplicitMethod(i)
		key := funcKey(m)
		if key == "" {
			continue
		}
		line := b.line(m.Pos())
		id := b.declare(Decl{Key: key, Package: pkg.Path(), PkgName: pkg.Name(), Name: s.Name.Name + "." + m.Name(), Path: filePath, Line: line, EndLine: line, Kind: KindInterfaceMethod, Signature: types.ObjectString(m, func(p *types.Package) string { return p.Name() })})
		b.ifaces[id] = iface
	}
}

// isTestFunc recognizes TestX(t *testing.T) syntactically: the testing package
// is not loaded, so the parameter type is matched against the file's import
// name for "testing".
func isTestFunc(fd *ast.FuncDecl, f *ast.File) bool {
	if fd.Recv != nil || !isGoTestName(fd.Name.Name) || fd.Type.TypeParams != nil || fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		return false
	}
	params := fd.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	for _, imp := range f.Imports {
		if imp.Path.Value != `"testing"` {
			continue
		}
		name := "testing"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == pkg.Name && name != "_" && name != "." {
			return true
		}
	}
	return false
}

func isGoTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	tail := strings.TrimPrefix(name, "Test")
	if tail == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(tail)
	return !unicode.IsLower(r)
}

// edgesOf records every reference to a function or method inside node. An
// identifier in call position (the function of a call expression, after
// parentheses and explicit instantiation) is a call; any other reference, such
// as a function value, is recorded with call=false. A method call on a value
// whose type could not be resolved is recorded as a name site. Every site is
// recorded in filePath, the file node belongs to, at its unadjusted line.
func (b *builder) edgesOf(caller int32, node ast.Node, filePath string, info *types.Info) {
	fileID := b.file(filePath)
	calls := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := ast.Unparen(call.Fun)
		for {
			switch f := fun.(type) {
			case *ast.IndexExpr:
				fun = ast.Unparen(f.X)
				continue
			case *ast.IndexListExpr:
				fun = ast.Unparen(f.X)
				continue
			}
			break
		}
		switch f := fun.(type) {
		case *ast.Ident:
			calls[f] = true
		case *ast.SelectorExpr:
			calls[f.Sel] = true
		}
		return true
	})
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if !calls[x.Sel] || b.nameCap {
				return true
			}
			if _, resolved := info.Uses[x.Sel]; resolved {
				return true
			}
			if id, ok := ast.Unparen(x.X).(*ast.Ident); ok {
				if _, qualified := info.Uses[id].(*types.PkgName); qualified {
					return true // a function of a package outside the repository
				}
			}
			if len(b.names) >= b.limits.MaxNameSites {
				b.nameCap = true
				return true
			}
			line := b.line(x.Sel.Pos())
			b.names = append(b.names, nameSite{caller: caller, name: x.Sel.Name, file: fileID, line: int32(line)})
		case *ast.Ident:
			fn, ok := info.Uses[x].(*types.Func)
			if !ok || b.edgeCap {
				return true
			}
			callee, ok := b.declID[funcKey(fn)]
			if !ok {
				return true
			}
			if len(b.edges) >= b.limits.MaxEdges {
				b.edgeCap = true
				return true
			}
			line, col := b.position(x.Pos())
			b.edges = append(b.edges, edge{caller: caller, callee: callee, file: fileID, line: int32(line), col: int32(col), call: calls[x]})
		}
		return true
	})
}

// finish sorts declarations by key and edges by callee and location, and
// builds the lookup tables. Every ordering is total, so an index built twice
// from the same commit is identical.
func (b *builder) finish(content map[string][]byte, indexedFiles int) *Index {
	perm := make([]int32, len(b.decls))
	for i := range perm {
		perm[i] = int32(i)
	}
	sort.Slice(perm, func(i, j int) bool { return b.decls[perm[i]].Key < b.decls[perm[j]].Key })
	remap := make([]int32, len(b.decls))
	x := &Index{byKey: map[string]int32{}, byName: map[string][]int32{}, content: content, recv: map[int32]*types.Named{}, ifaces: map[int32]*types.Interface{}, ifaceByName: map[string][]int32{}, changed: map[int32]string{}, impl: map[int32]implEntry{}, steps: map[*types.Named]int64{}, redacted: map[int32][]string{}, indexedFiles: indexedFiles}
	for newID, old := range perm {
		remap[old] = int32(newID)
		x.decls = append(x.decls, b.decls[old])
	}
	// Files are renumbered in path order so that edge order does not depend
	// on the order in which packages were checked.
	fileOrder := make([]int32, len(b.files))
	for i := range fileOrder {
		fileOrder[i] = int32(i)
	}
	sort.Slice(fileOrder, func(i, j int) bool { return b.files[fileOrder[i]] < b.files[fileOrder[j]] })
	fileRemap := make([]int32, len(b.files))
	for newID, old := range fileOrder {
		fileRemap[old] = int32(newID)
		x.files = append(x.files, b.files[old])
	}
	for i, d := range x.decls {
		id := int32(i)
		x.byKey[d.Key] = id
		names := []string{d.Name, d.PkgName + "." + d.Name}
		if last := d.Name[strings.LastIndex(d.Name, ".")+1:]; last != d.Name {
			names = append(names, last)
		}
		for _, n := range names {
			x.byName[n] = append(x.byName[n], id)
		}
	}
	for old, named := range b.recv {
		x.recv[remap[old]] = named
	}
	for old, iface := range b.ifaces {
		id := remap[old]
		x.ifaces[id] = iface
		name := x.decls[id].Name[strings.LastIndex(x.decls[id].Name, ".")+1:]
		x.ifaceByName[name] = append(x.ifaceByName[name], id)
	}
	for _, ids := range x.ifaceByName {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	x.edges = make([]edge, len(b.edges))
	for i, e := range b.edges {
		x.edges[i] = edge{caller: remap[e.caller], callee: remap[e.callee], file: fileRemap[e.file], line: e.line, col: e.col, call: e.call}
	}
	sort.Slice(x.edges, func(i, j int) bool {
		a, c := x.edges[i], x.edges[j]
		if a.callee != c.callee {
			return a.callee < c.callee
		}
		if a.file != c.file {
			return a.file < c.file
		}
		if a.line != c.line {
			return a.line < c.line
		}
		if a.col != c.col {
			return a.col < c.col
		}
		return a.caller < c.caller
	})
	x.calleeStart = make([]int32, len(x.decls)+1)
	for _, e := range x.edges {
		x.calleeStart[e.callee+1]++
	}
	for i := 1; i < len(x.calleeStart); i++ {
		x.calleeStart[i] += x.calleeStart[i-1]
	}
	x.callerOrder = make([]int32, len(x.edges))
	for i := range x.callerOrder {
		x.callerOrder[i] = int32(i)
	}
	sort.Slice(x.callerOrder, func(i, j int) bool {
		a, c := x.edges[x.callerOrder[i]], x.edges[x.callerOrder[j]]
		if a.caller != c.caller {
			return a.caller < c.caller
		}
		if a.file != c.file {
			return a.file < c.file
		}
		if a.line != c.line {
			return a.line < c.line
		}
		if a.col != c.col {
			return a.col < c.col
		}
		return a.callee < c.callee
	})
	x.callerStart = make([]int32, len(x.decls)+1)
	for _, e := range x.edges {
		x.callerStart[e.caller+1]++
	}
	for i := 1; i < len(x.callerStart); i++ {
		x.callerStart[i] += x.callerStart[i-1]
	}
	for _, n := range b.names {
		x.nameSites = append(x.nameSites, nameSite{caller: remap[n.caller], name: n.name, file: fileRemap[n.file], line: n.line})
	}
	sort.Slice(x.nameSites, func(i, j int) bool {
		a, c := x.nameSites[i], x.nameSites[j]
		if a.name != c.name {
			return a.name < c.name
		}
		if a.file != c.file {
			return a.file < c.file
		}
		if a.line != c.line {
			return a.line < c.line
		}
		return a.caller < c.caller
	})
	return x
}

// references returns the edges that reference id, in (file, line, col,
// caller) order.
func (x *Index) references(id int32) []edge {
	if x == nil || id < 0 || int(id) >= len(x.decls) {
		return nil
	}
	return x.edges[x.calleeStart[id]:x.calleeStart[id+1]]
}

// callees returns the edge indexes whose caller is id.
func (x *Index) callees(id int32) []int32 {
	if x == nil || id < 0 || int(id) >= len(x.decls) {
		return nil
	}
	return x.callerOrder[x.callerStart[id]:x.callerStart[id+1]]
}

// implementers returns the interface methods, sorted, whose named interface
// the receiver type of the concrete method id (T or *T) implements. A call to
// one of them may dispatch to id; the dispatch itself is not resolved.
//
// For a method of a generic type the check is made on the generic type
// itself. It finds the interfaces that every instantiation implements: those
// whose methods match methods of the type with signatures that do not use its
// type parameters. An interface that some instantiation may implement through
// a signature that uses them is not listed, and bud.implGeneric is set.
//
// Only the first maxImplementCandidates interface methods of the same name are
// checked; when more exist, bud.implCapped is set. A single go/types check can
// cost far more than a unit (see lookupSteps), so a check whose estimated
// work exceeds maxImplementSteps is not made and sets bud.implCostly; the
// deadline and the context are checked before every other check, and each is
// charged to bud in proportion to its estimated work. When bud stops during
// the checks, the partial result is returned and not cached; the caller sees
// bud.stopped.
func (x *Index) implementers(id int32, bud *budget) []int32 {
	if x == nil {
		return nil
	}
	named, ok := x.recv[id]
	if !ok {
		return nil
	}
	// The lock also serializes the go/types calls below.
	x.mu.Lock()
	defer x.mu.Unlock()
	if entry, ok := x.impl[id]; ok {
		entry.flag(bud)
		return entry.ids
	}
	candidates := x.ifaceByName[lastName(x.decls[id].Name)]
	entry := implEntry{capped: len(candidates) > maxImplementCandidates}
	if entry.capped {
		candidates = candidates[:maxImplementCandidates]
	}
	generic := named.TypeParams().Len() > 0
	interrupted := false
	func() {
		// go/types on faked imports is exercised far from its usual inputs; a
		// panic here only loses interface edges.
		defer func() { _ = recover() }()
		steps := x.lookupStepsLocked(named)
		ptr := types.NewPointer(named)
		for _, im := range candidates {
			iface := x.ifaces[im]
			if iface == nil || iface.Empty() {
				continue
			}
			work := implementWork(iface.NumMethods(), steps, generic)
			if work > maxImplementSteps {
				entry.costly = true
				continue
			}
			if !bud.ok() || !bud.spend(1+iface.NumMethods()+int(work>>10)) {
				interrupted = true
				return
			}
			switch {
			case types.Implements(named, iface) || types.Implements(ptr, iface):
				entry.ids = append(entry.ids, im)
			case generic && mayImplementOnceInstantiated(ptr, iface):
				entry.generic = true
			}
		}
	}()
	entry.flag(bud)
	if !interrupted {
		x.impl[id] = entry
	}
	return entry.ids
}

// flag records on bud which candidates the lookup left unchecked.
func (e implEntry) flag(bud *budget) {
	bud.implCapped = bud.implCapped || e.capped
	bud.implCostly = bud.implCostly || e.costly
	bud.implGeneric = bud.implGeneric || e.generic
}

// maxImplementSteps bounds the estimated go/types work (implementWork) of one
// implementation check of a receiver type against one interface. It is a
// variable only so that tests can lower it. Measured in golang:1.26-bookworm
// with 2 CPUs while other builds ran (BenchmarkImplementsWideEmbedding), a
// step cost 5.5 to 11 ns, so the largest check made takes up to about 100 ms.
var maxImplementSteps int64 = 1 << 23

// implementWork estimates the go/types work of checking a receiver type whose
// lookups cost steps each (lookupSteps) against an interface of n methods:
// types.Implements looks up each method, and once more for a missing one, on
// T and on *T; for a generic type, mayImplementOnceInstantiated looks up each
// method once more.
func implementWork(n int, steps int64, generic bool) int64 {
	lookups := 2 * int64(n+2)
	if generic {
		lookups += int64(n)
	}
	return lookups * steps
}

// lookupStepsLocked returns the cached lookupSteps of a receiver type; x.mu is
// held. The smallest check makes six lookups, so an estimate above a sixth of
// maxImplementSteps is not refined.
func (x *Index) lookupStepsLocked(named *types.Named) int64 {
	if s, ok := x.steps[named]; ok {
		return s
	}
	s := lookupSteps(named, maxImplementSteps/6)
	x.steps[named] = s
	return s
}

// lookupSteps estimates an upper bound, in elementary steps, of the work of
// one go/types lookup of a field or method name that named (or *named) does
// not have. go/types walks the embedded types breadth first. It scans the
// methods and fields of each type it meets, compares an instance of a generic
// type with the instances of the same type met before, and before each level
// de-duplicates the level's embedded types by comparing each with every
// distinct type kept so far, which is quadratic in the width of the level. A
// struct that embeds a few thousand types therefore costs hundreds of
// milliseconds per lookup. The walk here is linear in the types and fields it
// meets: named types are de-duplicated by pointer, which can only
// overestimate. It stops once the estimate exceeds limit and returns limit+1.
func lookupSteps(named *types.Named, limit int64) int64 {
	var steps int64
	seen := map[*types.Named]bool{}
	instances := map[*types.Named]int64{} // instances met per generic origin
	current := []types.Type{named}
	for len(current) > 0 {
		var next []types.Type
		for _, t := range current {
			steps++
			if n, ok := types.Unalias(t).(*types.Named); ok {
				if seen[n] {
					continue
				}
				seen[n] = true
				steps += int64(n.NumMethods())
				if n.TypeArgs().Len() > 0 {
					o := n.Origin()
					steps += instances[o]
					instances[o]++
				}
			}
			switch u := t.Underlying().(type) {
			case *types.Struct:
				steps += int64(u.NumFields())
				for i := 0; i < u.NumFields(); i++ {
					if f := u.Field(i); f.Embedded() {
						ft := types.Unalias(f.Type())
						if p, ok := ft.(*types.Pointer); ok {
							ft = p.Elem()
						}
						next = append(next, ft)
					}
				}
			case *types.Interface:
				steps += int64(u.NumMethods())
			}
			if steps > limit {
				return limit + 1
			}
		}
		w := int64(len(next))
		steps += w * w
		if steps > limit {
			return limit + 1
		}
		current = next
	}
	return steps
}

// mayImplementOnceInstantiated reports whether some instantiation of a
// generic receiver type may implement iface although the generic type itself
// does not: every method of iface is a method of the type (ptr is its pointer
// type) by name, since method names do not depend on the type arguments, and
// at least one of those methods has a signature that uses type parameters, so
// it may match after substitution.
func mayImplementOnceInstantiated(ptr types.Type, iface *types.Interface) bool {
	usesParams := false
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		obj, _, _ := types.LookupFieldOrMethod(ptr, false, m.Pkg(), m.Name())
		fn, ok := obj.(*types.Func)
		if !ok {
			return false
		}
		if !usesParams {
			if sig, ok := fn.Type().(*types.Signature); ok {
				usesParams = usesTypeParams(sig.Params()) || usesTypeParams(sig.Results())
			}
		}
	}
	return usesParams
}

// usesTypeParams reports whether t mentions a type parameter. A named type is
// not expanded, only its type arguments. A type too large or too deep to walk
// within a small bound counts as mentioning one, which can only report the
// generic-receiver gap more often.
func usesTypeParams(t types.Type) bool {
	left := 10000
	var walk func(t types.Type, depth int) bool
	walk = func(t types.Type, depth int) bool {
		left--
		if left < 0 || depth > 64 {
			return true
		}
		switch t := types.Unalias(t).(type) {
		case *types.TypeParam:
			return true
		case *types.Named:
			args := t.TypeArgs()
			for i := 0; i < args.Len(); i++ {
				if walk(args.At(i), depth+1) {
					return true
				}
			}
		case *types.Pointer:
			return walk(t.Elem(), depth+1)
		case *types.Slice:
			return walk(t.Elem(), depth+1)
		case *types.Array:
			return walk(t.Elem(), depth+1)
		case *types.Map:
			return walk(t.Key(), depth+1) || walk(t.Elem(), depth+1)
		case *types.Chan:
			return walk(t.Elem(), depth+1)
		case *types.Signature:
			return walk(t.Params(), depth+1) || walk(t.Results(), depth+1)
		case *types.Tuple:
			for i := 0; i < t.Len(); i++ {
				if walk(t.At(i).Type(), depth+1) {
					return true
				}
			}
		case *types.Struct:
			for i := 0; i < t.NumFields(); i++ {
				if walk(t.Field(i).Type(), depth+1) {
					return true
				}
			}
		case *types.Interface:
			for i := 0; i < t.NumExplicitMethods(); i++ {
				if walk(t.ExplicitMethod(i).Type(), depth+1) {
					return true
				}
			}
			for i := 0; i < t.NumEmbeddeds(); i++ {
				if walk(t.EmbeddedType(i), depth+1) {
					return true
				}
			}
		case *types.Union:
			for i := 0; i < t.Len(); i++ {
				if walk(t.Term(i).Type(), depth+1) {
					return true
				}
			}
		}
		return false
	}
	return walk(t, 0)
}

// Files returns the number of Go files the index type-checked.
func (x *Index) Files() int {
	if x == nil {
		return 0
	}
	return x.indexedFiles
}

// Lookup returns the declaration of key.
func (x *Index) Lookup(key string) (Decl, bool) {
	if x == nil {
		return Decl{}, false
	}
	id, ok := x.byKey[key]
	if !ok {
		return Decl{}, false
	}
	return x.decls[id], true
}

// maxRedactedBytes bounds the cache of redacted files kept for snippets; the
// cache is dropped when it would grow past it.
const maxRedactedBytes = 32 << 20

// snippet returns one line of an indexed file, cut to 240 bytes, or "" when
// the file is not held. The line is taken from the redaction of the whole
// file, as read_file and search show it: a credential whose key and value sit
// on different lines is masked there, and would not be if the line were
// redacted alone. Redaction keeps every newline, so line numbers still match.
// Computing a file's redaction is charged to bud (one unit per KiB); a
// stopped budget gives "".
func (x *Index) snippet(file int32, line int32, bud *budget) string {
	if int(file) >= len(x.files) || line < 1 {
		return ""
	}
	lines := x.redactedLines(file, bud)
	if int(line) > len(lines) {
		return ""
	}
	text := strings.TrimRight(lines[line-1], "\r")
	text = strings.ToValidUTF8(strings.TrimSpace(text), "�")
	return redact.TruncateUTF8(text, 240)
}

// redactedLines returns the lines of the whole-file redaction of an indexed
// file, computing and caching it on first use.
func (x *Index) redactedLines(file int32, bud *budget) []string {
	x.snipMu.Lock()
	defer x.snipMu.Unlock()
	if lines, ok := x.redacted[file]; ok {
		return lines
	}
	data := x.content[x.files[file]]
	if data == nil || !bud.spend(1+len(data)/1024) {
		return nil
	}
	text := redact.Redact(string(data))
	if strings.Count(text, "\n") != bytes.Count(data, []byte("\n")) {
		// Redact keeps newlines; if that ever failed, lines could not be
		// matched, so no snippet is given for the file.
		text = ""
	}
	lines := strings.Split(text, "\n")
	if x.redactedBytes+len(text) > maxRedactedBytes {
		x.redacted, x.redactedBytes = map[int32][]string{}, 0
	}
	x.redacted[file] = lines
	x.redactedBytes += len(text)
	return lines
}

// display is the short form of a key used in texts: the package name instead
// of the import path.
func (x *Index) display(id int32) string {
	d := x.decls[id]
	return d.PkgName + "." + d.Name
}

// location formats a declaration's position.
func (d Decl) location() string { return fmt.Sprintf("%s:%d", d.Path, d.Line) }

// dirOf is path.Dir with "" for the root.
func dirOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}
