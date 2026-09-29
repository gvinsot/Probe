// Package linter extracts deterministic review signals. Heuristics are review
// prompts, not vulnerability findings or claims about test coverage.
package linter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
)

// rule is a line heuristic. subject names what its pattern matches; the summary
// adds whether the matching lines of a file were added, removed or both.
type rule struct {
	kind, severity, subject string
	pattern                 *regexp.Regexp
	removedOnly             bool
}

var rules = []rule{
	{"auth_change", "high", "Authentication or authorization logic", regexp.MustCompile(`(?i)\b(authoriz\w*|authenticat\w*|checkPermission\w*|hasPermission\w*|requireRole\w*|jwt|bcrypt|argon2|csrf|cors)\b`), false},
	{"network_change", "medium", "Network interaction", regexp.MustCompile(`(?i)\b(http\.(Get|Post|Do|NewRequest|ListenAndServe)|https?://|fetch\s*\(|axios\.|requests\.|urllib\.|net\.Dial|grpc\.|client\.(Do|Send)\s*\()`), false},
	{"database_write", "high", "Possible database mutation or transaction", regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|DROP\s+TABLE|ALTER\s+TABLE|TRUNCATE\s+TABLE|\w+\.(Exec|ExecContext|Save|Create|Delete|Update|Transaction|Commit|Rollback)\s*\()`), false},
	{"validation_removed", "high", "Possible input validation", regexp.MustCompile(`(?i)\b(validate\w*|assert\w*|sanitize\w*|checkInput\w*|checkRange\w*|isValid\w*)\s*\(|\b(throw\s+new\s+(Error|TypeError|RangeError)|raise\s+(ValueError|ValidationError))\b`), true},
	{"error_handling_change", "medium", "Error handling", regexp.MustCompile(`\b(if\s+err\s*!=\s*nil|errors\.(Is|As|New)|fmt\.Errorf|catch\s*\(|except\b|panic\s*\(|recover\s*\()`), false},
	{"type_suppression", "high", "Type or safety checking suppression", regexp.MustCompile(`(?i)(@ts-(ignore|nocheck)|\bas\s+any\b|\bunsafe\.|\bunsafe\s*\{|#\s*type:\s*ignore|noqa|nolint|eslint-disable)`), false},
	{"todo_added", "low", "Unresolved work marker", regexp.MustCompile(`\b(TODO|FIXME|HACK|XXX)\b`), false},
	{"dynamic_execution", "high", "Dynamic code or process execution", regexp.MustCompile(`\b(eval\s*\(|exec\.(Command|CommandContext)\s*\(|subprocess\.|os\.system\s*\(|child_process|dangerouslySetInnerHTML|innerHTML\s*=)`), false},
}

// Analyze limits concurrent Git readers and returns stable, sorted signal IDs.
// Go declarations are compared structurally; other language signals use text.
func Analyze(ctx context.Context, repo *gitrepo.Repository, change model.Change, sensitivePaths []string) ([]model.Signal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	patterns := make([]sensitivePattern, 0, len(sensitivePaths))
	for _, glob := range sensitivePaths {
		p, err := compileGlob(glob)
		if err != nil {
			return nil, fmt.Errorf("sensitive path: %w", err)
		}
		patterns = append(patterns, sensitivePattern{glob, p})
	}
	results := make([][]model.Signal, len(change.Files))
	testDirs, testStems := map[string]bool{}, map[string]bool{}
	for _, file := range change.Files {
		if file.Status != "D" && isTest(file.Path) {
			testDirs[path.Dir(file.Path)] = true
			testStems[testStem(file.Path)] = true
		}
	}
	jobs := make(chan int)
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > len(change.Files) {
		workers = len(change.Files)
	}
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				file := change.Files[i]
				hasTestChange := testDirs[path.Dir(file.Path)] || testStems[strings.TrimSuffix(path.Base(file.Path), path.Ext(file.Path))]
				results[i] = analyzeFile(ctx, repo, change, file, patterns, hasTestChange)
			}
		}()
	}
	for i := range change.Files {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []model.Signal{}
	for _, signals := range results {
		out = append(out, signals...)
	}
	return finish(out), nil
}

// Merge combines signals produced elsewhere, such as from a recorded coverage
// run, with the deterministic signals of this package. Ordering and the
// content-derived ID recipe stay in one place, so an unchanged signal keeps its
// identifier no matter what it is merged with.
func Merge(existing, extra []model.Signal) []model.Signal {
	return finish(append(append([]model.Signal{}, existing...), extra...))
}

func finish(out []model.Signal) []model.Signal {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.Evidence < b.Evidence
	})
	for i := range out {
		s := &out[i]
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", s.Kind, s.Path, s.Line, s.Side, s.Symbol, s.Evidence)))
		s.ID = fmt.Sprintf("sig-%x", digest[:8])
	}
	return out
}

// sensitivePattern is a configured sensitive_paths glob and its compiled form;
// evidence names the glob as the policy wrote it.
type sensitivePattern struct {
	glob string
	re   *regexp.Regexp
}

func analyzeFile(ctx context.Context, repo *gitrepo.Repository, change model.Change, f model.ChangedFile, patterns []sensitivePattern, hasTestChange bool) []model.Signal {
	var signals []model.Signal
	line, side := firstChangedLine(f)
	// add records a file-level signal: the first changed line only places it.
	add := func(kind, severity, summary, evidence string) {
		signals = append(signals, model.Signal{Kind: kind, Path: f.Path, Line: line, Side: side, Scope: model.SignalScopeFile, Severity: severity, Summary: summary, Evidence: evidence})
	}
	for _, p := range patterns {
		if p.re.MatchString(f.Path) || f.OldPath != "" && p.re.MatchString(f.OldPath) {
			add("sensitive_path", "high", fileChange(f, "sensitive file"), "Path matches configured pattern "+p.glob)
			break
		}
	}
	if f.Binary {
		add("binary_change", "medium", "Binary content requires separate inspection", "Git reports a binary change; text analysis is unavailable")
		return signals
	}
	signals = append(signals, securitySignals(f)...)
	if isTest(f.Path) {
		signals = append(signals, testWeakeningSignals(f)...)
	}
	if isDependency(f.Path) {
		add("dependency_change", "medium", fileChange(f, "dependency manifest or lockfile"), "Review dependency versions, provenance, and transitive effects; a changed manifest does not prove a dependency was added")
	}
	lowerPath := strings.ToLower(f.Path)
	if strings.Contains(lowerPath, "migration") || strings.HasSuffix(lowerPath, ".sql") {
		add("migration_change", "high", fileChange(f, "database schema or migration file"), "Path-based heuristic; inspect reversibility, locking, and compatibility")
	}
	if strings.HasPrefix(lowerPath, ".github/workflows/") || strings.Contains(lowerPath, "dockerfile") || strings.HasSuffix(lowerPath, ".tf") {
		add("infrastructure_change", "high", fileChange(f, "deployment or execution configuration file"), "Path-based heuristic; inspect permissions and deployment effects")
	}
	if f.Status == "D" {
		add("file_deleted", "medium", "Tracked file removed", "A deleted file may remove behavior or checks; inspect its callers")
	}
	if f.Status == "T" {
		add("file_type_change", "high", "Tracked file type changed", "Git reports a type change, such as a regular file becoming a symlink")
	}
	if f.Additions+f.Deletions > 400 {
		add("large_change", "medium", "Large file-level change", fmt.Sprintf("%d added and %d deleted lines; size is a review-cost signal, not a defect probability", f.Additions, f.Deletions))
	}
	if isSource(f.Path) && !isTest(f.Path) && f.Status != "D" && f.Additions+f.Deletions > 0 {
		if !hasTestChange {
			add("no_test_change", "low", "No nearby test file changed", "No changed test file shares this source directory or filename stem. Existing test coverage has not been measured")
		}
	}
	if !isSource(f.Path) && !isDependency(f.Path) {
		return signals
	}
	// A line heuristic reports its first matching line once per file; sides
	// collects every side it matched so its summary says added, removed or both.
	type match struct {
		index int
		sides map[string]bool
	}
	matched := map[string]*match{}
	record := func(kind, severity, summary, evidence string, d model.DiffLine) {
		if m := matched[kind]; m != nil {
			m.sides[d.Kind] = true
			return
		}
		s := model.Signal{Kind: kind, Path: f.Path, Severity: severity, Summary: summary, Evidence: evidence + short(d.Content), Side: "new", Line: d.NewLine}
		if d.Kind == "delete" {
			s.Side = "old"
			s.Line = d.OldLine
		}
		matched[kind] = &match{index: len(signals), sides: map[string]bool{d.Kind: true}}
		signals = append(signals, s)
	}
	known := func(kind string, d model.DiffLine) bool { return matched[kind] != nil && matched[kind].sides[d.Kind] }
	addedBranches, removedBranches := 0, 0
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			if d.Kind != "add" && d.Kind != "delete" {
				continue
			}
			if branchPattern.MatchString(d.Content) {
				if d.Kind == "add" {
					addedBranches++
				} else {
					removedBranches++
				}
			}
			for _, r := range rules {
				if known(r.kind, d) || r.removedOnly && d.Kind != "delete" || r.kind == "todo_added" && d.Kind != "add" || !r.pattern.MatchString(d.Content) {
					continue
				}
				record(r.kind, r.severity, r.subject, "Text heuristic (not a verified defect): ", d)
			}
			if path.Ext(f.Path) != ".go" && !known("public_api_change", d) && publicPattern.MatchString(d.Content) {
				record("public_api_change", "medium", "Possible public declaration", "Text heuristic; no language-specific signature or compatibility analysis: ", d)
			}
		}
	}
	for _, m := range matched {
		signals[m.index].Summary += " " + linesVerb(m.sides["add"], m.sides["delete"])
	}
	if addedBranches-removedBranches >= 5 {
		add("branch_growth", "medium", "More branching constructs appear in the diff", fmt.Sprintf("Text heuristic: %d added vs %d removed lines containing branch constructs; this is not cyclomatic complexity", addedBranches, removedBranches))
	}
	if path.Ext(f.Path) == ".go" {
		signals = append(signals, analyzeGo(ctx, repo, change, f)...)
	}
	return signals
}

var branchPattern = regexp.MustCompile(`\b(if|for|while|case|catch|except)\b|&&|\|\|`)
var publicPattern = regexp.MustCompile(`^\s*(export\s+(default\s+)?(async\s+)?(function|class|interface|type|const|let|enum)|public\s+|pub\s+(fn|struct|enum|trait|type|mod))\b`)

// linesVerb says what happened to the lines a heuristic matched.
func linesVerb(added, removed bool) string {
	switch {
	case added && removed:
		return "changed"
	case removed:
		return "removed"
	}
	return "added"
}

// fileChange summarizes a file-level signal about noun, such as "sensitive
// file", with what the diff does to that file, so that a file which only gains
// lines does not read as modified.
func fileChange(f model.ChangedFile, noun string) string {
	subject := strings.ToUpper(noun[:1]) + noun[1:]
	switch {
	case f.Status == "A":
		return subject + " added"
	case f.Status == "D":
		return subject + " deleted"
	case f.Status == "T":
		return subject + " type changed"
	}
	if f.Status == "R" || f.Status == "C" {
		noun = map[string]string{"R": "renamed ", "C": "copied "}[f.Status] + noun
	}
	switch {
	case f.Additions > 0 && f.Deletions > 0:
		return "Lines added and removed in a " + noun
	case f.Additions > 0:
		return "Lines added to a " + noun
	case f.Deletions > 0:
		return "Lines removed from a " + noun
	case f.Status == "R":
		return subject + " renamed"
	case f.Status == "C":
		return subject + " copied"
	}
	return subject + " changed"
}

func firstChangedLine(f model.ChangedFile) (int, string) {
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			if d.Kind == "add" {
				return d.NewLine, "new"
			}
			if d.Kind == "delete" {
				return d.OldLine, "old"
			}
		}
	}
	if f.Status == "D" {
		return 1, "old"
	}
	return 1, "new"
}
func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
func isSource(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt", ".kts", ".cs", ".c", ".h", ".cpp", ".hpp", ".rb", ".php", ".swift", ".sql", ".sh":
		return true
	}
	return false
}
func isDependency(p string) bool {
	switch strings.ToLower(path.Base(p)) {
	case "go.mod", "go.sum", "go.work", "go.work.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb", "cargo.toml", "cargo.lock", "requirements.txt", "pyproject.toml", "poetry.lock", "uv.lock", "pipfile", "pipfile.lock", "gemfile", "gemfile.lock", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "composer.lock":
		return true
	}
	return strings.HasSuffix(p, ".csproj")
}
func isTest(p string) bool {
	p = strings.ToLower(p)
	b := path.Base(p)
	return strings.HasSuffix(b, "_test.go") || strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") || strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py") || strings.HasSuffix(b, "test.java") || strings.Contains("/"+p, "/tests/") || strings.Contains("/"+p, "/__tests__/")
}
func testStem(test string) string {
	t := strings.TrimSuffix(path.Base(test), path.Ext(test))
	t = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(t, "_test"), ".test"), ".spec"), "test_")
	return t
}

