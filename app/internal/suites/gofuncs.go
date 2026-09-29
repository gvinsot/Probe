package suites

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/linter"
)

// testFunc is one runnable Go test function of a parsed test file.
type testFunc struct {
	name          string
	line, endLine int
	digest        string // linter.TokenDigest of the whole declaration
}

// testFile is what planning needs from one Go test file: its runnable test
// functions in declaration order, a digest of every other top-level
// declaration (helpers, types, variables, constants, TestMain, init), and the
// file-level text that decides whether and how go test compiles the file.
type testFile struct {
	tests  []testFunc
	byName map[string]int
	shared map[string]string // declaration key -> token digest
	// effects lists the shared declarations that can change how the other
	// tests of the package run although no test refers to them: init,
	// TestMain, a package-level variable with an initializer, and a method.
	// A method maps to its receiver type name, every other key to "".
	effects map[string]string
	// header is the package clause, the build constraints before it, the
	// import set and the compiler directives (//go:, //line) of the file, in
	// a canonical form. The token digests ignore comments; this does not.
	header string
}

// test returns the runnable test function called name.
func (f testFile) test(name string) (testFunc, bool) {
	i, ok := f.byName[name]
	if !ok {
		return testFunc{}, false
	}
	return f.tests[i], true
}

// parseTestFile parses one Go test file statically: it never executes or
// type-checks repository code. Any syntax error rejects the whole file, so a
// partial AST never decides what is selected.
func parseTestFile(filename string, src []byte) (testFile, error) {
	if !utf8.Valid(src) {
		return testFile{}, errors.New("not valid UTF-8")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return testFile{}, err
	}
	tf := fset.File(file.Pos())
	if tf == nil || tf.Size() != len(src) {
		return testFile{}, errors.New("unexpected source size")
	}
	digest := func(from, to token.Pos) string {
		start, end := tf.Offset(from), tf.Offset(to)
		if start < 0 || end > len(src) || start > end {
			return ""
		}
		return linter.TokenDigest(src[start:end])
	}
	out := testFile{byName: map[string]int{}, shared: map[string]string{}, effects: map[string]string{}, header: fileHeader(file, src, tf)}
	occurrences := map[string]int{}
	addShared := func(key, d string) string {
		occurrences[key]++
		if n := occurrences[key]; n > 1 {
			key = fmt.Sprintf("%s#%d", key, n)
		}
		out.shared[key] = d
		return key
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if runnableTest(d) {
				if _, dup := out.byName[d.Name.Name]; dup {
					// The package would not compile; keep the first declaration.
					continue
				}
				out.byName[d.Name.Name] = len(out.tests)
				out.tests = append(out.tests, testFunc{
					name:    d.Name.Name,
					line:    fset.Position(d.Pos()).Line,
					endLine: fset.Position(d.End()).Line,
					digest:  digest(d.Pos(), d.End()),
				})
				continue
			}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := d.Recv.List[0].Type
				key := "func (" + strings.Join(strings.Fields(string(sourceOf(tf, src, recv.Pos(), recv.End()))), "") + ")." + d.Name.Name
				out.effects[addShared(key, digest(d.Pos(), d.End()))] = receiverTypeName(recv)
				continue
			}
			key := addShared("func "+d.Name.Name, digest(d.Pos(), d.End()))
			if d.Name.Name == "init" || d.Name.Name == "TestMain" {
				out.effects[key] = ""
			}
		case *ast.GenDecl:
			switch d.Tok {
			case token.IMPORT:
				continue
			case token.TYPE:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						addShared("type "+ts.Name.Name, digest(ts.Pos(), ts.End()))
					}
				}
			case token.VAR, token.CONST:
				// A group shares one digest: an implicit iota or repeated
				// expression makes every name depend on the whole group.
				group := digest(d.Pos(), d.End())
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						key := addShared(strings.ToLower(d.Tok.String())+" "+name.Name, group)
						if d.Tok == token.VAR && len(vs.Values) > 0 {
							out.effects[key] = ""
						}
					}
				}
			}
		}
	}
	return out, nil
}

