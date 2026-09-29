package symbols

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
)

// An import cycle between repository packages is broken: both packages are
// still indexed, and a reference through the faked side of the cycle is
// simply not resolved.
func TestImportCycleIsContained(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("a/a.go", "package a\n\nimport \"example.test/shop/b\"\n\nfunc A() int { return b.B() }\n")
	f.put("b/b.go", "package b\n\nimport \"example.test/shop/a\"\n\nfunc B() int { return 1 }\n\nfunc C() int { return a.A() }\n")
	base := f.commit()
	f.put("b/b.go", "package b\n\nimport \"example.test/shop/a\"\n\nfunc B() int { return 2 }\n\nfunc C() int { return a.A() }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if res.Report().IndexedFiles != 2 {
		t.Fatalf("%+v", res.Report())
	}
	fn := findFunction(t, res.Report(), "example.test/shop/b.B")
	if !fn.Indexed {
		t.Fatalf("%+v", fn)
	}
}

// The time limit also stops a type check from inside, on its next error: a
// package cannot keep go/types busy past the limit by producing errors.
func TestTypeCheckStopsAtDeadlineOnError(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p/p.go", "package p\n\nfunc F() int { return missing + other }\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	b := newBuilder(fset, DefaultLimits(), time.Now().Add(-time.Second))
	n := &pkgNode{path: "example.test/p", name: "p", files: []*ast.File{file}, paths: []string{"p/p.go"}}
	b.check(n)
	if !n.timedOut || n.failed || b.timedOut != 1 || b.failed != 0 || len(b.decls) != 0 {
		t.Fatalf("node %+v builder timed out %d failed %d", n, b.timedOut, b.failed)
	}
	// Without errors the same deadline does not interrupt a check.
	file, _ = parser.ParseFile(fset, "q/q.go", "package q\n\nfunc G() int { return 1 }\n", parser.SkipObjectResolution)
	m := &pkgNode{path: "example.test/q", name: "q", files: []*ast.File{file}, paths: []string{"q/q.go"}}
	b.check(m)
	if !m.checked || m.timedOut || len(b.decls) != 1 {
		t.Fatalf("node %+v decls %+v", m, b.decls)
	}
}

// Declarations and edges come out in a total order.
func TestIndexOrdering(t *testing.T) {
	x := resolveIndex(t)
	for i := 1; i < len(x.decls); i++ {
		if x.decls[i-1].Key >= x.decls[i].Key {
			t.Fatalf("declarations out of order at %d: %q %q", i, x.decls[i-1].Key, x.decls[i].Key)
		}
	}
	for i := 1; i < len(x.edges); i++ {
		a, b := x.edges[i-1], x.edges[i]
		if a.callee > b.callee || a.callee == b.callee && (a.file > b.file || a.file == b.file && (a.line > b.line || a.line == b.line && a.col > b.col)) {
			t.Fatalf("edges out of order at %d", i)
		}
	}
	for i := 1; i < len(x.files); i++ {
		if x.files[i-1] >= x.files[i] {
			t.Fatal("files out of order")
		}
	}
	if d, ok := x.Lookup("example.test/m/b.TestTwice"); !ok || !d.Test || d.Kind != KindFunc {
		t.Fatalf("%+v", d)
	}
	if d, ok := x.Lookup("example.test/m/b.I.M"); !ok || d.Kind != KindInterfaceMethod {
		t.Fatalf("%+v", d)
	}
	if _, ok := x.Lookup("example.test/m/b.nope"); ok {
		t.Fatal("unknown key found")
	}
	var nilIndex *Index
	if _, ok := nilIndex.Lookup("x"); ok || nilIndex.Files() != 0 {
		t.Fatal("nil index answered")
	}
}

// Test functions are recognized syntactically, with the file's name for the
// testing import.
func TestTestFunctionRecognition(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("p/p.go", "package p\n\nfunc Core() int { return 1 }\n")
	f.put("p/p_test.go", "package p\n\nimport (\n\tt2 \"testing\"\n)\n\nfunc TestRenamed(t *t2.T) { _ = Core() }\n\nfunc Testlower(t *t2.T) { _ = Core() }\n\nfunc TestTwoParams(t *t2.T, x int) { _ = Core() }\n\nfunc BenchmarkCore(b *t2.B) { _ = Core() }\n\nfunc TestResult(t *t2.T) int { return Core() }\n")
	base := f.commit()
	f.put("p/p.go", "package p\n\nfunc Core() int { return 2 }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/p.Core")
	if fn.TestsTotal != 1 || fn.Tests[0].Name != "TestRenamed" {
		t.Fatalf("%+v", fn.Tests)
	}
}

// BenchmarkIndexSynthetic indexes a synthetic repository of 40 packages of 25
// files, each file declaring 10 functions that call into the previous package.
func BenchmarkIndexSynthetic(b *testing.B) {
	f := &repoFixture{t: b, dir: b.TempDir()}
	f.git("init", "-b", "main")
	f.put("go.mod", "module example.test/bench\n\ngo 1.23\n")
	for p := 0; p < 40; p++ {
		for file := 0; file < 25; file++ {
			var src strings.Builder
			fmt.Fprintf(&src, "package p%d\n\n", p)
			if p > 0 {
				fmt.Fprintf(&src, "import \"example.test/bench/p%d\"\n\n", p-1)
			}
			for fn := 0; fn < 10; fn++ {
				call := "0"
				if p > 0 {
					call = fmt.Sprintf("p%d.F%d_%d()", p-1, file, fn)
				}
				fmt.Fprintf(&src, "func F%d_%d() int { return %s + %d }\n\n", file, fn, call, fn)
			}
			f.put(fmt.Sprintf("p%d/f%d.go", p, file), src.String())
		}
	}
	base := f.commit()
	f.put("p0/f0.go", strings.Replace(mustRead(b, f.dir+"/p0/f0.go"), "+ 0 }", "+ 100 }", 1))
	head := f.commit()
	repo, err := gitrepo.Open(context.Background(), f.dir)
	if err != nil {
		b.Fatal(err)
	}
	change, err := repo.Analyze(context.Background(), base, head, false)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := Analyze(context.Background(), repo, change, Options{})
		if err != nil || res.Report().Status != model.ImpactIndexed || res.Report().IndexedFiles != 1000 {
			b.Fatalf("%v %+v", err, res.Report())
		}
	}
}

func mustRead(b *testing.B, p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		b.Fatal(err)
	}
	return string(data)
}

// BenchmarkAnalyzeRepository measures Analyze on a real repository named by
// PROBE_BENCH_REPO, comparing HEAD~1 with HEAD, and reports the heap in
// use while the index is retained. It is skipped otherwise.
func BenchmarkAnalyzeRepository(b *testing.B) {
	dir := os.Getenv("PROBE_BENCH_REPO")
	if dir == "" {
		b.Skip("PROBE_BENCH_REPO is not set")
	}
	repo, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		b.Fatal(err)
	}
	change, err := repo.Analyze(context.Background(), "HEAD~1", "HEAD", false)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	var res *Result
	for i := 0; i < b.N; i++ {
		res, err = Analyze(context.Background(), repo, change, Options{})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	b.ReportMetric(float64(m.HeapInuse)/(1<<20), "heap-MiB")
	b.ReportMetric(float64(res.Report().IndexedFiles), "files")
	b.Logf("status %s, reason %q, %d indexed files, %d changed functions", res.Report().Status, res.Report().Reason, res.Report().IndexedFiles, len(res.Report().ChangedFunctions))
	runtime.KeepAlive(res)
}
