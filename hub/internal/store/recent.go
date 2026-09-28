package store

import (
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

func projectRecent(run *Run) RecentRun {
	variant := run.Variant
	if variant == "" {
		variant = "normal"
	}
	return RecentRun{
		Commit: bounded(run.Commit, 120), Status: bounded(run.Status, 16), Variant: bounded(variant, 16),
		QueuedAt: run.QueuedAt, FinishedAt: run.FinishedAt,
		Summary: RecentSummary{Verdict: bounded(run.Summary.Verdict, 16), Counts: run.Summary.Counts},
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

// ReposWithRecent snapshots repository metadata, then copies bounded per-repo
// indexes. First access can rebuild from artifacts under a repository lock;
// report work never holds the account metadata lock.
func (s *Store) ReposWithRecent(userKey string, since time.Time) ([]PublicRepo, error) {
	repos, err := s.Repos(userKey)
	if err != nil {
		return nil, err
	}
	out := make([]PublicRepo, 0, len(repos))
	for _, repo := range repos {
		public := repo.Public()
		recent, incomplete, err := s.Recent(userKey, repo.Key, since)
		if err != nil {
			return nil, err
		}
		public.Recent, public.RecentIncomplete = recent, incomplete
		out = append(out, public)
	}
	return out, nil
}
