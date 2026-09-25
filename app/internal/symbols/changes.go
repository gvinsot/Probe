package symbols

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// fingerprint is the token digest of one function's signature and body.
// Comments and formatting are ignored (linter.TokenDigest).
type fingerprint struct {
	name      string // "F" or "T.M"
	pkgName   string
	path      string
	line, end int
	sig, body string
}

// changedFunction is a function or method of a changed non-test Go file whose
// candidate version differs from its baseline version.
type changedFunction struct {
	path, dir, pkgName, name string
	line, end                int
	change                   string // model.ChangeBodyChanged | model.ChangeSignatureChanged
}

// fingerprints lists the functions and methods of one Go source. init
// functions, blank functions and main in package main cannot be called and are
// skipped.
func fingerprints(p string, src []byte) ([]fingerprint, string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, "", err
	}
	file := fset.File(f.Pos())
	offset := func(pos token.Pos) int { return file.Offset(pos) }
	var out []fingerprint
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name == "_" {
			continue
		}
		if fd.Recv == nil && (fd.Name.Name == "init" || fd.Name.Name == "main" && f.Name.Name == "main") {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			name = receiverName(fd.Recv.List[0].Type) + "." + name
		}
		start, sigEnd, end := offset(fd.Pos()), offset(fd.End()), offset(fd.End())
		body := ""
		if fd.Body != nil {
			sigEnd = offset(fd.Body.Lbrace)
			body = linter.TokenDigest(src[sigEnd:end])
		}
		out = append(out, fingerprint{
			name: name, pkgName: f.Name.Name, path: p,
			line: fset.Position(fd.Pos()).Line, end: fset.Position(fd.End()).Line,
			sig: linter.TokenDigest(src[start:sigEnd]), body: body,
		})
	}
	return out, f.Name.Name, nil
}

// receiverName is the receiver's type name without pointer or type arguments
// (the same convention as the linter).
func receiverName(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.StarExpr:
		return receiverName(n.X)
	case *ast.ParenExpr:
		return receiverName(n.X)
	case *ast.IndexExpr:
		return receiverName(n.X)
	case *ast.IndexListExpr:
		return receiverName(n.X)
	}
	return "?"
}

// fnKey identifies a function across revisions: its directory, package clause
// and name. It does not contain the file name, so a function moved between
// files of a package without other change is unchanged.
func fnKey(f fingerprint) string { return dirOf(f.path) + "\x00" + f.pkgName + "\x00" + f.name }

// sourcePair is one changed file: its candidate path and source ("" when
// deleted there) and its baseline path and source ("" when added).
type sourcePair struct {
	headPath, basePath string
	head, base         []byte
}

// goSource reports a non-test Go file.
func goSource(p string) bool { return strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") }

// compareFunctions classifies the candidate functions of the changed files. A
// function without a baseline counterpart is added and a function without a
// candidate counterpart removed; neither is a changed function. Several
// variants of one key (for example per-OS files) are compared as multisets. A
// file that does not parse on a side is left out on both sides. It returns
// the changed functions sorted by path and line, the keys of every changed or
// added function, and the number of files left out.
func compareFunctions(pairs []sourcePair) ([]changedFunction, map[string]bool, int) {
	failed := 0
	before, after := map[string][]fingerprint{}, map[string][]fingerprint{}
	sorted := append([]sourcePair(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].headPath != sorted[j].headPath {
			return sorted[i].headPath < sorted[j].headPath
		}
		return sorted[i].basePath < sorted[j].basePath
	})
	for _, p := range sorted {
		var headPrints, basePrints []fingerprint
		var err error
		if p.headPath != "" {
			if headPrints, _, err = fingerprints(p.headPath, p.head); err != nil {
				failed++
				continue
			}
		}
		if p.basePath != "" {
			if basePrints, _, err = fingerprints(p.basePath, p.base); err != nil {
				failed++
				continue
			}
		}
		for _, f := range headPrints {
			after[fnKey(f)] = append(after[fnKey(f)], f)
		}
		for _, f := range basePrints {
			before[fnKey(f)] = append(before[fnKey(f)], f)
		}
	}
	var changed []changedFunction
	touched := map[string]bool{}
	for key, variants := range after {
		previous := before[key]
		if len(previous) == 0 {
			touched[key] = true
			continue
		}
		same, sigs := map[[2]string]int{}, map[string]bool{}
		for _, p := range previous {
			same[[2]string{p.sig, p.body}]++
			sigs[p.sig] = true
		}
		for _, v := range variants {
			k := [2]string{v.sig, v.body}
			if same[k] > 0 {
				same[k]--
				continue
			}
			class := model.ChangeSignatureChanged
			if sigs[v.sig] {
				class = model.ChangeBodyChanged
			}
			touched[key] = true
			changed = append(changed, changedFunction{path: v.path, dir: dirOf(v.path), pkgName: v.pkgName, name: v.name, line: v.line, end: v.end, change: class})
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		a, b := changed[i], changed[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.name < b.name
	})
	return changed, touched, failed
}

// changedGoFiles returns the candidate paths of the changed non-test Go files
// the index may read, and whether any changed file (test files included, on
// either side) is a Go file the index may read.
func changedGoFiles(change model.Change, sensitive func(string) bool) (head []string, any bool) {
	for _, f := range change.Files {
		oldPath := f.Path
		if f.OldPath != "" {
			oldPath = f.OldPath
		}
		for _, p := range []string{f.Path, oldPath} {
			if strings.HasSuffix(p, ".go") && indexable(p, sensitive) {
				any = true
			}
		}
		if f.Status != "D" && goSource(f.Path) && indexable(f.Path, sensitive) {
			head = append(head, f.Path)
		}
	}
	return head, any
}

// addedLines is the set of candidate lines the change added, per path.
func addedLines(change model.Change) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, f := range change.Files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind == "add" && l.NewLine > 0 {
					if out[f.Path] == nil {
						out[f.Path] = map[int]bool{}
					}
					out[f.Path][l.NewLine] = true
				}
			}
		}
	}
	return out
}
