package analysis

import (
	"sort"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

// ActivityWindow retains completed attempts for 48 hours in this process only.
const ActivityWindow = 48 * time.Hour

// Activity describes an attempt without retaining a report or plan intent.
type Activity struct {
	RepoKey    string    `json:"repo_key"`
	Commit     string    `json:"commit"`
	Variant    string    `json:"variant"`
	Status     string    `json:"status"`
	Trigger    string    `json:"trigger,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Error      string    `json:"error,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type activityKey struct {
	user     string
	job      string
	queuedAt time.Time
}

func (r *Runner) rememberActivity(j Job, run store.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneActivity(time.Now())
	variant := run.Variant
	if variant == "" {
		variant = "normal"
	}
	r.activity[activityKey{j.UserKey, jobKey(j), run.QueuedAt}] = Activity{
		RepoKey: j.RepoKey, Commit: run.Commit, Variant: variant,
		Status: run.Status, Trigger: run.Trigger, Mode: run.Mode, Error: store.SafeError(run.Error),
		QueuedAt: run.QueuedAt, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
}

// pruneActivity is called under mu. Active work remains visible even after 48h.
func (r *Runner) pruneActivity(now time.Time) {
	cutoff := now.Add(-ActivityWindow)
	for key, item := range r.activity {
		if (item.Status == store.StatusDone || item.Status == store.StatusFailed || item.Status == store.StatusCancelled) && item.FinishedAt.Before(cutoff) {
			delete(r.activity, key)
		}
	}
}

// Activity returns a snapshot scoped to the authenticated account. Attempts are
// keyed by their enqueue time so rerunning a commit preserves its earlier runs.
func (r *Runner) Activity(userKey string) []Activity {
	r.mu.Lock()
	r.pruneActivity(time.Now())
	items := make([]Activity, 0)
	for key, item := range r.activity {
		if key.user == userKey {
			items = append(items, item)
		}
	}
	r.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].QueuedAt.Equal(items[j].QueuedAt) {
			return items[i].RepoKey+items[i].Commit+items[i].Variant < items[j].RepoKey+items[j].Commit+items[j].Variant
		}
		return items[i].QueuedAt.After(items[j].QueuedAt)
	})
	return items
}
