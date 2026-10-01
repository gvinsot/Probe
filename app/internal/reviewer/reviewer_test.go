package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
	reports "github.com/gvinsot/Probe/app/internal/report"
)

type fakeHarness struct{ calls []string }

func (f *fakeHarness) Call(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	f.calls = append(f.calls, name)
	return json.RawMessage(`{"content":"observed"}`), nil
}

func complete(w http.ResponseWriter, calls ...toolCall) {
	m := message{Role: "assistant", Content: "Done", ToolCalls: calls}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m, "finish_reason": "stop"}}})
}

func call(id, name, args string) toolCall {
	c := toolCall{ID: id, Type: "function"}
	c.Function.Name = name
	c.Function.Arguments = args
	return c
}

func TestToolLoopDoesNotTrustFabricatedProof(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer api-secret" {
			t.Error("authorization missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["max_completion_tokens"] != float64(defaultMaxTokens) {
			t.Error("missing token bound")
		}
		switch requests {
		case 1:
			complete(w, call("a", "read_file", `{"path":"main.go"}`))
		case 2:
			complete(w, call("b", "submit_hypothesis", `{"title":"Claim","severity":"high","status":"REPRODUCED","rationale":"Model assertion","evidence_ids":["invented"],"path":"main.go","line":1}`))
		default:
			complete(w)
		}
	}))
	defer server.Close()
	r := &model.Report{}
	h := &fakeHarness{}
	if err := Run(context.Background(), Options{Endpoint: server.URL + "/v1", Model: "test", APIKey: "api-secret"}, r, h); err != nil {
		t.Fatal(err)
	}
	if requests != 3 || len(h.calls) != 1 || h.calls[0] != "read_file" || len(r.Hypotheses) != 1 {
		t.Fatalf("unexpected loop: requests %d calls %v hypotheses %v", requests, h.calls, r.Hypotheses)
	}
	reports.Finalize(r, true)
	if r.Hypotheses[0].Status != "UNVERIFIED" || len(r.ReproducedIssues) != 0 {
		t.Fatal("model assertion became proof")
	}
}

func TestEndpointRejectsUnsafeConfiguration(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1", "file:///tmp/provider", "https://user:password@example.com/v1", "https://example.com/v1?key=secret", "https://example.com/v1#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			if err := Validate(Options{Endpoint: endpoint, Model: "test"}); err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
	_, endpoint, err := normalize(Options{Endpoint: "http://localhost:1234/v1", Model: "test"})
	if err != nil || endpoint != "http://127.0.0.1:1234/v1/chat/completions" {
		t.Fatalf("local endpoint %s %v", endpoint, err)
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var reached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true); complete(w) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", APIKey: "top-secret", AllowInsecureHTTP: true}, &model.Report{}, &fakeHarness{})
	if err == nil || reached.Load() {
		t.Fatal("redirect followed")
	}
	if strings.Contains(err.Error(), "top-secret") {
		t.Fatal("error leaked API key")
	}
}

func TestInitialPromptMasksSensitiveFilesAndKnownCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		for _, secret := range []string{"plain-sensitive-value", "specific-api-key"} {
			if strings.Contains(string(encoded), secret) {
				t.Errorf("leaked %s", secret)
			}
		}
		complete(w)
	}))
	defer server.Close()
	r := &model.Report{Intent: "specific-api-key", Change: model.Change{Files: []model.ChangedFile{{Path: ".env", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Content: "plain-sensitive-value"}}}}}}}}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", APIKey: "specific-api-key"}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetsFailClosed(t *testing.T) {
	t.Run("iteration", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, call("a", "run_build", `{}`)) }))
		defer server.Close()
		r := &model.Report{}
		h := &fakeHarness{}
		if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", MaxIterations: 1}, r, h); err != nil {
			t.Fatal(err)
		}
		if len(r.Unverified) == 0 || len(h.calls) != 1 {
			t.Fatal("budget exhaustion not recorded")
		}
	})
	t.Run("input", func(t *testing.T) {
		var reached bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; complete(w) }))
		defer server.Close()
		r := &model.Report{Intent: strings.Repeat("x", 4000)}
		if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", MaxInputBytes: 1024}, r, &fakeHarness{}); err != nil {
			t.Fatal(err)
		}
		if reached || len(r.Unverified) == 0 {
			t.Fatal("oversized request sent")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}))
		defer server.Close()
		err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", Timeout: 20 * time.Millisecond}, &model.Report{}, &fakeHarness{})
		if err == nil {
			t.Fatal("timeout ignored")
		}
	})
}