// compileGlob gives ** zero-or-more-directory semantics, including **/auth/**.
func compileGlob(glob string) (*regexp.Regexp, error) {
	if glob == "" || strings.ContainsAny(glob, "\\\x00") || strings.HasPrefix(glob, "/") {
		return nil, fmt.Errorf("invalid relative glob %q", glob)
	}
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

// declaration is an exported declaration's signature and the lines it spans; a
// function spans its signature only, since its body is not part of it.
type declaration struct {
	signature string
	line, end int
}

type sensitiveFunction struct {
	kind, body string
	start, end int
}
type goAnalysis struct {
	declarations map[string]declaration
	functions    map[string]sensitiveFunction
}

var authFunctionName = regexp.MustCompile(`(?i)(authori[sz]|authenticat|authz|checkpermission|haspermission|requirepermission|checkrole|hasrole|requirerole|requireauth|verifytoken|validatetoken|verifypassword|checkpassword|hashpassword|resetpassword|verifysignature|login|logout|signin|signout|csrf)`)
var paymentFunctionName = regexp.MustCompile(`(?i)(payment|refund|charge|capture|withdraw|deposit|transferfunds|transfermoney|settlement|balance)`)

func functionRisk(name string) string {
	name = strings.ReplaceAll(name, "_", "")
	if authFunctionName.MatchString(name) {
		return "auth_change"
	}
	if paymentFunctionName.MatchString(name) {
		return "sensitive_function_change"
	}
	return ""
}

// bodyTokens ignores formatting and comments while retaining literal values and
// operators. It supplements the parsed AST; it does not infer program semantics.
func bodyTokens(body string) string {
	var s scanner.Scanner
	set := token.NewFileSet()
	s.Init(set.AddFile("", -1, len(body)), []byte(body), nil, 0)
	var out strings.Builder
	for {
		_, tok, literal := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON {
			literal = ""
		}
		out.WriteString(tok.String())
		out.WriteByte(0)
		out.WriteString(literal)
		out.WriteByte(0)
	}
	return out.String()
}

func goDeclarations(filename string, content []byte) (map[string]declaration, error) {
	analysis, err := parseGo(filename, content)
	return analysis.declarations, err
}

func parseGo(filename string, content []byte) (goAnalysis, error) {
	analysis := goAnalysis{declarations: map[string]declaration{}, functions: map[string]sensitiveFunction{}}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, content, parser.SkipObjectResolution)
	if err != nil {
		return analysis, err
	}
	out := analysis.declarations
	printNode := func(n any) string {
		var b bytes.Buffer
		if err := format.Node(&b, fset, n); err != nil {
			return "<unprintable>"
		}
		return b.String()
	}
	for _, d := range f.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			name := n.Name.Name
			if n.Recv != nil && len(n.Recv.List) > 0 {
				name = receiverName(n.Recv.List[0].Type) + "." + name
			}
			if kind := functionRisk(name); kind != "" && n.Body != nil {
				analysis.functions[name] = sensitiveFunction{kind: kind, body: bodyTokens(printNode(n.Body)), start: fset.Position(n.Body.Pos()).Line, end: fset.Position(n.Body.End()).Line}
			}
			if !ast.IsExported(n.Name.Name) {
				continue
			}
			n.Body = nil
			n.Doc = nil
			out[name] = declaration{f.Name.Name + ":" + printNode(n), fset.Position(n.Pos()).Line, fset.Position(n.End()).Line}
		case *ast.GenDecl:
			var previousType ast.Expr
			var previousValues []ast.Expr
			for index, s := range n.Specs {
				switch spec := s.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(spec.Name.Name) {
						out[spec.Name.Name] = declaration{f.Name.Name + ":" + printNode(spec), fset.Position(spec.Pos()).Line, fset.Position(spec.End()).Line}
					}
				case *ast.ValueSpec:
					if len(spec.Values) > 0 {
						previousValues = spec.Values
						previousType = spec.Type
					}
					for j, name := range spec.Names {
						if !ast.IsExported(name.Name) {
							continue
						}
						typeExpr := spec.Type
						values := spec.Values
						if n.Tok == token.CONST && len(values) == 0 {
							typeExpr = previousType
							values = previousValues
						}
						sig := f.Name.Name + ":" + n.Tok.String() + " " + name.Name
						if typeExpr != nil {
							sig += " " + printNode(typeExpr)
						}
						if len(values) > 0 && (n.Tok == token.CONST || typeExpr == nil) {
							k := j
							if k >= len(values) {
								k = 0
							}
							value := printNode(values[k])
							sig += " = " + value
							if n.Tok == token.CONST && strings.Contains(value, "iota") {
								sig += fmt.Sprintf(" [iota=%d]", index)
							}
						}
						out[name.Name] = declaration{sig, fset.Position(name.Pos()).Line, fset.Position(spec.End()).Line}
					}
				}
			}
		}
	}
	return analysis, nil
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
	}
	return "?"
}
func analyzeGo(ctx context.Context, repo *gitrepo.Repository, change model.Change, f model.ChangedFile) []model.Signal {
	beforeAnalysis, afterAnalysis := goAnalysis{}, goAnalysis{}
	var result []model.Signal
	read := func(commit, p, side string) (goAnalysis, bool) {
		content, err := repo.ReadFile(ctx, commit, p)
		var analysis goAnalysis
		if err == nil {
			analysis, err = parseGo(p, content)
		}
		if err != nil {
			result = append(result, model.Signal{Kind: "analysis_limited", Path: f.Path, Line: 1, Side: side, Scope: model.SignalScopeFile, Severity: "medium", Summary: "Go public declaration analysis incomplete", Evidence: short(err.Error())})
			return analysis, false
		}
		return analysis, true
	}
	var ok bool
	if f.Status != "A" {
		p := f.Path
		if f.OldPath != "" {
			p = f.OldPath
		}
		beforeAnalysis, ok = read(change.BaseCommit, p, "old")
		if !ok {
			return result
		}
	}
	if f.Status != "D" {
		afterAnalysis, ok = read(change.HeadCommit, f.Path, "new")
		if !ok {
			return result
		}
	}
	added, removed := changedLines(f)
	old, newDecls := beforeAnalysis.declarations, afterAnalysis.declarations
	for name, before := range old {
		after, exists := newDecls[name]
		if exists && before.signature == after.signature {
			continue
		}
		s := model.Signal{Kind: "public_api_change", Path: f.Path, Symbol: name, Severity: "high", Summary: "Exported Go declaration removed", Evidence: "Go AST declaration: " + short(before.signature)}
		if exists {
			s.Summary = "Exported Go declaration changed"
			s.Evidence = "Go AST before: " + short(before.signature) + "; after: " + short(after.signature)
		}
		s.Line, s.EndLine, s.Side = declarationAnchor(added, removed, before, after, exists)
		result = append(result, s)
	}
	for name, after := range newDecls {
		if _, exists := old[name]; !exists {
			result = append(result, model.Signal{Kind: "public_api_change", Path: f.Path, Line: after.line, Side: "new", Symbol: name, Severity: "medium", Summary: "Exported Go declaration added", Evidence: "Go AST declaration: " + short(after.signature)})
		}
	}
	if !isTest(f.Path) {
		result = append(result, sensitiveBodyChanges(f.Path, added, removed, beforeAnalysis.functions, afterAnalysis.functions)...)
	}
	return result
}

