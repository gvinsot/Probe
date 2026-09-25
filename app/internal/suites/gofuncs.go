package suites

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/linter"
)

// testFunc is one runnable Go test function of a parsed test file.
type testFunc struct {
	name          string
	line, endLine int
	digest        string // linter.TokenDigest of the whole declaration
}

// testFile is what planning needs from one Go test file: its runnable test
// functions in declaration order, and a digest of every other top-level
// declaration (helpers, types, variables, constants, TestMain, init).
type testFile struct {
	tests  []testFunc
	byName map[string]int
	shared map[string]string // declaration key -> token digest
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
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
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
	out := testFile{byName: map[string]int{}, shared: map[string]string{}}
	occurrences := map[string]int{}
	addShared := func(key, d string) {
		occurrences[key]++
		if n := occurrences[key]; n > 1 {
			key = fmt.Sprintf("%s#%d", key, n)
		}
		out.shared[key] = d
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
			key := "func " + d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := d.Recv.List[0].Type
				key = "func (" + strings.Join(strings.Fields(string(sourceOf(tf, src, recv.Pos(), recv.End()))), "") + ")." + d.Name.Name
			}
			addShared(key, digest(d.Pos(), d.End()))
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
						addShared(strings.ToLower(d.Tok.String())+" "+name.Name, group)
					}
				}
			}
		}
	}
	return out, nil
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
// disappeared or changed in the candidate file. Declarations the candidate
// added are ignored: the hybrid tree runs the baseline file, not the
// candidate's.
func sharedChanged(base, candidate testFile) bool {
	for key, d := range base.shared {
		if c, ok := candidate.shared[key]; !ok || c != d {
			return true
		}
	}
	return false
}
