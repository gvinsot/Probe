package symbols

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// checkedPackage type-checks one self-contained file.
func checkedPackage(t *testing.T, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("p", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func namedType(t *testing.T, pkg *types.Package, name string) *types.Named {
	t.Helper()
	named, ok := pkg.Scope().Lookup(name).Type().(*types.Named)
	if !ok {
		t.Fatalf("%s is not a named type", name)
	}
	return named
}

// bigSource declares, in package pkg, width empty types and a struct Big
// embedding all of them.
func bigSource(pkg string, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	for i := 0; i < width; i++ {
		fmt.Fprintf(&b, "type E%d struct{}\n", i)
	}
	b.WriteString("\ntype Big struct {\n")
	for i := 0; i < width; i++ {
		fmt.Fprintf(&b, "\tE%d\n", i)
	}
	b.WriteString("}\n")
	return b.String()
}

// wideSource is bigSource in package p, with T embedding Big and a method Do.
func wideSource(width int) string {
	return bigSource("p", width) + "\ntype T struct{ Big }\n\nfunc (T) Do() int { return 1 }\n"
}

// The lookup estimate counts the methods and fields met and is quadratic in
// the width of an embedding level; it stops at its limit, and recursive
// embedding through pointers ends.
func TestLookupSteps(t *testing.T) {
	small := checkedPackage(t, "package p\n\ntype S struct{ a, b int }\n\nfunc (S) M() {}\n\ntype A struct{ *B; x int }\n\ntype B struct{ *A }\n")
	// S itself, its method and its two fields.
	if got := lookupSteps(namedType(t, small, "S"), 1<<20); got != 4 {
		t.Fatalf("S: %d steps", got)
	}
	if got := lookupSteps(namedType(t, small, "A"), 1<<20); got > 20 {
		t.Fatalf("A: %d steps", got)
	}
	wide := checkedPackage(t, wideSource(300))
	T := namedType(t, wide, "T")
	if got := lookupSteps(T, 1<<30); got < 300*300 || got > 300*300+1000 {
		t.Fatalf("T: %d steps", got)
	}
	if got := lookupSteps(T, 1000); got != 1001 {
		t.Fatalf("T with limit 1000: %d steps", got)
	}
}

// Only signatures that mention type parameters may match an interface after
// instantiation.
func TestUsesTypeParams(t *testing.T) {
	pkg := checkedPackage(t, "package p\n\ntype Box[T any] struct{ v T }\n\nfunc (b *Box[T]) Size() int { return 1 }\n\nfunc (b *Box[T]) Get() T { return b.v }\n\nfunc (b *Box[T]) Each(f func(map[string][]*T)) {}\n\nfunc (b *Box[T]) Self() *Box[T] { return b }\n")
	box := namedType(t, pkg, "Box")
	for name, want := range map[string]bool{"Size": false, "Get": true, "Each": true, "Self": true} {
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(box), false, pkg, name)
		sig := obj.(*types.Func).Type().(*types.Signature)
		if got := usesTypeParams(sig.Params()) || usesTypeParams(sig.Results()); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// wideEmbeddingRepo is a repository where every method T0.Do..T(types-1).Do
// calls the changed f.F, each T embeds big.Big, which embeds width empty
// types, and ifaces interfaces I{ Do() int; Xk() } share the method name Do.
// Checking whether a T implements one of them costs go/types a lookup that is
// quadratic in width.
func wideEmbeddingRepo(t testing.TB, width, typeCount, ifaces int) (*repoFixture, string, string) {
	t.Helper()
	f := &repoFixture{t: t, dir: t.TempDir()}
	f.git("init", "-b", "main")
	f.git("config", "core.autocrlf", "false")
	f.put("go.mod", "module example.test/wide\n\ngo 1.23\n")
	f.put("big/big.go", bigSource("big", width))
	var ifs strings.Builder
	ifs.WriteString("package ifs\n\n")
	for k := 0; k < ifaces; k++ {
		fmt.Fprintf(&ifs, "type I%d interface {\n\tDo() int\n\tX%d()\n}\n\n", k, k)
	}
	f.put("ifs/ifs.go", ifs.String())
	f.put("f/f.go", "package f\n\nfunc F() int { return 1 }\n")
	var ts strings.Builder
	ts.WriteString("package ts\n\nimport (\n\t\"example.test/wide/big\"\n\t\"example.test/wide/f\"\n)\n\n")
	for i := 0; i < typeCount; i++ {
		fmt.Fprintf(&ts, "type T%d struct{ big.Big }\n\nfunc (T%d) Do() int { return f.F() }\n\n", i, i)
	}
	f.put("ts/ts.go", ts.String())
	f.put("ts/ts_test.go", "package ts\n\nimport \"testing\"\n\nfunc TestDo(t *testing.T) { _ = T0{}.Do() }\n")
	base := f.commit()
	f.put("f/f.go", "package f\n\nfunc F() int { return 2 }\n")
	head := f.commit()
	return f, base, head
}

// BenchmarkImplementsWideEmbedding measures one implementation check (T and
// *T against an interface of two methods, one missing) of a type embedding a
// struct of width embedded types, and reports the time per estimated step
// (implementWork), the unit of maxImplementSteps.
func BenchmarkImplementsWideEmbedding(b *testing.B) {
	for _, width := range []int{100, 300, 1000} {
		b.Run(fmt.Sprintf("width=%d", width), func(b *testing.B) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", wideSource(width)+"\ntype I interface {\n\tDo() int\n\tX()\n}\n", 0)
			if err != nil {
				b.Fatal(err)
			}
			pkg, err := (&types.Config{}).Check("p", fset, []*ast.File{f}, nil)
			if err != nil {
				b.Fatal(err)
			}
			T := pkg.Scope().Lookup("T").Type().(*types.Named)
			iface := pkg.Scope().Lookup("I").Type().Underlying().(*types.Interface)
			work := implementWork(iface.NumMethods(), lookupSteps(T, 1<<40), false)
			ptr := types.NewPointer(T)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if types.Implements(T, iface) || types.Implements(ptr, iface) {
					b.Fatal("T implements I")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(work), "ns/step")
			b.ReportMetric(float64(work), "steps/op")
		})
	}
}

// BenchmarkImpactSearchWideEmbedding measures Analyze with a 3 s limit on
// 1,000 types whose Do method calls the changed function and embeds a struct
// of 1,000 or 3,000 embedded types, with 200 interfaces sharing the name Do.
// At width 1,000 each implementation check is just within maxImplementSteps
// and the reaching-test search stops at the time limit; at width 3,000 no
// check is made.
func BenchmarkImpactSearchWideEmbedding(b *testing.B) {
	for _, width := range []int{1000, 3000} {
		b.Run(fmt.Sprintf("width=%d", width), func(b *testing.B) {
			f, base, head := wideEmbeddingRepo(b, width, 1000, 200)
			repo, change := f.open(base, head)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := Analyze(context.Background(), repo, change, Options{Limits: Limits{Timeout: 3 * time.Second}})
				if err != nil || res.Report().Status != model.ImpactLimited {
					b.Fatalf("%v %s %q", err, res.Report().Status, res.Report().Reason)
				}
			}
		})
	}
}

// A receiver type whose embedding is too wide to check within the bound is
// not checked against interfaces: the analysis ends well within its time
// limit instead of spending minutes in go/types, the section is limited with
// a reason, and tool answers are marked truncated.
func TestWideEmbeddingReceiverNotChecked(t *testing.T) {
	f, base, head := wideEmbeddingRepo(t, 3000, 50, 200)
	repo, change := f.open(base, head)
	const limit = 3 * time.Second
	start := time.Now()
	res, err := Analyze(context.Background(), repo, change, Options{Limits: Limits{Timeout: limit}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	impact := res.Report()
	if elapsed > limit+3*time.Second {
		t.Fatalf("Analyze took %s with a %s limit", elapsed, limit)
	}
	fn := findFunction(t, impact, "example.test/wide/f.F")
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "estimated type-checker steps") || !strings.Contains(fn.Reason, reasonImplCostly) || strings.Contains(impact.Reason, "time limit") {
		t.Fatalf("status %q reason %q function %+v", impact.Status, impact.Reason, fn)
	}
	// The static callers and the reaching test are still found.
	if fn.CallersTotal != 50 || fn.TestsTotal != 1 || fn.Tests[0].Name != "TestDo" {
		t.Fatalf("%+v", fn)
	}
	if len(signalsOf(res.Signals(), model.SignalAnalysisLimited)) != 1 {
		t.Fatalf("signals %+v", res.Signals())
	}
	checkWording(t, "reason", impact.Reason)
	checkWording(t, "function reason", fn.Reason)
	for _, q := range []struct{ tool, symbol string }{{"find_references", "ts.T40.Do"}, {"find_callers", "ts.T41.Do"}, {"inspect_symbol", "ifs.I0.Do"}} {
		start := time.Now()
		m := query(t, res.Index(), q.tool, q.symbol, 3)
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("%s %s took %s", q.tool, q.symbol, d)
		}
		if m["truncated"] != true || m["truncated_note"] != truncatedNote {
			t.Fatalf("%s %s: %+v", q.tool, q.symbol, m)
		}
	}
	checkWording(t, "truncated note", truncatedNote)
}

// The deadline and the context are checked before every implementation
// check: a budget already past its deadline makes none, and its partial
// result is not cached.
func TestImplementersCheckDeadlineBeforeEachCheck(t *testing.T) {
	f, base, head := wideEmbeddingRepo(t, 2, 2, 3)
	res, _ := f.analyze(base, head, Options{})
	x := res.Index()
	id := x.byKey["example.test/wide/ts.T1.Do"]
	x.mu.Lock()
	delete(x.impl, id)
	x.mu.Unlock()
	bud := newBudget(context.Background(), time.Now().Add(-time.Second), 1<<40)
	bud.sinceCheck = 0
	if got := x.implementers(id, bud); len(got) != 0 || !bud.stopped || !bud.timedOut {
		t.Fatalf("implementers %v budget %+v", got, bud)
	}
	if _, cached := x.impl[id]; cached {
		t.Fatal("an interrupted lookup was cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bud = newBudget(ctx, time.Time{}, 1<<40)
	if got := x.implementers(id, bud); len(got) != 0 || !bud.cancelled {
		t.Fatalf("implementers %v budget %+v", got, bud)
	}
	// Without a bound, T1 implements none of the three interfaces (each also
	// needs its own Xk method), and the result is cached.
	bud = newBudget(context.Background(), time.Time{}, 1<<40)
	if got := x.implementers(id, bud); len(got) != 0 || bud.stopped || bud.implGap() {
		t.Fatalf("implementers %v budget %+v", got, bud)
	}
	if _, cached := x.impl[id]; !cached {
		t.Fatal("a finished lookup was not cached")
	}
}

// Checks just within the bound each cost go/types milliseconds; many of them
// stop at the analysis time limit, at the query time limit and at
// cancellation, each noticed before the next check.
func TestImplementationChecksStopAtTimeLimitsAndCancel(t *testing.T) {
	width := 900
	f, base, head := wideEmbeddingRepo(t, width, 50, 200)
	repo, change := f.open(base, head)
	pkg := checkedPackage(t, wideSource(width))
	if work := implementWork(2, lookupSteps(namedType(t, pkg, "T"), 1<<40), false); work > maxImplementSteps || work < maxImplementSteps/2 {
		t.Fatalf("the fixture's checks are estimated at %d steps", work)
	}
	// The whole search needs 10,000 checks: minutes of go/types work. The
	// searches get one second from their start, whatever the type check took.
	var start time.Time
	searchDeadlineHook = func(time.Time) time.Time {
		start = time.Now()
		return start.Add(time.Second)
	}
	defer func() { searchDeadlineHook = nil }()
	res, err := Analyze(context.Background(), repo, change, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the searches took %s with a 1s deadline", d)
	}
	impact := res.Report()
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "the reaching-test search stopped at the index time limit (2m0s)") {
		t.Fatalf("status %q reason %q", impact.Status, impact.Reason)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	searchDeadlineHook = func(d time.Time) time.Time {
		start = time.Now()
		time.AfterFunc(300*time.Millisecond, cancel)
		return d
	}
	if _, err := Analyze(ctx, repo, change, Options{}); err != context.Canceled {
		t.Fatalf("a cancelled search returned %v", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond+3*time.Second {
		t.Fatalf("the search stopped %s after it started, cancelled at 300ms", d)
	}

	// An index whose searches did nothing, so no lookup is cached.
	searchDeadlineHook = func(time.Time) time.Time { return time.Now().Add(-time.Second) }
	res, err = Analyze(context.Background(), repo, change, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { queryTimeout = d }(queryTimeout)
	queryTimeout = 500 * time.Millisecond
	start = time.Now()
	m := query(t, res.Index(), "inspect_symbol", "ifs.I0.Do", 0)
	if d := time.Since(start); d > queryTimeout+3*time.Second {
		t.Fatalf("inspect_symbol took %s with a %s query limit", d, queryTimeout)
	}
	if m["truncated"] != true {
		t.Fatalf("%+v", m)
	}
}

// A method of a generic type gets the interface callers that every
// instantiation may dispatch to. An interface that only some instantiation
// may implement is not checked, and that gap is reported.
func TestGenericReceiverInterfaceCallers(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", "module example.test/gen\n\ngo 1.23\n")
	box := "package g\n\ntype Box[T any] struct{ v T }\n\nfunc (b *Box[T]) Size() int { return %d }\n\nfunc (b *Box[T]) Get() T { _ = %d; return b.v }\n\ntype Sizer interface{ Size() int }\n\ntype Texter interface{ Size() string }\n\ntype IntGetter interface{ Get() int }\n"
	f.put("g/g.go", fmt.Sprintf(box, 1, 1))
	f.put("u/u.go", "package u\n\nimport \"example.test/gen/g\"\n\nfunc Use(s g.Sizer) int { return s.Size() }\n\nfunc Text(s g.Texter) string { return s.Size() }\n\nfunc Read(r g.IntGetter) int { return r.Get() }\n")
	base := f.commit()
	f.put("g/g.go", fmt.Sprintf(box, 2, 2))
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	size := findFunction(t, impact, "example.test/gen/g.Box.Size")
	if !size.Indexed || size.Reason != "" || size.CallersTotal != 1 || size.Callers[0].Path != "u/u.go" || size.Callers[0].Line != 5 || size.Callers[0].Resolution != model.ResolutionInterface {
		t.Fatalf("Size: %+v", size)
	}
	get := findFunction(t, impact, "example.test/gen/g.Box.Get")
	if !get.Indexed || get.CallersTotal != 0 || get.Reason != reasonImplGeneric {
		t.Fatalf("Get: %+v", get)
	}
	if impact.Status != model.ImpactLimited || impact.Reason != "interfaces that a generic receiver type may implement only once instantiated were not checked, so interface calls reaching 1 changed functions may be missing" {
		t.Fatalf("status %q reason %q", impact.Status, impact.Reason)
	}
	checkWording(t, "reason", impact.Reason)
	checkWording(t, "function reason", get.Reason)
	if m := query(t, res.Index(), "find_references", "g.Box.Size", 0); m["truncated"] != false || m["references_total"] != float64(1) {
		t.Fatalf("%+v", m)
	}
	if m := query(t, res.Index(), "find_references", "g.Box.Get", 0); m["truncated"] != true {
		t.Fatalf("%+v", m)
	}
}
