package store

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/hub/internal/report"
)

// RecentRun is the minimum browser projection needed to aggregate statuses.
// In particular, pipeline errors, commit metadata and plan intent stay out of
// the account-wide repository listing (including its latest result).
type RecentRun struct {
	Commit     string        `json:"commit"`
	Status     string        `json:"status"`
	Variant    string        `json:"variant,omitempty"`
	QueuedAt   time.Time     `json:"queued_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Summary    RecentSummary `json:"summary"`
}

// RecentSummary carries only the trusted verdict and severity counts.
type RecentSummary struct {
	Verdict string        `json:"verdict"`
	Counts  report.Counts `json:"counts"`
}

type recentKey struct{ user, repo string }

func projectRecent(run *Run) RecentRun {
	return RecentRun{
		Commit: run.Commit, Status: run.Status, Variant: run.Variant,
		QueuedAt: run.QueuedAt, FinishedAt: run.FinishedAt,
		Summary: RecentSummary{Verdict: run.Summary.Verdict, Counts: run.Summary.Counts},
	}
}

// activityAt keeps a long-running analysis in the window when it finishes.
func (r RecentRun) activityAt() time.Time {
	if r.FinishedAt.After(r.QueuedAt) {
		return r.FinishedAt
	}
	return r.QueuedAt
}

func (r RecentRun) inWindow(since time.Time) bool {
	at := r.activityAt()
	return r.Status == StatusQueued || r.Status == StatusRunning || at.IsZero() || !at.Before(since)
}

// loadRecent builds the rolling index once, before the store is shared. Old
// installations need no migration. Full reports are never parsed on dashboard
// requests; only the compact, current-window projections are retained here.
func (s *Store) loadRecent() error {
	root := filepath.Join(s.dir, "reports")
	since := time.Now().Add(-RecentWindow)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || !ValidKey(parts[0]) || !ValidKey(parts[1]) ||
			!strings.HasSuffix(parts[2], ".json") || strings.HasSuffix(parts[2], ".plan.json") {
			return nil
		}
		var run RecentRun
		if err := readJSON(path, &run); err != nil {
			return nil // Same tolerance as History.
		}
		if !ValidKey(run.Commit) {
			return nil
		}
		s.indexRecent(parts[0], parts[1], run, since)
		return nil
	})
}

// indexRecent runs under the write lock (or during Open). Updating one commit
// replaces its earlier result, just like the persisted report cache.
func (s *Store) indexRecent(user, repo string, run RecentRun, since time.Time) {
	if run.Variant != "" && run.Variant != "normal" {
		return
	}
	key := recentKey{user, repo}
	if !run.inWindow(since) {
		delete(s.recent[key], run.Commit)
		return
	}
	if s.recent[key] == nil {
		s.recent[key] = make(map[string]RecentRun)
	}
	s.recent[key][run.Commit] = run
}

// ReposWithRecent snapshots the account's repos and rolling index under one
// read lock. Its report work depends on the recent window, never on archived
// history, and performs no report-file reads. The returned values are copies.
func (s *Store) ReposWithRecent(userKey string, since time.Time) ([]PublicRepo, error) {
	s.mu.RLock()
	repos, err := s.reposLocked(userKey)
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	out := make([]PublicRepo, 0, len(repos))
	for _, repo := range repos {
		public := repo.Public()
		for _, run := range s.recent[recentKey{userKey, repo.Key}] {
			if run.inWindow(since) {
				public.Recent = append(public.Recent, run)
			}
		}
		out = append(out, public)
	}
	s.mu.RUnlock()
	// Sorting detached snapshots must not hold up analysis writers.
	for _, public := range out {
		sort.Slice(public.Recent, func(i, j int) bool {
			a, b := public.Recent[i], public.Recent[j]
			if a.activityAt().Equal(b.activityAt()) {
				return a.Commit < b.Commit
			}
			return a.activityAt().After(b.activityAt())
		})
	}
	return out, nil
}
