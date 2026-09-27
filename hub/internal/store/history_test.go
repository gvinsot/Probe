package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Seed legacy artifacts directly: no index, and enough payload to catch any
// accidental full-report reads in a listing. Open performs the one-time upgrade.
func legacyHistory(t testing.TB, n int) (string, time.Time) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "reports", "user", "repo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		rec := Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: fmt.Sprintf("commit-%06d", i), Status: StatusDone, QueuedAt: now.Add(-time.Duration(i) * time.Minute)}, Raw: json.RawMessage(`{"payload":"` + strings.Repeat("x", 4096) + `"}`)}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, recordName(rec.Commit, "")), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, now
}

func TestRecentScaleUsesOnlyMemoryAfterMigrationAndRestart(t *testing.T) {
	root, now := legacyHistory(t, 1005)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "reports", "user", "repo")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != MaxRecords {
		t.Fatalf("retained %d artifacts, want %d", len(files), MaxRecords)
	}
	if _, err := s.Record("user", "repo", "commit-001004"); err != ErrNotFound {
		t.Fatalf("old artifact retained: %v", err)
	}
	// Corrupt every payload after migration. A restart must use the index, not
	// deserialize any of these files. It must also preserve the eviction marker.
	for _, path := range files {
		if err := os.WriteFile(path, []byte("broken payload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Removing the entire directory proves hot reads perform no filesystem I/O;
	// this deterministic assertion avoids timing thresholds on busy CI machines.
	if err := os.Rename(dir, dir+"-offline"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		runs, partial, err := s.Recent("user", "repo", now.Add(-time.Hour))
		if err != nil || partial || len(runs) != 61 {
			t.Fatalf("recent = %d partial=%v err=%v", len(runs), partial, err)
		}
		runs, partial, err = s.Recent("user", "repo", now.Add(-RecentWindow))
		if err != nil || !partial || len(runs) != MaxRecent {
			t.Fatalf("bounded recent = %d partial=%v err=%v", len(runs), partial, err)
		}
	}
	runs, err := s.History("user", "repo", 3)
	if err != nil || len(runs) != 3 {
		t.Fatalf("history = %d, %v", len(runs), err)
	}
	runs[0].Status = "mutated copy"
	again, err := s.History("user", "repo", 1)
	if err != nil || again[0].Status == "mutated copy" {
		t.Fatal("caller mutated index")
	}
}

func TestRecordIndexRecoversInterruptedWrite(t *testing.T) {
	root, now := legacyHistory(t, 1)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "reports", "user", "repo")
	if err := writeJSON(filepath.Join(dir, dirtyFile), true); err != nil {
		t.Fatal(err)
	}
	rec := Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "new", QueuedAt: now.Add(time.Minute), Status: StatusDone}}
	if err := writeJSON(filepath.Join(dir, "new.json"), &rec); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	runs, _, err := s.Recent("user", "repo", now.Add(-time.Hour))
	if err != nil || len(runs) != 2 || runs[0].Commit != "new" {
		t.Fatalf("recovered = %+v, %v", runs, err)
	}
	// An explicit rerun replaces one variant, without mixing the plan and normal.
	rec.Variant = "plan"
	if err := s.PutRecord(&rec); err != nil {
		t.Fatal(err)
	}
	rec.Variant = "normal"
	rec.Status = StatusFailed
	if err := s.PutRecord(&rec); err != nil {
		t.Fatal(err)
	}
	runs, _, err = s.Recent("user", "repo", now.Add(-time.Hour))
	if err != nil || len(runs) != 2 || runs[0].Status != StatusFailed {
		t.Fatalf("rerun = %+v, %v", runs, err)
	}
	history, err := s.History("user", "repo", 0)
	if err != nil || len(history) != 3 {
		t.Fatalf("variants = %+v, %v", history, err)
	}
}

func TestSlowRepositoryDoesNotHoldGlobalStoreLock(t *testing.T) {
	s := open(t)
	idx, _, err := s.index("large-user", "large-repo")
	if err != nil {
		t.Fatal(err)
	}
	idx.mu.Lock()
	blocked := make(chan struct{})
	go func() { defer close(blocked); _, _ = s.History("large-user", "large-repo", 0) }()
	// Simulate a slow repository scan/write. Another tenant must still be able
	// to store its report and publish its repository metadata.
	done := make(chan error, 1)
	go func() {
		err := s.PutRecord(&Record{UserKey: "other-user", RepoKey: "other-repo", Run: Run{Commit: "commit", QueuedAt: time.Now()}})
		if err == nil {
			err = s.PutRepo("other-user", &Repo{Key: "other-repo"})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Error("another tenant is blocked by a repository index")
	}
	idx.mu.Unlock()
	<-blocked
}

func TestConcurrentHistoryAndRecordUpdates(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: fmt.Sprintf("commit-%d", i), QueuedAt: time.Now(), Status: StatusDone}}
				if err := s.PutRecord(rec); err != nil {
					t.Error(err)
					return
				}
				if _, _, err := s.Recent("user", "repo", time.Now().Add(-time.Hour)); err != nil {
					t.Error(err)
				}
				if _, err := s.Record("user", "repo", rec.Commit); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	runs, err := s.History("user", "repo", 0)
	if err != nil || len(runs) != 8 {
		t.Fatalf("runs = %d %v", len(runs), err)
	}
}

func TestRetentionAfterNewWriteSurvivesRestart(t *testing.T) {
	root, now := legacyHistory(t, MaxRecords)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "new", QueuedAt: now.Add(time.Minute)}}
	if err := s.PutRecord(&rec); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record("user", "repo", fmt.Sprintf("commit-%06d", MaxRecords-1)); err != ErrNotFound {
		t.Fatalf("eviction: %v", err)
	}
	history, err := s.History("user", "repo", 0)
	if err != nil || len(history) != MaxRecords || history[0].Commit != "new" {
		t.Fatalf("retained = %d, %v", len(history), err)
	}
}

func BenchmarkRecentIndexed(b *testing.B) {
	for _, n := range []int{50, 1000, 5000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			root, now := legacyHistory(b, n)
			s, err := Open(root)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := s.Recent("user", "repo", now.Add(-RecentWindow)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
