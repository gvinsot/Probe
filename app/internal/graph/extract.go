package graph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strings"
)

// fileFacts is what the extraction reads from one source file: its imports as
// written, and the types it declares. Nothing is resolved here.
type fileFacts struct {
	Package string // Go package name
	Imports []importSite
	Types   []typeDecl
}

type importSite struct {
	Spec string // as written: "github.com/x/y", "./util", "..models", "crate::a::b"
	Line int
}

type typeDecl struct {
	Name     string
	Kind     string // struct, interface, class, trait, enum, alias, type
	Line     int
	EndLine  int
	Exported bool
}

// extract reads the facts of one file of a language ("go", "typescript",
// "python", "rust"). A file that does not parse yields what could be read.
func extract(language, name string, src []byte) fileFacts {
	switch language {
	case "go":
		return extractGo(name, src)
	case "typescript":
		return extractLines(src, tsImport, tsType, func(n string) bool { return true })
	case "python":
		return extractLines(src, pyImport, pyType, func(n string) bool { return !strings.HasPrefix(n, "_") })
	case "rust":
		return extractLines(src, rsImport, rsType, nil)
	}
	return fileFacts{}
}

func extractGo(name string, src []byte) fileFacts {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if f == nil {
		return fileFacts{}
	}
	_ = err // a partial AST still gives its imports and declarations
	out := fileFacts{Package: f.Name.Name}
	for _, spec := range f.Imports {
		if spec.Path == nil {
			continue
		}
		out.Imports = append(out.Imports, importSite{Spec: strings.Trim(spec.Path.Value, "`\""), Line: fset.Position(spec.Pos()).Line})
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, s := range gen.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok {
				continue
			}
			kind := "type"
			switch ts.Type.(type) {
			case *ast.StructType:
				kind = "struct"
			case *ast.InterfaceType:
				kind = "interface"
			}
			if ts.Assign.IsValid() {
				kind = "alias"
			}
			out.Types = append(out.Types, typeDecl{
				Name: ts.Name.Name, Kind: kind, Exported: ast.IsExported(ts.Name.Name),
				Line: fset.Position(ts.Pos()).Line, EndLine: fset.Position(ts.End()).Line,
			})
		}
	}
	return out
}

// Line-based extraction of the other languages. The patterns read one
// declaration or import per line, which covers the usual formatting; a
// statement split over several lines is missed, as the lexical index misses
// it.
var (
	tsImport = []*regexp.Regexp{
		regexp.MustCompile(`^\s*import\s+(?:type\s+)?(?:[^'"]*?\s+from\s+)?['"]([^'"]+)['"]`),
		regexp.MustCompile(`^\s*export\s+(?:type\s+)?(?:\*|\{[^}]*\})(?:\s+as\s+\w+)?\s+from\s+['"]([^'"]+)['"]`),
		regexp.MustCompile(`\brequire\(\s*['"]([^'"]+)['"]\s*\)`),
		regexp.MustCompile(`\bimport\(\s*['"]([^'"]+)['"]\s*\)`),
	}
	tsType = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(class|interface|enum|type)\s+([A-Za-z_$][\w$]*)`)

	pyImport = []*regexp.Regexp{
		regexp.MustCompile(`^\s*from\s+(\.*[\w.]*)\s+import\b`),
		regexp.MustCompile(`^\s*import\s+([\w.]+(?:\s+as\s+\w+)?(?:\s*,\s*[\w.]+(?:\s+as\s+\w+)?)*)`),
	}
	pyType = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)

	rsImport = []*regexp.Regexp{
		regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?use\s+(?:::)?([\w]+(?:::[\w]+)*)`),
		regexp.MustCompile(`^\s*extern\s+crate\s+(\w+)`),
		regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+(\w+)\s*;`),
	}
	rsType = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(struct|enum|trait|union|type)\s+([A-Za-z_]\w*)`)
)

func extractLines(src []byte, imports []*regexp.Regexp, types *regexp.Regexp, exported func(string) bool) fileFacts {
	var out fileFacts
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		for i, re := range imports {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				spec := m[1]
				if types == rsType && i == 2 {
					spec = "mod:" + spec // a module declared in this file: mod x;
				}
				if types == pyType && i == 1 {
					// import a.b, c as d
					for _, part := range strings.Split(spec, ",") {
						part = strings.TrimSpace(strings.SplitN(strings.TrimSpace(part), " ", 2)[0])
						if part != "" {
							out.Imports = append(out.Imports, importSite{Spec: part, Line: line})
						}
					}
					continue
				}
				out.Imports = append(out.Imports, importSite{Spec: spec, Line: line})
			}
		}
		if m := types.FindStringSubmatch(text); m != nil {
			kind, name := "class", m[len(m)-1]
			if len(m) == 3 {
				kind = m[1]
			}
			if kind == "type" && types != rsType {
				kind = "alias"
			}
			exp := true
			if exported != nil {
				exp = exported(name)
			} else {
				exp = strings.HasPrefix(strings.TrimSpace(text), "pub")
			}
			out.Types = append(out.Types, typeDecl{Name: name, Kind: kind, Line: line, EndLine: line, Exported: exp})
		}
	}
	return out
}

