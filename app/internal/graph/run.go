package graph

import (
	"context"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// RunOptions configure ForReview.
type RunOptions struct {
	// Index is the symbol index of the head commit, for functions and calls.
	Index     *symbols.Index
	Sensitive func(string) bool
	Limits    Limits
	// Store keeps graphs by commit; nil builds them for this run only.
	Store *Store
	// Base is the base commit, whose structural graph the delta compares
	// with; empty for no delta.
	Base string
	// IndexLimitation, when set, says why the index is partial: the call
	// graph built from it is then marked incomplete.
	IndexLimitation string
}

// ForReview returns the graph of the head commit for the reviewer, and the
// report section: the counts, the components, and the delta against the
// base. A graph that cannot be built gives a nil view and an "unavailable"
// section; it never fails the review.
func ForReview(ctx context.Context, repo Tree, head string, o RunOptions) (*View, *model.Graph) {
	if o.Limits.MaxFiles == 0 {
		o.Limits = DefaultLimits()
	}
	g, cache, err := o.get(ctx, repo, head, o.Index)
	if err != nil {
		return nil, &model.Graph{Status: model.GraphUnavailable, Reason: err.Error(), Commit: head,
			Components: []model.GraphComponent{}, Note: "The repository graph could not be built; the reviewer works without it."}
	}
	v := NewView(g)
	s := v.Summary()
	s.Cache = cache
	if o.Base != "" && o.Base != head {
		if base, _, err := o.get(ctx, repo, o.Base, nil); err == nil {
			s.Delta = Delta(base, g)
		} else {
			s.Limitations = append(s.Limitations, "no delta: the graph of the base commit could not be built: "+err.Error())
		}
	}
	return v, s
}

// get reads a graph from the store, or builds and stores it. A stored graph
// with calls serves every request; one without calls serves only a request
// without an index.
func (o RunOptions) get(ctx context.Context, repo Tree, commit string, index *symbols.Index) (*Graph, string, error) {
	if o.Store != nil {
		if g, ok := o.Store.Get(commit, true, o.Limits); ok {
			return g, model.GraphCacheHit, nil
		}
		if index == nil {
			if g, ok := o.Store.Get(commit, false, o.Limits); ok {
				return g, model.GraphCacheHit, nil
			}
		}
	}
	g, err := Build(ctx, repo, commit, Options{Index: index, Sensitive: o.Sensitive, Limits: o.Limits})
	if err != nil {
		return nil, "", err
	}
	if index != nil && o.IndexLimitation != "" {
		g.Complete = false
		g.Limitations = append(g.Limitations, "functions and calls come from a limited symbol index: "+o.IndexLimitation)
	}
	if o.Store == nil {
		return g, model.GraphCacheOff, nil
	}
	if err := o.Store.Put(g, o.Limits); err != nil {
		g.Limitations = append(g.Limitations, "the graph could not be cached: "+err.Error())
		return g, model.GraphCacheOff, nil
	}
	return g, model.GraphCacheStored, nil
}

// HasCalls reports a stored graph with calls for commit.
func (s *Store) HasCalls(commit string, limits Limits) bool {
	if s == nil {
		return false
	}
	_, ok := s.Get(commit, true, limits)
	return ok
}
