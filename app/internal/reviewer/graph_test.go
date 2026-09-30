package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

type fakeGraph struct {
	calls   []string
	changed []string
}

func (f *fakeGraph) OverviewOf(changed []string) any {
	f.changed = changed
	return map[string]any{"components": []string{"svc", "web"}, "changed": []map[string]any{{"file": "svc/cart/cart.go", "package_dependents": []string{"example.test/shop/api"}}}}
}
func (f *fakeGraph) Search(query, kind string) map[string]any {
	f.calls = append(f.calls, "search "+query+" "+kind)
	return map[string]any{"results": []map[string]string{{"id": "function:x.Total", "name": "Cart.Total"}}}
}
func (f *fakeGraph) Neighbors(ref string, kinds []string, direction string, depth int) map[string]any {
	f.calls = append(f.calls, "neighbors "+ref+" "+strings.Join(kinds, ",")+" "+direction)
	return map[string]any{"links": []map[string]string{{"kind": "calls", "node": "api.Handle"}}}
}
func (f *fakeGraph) Path(from, to string, kinds []string, undirected bool) map[string]any {
	f.calls = append(f.calls, "path "+from+" "+to)
	return map[string]any{"found": true}
}

// With a repository graph, the reviewer receives the overview, the prompt and
// the three graph tools, answered locally in both modes and audited; without
// one, nothing.
func TestReviewerQueriesRepositoryGraph(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		server, bodies := scripted(t, []toolCall{
			call("s", graphSearchTool, `{"query":"Total","kind":"function"}`),
			call("n", graphNeighborsTool, `{"node":"Cart.Total","kinds":["calls"],"direction":"in","depth":2}`),
			call("p", graphPathTool, `{"from":"Handle","to":"Cart.Total"}`),
			call("x", graphNeighborsTool, `{"node":"Cart.Total","unknown":1}`),
		})
		g := &fakeGraph{}
		h := &fakeHarness{}
		r := &model.Report{Change: model.Change{Files: []model.ChangedFile{{Path: "svc/cart/cart.go", Status: "M"}}}}
		if err := Run(context.Background(), Options{ReadOnly: readOnly, Endpoint: server.URL, Model: "test", Graph: g}, r, h); err != nil {
			t.Fatal(err)
		}
		if len(h.calls) != 0 || len(g.calls) != 3 || strings.Join(g.changed, ",") != "svc/cart/cart.go" {
			t.Fatalf("readOnly=%v: harness %v, graph %v, changed %v", readOnly, h.calls, g.calls, g.changed)
		}
		first := (*bodies)[0]
		system := first["messages"].([]any)[0].(map[string]any)["content"].(string)
		user := first["messages"].([]any)[1].(map[string]any)["content"].(string)
		tools, _ := json.Marshal(first["tools"])
		if !strings.Contains(system, "Repository graph") || !strings.Contains(user, `"graph":{"changed"`) || !strings.Contains(string(tools), graphPathTool) {
			t.Fatalf("readOnly=%v: graph missing from prompt, input or tools", readOnly)
		}
		results := ""
		for _, m := range (*bodies)[1]["messages"].([]any) {
			if m.(map[string]any)["role"] == "tool" {
				results += m.(map[string]any)["content"].(string) + "\n"
			}
		}
		if !strings.Contains(results, "api.Handle") || !strings.Contains(results, "observations, not evidence") || !strings.Contains(results, "arguments must match the tool schema") {
			t.Fatalf("tool results %s", results)
		}
		audited := 0
		for _, e := range r.Audit {
			if isGraphTool(e.Tool) {
				audited++
			}
		}
		if audited != 4 {
			t.Fatalf("graph queries are audited: %d", audited)
		}
	}
	server, bodies := scripted(t)
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	first := (*bodies)[0]
	if tools, _ := json.Marshal(first["tools"]); strings.Contains(string(tools), graphSearchTool) {
		t.Fatal("graph tools offered without a graph")
	}
	if user := first["messages"].([]any)[1].(map[string]any)["content"].(string); strings.Contains(user, `"graph"`) {
		t.Fatal("graph input without a graph")
	}
}
