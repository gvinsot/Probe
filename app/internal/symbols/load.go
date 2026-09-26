package symbols

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
)

// indexable reports whether a tree path may be read by the index: a portable
// .go file or go.mod outside the directories the go command ignores (testdata,
// vendor, and names starting with "_" or "."), and not sensitive.
func indexable(p string, sensitive func(string) bool) bool {
	if gitrepo.SafePath(p) != nil {
		return false
	}
	if !strings.HasSuffix(p, ".go") && path.Base(p) != "go.mod" {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "testdata" || segment == "vendor" || strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return sensitive == nil || !sensitive(p)
}

// regularBlob reports whether a tree entry is a regular file. Symlinks and
// submodules are never followed.
func regularBlob(e gitrepo.TreeEntry) bool {
	return e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755")
}

// selectEntries lists the indexable regular files of a tree, sorted by path.
func selectEntries(tree []gitrepo.TreeEntry, sensitive func(string) bool) []gitrepo.TreeEntry {
	var out []gitrepo.TreeEntry
	for _, e := range tree {
		if regularBlob(e) && indexable(e.Path, sensitive) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readEntries reads the blobs of entries through one cat-file stream. Each
// entry must be at most limit bytes (callers filter on Size first).
func readEntries(ctx context.Context, repo *gitrepo.Repository, entries []gitrepo.TreeEntry, limit int64) (map[string][]byte, error) {
	out := make(map[string][]byte, len(entries))
	if len(entries) == 0 {
		return out, nil
	}
	oids := make([]string, len(entries))
	for i, e := range entries {
		oids[i] = e.OID
	}
	next := 0
	err := repo.ReadBlobs(ctx, oids, limit, func(oid string, data []byte) error {
		if next >= len(entries) || entries[next].OID != oid {
			return errors.New("Git blob stream does not match the tree listing")
		}
		out[entries[next].Path] = append([]byte(nil), data...)
		next++
		return nil
	})
	return out, err
}

// module is one go.mod: its repository-relative directory ("" for the root)
// and its module path ("" when it has none that can be read).
type module struct{ dir, path string }

// modulePattern matches the module directive; it is the pattern
// coverage.ModulePath uses, repeated here because that function reads disk.
var modulePattern = regexp.MustCompile(`(?m)^module[ \t]+"?([^\s"]+)"?[ \t]*(//.*)?$`)

// modulePath returns the module path of a go.mod, or "" unless it has exactly
// one plausible module directive.
func modulePath(data []byte) string {
	matches := modulePattern.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		return ""
	}
	p := string(matches[0][1])
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":\x00\\") {
		return ""
	}
	return p
}

// modules is the set of go.mod files of the indexed tree, deepest first, so
// that the first containing module is the innermost one.
type modules struct {
	list       []module
	unreadable int      // go.mod files without a readable module path
	duplicates []string // module directories skipped because another declares the same path
}

func discoverModules(files map[string][]byte) modules {
	var m modules
	byPath := map[string]string{}
	var dirs []string
	for p := range files {
		if path.Base(p) == "go.mod" {
			dirs = append(dirs, p)
		}
	}
	sort.Strings(dirs)
	for _, p := range dirs {
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		mp := modulePath(files[p])
		if mp == "" {
			m.unreadable++
		} else if first, ok := byPath[mp]; ok && first != dir {
			// Two modules with one path cannot both be imported; the lexically
			// first directory keeps it.
			m.duplicates = append(m.duplicates, dir)
			mp = ""
		} else {
			byPath[mp] = dir
		}
		m.list = append(m.list, module{dir: dir, path: mp})
	}
	sort.SliceStable(m.list, func(i, j int) bool { return depth(m.list[i].dir) > depth(m.list[j].dir) })
	return m
}

func depth(dir string) int {
	if dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

// importPath returns the import path of a repository directory in its
// innermost module, and false when the directory is outside every module or
// its module path cannot be used.
func (m modules) importPath(dir string) (string, bool) {
	if dir == "." {
		dir = ""
	}
	for _, mod := range m.list {
		switch {
		case mod.dir == dir:
			return mod.path, mod.path != ""
		case mod.dir == "":
			if mod.path == "" {
				return "", false
			}
			return mod.path + "/" + dir, true
		case strings.HasPrefix(dir, mod.dir+"/"):
			if mod.path == "" {
				return "", false
			}
			return mod.path + "/" + dir[len(mod.dir)+1:], true
		}
	}
	return "", false
}

// buildContext selects files as the go command would for linux/amd64 with cgo
// enabled. It reads file headers from memory only: no disk, no GOROOT, no
// process. ToolTags stay empty so the selection does not depend on the host.
func buildContext(files map[string][]byte) build.Context {
	return build.Context{
		GOOS:        "linux",
		GOARCH:      "amd64",
		Compiler:    "gc",
		CgoEnabled:  true,
		ReleaseTags: append([]string(nil), build.Default.ReleaseTags...),
		JoinPath:    path.Join,
		OpenFile: func(p string) (io.ReadCloser, error) {
			data, ok := files[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

// parsed is one successfully parsed file.
type parsed struct {
	path string
	file *ast.File
}

// parseFiles parses paths with at most eight workers. Results keep the order
// of paths; a file that fails to parse has a nil entry.
func parseFiles(ctx context.Context, fset *token.FileSet, paths []string, files map[string][]byte) []*ast.File {
	out := make([]*ast.File, len(paths))
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				f, err := parser.ParseFile(fset, paths[i], files[paths[i]], parser.SkipObjectResolution)
				if err == nil {
					out[i] = f
				}
			}
		}()
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// pkgNode is one package to type-check: the package of a directory with its
// in-package test files, or the external test package of a directory.
type pkgNode struct {
	path     string // types package path: the import path, plus "_test" for an external test package
	external bool   // an external test package (package x_test)
	dir      string
	name     string
	files    []*ast.File
	paths    []string
	imports  []string // import paths of the file set, sorted
	checked  bool
	failed   bool
	timedOut bool
}

// grouping is the package layout of the indexed files.
type grouping struct {
	nodes        []*pkgNode // sorted by path
	clauseSkips  int        // files whose package clause does not fit their directory
	outsideFiles int        // .go files outside every usable module
	indexedPaths map[string]*pkgNode
}

// groupPackages groups parsed files by directory and package clause. The
// non-test package name of a directory is the most common one among its
// non-test files (ties: the smallest name); in-package test files must use it,
// external test files must use it with "_test".
func groupPackages(files []parsed, mods modules) grouping {
	g := grouping{indexedPaths: map[string]*pkgNode{}}
	byDir := map[string][]parsed{}
	var dirs []string
	for _, f := range files {
		dir := path.Dir(f.path)
		if _, ok := byDir[dir]; !ok {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], f)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		group := byDir[dir]
		importPath, ok := mods.importPath(dir)
		if !ok {
			g.outsideFiles += len(group)
			continue
		}
		name := packageName(group)
		var main, external *pkgNode
		for _, f := range group {
			clause := f.file.Name.Name
			isTest := strings.HasSuffix(f.path, "_test.go")
			var n **pkgNode
			var nodePath, nodeName string
			switch {
			case clause == name:
				n, nodePath, nodeName = &main, importPath, name
			case isTest && clause == name+"_test":
				n, nodePath, nodeName = &external, importPath+"_test", clause
			default:
				g.clauseSkips++
				continue
			}
			if *n == nil {
				*n = &pkgNode{path: nodePath, dir: dir, name: nodeName, external: n == &external}
			}
			(*n).files = append((*n).files, f.file)
			(*n).paths = append((*n).paths, f.path)
		}
		for _, n := range []*pkgNode{main, external} {
			if n == nil {
				continue
			}
			n.imports = fileImports(n.files)
			g.nodes = append(g.nodes, n)
			for _, p := range n.paths {
				g.indexedPaths[p] = n
			}
		}
	}
	sort.Slice(g.nodes, func(i, j int) bool { return g.nodes[i].path < g.nodes[j].path })
	return g
}

// packageName picks the package clause of a directory's non-test files.
func packageName(group []parsed) string {
	counts := map[string]int{}
	for _, f := range group {
		if !strings.HasSuffix(f.path, "_test.go") {
			counts[f.file.Name.Name]++
		}
	}
	if len(counts) == 0 {
		// A directory with test files only: an in-package test package is named
		// without the suffix.
		for _, f := range group {
			counts[strings.TrimSuffix(f.file.Name.Name, "_test")]++
		}
	}
	best, bestCount := "", -1
	for name, count := range counts {
		if count > bestCount || count == bestCount && name < best {
			best, bestCount = name, count
		}
	}
	return best
}

func fileImports(files []*ast.File) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
