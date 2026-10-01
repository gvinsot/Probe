package analysis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

// CommitNode preserves Git's parent links, including both sides of merges.
type CommitNode struct {
	SHA      string   `json:"sha"`
	Parents  []string `json:"parents"`
	Message  string   `json:"message"`
	Author   string   `json:"author"`
	Date     string   `json:"date"`
	Branches []string `json:"branches"`
	// Stats is the size of the change against the first parent; nil when Git
	// cannot tell, at the cut of a shallow history.
	Stats *CommitStats `json:"stats,omitempty"`
}

// CommitStats is what git --shortstat counts for one commit.
type CommitStats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

type Branch struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

type CommitGraph struct {
	Commits  []CommitNode `json:"commits"`
	Branches []Branch     `json:"branches"`
	Limited  bool         `json:"limited"`
}

// Graph reads history only; it never checks out or executes repository files.
func (r *Runner) Graph(ctx context.Context, userKey, repoKey string) (graph *CommitGraph, resultErr error) {
	var secrets []string
	defer func() {
		if resultErr != nil {
			resultErr = errors.New(store.SafeError(resultErr.Error(), secrets...))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	repo, err := r.store.Repo(userKey, repoKey)
	if err != nil {
		return nil, err
	}
	user, err := r.store.User(userKey)
	if err != nil {
		return nil, err
	}
	provider, err := r.accounts.Provider(repo.Provider)
	if err != nil {
		return nil, err
	}
	token, err := r.accounts.Token(ctx, user)
	secrets = []string{provider.GitAuthHeader(token), token.AccessToken, token.RefreshToken}
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "probe-graph-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	g := &gitRunner{dir: work, env: gitEnv(work, repo.CloneURL, provider.GitAuthHeader(token))}
	if err := g.prepare(ctx, repo.CloneURL); err != nil {
		return nil, err
	}
	// ls-remote distinguishes a genuinely empty repository from a failed fetch.
	refs, err := g.run(ctx, "ls-remote", "--heads", "origin")
	if err != nil {
		return nil, err
	}
	if refs == "" {
		return &CommitGraph{Commits: []CommitNode{}, Branches: []Branch{}}, nil
	}
	if _, err := g.run(ctx, "fetch", "--quiet", "--no-tags", "--depth=100", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return nil, err
	}
	return g.graph(ctx)
}

func (g *gitRunner) graph(ctx context.Context) (*CommitGraph, error) {
	graph := &CommitGraph{Commits: []CommitNode{}, Branches: []Branch{}}
	refs, err := g.run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/origin/")
	if err != nil {
		return nil, err
	}
	tips := map[string][]string{}
	for _, line := range strings.Split(refs, "\n") {
		sha, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/remotes/origin/")
		graph.Branches = append(graph.Branches, Branch{Name: name, SHA: sha})
		tips[sha] = append(tips[sha], name)
	}
	if len(graph.Branches) == 0 {
		return graph, nil
	}
	// Each record starts with a record separator; the shortstat line, absent
	// for an empty change, follows the fields. Merges count against their
	// first parent, the side an analysis compares them to.
	raw, err := g.run(ctx, "log", "--remotes=origin", "--topo-order", "--max-count=301",
		"--shortstat", "--diff-merges=first-parent", "--format=%x1e%H%x00%P%x00%s%x00%an%x00%cI%x00")
	if err != nil {
		return nil, err
	}
	cut, err := g.shallowCommits(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(raw, "\x1e")[1:] {
		fields := strings.Split(record, "\x00")
		if len(fields) != 6 {
			return nil, fmt.Errorf("invalid commit in graph")
		}
		sha := strings.TrimSpace(fields[0])
		if !commitPattern.MatchString(sha) {
			return nil, fmt.Errorf("invalid commit in graph")
		}
		node := CommitNode{SHA: sha, Parents: strings.Fields(fields[1]), Message: fields[2], Author: fields[3], Date: fields[4], Branches: tips[sha]}
		// A shallow cut has no parent here, so Git would count its whole tree.
		if !cut[sha] {
			node.Stats = parseShortstat(fields[5])
		}
		graph.Commits = append(graph.Commits, node)
	}
	shallow, err := g.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	graph.Limited = shallow == "true" || len(graph.Commits) > 300
	if len(graph.Commits) > 300 {
		graph.Commits = graph.Commits[:300]
	}
	return graph, nil
}

// shallowCommits lists the commits where a shallow history is cut.
func (g *gitRunner) shallowCommits(ctx context.Context) (map[string]bool, error) {
	path, err := g.run(ctx, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.dir, path)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	cut := map[string]bool{}
	for _, sha := range strings.Fields(string(data)) {
		cut[sha] = true
	}
	return cut, nil
}

var shortstatPart = regexp.MustCompile(`(\d+) (file|insertion|deletion)`)

// parseShortstat reads " 3 files changed, 10 insertions(+), 2 deletions(-)";
// an empty line is an empty change.
func parseShortstat(line string) *CommitStats {
	stats := &CommitStats{}
	for _, m := range shortstatPart.FindAllStringSubmatch(line, -1) {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "file":
			stats.Files = n
		case "insertion":
			stats.Additions = n
		case "deletion":
			stats.Deletions = n
		}
	}
	return stats
}
