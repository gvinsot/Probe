package server

// The LLM gateway lends the deployment's model to the CLI of signed-in users:
// POST /llm/v1/chat/completions speaks the OpenAI chat completions API the
// CLI already uses, authenticated by an llm-scoped agent token (`probe login`
// or the dashboard). The hub relays the request to its own provider with its
// own key, which never leaves the process, under the deployment's model and
// completion cap, and counts the tokens against the account's daily quota.
//
// Only the fields a chat completion needs are relayed, and never a stream: a
// complete response is what lets the hub count what was consumed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/store"
)

const (
	// maxGatewayRequestBytes bounds a relayed request; the CLI's own input
	// budget is at most 2 MiB.
	maxGatewayRequestBytes = 4 << 20
	// maxGatewayResponseBytes bounds a relayed response.
	maxGatewayResponseBytes = 4 << 20
	// gatewayAccountParallel bounds the requests of one account in flight:
	// enough for a review swarm, not enough to crowd out other accounts.
	gatewayAccountParallel = 4
	// gatewayTimeout bounds one completion, queueing included.
	gatewayTimeout = 15 * time.Minute
)

// gatewayFields are the request fields relayed to the provider. The model is
// always the deployment's; anything else is dropped.
var gatewayFields = map[string]bool{
	"messages": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"max_completion_tokens": true, "max_tokens": true, "temperature": true, "top_p": true,
	"response_format": true, "stop": true, "seed": true,
}

// gatewayState is the gateway's shared machinery.
type gatewayState struct {
	client *http.Client
	slots  chan struct{}
	rate   *windowLimiter

	mu       sync.Mutex
	inflight map[string]int
}

func newGatewayState(g config.Gateway) *gatewayState {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = gatewayTimeout
	return &gatewayState{
		client: &http.Client{Transport: transport, Timeout: gatewayTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("the provider redirected")
		}},
		slots:    make(chan struct{}, max(g.Concurrency, 1)),
		rate:     newWindowLimiter(max(g.Rate, 1), time.Minute),
		inflight: map[string]int{},
	}
}

func (g *gatewayState) enter(userKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[userKey] >= gatewayAccountParallel {
		return false
	}
	g.inflight[userKey]++
	return true
}

func (g *gatewayState) leave(userKey string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[userKey]--; g.inflight[userKey] <= 0 {
		delete(g.inflight, userKey)
	}
}

// gatewayURL is the base URL the CLI points its reviewer at.
func (s *Server) gatewayURL() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/llm/v1"
}

// gatewayError answers in the OpenAI error shape, which OpenAI-compatible
// clients understand.
func gatewayError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": kind}})
}

// gatewayPrincipal authenticates an llm-scoped bearer token.
func (s *Server) gatewayPrincipal(w http.ResponseWriter, r *http.Request) (*agentPrincipal, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.cfg.Gateway.Enabled {
		gatewayError(w, http.StatusNotFound, "not_found", "this deployment does not lend its LLM")
		return nil, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub"`)
		gatewayError(w, http.StatusUnauthorized, "authentication_error", "run probe login, or send an llm agent token as Authorization: Bearer")
		return nil, false
	}
	principal, err := s.authenticateAgent(strings.TrimSpace(token), time.Now())
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub", error="invalid_token"`)
		gatewayError(w, http.StatusUnauthorized, "authentication_error", "invalid or expired token: run probe login again")
		return nil, false
	}
	if principal.scope != store.ScopeLLM {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub", error="insufficient_scope"`)
		gatewayError(w, http.StatusForbidden, "permission_error", "this agent token is for MCP; the LLM gateway needs an llm token (probe login)")
		return nil, false
	}
	return principal, true
}

// handleGatewayCompletions relays one chat completion.
func (s *Server) handleGatewayCompletions(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.gatewayPrincipal(w, r)
	if !ok {
		return
	}
	userKey := principal.session.UserKey
	if !s.gateway.rate.Allow(userKey) {
		w.Header().Set("Retry-After", "60")
		gatewayError(w, http.StatusTooManyRequests, "rate_limit_error", "too many requests from this account; retry in a minute")
		return
	}
	user, err := s.store.User(userKey)
	if err != nil {
		gatewayError(w, http.StatusUnauthorized, "authentication_error", "run probe login again")
		return
	}
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	if user.LLMUsage.Today(day) >= s.cfg.Gateway.DailyTokens {
		w.Header().Set("Retry-After", strconv.Itoa(int(nextUTCDay(now).Sub(now).Seconds())+1))
		gatewayError(w, http.StatusTooManyRequests, "insufficient_quota", "this account used its daily LLM quota; it resets at 00:00 UTC")
		return
	}
	if !s.gateway.enter(userKey) {
		w.Header().Set("Retry-After", "5")
		gatewayError(w, http.StatusTooManyRequests, "rate_limit_error", "too many requests in flight for this account")
		return
	}
	defer s.gateway.leave(userKey)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayRequestBytes+1))
	if err != nil || len(body) > maxGatewayRequestBytes {
		gatewayError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the request exceeds 4 MiB")
		return
	}
	payload, err := gatewayPayload(body, s.cfg.Gateway)
	if err != nil {
		gatewayError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()
	select {
	case s.gateway.slots <- struct{}{}:
		defer func() { <-s.gateway.slots }()
	case <-ctx.Done():
		gatewayError(w, http.StatusServiceUnavailable, "server_error", "the LLM is busy; retry later")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Gateway.Endpoint, bytes.NewReader(payload))
	if err != nil {
		gatewayError(w, http.StatusInternalServerError, "server_error", "could not build the request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.Gateway.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Gateway.APIKey)
	}
	started := time.Now()
	res, err := s.gateway.client.Do(req)
	if err != nil {
		// The transport error can name the internal endpoint: log it only.
		s.log.Warn("llm gateway", "user", user.Login, "error", err)
		gatewayError(w, http.StatusBadGateway, "server_error", "the LLM did not answer")
		return
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxGatewayResponseBytes+1))
	res.Body.Close()
	if err != nil || len(data) > maxGatewayResponseBytes {
		gatewayError(w, http.StatusBadGateway, "server_error", "the LLM response could not be read")
		return
	}
	tokens := int64(0)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		tokens = completionTokens(data, len(payload))
	}
	s.recordGatewayUsage(userKey, day, tokens)
	s.log.Info("llm gateway", "user", user.Login, "token", principal.token, "status", res.StatusCode, "tokens", tokens, "ms", time.Since(started).Milliseconds())
	if res.StatusCode >= 500 {
		gatewayError(w, http.StatusBadGateway, "server_error", "the LLM failed to answer ("+strconv.Itoa(res.StatusCode)+")")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(data)
}

