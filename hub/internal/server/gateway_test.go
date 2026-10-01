package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// fakeLLM records what the gateway relayed and answers with a fixed usage.
type fakeLLM struct {
	mu       sync.Mutex
	requests []map[string]any
	auth     []string
	status   int
	usage    int
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status, usage := f.status, f.usage
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"total_tokens": usage},
	})
}

func (f *fakeLLM) last() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil, ""
	}
	return f.requests[len(f.requests)-1], f.auth[len(f.auth)-1]
}

func newGatewayHarness(t *testing.T, llm *fakeLLM) *harness {
	t.Helper()
	upstream := httptest.NewServer(llm)
	t.Cleanup(upstream.Close)
	h := newHarnessWith(t, func(c *config.Config) {
		c.Gateway = config.Gateway{Enabled: true, Endpoint: upstream.URL + "/v1/chat/completions", Model: "deployment-model", APIKey: "provider-key", DailyTokens: 1000, Rate: 100, Concurrency: 2, MaxCompletionTokens: 512}
	})
	h.signIn()
	return h
}

// raw sends a request with no cookie, and a bearer token when given.
func (h *harness) raw(method, path, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// login runs the device flow and returns the issued token.
func (h *harness) login() string {
	h.t.Helper()
	w := h.raw(http.MethodPost, "/api/device/code", "", map[string]string{"client": "Probe CLI on laptop\n(linux)"})
	if w.Code != http.StatusOK {
		h.t.Fatalf("device code = %d: %s", w.Code, w.Body)
	}
	start := h.decode(w)
	device, user := start["device_code"].(string), start["user_code"].(string)
	if start["verification_uri_complete"] != "https://hub.example/device.html?code="+user {
		h.t.Fatalf("verification link %v", start)
	}
	if w := h.raw(http.MethodPost, "/api/device/token", "", map[string]string{"device_code": device}); w.Code != http.StatusBadRequest || h.decode(w)["error"] != "authorization_pending" {
		h.t.Fatalf("poll before approval = %d: %s", w.Code, w.Body)
	}
	look := h.do(http.MethodGet, "/api/device?code="+strings.ToLower(strings.ReplaceAll(user, "-", "")), nil)
	if look.Code != http.StatusOK || h.decode(look)["client"] != "Probe CLI on laptop (linux)" {
		h.t.Fatalf("lookup = %d: %s", look.Code, look.Body)
	}
	if w := h.do(http.MethodPost, "/api/device/decide", map[string]any{"user_code": user, "approve": true}); w.Code != http.StatusOK {
		h.t.Fatalf("approve = %d: %s", w.Code, w.Body)
	}
	// The poll interval applies between two polls: move the last one back.
	h.server.devices.mu.Lock()
	for _, g := range h.server.devices.byDevice {
		g.lastPoll = time.Time{}
	}
	h.server.devices.mu.Unlock()
	w = h.raw(http.MethodPost, "/api/device/token", "", map[string]string{"device_code": device})
	if w.Code != http.StatusOK {
		h.t.Fatalf("token = %d: %s", w.Code, w.Body)
	}
	out := h.decode(w)
	if out["endpoint"] != "https://hub.example/llm/v1" || out["model"] != "deployment-model" || out["login"] != "octocat" {
		h.t.Fatalf("token reply %v", out)
	}
	// The grant is handed over once.
	if w := h.raw(http.MethodPost, "/api/device/token", "", map[string]string{"device_code": device}); h.decode(w)["error"] != "invalid_grant" {
		h.t.Fatalf("second redemption = %d: %s", w.Code, w.Body)
	}
	return out["access_token"].(string)
}

func completion(extra map[string]any) map[string]any {
	body := map[string]any{"model": "gpt-5", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestGatewayLoginAndRelay(t *testing.T) {
	llm := &fakeLLM{usage: 120}
	h := newGatewayHarness(t, llm)
	token := h.login()

	w := h.raw(http.MethodPost, "/llm/v1/chat/completions", token, completion(map[string]any{"max_completion_tokens": 100000, "user_secret": "dropped", "temperature": 0.2}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("completion = %d: %s", w.Code, w.Body)
	}
	sent, auth := llm.last()
	if sent["model"] != "deployment-model" || sent["max_completion_tokens"] != float64(512) || sent["temperature"] != 0.2 {
		t.Fatalf("relayed %v", sent)
	}
	if _, ok := sent["user_secret"]; ok {
		t.Fatalf("an unknown field was relayed: %v", sent)
	}
	if auth != "Bearer provider-key" {
		t.Fatalf("provider auth %q", auth)
	}
	user, err := h.store.User(h.userKey)
	if err != nil {
		t.Fatal(err)
	}
	if user.LLMUsage.Today(time.Now().UTC().Format(time.DateOnly)) != 120 || user.LLMUsage.Requests != 1 {
		t.Fatalf("usage %+v", user.LLMUsage)
	}

	account := h.raw(http.MethodGet, "/llm/v1/account", token, nil)
	if account.Code != http.StatusOK {
		t.Fatalf("account = %d: %s", account.Code, account.Body)
	}
	if got := h.decode(account); got["login"] != "octocat" || got["used_today"] != float64(120) || got["daily_tokens"] != float64(1000) {
		t.Fatalf("account %v", got)
	}

	// The token is an llm token: it does not open MCP.
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); w.Code != http.StatusForbidden {
		t.Fatalf("llm token on MCP = %d: %s", w.Code, w.Body)
	}

	if w := h.raw(http.MethodDelete, "/llm/v1/token", token, nil); w.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", w.Code, w.Body)
	}
	if w := h.raw(http.MethodPost, "/llm/v1/chat/completions", token, completion(nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d: %s", w.Code, w.Body)
	}
}

func TestGatewayRefusals(t *testing.T) {
	llm := &fakeLLM{usage: 600}
	h := newGatewayHarness(t, llm)
	token := h.login()
	mcpToken := h.createToken(store.ScopeWrite)

	cases := []struct {
		name   string
		token  string
		body   map[string]any
		status int
	}{
		{"no token", "", completion(nil), http.StatusUnauthorized},
		{"MCP token", mcpToken, completion(nil), http.StatusForbidden},
		{"stream", token, completion(map[string]any{"stream": true}), http.StatusBadRequest},
		{"several choices", token, completion(map[string]any{"n": 3}), http.StatusBadRequest},
		{"no messages", token, map[string]any{"model": "x"}, http.StatusBadRequest},
		{"negative cap", token, completion(map[string]any{"max_tokens": -1}), http.StatusBadRequest},
	}
	for _, c := range cases {
		if w := h.raw(http.MethodPost, "/llm/v1/chat/completions", c.token, c.body); w.Code != c.status {
			t.Errorf("%s = %d, want %d: %s", c.name, w.Code, c.status, w.Body)
		}
	}
	if sent, _ := llm.last(); sent != nil {
		t.Fatalf("a refused request reached the provider: %v", sent)
	}

	// Two completions of 600 tokens exhaust a daily quota of 1000.
	for i := 0; i < 2; i++ {
		if w := h.raw(http.MethodPost, "/llm/v1/chat/completions", token, completion(nil)); w.Code != http.StatusOK {
			t.Fatalf("completion %d = %d: %s", i, w.Code, w.Body)
		}
	}
	w := h.raw(http.MethodPost, "/llm/v1/chat/completions", token, completion(nil))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "insufficient_quota") {
		t.Fatalf("over quota = %d: %s", w.Code, w.Body)
	}
}

func TestGatewayProviderFailureIsNotEchoed(t *testing.T) {
	llm := &fakeLLM{status: http.StatusInternalServerError}
	h := newGatewayHarness(t, llm)
	token := h.login()
	w := h.raw(http.MethodPost, "/llm/v1/chat/completions", token, completion(nil))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("provider failure = %d: %s", w.Code, w.Body)
	}
	user, _ := h.store.User(h.userKey)
	if user.LLMUsage.Today(time.Now().UTC().Format(time.DateOnly)) != 0 {
		t.Fatalf("a failed completion was counted: %+v", user.LLMUsage)
	}
}

func TestGatewayDisabled(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	for _, path := range []string{"/api/device/code", "/llm/v1/chat/completions"} {
		if w := h.raw(http.MethodPost, path, "", map[string]string{}); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d: %s", path, w.Code, w.Body)
		}
	}
	w := h.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scope": store.ScopeLLM, "expires_days": 30})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("llm token without a gateway = %d: %s", w.Code, w.Body)
	}
}

