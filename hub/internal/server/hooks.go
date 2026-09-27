package server

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gvinsot/SwiftProof/hub/internal/analysis"
	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

// maxHookBytes bounds a webhook delivery. A push payload lists the pushed
// commits; forges cap it well below this.
const maxHookBytes = 5 << 20

// handleWebhook receives a push delivery and schedules the analysis.
//
// The forge is the caller, so there is no browser session. What stands for it
// is the installation token: a random secret issued to the signed-in owner
// when they switched monitoring on, sealed at rest, and carried only in the
// webhook URL registered on the forge. A delivery must present that token
// and the forge signature; the routing key alone, which is what an attacker
// could guess or scrape, authorizes nothing. Every refusal looks the same.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	hookKey := r.PathValue("hook")
	// A malformed routing key cannot name an installation: refused before it
	// costs a limiter entry, a lookup or any crypto, like every other refusal.
	if len(hookKey) > maxHookKeyBytes || !store.ValidKey(hookKey) {
		writeError(w, http.StatusUnauthorized, "unauthorized webhook")
		return
	}
	// Bounded before any lookup, so a flood costs neither disk nor crypto.
	if !s.hookLimit.Allow(hookKey) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many deliveries")
		return
	}
	route, repo, ok := s.hookInstallation(hookKey, r.URL.Query().Get(hookTokenParam))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized webhook")
		return
	}
	provider, err := s.accounts.Provider(repo.Provider)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized webhook")
		return
	}
	secret, err := s.keys.Open(repo.HookSecret)
	if err != nil || secret == "" {
		s.log.Error("webhook secret unreadable", "repo", repo.FullName)
		writeError(w, http.StatusUnauthorized, "unauthorized webhook")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBytes))
	if err != nil {
		http.Error(w, "unreadable payload", http.StatusBadRequest)
		return
	}
	if err := provider.VerifyWebhook(r, body, secret); err != nil {
		s.log.Warn("webhook rejected", "repo", repo.FullName, "reason", err)
		writeError(w, http.StatusUnauthorized, "unauthorized webhook")
		return
	}
	if ping(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if !isPush(r) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	push, err := provider.ParsePush(body)
	if err != nil {
		http.Error(w, "unreadable payload", http.StatusBadRequest)
		return
	}
	// The secret already proves the sender; matching the repository identity
	// makes sure a hook cannot be pointed at another entry of the same user.
	if push.RepoID != "" && repo.ID != "" && push.RepoID != repo.ID {
		s.log.Warn("webhook repository mismatch", "repo", repo.FullName, "payload", push.FullName)
		http.Error(w, "repository mismatch", http.StatusBadRequest)
		return
	}
	if !push.IsBranch || push.IsDeletion {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	branch := strings.TrimPrefix(push.Ref, "refs/heads/")
	if s.cfg.DefaultBranchOnly && repo.DefaultBranch != "" && branch != repo.DefaultBranch {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	job := analysis.Job{
		UserKey: route.UserKey, RepoKey: route.RepoKey, Commit: push.After, Before: push.Before,
		Ref: push.Ref, Message: push.Message, Author: push.Author, Trigger: analysis.TriggerPush,
	}
	if err := s.runner.Enqueue(job); err != nil {
		s.writeEnqueueError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "commit": push.After})
}

// maxHookKeyBytes bounds a routing key before it is even pattern-matched.
const maxHookKeyBytes = 128

// hookTokenParam names the installation token in the webhook URL.
const hookTokenParam = "token"

// hookInstallation resolves a routing key and checks the installation token
// against the one sealed on a monitored repository whose owner still exists.
func (s *Server) hookInstallation(hookKey, token string) (store.HookRoute, *store.Repo, bool) {
	if token == "" || !store.ValidKey(hookKey) {
		return store.HookRoute{}, nil, false
	}
	route, err := s.store.Hook(hookKey)
	if err != nil {
		return store.HookRoute{}, nil, false
	}
	repo, err := s.store.Repo(route.UserKey, route.RepoKey)
	if err != nil || !repo.Monitored || repo.HookKey != hookKey || repo.HookToken == "" {
		return store.HookRoute{}, nil, false
	}
	want, err := s.keys.Open(repo.HookToken)
	if err != nil || want == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return store.HookRoute{}, nil, false
	}
	// The installation belongs to an account: once it is gone, so is the hook.
	if _, err := s.store.User(route.UserKey); err != nil {
		return store.HookRoute{}, nil, false
	}
	return route, repo, true
}

// writeEnqueueError maps a refused analysis onto the status a caller acts on:
// a forge retries a 503, and a 429 tells a user they are at their quota.
func (s *Server) writeEnqueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, analysis.ErrQuota):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, analysis.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func ping(r *http.Request) bool {
	return r.Header.Get("X-GitHub-Event") == "ping"
}

func isPush(r *http.Request) bool {
	if r.Header.Get("X-GitHub-Event") == "push" {
		return true
	}
	event := r.Header.Get("X-Gitlab-Event")
	return event == "Push Hook" || event == "Tag Push Hook"
}
