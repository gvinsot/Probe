package cli

// Cross-repository context: the trusted policy's "context" key names other
// repositories (directly or through repository clusters) that the reviewer
// may read while reviewing a change. probe context check shows how each one
// resolves before a review needs it.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/xrepo"
)

// repoPaths collects repeated --context-repo NAME=PATH flags.
type repoPaths map[string]string

func (p repoPaths) String() string { return "" }

func (p repoPaths) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(path) == "" {
		return errors.New("use NAME=PATH, e.g. company/shared-types=../shared-types")
	}
	p[strings.TrimSpace(name)] = strings.TrimSpace(path)
	return nil
}

// contextFlags are the cross-repository flags shared by review and context check.
type contextFlags struct {
	paths    repoPaths
	dir      *string
	fetch    *bool
	clusters *string
	enabled  *bool
}

func addContextFlags(f *flag.FlagSet) contextFlags {
	c := contextFlags{paths: repoPaths{}}
	f.Var(c.paths, "context-repo", "review: local checkout of a context repository, NAME=PATH (repeatable)")
	c.dir = f.String("context-dir", "", "review: directory holding context repository checkouts as DIR/owner/repo or DIR/repo")
	c.fetch = f.Bool("fetch-context", false, "review: shallow-fetch the policy url of a context repository that has no local checkout")
	c.clusters = f.String("clusters", "", "review: operator JSON file of shared repository cluster definitions")
	c.enabled = f.Bool("context", true, "review: read the cross-repository context the policy declares; --context=false disables it")
	return c
}

// selfName is the owner/repo of the reviewed repository, from its origin
// remote, so that a cluster listing it does not make it its own context.
func selfName(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	u := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	if i := strings.LastIndexAny(u, ":/"); i >= 0 {
		rest := u[:i]
		if j := strings.LastIndexAny(rest, ":/"); j >= 0 {
			return rest[j+1:] + "/" + u[i+1:]
		}
	}
	return ""
}

// openContext resolves the policy's context. It returns nil without error
// when the policy declares none. The caller removes temp.
func openContext(ctx context.Context, cfg config.Config, repo *gitrepo.Repository, flags contextFlags, temp string) (*xrepo.Set, []model.ContextRepo, error) {
	if cfg.Context == nil || !*flags.enabled {
		return nil, nil, nil
	}
	var shared map[string]config.Cluster
	if *flags.clusters != "" {
		data, err := readLimited(*flags.clusters, 1<<20)
		if err != nil {
			return nil, nil, fmt.Errorf("clusters: %w", err)
		}
		if shared, err = config.DecodeClusters(data); err != nil {
			return nil, nil, err
		}
	}
	entries, err := cfg.Context.ResolveContext(shared, selfName(ctx, repo.Root))
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	set, records := xrepo.Open(ctx, entries, xrepo.Options{Paths: flags.paths, Dir: *flags.dir, Fetch: *flags.fetch, Temp: temp})
	return set, records, nil
}

func contextSummary(records []model.ContextRepo) string {
	available := 0
	for _, r := range records {
		if r.Status == model.ContextAvailable {
			available++
		}
	}
	return fmt.Sprintf("Cross-repository context: %d of %d repositories available.", available, len(records))
}

func contextCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		return fail(errOut, 3, "usage: probe context check [--base main] [--context-dir DIR] [--context-repo NAME=PATH] [--clusters FILE] [--fetch-context]")
	}
	f := flag.NewFlagSet("context check", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	base := f.String("base", "main", "branch or revision whose tip supplies the trusted policy")
	policyPath := f.String("config", "", "explicit trusted local configuration (default: policy at the tip of --base)")
	flags := addContextFlags(f)
	if err := f.Parse(args[1:]); err != nil {
		return flagCode(err)
	}
	repo, err := gitrepo.Open(ctx, *repoPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	start, err := repo.Analyze(ctx, *base, *base, false)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	commit := start.BaseRefCommit
	if commit == "" {
		commit = start.BaseCommit
	}
	cfg, _, err := loadPolicy(ctx, repo, commit, *policyPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if cfg.Context == nil {
		fmt.Fprintf(out, "The policy at %s declares no cross-repository context.\n", start.BaseRef)
		return 0
	}
	temp, err := os.MkdirTemp("", "probe-context-")
	if err != nil {
		return fail(errOut, 4, "%v", err)
	}
	defer os.RemoveAll(temp)
	_, records, err := openContext(ctx, cfg, repo, flags, temp)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	code := 0
	for _, r := range records {
		clusters := "direct"
		if len(r.Clusters) > 0 {
			clusters = "cluster " + strings.Join(r.Clusters, ", ")
		}
		if r.Status == model.ContextAvailable {
			fmt.Fprintf(out, "available    %s (%s) at %s %s, %s: %d files", r.Name, clusters, r.Ref, shortCommit(r.Commit), r.Source, r.Files)
			if r.Reason != "" {
				fmt.Fprintf(out, "; %s", r.Reason)
			}
			fmt.Fprintln(out)
		} else {
			code = 1
			fmt.Fprintf(out, "unavailable  %s (%s): %s\n", r.Name, clusters, r.Reason)
		}
	}
	fmt.Fprintln(out, contextSummary(records))
	return code
}