// fileHeader returns the canonical file-level text of a parsed file: the
// package clause, every //go:build or // +build line before it, the import
// set (sorted, so reordering imports changes nothing) and every compiler
// directive comment (//go:..., //line) in source order. The token digests
// ignore comments, and these comments change what go test compiles or runs.
func fileHeader(file *ast.File, src []byte, tf *token.File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n", file.Name.Name)
	for _, group := range file.Comments {
		for _, c := range group.List {
			text := c.Text
			switch {
			case c.End() < file.Package && (constraint.IsGoBuild(text) || constraint.IsPlusBuild(text)):
				fmt.Fprintf(&b, "constraint %s\n", strings.Join(strings.Fields(text), " "))
			case strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "/*line "):
				fmt.Fprintf(&b, "directive %s\n", strings.Join(strings.Fields(text), " "))
			}
		}
	}
	var imports []string
	for _, spec := range file.Imports {
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			p = string(sourceOf(tf, src, spec.Path.Pos(), spec.Path.End()))
		}
		imports = append(imports, fmt.Sprintf("import %q %q\n", name, p))
	}
	sort.Strings(imports)
	for _, line := range imports {
		b.WriteString(line)
	}
	return b.String()
}

// receiverTypeName returns the base type name of a method receiver: T for T,
// *T, T[P] and *T[P].
func receiverTypeName(expr ast.Expr) string {
	for {
		switch x := expr.(type) {
		case *ast.StarExpr:
			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.IndexListExpr:
			expr = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// sourceOf returns the source bytes between two positions, or nil.
func sourceOf(tf *token.File, src []byte, from, to token.Pos) []byte {
	start, end := tf.Offset(from), tf.Offset(to)
	if start < 0 || end > len(src) || start > end {
		return nil
	}
	return src[start:end]
}

// runnableTest mirrors the rule go test applies to a top-level function: a
// name that is Test or Test followed by a non-lowercase rune, other than
// TestMain, no receiver, no type parameters, no results, and exactly one
// parameter of type *T or *pkg.T.
func runnableTest(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !isTestName(fn.Name.Name) || fn.Name.Name == "TestMain" {
		return false
	}
	t := fn.Type
	if t.TypeParams != nil && len(t.TypeParams.List) > 0 {
		return false
	}
	if t.Results != nil && len(t.Results.List) > 0 {
		return false
	}
	if t.Params == nil || len(t.Params.List) != 1 || len(t.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := t.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := star.X.(type) {
	case *ast.Ident:
		return x.Name == "T"
	case *ast.SelectorExpr:
		return x.Sel.Name == "T"
	}
	return false
}

// isTestName reports whether name is a Go test function name.
func isTestName(name string) bool {
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

// sharedChanged reports whether a shared declaration of the baseline file
// disappeared or changed in the candidate file.
func sharedChanged(base, candidate testFile) bool {
	for key, d := range base.shared {
		if c, ok := candidate.shared[key]; !ok || c != d {
			return true
		}
	}
	return false
}

// addedEffect reports whether the candidate file added a declaration that can
// change how its unchanged tests run although none of them refers to it:
// init, TestMain (which may never call m.Run), a package-level variable with
// an initializer, or a method of a type that the candidate file did not newly
// declare (a method can change which interfaces a type satisfies or how it
// prints). Other added declarations (tests, functions, types, constants)
// change nothing until a test uses them, and that use changes the test.
func addedEffect(base, candidate testFile) bool {
	for key, recv := range candidate.effects {
		if _, ok := base.shared[key]; ok {
			continue
		}
		if recv != "" {
			_, declaredNow := candidate.shared["type "+recv]
			_, declaredBefore := base.shared["type "+recv]
			if declaredNow && !declaredBefore {
				continue
			}
		}
		return true
	}
	return false
}

// fileChanged reports whether the candidate file differs from the baseline
// file outside its test functions in a way that can change how an unchanged
// test runs: the file-level header, a shared declaration, or an added
// declaration with an effect.
func fileChanged(base, candidate testFile) bool {
	return base.header != candidate.header || sharedChanged(base, candidate) || addedEffect(base, candidate)
}
