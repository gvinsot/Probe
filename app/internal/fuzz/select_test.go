package fuzz

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// selectTrees writes the two revisions and runs Select on change.
func selectTrees(t *testing.T, base, candidate map[string]string, change model.Change, signals []model.Signal, limits Limits) Plan {
	t.Helper()
	root := t.TempDir()
	b, c := filepath.Join(root, "base"), filepath.Join(root, "candidate")
	writeTree(t, b, base)
	writeTree(t, c, candidate)
	plan, err := Select(b, c, change, signals, limits)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func plannedNames(plan Plan) []string {
	var names []string
	for _, p := range plan.Packages {
		for _, t := range p.Targets {
			names = append(names, t.Symbol)
		}
	}
	return names
}

func TestSelectCalcFixture(t *testing.T) {
	base, candidate := calcTrees(t)
	plan, err := Select(base, candidate, modified("calc/calc.go"), nil, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plannedNames(plan), ","); got != "calc.Percent,calc.Join,calc.Discount,calc.Stamp,calc.Halt" {
		t.Fatalf("planned %s; skipped %+v", got, plan.Skipped)
	}
	if len(plan.Skipped) != 0 || plan.BudgetSkipped != 0 {
		t.Fatalf("skipped %+v", plan.Skipped)
	}
	p := plan.Packages[0]
	if p.Dir != "calc" || p.Name != "calc" || !p.Idents["Cents"] || !p.Idents["Discount"] || !p.Idents["strings"] {
		t.Fatalf("package %+v", p)
	}
	d := targetNamed(t, plan, "Discount")
	want := Target{Dir: "calc", Path: "calc/calc.go", Line: d.Line, EndLine: d.EndLine, Name: "Discount", Symbol: "calc.Discount",
		Signature: "func(Cents) Cents", Params: []Param{{Kind: ParamScalar, Basic: "int64", Named: "Cents"}}, Results: 1, Inputs: 64, Exported: true}
	if !reflect.DeepEqual(d, want) || d.EndLine <= d.Line {
		t.Fatalf("Discount = %+v", d)
	}
	if j := targetNamed(t, plan, "Join"); j.Signature != "func([]string) string" || j.Params[0].Kind != ParamSlice {
		t.Fatalf("Join = %+v", j)
	}
	if pct := targetNamed(t, plan, "Percent"); pct.Signature != "func(int, int) int" || len(pct.Params) != 2 {
		t.Fatalf("Percent = %+v", pct)
	}
	again, _ := Select(base, candidate, modified("calc/calc.go"), nil, defaultLimits())
	if !reflect.DeepEqual(plan, again) {
		t.Fatal("two selections of the same change differ")
	}
}

func TestSelectEligibilityRules(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	base := map[string]string{
		"go.mod": gomod,
		"p/p.go": `package p

import "time"

type ID int32
type Label = string
type Changed int
type Shape struct{ W int }

func Same(x int) int { return x + 1 }
func Comment(x int) int { return x + 2 }
func Sig(x int) int { return x }
func Named(id ID, l Label) ID { return id }
func Redef(c Changed) Changed { return c }
func Maps(m map[string]int) int { return len(m) }
func Ptr(p *int) int { return 0 }
func Struct(s Shape) int { return s.W }
func Qualified(d time.Duration) int { return 0 }
func Complex(c complex128) int { return 0 }
func Arr(a [4]int, b []byte, rest ...uint16) int { return len(b) }
func Big(a [17]int) int { return 0 }
func unexported(s string) string { return s }
func Gone(x int) int { return x }
func Zero() int { return 1 }
func Gen[T any](v T) T { return v }
func (s Shape) Area() int { return s.W }
func Asm(x int) int
func Moved(x int) int { return x }
`,
		"p/other.go": `package p

func Elsewhere(x int) int { return x * 2 }
`,
		"p/p_test.go": `package p

import "testing"

func TestSame(t *testing.T) {}
`,
	}
	candidate := map[string]string{
		"go.mod": gomod,
		"p/p.go": `package p

import "time"

type ID int32
type Label = string
type Changed int64
type Shape struct{ W int }

func Same(x int) int { return x + 1 }

// Comment gained a comment and new layout only.
func Comment(x int) int {
	return x + 2 // still two
}
func Sig(x int64) int { return int(x) }
func Named(id ID, l Label) ID { return id + 1 }
func Redef(c Changed) Changed { return c + 1 }
func Maps(m map[string]int) int { return len(m) + 1 }
func Ptr(p *int) int { return 1 }
func Struct(s Shape) int { return s.W + 1 }
func Qualified(d time.Duration) int { return 1 }
func Complex(c complex128) int { return 1 }
func Arr(a [4]int, b []byte, rest ...uint16) int { return len(b) + len(rest) }
func Big(a [17]int) int { return 1 }
func unexported(s string) string { return s + "!" }
func New(x int) int { return x }
func Zero() int { return 2 }
func Gen[T any](v T) T { var zero T; return zero }
func (s Shape) Area() int { return s.W * 2 }
func Asm(x int) int { return x }
`,
		"p/other.go": `package p

func Elsewhere(x int) int { return x * 3 }

func Moved(x int) int { return x + 1 }
`,
		"p/p_test.go": `package p

import "testing"

func TestSame(t *testing.T) { t.Log("changed test") }
`,
	}
	change := model.Change{Files: []model.ChangedFile{
		{Path: "p/p.go", Status: "M"}, {Path: "p/other.go", Status: "M"}, {Path: "p/p_test.go", Status: "M"},
	}}
	plan := selectTrees(t, base, candidate, change, nil, defaultLimits())
	got := strings.Join(plannedNames(plan), ",")
	if got != "p.Elsewhere,p.Moved,p.Named,p.Arr,p.Zero,p.unexported" {
		t.Fatalf("planned %s", got)
	}
	reasons := map[string]string{
		"p.Sig":        ReasonSignature,
		"p.Redef":      reasonNamedDiffers("Changed"),
		"p.Maps":       reasonParamType("map[string]int"),
		"p.Ptr":        reasonParamType("*int"),
		"p.Struct":     reasonParamType("Shape"),
		"p.Qualified":  reasonParamType("time.Duration"),
		"p.Complex":    reasonParamType("complex128"),
		"p.Big":        reasonParamType("[17]int"),
		"p.Gen":        ReasonGeneric,
		"p.Shape.Area": ReasonMethod,
		"p.Asm":        ReasonNoBody,
	}
	for symbol, reason := range reasons {
		s, ok := skipFor(plan, symbol)
		if !ok || s.Symbol != symbol || s.Reason != reason || s.Path != "p/p.go" || s.Line < 1 {
			t.Errorf("%s: skip %+v, want %q", symbol, s, reason)
		}
	}
	if len(plan.Skipped) != len(reasons) {
		t.Errorf("unexpected skips: %+v", plan.Skipped)
	}
	arr := targetNamed(t, plan, "Arr")
	wantParams := []Param{{Kind: ParamArray, Basic: "int", Len: 4}, {Kind: ParamSlice, Basic: "byte"}, {Kind: ParamVariadic, Basic: "uint16"}}
	if !reflect.DeepEqual(arr.Params, wantParams) || arr.Signature != "func([4]int, []byte, ...uint16) int" {
		t.Fatalf("Arr = %+v", arr)
	}
	named := targetNamed(t, plan, "Named")
	if !reflect.DeepEqual(named.Params, []Param{{Kind: ParamScalar, Basic: "int32", Named: "ID"}, {Kind: ParamScalar, Basic: "string", Named: "Label"}}) {
		t.Fatalf("Named = %+v", named.Params)
	}
	if zero := targetNamed(t, plan, "Zero"); zero.Inputs != 1 || len(zero.Params) != 0 {
		t.Fatalf("Zero = %+v", zero)
	}
	if u := targetNamed(t, plan, "unexported"); u.Exported {
		t.Fatalf("unexported = %+v", u)
	}
	if m := targetNamed(t, plan, "Moved"); m.Path != "p/other.go" {
		t.Fatalf("Moved = %+v", m)
	}
}

func TestSelectPackageLevelRules(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	fn := func(pkg, body string) string {
		return "package " + pkg + "\n\nfunc F(x int) int { return " + body + " }\n"
	}
	cases := []struct {
		name            string
		base, candidate map[string]string
		change          model.Change
		want            string // skip reason, or "" for planned
	}{
		{"go:build line", map[string]string{"q/q.go": "//go:build linux\n\n" + fn("q", "x")}, map[string]string{"q/q.go": "//go:build linux\n\n" + fn("q", "x+1")}, modified("q/q.go"), ReasonConstrained},
		{"+build line", map[string]string{"q/q.go": "// +build linux\n\n" + fn("q", "x")}, map[string]string{"q/q.go": "// +build linux\n\n" + fn("q", "x+1")}, modified("q/q.go"), ReasonConstrained},
		{"windows file name", map[string]string{"q/q_windows.go": fn("q", "x")}, map[string]string{"q/q_windows.go": fn("q", "x+1")}, modified("q/q_windows.go"), ReasonConstrained},
		{"amd64-only file name", map[string]string{"q/q_amd64.go": fn("q", "x")}, map[string]string{"q/q_amd64.go": fn("q", "x+1")}, modified("q/q_amd64.go"), ReasonConstrained},
		{"linux file name", map[string]string{"q/q_linux.go": fn("q", "x")}, map[string]string{"q/q_linux.go": fn("q", "x+1")}, modified("q/q_linux.go"), ""},
		{"cgo", map[string]string{"q/q.go": fn("q", "x"), "q/c.go": "package q\n\nimport \"C\"\n"}, map[string]string{"q/q.go": fn("q", "x+1"), "q/c.go": "package q\n\nimport \"C\"\n"}, modified("q/q.go"), ReasonCgo},
		{"package renamed", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"q/q.go": fn("r", "x+1")}, modified("q/q.go"), ReasonPackageName},
		{"two package clauses", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"q/q.go": fn("q", "x+1"), "q/z.go": "package z\n"}, modified("q/q.go"), ReasonPackageClause},
		{"shadowed type", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"q/q.go": fn("q", "x+1"), "q/s.go": "package q\n\ntype string int\n"}, modified("q/q.go"), ReasonShadowed},
		{"shadowed builtin in a test file", map[string]string{"q/q.go": fn("q", "x"), "q/q_test.go": "package q\n\nvar len = 3\n"}, map[string]string{"q/q.go": fn("q", "x+1"), "q/q_test.go": "package q\n\nvar len = 3\n"}, modified("q/q.go"), ReasonShadowed},
		{"external test shadowing is harmless", map[string]string{"q/q.go": fn("q", "x"), "q/x_test.go": "package q_test\n\nvar len = 3\n"}, map[string]string{"q/q.go": fn("q", "x+1"), "q/x_test.go": "package q_test\n\nvar len = 3\n"}, modified("q/q.go"), ""},
		{"duplicate in constrained files", map[string]string{"q/q.go": "package q\n", "q/f_linux.go": fn("q", "x"), "q/f_windows.go": fn("q", "x")}, map[string]string{"q/q.go": "package q\n", "q/f_linux.go": fn("q", "x+1"), "q/f_windows.go": fn("q", "x")}, modified("q/f_linux.go"), ReasonDuplicate},
		{"sensitive directory", map[string]string{"credentials/q.go": fn("q", "x")}, map[string]string{"credentials/q.go": fn("q", "x+1")}, modified("credentials/q.go"), ReasonSensitive},
		{"moved across directories", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"r/q.go": fn("q", "x+1")}, model.Change{Files: []model.ChangedFile{{Path: "r/q.go", OldPath: "q/q.go", Status: "R"}}}, ReasonMovedDir},
		{"renamed within the directory", map[string]string{"q/a.go": fn("q", "x")}, map[string]string{"q/b.go": fn("q", "x+1")}, model.Change{Files: []model.ChangedFile{{Path: "q/b.go", OldPath: "q/a.go", Status: "R"}}}, ""},
		{"unparsable candidate package", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"q/q.go": fn("q", "x+1"), "q/broken.go": "package q\n\nfunc {"}, modified("q/q.go"), reasonUnreadable("broken.go could not be parsed")},
		{"oversized file", map[string]string{"q/q.go": fn("q", "x")}, map[string]string{"q/q.go": fn("q", "x+1"), "q/big.go": "package q\n\n//" + strings.Repeat("x", maxSourceBytes) + "\n"}, modified("q/q.go"), reasonUnreadable("big.go exceeds 2097152 bytes")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.base["go.mod"], tc.candidate["go.mod"] = gomod, gomod
			plan := selectTrees(t, tc.base, tc.candidate, tc.change, nil, defaultLimits())
			if tc.want == "" {
				if len(plan.Skipped) != 0 || len(plannedNames(plan)) != 1 {
					t.Fatalf("plan %+v", plan)
				}
				return
			}
			if len(plannedNames(plan)) != 0 || len(plan.Skipped) != 1 || plan.Skipped[0].Reason != tc.want {
				t.Fatalf("plan %+v, want skip %q", plan, tc.want)
			}
		})
	}
}

