package symbols

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The resolution fixture: calls across packages, a promoted method, a generic
// method, function values, an interface call, calls into packages outside the
// repository and a method call on a value of unresolved type.
const (
	resolveA = `package a

import "context"

type T struct{}

func New() *T { return &T{} }

func (t *T) M() int { return 1 }

type G[K comparable] struct{ v map[K]int }

func (g *G[K]) Get(k K) int { return g.v[k] }

type Outer struct{ *T }

func F(x int) int { return x }

func Ctx(ctx context.Context) {}
`
	resolveB = `package b

import (
	"context"
	"strings"

	"example.test/m/a"
	"github.com/other/lib"
)

type I interface{ M() int }

func apply(f func(int) int) int { return f(1) }

func Use(ctx context.Context) int {
	t := a.New()
	o := a.Outer{T: t}
	n := o.M()
	g := &a.G[string]{}
	n += g.Get("k")
	f := a.F
	n += apply(a.F)
	var i I = t
	n += i.M()
	_ = strings.ToUpper("x")
	lib.Open(ctx).M()
	return n + f(2)
}

func Twice(ctx context.Context) int { return Use(ctx) + Use(ctx) }
`
	resolveBTest = `package b

import (
	"context"
	"testing"
)

func TestTwice(t *testing.T) { _ = Twice(context.Background()) }
`
)

func resolveIndex(t *testing.T) *Index {
	t.Helper()
	f := newRepo(t)
	f.put("go.mod", "module example.test/m\n\ngo 1.23\n")
	f.put("a/a.go", resolveA)
	f.put("b/b.go", resolveB)
	f.put("b/b_test.go", resolveBTest)
	base := f.commit()
	f.put("a/a.go", strings.Replace(resolveA, "return 1", "return 2", 1))
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if res.Report().Status != "indexed" || res.Index() == nil {
		t.Fatalf("%+v", res.Report())
	}
	return res.Index()
}

func query(t *testing.T, x *Index, tool, symbol string, depth int) map[string]any {
	t.Helper()
	out, found, err := x.Query(context.Background(), tool, symbol, depth)
	if err != nil || !found {
		t.Fatalf("%s %q: found %v, %v", tool, symbol, found, err)
	}
	// Responses are JSON-encoded for the reviewer; decode as the reviewer sees them.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["method"] != Method || m["limitations"] != Limitations {
		t.Fatalf("response without method and limitations: %s", data)
	}
	return m
}

// sites renders the references or callers of a response as path:line:resolution:call.
func sites(m map[string]any, field string) []string {
	var out []string
	for _, raw := range m[field].([]any) {
		r := raw.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v:%v:%v", r["path"], r["line"], r["resolution"], r["call"]))
	}
	return out
}

func TestIndexResolvesCrossPackageCallsAndMethods(t *testing.T) {
	x := resolveIndex(t)
	for _, tc := range []struct {
		symbol string
		want   string
	}{
		{"a.New", "b/b.go:16:static:true"},
		{"example.test/m/a.New", "b/b.go:16:static:true"},
		// The promoted call is static; the interface call through b.I may dispatch.
		{"a.T.M", "b/b.go:18:static:true b/b.go:24:interface:true"},
		{"(*T).M", "b/b.go:18:static:true b/b.go:24:interface:true"},
		{"a.G.Get", "b/b.go:20:static:true"},
		// Function values are references, not calls.
		{"a.F", "b/b.go:21:static:false b/b.go:22:static:false"},
		{"b.I.M", "b/b.go:24:static:true"},
	} {
		m := query(t, x, "find_references", tc.symbol, 0)
		if got := strings.Join(sites(m, "references"), " "); got != tc.want {
			t.Errorf("%s: references %q, want %q", tc.symbol, got, tc.want)
		}
	}
	// The unresolved lib.Open(ctx).M() call is a name match of a.T.M only.
	m := query(t, x, "find_references", "a.T.M", 0)
	if m["name_matches_total"] != float64(1) {
		t.Fatalf("name matches %+v", m["name_matches"])
	}
	if m := query(t, x, "find_references", "a.New", 0); m["name_matches"] != nil {
		t.Fatalf("a function has no name matches: %+v", m)
	}
}

