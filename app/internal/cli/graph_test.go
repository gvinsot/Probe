package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

func lintReport(t *testing.T, dir string, args ...string) (model.Report, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	all := append([]string{"lint", "--repo", dir, "--base", "main", "--head", "candidate", "--out", "reports", "--format", "json,markdown"}, args...)
	if code := Run(context.Background(), all, &out, &errOut, "test"); code != 0 {
		t.Fatalf("%v: code %d: %s", all, code, errOut.String())
	}
	var r model.Report
	data, err := os.ReadFile(filepath.Join(dir, "reports", "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "reports", "CONFIDENCE_REPORT.md"))
	return r, errOut.String() + string(md)
}

// Lint records the repository graph of the head commit and its delta against
// the base, caches it by commit, and --graph=false leaves it out.
func TestLintRecordsRepositoryGraph(t *testing.T) {
	dir := fixture(t)
	git(t, dir, "checkout", "-q", "candidate")
	write(t, dir, "web/package.json", `{"dependencies":{"react":"18"}}`)
	write(t, dir, "web/app.ts", "import React from 'react';\nexport const x = 1;\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "web")
	cache := t.TempDir()

	r, text := lintReport(t, dir, "--graph-cache", cache)
	g := r.Graph
	if g == nil || g.Status != model.GraphBuilt || g.Cache != model.GraphCacheStored || !g.Calls || g.Commit != r.Change.HeadCommit {
		t.Fatalf("graph section = %+v", g)
	}
	if g.Nodes.Components != 2 || g.Nodes.Functions == 0 || g.Nodes.Dependencies != 1 {
		t.Errorf("counts = %+v", g.Nodes)
	}
	if g.Delta == nil || g.Delta.BaseCommit != r.Change.BaseCommit || g.Delta.AddedTotal == 0 {
		t.Fatalf("delta = %+v", g.Delta)
	}
	added := ""
	for _, it := range g.Delta.Added {
		added += it.Kind + ":" + it.Name + it.To + " "
	}
	if !strings.Contains(added, "dependency:npm:react") || !strings.Contains(added, "component:web") {
		t.Errorf("added = %s", added)
	}
	if !strings.Contains(text, "Repository graph: 2 components") || !strings.Contains(text, "## Repository Graph") || !strings.Contains(text, "Added dependency: npm:react") {
		t.Errorf("stderr and Markdown lack the graph:\n%s", text)
	}

	// The same head is read back from the cache.
	if r, _ := lintReport(t, dir, "--graph-cache", cache); r.Graph.Cache != model.GraphCacheHit {
		t.Errorf("second run cache = %q", r.Graph.Cache)
	}
	if r, _ := lintReport(t, dir, "--graph-cache", "off"); r.Graph.Cache != model.GraphCacheOff {
		t.Errorf("--graph-cache off = %q", r.Graph.Cache)
	}
	if r, _ := lintReport(t, dir, "--graph=false"); r.Graph != nil {
		t.Error("--graph=false still records a graph")
	}

	// A cache the candidate could plant is refused.
	var errOut bytes.Buffer
	code := Run(context.Background(), []string{"lint", "--repo", dir, "--base", "main", "--head", "candidate", "--out", "reports", "--graph-cache", filepath.Join(dir, "cache")}, &bytes.Buffer{}, &errOut, "test")
	if code != 3 || !strings.Contains(errOut.String(), "--graph-cache") {
		t.Errorf("cache inside the repository: code %d, %s", code, errOut.String())
	}
}

// probe graph builds and queries the graph outside a review.
func TestGraphCommand(t *testing.T) {
	dir := fixture(t)
	cache := t.TempDir()
	var out, errOut bytes.Buffer
	file := filepath.Join(t.TempDir(), "graph.json")
	if code := Run(context.Background(), []string{"graph", "build", "--repo", dir, "--commit", "candidate", "--graph-cache", cache, "--out", file}, &out, &errOut, "test"); code != 0 {
		t.Fatalf("build: %d %s", code, errOut.String())
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), `"schema": "probe-graph/v1"`) {
		t.Fatalf("graph file: %v %.200s", err, data)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"graph", "query", "search", "Allowed", "--repo", dir, "--commit", "candidate", "--graph-cache", cache}, &out, &errOut, "test"); code != 0 {
		t.Fatalf("query: %d %s", code, errOut.String())
	}
	var answer struct {
		Results []struct{ ID, Kind string }
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil || len(answer.Results) == 0 || answer.Results[0].Kind != "function" {
		t.Fatalf("search answer: %v %s", err, out.String())
	}
	for _, bad := range [][]string{{"graph"}, {"graph", "query", "path", "OnlyOne"}, {"graph", "query", "teleport", "x"}} {
		if code := Run(context.Background(), append(bad, "--repo", dir), &bytes.Buffer{}, &bytes.Buffer{}, "test"); code != 3 {
			t.Errorf("%v: code %d, want 3", bad, code)
		}
	}
}
