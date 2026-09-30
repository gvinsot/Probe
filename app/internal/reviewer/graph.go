package reviewer

import (
	"bytes"
	"encoding/json"
	"errors"
)

// GraphReader answers the repository-graph tools. graph.View implements it.
type GraphReader interface {
	OverviewOf(changed []string) any
	Search(query, kind string) map[string]any
	Neighbors(ref string, kinds []string, direction string, depth int) map[string]any
	Path(from, to string, kinds []string, undirected bool) map[string]any
}

// Repository-graph tools, answered locally from the graph of the head commit.
const (
	graphSearchTool    = "graph_search"
	graphNeighborsTool = "graph_neighbors"
	graphPathTool      = "graph_path"
)

const graphPrompt = `
Repository graph: the input field "graph" places the change in the whole repository: its components (modules or top-level directories) and how they depend on each other and on external dependencies, and for each changed file its package, the packages that depend on it, the files that import it, and how many calls reach its functions from other files and other components. Query the graph of the head commit to reason beyond the diff: graph_search finds components, packages, files, functions, types and dependencies by name; graph_neighbors follows their relationships (contains, calls, member_of, implements, imports, depends_on, declares) in either direction, up to 3 hops; graph_path finds how one node reaches another (a call chain, an import or dependency path). Use it to find who is affected by a changed contract, which component a change crosses into, which types implement a changed interface, and which callers or tests exist outside the diff, then read the code you need. The graph is a static observation of committed files: an edge is a place to look, not a defect, and a missing edge is not proof that none exists (dynamic dispatch, reflection and generated code are not resolved). A concern you find through it is submitted with submit_hypothesis like any other, UNVERIFIED unless you verify it.`

func isGraphTool(name string) bool {
	return name == graphSearchTool || name == graphNeighborsTool || name == graphPathTool
}

// graphTools are the definitions of the repository-graph tools.
func graphTools() []map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	kinds := map[string]any{"type": "array", "maxItems": 7, "description": "Edge kinds to follow (default: all)",
		"items": map[string]any{"type": "string", "enum": []string{"contains", "calls", "member_of", "implements", "imports", "depends_on", "declares"}}}
	tool := func(name, description string, properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": map[string]any{
			"type": "object", "additionalProperties": false, "properties": properties, "required": required,
		}}}
	}
	return []map[string]any{
		tool(graphSearchTool, "Find nodes of the repository graph whose name or path contains the query (at most 30): components, packages, files, functions, types, external dependencies.",
			map[string]any{"query": str("Name or path fragment, e.g. Cart.Total, internal/payment, react"),
				"kind": map[string]any{"type": "string", "enum": []string{"component", "package", "file", "function", "type", "dependency"}, "description": "Optional node kind"}}, "query"),
		tool(graphNeighborsTool, "List the nodes linked to a node of the repository graph, breadth first up to depth 3 (at most 100): e.g. callers of a function (kinds [calls], direction in), packages depending on a package (depends_on, in), what a component depends on (depends_on, out), implementations of an interface (implements, in).",
			map[string]any{"node": str("Node id from graph_search or the input, or an exact name or path"),
				"direction": map[string]any{"type": "string", "enum": []string{"out", "in", "both"}, "description": "out: from the node; in: towards it (default both)"},
				"kinds":     kinds,
				"depth":     map[string]any{"type": "integer", "minimum": 1, "maximum": 3, "description": "Hops (default 1)"}}, "node"),
		tool(graphPathTool, "Find a shortest chain of edges from one node of the repository graph to another (at most 12 edges), following edges forward (calls, imports, depends_on…), or in both directions with undirected.",
			map[string]any{"from": str("Start node id, name or path"), "to": str("End node id, name or path"), "kinds": kinds,
				"undirected": map[string]any{"type": "boolean", "description": "Follow edges both ways (default false)"}}, "from", "to"),
	}
}

// callGraph answers one repository-graph tool call.
func callGraph(g GraphReader, name string, data []byte) (json.RawMessage, error) {
	if g == nil {
		return nil, errors.New("tool is not available")
	}
	var a struct {
		Query      string   `json:"query"`
		Kind       string   `json:"kind"`
		Node       string   `json:"node"`
		Direction  string   `json:"direction"`
		Kinds      []string `json:"kinds"`
		Depth      int      `json:"depth"`
		From       string   `json:"from"`
		To         string   `json:"to"`
		Undirected bool     `json:"undirected"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, errors.New("arguments must match the tool schema")
	}
	if len(a.Kinds) > 7 {
		return nil, errors.New("too many edge kinds")
	}
	var out map[string]any
	switch name {
	case graphSearchTool:
		out = g.Search(a.Query, a.Kind)
	case graphNeighborsTool:
		out = g.Neighbors(a.Node, a.Kinds, a.Direction, a.Depth)
	case graphPathTool:
		out = g.Path(a.From, a.To, a.Kinds, a.Undirected)
	default:
		return nil, errors.New("tool is not available")
	}
	out["note"] = "Static graph of the committed files of the head commit: observations, not evidence."
	return json.Marshal(out)
}
