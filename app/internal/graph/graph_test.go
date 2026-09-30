package graph

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

type fixture struct {
	t   *testing.T
	dir string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, dir: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	c := exec.Command("git", append([]string{"-C", f.dir, "-c", "user.name=Probe Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) put(files map[string]string) {
	f.t.Helper()
	for p, content := range files {
		full := filepath.Join(f.dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) commit() string {
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "c")
	return f.git("rev-parse", "HEAD")
}

func (f *fixture) repo() *gitrepo.Repository {
	r, err := gitrepo.Open(context.Background(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// base is a small monorepo: a Go service, a Go library module, a TypeScript
// web app, a Python package and a Rust crate.
var base = map[string]string{
	"lib/go.mod":       "module example.test/lib\n\ngo 1.22\n",
	"lib/money/fmt.go": "package money\n\n// Format formats an amount.\nfunc Format(cents int) string { return \"\" }\n",
	"svc/go.mod":       "module example.test/shop\n\ngo 1.22\n\nrequire (\n\tgithub.com/google/uuid v1.6.0\n\texample.test/lib v0.0.0\n)\n",
	"svc/cart/cart.go": `package cart

// Totaler computes a total.
type Totaler interface{ Total() int }

// Cart holds items.
type Cart struct{ Items []int }

// New returns an empty cart.
func New() *Cart { return &Cart{} }

// Total sums the items.
func (c *Cart) Total() int {
	sum := 0
	for _, i := range c.Items {
		sum += i
	}
	return sum
}
`,
	"svc/api/api.go": `package api

import (
	"fmt"

	"example.test/shop/cart"
)

// Handle answers a request.
func Handle() string { return fmt.Sprint(cart.New().Total()) }
`,
	"svc/.env":           "SECRET=1\n",
	"web/package.json":   `{"name":"web","dependencies":{"react":"^18.0.0"},"devDependencies":{"vitest":"1"}}`,
	"web/src/app.ts":     "import { Helper } from './util';\nimport React from 'react';\nimport fs from 'fs';\nexport function main() { return new Helper().run(); }\n",
	"web/src/util.ts":    "export class Helper {\n  run() { return 1; }\n}\n",
	"py/pyproject.toml":  "[project]\nname = \"py\"\nauthors = [\"someone\"]\ndependencies = [\"requests>=2\", \"Flask\"]\n",
	"py/pkg/__init__.py": "",
	"py/pkg/core.py":     "class Engine:\n    def run(self):\n        return 1\n",
	"py/main.py":         "import os\nimport requests\nfrom pkg.core import Engine\n\ndef main():\n    return Engine().run()\n",
	"rs/Cargo.toml":      "[package]\nname = \"rs\"\n\n[dependencies]\nserde = \"1\"\n\n[dependencies.tokio-util]\nversion = \"0.7\"\n",
	"rs/src/lib.rs":      "mod model;\nuse serde::Serialize;\nuse std::fmt;\nuse tokio_util::codec;\n",
	"rs/src/model.rs":    "pub struct Item { pub id: u32 }\n",
	"README.md":          "# monorepo\n",
}

func build(t *testing.T, f *fixture, commit string, withIndex bool) *Graph {
	t.Helper()
	ctx := context.Background()
	repo := f.repo()
	opts := Options{Sensitive: func(p string) bool { return strings.HasSuffix(p, ".env") }}
	if withIndex {
		x, _, err := symbols.IndexCommit(ctx, repo, commit, symbols.Options{})
		if err != nil {
			t.Fatal(err)
		}
		opts.Index = x
	}
	g, err := Build(ctx, repo, commit, opts)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func hasNode(g *Graph, id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func hasEdge(g *Graph, from, kind, to string) bool {
	for _, e := range g.Edges {
		if e.From == from && e.Kind == kind && e.To == to {
			return true
		}
	}
	return false
}

func TestBuildMonorepo(t *testing.T) {
	f := newFixture(t)
	f.put(base)
	commit := f.commit()
	g := build(t, f, commit, true)
	if !g.Complete || !g.Calls || g.Schema != Schema || g.Commit != commit {
		t.Fatalf("header: complete=%v calls=%v schema=%q limitations=%v", g.Complete, g.Calls, g.Schema, g.Limitations)
	}

	for _, c := range []string{"lib", "svc", "web", "py", "rs"} {
		if _, ok := hasNode(g, componentID(c)); !ok {
			t.Errorf("component %s missing", c)
		}
	}
	if n, _ := hasNode(g, packageID("svc/cart")); n.Name != "example.test/shop/cart" || n.Component != componentID("svc") {
		t.Errorf("Go package node = %+v", n)
	}
	if _, ok := hasNode(g, fileID("svc/.env")); ok {
		t.Error("a sensitive file is in the graph")
	}

	edges := []struct{ from, kind, to string }{
		// Go: an internal package, an external module, a module of the repository; no standard library.
		{fileID("svc/api/api.go"), EdgeImports, packageID("svc/cart")},
		{packageID("svc/api"), EdgeDependsOn, packageID("svc/cart")},
		{componentID("svc"), EdgeDeclares, dependencyID("go", "github.com/google/uuid")},
		// TypeScript: a relative file and an npm package; no Node built-in.
		{fileID("web/src/app.ts"), EdgeImports, fileID("web/src/util.ts")},
		{fileID("web/src/app.ts"), EdgeImports, dependencyID("npm", "react")},
		{componentID("web"), EdgeDependsOn, dependencyID("npm", "react")},
		{componentID("web"), EdgeDeclares, dependencyID("npm", "vitest")},
		// Python: a module of the package and a declared dependency; no standard library.
		{fileID("py/main.py"), EdgeImports, fileID("py/pkg/core.py")},
		{fileID("py/main.py"), EdgeImports, dependencyID("pypi", "requests")},
		{componentID("py"), EdgeDeclares, dependencyID("pypi", "flask")},
		// Rust: a module file and a Cargo dependency, including a renamed one; no std.
		{fileID("rs/src/lib.rs"), EdgeImports, fileID("rs/src/model.rs")},
		{fileID("rs/src/lib.rs"), EdgeImports, dependencyID("cargo", "serde")},
		{fileID("rs/src/lib.rs"), EdgeImports, dependencyID("cargo", "tokio-util")},
		// Types, members, implementations and calls.
		{fileID("svc/cart/cart.go"), EdgeContains, typeID("example.test/shop/cart.Cart")},
		{functionID("example.test/shop/cart.Cart.Total"), EdgeMemberOf, typeID("example.test/shop/cart.Cart")},
		{typeID("example.test/shop/cart.Cart"), EdgeImplements, typeID("example.test/shop/cart.Totaler")},
		{functionID("example.test/shop/api.Handle"), EdgeCalls, functionID("example.test/shop/cart.Cart.Total")},
		{fileID("web/src/util.ts"), EdgeContains, typeID("web/src/util.ts.Helper")},
		{fileID("py/pkg/core.py"), EdgeContains, typeID("py/pkg/core.py.Engine")},
		{fileID("rs/src/model.rs"), EdgeContains, typeID("rs/src/model.rs.Item")},
	}
	for _, e := range edges {
		if !hasEdge(g, e.from, e.kind, e.to) {
			t.Errorf("missing edge %s -%s-> %s", e.from, e.kind, e.to)
		}
	}
	for _, id := range []string{dependencyID("npm", "fs"), dependencyID("pypi", "os"), dependencyID("cargo", "std"), dependencyID("go", "fmt"), dependencyID("pypi", "someone")} {
		if _, ok := hasNode(g, id); ok {
			t.Errorf("%s should not be a dependency", id)
		}
	}
	if n, _ := hasNode(g, typeID("example.test/shop/cart.Totaler")); n.Detail != "interface" || !n.Exported {
		t.Errorf("interface node = %+v", n)
	}

	// The same commit always gives the same graph.
	again := build(t, f, commit, true)
	a, _ := json.Marshal(g)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Error("two builds of one commit differ")
	}
	// Without an index: the structure, no functions.
	structural := build(t, f, commit, false)
	if structural.Calls {
		t.Error("structural graph claims calls")
	}
	for _, n := range structural.Nodes {
		if n.Kind == KindFunction {
			t.Fatalf("structural graph has function %s", n.ID)
		}
	}
}

func TestCrossModuleDependencyAndDelta(t *testing.T) {
	f := newFixture(t)
	f.put(base)
	before := f.commit()
	f.put(map[string]string{
		"svc/api/api.go":   "package api\n\nimport (\n\t\"example.test/lib/money\"\n\t\"github.com/google/uuid\"\n\n\t\"example.test/shop/cart\"\n)\n\n// Handle answers a request.\nfunc Handle() string { _ = uuid.New(); return money.Format(cart.New().Total()) }\n",
		"web/package.json": `{"name":"web","dependencies":{"react":"^18.0.0","left-pad":"1"}}`,
	})
	after := f.commit()
	head := build(t, f, after, true)
	if !hasEdge(head, componentID("svc"), EdgeDependsOn, componentID("lib")) {
		t.Error("the svc component does not depend on the lib module it imports")
	}
	if !hasEdge(head, fileID("svc/api/api.go"), EdgeImports, dependencyID("go", "github.com/google/uuid")) {
		t.Error("the external Go module is not resolved to its require line")
	}
	d := Delta(build(t, f, before, false), head)
	want := map[string]bool{}
	for _, it := range d.Added {
		want[it.Kind+" "+it.Name+it.From+">"+it.To] = true
	}
	for _, w := range []string{"depends_on svc>lib", "declares web>npm:left-pad", "dependency npm:left-pad>", "depends_on example.test/shop/api>example.test/lib/money"} {
		if !want[w] {
			t.Errorf("delta lacks %q; added: %v", w, d.Added)
		}
	}
	for _, it := range d.Removed {
		if (it.Kind == EdgeDeclares && it.To == "npm:vitest") || (it.Kind == KindDependency && it.Name == "npm:vitest") {
			continue // the new package.json drops vitest
		}
		t.Errorf("unexpected removal %+v", it)
	}
	if d.AddedTotal != len(d.Added) || d.BaseCommit != before {
		t.Errorf("delta totals: %+v", d)
	}
}

func TestQueries(t *testing.T) {
	f := newFixture(t)
	f.put(base)
	g := build(t, f, f.commit(), true)
	v := NewView(g)

	s := v.Search("Total", "")
	results := s["results"].([]NodeRef)
	if len(results) == 0 || results[0].ID != functionID("example.test/shop/cart.Cart.Total") {
		t.Fatalf("search Total = %+v", s)
	}
	if r := v.Search("react", KindDependency)["results"].([]NodeRef); len(r) != 1 || r[0].Name != "npm:react" {
		t.Errorf("search dependency = %+v", r)
	}

	n := v.Neighbors("Cart.Total", []string{EdgeCalls}, "in", 1)
	links := n["links"].([]Link)
	if len(links) != 1 || links[0].Node.ID != functionID("example.test/shop/api.Handle") || links[0].Site != "svc/api/api.go:10" {
		t.Fatalf("callers of Cart.Total = %+v", n)
	}
	// Types implementing an interface.
	impl := v.Neighbors(typeID("example.test/shop/cart.Totaler"), []string{EdgeImplements}, "in", 1)["links"].([]Link)
	if len(impl) != 1 || impl[0].Node.Name != "Cart" {
		t.Errorf("implementations = %+v", impl)
	}
	// Depth 2 reaches the component through the package.
	deep := v.Neighbors("svc/cart/cart.go", []string{EdgeContains}, "in", 2)["links"].([]Link)
	if len(deep) != 2 || deep[1].Node.ID != componentID("svc") || deep[1].Via != packageID("svc/cart") {
		t.Errorf("containers = %+v", deep)
	}
	if bad := v.Neighbors("Total", nil, "sideways", 1); bad["error"] == nil {
		t.Error("invalid direction accepted")
	}

	p := v.Path("Handle", "Cart.Total", nil, false)
	if p["found"] != true {
		t.Fatalf("path = %+v", p)
	}
	if p := v.Path("Cart.Total", "Handle", []string{EdgeCalls}, false); p["found"] != false {
		t.Errorf("reverse path found without undirected: %+v", p)
	}
	if p := v.Path("Cart.Total", "Handle", []string{EdgeCalls}, true); p["found"] != true {
		t.Errorf("undirected path = %+v", p)
	}
	// "run" names two methods: an ambiguous reference lists candidates.
	if amb := v.Neighbors("run", nil, "", 1); amb["ambiguous"] != true {
		t.Errorf("ambiguous reference = %+v", amb)
	}
	if missing := v.Path("NoSuchThing", "Handle", nil, false); missing["error"] == nil || missing["side"] != "from" {
		t.Errorf("unknown node = %+v", missing)
	}

	o := v.Overview([]string{"svc/cart/cart.go", "README.md"})
	if len(o.Changed) != 1 {
		t.Fatalf("overview changed = %+v", o.Changed)
	}
	c := o.Changed[0]
	if c.Component != "svc" || c.Package != "example.test/shop/cart" || len(c.PackageDependents) != 1 || c.PackageDependents[0] != "example.test/shop/api" || c.ExternalCallers < 2 {
		t.Errorf("overview of cart.go = %+v", c)
	}
	if len(o.Components) != 5 {
		t.Errorf("components = %+v", o.Components)
	}
	s2 := v.Summary()
	if s2.Nodes.Components != 5 || s2.Edges.Implements != 1 || s2.Status != "built" {
		t.Errorf("summary = %+v", s2)
	}
}

func TestStore(t *testing.T) {
	f := newFixture(t)
	f.put(base)
	commit := f.commit()
	g := build(t, f, commit, true)
	dir := t.TempDir()
	s := OpenStore(dir, "probe test sha256:1")
	limits := DefaultLimits()
	if _, ok := s.Get(commit, true, limits); ok {
		t.Fatal("hit in an empty store")
	}
	if err := s.Put(g, limits); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(commit, true, limits)
	if !ok || len(got.Nodes) != len(g.Nodes) || len(got.Edges) != len(g.Edges) {
		t.Fatal("stored graph not read back")
	}
	if !s.HasCalls(commit, limits) {
		t.Error("HasCalls")
	}
	// Another build, a structural request with other limits, never read it.
	if _, ok := OpenStore(dir, "probe other sha256:2").Get(commit, true, limits); ok {
		t.Error("another probe build read the entry")
	}
	other := limits
	other.MaxFiles = 10
	if _, ok := s.Get(commit, true, other); ok {
		t.Error("other limits read the entry")
	}
	// A corrupted entry is refused and deleted.
	path := s.path(s.key(commit, true, limits))
	if err := os.WriteFile(path, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(commit, true, limits); ok {
		t.Error("corrupted entry read")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupted entry kept")
	}

	// ForReview: stored, then read from the store, with the delta.
	opts := RunOptions{Index: nil, Store: OpenStore(t.TempDir(), "id"), Base: commit}
	f.put(map[string]string{"web/package.json": `{"dependencies":{"react":"1","zod":"3"}}`})
	head := f.commit()
	_, first := ForReview(context.Background(), f.repo(), head, opts)
	_, second := ForReview(context.Background(), f.repo(), head, opts)
	if first.Cache != "stored" || second.Cache != "hit" || first.Delta == nil || first.Delta.AddedTotal == 0 {
		t.Errorf("ForReview: first %+v second %+v", first, second)
	}
}
