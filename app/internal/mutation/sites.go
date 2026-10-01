package mutation

import (
	"bytes"
	"errors"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Operator identifiers, in rank order. The rank orders the sites of one line
// and so decides which operator breadth-first selection takes first.
const (
	OpDropError         = "drop_error"         // last result of a return in a function whose last result is error -> nil
	OpNegateCondition   = "negate_condition"   // if condition c -> !(c)
	OpBoundary          = "boundary"           // < <-> <=, > <-> >=
	OpNegateComparison  = "negate_comparison"  // == <-> !=
	OpSwapLogical       = "swap_logical"       // && <-> ||
	OpIncrementConstant = "increment_constant" // integer literal operand n of a comparison -> (n+1)
	OpFlipBoolean       = "flip_boolean"       // true <-> false as a direct return result
	OpSwapArithmetic    = "swap_arithmetic"    // + <-> -, * <-> /
)

// Operators lists every operator in rank order.
var Operators = []string{OpDropError, OpNegateCondition, OpBoundary, OpNegateComparison, OpSwapLogical, OpIncrementConstant, OpFlipBoolean, OpSwapArithmetic}

var operatorRank = func() map[string]int {
	m := make(map[string]int, len(Operators))
	for i, op := range Operators {
		m[op] = i
	}
	return m
}()

// Enumeration caps. A file with more sites stops at maxSitesPerFile and the
// plan stops enumerating new files at maxSitesTotal; both are recorded.
const (
	maxSitesPerFile = 2000
	maxSitesTotal   = 20000
)

// Site is one candidate mutant: the byte span [Start, End) of a source file,
// its original text and its replacement. Line..EndLine are the new-side lines
// the span covers; every one of them is an added line of the change.
type Site struct {
	Path        string
	Line        int
	EndLine     int
	Column      int // 1-based byte column of Start
	Start, End  int // byte offsets in the source the site was found in
	Operator    string
	Original    string
	Replacement string
	Symbol      string // enclosing function: Name, or Recv.Name for a method
}

func (s Site) rank() int { return operatorRank[s.Operator] }

// Fixed file skip reasons produced by FileSites.
const (
	skipParse      = "the file does not parse as Go source"
	skipConstraint = "the file has a //go:build or // +build constraint; files a build may exclude are not mutated"
	skipGenerated  = "the file is marked as generated code"
	skipCgo        = "the file uses cgo (it imports the C pseudo-package)"
)

// FileSites parses src and returns its mutation sites whose whole replaced
// span lies on added lines, sorted by line, operator rank and column. It
// returns a skip reason, and no site, when the whole file is excluded; capped
// is true when enumeration stopped at maxSitesPerFile. Only function bodies
// are mutated. Repository code is parsed on the host, never executed or
// type-checked.
func FileSites(path string, src []byte, added map[int]bool) (sites []Site, capped bool, skip string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, skipParse
	}
	if hasBuildConstraint(file) {
		return nil, false, skipConstraint
	}
	if ast.IsGenerated(file) {
		return nil, false, skipGenerated
	}
	for _, imp := range file.Imports {
		if imp.Path != nil && imp.Path.Value == `"C"` {
			return nil, false, skipCgo
		}
	}
	tf := fset.File(file.Pos())
	if tf == nil {
		return nil, false, skipParse
	}
	imports := importUses(file, tf)
	add := func(start, end token.Pos, op, replacement, symbol string) {
		if capped {
			return
		}
		s, e := tf.Offset(start), tf.Offset(end)
		if s < 0 || e > len(src) || s >= e || joinsNeighbour(src, s, e, replacement) {
			return
		}
		first, last := tf.Line(start), tf.Position(tf.Pos(e-1)).Line
		for line := first; line <= last; line++ {
			if !added[line] {
				return
			}
		}
		if len(sites) >= maxSitesPerFile {
			capped = true
			return
		}
		sites = append(sites, Site{Path: path, Line: first, EndLine: last, Column: tf.Position(start).Column, Start: s, End: e, Operator: op, Original: string(src[s:e]), Replacement: replacement, Symbol: symbol})
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		symbol := funcName(fd)
		var stack []ast.Node
		ast.Inspect(fd, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.ReturnStmt:
				returnSites(x, innermostFuncType(stack), imports, tf, src, symbol, add)
			case *ast.IfStmt:
				if x.Cond != nil {
					s, e := tf.Offset(x.Cond.Pos()), tf.Offset(x.Cond.End())
					if s >= 0 && e <= len(src) && s < e {
						add(x.Cond.Pos(), x.Cond.End(), OpNegateCondition, "!("+string(src[s:e])+")", symbol)
					}
				}
			case *ast.BinaryExpr:
				binarySites(x, add, symbol)
			}
			return true
		})
	}
	sort.SliceStable(sites, func(i, j int) bool {
		a, b := sites[i], sites[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.rank() != b.rank() {
			return a.rank() < b.rank()
		}
		return a.Column < b.Column
	})
	return sites, capped, ""
}

