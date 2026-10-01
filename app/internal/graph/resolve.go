package graph

import (
	"path"
	"sort"
	"strings"
)

// manifestInfo is what a manifest contributes.
type manifestInfo struct {
	ecosystem string
	deps      []string
	module    string // Go module path of a go.mod
}

// resolver maps files to components and packages, and import specs to the
// files, packages or dependencies they name.
type resolver struct {
	files         map[string]bool // every source file read
	dirs          map[string]bool // directories holding source files
	manifests     map[string]manifestInfo
	manifestPaths []string            // sorted
	manifestsOf   map[string][]string // component dir -> manifest file names
	roots         []string            // non-root component roots from manifests, deepest first
	goModules     []goModuleInfo      // longest module dir first
	declared      map[string]map[string]string
	pyModules     map[string]string // dotted module -> file (or package dir + "/__init__.py")
}

type goModuleInfo struct{ dir, module string }

func newResolver(files []source, manifests map[string]manifestInfo) *resolver {
	r := &resolver{files: map[string]bool{}, dirs: map[string]bool{}, manifests: manifests,
		manifestsOf: map[string][]string{}, declared: map[string]map[string]string{}, pyModules: map[string]string{}}
	for _, f := range files {
		if f.language != "" {
			r.files[f.path] = true
			r.dirs[path.Dir(f.path)] = true
		}
	}
	for p, info := range manifests {
		r.manifestPaths = append(r.manifestPaths, p)
		dir := path.Dir(p)
		if isManifest(p) {
			r.manifestsOf[dir] = append(r.manifestsOf[dir], path.Base(p))
			if dir != "." {
				r.roots = append(r.roots, dir)
			}
		}
		if info.module != "" {
			r.goModules = append(r.goModules, goModuleInfo{dir, info.module})
		}
		if r.declared[info.ecosystem] == nil {
			r.declared[info.ecosystem] = map[string]string{}
		}
		for _, d := range info.deps {
			r.declared[info.ecosystem][normalizeDep(info.ecosystem, d)] = d
		}
	}
	sort.Strings(r.manifestPaths)
	for dir := range r.manifestsOf {
		sort.Strings(r.manifestsOf[dir])
	}
	r.roots = uniqueSorted(r.roots)
	sort.Slice(r.roots, func(i, j int) bool { return len(r.roots[i]) > len(r.roots[j]) })
	sort.Slice(r.goModules, func(i, j int) bool { return len(r.goModules[i].dir) > len(r.goModules[j].dir) })

	// Python modules, named from the repository root, from each component
	// root and from their src/ directories.
	bases := append([]string{"."}, r.roots...)
	var withSrc []string
	for _, b := range bases {
		withSrc = append(withSrc, b, path.Join(b, "src"))
	}
	for f := range r.files {
		if !strings.HasSuffix(f, ".py") {
			continue
		}
		for _, base := range withSrc {
			rel := f
			if base != "." {
				if !strings.HasPrefix(f, base+"/") {
					continue
				}
				rel = strings.TrimPrefix(f, base+"/")
			}
			mod := strings.TrimSuffix(rel, ".py")
			mod = strings.TrimSuffix(mod, "/__init__")
			mod = strings.ReplaceAll(mod, "/", ".")
			if prev, ok := r.pyModules[mod]; !ok || len(f) < len(prev) {
				r.pyModules[mod] = f
			}
		}
	}
	return r
}

// componentOf returns the component root of a file: the deepest directory
// holding a manifest (other than the repository root), else the top-level
// directory, else "." for a file at the root.
func (r *resolver) componentOf(file string) string {
	for _, root := range r.roots {
		if strings.HasPrefix(file, root+"/") {
			return root
		}
	}
	if i := strings.Index(file, "/"); i > 0 {
		return file[:i]
	}
	return "."
}

