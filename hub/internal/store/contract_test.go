package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/jackc/pgx/v5"
)

// testDatabaseEnv names a PostgreSQL database the tests may create schemas in.
// Without it, only the file store runs the contract.
const testDatabaseEnv = "PROBE_TEST_POSTGRES_CONNECTION_STRING"

// openPostgres opens a store in a schema of its own, dropped after the test.
func openPostgres(t *testing.T) *Postgres {
	t.Helper()
	conn := os.Getenv(testDatabaseEnv)
	if conn == "" {
		t.Skipf("%s is not set", testDatabaseEnv)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	suffix := make([]byte, 6)
	rand.Read(suffix)
	schema := "test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	u, err := url.Parse(conn)
	if err != nil {
		t.Fatalf("%s must be a URL: %v", testDatabaseEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	p, err := OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// eachStore runs a check against every implementation.
func eachStore(t *testing.T, check func(t *testing.T, s Store)) {
	t.Run("files", func(t *testing.T) { check(t, open(t)) })
	t.Run("postgres", func(t *testing.T) { check(t, openPostgres(t)) })
}

func TestContractAccounts(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		if _, err := s.User("missing-000000000000"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
		if err := s.UpdateUser("missing-000000000000", func(*User) error { return nil }); !errors.Is(err, ErrNotFound) {
			t.Fatalf("updating a missing account: %v", err)
		}
		expiry := time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.UTC)
		for _, id := range []string{"2", "1"} {
			u := &User{Key: Key("github", id), Provider: "github", ID: id, Login: "user" + id, Token: "sealed", TokenExpiry: expiry}
			if err := s.PutUser(u); err != nil {
				t.Fatalf("PutUser: %v", err)
			}
			if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
				t.Error("PutUser must stamp the record")
			}
		}
		key := Key("github", "1")
		if err := s.UpdateUser(key, func(u *User) error { u.Token = "resealed"; return nil }); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}
		if err := s.UpdateUser(key, func(u *User) error { u.Token = "lost"; return errors.New("no") }); err == nil {
			t.Error("a failing mutation must be reported")
		}
		got, err := s.User(key)
		if err != nil || got.Login != "user1" || got.Token != "resealed" || !got.TokenExpiry.Equal(expiry) {
			t.Fatalf("User = %+v, %v", got, err)
		}
		keys, err := s.UserKeys()
		if err != nil || len(keys) != 2 || keys[0] > keys[1] {
			t.Fatalf("UserKeys = %v, %v; want both, sorted", keys, err)
		}
		if _, err := s.User("../escape"); err == nil {
			t.Error("a traversing key must be refused")
		}
	})
}

func TestContractRepositories(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		userA, userB := Key("github", "1"), Key("github", "2")
		if err := s.PutUser(&User{Key: userA, Login: "octocat"}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"acme/b", "acme/a", "Acme/c"} {
			if err := s.PutRepo(userA, &Repo{Key: Key("github", name), FullName: name}); err != nil {
				t.Fatalf("PutRepo: %v", err)
			}
		}
		if err := s.PutRepo(userB, &Repo{Key: Key("github", "other"), FullName: "other/repo"}); err != nil {
			t.Fatalf("PutRepo: %v", err)
		}
		repos, err := s.Repos(userA)
		if err != nil || len(repos) != 3 {
			t.Fatalf("Repos = %d entries, %v", len(repos), err)
		}
		if names := []string{repos[0].FullName, repos[1].FullName, repos[2].FullName}; strings.Join(names, ",") != "Acme/c,acme/a,acme/b" {
			t.Errorf("repositories must be sorted by name as Go sorts strings, got %v", names)
		}
		key := Key("github", "acme/a")
		if _, err := s.Repo(userB, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another account must not see the repository, got %v", err)
		}
		updated, err := s.UpdateRepo(userA, key, func(r *Repo) error {
			r.Monitored, r.HookKey, r.HookSecret = true, "hook", "sealed"
			return nil
		})
		if err != nil || !updated.Monitored || updated.UpdatedAt.IsZero() {
			t.Fatalf("UpdateRepo = %+v, %v", updated, err)
		}
		if reread, _ := s.Repo(userA, key); !reread.Monitored || reread.HookSecret != "sealed" {
			t.Error("the mutation must be persisted")
		}
		if _, err := s.UpdateRepo(userA, "missing", func(*Repo) error { return nil }); !errors.Is(err, ErrNotFound) {
			t.Errorf("updating a missing repository: %v", err)
		}
		if n, err := s.OutdatedHooks(); err != nil || n != 1 {
			t.Errorf("OutdatedHooks = %d, %v; want 1", n, err)
		}
		if empty, err := s.Repos(Key("github", "nobody")); err != nil || len(empty) != 0 {
			t.Errorf("an unknown account lists nothing, got %d, %v", len(empty), err)
		}
		if _, err := s.Repos("../escape"); err == nil {
			t.Error("a traversing key must be refused")
		}
	})
}

