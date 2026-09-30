package graph

import (
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// maxDeltaItems bounds each list of the delta in the report.
const maxDeltaItems = 100

// Summary is the report section of a graph.
func (v *View) Summary() *model.Graph {
	g := v.g
	s := &model.Graph{Status: model.GraphBuilt, Commit: g.Commit, Calls: g.Calls,
		Components: []model.GraphComponent{}, Limitations: g.Limitations,
		Note: "Static graph of the committed files of the head commit, queried by the AI reviewer; relationships are observations, never evidence."}
	if !g.Complete {
		s.Status = model.GraphPartial
	}
	for _, n := range g.Nodes {
		switch n.Kind {
		case KindComponent:
			s.Nodes.Components++
		case KindPackage:
			s.Nodes.Packages++
		case KindFile:
			s.Nodes.Files++
		case KindFunction:
			s.Nodes.Functions++
		case KindType:
			s.Nodes.Types++
		case KindDependency:
			s.Nodes.Dependencies++
		}
	}
	for _, e := range g.Edges {
		switch e.Kind {
		case EdgeContains:
			s.Edges.Contains++
		case EdgeCalls:
			s.Edges.Calls++
		case EdgeMemberOf:
			s.Edges.MemberOf++
		case EdgeImplements:
			s.Edges.Implements++
		case EdgeImports:
			s.Edges.Imports++
		case EdgeDependsOn:
			s.Edges.DependsOn++
		case EdgeDeclares:
			s.Edges.Declares++
		}
	}
	for _, c := range v.Overview(nil).Components {
		s.Components = append(s.Components, model.GraphComponent{Name: c.Name, Files: c.Files, DependsOn: c.DependsOn, Dependencies: c.Dependencies})
	}
	return s
}

// structural node and edge kinds compared by Delta.
var (
	deltaNodes = map[string]bool{KindComponent: true, KindPackage: true, KindType: true, KindDependency: true}
	// implements edges come from the symbol index, which the base graph is
	// built without: comparing them would report every one as added.
	deltaEdges = map[string]bool{EdgeDependsOn: true, EdgeDeclares: true}
)

// Delta compares the structure of two graphs of the same repository.
func Delta(base, head *Graph) *model.GraphDelta {
	d := &model.GraphDelta{BaseCommit: base.Commit, Added: []model.GraphItem{}, Removed: []model.GraphItem{}}
	name := func(g *Graph) map[string]string {
		m := map[string]string{}
		for _, n := range g.Nodes {
			m[n.ID] = n.Name
		}
		return m
	}
	baseNames, headNames := name(base), name(head)
	items := func(g *Graph, names map[string]string) map[string]model.GraphItem {
		m := map[string]model.GraphItem{}
		for _, n := range g.Nodes {
			if deltaNodes[n.Kind] {
				// A type is identified by its package and name, not its file.
				m[n.ID] = model.GraphItem{Kind: n.Kind, Name: n.Name + typeWhere(n)}
			}
		}
		for _, e := range g.Edges {
			if deltaEdges[e.Kind] {
				m[e.From+"\x00"+e.Kind+"\x00"+e.To] = model.GraphItem{Kind: e.Kind, From: names[e.From], To: names[e.To]}
			}
		}
		return m
	}
	before, after := items(base, baseNames), items(head, headNames)
	var added, removed []string
	for k := range after {
		if _, ok := before[k]; !ok {
			added = append(added, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	order := func(keys []string, m map[string]model.GraphItem) []model.GraphItem {
		sort.Slice(keys, func(i, j int) bool {
			a, b := m[keys[i]], m[keys[j]]
			if rank(a.Kind) != rank(b.Kind) {
				return rank(a.Kind) < rank(b.Kind)
			}
			return keys[i] < keys[j]
		})
		out := []model.GraphItem{}
		for _, k := range keys[:min(len(keys), maxDeltaItems)] {
			out = append(out, m[k])
		}
		return out
	}
	d.AddedTotal, d.RemovedTotal = len(added), len(removed)
	d.Added, d.Removed = order(added, after), order(removed, before)
	return d
}

// rank lists the most structural changes first.
func rank(kind string) int {
	switch kind {
	case KindComponent:
		return 0
	case KindDependency:
		return 1
	case EdgeDependsOn:
		return 2
	case EdgeDeclares:
		return 3
	case KindPackage:
		return 4
	case EdgeImplements:
		return 5
	}
	return 6
}

func typeWhere(n Node) string {
	if n.Kind != KindType || n.Path == "" {
		return ""
	}
	return " (" + strings.TrimSpace(n.Detail+" in "+n.Path) + ")"
}
