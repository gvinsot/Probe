package fuzz

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Bounds of host-side selection. Selection reads the two sanitized snapshots
// only; a bound that is hit becomes a visible skip reason, never a silent drop.
const (
	maxPackageFiles = 1000     // .go files per package directory and revision
	maxSourceBytes  = 2 << 20  // bytes per file
	maxPackageBytes = 32 << 20 // bytes per package directory and revision

	// MaxPackageInputs bounds the seeded inputs of one package run; the inputs
	// of each function are reduced evenly to stay within it.
	MaxPackageInputs = 1024
	// MaxArrayLen is the longest array parameter that is generated.
	MaxArrayLen = 16
)

// Fixed skip reasons. Parameterized reasons are built by the helpers below.
const (
	ReasonMethod          = "methods are not fuzzed in this version"
	ReasonGeneric         = "generic functions are not fuzzed in this version"
	ReasonSignature       = "signature changed"
	ReasonConstrained     = "file has build constraints"
	ReasonCgo             = "package uses cgo"
	ReasonPackageName     = "package name differs between revisions"
	ReasonPackageClause   = "package has more than one package clause"
	ReasonShadowed        = "predeclared identifier shadowed"
	ReasonSensitive       = "sensitive path excluded from the sandbox"
	ReasonMovedDir        = "file moved to another directory"
	ReasonNotInSnapshot   = "file is not in the candidate snapshot"
	ReasonNoBody          = "function has no Go body on one revision"
	ReasonDuplicate       = "function is declared more than once in the package"
	ReasonNoInput         = "no seeded input has a call text that redaction leaves unchanged"
	ReasonBudgetPackages  = "fuzz budget reached (max_packages)"
	ReasonBudgetFunctions = "fuzz budget reached (max_functions)"
)

func reasonParamType(t string) string {
	return "parameter type " + shortText(t, 64) + " is not generated in this version"
}

func reasonNamedDiffers(t string) string {
	return "named type " + t + " differs between revisions"
}

// reasonUnreadable and reasonUnparsable name the file or directory relative to
// the snapshot, with a fixed cause: no host path and no operating system
// error text reaches the report.
func reasonUnreadable(detail string) string {
	return "package could not be read within the selection bounds: " + shortText(detail, 160)
}

func reasonUnparsable(name string) string {
	return "package could not be parsed: " + shortText(name, 160) + " has a syntax error"
}

// basicTypes are the predeclared types the corpus generates. uintptr and the
// complex types are deliberately absent.
var basicTypes = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true, "rune": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "byte": true,
	"float32": true, "float64": true,
}

// predeclared lists every predeclared identifier. A package that declares one
// of them at package level is skipped: the rendered harness relies on them.
var predeclared = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "max": true, "min": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
}

