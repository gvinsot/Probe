// Package graph builds the repository graph: a persistent representation of a
// commit as components, packages, files, functions, types and external
// dependencies, linked by containment, calls, imports and dependencies.
//
// The AI reviewer queries it (graph_search, graph_neighbors, graph_path) to
// reason beyond the diff: who depends on a changed package, which component a
// change crosses into, which types implement an interface, how a changed
// function is reached. Every answer is a static observation of committed
// files, never evidence that a behavior holds.
//
// A graph describes exactly one commit and is built from its Git objects,
// never from a working tree: the same commit always gives the same graph, so
// graphs are cached by commit (Store). Function and call nodes come from the
// static symbol index (package symbols); types, imports, components and
// declared dependencies from a light per-language extraction (extract.go).
package graph

// Schema identifies the format of a serialized graph.
const Schema = "probe-graph/v1"

// Node kinds.
const (
	KindComponent  = "component"  // a module or top-level directory
	KindPackage    = "package"    // a Go package, or a source directory
	KindFile       = "file"       // a source file
	KindFunction   = "function"   // a function, method or interface method
	KindType       = "type"       // a struct, interface, class, trait, enum or type alias
	KindDependency = "dependency" // an external module or package
)

// Edge kinds. From → To.
const (
	EdgeContains   = "contains"   // component → package → file → function or type
	EdgeCalls      = "calls"      // function → function (or use as a value)
	EdgeMemberOf   = "member_of"  // method → its type
	EdgeImplements = "implements" // Go type → interface it implements
	EdgeImports    = "imports"    // file → package, file, or dependency
	EdgeDependsOn  = "depends_on" // package → package, component → component or dependency
	EdgeDeclares   = "declares"   // component → dependency declared in its manifest
)

// Graph is the repository graph of one commit.
type Graph struct {
	Schema string `json:"schema"`
	Commit string `json:"commit"`
	// Complete is false when a limit left part of the repository or of the
	// call graph out; Limitations says what.
	Complete    bool     `json:"complete"`
	Limitations []string `json:"limitations,omitempty"`
	// Calls tells whether the function and call nodes are present: they come
	// from the symbol index, which a structural graph does without.
	Calls bool   `json:"calls"`
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one entity of the graph.
type Node struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Name is the display name: a component or package path, a file path, a
	// function "pkg.F" or "T.M", a type, or a dependency "npm:react".
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Language  string `json:"language,omitempty"`
	Component string `json:"component,omitempty"` // id of the component
	// Detail is the function signature, the type kind (struct, interface,
	// class, trait, enum, alias), the manifest of a component, or the
	// ecosystem of a dependency.
	Detail   string `json:"detail,omitempty"`
	Test     bool   `json:"test,omitempty"`
	Exported bool   `json:"exported,omitempty"`
}

// Edge is one relationship. Count is the number of sites it stands for (calls,
// imports) when more than one.
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line,omitempty"`
	Count int    `json:"count,omitempty"`
}

// Node identifiers.
func componentID(dir string) string { return "component:" + dir }
func packageID(dir string) string   { return "package:" + dir }
func fileID(path string) string     { return "file:" + path }
func functionID(key string) string  { return "function:" + key }
func typeID(key string) string      { return "type:" + key }
func dependencyID(ecosystem, name string) string {
	return "dependency:" + ecosystem + ":" + name
}
