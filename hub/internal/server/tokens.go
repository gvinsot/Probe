package server

// Agent tokens: the credentials coding agents present to the MCP endpoint.
// A signed-in user creates them from the dashboard (session cookie and CSRF
// token), sees each one once, and revokes them there. Only a SHA-256 of the
// token is stored, on the account it belongs to, so the token names its
// account and is checked against that account's list only.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
)

const (
	agentTokenPrefix = "probe_mcp."
	// maxAgentTokens bounds the tokens of one account.
	maxAgentTokens = 20
	// maxAgentTokenName bounds the label of a token.
	maxAgentTokenName = 80
	// maxAgentTokenDays bounds a token's lifetime; 0 days means no expiry.
	maxAgentTokenDays = 366
	// lastUsedGranularity limits last-use writes to one per token and period.
	lastUsedGranularity = time.Hour
)

var errAgentToken = errors.New("invalid or expired agent token")

// agentTokenView is what the dashboard sees of a token: never its hash.
type agentTokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func viewAgentToken(t store.AgentToken) agentTokenView {
	v := agentTokenView{ID: t.ID, Name: t.Name, Scope: t.Scope, CreatedAt: t.CreatedAt}
	if !t.ExpiresAt.IsZero() {
		e := t.ExpiresAt
		v.ExpiresAt = &e
	}
	if !t.LastUsedAt.IsZero() {
		u := t.LastUsedAt
		v.LastUsedAt = &u
	}
	return v
}

func hashAgentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newAgentToken returns a token for userKey: the prefix, the account key and
// 256 random bits.
func newAgentToken(userKey string) (string, error) {
	secret, err := secrets.Random(32)
	if err != nil {
		return "", err
	}
	return agentTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(userKey)) + "." + secret, nil
}

// agentTokenUser extracts the account key a token names.
func agentTokenUser(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, agentTokenPrefix)
	if !ok || len(token) > 512 {
		return "", false
	}
	encoded, _, ok := strings.Cut(rest, ".")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || !store.ValidKey(string(raw)) {
		return "", false
	}
	return string(raw), true
}

// authenticateAgent checks a bearer token and returns the identity it stands
// for: the account, as a short session, and the token's scope. Feedback an
// agent writes is signed "login (agent: token name)".
func (s *Server) authenticateAgent(token string, now time.Time) (*agentPrincipal, error) {
	userKey, ok := agentTokenUser(token)
	if !ok {
		return nil, errAgentToken
	}
	user, err := s.store.User(userKey)
	if err != nil {
		return nil, errAgentToken
	}
	hash := []byte(hashAgentToken(token))
	var found *store.AgentToken
	for i := range user.AgentTokens {
		t := &user.AgentTokens[i]
		if subtle.ConstantTimeCompare(hash, []byte(t.Hash)) == 1 {
			found = t
		}
	}
	if found == nil || (!found.ExpiresAt.IsZero() && now.After(found.ExpiresAt)) {
		return nil, errAgentToken
	}
	if now.Sub(found.LastUsedAt) > lastUsedGranularity {
		id := found.ID
		if err := s.store.UpdateUser(user.Key, func(u *store.User) error {
			for i := range u.AgentTokens {
				if u.AgentTokens[i].ID == id {
					u.AgentTokens[i].LastUsedAt = now.UTC()
				}
			}
			return nil
		}); err != nil {
			s.log.Warn("record agent token use", "error", err)
		}
	}
	sess := secrets.Session{UserKey: user.Key, Provider: user.Provider, Login: user.Login + " (agent: " + found.Name + ")", IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix()}
	return &agentPrincipal{session: sess, scope: found.Scope, token: found.Name}, nil
}

// handleTokens lists the agent tokens of the signed-in account.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	_, user, ok := s.require(w, r)
	if !ok {
		return
	}
	if agentOf(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "agent tokens are managed from the dashboard")
		return
	}
	views := make([]agentTokenView, 0, len(user.AgentTokens))
	for _, t := range user.AgentTokens {
		views = append(views, viewAgentToken(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": views, "endpoint": s.mcpURL()})
}

// handleCreateToken issues a token and returns it once.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	if agentOf(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "agent tokens are managed from the dashboard")
		return
	}
	var body struct {
		Name    string `json:"name"`
		Scope   string `json:"scope"`
		Expires int    `json:"expires_days"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.Join(strings.Fields(body.Name), " ")
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, "name the token, for example after the agent that uses it")
		return
	case utf8.RuneCountInString(name) > maxAgentTokenName || !utf8.ValidString(name):
		writeError(w, http.StatusBadRequest, "the token name must be at most 80 characters")
		return
	case body.Scope != store.ScopeRead && body.Scope != store.ScopeWrite:
		writeError(w, http.StatusBadRequest, `scope must be "read" or "write"`)
		return
	case body.Expires < 0 || body.Expires > maxAgentTokenDays:
		writeError(w, http.StatusBadRequest, "expires_days must be between 0 (never) and 366")
		return
	}
	token, err := newAgentToken(sess.UserKey)
	id, idErr := secrets.Random(9)
	if err != nil || idErr != nil {
		writeError(w, http.StatusInternalServerError, "could not create the token")
		return
	}
	now := time.Now().UTC()
	record := store.AgentToken{ID: id, Name: name, Scope: body.Scope, Hash: hashAgentToken(token), CreatedAt: now}
	if body.Expires > 0 {
		record.ExpiresAt = now.Add(time.Duration(body.Expires) * 24 * time.Hour)
	}
	err = s.store.UpdateUser(sess.UserKey, func(u *store.User) error {
		// Expired tokens make room for new ones.
		kept := u.AgentTokens[:0]
		for _, t := range u.AgentTokens {
			if t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt) {
				kept = append(kept, t)
			}
		}
		if len(kept) >= maxAgentTokens {
			return errTooManyTokens
		}
		u.AgentTokens = append(kept, record)
		return nil
	})
	if errors.Is(err, errTooManyTokens) {
		writeError(w, http.StatusConflict, "this account already has 20 agent tokens; revoke one first")
		return
	}
	if err != nil {
		s.log.Error("store agent token", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store the token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "info": viewAgentToken(record), "endpoint": s.mcpURL()})
}

var errTooManyTokens = errors.New("too many agent tokens")

// handleDeleteToken revokes a token at once.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	if agentOf(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "agent tokens are managed from the dashboard")
		return
	}
	id := r.PathValue("token")
	removed := false
	err := s.store.UpdateUser(sess.UserKey, func(u *store.User) error {
		kept := u.AgentTokens[:0]
		for _, t := range u.AgentTokens {
			if t.ID == id {
				removed = true
				continue
			}
			kept = append(kept, t)
		}
		u.AgentTokens = kept
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke the token")
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "unknown token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": id})
}

// mcpURL is the public address of the MCP endpoint.
func (s *Server) mcpURL() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/mcp"
}