// Select plans differential fuzzing of the changed Go functions of change. It
// parses both snapshots on the host and never executes anything. A function is
// planned when it is a package-level function (no receiver, no type
// parameters) whose body token digest differs between the revisions, whose
// signature is textually identical on both, whose parameters are generated,
// whose file and package pass the conservative build checks, and whose corpus
// has at least one input. Every other changed function is listed in
// Plan.Skipped with a fixed reason; functions that exist on one revision only,
// or whose body is unchanged, are not rewrites and are not listed.
//
// Priority is the highest severity of the signals overlapping the function,
// then exported before unexported, then path and line. Packages are taken in
// the order of their best function until max_packages, then functions until
// max_functions; the rest are budget skips. Two calls with the same arguments
// return identical plans.
func Select(baseDir, candidateDir string, change model.Change, signals []model.Signal, limits Limits) (Plan, error) {
	if baseDir == "" || candidateDir == "" {
		return Plan{}, errors.New("differential fuzzing needs both the baseline and the candidate snapshot")
	}
	if limits.MaxFunctions < 1 || limits.MaxPackages < 1 || limits.MaxInputs < 1 {
		return Plan{}, errors.New("differential fuzzing limits must be positive")
	}
	s := &selection{base: baseDir, candidate: candidateDir, packages: map[string]*packagePair{}}
	var eligible []Target
	var skipped []model.FuzzSkip
	skipFile := func(p, reason string) {
		skipped = append(skipped, model.FuzzSkip{Path: p, Line: 0, Symbol: "", Reason: reason})
	}
	seenFile := map[string]bool{}
	for _, f := range change.Files {
		if !goSourceCandidate(f) || seenFile[f.Path] {
			continue
		}
		seenFile[f.Path] = true
		dir := path.Dir(f.Path)
		if f.Status == "R" && f.OldPath != "" && path.Dir(f.OldPath) != dir {
			skipFile(f.Path, ReasonMovedDir)
			continue
		}
		if harness.IsSensitivePath(f.Path) {
			skipFile(f.Path, ReasonSensitive)
			continue
		}
		pair := s.pair(dir)
		if pair.candidate.problem != "" {
			skipFile(f.Path, pair.candidate.problem)
			continue
		}
		if !pair.base.exists {
			continue // a new package: nothing was rewritten
		}
		if pair.base.problem != "" {
			skipFile(f.Path, pair.base.problem)
			continue
		}
		file := pair.candidate.file(f.Path)
		if file == nil {
			skipFile(f.Path, ReasonNotInSnapshot)
			continue
		}
		for _, decl := range file.syntax.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			target, reason, changed := s.examine(pair, file, fd)
			if !changed {
				continue
			}
			if reason == "" && len(Corpus(target, 1)) == 0 {
				reason = ReasonNoInput
			}
			if reason != "" {
				skipped = append(skipped, model.FuzzSkip{Path: f.Path, Line: target.Line, Symbol: target.Symbol, Reason: reason})
				continue
			}
			target.Priority = priority(target, signals)
			eligible = append(eligible, target)
		}
	}
	plan := budget(eligible, limits, s)
	skipped = append(skipped, plan.Skipped...)
	sort.SliceStable(skipped, func(i, j int) bool {
		a, b := skipped[i], skipped[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.Reason < b.Reason
	})
	plan.Skipped = skipped
	if plan.Skipped == nil {
		plan.Skipped = []model.FuzzSkip{}
	}
	return plan, nil
}

// goSourceCandidate reports whether f is a changed, non-test Go source file in
// a directory the go tool builds. Added files count: a function may move to a
// new file of the same package.
func goSourceCandidate(f model.ChangedFile) bool {
	if f.Binary || !strings.HasSuffix(f.Path, ".go") || strings.HasSuffix(f.Path, "_test.go") {
		return false
	}
	switch f.Status {
	case "M", "R", "A":
	default:
		return false
	}
	if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "\\") || path.Clean(f.Path) != f.Path {
		return false
	}
	parts := strings.Split(f.Path, "/")
	for i, part := range parts {
		if part == "" || part == ".." || strings.HasPrefix(part, "_") || strings.HasPrefix(part, ".") {
			return false
		}
		if i < len(parts)-1 && (part == "testdata" || part == "vendor") {
			return false
		}
	}
	return true
}

type selection struct {
	base, candidate string
	packages        map[string]*packagePair
}

type packagePair struct {
	dir             string
	base, candidate *goPackage
}

func (s *selection) pair(dir string) *packagePair {
	if p, ok := s.packages[dir]; ok {
		return p
	}
	p := &packagePair{dir: dir, candidate: loadPackage(s.candidate, dir), base: loadPackage(s.base, dir)}
	s.packages[dir] = p
	return p
}

