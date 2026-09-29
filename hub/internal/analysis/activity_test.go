package analysis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

func TestActivityLifecycleAndRepeatedAttempts(t *testing.T) {
	r, _ := testRunner(t, "")
	j := Job{UserKey: "owner", RepoKey: "repo", Commit: strings.Repeat("a", 40)}
	if err := r.Enqueue(j); err != nil {
		t.Fatal(err)
	}
	if err := r.Enqueue(j); err != nil {
		t.Fatal(err)
	}
	items := r.Activity("owner")
	if len(items) != 1 || items[0].Status != store.StatusQueued {
		t.Fatalf("queued: %+v", items)
	}
	if len(r.Activity("other")) != 0 {
		t.Fatal("another account can read activity")
	}
	queued := <-r.queue
	// Missing repository fails before any external command, exercising real transitions.
	r.process(context.Background(), queued)
	r.release(queued)
	items = r.Activity("owner")
	if len(items) != 1 || items[0].Status != store.StatusFailed || items[0].StartedAt.IsZero() || items[0].FinishedAt.IsZero() || items[0].Error == "" {
		t.Fatalf("failed attempt: %+v", items)
	}
	if err := r.Enqueue(j); err != nil {
		t.Fatal(err)
	}
	plan := j
	plan.Variant = "plan"
	plan.Intent = "private task"
	if err := r.Enqueue(plan); err != nil {
		t.Fatal(err)
	}
	items = r.Activity("owner")
	if len(items) != 3 || items[0].Variant != "plan" || items[1].Status != store.StatusQueued || items[2].Status != store.StatusFailed {
		t.Fatalf("attempts not retained separately: %+v", items)
	}
	items[0].Status = "modified"
	if r.Activity("owner")[0].Status == "modified" {
		t.Fatal("snapshot aliases cache")
	}
}

func TestActivityRetention(t *testing.T) {
	r, _ := testRunner(t, "")
	now := time.Now()
	for i, status := range []string{store.StatusDone, store.StatusFailed, store.StatusQueued, store.StatusRunning, store.StatusDone} {
		queued := now.Add(-72 * time.Hour).Add(time.Duration(i) * time.Second)
		finished := now.Add(-49 * time.Hour)
		if i == 4 {
			finished = now.Add(-time.Hour)
		}
		j := Job{UserKey: "owner", RepoKey: "repo", Commit: strings.Repeat("a", 40)}
		r.rememberActivity(j, store.Run{Commit: j.Commit, Status: status, QueuedAt: queued, FinishedAt: finished})
	}
	items := r.Activity("owner")
	if len(items) != 3 || items[0].Status != store.StatusDone || items[1].Status != store.StatusRunning || items[2].Status != store.StatusQueued {
		t.Fatalf("keep active work and recently finished long runs: %+v", items)
	}
	if len(r.activity) != 3 {
		t.Fatal("expired attempts remain in memory")
	}
	fresh, _ := testRunner(t, "")
	if len(fresh.Activity("owner")) != 0 {
		t.Fatal("history persisted across runners")
	}
}

func TestActivityExcludesRejectedJobs(t *testing.T) {
	r, _ := testRunner(t, "")
	r.cfg.UserQuota = 1
	j := Job{UserKey: "owner", RepoKey: "repo", Commit: strings.Repeat("a", 40)}
	if err := r.Enqueue(j); err != nil {
		t.Fatal(err)
	}
	j.Commit = strings.Repeat("b", 40)
	if err := r.Enqueue(j); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
	r.cfg.UserQuota = 0
	for i := 0; i < 3; i++ {
		j.UserKey = string(rune('b' + i))
		if err := r.Enqueue(j); err != nil {
			t.Fatal(err)
		}
	}
	j.UserKey = "rejected"
	if err := r.Enqueue(j); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if len(r.Activity("rejected")) != 0 || len(r.Activity("owner")) != 1 {
		t.Fatal("rejected jobs entered activity")
	}
}