// TestContractUpdatesAreSerialized checks that concurrent mutations of one
// repository never lose an update.
func TestContractUpdatesAreSerialized(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.PutRepo("user", &Repo{Key: "repo", FullName: "acme/shop"}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.UpdateRepo("user", "repo", func(r *Repo) error {
					r.HookID += "x"
					return nil
				}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if r, err := s.Repo("user", "repo"); err != nil || len(r.HookID) != 20 {
			t.Fatalf("%d of 20 updates kept, %v", len(r.HookID), err)
		}
	})
}

func TestContractRoutes(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		route := HookRoute{UserKey: Key("github", "1"), RepoKey: Key("github", "10"), Provider: "github"}
		if err := s.PutHook("key-1", route); err != nil {
			t.Fatalf("PutHook: %v", err)
		}
		if got, err := s.Hook("key-1"); err != nil || got != route {
			t.Fatalf("Hook = %+v, %v", got, err)
		}
		if _, err := s.Badge("key-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a hook key must not resolve as a badge, got %v", err)
		}
		if err := s.PutBadge("key-1", route); err != nil {
			t.Fatalf("PutBadge: %v", err)
		}
		if err := s.DeleteHook("key-1"); err != nil {
			t.Fatalf("DeleteHook: %v", err)
		}
		if _, err := s.Hook("key-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a deleted route must be gone, got %v", err)
		}
		if got, err := s.Badge("key-1"); err != nil || got != route {
			t.Fatalf("deleting a hook must keep the badge, got %+v, %v", got, err)
		}
		if err := s.DeleteBadge("never-existed"); err != nil {
			t.Errorf("deleting an unknown route must be a no-op, got %v", err)
		}
		if err := s.PutHook("../x", route); err == nil {
			t.Error("a traversing key must be refused")
		}
	})
}

func TestContractRecords(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		userKey, repoKey := Key("github", "1"), Key("github", "10")
		base := time.Now().UTC().Add(-time.Hour)
		for i := 0; i < MaxHistory+5; i++ {
			rec := &Record{UserKey: userKey, RepoKey: repoKey, RepoName: "acme/shop", Raw: json.RawMessage(`{"version":1,"i":` + fmt.Sprint(i) + `}`)}
			rec.Commit = commitOf(i)
			rec.QueuedAt = base.Add(time.Duration(i) * time.Minute)
			rec.Status = StatusDone
			rec.Intent = "private intent"
			rec.Summary = report.Summary{Verdict: report.VerdictClear}
			if err := s.PutRecord(rec); err != nil {
				t.Fatalf("PutRecord: %v", err)
			}
		}
		runs, err := s.History(userKey, repoKey, 0)
		if err != nil || len(runs) != MaxHistory+5 {
			t.Fatalf("History = %d runs, %v", len(runs), err)
		}
		if runs[0].Commit != commitOf(MaxHistory+4) || runs[0].Intent != "" {
			t.Errorf("history must be newest first and never carry the intent, got %+v", runs[0])
		}
		for i := 1; i < len(runs); i++ {
			if runs[i-1].QueuedAt.Before(runs[i].QueuedAt) {
				t.Fatal("history must be newest first")
			}
		}
		if limited, _ := s.History(userKey, repoKey, 3); len(limited) != 3 {
			t.Errorf("History(limit 3) returned %d entries", len(limited))
		}
		rec, err := s.Record(userKey, repoKey, commitOf(7))
		if err != nil || string(rec.Raw) != `{"version":1,"i":7}` || rec.RepoName != "acme/shop" || rec.Intent != "private intent" {
			t.Fatalf("Record = %+v, %v", rec, err)
		}
		if !rec.QueuedAt.Equal(base.Add(7 * time.Minute)) {
			t.Errorf("queued time changed: %v", rec.QueuedAt)
		}
		if _, err := s.Record(userKey, repoKey, "unknown"); !errors.Is(err, ErrNotFound) {
			t.Errorf("an unknown commit: %v", err)
		}
		if _, err := s.Record(userKey, repoKey, "../../etc/passwd"); err == nil {
			t.Error("a traversing commit must be refused")
		}
		if _, err := s.RecordVariant(userKey, repoKey, commitOf(1), "other"); err == nil {
			t.Error("an unknown variant must be refused")
		}
		if empty, err := s.History(userKey, Key("github", "11"), 0); err != nil || len(empty) != 0 {
			t.Errorf("History of an unknown repository = %d, %v", len(empty), err)
		}
	})
}

