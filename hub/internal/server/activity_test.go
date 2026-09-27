package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/hub/internal/analysis"
)

func TestAnalysesAuthenticationAndIsolation(t *testing.T) {
	h := newHarness(t)
	if got := h.do(http.MethodGet, "/api/analyses", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", got.Code)
	}
	h.signIn()
	for _, user := range []string{h.userKey, "other-account"} {
		if err := h.server.runner.Enqueue(analysis.Job{UserKey: user, RepoKey: "repo", Commit: strings.Repeat("a", 40), Variant: "plan", Intent: "private plan intent"}); err != nil {
			t.Fatal(err)
		}
	}
	got := h.do(http.MethodGet, "/api/analyses?user=other-account", nil)
	var body struct {
		Analyses []analysis.Activity `json:"analyses"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got.Code != http.StatusOK || len(body.Analyses) != 1 || body.Analyses[0].Status != "queued" || body.Analyses[0].Variant != "plan" {
		t.Fatalf("activity: %d %s", got.Code, got.Body)
	}
	for _, secret := range []string{"private plan intent", "other-account", "access-token"} {
		if strings.Contains(got.Body.String(), secret) {
			t.Fatalf("activity leaked %s", secret)
		}
	}
}