// componentDirs returns every component root that holds a file or a
// manifest, sorted.
func (r *resolver) componentDirs() []string {
	set := map[string]bool{}
	for f := range r.files {
		set[r.componentOf(f)] = true
	}
	for _, m := range r.manifestPaths {
		set[r.componentOf(m)] = true
	}
	var out []string
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// goModuleOf returns the Go module holding a directory.
func (r *resolver) goModuleOf(dir string) (goModuleInfo, bool) {
	for _, m := range r.goModules {
		if m.dir == "." || dir == m.dir || strings.HasPrefix(dir, m.dir+"/") {
			return m, true
		}
	}
	return goModuleInfo{}, false
}

// goImportPath returns the import path of a Go package directory.
func (r *resolver) goImportPath(dir string) string {
	m, ok := r.goModuleOf(dir)
	if !ok {
		return dir
	}
	rel := dir
	if m.dir != "." {
		rel = strings.TrimPrefix(strings.TrimPrefix(dir, m.dir), "/")
	}
	if rel == "" || rel == "." {
		return m.module
	}
	return m.module + "/" + rel
}

func (r *resolver) packageName(dir, language string) string {
	if language == "go" {
		return r.goImportPath(dir)
	}
	return displayDir(dir)
}

// typeKey names a type as the symbol index names its methods' owner: the Go
// import path, or the file path for the lexical languages.
func (r *resolver) typeKey(file, language, name string) string {
	if language == "go" {
		return r.goImportPath(path.Dir(file)) + "." + name
	}
	return file + "." + name
}

var nodeBuiltins = map[string]bool{
	"assert": true, "buffer": true, "child_process": true, "cluster": true, "crypto": true, "dgram": true, "dns": true,
	"events": true, "fs": true, "http": true, "http2": true, "https": true, "net": true, "os": true, "path": true,
	"perf_hooks": true, "process": true, "querystring": true, "readline": true, "stream": true, "string_decoder": true,
	"timers": true, "tls": true, "tty": true, "url": true, "util": true, "v8": true, "vm": true, "worker_threads": true, "zlib": true,
}

var tsExtensions = []string{"", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".d.ts",
	"/index.ts", "/index.tsx", "/index.js", "/index.jsx", "/index.mjs"}

// resolve names the target of one import: a file or package of the
// repository (by path), or a dependency ("npm:react"), with the kind of the
// target; kind "" when the import is left out (standard library, unresolved).
func (r *resolver) resolve(f source, spec string) (target, kind string) {
	dir := path.Dir(f.path)
	switch f.language {
	case "go":
		for _, m := range r.goModules {
			if spec == m.module || strings.HasPrefix(spec, m.module+"/") {
				rel := strings.TrimPrefix(strings.TrimPrefix(spec, m.module), "/")
				target := path.Join(m.dir, rel)
				if r.dirs[target] {
					return target, KindPackage
				}
				return "", ""
			}
		}
		if mod, ok := r.goModuleOf(dir); ok {
			best := ""
			for _, dep := range r.manifests[path.Join(mod.dir, "go.mod")].deps {
				if (spec == dep || strings.HasPrefix(spec, dep+"/")) && len(dep) > len(best) {
					best = dep
				}
			}
			if best != "" {
				return "go:" + best, KindDependency
			}
		}
		if first, _, _ := strings.Cut(spec, "/"); strings.Contains(first, ".") {
			return "go:" + spec, KindDependency
		}
		return "", "" // standard library

	case "typescript":
		if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
			base := path.Join(dir, spec)
			candidates := []string{}
			for _, ext := range tsExtensions {
				candidates = append(candidates, base+ext)
			}
			// ESM TypeScript imports "./x.js" for the source x.ts.
			if ext := path.Ext(base); ext == ".js" || ext == ".jsx" || ext == ".mjs" {
				stem := strings.TrimSuffix(base, ext)
				candidates = append(candidates, stem+".ts", stem+".tsx", stem+".mts")
			}
			for _, c := range candidates {
				if r.files[c] {
					return c, KindFile
				}
			}
			return "", ""
		}
		name := strings.TrimPrefix(spec, "node:")
		if strings.HasPrefix(spec, "node:") || strings.HasPrefix(spec, "@/") || strings.HasPrefix(spec, "~") || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "#") {
			return "", ""
		}
		parts := strings.Split(name, "/")
		pkg := parts[0]
		if strings.HasPrefix(pkg, "@") && len(parts) > 1 {
			pkg += "/" + parts[1]
		}
		if nodeBuiltins[pkg] {
			return "", ""
		}
		return "npm:" + pkg, KindDependency

	case "python":
		if strings.HasPrefix(spec, ".") {
			levels := len(spec) - len(strings.TrimLeft(spec, "."))
			base := dir
			for i := 1; i < levels; i++ {
				base = path.Dir(base)
			}
			rest := strings.TrimLeft(spec, ".")
			if rest == "" {
				if r.dirs[base] {
					return base, KindPackage
				}
				return "", ""
			}
			p := path.Join(base, strings.ReplaceAll(rest, ".", "/"))
			if r.files[p+".py"] {
				return p + ".py", KindFile
			}
			if r.files[p+"/__init__.py"] {
				return p + "/__init__.py", KindFile
			}
			if r.dirs[p] {
				return p, KindPackage
			}
			return "", ""
		}
		parts := strings.Split(spec, ".")
		for n := len(parts); n > 0; n-- {
			if file, ok := r.pyModules[strings.Join(parts[:n], ".")]; ok {
				return file, KindFile
			}
		}
		if declared, ok := r.declared["pypi"][normalizeDep("pypi", parts[0])]; ok {
			return "pypi:" + declared, KindDependency
		}
		return "", "" // standard library or an undeclared module

	case "rust":
		if strings.HasPrefix(spec, "mod:") {
			name := strings.TrimPrefix(spec, "mod:")
			candidates := []string{path.Join(dir, name+".rs"), path.Join(dir, name, "mod.rs")}
			if stem := strings.TrimSuffix(path.Base(f.path), ".rs"); stem != "mod" && stem != "lib" && stem != "main" {
				candidates = append(candidates, path.Join(dir, stem, name+".rs"), path.Join(dir, stem, name, "mod.rs"))
			}
			for _, c := range candidates {
				if r.files[c] {
					return c, KindFile
				}
			}
			return "", ""
		}
		parts := strings.Split(spec, "::")
		switch parts[0] {
		case "crate":
			src := path.Join(r.componentOf(f.path), "src")
			for n := len(parts); n > 1; n-- {
				p := path.Join(append([]string{src}, parts[1:n]...)...)
				if r.files[p+".rs"] {
					return p + ".rs", KindFile
				}
				if r.files[p+"/mod.rs"] {
					return p + "/mod.rs", KindFile
				}
			}
			return "", ""
		case "self", "super", "std", "core", "alloc", "proc_macro", "test":
			return "", ""
		}
		if declared, ok := r.declared["cargo"][normalizeDep("cargo", parts[0])]; ok {
			return "cargo:" + declared, KindDependency
		}
		return "", ""
	}
	return "", ""
}

// normalizeDep folds the spellings of one dependency name: Python and Rust
// treat - and _ alike, Python ignores case.
func normalizeDep(ecosystem, name string) string {
	switch ecosystem {
	case "pypi":
		return strings.ReplaceAll(strings.ToLower(name), "_", "-")
	case "cargo":
		return strings.ReplaceAll(name, "-", "_")
	}
	return name
}