func TestContractVariantsAndReplacement(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		for _, variant := range []string{"", "plan"} {
			rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "abcdef0123", Variant: variant, Status: StatusDone}, Raw: json.RawMessage(`{"variant":"` + variant + `"}`)}
			if err := s.PutRecord(rec); err != nil {
				t.Fatal(err)
			}
		}
		normal, err := s.RecordVariant("user", "repo", "abcdef0123", "normal")
		if err != nil || string(normal.Raw) != `{"variant":""}` {
			t.Fatalf("normal = %+v, %v", normal, err)
		}
		plan, err := s.RecordVariant("user", "repo", "abcdef0123", "plan")
		if err != nil || plan.Variant != "plan" || string(plan.Raw) != `{"variant":"plan"}` {
			t.Fatalf("plan = %+v, %v", plan, err)
		}
		if _, err := s.RecordVariant("other", "repo", "abcdef0123", "plan"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-user: %v", err)
		}
		// A new result for the same commit replaces the previous one; a
		// queued attempt carries no report yet.
		queued := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "abcdef0123", Status: StatusQueued,
			Error: "clone failed: https://x-access-token:ghp_abcdefghijkl@github.com/acme/shop"}}
		if err := s.PutRecord(queued); err != nil {
			t.Fatal(err)
		}
		again, err := s.Record("user", "repo", "abcdef0123")
		if err != nil || again.Status != StatusQueued || again.Raw != nil {
			t.Fatalf("replacement = %+v, %v", again, err)
		}
		if strings.Contains(again.Error, "ghp_") {
			t.Errorf("a stored diagnostic must be redacted: %q", again.Error)
		}
		if runs, err := s.History("user", "repo", 0); err != nil || len(runs) != 2 {
			t.Fatalf("history = %+v, %v", runs, err)
		}
	})
}

func TestContractRecentWindow(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
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
		if err := s.PutRecord(&Record{UserKey: "other", RepoKey: "repo", Run: Run{Commit: "private", QueuedAt: now}}); err != nil {
			t.Fatal(err)
		}
		repos, err := s.ReposWithRecent("owner", now.Add(-2*time.Hour))
		if err != nil || len(repos) != 1 {
			t.Fatalf("repos=%v err=%v", repos, err)
		}
		var order []string
		for _, run := range repos[0].Recent {
			order = append(order, run.Commit)
		}
		// Pending and undated first, then latest activity, ties by commit.
		if got := strings.Join(order, ","); got != "running,undated,finished,boundary,recent" {
			t.Errorf("recent = %s", got)
		}
		if repos[0].RecentIncomplete {
			t.Error("nothing was evicted: the window is complete")
		}
		if _, err := s.ReposWithRecent("../owner", since); err == nil {
			t.Fatal("invalid user key accepted")
		}
	})
}

// TestContractRetention fills a history past MaxRecords: the oldest results
// are evicted, and a window reaching back to them is reported incomplete.
func TestContractRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("writes more than MaxRecords results")
	}
	eachStore(t, func(t *testing.T, s Store) {
		base := time.Now().UTC().Add(-48 * time.Hour)
		for i := 0; i < MaxRecords+3; i++ {
			rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: fmt.Sprintf("commit-%06d", i), Status: StatusDone, QueuedAt: base.Add(time.Duration(i) * time.Minute)}}
			if err := s.PutRecord(rec); err != nil {
				t.Fatal(err)
			}
		}
		runs, err := s.History("user", "repo", 0)
		if err != nil || len(runs) != MaxRecords || runs[len(runs)-1].Commit != "commit-000003" {
			t.Fatalf("history kept %d, oldest %q, %v", len(runs), runs[len(runs)-1].Commit, err)
		}
		if _, err := s.Record("user", "repo", "commit-000002"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an evicted result must be gone, got %v", err)
		}
		if _, incomplete, err := s.Recent("user", "repo", base); err != nil || !incomplete {
			t.Errorf("a window over evicted results must be incomplete (%v)", err)
		}
		recent, incomplete, err := s.Recent("user", "repo", base.Add(10*time.Minute))
		if err != nil || !incomplete || len(recent) != MaxRecent {
			t.Errorf("a window over more than MaxRecent results is truncated and incomplete: %d, %v, %v", len(recent), incomplete, err)
		}
		if _, incomplete, err := s.Recent("user", "repo", base.Add(time.Duration(MaxRecords-10)*time.Minute)); err != nil || incomplete {
			t.Errorf("a window after the evicted results is complete (%v)", err)
		}
	})
}
