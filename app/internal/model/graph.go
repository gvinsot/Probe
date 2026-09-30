package model

// Graph statuses.
const (
	GraphBuilt       = "built"       // the graph of the head commit is complete
	GraphPartial     = "partial"     // a limit left part of it out (see Limitations)
	GraphUnavailable = "unavailable" // it could not be built (see Reason)
)

// Graph cache outcomes.
const (
	GraphCacheHit    = "hit"    // read from the graph cache
	GraphCacheStored = "stored" // built, then stored in the graph cache
	GraphCacheOff    = "off"    // no usable cache: built for this run only
)

// Graph is the repository-graph section: the static graph of the head commit
// (components, packages, files, functions, types, external dependencies and
// their relationships) that the AI reviewer queries, and how the change moves
// its structure against the base. It is present unless --graph=false. Like the
// impact index, it is an observation of committed files, never evidence.
type Graph struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Commit string `json:"commit,omitempty"`
	Cache  string `json:"cache,omitempty"`
	// Calls tells whether functions and calls are in the graph: they come from
	// the symbol index, which the graph cannot hold without.
	Calls bool `json:"calls"`
	// Nodes and Edges count the graph by kind.
	Nodes       GraphNodeCounts  `json:"nodes"`
	Edges       GraphEdgeCounts  `json:"edges"`
	Components  []GraphComponent `json:"components"`
	Delta       *GraphDelta      `json:"delta,omitempty"`
	Limitations []string         `json:"limitations,omitempty"`
	Note        string           `json:"note"`
}

// GraphNodeCounts counts the nodes of a graph by kind.
type GraphNodeCounts struct {
	Components   int `json:"components"`
	Packages     int `json:"packages"`
	Files        int `json:"files"`
	Functions    int `json:"functions"`
	Types        int `json:"types"`
	Dependencies int `json:"dependencies"`
}

// GraphEdgeCounts counts the edges of a graph by kind.
type GraphEdgeCounts struct {
	Contains   int `json:"contains"`
	Calls      int `json:"calls"`
	MemberOf   int `json:"member_of"`
	Implements int `json:"implements"`
	Imports    int `json:"imports"`
	DependsOn  int `json:"depends_on"`
	Declares   int `json:"declares"`
}

// GraphComponent is one component of the head commit: a module (a directory
// holding go.mod, package.json, Cargo.toml or pyproject.toml) or a top-level
// directory.
type GraphComponent struct {
	Name      string   `json:"name"`
	Files     int      `json:"files"`
	DependsOn []string `json:"depends_on,omitempty"`
	// Dependencies counts the external dependencies it uses or declares.
	Dependencies int `json:"dependencies"`
}

// GraphDelta is how the structure of the graph moved between the base and
// the head commits: components, packages, types and external dependencies
// added or removed, and dependency and declaration edges added or removed.
// Function-level changes are the diff itself.
type GraphDelta struct {
	BaseCommit   string      `json:"base_commit"`
	Added        []GraphItem `json:"added"`
	Removed      []GraphItem `json:"removed"`
	AddedTotal   int         `json:"added_total"`
	RemovedTotal int         `json:"removed_total"`
}

// GraphItem is a node (Name) or an edge (From, To) of the delta.
type GraphItem struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}