// changedLines returns the sorted added (new side) and deleted (old side) line
// numbers of f.
func changedLines(f model.ChangedFile) (added, removed []int) {
	for _, hunk := range f.Hunks {
		for _, line := range hunk.Lines {
			if line.Kind == "add" && line.NewLine > 0 {
				added = append(added, line.NewLine)
			}
			if line.Kind == "delete" && line.OldLine > 0 {
				removed = append(removed, line.OldLine)
			}
		}
	}
	sort.Ints(added)
	sort.Ints(removed)
	return added, removed
}

// declarationAnchor locates a changed or removed exported declaration on the
// changed lines inside it, so that a struct whose field changed points at the
// field rather than at its unchanged first line. A change the declaration's own
// lines do not show, such as an iota value shifted by an earlier constant, stays
// on its first line. EndLine is 0 for a single line.
func declarationAnchor(added, removed []int, before, after declaration, exists bool) (int, int, string) {
	if exists {
		if line, end := changedRange(added, after.line, after.end); line != 0 {
			return line, lineEnd(line, end), "new"
		}
		if line, end := changedRange(removed, before.line, before.end); line != 0 {
			return line, lineEnd(line, end), "old"
		}
		return after.line, 0, "new"
	}
	if line, end := changedRange(removed, before.line, before.end); line != 0 {
		return line, lineEnd(line, end), "old"
	}
	return before.line, 0, "old"
}