// gatewayPayload keeps the relayed fields of a request, imposes the
// deployment's model and caps the completion length.
func gatewayPayload(body []byte, g config.Gateway) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, errors.New("the request is not a JSON object")
	}
	if len(in["messages"]) == 0 {
		return nil, errors.New("messages is required")
	}
	if raw, ok := in["stream"]; ok && string(bytes.TrimSpace(raw)) != "false" {
		return nil, errors.New("streaming is not supported by this gateway")
	}
	if raw, ok := in["n"]; ok && string(bytes.TrimSpace(raw)) != "1" {
		return nil, errors.New("only one choice (n=1) is supported")
	}
	out := map[string]any{}
	for key, value := range in {
		if gatewayFields[key] {
			out[key] = value
		}
	}
	out["model"] = g.Model
	capped := false
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		raw, ok := in[key]
		if !ok {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n < 1 {
			return nil, errors.New(key + " must be a positive integer")
		}
		out[key] = min(n, g.MaxCompletionTokens)
		capped = true
	}
	if !capped {
		out["max_completion_tokens"] = g.MaxCompletionTokens
	}
	return json.Marshal(out)
}

// completionTokens reads the usage a provider reports, or estimates it at
// four bytes a token when the provider reports none.
func completionTokens(response []byte, requestBytes int) int64 {
	var reply struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(response, &reply) == nil {
		if reply.Usage.TotalTokens > 0 {
			return reply.Usage.TotalTokens
		}
		if sum := reply.Usage.PromptTokens + reply.Usage.CompletionTokens; sum > 0 {
			return sum
		}
	}
	return int64(requestBytes+len(response))/4 + 1
}

// recordGatewayUsage adds a request to the account's count for day.
func (s *Server) recordGatewayUsage(userKey, day string, tokens int64) {
	err := s.store.UpdateUser(userKey, func(u *store.User) error {
		if u.LLMUsage == nil || u.LLMUsage.Day != day {
			u.LLMUsage = &store.LLMUsage{Day: day}
		}
		u.LLMUsage.Tokens += tokens
		u.LLMUsage.Requests++
		return nil
	})
	if err != nil {
		s.log.Warn("record llm usage", "error", err)
	}
}

// gatewayUsage describes the account's gateway budget.
func (s *Server) gatewayUsage(user *store.User, now time.Time) map[string]any {
	now = now.UTC()
	return map[string]any{
		"endpoint":     s.gatewayURL(),
		"model":        s.cfg.Gateway.Model,
		"daily_tokens": s.cfg.Gateway.DailyTokens,
		"used_today":   user.LLMUsage.Today(now.Format(time.DateOnly)),
		"resets_at":    nextUTCDay(now),
	}
}

func nextUTCDay(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// handleGatewayAccount tells `probe login --status` which account a token
// stands for and what remains of its quota.
func (s *Server) handleGatewayAccount(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.gatewayPrincipal(w, r)
	if !ok {
		return
	}
	user, err := s.store.User(principal.session.UserKey)
	if err != nil {
		gatewayError(w, http.StatusUnauthorized, "authentication_error", "run probe login again")
		return
	}
	out := s.gatewayUsage(user, time.Now())
	out["login"], out["provider"], out["token"] = user.Login, user.Provider, principal.token
	writeJSON(w, http.StatusOK, out)
}

// handleGatewayLogout revokes the token that authenticates the request:
// `probe logout` leaves no live credential behind.
func (s *Server) handleGatewayLogout(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.gatewayPrincipal(w, r)
	if !ok {
		return
	}
	err := s.store.UpdateUser(principal.session.UserKey, func(u *store.User) error {
		kept := u.AgentTokens[:0]
		for _, t := range u.AgentTokens {
			if t.ID != principal.id {
				kept = append(kept, t)
			}
		}
		u.AgentTokens = kept
		return nil
	})
	if err != nil {
		gatewayError(w, http.StatusInternalServerError, "server_error", "could not revoke the token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
