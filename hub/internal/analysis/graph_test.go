package analysis

import (
	"bytes"
	"context"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/hub/internal/accounts"
	"github.com/gvinsot/Probe/hub/internal/forge"
	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
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

func TestGraphCountsEachCommitAgainstItsFirstParent(t *testing.T) {
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
	git("checkout", "-b", "side", commits[1])
	if err := os.WriteFile(filepath.Join(g.dir, "other.txt"), []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "other.txt")
	git("commit", "--quiet", "-m", "other")
	git("checkout", "main")
	git("commit", "--quiet", "--allow-empty", "-m", "empty")
	git("merge", "--no-ff", "side", "-m", "merge")
	git("update-ref", "refs/remotes/origin/main", git("rev-parse", "HEAD"))
	graph, err := g.graph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stats := map[string]CommitStats{}
	for _, c := range graph.Commits {
		if c.Stats == nil {
			t.Fatalf("no stats for %+v", c)
		}
		stats[c.Message] = *c.Stats
	}
	want := map[string]CommitStats{"merge": {Files: 1, Additions: 3}, "other": {Files: 1, Additions: 3}, "empty": {}}
	for message, w := range want {
		if stats[message] != w {
			t.Fatalf("%s: stats = %+v, want %+v", message, stats[message], w)
		}
	}
	// repoWithCommits writes one more line per commit.
	if first := graph.Commits[len(graph.Commits)-1]; first.SHA != commits[0] || *first.Stats != (CommitStats{Files: 1, Additions: 1}) {
		t.Fatalf("root = %+v", first)
	}
}

func TestGraphLeavesTheShallowCutUncounted(t *testing.T) {
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
	if len(graph.Commits) != 2 || graph.Commits[1].SHA != commits[2] || graph.Commits[1].Stats != nil {
		t.Fatalf("cut = %+v", graph.Commits)
	}
	if s := graph.Commits[0].Stats; s == nil || *s != (CommitStats{Files: 1, Additions: 1}) {
		t.Fatalf("head stats = %+v", s)
	}
}

func TestParseShortstat(t *testing.T) {
	got := parseShortstat(" 1 file changed, 2 deletions(-)")
	if *got != (CommitStats{Files: 1, Deletions: 2}) {
		t.Fatalf("got %+v", got)
	}
}

// A branch's graph holds that branch's history only, still lists every
// branch, and labels the other tips it holds; an unknown branch is refused.
func TestGraphOfOneBranch(t *testing.T) {
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
	git("checkout", "--quiet", "-b", "feature")
	git("commit", "--quiet", "--allow-empty", "-m", "feature work")
	feature := git("rev-parse", "HEAD")
	git("checkout", "--quiet", "main")
	git("commit", "--quiet", "--allow-empty", "-m", "main work")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + g.dir, "GIT_HTTP_EXPORT_ALL=1"}})
	defer remote.Close()
	runner, st := testRunner(t, "/must-not-run")
	keys, err := secrets.New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.Seal("access")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutUser(&store.User{Key: "user", Provider: "github", Token: token}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRepo("user", &store.Repo{Key: "repo", Provider: "github", DefaultBranch: "main", CloneURL: remote.URL + "/.git"}); err != nil {
		t.Fatal(err)
	}
	runner.accounts = accounts.New(st, keys, map[string]forge.Provider{"github": gitOnlyProvider{}})

	graph, err := runner.Graph(ctx, "user", "repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	messages := []string{}
	for _, c := range graph.Commits {
		messages = append(messages, c.Message)
	}
	if strings.Join(messages, "|") != "feature work|change|change" || graph.Commits[0].SHA != feature {
		t.Fatalf("feature history = %v", messages)
	}
	if len(graph.Branches) != 2 || graph.Branches[0].Name != "feature" || graph.Branches[1].Name != "main" {
		t.Fatalf("branches = %+v", graph.Branches)
	}
	if strings.Join(graph.Commits[0].Branches, ",") != "feature" || graph.Commits[1].SHA != commits[1] {
		t.Fatalf("tips = %+v", graph.Commits)
	}
	if _, err := runner.Graph(ctx, "user", "repo", "missing"); !errors.Is(err, ErrUnknownBranch) {
		t.Fatalf("unknown branch: %v", err)
	}
	if _, err := runner.Graph(ctx, "user", "repo", "../main"); !errors.Is(err, ErrUnknownBranch) {
		t.Fatalf("crafted branch: %v", err)
	}
}