func TestTruncatedResponsesAreRetriedInSmallerSteps(t *testing.T) {
	// Two responses cut off by the length limit are dropped, with a reminder
	// each; the third goes on. A tool call of a cut-off response never runs.
	var requests int
	var reminders []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1]
		if last.Content == truncationReminder {
			reminders = append(reminders, requests)
		}
		if requests <= 2 {
			m := message{Role: "assistant", ToolCalls: []toolCall{call("cut", "run_build", `{"pa`)}}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m, "finish_reason": "length"}}})
			return
		}
		if requests == 3 {
			complete(w, call("a", "run_build", `{}`))
			return
		}
		complete(w)
	}))
	defer server.Close()
	r := &model.Report{}
	h := &fakeHarness{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, r, h); err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 2 || reminders[0] != 2 || reminders[1] != 3 || len(h.calls) != 1 || len(r.Unverified) != 0 {
		t.Fatalf("reminders %v, harness calls %v, unverified %v", reminders, h.calls, r.Unverified)
	}

	// A third cut-off response ends the investigation, recorded as incomplete.
	requests = 0
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message{Role: "assistant", Content: "cut"}, "finish_reason": "length"}}})
	}))
	defer server2.Close()
	r = &model.Report{}
	if err := Run(context.Background(), Options{Endpoint: server2.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	if requests != maxTruncations+1 || len(r.Unverified) != 1 || !strings.Contains(r.Unverified[0], "truncated") {
		t.Fatalf("%d requests, unverified %v", requests, r.Unverified)
	}
}

func TestOutputBudgetFallsBackOnABadRequest(t *testing.T) {
	// A provider that rejects 8192 output tokens gets one retry with 4096,
	// kept for the next requests.
	var budgets []float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		budget := body["max_completion_tokens"].(float64)
		budgets = append(budgets, budget)
		if budget > fallbackMaxTokens {
			http.Error(w, `{"error":"max_tokens exceeds the context"}`, http.StatusBadRequest)
			return
		}
		if len(budgets) == 2 {
			complete(w, call("a", "run_build", `{}`))
			return
		}
		complete(w)
	}))
	defer server.Close()
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(budgets) != fmt.Sprint([]float64{defaultMaxTokens, fallbackMaxTokens, fallbackMaxTokens}) {
		t.Fatalf("budgets %v", budgets)
	}
}

// evidenceHarness answers run_generated_test with a reproduced issue.
type evidenceHarness struct{}

func (evidenceHarness) Call(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"evidence":{"id":"evidence-9","kind":"differential_test","status":"REPRODUCED","description":"Refund must not exceed the paid amount","path":"payment/x_test.go","test_names":["TestCap"]},"base_check":{},"candidate_check":{}}`), nil
}

func TestWrapUpLetsAStoppedModelCiteItsEvidence(t *testing.T) {
	// The iteration budget ends right after a reproduced result: one last
	// request offers submit_hypothesis only, with the uncited evidence.
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		if len(requests) == 1 {
			complete(w, call("a", "run_generated_test", `{"test_id":"t1"}`))
			return
		}
		complete(w, call("b", "submit_hypothesis", `{"title":"Refunds can exceed the paid amount","severity":"critical","status":"REPRODUCED","rationale":"The cap check was removed.","evidence_ids":["evidence-9"],"path":"payment/refund.go","line":23}`))
	}))
	defer server.Close()
	r := &model.Report{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", MaxIterations: 1}, r, evidenceHarness{}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	tools := requests[1]["tools"].([]any)
	messages := requests[1]["messages"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "submit_hypothesis" || len(messages) != 2 || !strings.Contains(messages[1].(map[string]any)["content"].(string), `"id":"evidence-9"`) {
		t.Fatalf("wrap-up request: tools %v, messages %v", tools, messages)
	}
	if len(r.Hypotheses) != 1 || r.Hypotheses[0].EvidenceIDs[0] != "evidence-9" {
		t.Fatalf("hypotheses %+v", r.Hypotheses)
	}
	found := false
	for _, e := range r.Audit {
		found = found || (e.Tool == "reviewer_completion" && e.Arguments == "wrap_up")
	}
	if !found {
		t.Fatal("the wrap-up completion is not audited")
	}

	// Without uncited evidence there is no last request.
	requests = nil
	r = &model.Report{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", MaxIterations: 1}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("%d requests without evidence to cite", len(requests))
	}
}

func TestUnknownToolNeverReachesHarness(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			complete(w, call("x", "shell", `{"cmd":"bad"}`))
			return
		}
		complete(w)
	}))
	defer server.Close()
	h := &fakeHarness{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, h); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("unavailable tool called: %v", h.calls)
	}
}

// Gateways omit the call type and restart call IDs at every turn: the loop
// gives each call its own ID, and each result answers the call it belongs to.
func TestToolCallIDsAreMadeDistinct(t *testing.T) {
	requests := 0
	var last struct{ Messages []message }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		last.Messages = nil
		_ = json.NewDecoder(r.Body).Decode(&last)
		untyped := call("call_0", "read_file", `{"path":"main.go"}`)
		untyped.Type = ""
		switch requests {
		case 1:
			complete(w, untyped)
		case 2:
			complete(w, call("call_0", "read_file", `{"path":"other.go"}`), call("", "read_file", `{"path":"x.go"}`))
		default:
			complete(w)
		}
	}))
	defer server.Close()
	h := &fakeHarness{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, h); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 3 {
		t.Fatalf("harness calls %v", h.calls)
	}
	seen, pending := map[string]bool{}, []string{}
	for _, m := range last.Messages {
		for _, c := range m.ToolCalls {
			if c.ID == "" || c.Type != "function" || seen[c.ID] {
				t.Fatalf("call sent back as %+v", c)
			}
			seen[c.ID] = true
			pending = append(pending, c.ID)
		}
		if m.Role == "tool" {
			if len(pending) == 0 || m.ToolCallID != pending[0] {
				t.Fatalf("result %q does not answer the pending call %v", m.ToolCallID, pending)
			}
			pending = pending[1:]
		}
	}
	if len(seen) != 3 || len(pending) != 0 {
		t.Fatalf("calls %v, unanswered %v", seen, pending)
	}
}

func TestToolCallOfAnotherTypeIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call("a", "read_file", `{}`)
		c.Type = "code_interpreter"
		complete(w, c)
	}))
	defer server.Close()
	h := &fakeHarness{}
	err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, &model.Report{}, h)
	if err == nil || !strings.Contains(err.Error(), `unsupported type "code_interpreter"`) || len(h.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, h.calls)
	}
}

func TestHTTPErrorDoesNotEchoProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "api-secret provider sensitive body")
	}))
	defer server.Close()
	err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", APIKey: "api-secret"}, &model.Report{}, &fakeHarness{})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad error: %v", err)
	}
}