func TestSelectIgnoresWhatIsNotARewrite(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	base := map[string]string{"go.mod": gomod, "q/q.go": "package q\n\nfunc Old(x int) int { return x }\n"}
	candidate := map[string]string{
		"go.mod":            gomod,
		"q/q.go":            "package q\n\nfunc New(x int) int { return x }\n",
		"n/n.go":            "package n\n\nfunc Fresh(x int) int { return x }\n",
		"q/testdata/t.go":   "package t\n\nfunc Old(x int) int { return x + 1 }\n",
		"vendor/v/v.go":     "package v\n\nfunc Old(x int) int { return x + 1 }\n",
		"_hidden/h.go":      "package h\n\nfunc Old(x int) int { return x + 1 }\n",
		"q/q_test.go":       "package q\n\nfunc helper(x int) int { return x }\n",
		"q/README.md":       "not go",
		"q/.hidden.go":      "package q\n",
		"q/_ignored.go":     "package q\n",
		"q/deleted_file.go": "package q\n",
	}
	change := model.Change{Files: []model.ChangedFile{
		{Path: "q/q.go", Status: "M"}, {Path: "n/n.go", Status: "A"}, {Path: "q/testdata/t.go", Status: "M"},
		{Path: "vendor/v/v.go", Status: "M"}, {Path: "_hidden/h.go", Status: "M"}, {Path: "q/q_test.go", Status: "M"},
		{Path: "q/README.md", Status: "M"}, {Path: "q/.hidden.go", Status: "M"}, {Path: "q/_ignored.go", Status: "M"},
		{Path: "q/gone.go", Status: "D"}, {Path: "q/bin.go", Status: "M", Binary: true},
	}}
	plan := selectTrees(t, base, candidate, change, nil, defaultLimits())
	if len(plan.Packages) != 0 || len(plan.Skipped) != 0 || plan.Targets() != 0 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestSelectKeepsHarnessLikeIdentifiersEligible(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	decls := `
import (
	json "strings"
	"time"
)

var _ = json.ToUpper
var _ = time.Now

var swiftproofFuzz_run = 1
var swiftproofFuzz0123abcd_run = 2

func TestSwiftProofFuzz_x() {}
`
	base := map[string]string{"go.mod": gomod, "q/q.go": "package q\n" + decls + "\nfunc F(x int) int { return x }\n"}
	candidate := map[string]string{"go.mod": gomod, "q/q.go": "package q\n" + decls + "\nfunc F(x int) int { return x + 1 }\n"}
	plan := selectTrees(t, base, candidate, modified("q/q.go"), nil, defaultLimits())
	if plan.Targets() != 1 {
		t.Fatalf("plan %+v", plan)
	}
	idents := plan.Packages[0].Idents
	for _, name := range []string{"json", "time", "swiftproofFuzz_run", "swiftproofFuzz0123abcd_run", "TestSwiftProofFuzz_x", "F"} {
		if !idents[name] {
			t.Errorf("identifier %s missing from %v", name, idents)
		}
	}
	// A suffix the package happens to use is refused at render time; a fresh
	// one renders.
	if _, err := Render(plan.Packages[0], renderOptions("0123abcd")); !errors.Is(err, ErrCollision) {
		t.Fatalf("collision not refused: %v", err)
	}
	if _, err := Render(plan.Packages[0], renderOptions("89abcdef")); err != nil {
		t.Fatal(err)
	}
}

func TestSelectPriorityAndBudget(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	src := func(pkg string, delta string, names ...string) string {
		out := "package " + pkg + "\n"
		for _, n := range names {
			out += "\nfunc " + n + "(x int) int { return x" + delta + " }\n"
		}
		return out
	}
	base := map[string]string{"go.mod": gomod,
		"a/a.go": src("a", "", "A1", "A2", "a3"),
		"b/b.go": src("b", "", "B1", "B2"),
		"c/c.go": src("c", "", "C1"),
	}
	candidate := map[string]string{"go.mod": gomod,
		"a/a.go": src("a", "+1", "A1", "A2", "a3"),
		"b/b.go": src("b", "+1", "B1", "B2"),
		"c/c.go": src("c", "+1", "C1"),
	}
	change := modified("a/a.go", "b/b.go", "c/c.go")
	// B2 (line 5 of b/b.go) carries a high signal, C1 a medium one.
	signals := []model.Signal{
		{Path: "b/b.go", Line: 5, Side: "new", Severity: "high"},
		{Path: "c/c.go", Line: 3, EndLine: 3, Side: "new", Severity: "medium"},
		{Path: "a/a.go", Line: 3, Side: "old", Severity: "critical"},
	}
	plan := selectTrees(t, base, candidate, change, signals, defaultLimits())
	if got := strings.Join(plannedNames(plan), ","); got != "b.B2,b.B1,c.C1,a.A1,a.A2,a.a3" {
		t.Fatalf("order %s", got)
	}
	if b2 := targetNamed(t, plan, "B2"); b2.Priority != 3 {
		t.Fatalf("B2 priority %d", b2.Priority)
	}
	limits := defaultLimits()
	limits.MaxPackages, limits.MaxFunctions = 2, 2
	plan = selectTrees(t, base, candidate, change, signals, limits)
	if got := strings.Join(plannedNames(plan), ","); got != "b.B2,c.C1" {
		t.Fatalf("budgeted %s", got)
	}
	if plan.BudgetSkipped != 4 || len(plan.Skipped) != 4 {
		t.Fatalf("budget skips %+v", plan.Skipped)
	}
	for _, s := range plan.Skipped {
		want := ReasonBudgetFunctions
		if strings.HasPrefix(s.Symbol, "a.") {
			want = ReasonBudgetPackages
		}
		if s.Reason != want {
			t.Errorf("%s: %s, want %s", s.Symbol, s.Reason, want)
		}
	}
}

func TestSelectCapsPackageInputs(t *testing.T) {
	gomod := "module example.test/sel\n\ngo 1.21\n"
	var b, c strings.Builder
	b.WriteString("package q\n")
	c.WriteString("package q\n")
	for i := 0; i < 32; i++ {
		name := "F" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		b.WriteString("\nfunc " + name + "(x int) int { return x }\n")
		c.WriteString("\nfunc " + name + "(x int) int { return x + 1 }\n")
	}
	limits := defaultLimits()
	limits.MaxFunctions, limits.MaxInputs = 32, 256
	plan := selectTrees(t, map[string]string{"go.mod": gomod, "q/q.go": b.String()}, map[string]string{"go.mod": gomod, "q/q.go": c.String()}, modified("q/q.go"), nil, limits)
	total := 0
	for _, target := range plan.Packages[0].Targets {
		if target.Inputs != MaxPackageInputs/32 {
			t.Fatalf("%s inputs %d", target.Name, target.Inputs)
		}
		total += target.Inputs
	}
	if total > MaxPackageInputs || len(plan.Packages[0].Targets) != 32 {
		t.Fatalf("total %d", total)
	}
}

func TestSelectRootPackageSymbols(t *testing.T) {
	gomod := "module example.test/root\n\ngo 1.21\n"
	plan := selectTrees(t,
		map[string]string{"go.mod": gomod, "root.go": "package root\n\nfunc F(x int) int { return x }\n", "only_windows.go": "package root\n\nfunc W(x int) int { return x }\n"},
		map[string]string{"go.mod": gomod, "root.go": "package root\n\nfunc F(x int) int { return -x }\n", "only_windows.go": "package root\n\nfunc W(x int) int { return -x }\n"},
		modified("root.go", "only_windows.go"), nil, defaultLimits())
	if got := strings.Join(plannedNames(plan), ","); got != "root.F" || plan.Packages[0].Dir != "." {
		t.Fatalf("planned %s in %+v", got, plan.Packages)
	}
	if s, ok := skipFor(plan, "W"); !ok || s.Symbol != "root.W" || s.Reason != ReasonConstrained {
		t.Fatalf("skip %+v", plan.Skipped)
	}
	if got := symbolOf(".", "", "F"); got != "F" {
		t.Fatalf("unnamed root symbol %q", got)
	}
}

func TestSelectRejectsInvalidArguments(t *testing.T) {
	if _, err := Select("", "x", model.Change{}, nil, defaultLimits()); err == nil {
		t.Fatal("empty base accepted")
	}
	if _, err := Select("x", "y", model.Change{}, nil, Limits{}); err == nil {
		t.Fatal("zero limits accepted")
	}
	plan, err := Select(t.TempDir(), t.TempDir(), model.Change{}, nil, defaultLimits())
	if err != nil || plan.Targets() != 0 || plan.Skipped == nil {
		t.Fatalf("empty change: %+v, %v", plan, err)
	}
}