func lineEnd(line, end int) int {
	if end > line {
		return end
	}
	return 0
}

func sensitiveBodyChanges(path string, added, removed []int, before, after map[string]sensitiveFunction) []model.Signal {
	var result []model.Signal
	seen := map[string]bool{}
	for name, function := range after {
		seen[name] = true
		previous, exists := before[name]
		if exists && function.body == previous.body {
			continue
		}
		line, end := changedRange(added, function.start, function.end)
		side := "new"
		if line == 0 && exists {
			line, end = changedRange(removed, previous.start, previous.end)
			side = "old"
		}
		if line != 0 {
			result = append(result, sensitiveBodySignal(path, name, function.kind, side, line, end))
		}
	}
	for name, function := range before {
		if seen[name] {
			continue
		}
		line, end := changedRange(removed, function.start, function.end)
		if line != 0 {
			result = append(result, sensitiveBodySignal(path, name, function.kind, "old", line, end))
		}
	}
	return result
}

// changedRange returns the first and last of the sorted lines that fall within
// [start, end], or 0, 0 when none does.
func changedRange(lines []int, start, end int) (int, int) {
	first := sort.SearchInts(lines, start)
	last := sort.SearchInts(lines, end+1)
	if first == last {
		return 0, 0
	}
	return lines[first], lines[last-1]
}