func TestDashboardLLMToken(t *testing.T) {
	h := newGatewayHarness(t, &fakeLLM{usage: 1})
	w := h.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scope": store.ScopeLLM, "expires_days": 30})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	out := h.decode(w)
	if out["endpoint"] != "https://hub.example/llm/v1" {
		t.Fatalf("reply %v", out)
	}
	if w := h.raw(http.MethodPost, "/llm/v1/chat/completions", out["token"].(string), completion(nil)); w.Code != http.StatusOK {
		t.Fatalf("completion = %d: %s", w.Code, w.Body)
	}
	list := h.decode(h.do(http.MethodGet, "/api/tokens", nil))
	if llm, ok := list["llm"].(map[string]any); !ok || llm["used_today"] != float64(1) {
		t.Fatalf("token list %v", list)
	}
}

func TestDeviceFlowStates(t *testing.T) {
	d := newDeviceFlows()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	device, user, err := d.start("cli", now)
	if err != nil {
		t.Fatal(err)
	}
	if normalizeUserCode(displayUserCode(user)) != user || normalizeUserCode("AEIO-UAEI") != "" {
		t.Fatal("user code normalization")
	}
	if _, code := d.poll(device, now); code != "authorization_pending" {
		t.Fatalf("first poll %q", code)
	}
	if _, code := d.poll(device, now.Add(time.Second)); code != "slow_down" {
		t.Fatalf("fast poll %q", code)
	}
	// slow_down raised the interval to 10 s.
	if _, code := d.poll(device, now.Add(8*time.Second)); code != "slow_down" {
		t.Fatalf("poll within the raised interval %q", code)
	}
	if !d.decide(user, "", false, now.Add(20*time.Second)) {
		t.Fatal("deny")
	}
	if d.decide(user, "github:1", true, now.Add(21*time.Second)) {
		t.Fatal("a denied request was approved")
	}
	if _, code := d.poll(device, now.Add(40*time.Second)); code != "access_denied" {
		t.Fatalf("denied poll %q", code)
	}

	device, user, _ = d.start("cli", now)
	if _, ok := d.pending(user, now.Add(11*time.Minute)); ok {
		t.Fatal("an expired request is still pending")
	}
	if _, code := d.poll(device, now.Add(11*time.Minute)); code != "expired_token" {
		t.Fatalf("expired poll %q", code)
	}
}

func TestDeviceApprovalNeedsBrowserSession(t *testing.T) {
	h := newGatewayHarness(t, &fakeLLM{})
	w := h.raw(http.MethodPost, "/api/device/code", "", map[string]string{"client": "cli"})
	user := h.decode(w)["user_code"].(string)
	// No cookie: refused.
	if w := h.raw(http.MethodPost, "/api/device/decide", "", map[string]any{"user_code": user, "approve": true}); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approval = %d", w.Code)
	}
	// A cookie without the CSRF token: refused.
	r := httptest.NewRequest(http.MethodPost, "/api/device/decide", strings.NewReader(`{"user_code":"`+user+`","approve":true}`))
	r.AddCookie(h.cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("approval without CSRF = %d", rec.Code)
	}
}

func TestDeviceClient(t *testing.T) {
	if got := deviceClient("  a\x00b\tc  "); got != "a b c" {
		t.Fatalf("%q", got)
	}
	if got := deviceClient(""); got != "Probe CLI" {
		t.Fatalf("%q", got)
	}
	if got := deviceClient(strings.Repeat("é", 100)); len([]rune(got)) != maxDeviceClient {
		t.Fatalf("%d runes", len([]rune(got)))
	}
}