type addFunc func(start, end token.Pos, op, replacement, symbol string)

// returnSites adds drop_error and flip_boolean sites of one return statement.
// ftype is the innermost function type (a function literal's own result list
// wins over the declaration's).
//
// A dropped result that spans several lines (a gofmt'd multi-line
// fmt.Errorf call or composite literal) is replaced by nil followed by one
// newline per newline of the span: nil ends the return statement at the end of
// its line, the rest of the span becomes empty lines, and every later line
// keeps its number, as Apply requires.
func returnSites(ret *ast.ReturnStmt, ftype *ast.FuncType, imports map[string][]int, tf *token.File, src []byte, symbol string, add addFunc) {
	if ftype != nil && ftype.Results != nil && len(ftype.Results.List) > 0 && len(ret.Results) > 0 && len(ret.Results) == resultCount(ftype) {
		lastField := ftype.Results.List[len(ftype.Results.List)-1]
		if id, ok := lastField.Type.(*ast.Ident); ok && id.Name == "error" {
			last := ret.Results[len(ret.Results)-1]
			s, e := tf.Offset(last.Pos()), tf.Offset(last.End())
			if !isIdent(last, "nil") && s >= 0 && e <= len(src) && s < e && !dropsLastImportUse(imports, s, e) {
				add(last.Pos(), last.End(), OpDropError, "nil"+strings.Repeat("\n", bytes.Count(src[s:e], []byte("\n"))), symbol)
			}
		}
	}
	for _, result := range ret.Results {
		switch {
		case isIdent(result, "true"):
			add(result.Pos(), result.End(), OpFlipBoolean, "false", symbol)
		case isIdent(result, "false"):
			add(result.Pos(), result.End(), OpFlipBoolean, "true", symbol)
		}
	}
}

// binarySites adds the operator swaps of one binary expression and the
// increment_constant sites of a comparison's integer literal operands.
func binarySites(x *ast.BinaryExpr, add addFunc, symbol string) {
	swap := func(op string, to token.Token) {
		add(x.OpPos, x.OpPos+token.Pos(len(x.Op.String())), op, to.String(), symbol)
	}
	switch x.Op {
	case token.LSS:
		swap(OpBoundary, token.LEQ)
	case token.LEQ:
		swap(OpBoundary, token.LSS)
	case token.GTR:
		swap(OpBoundary, token.GEQ)
	case token.GEQ:
		swap(OpBoundary, token.GTR)
	case token.EQL:
		swap(OpNegateComparison, token.NEQ)
	case token.NEQ:
		swap(OpNegateComparison, token.EQL)
	case token.LAND:
		swap(OpSwapLogical, token.LOR)
	case token.LOR:
		swap(OpSwapLogical, token.LAND)
	case token.ADD:
		// + concatenates strings; - does not. A string literal operand makes the
		// mutant fail to compile, so it is not generated.
		if !isStringLit(x.X) && !isStringLit(x.Y) {
			swap(OpSwapArithmetic, token.SUB)
		}
	case token.SUB:
		swap(OpSwapArithmetic, token.ADD)
	case token.MUL:
		swap(OpSwapArithmetic, token.QUO)
	case token.QUO:
		swap(OpSwapArithmetic, token.MUL)
	}
	switch x.Op {
	case token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ:
		for _, operand := range []ast.Expr{x.X, x.Y} {
			if lit, ok := operand.(*ast.BasicLit); ok && lit.Kind == token.INT {
				add(lit.Pos(), lit.End(), OpIncrementConstant, "("+lit.Value+"+1)", symbol)
			}
		}
	}
}