// examine classifies one candidate function declaration. changed is false when
// the function is not a rewrite (no counterpart, or an identical body digest).
func (s *selection) examine(pair *packagePair, file *goFile, fd *ast.FuncDecl) (Target, string, bool) {
	cand := pair.candidate
	name := fd.Name.Name
	if name == "_" || name == "init" || fd.Recv == nil && name == "main" && cand.name == "main" {
		return Target{}, "", false
	}
	key := funcKey(fd)
	t := Target{
		Dir:      pair.dir,
		Path:     file.path,
		Line:     cand.fset.Position(fd.Pos()).Line,
		EndLine:  cand.fset.Position(fd.End()).Line,
		Name:     name,
		Symbol:   symbolOf(pair.dir, cand.name, key),
		Exported: ast.IsExported(name),
	}
	bases := pair.base.funcs[key]
	if len(bases) == 0 {
		return t, "", false
	}
	cands := cand.funcs[key]
	if len(bases) > 1 || len(cands) > 1 {
		// Declarations of one name in several files (build-constrained
		// variants) form a rewrite only when their bodies changed.
		if sameDigests(bases, cands) {
			return t, "", false
		}
		return t, ReasonDuplicate, true
	}
	base := bases[0]
	candDigest := bodyDigest(cand.fset, file.src, fd)
	if candDigest == base.digest {
		return t, "", false
	}
	switch {
	case fd.Recv != nil:
		return t, ReasonMethod, true
	case fd.Type.TypeParams != nil || base.decl.Type.TypeParams != nil:
		return t, ReasonGeneric, true
	case fd.Body == nil || base.decl.Body == nil:
		return t, ReasonNoBody, true
	case file.constrained || base.file.constrained:
		return t, ReasonConstrained, true
	case cand.cgo || pair.base.cgo:
		return t, ReasonCgo, true
	case cand.multipleNames || pair.base.multipleNames:
		return t, ReasonPackageClause, true
	case cand.name != pair.base.name:
		return t, ReasonPackageName, true
	case cand.shadowed || pair.base.shadowed:
		return t, ReasonShadowed, true
	}
	candParams, candResults, candSig := signature(cand.fset, fd.Type)
	_, _, baseSig := signature(pair.base.fset, base.decl.Type)
	t.Signature = candSig
	if candSig != baseSig {
		return t, ReasonSignature, true
	}
	for _, p := range candParams {
		param, reason := generatedParam(cand.fset, p, cand, pair.base)
		if reason != "" {
			return t, reason, true
		}
		t.Params = append(t.Params, param)
	}
	t.Results = len(candResults)
	return t, "", true
}

// sameDigests reports whether two sets of declarations have the same body
// digests, counted with multiplicity.
func sameDigests(a, b []*funcDecl) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, d := range a {
		count[d.digest]++
	}
	for _, d := range b {
		count[d.digest]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}

// symbolOf qualifies key with the slash package directory, or with the
// package name for the repository root (unqualified when the root package has
// no name, because all its files are constrained).
func symbolOf(dir, name, key string) string {
	switch {
	case dir != ".":
		return dir + "." + key
	case name != "":
		return name + "." + key
	}
	return key
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return receiverName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func receiverName(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.StarExpr:
		return receiverName(n.X)
	case *ast.IndexExpr:
		return receiverName(n.X)
	case *ast.IndexListExpr:
		return receiverName(n.X)
	case *ast.ParenExpr:
		return receiverName(n.X)
	}
	return "?"
}

// bodyDigest is the token digest of the function body source: comments and
// layout are ignored; literals, operators and statement separators are kept.
// A declaration without a body digests to "".
//
// The body is first reduced to its tokens, one space apart on a single line
// (canonicalTokens), so that a body on one line and the same body over several
// lines give the same digest.
func bodyDigest(fset *token.FileSet, src []byte, fd *ast.FuncDecl) string {
	if fd.Body == nil {
		return ""
	}
	tf := fset.File(fd.Body.Pos())
	if tf == nil {
		return ""
	}
	start, end := tf.Offset(fd.Body.Pos()), tf.Offset(fd.Body.End())
	if start < 0 || end > len(src) || start > end {
		return ""
	}
	return linter.TokenDigest([]byte(canonicalTokens(src[start:end])))
}

// canonicalTokens returns the tokens of src separated by single spaces, with
// comments dropped. Every semicolon, explicit or inserted by the scanner at a
// line end, is written as ";" because it separates statements: `return` and
// `g()` on two lines differ from `return g()`. A semicolon directly before a
// closing brace or parenthesis separates nothing and is dropped, so
// `{ return x }` and the same body over three lines agree.
func canonicalTokens(src []byte) string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile("", -1, len(src)), src, nil, 0)
	var tokens []string
	semicolons := 0 // semicolons seen since the last other token
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON {
			semicolons++
			continue
		}
		if tok != token.RBRACE && tok != token.RPAREN {
			for ; semicolons > 0; semicolons-- {
				tokens = append(tokens, ";")
			}
		}
		semicolons = 0
		if lit == "" {
			lit = tok.String()
		}
		tokens = append(tokens, lit)
	}
	return strings.Join(tokens, " ")
}

