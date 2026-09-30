package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// createToken issues an agent token from the dashboard.
func (h *harness) createToken(scope string) string {
	h.t.Helper()
	w := h.do(http.MethodPost, "/api/tokens", map[string]any{"name": "claude code", "scope": scope, "expires_days": 30})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("create token = %d: %s", w.Code, w.Body)
	}
	out := h.decode(w)
	token, _ := out["token"].(string)
	if !strings.HasPrefix(token, agentTokenPrefix) || out["endpoint"] != "https://hub.example/mcp" {
		h.t.Fatalf("token reply %v", out)
	}
	return token
}

// mcp posts one JSON-RPC message with a bearer token.
func (h *harness) mcp(token string, message any, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		h.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// call runs a tool and returns its result object.
func (h *harness) call(token, tool string, args map[string]any) map[string]any {
	h.t.Helper()
	w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if w.Code != http.StatusOK {
		h.t.Fatalf("%s = %d: %s", tool, w.Code, w.Body)
	}
	reply := h.decode(w)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("%s: no result: %v", tool, reply)
	}
	return result
}

func toolText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func toolData(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] == true {
		t.Fatalf("tool error: %s", toolText(result))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolText(result)), &out); err != nil {
		t.Fatalf("tool text is not JSON: %v\n%s", err, toolText(result))
	}
	return out
}

func toolNames(t *testing.T, h *harness, token string) []string {
	t.Helper()
	reply := h.decode(h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}))
	tools := reply["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func TestMCPTokenLifecycleAndProtocol(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	token := h.createToken(store.ScopeWrite)

	listed := h.decode(h.do(http.MethodGet, "/api/tokens", nil))
	tokens := listed["tokens"].([]any)
	if len(tokens) != 1 || strings.Contains(fmtJSON(t, listed), "hash") || strings.Contains(fmtJSON(t, listed), token) {
		t.Fatalf("token list leaks or misses: %v", listed)
	}

	// No token, a cookie alone, a forged token: refused.
	if w := h.mcp("", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous = %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.AddCookie(h.cookie)
	r.Header.Set(csrfHeader, h.csrf)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a session cookie must not authenticate MCP, got %d", w.Code)
	}
	if w := h.mcp(token[:len(token)-2]+"xx", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged token = %d", w.Code)
	}
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, "Origin", "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", w.Code)
	}
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, "MCP-Protocol-Version", "1999-01-01"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown protocol version = %d", w.Code)
	}

	init := h.decode(h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" || result["serverInfo"].(map[string]any)["name"] != "probe-hub" {
		t.Fatalf("initialize = %v", init)
	}
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification = %d %q", w.Code, w.Body)
	}
	unknown := h.decode(h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/list"}))
	if unknown["error"].(map[string]any)["code"].(float64) != rpcMethodNotFound {
		t.Fatalf("unknown method = %v", unknown)
	}
	batch := h.mcp(token, []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"},
	})
	var replies []map[string]any
	if err := json.Unmarshal(batch.Body.Bytes(), &replies); err != nil || len(replies) != 2 {
		t.Fatalf("batch = %s", batch.Body)
	}
	get := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	gw := httptest.NewRecorder()
	h.handler.ServeHTTP(gw, get)
	if gw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d", gw.Code)
	}
	if names := toolNames(t, h, token); len(names) != len(mcpTools) {
		t.Fatalf("write token sees %v", names)
	}

	// Revocation takes effect at once.
	id := tokens[0].(map[string]any)["id"].(string)
	if w := h.do(http.MethodDelete, "/api/tokens/"+id, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d", w.Code)
	}
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d", w.Code)
	}
}

func fmtJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMCPTokenValidationAndExpiry(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	for _, body := range []map[string]any{
		{"name": "", "scope": "read"},
		{"name": "x", "scope": "admin"},
		{"name": "x", "scope": "read", "expires_days": 400},
		{"name": strings.Repeat("n", 81), "scope": "read"},
	} {
		if w := h.do(http.MethodPost, "/api/tokens", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v = %d", body, w.Code)
		}
	}
	token := h.createToken(store.ScopeRead)
	if err := h.store.UpdateUser(h.userKey, func(u *store.User) error {
		u.AgentTokens[0].ExpiresAt = time.Now().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := h.mcp(token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d", w.Code)
	}
	// Without a session, tokens cannot be managed.
	h.cookie = nil
	if w := h.do(http.MethodGet, "/api/tokens", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous token list = %d", w.Code)
	}
}

func TestMCPReadTokenCannotWrite(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.addRepo(func(r *store.Repo) { r.HasPolicy = true })
	token := h.createToken(store.ScopeRead)
	for _, name := range toolNames(t, h, token) {
		if tool, _ := findTool(name); tool.write {
			t.Errorf("read token lists write tool %s", name)
		}
	}
	result := h.call(token, "update_coding_rules", map[string]any{"repo": "acme/shop", "rules": "- no panics"})
	if result["isError"] != true || !strings.Contains(toolText(result), "read-only") {
		t.Fatalf("read token wrote: %v", result)
	}
	repo, _ := h.store.Repo(h.userKey, store.Key(config.GitHub, "10"))
	if repo.CodingRules != "" {
		t.Fatal("rules changed through a read token")
	}
}

func TestMCPReviewWorkflow(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(func(r *store.Repo) { r.HasPolicy = true })
	commit := strings.Repeat("a", 40)
	rec := &store.Record{UserKey: h.userKey, RepoKey: repo.Key, RepoName: repo.FullName, Raw: json.RawMessage(storedReport)}
	rec.Commit, rec.Status, rec.QueuedAt = commit, store.StatusDone, time.Now().UTC()
	if err := h.store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	token := h.createToken(store.ScopeWrite)

	repos := toolData(t, h.call(token, "list_repositories", map[string]any{"query": "SHOP"}))
	if repos["count"].(float64) != 1 {
		t.Fatalf("list_repositories = %v", repos)
	}
	if r := h.call(token, "get_repository", map[string]any{"repo": "nope"}); r["isError"] != true || !strings.Contains(toolText(r), "unknown repository") {
		t.Fatalf("unknown repo = %v", r)
	}

	// Bare name and a 7-character prefix resolve.
	findings := toolData(t, h.call(token, "get_findings", map[string]any{"repo": "shop", "commit": "aaaaaaa"}))
	if findings["commit"] != commit || findings["summary"] == nil {
		t.Fatalf("get_findings = %v", findings)
	}
	if strings.Contains(fmtJSON(t, findings), `"files"`) {
		t.Fatal("findings must not carry the diffs")
	}
	all := len(findings["alerts"].([]any))
	critical := toolData(t, h.call(token, "get_findings", map[string]any{"repo": "acme/shop", "commit": commit, "min_severity": "critical"}))
	if len(critical["alerts"].([]any)) >= all && all > 0 {
		t.Fatalf("min_severity did not filter: %d of %d", len(critical["alerts"].([]any)), all)
	}
	raw := toolData(t, h.call(token, "get_report", map[string]any{"repo": "acme/shop", "commit": commit, "format": "raw"}))
	if raw["report"] == nil {
		t.Fatalf("raw report = %v", raw)
	}
	runs := toolData(t, h.call(token, "list_analyses", map[string]any{"repo": "acme/shop", "limit": 5}))
	if len(runs["runs"].([]any)) != 1 {
		t.Fatalf("list_analyses = %v", runs)
	}

	// Review context.
	toolData(t, h.call(token, "update_coding_rules", map[string]any{"repo": "acme/shop", "rules": "- Money uses integer cents."}))
	stored, _ := h.store.Repo(h.userKey, repo.Key)
	if stored.CodingRules != "- Money uses integer cents." {
		t.Fatalf("rules = %q", stored.CodingRules)
	}
	toolData(t, h.call(token, "set_learning", map[string]any{"repo": "acme/shop", "enabled": false}))
	if stored, _ = h.store.Repo(h.userKey, repo.Key); !stored.LearningOff {
		t.Fatal("learning still on")
	}
	toolData(t, h.call(token, "set_learning", map[string]any{"repo": "acme/shop", "enabled": true}))
	alertID := findings["alerts"].([]any)[0].(map[string]any)["id"].(string)
	added := toolData(t, h.call(token, "add_feedback", map[string]any{"repo": "acme/shop", "commit": commit, "alert_id": alertID, "vote": "up", "comment": "Real issue."}))
	if author := added["entry"].(map[string]any)["author"]; author != "octocat (agent: claude code)" {
		t.Fatalf("author = %v", author)
	}
	if fb := toolData(t, h.call(token, "list_feedback", map[string]any{"repo": "acme/shop", "commit": commit})); len(fb["entries"].([]any)) != 1 {
		t.Fatalf("list_feedback = %v", fb)
	}

	// Trigger, see it queued, wait with a short timeout, cancel.
	waitPoll = 10 * time.Millisecond
	defer func() { waitPoll = 2 * time.Second }()
	next := strings.Repeat("b", 40)
	queued := toolData(t, h.call(token, "trigger_review", map[string]any{"repo": "acme/shop", "commit": next}))
	if queued["status"] != "queued" || queued["commit"] != next {
		t.Fatalf("trigger_review = %v", queued)
	}
	if r := h.call(token, "trigger_review", map[string]any{"repo": "acme/shop", "commit": next, "branch": "main"}); r["isError"] != true {
		t.Fatal("commit and branch together must be refused")
	}
	waited := toolData(t, h.call(token, "wait_for_analysis", map[string]any{"repo": "acme/shop", "commit": next, "timeout_seconds": 1}))
	if waited["status"] != "queued" && waited["status"] != "running" && waited["status"] != "failed" && waited["status"] != "done" {
		t.Fatalf("wait_for_analysis = %v", waited)
	}
	done := toolData(t, h.call(token, "wait_for_analysis", map[string]any{"repo": "acme/shop", "commit": commit}))
	if done["status"] != "done" || done["findings"] == nil {
		t.Fatalf("finished analysis = %v", done)
	}
}

func TestMCPCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.addRepo(nil)
	other := &store.User{Key: store.Key(config.GitHub, "2"), Provider: config.GitHub, ID: "2", Login: "mallory"}
	if err := h.store.PutUser(other); err != nil {
		t.Fatal(err)
	}
	// A token of another account, forged with this account's key, fails:
	// only hashes stored on the named account are accepted.
	forged, err := newAgentToken(h.userKey)
	if err != nil {
		t.Fatal(err)
	}
	if w := h.mcp(forged, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged = %d", w.Code)
	}
	// Mallory's own token does not see acme/shop.
	token, _ := newAgentToken(other.Key)
	if err := h.store.UpdateUser(other.Key, func(u *store.User) error {
		u.AgentTokens = append(u.AgentTokens, store.AgentToken{ID: "m", Name: "m", Scope: store.ScopeWrite, Hash: hashAgentToken(token), CreatedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := h.call(token, "get_repository", map[string]any{"repo": "acme/shop"}); r["isError"] != true {
		t.Fatalf("cross-account read = %v", r)
	}
	if r := h.call(token, "get_repository", map[string]any{"repo": store.Key(config.GitHub, "10")}); r["isError"] != true {
		t.Fatalf("cross-account read by key = %v", r)
	}
}
