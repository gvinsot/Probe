package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/graph"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/knowledge"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

const graphUsage = `usage:
  probe graph build [--repo .] [--commit HEAD] [--out FILE] [--graph-cache DIR]
  probe graph query search QUERY [--kind KIND] [--commit HEAD]
  probe graph query neighbors NODE [--direction out|in|both] [--kinds calls,imports,...] [--depth 1..3] [--commit HEAD]
  probe graph query path FROM TO [--kinds ...] [--undirected] [--commit HEAD]`

// graphCommand builds or queries the repository graph of a commit outside a
// review, for people and for coding agents. It reads committed files only
// and uses the same cache as lint and review.
func graphCommand(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	if len(args) == 0 {
		return fail(errOut, 3, "%s", graphUsage)
	}
	switch args[0] {
	case "build":
		return graphBuild(ctx, args[1:], out, errOut, version)
	case "query":
		return graphQuery(ctx, args[1:], out, errOut, version)
	}
	return fail(errOut, 3, "%s", graphUsage)
}

type graphFlags struct {
	repo, commit, cache *string
	set                 *flag.FlagSet
}

func newGraphFlags(name string, errOut io.Writer) graphFlags {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(errOut)
	return graphFlags{
		repo:   f.String("repo", ".", "repository directory"),
		commit: f.String("commit", "HEAD", "commit (or revision) whose graph is built"),
		cache:  f.String("graph-cache", "", "graph cache directory (default: the user cache directory; \"off\" disables it)"),
		set:    f,
	}
}

// open resolves the commit and returns its graph, from the cache or built.
func (g graphFlags) open(ctx context.Context, errOut io.Writer, version string) (*graph.View, string, int) {
	repo, err := gitrepo.Open(ctx, *g.repo)
	if err != nil {
		return nil, "", fail(errOut, 3, "%v", err)
	}
	commit, err := repo.ResolveCommit(ctx, *g.commit)
	if err != nil {
		return nil, "", fail(errOut, 3, "%v", err)
	}
	explicit := visitedFlags(g.set)
	store, err := openGraphStore(*g.cache, explicit["graph-cache"], repo.Root, "", version, errOut)
	if err != nil {
		return nil, "", fail(errOut, 3, "%v", err)
	}
	var index *symbols.Index
	if !store.HasCalls(commit, graph.DefaultLimits()) {
		if index, _, err = symbols.IndexCommit(ctx, repo, commit, symbols.Options{Sensitive: harness.IsSensitivePath}); err != nil {
			fmt.Fprintf(errOut, "Symbol index unavailable, graph without functions and calls: %v\n", err)
			index = nil
		}
	}
	view, section := graph.ForReview(ctx, repo, commit, graph.RunOptions{Index: index, Sensitive: harness.IsSensitivePath, Store: store})
	if view == nil {
		return nil, "", fail(errOut, 4, "repository graph: %s", section.Reason)
	}
	return view, graphSummary(section), 0
}

func graphBuild(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	g := newGraphFlags("graph build", errOut)
	outFile := g.set.String("out", "", "write the whole graph as JSON to this file (\"-\" for standard output)")
	if err := g.set.Parse(args); err != nil {
		return flagCode(err)
	}
	view, summary, code := g.open(ctx, errOut, version)
	if code != 0 {
		return code
	}
	fmt.Fprintln(errOut, summary)
	if *outFile == "" {
		return 0
	}
	data, err := json.MarshalIndent(view.Graph(), "", " ")
	if err != nil {
		return fail(errOut, 4, "%v", err)
	}
	if *outFile == "-" {
		out.Write(append(data, '\n'))
		return 0
	}
	if err := knowledge.WriteFile(*outFile, append(data, '\n')); err != nil {
		return fail(errOut, 4, "%v", err)
	}
	return 0
}

func graphQuery(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	if len(args) == 0 {
		return fail(errOut, 3, "%s", graphUsage)
	}
	tool := args[0]
	g := newGraphFlags("graph query "+tool, errOut)
	kind := g.set.String("kind", "", "search: node kind (component, package, file, function, type, dependency)")
	direction := g.set.String("direction", "both", "neighbors: out, in or both")
	kinds := g.set.String("kinds", "", "neighbors and path: comma-separated edge kinds (contains, calls, member_of, implements, imports, depends_on, declares)")
	depth := g.set.Int("depth", 1, "neighbors: hops, 1 to 3")
	undirected := g.set.Bool("undirected", false, "path: follow edges both ways")
	// Positional arguments may come before the flags.
	var positional []string
	rest := args[1:]
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		positional, rest = append(positional, rest[0]), rest[1:]
	}
	if err := g.set.Parse(rest); err != nil {
		return flagCode(err)
	}
	positional = append(positional, g.set.Args()...)
	var edgeKinds []string
	if *kinds != "" {
		edgeKinds = strings.Split(*kinds, ",")
	}
	need := map[string]int{"search": 1, "neighbors": 1, "path": 2}[tool]
	if need == 0 || len(positional) != need {
		return fail(errOut, 3, "%s", graphUsage)
	}
	view, _, code := g.open(ctx, errOut, version)
	if code != 0 {
		return code
	}
	var answer map[string]any
	switch tool {
	case "search":
		answer = view.Search(positional[0], *kind)
	case "neighbors":
		answer = view.Neighbors(positional[0], edgeKinds, *direction, *depth)
	case "path":
		answer = view.Path(positional[0], positional[1], edgeKinds, *undirected)
	}
	data, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return fail(errOut, 4, "%v", err)
	}
	out.Write(append(data, '\n'))
	return 0
}
