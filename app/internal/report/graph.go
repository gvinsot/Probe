package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// maxGraphDeltaLines bounds each list of the delta in the Markdown report;
// the JSON keeps up to 100 items.
const maxGraphDeltaLines = 20

// writeGraph renders "## Repository Graph" when r.Graph is present: the size
// of the graph, its components and their dependencies, and how the change
// moves the structure against the base. Every string goes through inline().
func writeGraph(b *bytes.Buffer, r *model.Report) {
	g := r.Graph
	if g == nil {
		return
	}
	line(b, "\n## Repository Graph\n")
	if g.Status == model.GraphUnavailable {
		fmt.Fprintf(b, "The repository graph is unavailable: %s. The reviewer worked without it.\n\n", inline(orNone(g.Reason)))
		return
	}
	n, e := g.Nodes, g.Edges
	fmt.Fprintf(b, "Static graph of the head commit: %d components, %d packages, %d files, %d functions, %d types, %d external dependencies; %d calls, %d imports, %d dependency and %d implementation edges.",
		n.Components, n.Packages, n.Files, n.Functions, n.Types, n.Dependencies, e.Calls, e.Imports, e.DependsOn, e.Implements)
	if !g.Calls {
		b.WriteString(" Functions and calls are not included: no symbol index was available.")
	}
	b.WriteString("\n\n")
	for _, c := range g.Components {
		deps := ""
		if len(c.DependsOn) > 0 {
			deps = "; depends on " + inline(strings.Join(c.DependsOn, ", "))
		}
		fmt.Fprintf(b, "- %s: %d files, %d external dependencies%s\n", inline(c.Name), c.Files, c.Dependencies, deps)
	}
	if d := g.Delta; d != nil {
		if d.AddedTotal == 0 && d.RemovedTotal == 0 {
			line(b, "\nThe change adds or removes no component, package, type, external dependency or dependency edge.")
		} else {
			fmt.Fprintf(b, "\nStructure against the base: %d added, %d removed.\n\n", d.AddedTotal, d.RemovedTotal)
			writeGraphItems(b, "Added", d.Added, d.AddedTotal)
			writeGraphItems(b, "Removed", d.Removed, d.RemovedTotal)
		}
	}
	for _, l := range g.Limitations {
		fmt.Fprintf(b, "- Limitation: %s\n", inline(l))
	}
	fmt.Fprintf(b, "\n%s\n", inline(g.Note))
}

func writeGraphItems(b *bytes.Buffer, label string, items []model.GraphItem, total int) {
	for i, it := range items {
		if i == maxGraphDeltaLines {
			fmt.Fprintf(b, "- %s: %d more (see the JSON report)\n", label, total-i)
			return
		}
		if it.Name != "" {
			fmt.Fprintf(b, "- %s %s: %s\n", label, inline(it.Kind), inline(it.Name))
		} else {
			fmt.Fprintf(b, "- %s %s: %s → %s\n", label, inline(it.Kind), inline(it.From), inline(it.To))
		}
	}
}