// paramText is one expanded parameter: `a, b int` gives two.
type paramText struct {
	expr     ast.Expr // the element type for a variadic parameter
	text     string   // printed type, with "..." for a variadic parameter
	variadic bool
}

// signature returns the expanded parameters, the expanded result types and the
// types-only signature text, e.g. func(int64, ...string) (Cents, error).
func signature(fset *token.FileSet, ft *ast.FuncType) ([]paramText, []string, string) {
	var params []paramText
	if ft.Params != nil {
		for _, field := range ft.Params.List {
			expr, variadic := field.Type, false
			if e, ok := expr.(*ast.Ellipsis); ok {
				expr, variadic = e.Elt, true
			}
			text := printNode(fset, expr)
			if variadic {
				text = "..." + text
			}
			for i := 0; i < max(1, len(field.Names)); i++ {
				params = append(params, paramText{expr: expr, text: text, variadic: variadic})
			}
		}
	}
	var results []string
	if ft.Results != nil {
		for _, field := range ft.Results.List {
			text := printNode(fset, field.Type)
			for i := 0; i < max(1, len(field.Names)); i++ {
				results = append(results, text)
			}
		}
	}
	texts := make([]string, len(params))
	for i, p := range params {
		texts[i] = p.text
	}
	sig := "func(" + strings.Join(texts, ", ") + ")"
	switch len(results) {
	case 0:
	case 1:
		sig += " " + results[0]
	default:
		sig += " (" + strings.Join(results, ", ") + ")"
	}
	return params, results, sig
}

// generatedParam maps a parameter type to a generated Param, or returns a
// fixed skip reason.
func generatedParam(fset *token.FileSet, p paramText, cand, base *goPackage) (Param, string) {
	kind := ParamScalar
	expr := p.expr
	length := 0
	if p.variadic {
		kind = ParamVariadic
	} else if arr, ok := expr.(*ast.ArrayType); ok {
		if arr.Len == nil {
			kind = ParamSlice
		} else {
			lit, ok := arr.Len.(*ast.BasicLit)
			if !ok || lit.Kind != token.INT || !decimalLiteral(lit.Value) {
				return Param{}, reasonParamType(p.text)
			}
			n, err := strconv.Atoi(lit.Value)
			if err != nil || n < 0 || n > MaxArrayLen {
				return Param{}, reasonParamType(p.text)
			}
			kind, length = ParamArray, n
		}
		expr = arr.Elt
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return Param{}, reasonParamType(p.text)
	}
	if basicTypes[ident.Name] {
		return Param{Kind: kind, Basic: ident.Name, Len: length}, ""
	}
	nc, inCandidate := cand.named[ident.Name]
	nb, inBase := base.named[ident.Name]
	switch {
	case inCandidate && inBase && nc.spec == nb.spec:
		return Param{Kind: kind, Basic: nc.basic, Named: ident.Name, Len: length}, ""
	case inCandidate || inBase:
		return Param{}, reasonNamedDiffers(ident.Name)
	}
	return Param{}, reasonParamType(p.text)
}

