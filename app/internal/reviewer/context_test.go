package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/xrepo"
)

type fakeContext struct{ calls []string }

func (f *fakeContext) Repos() []model.ContextRepo {
	return []model.ContextRepo{{Name: "company/sdk", Role: "payment SDK", Clusters: []string{"payments"}, Ref: "main", Commit: "abc", Files: 3, Status: model.ContextAvailable}}
}
func (f *fakeContext) List(name, prefix string, limit int) ([]string, int, error) {
	f.calls = append(f.calls, "list "+name+" "+prefix)
	return []string{"client.go"}, 1, nil
}
func (f *fakeContext) Read(_ context.Context, name, path string, start, end int) (string, int, bool, error) {
	f.calls = append(f.calls, "read "+name+" "+path)
	if path != "client.go" {
		return "", 0, false, errors.New("unknown file")
	}
	return "1: func Refund(amountCents int) {}\n", 1, false, nil
}
func (f *fakeContext) Search(_ context.Context, name, query string) ([]xrepo.Match, bool, error) {
	f.calls = append(f.calls, "search "+query)
	return []xrepo.Match{{Repo: "company/sdk", Path: "client.go", Line: 1, Text: "func Refund(amountCents int) {}"}}, false, nil
}

// With context repositories, the reviewer receives them, the prompt and the
// three read tools, answered locally in both modes; without, nothing.
func TestReviewerReadsContextRepositories(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		server, bodies := scripted(t, []toolCall{
			call("s", contextSearchTool, `{"query":"Refund"}`),
			call("r", contextReadTool, `{"repo":"company/sdk","path":"client.go","start_line":1}`),
			call("l", contextListTool, `{"repo":"company/sdk"}`),
			call("x", contextReadTool, `{"repo":"company/sdk","path":"nope.go"}`),
		})
		reader := &fakeContext{}
		h := &fakeHarness{}
		r := &model.Report{}
		if err := Run(context.Background(), Options{ReadOnly: readOnly, Endpoint: server.URL, Model: "test", Context: reader}, r, h); err != nil {
			t.Fatal(err)
		}
		if len(h.calls) != 0 || len(reader.calls) != 4 {
			t.Fatalf("readOnly=%v: harness %v, context %v", readOnly, h.calls, reader.calls)
		}
		first := (*bodies)[0]
		system := first["messages"].([]any)[0].(map[string]any)["content"].(string)
		user := first["messages"].([]any)[1].(map[string]any)["content"].(string)
		tools, _ := json.Marshal(first["tools"])
		if !strings.Contains(system, "Cross-repository context") || !strings.Contains(user, `"context_repos":[{"name":"company/sdk"`) || !strings.Contains(string(tools), contextSearchTool) {
			t.Fatalf("readOnly=%v: context missing from prompt, input or tools", readOnly)
		}
		messages := (*bodies)[1]["messages"].([]any)
		results := ""
		for _, m := range messages {
			if m.(map[string]any)["role"] == "tool" {
				results += m.(map[string]any)["content"].(string) + "\n"
			}
		}
		if !strings.Contains(results, "amountCents int") || !strings.Contains(results, `"files":["client.go"]`) || !strings.Contains(results, "unknown file") {
			t.Fatalf("tool results %s", results)
		}
		audited := 0
		for _, e := range r.Audit {
			if isContextTool(e.Tool) {
				audited++
			}
		}
		if audited != 4 {
			t.Fatalf("context reads are audited: %d", audited)
		}
	}
	server, bodies := scripted(t)
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	if tools, _ := json.Marshal((*bodies)[0]["tools"]); strings.Contains(string(tools), contextReadTool) {
		t.Fatal("context tools offered without context")
	}
}
