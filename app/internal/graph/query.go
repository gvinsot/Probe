package graph

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Query bounds.
const (
	MaxSearchResults   = 30
	MaxNeighbors       = 100
	MaxNeighborDepth   = 3
	MaxPathLength      = 12
	maxPathVisits      = 200_000
	maxOverviewChanged = 40
	maxOverviewList    = 10
)

// View indexes a graph for queries. It is read-only and safe for concurrent
// use.
type View struct {
	g       *Graph
	byID    map[string]int
	out, in map[int][]int // node -> edge indexes
	byName  map[string][]int
}

// NewView indexes a graph.
func NewView(g *Graph) *View {
	v := &View{g: g, byID: map[string]int{}, out: map[int][]int{}, in: map[int][]int{}, byName: map[string][]int{}}
	for i, n := range g.Nodes {
		v.byID[n.ID] = i
		// Every suffix after a "." or "/" names the node too: "Cart.Total"
		// and "Total" for "cart.Cart.Total", "cart/cart.go" for a file.
		name := strings.ToLower(n.Name)
		v.byName[name] = append(v.byName[name], i)
		for j := 0; j < len(name)-1; j++ {
			if name[j] == '.' || name[j] == '/' {
				v.byName[name[j+1:]] = append(v.byName[name[j+1:]], i)
			}
		}
	}
	for i, e := range g.Edges {
		from, ok1 := v.byID[e.From]
		to, ok2 := v.byID[e.To]
		if ok1 && ok2 {
			v.out[from] = append(v.out[from], i)
			v.in[to] = append(v.in[to], i)
		}
	}
	return v
}

// Graph returns the indexed graph.
func (v *View) Graph() *Graph { return v.g }