func decimalLiteral(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func printNode(fset *token.FileSet, node any) string {
	var b bytes.Buffer
	if err := format.Node(&b, fset, node); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func shortText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// priority is the highest severity rank of the new-side signals of the
// target's file whose lines overlap the function.
func priority(t Target, signals []model.Signal) int {
	best := 0
	for _, s := range signals {
		if s.Path != t.Path || s.Side != "new" {
			continue
		}
		end := s.EndLine
		if end < s.Line {
			end = s.Line
		}
		if end < t.Line || s.Line > t.EndLine {
			continue
		}
		if r := severityRank(s.Severity); r > best {
			best = r
		}
	}
	return best
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// budget orders the eligible targets, applies max_packages and max_functions,
// and assigns each planned target its number of inputs.
func budget(eligible []Target, limits Limits, s *selection) Plan {
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.Exported != b.Exported {
			return a.Exported
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
	var order []string
	chosen := map[string]bool{}
	for _, t := range eligible {
		if _, seen := chosen[t.Dir]; seen {
			continue
		}
		chosen[t.Dir] = len(order) < limits.MaxPackages
		order = append(order, t.Dir)
	}
	byDir := map[string][]Target{}
	plan := Plan{}
	taken := 0
	for _, t := range eligible {
		switch {
		case !chosen[t.Dir]:
			plan.Skipped = append(plan.Skipped, model.FuzzSkip{Path: t.Path, Line: t.Line, Symbol: t.Symbol, Reason: ReasonBudgetPackages})
		case taken >= limits.MaxFunctions:
			plan.Skipped = append(plan.Skipped, model.FuzzSkip{Path: t.Path, Line: t.Line, Symbol: t.Symbol, Reason: ReasonBudgetFunctions})
		default:
			byDir[t.Dir] = append(byDir[t.Dir], t)
			taken++
		}
	}
	plan.BudgetSkipped = len(plan.Skipped)
	for _, dir := range order {
		targets := byDir[dir]
		if len(targets) == 0 {
			continue
		}
		// Inputs is what the corpus actually yields within the share: one for
		// a function without parameters, two for func(bool), and so on.
		share := max(1, MaxPackageInputs/len(targets))
		for i := range targets {
			targets[i].Inputs = len(Corpus(targets[i], min(limits.MaxInputs, share)))
		}
		pair := s.packages[dir]
		idents := map[string]bool{}
		for name := range pair.candidate.idents {
			idents[name] = true
		}
		for name := range pair.base.idents {
			idents[name] = true
		}
		plan.Packages = append(plan.Packages, PackagePlan{Dir: dir, Name: pair.candidate.name, Targets: targets, Idents: idents})
	}
	return plan
}

// goFile is one parsed file of a package directory.
type goFile struct {
	name, path  string // base name; slash path relative to the snapshot root
	src         []byte
	syntax      *ast.File
	test        bool
	constrained bool // a build constraint line, or excluded on linux/amd64 or linux/arm64 by its name
}

type namedBasic struct{ basic, spec string }

type funcDecl struct {
	decl   *ast.FuncDecl
	file   *goFile
	digest string
}

// goPackage is what selection knows about one package directory of one
// revision.
type goPackage struct {
	exists        bool
	problem       string // skip reason when the directory could not be read or parsed
	fset          *token.FileSet
	files         []*goFile
	name          string // package clause of the unconstrained non-test files
	multipleNames bool
	idents        map[string]bool
	cgo           bool
	shadowed      bool
	named         map[string]namedBasic // unconstrained non-test named types with a basic underlying type
	funcs         map[string][]*funcDecl
}

func (p *goPackage) file(slashPath string) *goFile {
	for _, f := range p.files {
		if f.path == slashPath && !f.test {
			return f
		}
	}
	return nil
}

// loadPackage reads and parses the .go files of one directory of a snapshot
// with bounded reads. A missing directory is reported through exists; any read
// or parse problem through problem, a skip reason that names files relative to
// the snapshot and never quotes an operating system error (which would carry
// the host path of the snapshot).
func loadPackage(root, dir string) *goPackage {
	p := &goPackage{fset: token.NewFileSet(), idents: map[string]bool{}, named: map[string]namedBasic{}, funcs: map[string][]*funcDecl{}}
	abs := filepath.Join(root, filepath.FromSlash(dir))
	fail := func(reason string) *goPackage {
		p.exists, p.problem = true, reason
		return p
	}
	if dir != "." {
		cursor := root
		for _, part := range strings.Split(dir, "/") {
			cursor = filepath.Join(cursor, part)
			info, err := os.Lstat(cursor)
			if os.IsNotExist(err) {
				return p
			}
			if err != nil {
				return fail(reasonUnreadable("directory " + dir + " could not be read"))
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fail(reasonUnreadable(dir + " is not a plain directory"))
			}
		}
	}
	entries, err := os.ReadDir(abs)
	if os.IsNotExist(err) {
		return p
	}
	if err != nil {
		return fail(reasonUnreadable("directory " + dir + " could not be read"))
	}
	p.exists = true
	total := 0
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		count++
		if count > maxPackageFiles {
			return fail(reasonUnreadable(fmt.Sprintf("more than %d Go files", maxPackageFiles)))
		}
		full := filepath.Join(abs, name)
		info, err := os.Lstat(full)
		if err != nil {
			return fail(reasonUnreadable(name + " could not be read"))
		}
		if !info.Mode().IsRegular() {
			return fail(reasonUnreadable(name + " is not a regular file"))
		}
		if info.Size() > maxSourceBytes {
			return fail(reasonUnreadable(fmt.Sprintf("%s exceeds %d bytes", name, maxSourceBytes)))
		}
		src, err := readLimited(full, maxSourceBytes)
		if errors.Is(err, errSourceTooLarge) {
			return fail(reasonUnreadable(fmt.Sprintf("%s exceeds %d bytes", name, maxSourceBytes)))
		}
		if err != nil {
			return fail(reasonUnreadable(name + " could not be read"))
		}
		total += len(src)
		if total > maxPackageBytes {
			return fail(reasonUnreadable(fmt.Sprintf("the directory exceeds %d bytes of Go source", maxPackageBytes)))
		}
		slashPath := name
		if dir != "." {
			slashPath = dir + "/" + name
		}
		syntax, err := parser.ParseFile(p.fset, slashPath, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return fail(reasonUnparsable(name))
		}
		f := &goFile{name: name, path: slashPath, src: src, syntax: syntax, test: strings.HasSuffix(name, "_test.go")}
		f.constrained = constraintLine(p.fset, syntax, src) || !buildsOnLinux(dir, name, src)
		p.files = append(p.files, f)
	}
	p.index()
	return p
}

var errSourceTooLarge = errors.New("source file exceeds the size bound")

func readLimited(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errSourceTooLarge
	}
	return data, nil
}

// constraintLine reports a //go:build or // +build line before the package
// clause. Such files are skipped conservatively: the sandbox's tags are not
// known on the host.
func constraintLine(fset *token.FileSet, syntax *ast.File, src []byte) bool {
	tf := fset.File(syntax.Package)
	if tf == nil {
		return true
	}
	header := src[:tf.Offset(syntax.Package)]
	for _, line := range strings.Split(string(header), "\n") {
		line = strings.TrimSpace(line)
		if constraint.IsGoBuild(line) || constraint.IsPlusBuild(line) {
			return true
		}
	}
	return false
}

// buildsOnLinux evaluates the file name constraints (_windows.go, _arm64.go,
// ...) for linux on both amd64 and arm64, the sandbox platforms, with
// in-memory file access only.
func buildsOnLinux(dir, name string, src []byte) bool {
	for _, arch := range []string{"amd64", "arm64"} {
		ctx := build.Context{
			GOOS: "linux", GOARCH: arch, Compiler: "gc", CgoEnabled: true,
			JoinPath: path.Join,
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil },
		}
		ok, err := ctx.MatchFile(dir, name)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// index derives the package facts from the parsed files.
func (p *goPackage) index() {
	names := map[string]bool{}
	for _, f := range p.files {
		if !f.test && !f.constrained {
			names[f.syntax.Name.Name] = true
		}
	}
	if len(names) > 1 {
		p.multipleNames = true
	}
	for name := range names {
		if p.name == "" || name < p.name {
			p.name = name
		}
	}
	for _, f := range p.files {
		internal := f.syntax.Name.Name == p.name
		for _, imp := range f.syntax.Imports {
			value, _ := strconv.Unquote(imp.Path.Value)
			if value == "C" && !f.test {
				p.cgo = true
			}
			if imp.Name != nil {
				p.idents[imp.Name.Name] = true
			} else {
				last := path.Base(value)
				p.idents[last] = true
				if dot := strings.IndexByte(last, '.'); dot > 0 {
					p.idents[last[:dot]] = true
				}
			}
		}
		for _, decl := range f.syntax.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.declare(d.Name.Name, internal)
				}
				if !f.test {
					key := funcKey(d)
					p.funcs[key] = append(p.funcs[key], &funcDecl{decl: d, file: f, digest: bodyDigest(p.fset, f.src, d)})
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						p.declare(sp.Name.Name, internal)
						if f.test || f.constrained || !internal || sp.TypeParams != nil {
							continue
						}
						if ident, ok := sp.Type.(*ast.Ident); ok && basicTypes[ident.Name] {
							p.named[sp.Name.Name] = namedBasic{basic: ident.Name, spec: printNode(p.fset, sp)}
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							p.declare(n.Name, internal)
						}
					}
				}
			}
		}
	}
}

func (p *goPackage) declare(name string, internal bool) {
	p.idents[name] = true
	if internal && predeclared[name] {
		p.shadowed = true
	}
}