func sensitiveBodySignal(path, name, kind, side string, line, end int) model.Signal {
	domain := "Authentication or authorization"
	if kind == "sensitive_function_change" {
		domain = "Payment-sensitive"
	}
	return model.Signal{Kind: kind, Path: path, Line: line, EndLine: end, Side: side, Symbol: name, Severity: "high", Summary: domain + " function body changed", Evidence: "Go AST locates changed statements in " + name + "; the function or receiver name suggests sensitive behavior. Name/context heuristic, not a verified vulnerability; formatting and comment-only body changes are ignored"}
}

// MatchSensitive reports the first configured sensitive-path glob that p
// matches, with the same glob semantics as the sensitive_path signal. An
// invalid glob is an error.
func MatchSensitive(globs []string, p string) (string, bool, error) {
	for _, glob := range globs {
		re, err := compileGlob(glob)
		if err != nil {
			return "", false, fmt.Errorf("sensitive path: %w", err)
		}
		if re.MatchString(p) {
			return glob, true, nil
		}
	}
	return "", false, nil
}

// FunctionRisk classifies a function or "T.M" method name with the name
// heuristic of the auth_change and sensitive_function_change signals: it
// returns one of those kinds, or "" when the name suggests neither.
func FunctionRisk(name string) string { return functionRisk(name) }

// IsTestPath reports a test file by the name rules the linter uses to pair
// changed code with changed tests.
func IsTestPath(p string) bool { return isTest(p) }