func TestFindCallersDepth(t *testing.T) {
	x := resolveIndex(t)
	m := query(t, x, "find_callers", "a.New", 0)
	if m["depth"] != float64(1) || strings.Join(sites(m, "callers"), " ") != "b/b.go:16:static:true" {
		t.Fatalf("depth 1: %+v", m)
	}
	m = query(t, x, "find_callers", "a.New", 3)
	got := sites(m, "callers")
	want := "b/b.go:16:static:true b/b.go:30:static:true b/b.go:30:static:true b/b_test.go:8:static:true"
	if strings.Join(got, " ") != want {
		t.Fatalf("depth 3: %q, want %q", got, want)
	}
	last := m["callers"].([]any)[3].(map[string]any)
	if last["depth"] != float64(3) || last["test"] != true || fmt.Sprint(last["via"]) != "[example.test/m/b.TestTwice example.test/m/b.Twice example.test/m/b.Use example.test/m/a.New]" {
		t.Fatalf("depth-3 caller %+v", last)
	}
	// Depth above the maximum is clamped by the index; the harness rejects it first.
	if m := query(t, x, "find_callers", "a.New", 9); m["depth"] != float64(MaxDepth) {
		t.Fatalf("%+v", m["depth"])
	}
}

func TestInspectSymbol(t *testing.T) {
	x := resolveIndex(t)
	m := query(t, x, "inspect_symbol", "a.T.M", 0)
	decl := m["declaration"].(map[string]any)
	if decl["symbol"] != "example.test/m/a.T.M" || decl["kind"] != KindMethod || decl["path"] != "a/a.go" || decl["line"] != float64(9) || decl["changed"] != "body_changed" || !strings.Contains(decl["signature"].(string), "M() int") {
		t.Fatalf("declaration %+v", decl)
	}
	if m["reference_sites"] != float64(2) || fmt.Sprint(m["interface_dispatch"]) != "[map[interface_method:example.test/m/b.I.M line:11 path:b/b.go]]" {
		t.Fatalf("%+v", m)
	}
	if m["tests_reaching_total"] != float64(1) {
		t.Fatalf("tests %+v", m["tests_reaching"])
	}
	m = query(t, x, "inspect_symbol", "b.I.M", 0)
	if fmt.Sprint(m["implementations"]) != "[map[line:9 path:a/a.go symbol:example.test/m/a.T.M]]" {
		t.Fatalf("implementations %+v", m)
	}
	m = query(t, x, "inspect_symbol", "b.Use", 0)
	var callees []string
	for _, c := range m["callees"].([]any) {
		callees = append(callees, c.(map[string]any)["symbol"].(string))
	}
	want := "example.test/m/a.New example.test/m/a.T.M example.test/m/a.G.Get example.test/m/a.F example.test/m/b.apply example.test/m/b.I.M"
	if strings.Join(callees, " ") != want {
		t.Fatalf("callees %q, want %q", callees, want)
	}
}

func TestQueryResolution(t *testing.T) {
	x := resolveIndex(t)
	// A bare method name matching a concrete and an interface method is ambiguous.
	m := query(t, x, "find_references", "M", 0)
	if m["ambiguous"] != true || m["candidates_total"] != float64(2) {
		t.Fatalf("%+v", m)
	}
	for _, symbol := range []string{"Nope", "a.Nope", "T", "bad symbol!", "a.T.M;rm", strings.Repeat("x", 300)} {
		if _, found, err := x.Query(context.Background(), "find_references", symbol, 0); found || err != nil {
			t.Errorf("%q: found %v, %v", symbol, found, err)
		}
	}
	var nilIndex *Index
	if _, found, err := nilIndex.Query(context.Background(), "find_callers", "a.New", 1); found || err != nil {
		t.Fatal("a nil index answered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := x.Query(ctx, "find_callers", "a.New", 1); err == nil {
		t.Fatal("a cancelled query answered")
	}
}

// Queries may run concurrently (the index guards its lazy cache).
func TestQueryConcurrent(t *testing.T) {
	x := resolveIndex(t)
	done := make(chan bool)
	for i := 0; i < 8; i++ {
		go func() {
			for _, tool := range []string{"find_references", "find_callers", "inspect_symbol"} {
				_, _, _ = x.Query(context.Background(), tool, "a.T.M", 3)
			}
			done <- true
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
