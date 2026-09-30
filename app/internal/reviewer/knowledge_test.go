package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// With a knowledge base, the reviewer receives the relevant entries and the
// record_knowledge tool, in both modes; the tool never reaches the harness.
func TestReviewRecordsKnowledgeUpdates(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		server, bodies := scripted(t, []toolCall{call("k", KnowledgeTool, `{"entries":[
			{"title":"Refund guard","kind":"risk","paths":["pay/**"],"text":"Refunds skip the amount check when the gateway already did.","reason":"read pay/refund.go"},
			{"title":"Bad","kind":"gossip","paths":[],"text":"x"},
			{"title":"refund GUARD","kind":"risk","paths":["pay/**"],"text":"Corrected text."}
		]}`)})
		r := &model.Report{Knowledge: &model.Knowledge{Path: "PROBE_KNOWLEDGE.md", Entries: []model.KnowledgeEntry{{Title: "Cents", Kind: model.KnowledgeConvention, Paths: []string{}, Text: "Amounts are in cents."}}}}
		h := &fakeHarness{}
		if err := Run(context.Background(), Options{ReadOnly: readOnly, Endpoint: server.URL, Model: "test"}, r, h); err != nil {
			t.Fatal(err)
		}
		if len(h.calls) != 0 {
			t.Fatalf("record_knowledge reached the harness: %v", h.calls)
		}
		body := (*bodies)[0]
		system := body["messages"].([]any)[0].(map[string]any)["content"].(string)
		user := body["messages"].([]any)[1].(map[string]any)["content"].(string)
		tools, _ := json.Marshal(body["tools"])
		if !strings.Contains(system, "Codebase knowledge") || !strings.Contains(user, `"knowledge":[{"title":"Cents"`) || !strings.Contains(string(tools), KnowledgeTool) {
			t.Fatalf("readOnly=%v: knowledge missing from the prompt, input or tools", readOnly)
		}
		if u := r.Knowledge.Updates; len(u) != 1 || u[0].Text != "Corrected text." || u[0].Title != "refund GUARD" {
			t.Fatalf("readOnly=%v: updates %+v", readOnly, u)
		}
	}
	// Without a knowledge base, nothing is offered.
	server, bodies := scripted(t)
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	tools, _ := json.Marshal((*bodies)[0]["tools"])
	if strings.Contains(string(tools), KnowledgeTool) {
		t.Fatal("record_knowledge offered without a knowledge base")
	}
}

func TestRecordKnowledgeBounds(t *testing.T) {
	var updates []model.KnowledgeUpdate
	for _, args := range []string{`{}`, `{"entries":[]}`, `{"entries":[{"title":"t","kind":"note","paths":[],"text":"x","extra":1}]}`} {
		if _, err := recordKnowledge(&updates, []byte(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	for i := 0; i < MaxKnowledgeUpdates+2; i++ {
		entry, _ := json.Marshal(map[string]any{"entries": []map[string]any{{"title": "T" + string(rune('a'+i)), "kind": "note", "paths": []string{}, "text": "x"}}})
		out, err := recordKnowledge(&updates, entry)
		if err != nil {
			t.Fatal(err)
		}
		if i >= MaxKnowledgeUpdates && !strings.Contains(string(out), "at most") {
			t.Fatalf("update %d beyond the session bound: %s", i, out)
		}
	}
	if len(updates) != MaxKnowledgeUpdates {
		t.Fatalf("%d updates kept", len(updates))
	}
}

// The build session lists files, reads through the harness, records entries
// and keeps them when it ends.
func TestBuildKnowledge(t *testing.T) {
	server, bodies := scripted(t,
		[]toolCall{call("l", planListTool, `{"prefix":"pay"}`), call("r", "read_file", `{"path":"pay/refund.go"}`)},
		[]toolCall{call("k", KnowledgeTool, `{"entries":[{"title":"Refund flow","kind":"component","paths":["pay/**"],"text":"Refunds go through the gateway."}]}`)},
	)
	h := &fakeHarness{}
	res, err := BuildKnowledge(context.Background(), Options{Endpoint: server.URL, Model: "test"}, KnowledgeInput{BaseRef: "main", BaseCommit: "abc", Language: "go", Files: []string{"pay/refund.go", "cart/cart.go"}, Existing: []model.KnowledgeEntry{{Title: "Old", Kind: model.KnowledgeNote, Paths: []string{}, Text: "x"}}}, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 1 || res.Updates[0].Title != "Refund flow" || res.Summary != "Done" {
		t.Fatalf("result %+v", res)
	}
	if len(h.calls) != 1 || h.calls[0] != "read_file" {
		t.Fatalf("harness calls %v", h.calls)
	}
	tools, _ := json.Marshal((*bodies)[0]["tools"])
	if strings.Contains(string(tools), planSubmitTool) || !strings.Contains(string(tools), KnowledgeTool) {
		t.Fatal("build tools")
	}
	user := (*bodies)[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, `"files":["pay/refund.go","cart/cart.go"]`) || !strings.Contains(user, `"title":"Old"`) {
		t.Fatalf("initial input %s", user)
	}
	listed := (*bodies)[1]["messages"].([]any)[3].(map[string]any)["content"].(string)
	if !strings.Contains(listed, `"files":["pay/refund.go"]`) {
		t.Fatalf("list_files answer %s", listed)
	}
}