// shortName is the last element of a function or type name, lower-cased
// ("Cart.Total" -> "total" is not used: "cart.total" and "total").
func shortName(n Node) string {
	name := strings.ToLower(n.Name)
	if i := strings.LastIndexAny(name, "./"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return ""
}

// NodeRef is a compact description of a node in query answers.
type NodeRef struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Location  string `json:"location,omitempty"`
	Component string `json:"component,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Test      bool   `json:"test,omitempty"`
}

func (v *View) ref(i int) NodeRef {
	n := v.g.Nodes[i]
	loc := n.Path
	if n.Line > 0 {
		loc = fmt.Sprintf("%s:%d", n.Path, n.Line)
	}
	detail := n.Detail
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	comp := ""
	if c, ok := v.byID[n.Component]; ok {
		comp = v.g.Nodes[c].Name
	}
	return NodeRef{ID: n.ID, Kind: n.Kind, Name: n.Name, Location: loc, Component: comp, Detail: detail, Test: n.Test}
}

// Resolve finds the nodes a reference names: a node id, a file or directory
// path, an exact name, or a last name element ("Total" for "Cart.Total").
func (v *View) Resolve(ref string) []int {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if i, ok := v.byID[ref]; ok {
		return []int{i}
	}
	for _, prefix := range []string{"file:", "package:", "component:"} {
		if i, ok := v.byID[prefix+ref]; ok {
			return []int{i}
		}
	}
	if hits := v.byName[strings.ToLower(ref)]; len(hits) > 0 {
		return append([]int(nil), hits...)
	}
	return nil
}

// resolveOne resolves a reference to one node, or explains why it cannot.
func (v *View) resolveOne(ref string) (int, map[string]any) {
	hits := v.Resolve(ref)
	switch {
	case len(hits) == 0:
		return -1, map[string]any{"error": fmt.Sprintf("no node matches %q: use graph_search", ref)}
	case len(hits) > 1:
		var cands []NodeRef
		for _, h := range hits[:min(len(hits), MaxSearchResults)] {
			cands = append(cands, v.ref(h))
		}
		return -1, map[string]any{"ambiguous": true, "candidates": cands, "total": len(hits), "hint": "repeat with the id of one candidate"}
	}
	return hits[0], nil
}

// Search returns the nodes whose name or path contains query (case
// insensitive), exact names first, optionally of one kind.
func (v *View) Search(query, kind string) map[string]any {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return map[string]any{"error": "query is empty"}
	}
	type hit struct {
		i     int
		score int
	}
	var hits []hit
	for i, n := range v.g.Nodes {
		if kind != "" && n.Kind != kind {
			continue
		}
		name, p := strings.ToLower(n.Name), strings.ToLower(n.Path)
		switch {
		case name == q || shortName(n) == q:
			hits = append(hits, hit{i, 0})
		case strings.HasSuffix(name, q) || strings.HasSuffix(p, q):
			hits = append(hits, hit{i, 1})
		case strings.Contains(name, q) || strings.Contains(p, q):
			hits = append(hits, hit{i, 2})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score < hits[b].score
		}
		return len(v.g.Nodes[hits[a].i].Name) < len(v.g.Nodes[hits[b].i].Name)
	})
	out := []NodeRef{}
	for _, h := range hits[:min(len(hits), MaxSearchResults)] {
		out = append(out, v.ref(h.i))
	}
	return map[string]any{"results": out, "total": len(hits), "truncated": len(hits) > len(out)}
}

// Link is one edge in a query answer, seen from the node it was reached from.
type Link struct {
	Direction string  `json:"direction"` // out: node → other; in: other → node
	Kind      string  `json:"kind"`
	Node      NodeRef `json:"node"`
	Via       string  `json:"via,omitempty"` // id of the node the edge starts from, at depth > 1
	Depth     int     `json:"depth"`
	Site      string  `json:"site,omitempty"`
	Count     int     `json:"count,omitempty"`
}

// Neighbors returns the nodes linked to a node, breadth first up to depth
// (1 to 3), following the given edge kinds (all when empty) in the given
// direction ("out", "in" or "both").
func (v *View) Neighbors(ref string, kinds []string, direction string, depth int) map[string]any {
	if direction == "" {
		direction = "both"
	}
	if direction != "out" && direction != "in" && direction != "both" {
		return map[string]any{"error": "direction must be out, in or both"}
	}
	start, problem := v.resolveOne(ref)
	if problem != nil {
		return problem
	}
	if depth < 1 {
		depth = 1
	}
	depth = min(depth, MaxNeighborDepth)
	allowed := map[string]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	seen := map[int]bool{start: true}
	frontier := []int{start}
	links := []Link{}
	total := 0
	for d := 1; d <= depth && len(frontier) > 0; d++ {
		var next []int
		for _, from := range frontier {
			visit := func(edges []int, dir string) {
				for _, ei := range edges {
					e := v.g.Edges[ei]
					if len(allowed) > 0 && !allowed[e.Kind] {
						continue
					}
					other := v.byID[e.To]
					if dir == "in" {
						other = v.byID[e.From]
					}
					if seen[other] {
						continue
					}
					seen[other] = true
					total++
					next = append(next, other)
					if len(links) < MaxNeighbors {
						l := Link{Direction: dir, Kind: e.Kind, Node: v.ref(other), Depth: d, Count: e.Count}
						if d > 1 {
							l.Via = v.g.Nodes[from].ID
						}
						if e.Line > 0 {
							l.Site = fmt.Sprintf("%s:%d", e.Path, e.Line)
						}
						links = append(links, l)
					}
				}
			}
			if direction != "in" {
				visit(v.out[from], "out")
			}
			if direction != "out" {
				visit(v.in[from], "in")
			}
		}
		frontier = next
	}
	return map[string]any{"node": v.ref(start), "links": links, "total": total, "truncated": total > len(links)}
}

// Path returns a shortest chain of edges from one node to another, following
// edges forward (a calls b, a imports b, a depends on b…), within
// MaxPathLength edges; with undirected, edges are followed both ways.
func (v *View) Path(fromRef, toRef string, kinds []string, undirected bool) map[string]any {
	from, problem := v.resolveOne(fromRef)
	if problem != nil {
		problem["side"] = "from"
		return problem
	}
	to, problem := v.resolveOne(toRef)
	if problem != nil {
		problem["side"] = "to"
		return problem
	}
	allowed := map[string]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	type step struct{ prev, edge int }
	prev := map[int]step{from: {-1, -1}}
	frontier := []int{from}
	visits := 0
	for depth := 0; depth < MaxPathLength && len(frontier) > 0; depth++ {
		var next []int
		for _, n := range frontier {
			try := func(ei, other int) {
				if _, ok := prev[other]; ok {
					return
				}
				prev[other] = step{n, ei}
				next = append(next, other)
			}
			for _, ei := range v.out[n] {
				if visits++; visits > maxPathVisits {
					return map[string]any{"found": false, "reason": "search bound reached"}
				}
				if e := v.g.Edges[ei]; len(allowed) == 0 || allowed[e.Kind] {
					try(ei, v.byID[e.To])
				}
			}
			if undirected {
				for _, ei := range v.in[n] {
					if e := v.g.Edges[ei]; len(allowed) == 0 || allowed[e.Kind] {
						try(ei, v.byID[e.From])
					}
				}
			}
		}
		if _, ok := prev[to]; ok {
			break
		}
		frontier = next
	}
	if _, ok := prev[to]; !ok {
		return map[string]any{"found": false, "from": v.ref(from), "to": v.ref(to),
			"reason": fmt.Sprintf("no chain of at most %d edges", MaxPathLength)}
	}
	type hop struct {
		Kind string  `json:"kind"`
		From string  `json:"from"`
		To   NodeRef `json:"to"`
		Site string  `json:"site,omitempty"`
	}
	var hops []hop
	for n := to; n != from; n = prev[n].prev {
		e := v.g.Edges[prev[n].edge]
		h := hop{Kind: e.Kind, From: v.g.Nodes[prev[n].prev].ID, To: v.ref(n)}
		if e.From != v.g.Nodes[prev[n].prev].ID {
			h.Kind += " (reversed)"
		}
		if e.Line > 0 {
			h.Site = fmt.Sprintf("%s:%d", e.Path, e.Line)
		}
		hops = append([]hop{h}, hops...)
	}
	return map[string]any{"found": true, "from": v.ref(from), "path": hops}
}

// Overview summarizes the graph for the reviewer: the components and their
// dependencies, and for each changed file its place in the graph and who
// depends on it outside the diff.
type Overview struct {
	Commit      string              `json:"commit"`
	Complete    bool                `json:"complete"`
	Calls       bool                `json:"calls"`
	Counts      map[string]int      `json:"counts"`
	Components  []ComponentOverview `json:"components"`
	Changed     []ChangedOverview   `json:"changed"`
	Limitations []string            `json:"limitations,omitempty"`
}

// ComponentOverview is one component with its dependencies.
type ComponentOverview struct {
	Name         string   `json:"name"`
	Manifests    string   `json:"manifests,omitempty"`
	Files        int      `json:"files"`
	DependsOn    []string `json:"depends_on,omitempty"`
	DependedBy   []string `json:"depended_on_by,omitempty"`
	Dependencies int      `json:"external_dependencies"`
}

// ChangedOverview places one changed file in the graph.
type ChangedOverview struct {
	File      string `json:"file"`
	Component string `json:"component,omitempty"`
	Package   string `json:"package,omitempty"`
	// PackageDependents are the packages that depend on the file's package
	// (import it), outside that package.
	PackageDependents []string `json:"package_dependents,omitempty"`
	// Importers are files importing this very file (TypeScript, Python, Rust).
	Importers []string `json:"importers,omitempty"`
	// ExternalCallers counts calls into the file's functions from other files,
	// and CrossComponentCallers those from other components.
	ExternalCallers       int      `json:"external_callers"`
	CrossComponentCallers int      `json:"cross_component_callers"`
	Types                 []string `json:"types,omitempty"`
	Missing               bool     `json:"missing,omitempty"` // deleted, or not in the graph
}

// Overview builds the overview for the changed paths.
func (v *View) Overview(changed []string) Overview {
	o := Overview{Commit: v.g.Commit, Complete: v.g.Complete, Calls: v.g.Calls, Counts: map[string]int{}, Limitations: v.g.Limitations}
	for _, n := range v.g.Nodes {
		o.Counts[n.Kind]++
	}
	for i, n := range v.g.Nodes {
		if n.Kind != KindComponent {
			continue
		}
		c := ComponentOverview{Name: n.Name, Manifests: n.Detail}
		external := map[string]bool{} // a dependency both declared and used counts once
		for _, ei := range v.out[i] {
			e := v.g.Edges[ei]
			switch {
			case e.Kind == EdgeContains:
				for _, fi := range v.out[v.byID[e.To]] {
					if v.g.Edges[fi].Kind == EdgeContains {
						c.Files++
					}
				}
			case e.Kind == EdgeDependsOn && strings.HasPrefix(e.To, "component:"):
				c.DependsOn = append(c.DependsOn, v.g.Nodes[v.byID[e.To]].Name)
			case e.Kind == EdgeDependsOn || e.Kind == EdgeDeclares:
				if !external[e.To] {
					external[e.To] = true
					c.Dependencies++
				}
			}
		}
		for _, ei := range v.in[i] {
			if e := v.g.Edges[ei]; e.Kind == EdgeDependsOn {
				c.DependedBy = append(c.DependedBy, v.g.Nodes[v.byID[e.From]].Name)
			}
		}
		o.Components = append(o.Components, c)
	}
	for _, p := range changed {
		if len(o.Changed) == maxOverviewChanged {
			break
		}
		fi, ok := v.byID[fileID(p)]
		if !ok {
			continue // not a source file of the graph, or deleted
		}
		f := v.g.Nodes[fi]
		c := ChangedOverview{File: p}
		if ci, ok := v.byID[f.Component]; ok {
			c.Component = v.g.Nodes[ci].Name
		}
		pkg := packageID(path.Dir(p))
		if pi, ok := v.byID[pkg]; ok {
			c.Package = v.g.Nodes[pi].Name
			for _, ei := range v.in[pi] {
				if e := v.g.Edges[ei]; e.Kind == EdgeDependsOn && len(c.PackageDependents) < maxOverviewList {
					c.PackageDependents = append(c.PackageDependents, v.g.Nodes[v.byID[e.From]].Name)
				}
			}
		}
		for _, ei := range v.in[fi] {
			if e := v.g.Edges[ei]; e.Kind == EdgeImports && len(c.Importers) < maxOverviewList {
				c.Importers = append(c.Importers, v.g.Nodes[v.byID[e.From]].Name)
			}
		}
		for _, ei := range v.out[fi] {
			e := v.g.Edges[ei]
			if e.Kind != EdgeContains {
				continue
			}
			member := v.byID[e.To]
			switch v.g.Nodes[member].Kind {
			case KindType:
				if len(c.Types) < maxOverviewList {
					c.Types = append(c.Types, v.g.Nodes[member].Name)
				}
			case KindFunction:
				for _, ci := range v.in[member] {
					call := v.g.Edges[ci]
					if call.Kind != EdgeCalls {
						continue
					}
					caller := v.g.Nodes[v.byID[call.From]]
					if caller.Path != p {
						c.ExternalCallers++
						if caller.Component != f.Component {
							c.CrossComponentCallers++
						}
					}
				}
			}
		}
		o.Changed = append(o.Changed, c)
	}
	return o
}

// OverviewOf is Overview for the reviewer's input.
func (v *View) OverviewOf(changed []string) any { return v.Overview(changed) }
