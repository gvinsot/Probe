package mutation

import (
	"bytes"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// allLines marks every line of src as added.
func allLines(src string) map[int]bool {
	added := map[int]bool{}
	for i := 1; i <= strings.Count(src, "\n")+1; i++ {
		added[i] = true
	}
	return added
}

type siteView struct {
	op, original, replacement string
	line, endLine, column     int
	symbol                    string
}

func views(sites []Site) []siteView {
	out := make([]siteView, len(sites))
	for i, s := range sites {
		out[i] = siteView{s.Operator, s.Original, s.Replacement, s.Line, s.EndLine, s.Column, s.Symbol}
	}
	return out
}

// discountSource is the design's Discount example. ErrNegative keeps the
// errors import used outside Discount, so dropping the error still compiles.
const discountSource = `package price

import "errors"

func Discount(total int) (int, error) {
	if total < 0 {
		return 0, ErrNegative
	}
	if total >= 100 {
		return total - 10, nil
	}
	return total, nil
}

// ErrNegative is returned for a negative total.
var ErrNegative = errors.New("negative total")
`

// Every operator yields an exact splice: its original text at the recorded
// line and column, a replacement that re-parses, and the same line count.
func TestOperatorsProduceExactSplices(t *testing.T) {
	src := `package p

import "errors"

type T struct{ n int }

func (t *T) Check(a, b int, ok bool) (bool, error) {
	if a == b && ok {
		return true, errors.New("same")
	}
	if a != b || a*b > 7 {
		return false, nil
	}
	return a/b <= 3, nil
}

var errOther = errors.New("other")
`
	sites, capped, skip := FileSites("p/p.go", []byte(src), allLines(src))
	if capped || skip != "" {
		t.Fatalf("capped %v skip %q", capped, skip)
	}
	want := []siteView{
		{OpNegateCondition, "a == b && ok", "!(a == b && ok)", 8, 8, 5, "T.Check"},
		{OpNegateComparison, "==", "!=", 8, 8, 7, "T.Check"},
		{OpSwapLogical, "&&", "||", 8, 8, 12, "T.Check"},
		{OpDropError, `errors.New("same")`, "nil", 9, 9, 16, "T.Check"},
		{OpFlipBoolean, "true", "false", 9, 9, 10, "T.Check"},
		{OpNegateCondition, "a != b || a*b > 7", "!(a != b || a*b > 7)", 11, 11, 5, "T.Check"},
		{OpBoundary, ">", ">=", 11, 11, 19, "T.Check"},
		{OpNegateComparison, "!=", "==", 11, 11, 7, "T.Check"},
		{OpSwapLogical, "||", "&&", 11, 11, 12, "T.Check"},
		{OpIncrementConstant, "7", "(7+1)", 11, 11, 21, "T.Check"},
		{OpSwapArithmetic, "*", "/", 11, 11, 16, "T.Check"},
		{OpFlipBoolean, "false", "true", 12, 12, 10, "T.Check"},
		{OpBoundary, "<=", "<", 14, 14, 13, "T.Check"},
		{OpIncrementConstant, "3", "(3+1)", 14, 14, 16, "T.Check"},
		{OpSwapArithmetic, "/", "*", 14, 14, 10, "T.Check"},
	}
	if got := views(sites); !reflect.DeepEqual(got, want) {
		t.Fatalf("sites:\n got %+v\nwant %+v", got, want)
	}
	for _, s := range sites {
		mutated, err := s.Apply([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", s.Operator, err)
		}
		if bytes.Equal(mutated, []byte(src)) {
			t.Fatalf("%s: mutant equals the original", s.Operator)
		}
		if bytes.Count(mutated, []byte("\n")) != strings.Count(src, "\n") {
			t.Fatalf("%s changed the line count", s.Operator)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), "p.go", mutated, 0); err != nil {
			t.Fatalf("%s: %v", s.Operator, err)
		}
		if !strings.Contains(string(mutated), s.Replacement) {
			t.Fatalf("%s: replacement missing", s.Operator)
		}
	}
}

// The design's Discount example has exactly eight sites on its added lines.
func TestDiscountSites(t *testing.T) {
	sites, _, skip := FileSites("price/price.go", []byte(discountSource), allLines(discountSource))
	if skip != "" {
		t.Fatal(skip)
	}
	got := views(sites)
	want := []siteView{
		{OpNegateCondition, "total < 0", "!(total < 0)", 6, 6, 5, "Discount"},
		{OpBoundary, "<", "<=", 6, 6, 11, "Discount"},
		{OpIncrementConstant, "0", "(0+1)", 6, 6, 13, "Discount"},
		{OpDropError, "ErrNegative", "nil", 7, 7, 13, "Discount"},
		{OpNegateCondition, "total >= 100", "!(total >= 100)", 9, 9, 5, "Discount"},
		{OpBoundary, ">=", ">", 9, 9, 11, "Discount"},
		{OpIncrementConstant, "100", "(100+1)", 9, 9, 14, "Discount"},
		{OpSwapArithmetic, "-", "+", 10, 10, 16, "Discount"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sites:\n got %+v\nwant %+v", got, want)
	}
}

// Context and removed lines give no site, and a span covering several lines
// needs every one of them to be added.
func TestSitesOnlyOnAddedLines(t *testing.T) {
	src := `package p

func F(a, b int) bool {
	if a > 1 &&
		b > 2 {
		return true
	}
	return a < b
}
`
	sites, _, _ := FileSites("p.go", []byte(src), map[int]bool{5: true, 8: true})
	for _, s := range sites {
		if s.Line != 5 && s.Line != 8 {
			t.Fatalf("site on a line that is not added: %+v", s)
		}
		if s.Operator == OpNegateCondition || s.Operator == OpSwapLogical {
			t.Fatalf("a span touching line 4 was generated: %+v", s)
		}
	}
	// Line 5: the boundary of > and the increment of 2; line 8: the boundary of <.
	if len(sites) != 3 {
		t.Fatalf("sites %+v, want 3", views(sites))
	}
	sites, _, _ = FileSites("p.go", []byte(src), map[int]bool{4: true, 5: true})
	found := false
	for _, s := range sites {
		if s.Operator == OpNegateCondition {
			found = true
			if s.Line != 4 || s.EndLine != 5 {
				t.Fatalf("multi-line condition span %+v", s)
			}
			mutated, err := s.Apply([]byte(src))
			if err != nil || bytes.Count(mutated, []byte("\n")) != strings.Count(src, "\n") {
				t.Fatalf("multi-line apply: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("a condition whose every line is added was not mutated")
	}
	if sites, _, _ := FileSites("p.go", []byte(src), map[int]bool{}); len(sites) != 0 {
		t.Fatalf("sites without added lines: %+v", views(sites))
	}
}

// drop_error applies only to a complete return of a function whose last
// result is error, never to nil, and the innermost function literal decides.
func TestDropErrorOnlyForErrorResult(t *testing.T) {
	src := `package p

import "errors"

func A() error { return errors.New("a") }
func B() error { return nil }
func C() (err error) { return }
func D() (int, error) { return pair() }
func E() (int, string) { return 1, "x" }
func F() error {
	g := func() int { return len("x") }
	_ = g
	h := func() error { return errors.New("h") }
	return h()
}
func pair() (int, error) { return 0, nil }
func G() (a, b int, err error) { return 1, 2, errors.New("g") }
`
	sites, _, _ := FileSites("p.go", []byte(src), allLines(src))
	var got []string
	for _, s := range sites {
		if s.Operator == OpDropError {
			got = append(got, s.Symbol+":"+s.Original)
		}
	}
	want := []string{`A:errors.New("a")`, `F:errors.New("h")`, `F:h()`, `G:errors.New("g")`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drop_error sites %q, want %q", got, want)
	}
}

// A dropped error that spans several lines (a gofmt'd fmt.Errorf call, a
// composite literal, a raw string) is replaced by nil plus one newline per
// newline of the span: Apply accepts it, the return ends on its first line,
// and every other line of the file keeps its number and content.
func TestDropErrorSpanningLines(t *testing.T) {
	src := "package load\n" + // 1
		"\n" + // 2
		"import \"fmt\"\n" + // 3
		"\n" + // 4
		"type E struct{ Name string; N int }\n" + // 5
		"\n" + // 6
		"func (e *E) Error() string { return fmt.Sprint(e.Name) }\n" + // 7
		"\n" + // 8
		"func Parse(name string, n int) (int, error) {\n" + // 9
		"\tif n < 0 {\n" + // 10
		"\t\treturn 0, fmt.Errorf(\"parse %s: negative %d\",\n" + // 11
		"\t\t\tname, n) // why\n" + // 12
		"\t}\n" + // 13
		"\tif n == 0 {\n" + // 14
		"\t\treturn 0, &E{\n" + // 15
		"\t\t\tName: name,\n" + // 16
		"\t\t\tN:    n,\n" + // 17
		"\t\t}\n" + // 18
		"\t}\n" + // 19
		"\tf := func() error { return fmt.Errorf(`raw\n" + // 20
		"text`) }\n" + // 21
		"\t_ = f\n" + // 22
		"\treturn n, nil\n" + // 23
		"}\n" // 24
	sites, capped, skip := FileSites("load/load.go", []byte(src), allLines(src))
	if capped || skip != "" {
		t.Fatalf("capped %v skip %q", capped, skip)
	}
	var drops []Site
	for _, s := range sites {
		if s.Operator == OpDropError {
			drops = append(drops, s)
		}
	}
	type span struct {
		line, endLine int
		replacement   string
	}
	var got []span
	for _, s := range drops {
		got = append(got, span{s.Line, s.EndLine, s.Replacement})
	}
	want := []span{{11, 12, "nil\n"}, {15, 18, "nil\n\n\n"}, {20, 21, "nil\n"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drop_error spans %+v, want %+v", got, want)
	}
	srcLines := strings.Split(src, "\n")
	for _, s := range drops {
		mutated, err := s.Apply([]byte(src))
		if err != nil {
			t.Fatalf("line %d: %v", s.Line, err)
		}
		lines := strings.Split(string(mutated), "\n")
		if len(lines) != len(srcLines) {
			t.Fatalf("line %d: %d lines, want %d", s.Line, len(lines), len(srcLines))
		}
		for i := range lines {
			n := i + 1
			if n < s.Line || n > s.EndLine {
				if lines[i] != srcLines[i] {
					t.Fatalf("mutant of line %d changed line %d: %q", s.Line, n, lines[i])
				}
			}
		}
		if !strings.HasSuffix(lines[s.Line-1], "nil") {
			t.Fatalf("mutant of line %d: first line %q does not end with nil", s.Line, lines[s.Line-1])
		}
	}
}

// Every site FileSites generates is accepted by Apply: its original text is
// at the recorded offsets, the replacement keeps the number of lines and the
// mutated file parses. A generated site that Apply refuses would always be
// INCONCLUSIVE and spend a max_mutants slot. The corpus is the handwritten
// sources of this file plus a sample of the Go standard library, when its
// sources are present (they are in CI and in the golang image).
func TestEverySiteApplies(t *testing.T) {
	corpus := map[string]string{
		"discount.go": discountSource,
		"multi.go": "package p\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n)\n\n" +
			"var ErrX = errors.New(\"x\")\n\n" +
			"func A(a, b int) (int, error) {\n\tif a > b &&\n\t\tb > 0 ||\n\t\ta == 3 {\n\t\treturn 0, fmt.Errorf(\n\t\t\t\"a %d b %d\",\n\t\t\ta, b,\n\t\t)\n\t}\n" +
			"\tswitch {\n\tcase a*b >= 10:\n\t\treturn a - b, errors.Join(ErrX,\n\t\t\tfmt.Errorf(\"b\"))\n\t}\n" +
			"\tg := func() (bool, error) { return a <= b, fmt.Errorf(\"g %d\",\n\t\ta) }\n\t_, _ = g()\n" +
			"\treturn a / b, ErrX\n}\n",
	}
	check := func(name string, src []byte) int {
		sites, _, skip := FileSites(name, src, allLines(string(src)))
		if skip != "" {
			return 0
		}
		for _, s := range sites {
			if _, err := s.Apply(src); err != nil {
				t.Errorf("%s:%d %s %q -> %q: %v", name, s.Line, s.Operator, s.Original, s.Replacement, err)
			}
		}
		return len(sites)
	}
	for name, src := range corpus {
		if check(name, []byte(src)) == 0 {
			t.Fatalf("%s has no site", name)
		}
	}
	root := filepath.Join(build.Default.GOROOT, "src")
	if _, err := os.Stat(filepath.Join(root, "strconv")); err != nil {
		t.Logf("standard library sources not found under %s; checked the handwritten corpus only", root)
		return
	}
	total := 0
	for _, dir := range []string{"errors", "strconv", "strings", "fmt", "encoding/json", "net/url", "path/filepath", "text/template/parse", "go/scanner", "archive/tar"} {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name))
			if err != nil || len(src) > 1<<20 {
				continue
			}
			total += check(dir+"/"+name, src)
		}
	}
	if total < 1000 {
		t.Fatalf("only %d standard library sites were checked", total)
	}
}

// A swap whose replacement would merge with a neighbouring character into
// another token is not generated (the two cases the standard library holds are
// -1+-4 and w+-a.dp, both gofmt'd), and Apply refuses one built by hand.
func TestNoSiteMergesWithNeighbour(t *testing.T) {
	src := "package p\n\nfunc J(a int, p *int) int {\n\tif a == -1+-4 {\n\t\treturn a**p\n\t}\n\treturn a+-a + a- -a\n}\n"
	sites, _, skip := FileSites("p.go", []byte(src), allLines(src))
	if skip != "" {
		t.Fatal(skip)
	}
	var swaps []string
	for _, s := range sites {
		if _, err := s.Apply([]byte(src)); err != nil {
			t.Fatalf("%s at %d:%d: %v", s.Operator, s.Line, s.Column, err)
		}
		if s.Operator == OpSwapArithmetic {
			swaps = append(swaps, fmt.Sprintf("%d:%d %s->%s", s.Line, s.Column, s.Original, s.Replacement))
		}
	}
	// Only the + between the two sums (7:14) and the - of a- -a (7:17) swap.
	if want := []string{"7:14 +->-", "7:17 -->+"}; !reflect.DeepEqual(swaps, want) {
		t.Fatalf("arithmetic swaps %q, want %q", swaps, want)
	}
	offset := strings.Index(src, "+-4")
	bad := Site{Path: "p.go", Line: 4, EndLine: 4, Start: offset, End: offset + 1, Operator: OpSwapArithmetic, Original: "+", Replacement: "-"}
	if _, err := bad.Apply([]byte(src)); err == nil || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("a merging replacement was applied: %v", err)
	}
}

// drop_error is not generated when the dropped expression holds every use of
// an imported package in the file: the import would become unused and the
// mutant could not compile.
func TestDropErrorKeepsImportsUsed(t *testing.T) {
	dropped := func(src string) []string {
		sites, _, skip := FileSites("p.go", []byte(src), allLines(src))
		if skip != "" {
			t.Fatalf("skip %q", skip)
		}
		var got []string
		for _, s := range sites {
			if s.Operator == OpDropError {
				got = append(got, s.Symbol+":"+s.Original)
			}
		}
		return got
	}
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"sole use of errors", `package p

import "errors"

func A() error { return errors.New("a") }
`, nil},
		{"sole use of fmt", `package p

import "fmt"

func A(n int) error { return fmt.Errorf("n=%d", n) }
`, nil},
		{"aliased sole use", `package p

import e "errors"

func A() error { return e.New("a") }
`, nil},
		{"import used elsewhere", `package p

import "errors"

func A() error { return errors.New("a") }

var b = errors.New("b")
`, []string{`A:errors.New("a")`}},
		{"two returns share the import", `package p

import "errors"

func A(x bool) error {
	if x {
		return errors.New("a")
	}
	return errors.New("b")
}
`, []string{`A:errors.New("a")`, `A:errors.New("b")`}},
		{"no import involved", `package p

var errA error

func A() error { return errA }
`, []string{"A:errA"}},
		{"versioned path", `package p

import "example.test/errs/v2"

func A() error { return errs.New("a") }
`, []string{`A:errs.New("a")`}},
	} {
		if got := dropped(tc.src); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: drop_error sites %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSwapArithmeticSkipsStringConcat(t *testing.T) {
	src := `package p

func F(s string, n int) (string, int) {
	return s + "!", n + 1
}
`
	sites, _, _ := FileSites("p.go", []byte(src), allLines(src))
	var swaps []Site
	for _, s := range sites {
		if s.Operator == OpSwapArithmetic {
			swaps = append(swaps, s)
		}
	}
	if len(swaps) != 1 || swaps[0].Column != 20 || swaps[0].Replacement != "-" {
		t.Fatalf("arithmetic swaps %+v, want only the integer + at column 20", views(swaps))
	}
}

// A whole file is skipped for build constraints, generated code, cgo and a
// parse error; a //go:build comment after the package clause is not a
// constraint.
func TestFileSitesSkips(t *testing.T) {
	body := "\nfunc F(a int) bool { return a > 1 }\n"
	for _, tc := range []struct{ name, src, want string }{
		{"go:build", "//go:build linux\n\npackage p\n" + body, skipConstraint},
		{"+build", "// +build linux\n\npackage p\n" + body, skipConstraint},
		{"generated", "// Code generated by x. DO NOT EDIT.\n\npackage p\n" + body, skipGenerated},
		{"cgo", "package p\n\nimport \"C\"\n" + body, skipCgo},
		{"parse error", "package p\n\nfunc F( {\n", skipParse},
		{"comment after the package clause", "package p\n\n//go:build linux\n" + body, ""},
	} {
		sites, _, skip := FileSites("p.go", []byte(tc.src), allLines(tc.src))
		if skip != tc.want || (tc.want == "") != (len(sites) > 0) {
			t.Errorf("%s: skip %q, %d sites; want skip %q", tc.name, skip, len(sites), tc.want)
		}
	}
}

func TestApplyRejectsStaleSource(t *testing.T) {
	sites, _, _ := FileSites("price/price.go", []byte(discountSource), allLines(discountSource))
	s := sites[1]
	if _, err := s.Apply([]byte(strings.Replace(discountSource, "total < 0", "total > 0", 1))); err == nil {
		t.Fatal("a stale source was mutated")
	}
	if _, err := s.Apply([]byte("package price\n")); err == nil {
		t.Fatal("a short source was mutated")
	}
	bad := s
	bad.Replacement = "<\n"
	if _, err := bad.Apply([]byte(discountSource)); err == nil {
		t.Fatal("a line-count change was accepted")
	}
	bad.Replacement = "<<<"
	if _, err := bad.Apply([]byte(discountSource)); err == nil {
		t.Fatal("a mutant that does not parse was accepted")
	}
}

func TestSitesDeterministic(t *testing.T) {
	first, _, _ := FileSites("price/price.go", []byte(discountSource), allLines(discountSource))
	for i := 0; i < 20; i++ {
		again, _, _ := FileSites("price/price.go", []byte(discountSource), allLines(discountSource))
		if !reflect.DeepEqual(first, again) {
			t.Fatal("site enumeration is not deterministic")
		}
	}
}

func TestSymbolNames(t *testing.T) {
	src := `package p

type G[T any] struct{ v T }

func (g *G[T]) Less(a, b int) bool { return a < b }
func (g G[T]) More(a, b int) bool  { return a > b }
func Plain(a, b int) bool          { return a == b }
`
	sites, _, _ := FileSites("p.go", []byte(src), allLines(src))
	names := map[string]bool{}
	for _, s := range sites {
		names[s.Symbol] = true
	}
	for _, want := range []string{"G.Less", "G.More", "Plain"} {
		if !names[want] {
			t.Errorf("missing symbol %s in %v", want, names)
		}
	}
}

func TestPerFileCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\nfunc F(a int) bool {\n")
	for i := 0; i < 1100; i++ {
		b.WriteString("\t_ = a < 1\n") // boundary + increment_constant per line
	}
	b.WriteString("\treturn a > 2\n}\n")
	src := b.String()
	sites, capped, _ := FileSites("p.go", []byte(src), allLines(src))
	if !capped || len(sites) != maxSitesPerFile {
		t.Fatalf("capped %v with %d sites, want %d", capped, len(sites), maxSitesPerFile)
	}
}
