package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/hub/internal/report"
)

func TestRecentIndexWindowAndReopen(t *testing.T) {
	s := open(t)
	now := time.Now().UTC()
	since := now.Add(-RecentWindow)
	for _, user := range []string{"owner", "other"} {
		if err := s.PutRepo(user, &Repo{Key: "repo"}); err != nil {
			t.Fatal(err)
		}
	}
	runs := []Run{
		{Commit: "recent", Status: StatusDone, QueuedAt: now.Add(-time.Hour)},
		{Commit: "boundary", Status: StatusDone, QueuedAt: now.Add(-time.Hour)},
		{Commit: "old", Status: StatusDone, QueuedAt: since.Add(-time.Hour)},
		{Commit: "finished", Status: StatusDone, QueuedAt: since.Add(-time.Hour), FinishedAt: now},
		{Commit: "running", Status: StatusRunning, QueuedAt: since.Add(-time.Hour)},
		{Commit: "undated", Status: StatusDone, Summary: report.Summary{Verdict: report.VerdictBlocked}},
		{Commit: "plan", Variant: "plan", Status: StatusDone, QueuedAt: now},
	}
	for _, run := range runs {
		if err := s.PutRecord(&Record{UserKey: "owner", RepoKey: "repo", Run: run}); err != nil {
			t.Fatal(err)
		}
	}
	// Same repo identifier in another account must never leak into the index.
	if err := s.PutRecord(&Record{UserKey: "other", RepoKey: "repo", Run: Run{Commit: "private", QueuedAt: now}}); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		repos, err := s.ReposWithRecent("owner", now.Add(-time.Hour))
		if err != nil || len(repos) != 1 {
			t.Fatalf("repos=%v err=%v", repos, err)
		}
		got := make(map[string]bool)
		for _, run := range repos[0].Recent {
			got[run.Commit] = true
		}
		for _, commit := range []string{"recent", "boundary", "finished", "running", "undated"} {
			if !got[commit] {
				t.Errorf("missing %s: %v", commit, got)
			}
		}
		if len(got) != 5 {
			t.Fatalf("unexpected recent runs: %v", got)
		}
		// Returned slices must not allow mutation of the shared index.
		repos[0].Recent[0].Commit = "tampered"
	}
	check(s)
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	check(reopened)
	// An API snapshot must be independent of report-file reads after startup.
	if err := os.Rename(filepath.Join(s.dir, "reports"), filepath.Join(s.dir, "archived-reports")); err != nil {
		t.Fatal(err)
	}
	check(reopened)
	if _, err := reopened.ReposWithRecent("../owner", since); err == nil {
		t.Fatal("invalid user key accepted")
	}
}

func TestRecentIndexReplacesResultsAndExpiresOldEntries(t *testing.T) {
	s := open(t)
	if err := s.PutRepo("user", &Repo{Key: "repo"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "commit", Status: StatusDone, QueuedAt: now, Summary: report.Summary{Verdict: report.VerdictBlocked}}}
	if err := s.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	rec.Summary.Verdict = report.VerdictClear
	if err := s.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	repos, err := s.ReposWithRecent("user", now.Add(-time.Hour))
	if err != nil || len(repos[0].Recent) != 1 || repos[0].Recent[0].Summary.Verdict != report.VerdictClear {
		t.Fatalf("replacement: %v %v", repos, err)
	}
	// Simulate an index entry aging out without deleting its persisted report.
	key := recentKey{"user", "repo"}
	expired := s.recent[key]["commit"]
	expired.QueuedAt = now.Add(-RecentWindow - time.Hour)
	s.recent[key]["commit"] = expired
	rec.Commit = "new"
	if err := s.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	if len(s.recent[key]) != 1 {
		t.Fatal("expired projection retained in the rolling index")
	}
	if _, err := s.Record("user", "repo", "commit"); err != nil {
		t.Fatal("archived report was lost:", err)
	}
}

func BenchmarkReposWithRecent(b *testing.B) {
	for _, archived := range []int{0, 10000} {
		b.Run(fmt.Sprintf("archived=%d", archived), func(b *testing.B) {
			dir := b.TempDir()
			s, err := Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now().UTC()
			for i := 0; i < 60; i++ {
				repo := fmt.Sprintf("repo-%d", i)
				if err := s.PutRepo("user", &Repo{Key: repo}); err != nil {
					b.Fatal(err)
				}
				for j := 0; j < 240; j++ {
					s.indexRecent("user", repo, RecentRun{Commit: fmt.Sprint(j), QueuedAt: now, Status: StatusDone}, now.Add(-RecentWindow))
				}
			}
			// Archive size must not affect dashboard cost. Seed files directly: they
			// deliberately cannot be parsed as reports, so reading them would fail.
			archive := filepath.Join(dir, "reports", "user", "repo-0")
			if err := os.MkdirAll(archive, 0700); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < archived; i++ {
				if err := os.WriteFile(filepath.Join(archive, fmt.Sprintf("old-%d.json", i)), []byte("archived"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				repos, err := s.ReposWithRecent("user", now.Add(-RecentWindow))
				if err != nil {
					b.Fatal(err)
				}
				if i == 0 {
					payload, err := json.Marshal(map[string]any{"repos": repos})
					if err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(len(payload)), "JSON-bytes")
				}
			}
		})
	}
}

func TestRecentSnapshotsDuringWrites(t *testing.T) {
	s := open(t)
	if err := s.PutRepo("user", &Repo{Key: "repo"}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		for i := 0; i < 30; i++ {
			rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: fmt.Sprint(i), Status: StatusDone, QueuedAt: time.Now().UTC()}}
			if err := s.PutRecord(rec); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	for i := 0; i < 30; i++ {
		if _, err := s.ReposWithRecent("user", time.Now().Add(-RecentWindow)); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	repos, err := s.ReposWithRecent("user", time.Now().Add(-RecentWindow))
	if err != nil || len(repos[0].Recent) != 30 {
		t.Fatalf("lost concurrent results: %v %v", repos, err)
	}
}
