package analysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

func TestGitFailuresNeverExposeProcessOutput(t *testing.T) {
	bin := t.TempDir()
	// A fake Git echoes both its authentication environment and a bare token.
	// Neither output stream may escape a failed command, including cat-file.
	script := "#!/bin/sh\nprintf '%s\\n' \"$GIT_CONFIG_VALUE_0\" 'secret-token'\nprintf '%s\\n' \"$GIT_CONFIG_VALUE_0\" 'secret-token' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	g := &gitRunner{dir: t.TempDir(), env: gitEnv(t.TempDir(), "https://host/repo", "Basic secret-encoded")}
	out, err := g.run(context.Background(), "fetch", "origin")
	if err == nil || out != "" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("git failure leaked: %q %v", out, err)
	}
	blob, err := g.blob(context.Background(), "HEAD", "policy")
	if err == nil || len(blob) != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("blob failure leaked: %q %v", blob, err)
	}
}

func TestQueueTimeSurvivesWorkerStartAndFailure(t *testing.T) {
	r, st := testRunner(t, "/must-not-run")
	if err := st.PutRepo("user", &store.Repo{Key: "repo"}); err != nil {
		t.Fatal(err)
	}
	queued := time.Now().UTC().Add(-time.Hour)
	job := Job{UserKey: "user", RepoKey: "repo", Commit: strings.Repeat("a", 40), Variant: "normal", queuedAt: queued}
	r.markQueued(job)
	r.process(context.Background(), job) // missing account; no external calls
	rec, err := st.Record("user", "repo", job.Commit)
	if err != nil || !rec.QueuedAt.Equal(queued) {
		t.Fatalf("queue time changed: %+v %v", rec, err)
	}
	repo, err := st.Repo("user", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.Public().Latest.QueuedAt.Equal(queued) || repo.Public().Latest.Variant != "normal" {
		t.Fatal("public latest lost queue time or variant")
	}
}
