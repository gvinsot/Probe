package cli

// Repository graph: with --graph (the default) lint and review build the
// graph of the head commit (components, packages, files, functions, types,
// external dependencies and their relationships) from committed Git objects,
// record its summary and its structural delta against the base in the report,
// and give it to the reviewer, which queries it with graph_search,
// graph_neighbors and graph_path. Graphs are cached by commit in
// --graph-cache (default: the user cache directory), so the graph of a base
// tip is built once for every review against it.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gvinsot/Probe/app/internal/execcache"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/graph"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// graphCacheOff disables the graph cache.
const graphCacheOff = "off"

// defaultGraphCache is the user cache directory, or "" when the system has
// none.
func defaultGraphCache() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "probe", "graph")
}

// openGraphStore validates the graph cache directory. An explicit directory
// that is unusable is an argument error; the default one only a warning.
func openGraphStore(dir string, explicit bool, repoRoot, output, version string, errOut io.Writer) (*graph.Store, error) {
	if dir == graphCacheOff {
		return nil, nil
	}
	if dir == "" {
		if dir = defaultGraphCache(); dir == "" {
			return nil, nil
		}
	}
	canonical, err := execcache.ValidateDir(dir, repoRoot, output)
	if err == nil {
		var identity string
		if identity, err = execcache.ToolIdentity(version); err == nil {
			return graph.OpenStore(canonical, identity), nil
		}
	}
	if explicit {
		return nil, fmt.Errorf("--graph-cache: %w", err)
	}
	fmt.Fprintf(errOut, "Repository graph not cached: %v\n", err)
	return nil, nil
}

// buildGraph returns the graph view for the reviewer (nil when none) and the
// report section (nil with --graph=false). It returns an error only when ctx
// is cancelled.
func buildGraph(ctx context.Context, repo *gitrepo.Repository, change model.Change, impact impactResult, store *graph.Store, enabled bool) (*graph.View, *model.Graph, error) {
	if !enabled {
		return nil, nil, nil
	}
	index := impact.index
	if index == nil && !store.HasCalls(change.HeadCommit, graph.DefaultLimits()) {
		// No indexable file changed, or --impact=false: index the head commit
		// for the graph. A failed index leaves a graph without calls.
		if x, _, err := symbols.IndexCommit(ctx, repo, change.HeadCommit, symbols.Options{Sensitive: harness.IsSensitivePath}); err == nil {
			index = x
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	opts := graph.RunOptions{Index: index, Sensitive: harness.IsSensitivePath, Store: store, Base: change.BaseCommit}
	if index == impact.index && impact.report != nil && impact.report.Status != model.ImpactIndexed {
		opts.IndexLimitation = impact.report.Reason
	}
	view, section := graph.ForReview(ctx, repo, change.HeadCommit, opts)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return view, section, nil
}

// graphSummary is the line printed after the analysis.
func graphSummary(s *model.Graph) string {
	if s == nil {
		return ""
	}
	if s.Status == model.GraphUnavailable {
		return "Repository graph: unavailable: " + s.Reason
	}
	line := fmt.Sprintf("Repository graph: %d components, %d files, %d functions, %d types, %d external dependencies (cache: %s)",
		s.Nodes.Components, s.Nodes.Files, s.Nodes.Functions, s.Nodes.Types, s.Nodes.Dependencies, s.Cache)
	if s.Delta != nil && (s.Delta.AddedTotal > 0 || s.Delta.RemovedTotal > 0) {
		line += fmt.Sprintf("; structure against the base: %d added, %d removed", s.Delta.AddedTotal, s.Delta.RemovedTotal)
	}
	return line
}
