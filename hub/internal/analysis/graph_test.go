package analysis

import (
	"context"
	"strings"
	"testing"
)

func TestGraphIncludesBranchesAndBothMergeParents(t *testing.T) {
	g, commits := repoWithCommits(t, 2)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		out, err := g.run(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("checkout", "-b", "feature/ui", commits[0])
	git("commit", "--allow-empty", "-m", "side branch <script>")
	side := git("rev-parse", "HEAD")
	git("checkout", "main")
	git("merge", "--no-ff", "feature/ui", "-m", "merge")
	merge := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", merge)
	git("update-ref", "refs/remotes/origin/feature/ui", side)
	graph, err := g.graph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Limited || len(graph.Commits) != 4 || len(graph.Branches) != 2 {
		t.Fatalf("graph = %+v", graph)
	}
	head := graph.Commits[0]
	if head.SHA != merge || strings.Join(head.Parents, ",") != commits[1]+","+side {
		t.Fatalf("merge = %+v", head)
	}
	positions := map[string]int{}
	for i, c := range graph.Commits {
		positions[c.SHA] = i
	}
	for i, c := range graph.Commits {
		for _, p := range c.Parents {
			if positions[p] <= i {
				t.Fatalf("parent precedes child: %+v", c)
			}
		}
	}
	if graph.Branches[0].Name != "feature/ui" {
		t.Fatalf("branches: %+v", graph.Branches)
	}
}

func TestGraphEmptyRepository(t *testing.T) {
	g, _ := repoWithCommits(t, 0)
	graph, err := g.graph(context.Background())
	if err != nil || len(graph.Commits) != 0 {
		t.Fatalf("graph = %+v, %v", graph, err)
	}
}

func TestGraphMarksShallowHistoryAsLimited(t *testing.T) {
	source, commits := repoWithCommits(t, 4)
	g, _ := repoWithCommits(t, 0)
	ctx := context.Background()
	if _, err := g.run(ctx, "fetch", "--depth=2", "file://"+source.dir, "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	graph, err := g.graph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Limited || len(graph.Commits) != 2 || graph.Commits[0].SHA != commits[3] {
		t.Fatalf("graph = %+v", graph)
	}
}
