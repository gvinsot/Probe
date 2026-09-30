package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenPostgresHidesTheConnectionString(t *testing.T) {
	_, err := OpenPostgres(context.Background(), "postgresql://app:s3cret-password@localhost:5432/db?sslmode=bogus")
	if err == nil {
		t.Fatal("an invalid connection string must be refused")
	}
	if strings.Contains(err.Error(), "s3cret-password") {
		t.Fatalf("the error leaks the password: %v", err)
	}
}

func TestPostgresMigrationsAreApplied(t *testing.T) {
	p := openPostgres(t)
	// A second start finds every migration applied.
	if err := p.migrate(context.Background()); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	var versions int
	if err := p.pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("schema_migrations = %d, %v", versions, err)
	}
}

func TestPostgresImportsTheDataDirectoryOnce(t *testing.T) {
	p := openPostgres(t)
	ctx := context.Background()
	if stats, err := p.ImportFiles(ctx, t.TempDir()); err != nil || stats.Ran {
		t.Fatalf("an empty directory has nothing to import: %+v, %v", stats, err)
	}

	dir := t.TempDir()
	files, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := &User{Key: "user", Provider: "github", ID: "1", Login: "octocat", Token: "sealed"}
	if err := files.PutUser(user); err != nil {
		t.Fatal(err)
	}
	repo := &Repo{Key: "repo", FullName: "acme/shop", Monitored: true, HookKey: "hook", HookToken: "sealed-token", BadgeKey: "badge"}
	if err := files.PutRepo("user", repo); err != nil {
		t.Fatal(err)
	}
	route := HookRoute{UserKey: "user", RepoKey: "repo", Provider: "github"}
	if err := files.PutHook("hook", route); err != nil {
		t.Fatal(err)
	}
	if err := files.PutBadge("badge", route); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, run := range []Run{
		{Commit: "first", Status: StatusDone, QueuedAt: now.Add(-2 * time.Hour), Intent: "kept"},
		{Commit: "first", Variant: "plan", Status: StatusDone, QueuedAt: now.Add(-time.Hour)},
		{Commit: "broken", Status: StatusDone, QueuedAt: now},
	} {
		rec := &Record{UserKey: "user", RepoKey: "repo", RepoName: "acme/shop", Run: run, Raw: json.RawMessage(`{"commit":"` + run.Commit + `"}`)}
		if err := files.PutRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "user", "repo", "broken.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := p.ImportFiles(ctx, dir)
	if err != nil || !stats.Ran {
		t.Fatalf("ImportFiles = %+v, %v", stats, err)
	}
	if stats.Users != 1 || stats.Repos != 1 || stats.Routes != 2 || stats.Runs != 2 {
		t.Errorf("imported %+v", stats)
	}
	got, err := p.User("user")
	if err != nil || got.Token != "sealed" || !got.UpdatedAt.Equal(user.UpdatedAt) {
		t.Fatalf("imported account = %+v, %v", got, err)
	}
	if r, err := p.Repo("user", "repo"); err != nil || r.HookToken != "sealed-token" || !r.UpdatedAt.Equal(repo.UpdatedAt) {
		t.Fatalf("imported repository = %+v, %v", r, err)
	}
	if h, err := p.Hook("hook"); err != nil || h != route {
		t.Fatalf("imported hook = %+v, %v", h, err)
	}
	if b, err := p.Badge("badge"); err != nil || b != route {
		t.Fatalf("imported badge = %+v, %v", b, err)
	}
	rec, err := p.Record("user", "repo", "first")
	if err != nil || rec.Intent != "kept" || string(rec.Raw) != `{"commit":"first"}` {
		t.Fatalf("imported record = %+v, %v", rec, err)
	}
	if plan, err := p.RecordVariant("user", "repo", "first", "plan"); err != nil || plan.Variant != "plan" {
		t.Fatalf("imported plan = %+v, %v", plan, err)
	}
	// The unreadable result is missing: the window must say so.
	if _, incomplete, err := p.Recent("user", "repo", now.Add(-RecentWindow)); err != nil || !incomplete {
		t.Errorf("a window missing an unreadable result must be incomplete (%v)", err)
	}

	// Changes made after the import are never overwritten by a later start.
	if err := p.UpdateUser("user", func(u *User) error { u.Login = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if again, err := p.ImportFiles(ctx, dir); err != nil || again.Ran {
		t.Fatalf("a completed import must not run again: %+v, %v", again, err)
	}
	if u, _ := p.User("user"); u.Login != "renamed" {
		t.Error("a later start overwrote the database")
	}
}

func TestPostgresRecordSizeIsBounded(t *testing.T) {
	p := openPostgres(t)
	if err := p.PutRecord(&Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "large", Status: StatusDone},
		Raw: json.RawMessage(`"` + strings.Repeat("a", maxRecordBytes) + `"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Record("user", "repo", "large"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("an oversized report must not be loaded, got %v", err)
	}
	if runs, err := p.History("user", "repo", 0); err != nil || len(runs) != 1 {
		t.Fatalf("its metadata stays listed: %d, %v", len(runs), err)
	}
}