// Manifests: the external dependencies a component declares.

// manifestDeps returns the dependencies a manifest declares, as (ecosystem,
// name), sorted, or nil for a file that is not a manifest.
func manifestDeps(name string, src []byte) (ecosystem string, deps []string) {
	switch path.Base(name) {
	case "go.mod":
		return "go", goModRequires(src)
	case "package.json":
		return "npm", packageJSONDeps(src)
	case "Cargo.toml":
		return "cargo", tomlSectionKeys(src, "dependencies", "dev-dependencies", "build-dependencies")
	case "pyproject.toml":
		return "pypi", pyprojectDeps(src)
	}
	if b := path.Base(name); strings.HasPrefix(b, "requirements") && strings.HasSuffix(b, ".txt") {
		return "pypi", requirementsDeps(src)
	}
	return "", nil
}

// isManifest reports a file that makes its directory a component root.
func isManifest(name string) bool {
	switch path.Base(name) {
	case "go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py":
		return true
	}
	return false
}

// goModule returns the module path of a go.mod.
func goModule(src []byte) string {
	for _, l := range strings.Split(string(src), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

func goModRequires(src []byte) []string {
	var out []string
	block := false
	for _, l := range strings.Split(string(src), "\n") {
		l = strings.TrimSpace(strings.SplitN(l, "//", 2)[0])
		switch {
		case l == "require (":
			block = true
		case block && l == ")":
			block = false
		case block && l != "":
			out = append(out, strings.Fields(l)[0])
		case strings.HasPrefix(l, "require ") && !strings.HasSuffix(l, "("):
			if f := strings.Fields(l); len(f) >= 2 {
				out = append(out, f[1])
			}
		}
	}
	return uniqueSorted(out)
}

func packageJSONDeps(src []byte) []string {
	var p map[string]json.RawMessage
	if json.Unmarshal(src, &p) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]any
		if json.Unmarshal(p[k], &deps) == nil {
			for name := range deps {
				out = append(out, name)
			}
		}
	}
	return uniqueSorted(out)
}

var (
	tomlSection = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	tomlKey     = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+|"[^"]+")\s*=`)
	pepName     = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._\-]*)`)
)

// tomlSectionKeys returns the keys of the given sections, and the names of
// [section.name] tables under them (Cargo's detailed dependencies).
func tomlSectionKeys(src []byte, sections ...string) []string {
	want := map[string]bool{}
	for _, s := range sections {
		want[s] = true
	}
	var out []string
	in := false
	for _, l := range strings.Split(string(src), "\n") {
		if m := tomlSection.FindStringSubmatch(l); m != nil {
			name := strings.TrimSpace(m[1])
			in = want[name]
			for s := range want {
				if strings.HasPrefix(name, s+".") {
					out = append(out, strings.Trim(strings.TrimPrefix(name, s+"."), `"`))
				}
			}
			continue
		}
		if in {
			if m := tomlKey.FindStringSubmatch(l); m != nil {
				out = append(out, strings.Trim(m[1], `"`))
			}
		}
	}
	return uniqueSorted(out)
}

var (
	pyDepsArray     = regexp.MustCompile(`(?ms)^\s*dependencies\s*=\s*\[(.*?)\]`)
	pyOptionalBlock = regexp.MustCompile(`(?ms)^\[project\.optional-dependencies\]\s*$(.*?)(?:^\[|\z)`)
	pyArray         = regexp.MustCompile(`(?ms)=\s*\[(.*?)\]`)
	pyQuoted        = regexp.MustCompile(`["']([^"']+)["']`)
)

// pyprojectDeps reads [project] dependencies and optional-dependencies, and
// the Poetry dependency tables.
func pyprojectDeps(src []byte) []string {
	out := tomlSectionKeys(src, "tool.poetry.dependencies", "tool.poetry.dev-dependencies", "tool.poetry.group.dev.dependencies")
	var arrays []string
	for _, m := range pyDepsArray.FindAllSubmatch(src, -1) {
		arrays = append(arrays, string(m[1]))
	}
	for _, block := range pyOptionalBlock.FindAllSubmatch(src, -1) {
		for _, m := range pyArray.FindAllSubmatch(block[1], -1) {
			arrays = append(arrays, string(m[1]))
		}
	}
	for _, a := range arrays {
		for _, item := range pyQuoted.FindAllStringSubmatch(a, -1) {
			if m := pepName.FindStringSubmatch(item[1]); m != nil {
				out = append(out, m[1])
			}
		}
	}
	var clean []string
	for _, d := range out {
		if d = strings.ToLower(d); d != "python" {
			clean = append(clean, d)
		}
	}
	return uniqueSorted(clean)
}

func requirementsDeps(src []byte) []string {
	var out []string
	for _, l := range strings.Split(string(src), "\n") {
		l = strings.TrimSpace(strings.SplitN(l, "#", 2)[0])
		if l == "" || strings.HasPrefix(l, "-") {
			continue
		}
		if m := pepName.FindStringSubmatch(l); m != nil {
			out = append(out, strings.ToLower(m[1]))
		}
	}
	return uniqueSorted(out)
}

func uniqueSorted(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	sort.Strings(list)
	out := list[:1]
	for _, s := range list[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
