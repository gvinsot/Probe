package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/hub/internal/analysis"
	"github.com/gvinsot/SwiftProof/hub/internal/store"
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

func TestCancelAndRerunFromTheActivityList(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(nil)
	path := "/api/repos/" + repo.Key
	commit := strings.Repeat("b", 40)

	for _, body := range []map[string]string{{"commit": "../x"}, {"commit": commit, "variant": "other"}} {
		if got := h.do(http.MethodPost, path+"/cancel", body); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid cancel: %d %s", got.Code, got.Body)
		}
	}
	if got := h.do(http.MethodPost, path+"/cancel", map[string]string{"commit": commit}); got.Code != http.StatusConflict {
		t.Fatalf("cancel of nothing queued: %d %s", got.Code, got.Body)
	}
	if got := h.do(http.MethodPost, path+"/analyze", map[string]string{"commit": commit}); got.Code != http.StatusAccepted {
		t.Fatalf("queue: %d %s", got.Code, got.Body)
	}
	if got := h.do(http.MethodPost, path+"/cancel", map[string]string{"commit": commit}); got.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", got.Code, got.Body)
	}
	if items := h.server.runner.Activity(h.userKey); len(items) != 1 || items[0].Status != "cancelled" {
		t.Fatalf("activity after cancel: %+v", items)
	}

	// Only a stored result can run again, with the parameters it recorded.
	if got := h.do(http.MethodPost, path+"/rerun", map[string]string{"commit": commit, "variant": "plan"}); got.Code != http.StatusNotFound {
		t.Fatalf("rerun without a result: %d %s", got.Code, got.Body)
	}
	run := store.Run{Commit: commit, Variant: "plan", Intent: "stored intent", Ref: "refs/heads/feature", Status: store.StatusFailed}
	if err := h.store.PutRecord(&store.Record{UserKey: h.userKey, RepoKey: repo.Key, Run: run}); err != nil {
		t.Fatal(err)
	}
	got := h.do(http.MethodPost, path+"/rerun", map[string]string{"commit": commit, "variant": "plan"})
	if got.Code != http.StatusAccepted {
		t.Fatalf("rerun: %d %s", got.Code, got.Body)
	}
	if queued := h.decode(got); queued["commit"] != commit || queued["variant"] != "plan" || queued["status"] != "queued" {
		t.Fatalf("rerun response: %v", queued)
	}

	// Another account cannot reach the repository.
	h.server.runner.Enqueue(analysis.Job{UserKey: "other-account", RepoKey: repo.Key, Commit: commit})
	if got := h.do(http.MethodPost, path+"/cancel", map[string]string{"commit": commit}); got.Code != http.StatusConflict {
		t.Fatalf("cancel must not reach another account's job: %d %s", got.Code, got.Body)
	}
}
