package reviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

func TestReadOnlyRejectsExecutionAndFabricatedProof(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, d := range request.Tools {
			name := definitionName(d)
			if name != "submit_hypothesis" && name != AssessTool && !IsReadOnlyTool(name) {
				t.Errorf("unsafe tool offered: %s", name)
			}
		}
		if requests == 1 {
			complete(w,
				call("read", "read_file", `{"path":"main.go"}`),
				call("run", "run_tests", `{}`),
				call("write", "create_test", `{"path":"evil_test.go","content":"malicious"}`),
				call("intent", "run_intent_test", `{}`),
				call("proof", "submit_hypothesis", `{"title":"Fake proof","severity":"high","status":"REPRODUCED","rationale":"Invented","evidence_ids":["invented"],"path":"main.go","line":1}`),
				call("suspicion", "submit_hypothesis", `{"title":"Potential regression","severity":"high","status":"UNVERIFIED","rationale":"Needs a human check","evidence_ids":[],"path":"main.go","line":1}`))
			return
		}
		complete(w)
	}))
	defer server.Close()
	h := &fakeHarness{}
	r := &model.Report{IntentCriteria: []model.IntentCriterion{{ID: "AC-1"}}}
	if err := Run(context.Background(), Options{ReadOnly: true, Endpoint: server.URL, Model: "test"}, r, h); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.calls, ",") != "read_file" {
		t.Fatalf("unsafe dispatch: %v", h.calls)
	}
	if requests != 2 || len(r.Hypotheses) != 1 || r.Hypotheses[0].Status != model.StatusUnverified {
		t.Fatalf("unexpected result: requests=%d hypotheses=%+v", requests, r.Hypotheses)
	}
}

func TestReadOnlyCapabilityGuardDeniesWritesAndUnknownTools(t *testing.T) {
	// A nil harness proves rejection happens before reaching any implementation.
	guard := ReadOnlyTools{}
	for _, name := range []string{"run_tests", "run_test", "run_typecheck", "run_build", "create_test", "run_generated_test", "delete_generated_test", "create_intent_test", "run_intent_test", "future_tool"} {
		if _, err := guard.Call(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	for _, status := range []string{"REPRODUCED", "NOT_REPRODUCED", "DIVERGED", "INTENT_TEST_FAILED"} {
		data, _ := json.Marshal(model.Hypothesis{Status: status})
		if _, err := submitReadOnly(&model.Report{}, data); err == nil {
			t.Errorf("accepted %s", status)
		}
	}
}