// Apply returns the mutated source. It refuses a source whose recorded span
// no longer holds Original, a result with a different number of lines, and a
// result that does not parse, so a mutant never shifts line numbers and never
// runs on a tree the report does not describe.
func (s Site) Apply(src []byte) ([]byte, error) {
	if scriptPath(s.Path) {
		return s.applyScript(src)
	}
	if pythonPath(s.Path) {
		return s.applyPython(src)
	}
	if s.Start < 0 || s.End > len(src) || s.Start >= s.End || string(src[s.Start:s.End]) != s.Original {
		return nil, errors.New("the source no longer holds the original text at the recorded position")
	}
	if joinsNeighbour(src, s.Start, s.End, s.Replacement) {
		return nil, errors.New("the replacement would merge with a neighbouring character into another token")
	}
	out := make([]byte, 0, len(src)-len(s.Original)+len(s.Replacement))
	out = append(out, src[:s.Start]...)
	out = append(out, s.Replacement...)
	out = append(out, src[s.End:]...)
	if bytes.Count(out, []byte("\n")) != bytes.Count(src, []byte("\n")) {
		return nil, errors.New("the mutant would change the number of lines")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), s.Path, out, parser.SkipObjectResolution); err != nil {
		return nil, errors.New("the mutated file does not parse")
	}
	return out, nil
}

// joinedTokens are the two-character Go tokens and comment openers. A
// replacement whose first or last character forms one of them with the
// neighbouring source byte would be scanned differently from the recorded
// mutant: -1+-4 swapped to -1--4 becomes a decrement, a**p swapped to a/*p
// opens a comment. Such sites are not generated.
var joinedTokens = map[string]bool{
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true, "^=": true,
	"<<": true, ">>": true, "&^": true, "&&": true, "||": true, "<-": true, "++": true, "--": true,
	"==": true, "!=": true, "<=": true, ">=": true, ":=": true, "..": true, "//": true, "/*": true,
}

// joinsNeighbour reports whether replacing src[start:end] by replacement
// joins the replacement's first character with the byte before start, or its
// last character with the byte at end, into one of joinedTokens.
func joinsNeighbour(src []byte, start, end int, replacement string) bool {
	if replacement == "" {
		return false
	}
	if start > 0 && joinedTokens[string([]byte{src[start-1], replacement[0]})] {
		return true
	}
	return end < len(src) && joinedTokens[string([]byte{replacement[len(replacement)-1], src[end]})]
}

// importUses maps the name of each named or plainly imported package of the
// file to the byte offsets of its qualified uses (name.X). The name is the
// import alias, or the last element of the import path when that is an
// identifier; blank and dot imports, and paths whose last element is not an
// identifier, are left out. The analysis is syntactic: a local variable that
// shadows a package name is counted as a use, which can only keep a site.
func importUses(file *ast.File, tf *token.File) map[string][]int {
	uses := map[string][]int{}
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			name = path.Base(p)
		}
		if name == "_" || name == "." || !token.IsIdentifier(name) {
			continue
		}
		uses[name] = []int{}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				if list, known := uses[id.Name]; known {
					uses[id.Name] = append(list, tf.Offset(id.Pos()))
				}
			}
		}
		return true
	})
	return uses
}

// dropsLastImportUse reports whether the span [start, end) holds every
// qualified use of some imported package. Replacing such a span makes the
// import unused, and a file with an unused import does not compile, so the
// mutant could only be INVALID.
func dropsLastImportUse(imports map[string][]int, start, end int) bool {
	for _, offsets := range imports {
		if len(offsets) == 0 {
			continue
		}
		inside := true
		for _, offset := range offsets {
			if offset < start || offset >= end {
				inside = false
				break
			}
		}
		if inside {
			return true
		}
	}
	return false
}

// hasBuildConstraint reports a //go:build or // +build line before the package
// clause.
func hasBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return true
			}
		}
	}
	return false
}

// innermostFuncType returns the type of the closest enclosing function
// declaration or literal on the inspection stack.
func innermostFuncType(stack []ast.Node) *ast.FuncType {
	for i := len(stack) - 1; i >= 0; i-- {
		switch f := stack[i].(type) {
		case *ast.FuncLit:
			return f.Type
		case *ast.FuncDecl:
			return f.Type
		}
	}
	return nil
}

// resultCount is the number of values a function returns.
func resultCount(ftype *ast.FuncType) int {
	if ftype.Results == nil {
		return 0
	}
	n := 0
	for _, field := range ftype.Results.List {
		if len(field.Names) == 0 {
			n++
		} else {
			n += len(field.Names)
		}
	}
	return n
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

// funcName is Name for a function and Recv.Name for a method.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fd.Name.Name
		}
		return fd.Name.Name
	}
}
