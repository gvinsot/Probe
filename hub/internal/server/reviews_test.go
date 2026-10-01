package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// A finished analysis is marked reviewed and unmarked by the signed-in
// person; the repository projection carries the mark, the history logs both
// actions newest first, and the stored report keeps its CLI verdict.
func TestReviewMarksACommit(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(nil)
	commit := strings.Repeat("a", 40)
	rec := &store.Record{UserKey: h.userKey, RepoKey: repo.Key, RepoName: repo.FullName, Raw: json.RawMessage(storedReport)}
	rec.Commit, rec.Status, rec.Message, rec.QueuedAt = commit, store.StatusDone, "Fix refunds", time.Now().UTC()
	rec.Summary.Verdict = report.VerdictReview
	if err := h.store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	path := "/api/repos/" + repo.Key + "/reports/" + commit + "/review"

	marked := h.do(http.MethodPut, path, map[string]any{"reviewed": true})
	if marked.Code != http.StatusOK {
		t.Fatalf("mark = %d: %s", marked.Code, marked.Body)
	}
	mark, ok := h.decode(marked)["repo"].(map[string]any)["reviewed"].(map[string]any)[commit].(map[string]any)
	if !ok || mark["by"] != "octocat" {
		t.Fatalf("projection = %s", marked.Body)
	}
	// Marking twice records one action.
	h.do(http.MethodPut, path, map[string]any{"reviewed": true})
	unmarked := h.do(http.MethodPut, path, map[string]any{"reviewed": false})
	if unmarked.Code != http.StatusOK {
		t.Fatalf("unmark = %d", unmarked.Code)
	}
	if _, present := h.decode(unmarked)["repo"].(map[string]any)["reviewed"]; present {
		t.Fatalf("a withdrawn mark is still projected: %s", unmarked.Body)
	}

	history := h.do(http.MethodGet, "/api/repos/"+repo.Key+"/reviews", nil)
	var listed struct {
		Reviews []store.ReviewEntry `json:"reviews"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Reviews) != 2 || listed.Reviews[0].Reviewed || !listed.Reviews[1].Reviewed {
		t.Fatalf("history = %+v", listed.Reviews)
	}
	if e := listed.Reviews[1]; e.Commit != commit || e.Message != "Fix refunds" || e.Verdict != report.VerdictReview || e.By != "octocat" {
		t.Fatalf("entry = %+v", e)
	}
	if stored, _ := h.store.Record(h.userKey, repo.Key, commit); stored.Summary.Verdict != report.VerdictReview {
		t.Fatalf("the CLI verdict changed: %q", stored.Summary.Verdict)
	}

	for name, tc := range map[string]struct {
		path string
		body map[string]any
		code int
	}{
		"no report":     {"/api/repos/" + repo.Key + "/reports/" + strings.Repeat("b", 40) + "/review", map[string]any{"reviewed": true}, http.StatusNotFound},
		"plan":          {path + "?variant=plan", map[string]any{"reviewed": true}, http.StatusBadRequest},
		"unknown field": {path, map[string]any{"reviewed": true, "by": "someone"}, http.StatusBadRequest},
	} {
		if res := h.do(http.MethodPut, tc.path, tc.body); res.Code != tc.code {
			t.Errorf("%s: %d", name, res.Code)
		}
	}
}

func TestReviewHistoryIsBounded(t *testing.T) {
	r := &store.Repo{}
	for i := 0; i < store.MaxReviews+5; i++ {
		r.AddReview(store.ReviewEntry{Commit: "c", Reviewed: i%2 == 0})
	}
	if len(r.Reviews) != store.MaxReviews {
		t.Fatalf("kept %d", len(r.Reviews))
	}
	if _, marked := r.Reviewed()["c"]; !marked {
		t.Fatal("the latest action marked c reviewed")
	}
}
