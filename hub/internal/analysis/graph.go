package analysis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// CommitNode preserves Git's parent links, including both sides of merges.
type CommitNode struct {
	SHA      string   `json:"sha"`
	Parents  []string `json:"parents"`
	Message  string   `json:"message"`
	Author   string   `json:"author"`
	Date     string   `json:"date"`
	Branches []string `json:"branches"`
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
func (r *Runner) Graph(ctx context.Context, userKey, repoKey string) (*CommitGraph, error) {
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
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "swiftproof-graph-")
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
	raw, err := g.run(ctx, "log", "--remotes=origin", "--topo-order", "--max-count=301", "--format=%H%x00%P%x00%s%x00%an%x00%cI%x00")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(raw, "\x00")
	for len(fields) >= 6 {
		sha := strings.TrimSpace(fields[0])
		if !commitPattern.MatchString(sha) {
			return nil, fmt.Errorf("invalid commit in graph")
		}
		graph.Commits = append(graph.Commits, CommitNode{SHA: sha, Parents: strings.Fields(fields[1]), Message: fields[2], Author: fields[3], Date: fields[4], Branches: tips[sha]})
		fields = fields[5:]
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
