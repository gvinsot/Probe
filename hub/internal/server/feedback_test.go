package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

// Votes, comments and replies on a finding are recorded with what the stored
// report says about it; the learned summary counts each person's latest vote
// and quotes the comments with the one a reply answers.
func TestFeedbackOnFindings(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(func(r *store.Repo) { r.HasPolicy = true })
	commit := strings.Repeat("a", 40)
	rec := &store.Record{UserKey: h.userKey, RepoKey: repo.Key, RepoName: repo.FullName, Raw: json.RawMessage(storedReport)}
	rec.Commit, rec.Status, rec.QueuedAt = commit, store.StatusDone, time.Now().UTC()
	if err := h.store.PutRecord(rec); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	path := "/api/repos/" + repo.Key + "/reports/" + commit + "/feedback"

	first := h.do(http.MethodPost, path, map[string]any{"alert_id": "signal:s1", "vote": "up"})
	if first.Code != http.StatusCreated {
		t.Fatalf("vote = %d: %s", first.Code, first.Body)
	}
	down := h.do(http.MethodPost, path, map[string]any{"alert_id": "signal:s1", "vote": "down", "comment": "The guard moved to the caller."})
	if down.Code != http.StatusCreated {
		t.Fatalf("comment = %d: %s", down.Code, down.Body)
	}
	var created struct {
		Entry store.FeedbackEntry `json:"entry"`
	}
	if err := json.Unmarshal(down.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	e := created.Entry
	if e.Topic != "signal:removed_validation" || e.Path != "pay/refund.go" || e.Author != "octocat" || e.Commit != commit {
		t.Fatalf("entry = %+v", e)
	}
	reply := h.do(http.MethodPost, path, map[string]any{"alert_id": "signal:s1", "comment": "Agreed, intentional.", "reply_to": e.ID})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply = %d: %s", reply.Code, reply.Body)
	}

	for name, body := range map[string]map[string]any{
		"unknown finding":  {"alert_id": "signal:nope", "vote": "up"},
		"bad vote":         {"alert_id": "signal:s1", "vote": "meh"},
		"nothing":          {"alert_id": "signal:s1"},
		"reply w/o text":   {"alert_id": "signal:s1", "vote": "up", "reply_to": e.ID},
		"unknown parent":   {"alert_id": "signal:s1", "comment": "x", "reply_to": "fb-0"},
		"oversized":        {"alert_id": "signal:s1", "comment": strings.Repeat("x", maxFeedbackComment+1)},
		"review target":    {"alert_id": "focus:0", "vote": "up"},
		"forged path/kind": {"alert_id": "signal:s1", "vote": "up", "topic": "issue"},
	} {
		if res := h.do(http.MethodPost, path, body); res.Code < 400 {
			t.Errorf("%s: accepted (%d)", name, res.Code)
		}
	}

	listed := h.decode(h.do(http.MethodGet, path, nil))
	if entries := listed["entries"].([]any); len(entries) != 3 || listed["learning"] != true {
		t.Fatalf("listed = %v", listed)
	}

	learned := h.decode(h.do(http.MethodGet, "/api/repos/"+repo.Key+"/learning", nil))
	feedback := learned["feedback"].(map[string]any)
	topic := feedback["topics"].([]any)[0].(map[string]any)
	if topic["topic"] != "signal:removed_validation" || topic["useful"] != 0.0 || topic["not_useful"] != 1.0 {
		t.Fatalf("the latest vote must be the one counted: %v", topic)
	}
	comments := feedback["comments"].([]any)
	if len(comments) != 2 || comments[0].(map[string]any)["reply_to"] != "The guard moved to the caller." {
		t.Fatalf("comments = %v", comments)
	}

	off := h.do(http.MethodPut, "/api/repos/"+repo.Key+"/learning", map[string]any{"enabled": false})
	if off.Code != http.StatusOK || h.decode(off)["repo"].(map[string]any)["learning"] != false {
		t.Fatalf("switch off = %d: %s", off.Code, off.Body)
	}
	if stored, _ := h.store.Repo(h.userKey, repo.Key); !stored.LearningOff || len(stored.Feedback) != 3 {
		t.Fatal("switching learning off must keep what was learned")
	}
	reset := h.do(http.MethodDelete, "/api/repos/"+repo.Key+"/feedback", nil)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset = %d", reset.Code)
	}
	if stored, _ := h.store.Repo(h.userKey, repo.Key); len(stored.Feedback) != 0 || stored.Outcomes != nil {
		t.Fatal("reset kept feedback")
	}
	if res := h.do(http.MethodPost, "/api/repos/x/reports/"+commit+"/feedback", map[string]any{"alert_id": "signal:s1", "vote": "up"}); res.Code != http.StatusBadRequest && res.Code != http.StatusNotFound {
		t.Errorf("unknown repository: %d", res.Code)
	}
}
