package analysis

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

func TestGitFailuresRedactCredentialsAndKeepDiagnostics(t *testing.T) {
	bin := t.TempDir()
	// A fake Git echoes both its authentication environment and a bare token.
	// Credentials from either stream must not escape, including cat-file.
	script := "#!/bin/sh\nprintf '%s\\n' \"$GIT_CONFIG_VALUE_0\" 'ghs_0123456789abcdefghijklmnopqrstuvwxyz'\nprintf '%s\\n' \"$GIT_CONFIG_VALUE_0\" 'ghs_0123456789abcdefghijklmnopqrstuvwxyz' 'repository unavailable' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	g := &gitRunner{dir: t.TempDir(), env: gitEnv(t.TempDir(), "https://host/repo", "Basic secret-encoded")}
	out, err := g.run(context.Background(), "fetch", "origin")
	if err == nil || out != "" || (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "ghs_") || !strings.Contains(err.Error(), "repository unavailable")) {
		t.Fatalf("git failure leaked: %q %v", out, err)
	}
	blob, err := g.blob(context.Background(), "HEAD", "policy")
	if err == nil || len(blob) != 0 || (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "ghs_") || !strings.Contains(err.Error(), "repository unavailable")) {
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

func TestDiagnosticCaptureCannotBypassLimit(t *testing.T) {
	b := &diagnosticBuffer{}
	// Hide WriterTo to exercise io.Copy's ReaderFrom path as os/exec does.
	r := struct{ io.Reader }{strings.NewReader(strings.Repeat("s", 128<<10))}
	n, err := io.Copy(b, r)
	if err != nil || n != 128<<10 || b.buffer.Len() > 64<<10 || !b.truncated {
		t.Fatalf("unbounded capture: n=%d retained=%d err=%v", n, b.buffer.Len(), err)
	}
	if strings.Contains(b.String(), strings.Repeat("s", 10)) {
		t.Fatal("partial secret exposed")
	}
}

func TestGitDiagnosticRedactsKnownBareToken(t *testing.T) {
	const token = "opaque-short-value"
	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	g := &gitRunner{env: gitEnv("", "https://host/repo", header)}
	got := g.safeDiagnostic("fetch '" + token + "' failed")
	if strings.Contains(got, token) || !strings.Contains(got, "failed") {
		t.Fatalf("known token leaked: %s", got)
	}
}

func TestPublishRunRedactsBeforeStreamingAndPersistence(t *testing.T) {
	r, st := testRunner(t, "/must-not-run")
	if err := st.PutRepo("user", &store.Repo{Key: "repo"}); err != nil {
		t.Fatal(err)
	}
	stream, cancel := r.events.Subscribe("user")
	defer cancel()
	const token = "ghs_0123456789abcdefghijklmnopqrstuvwxyz"
	j := Job{UserKey: "user", RepoKey: "repo", Commit: "abc"}
	r.publishRun(j, store.Run{Commit: j.Commit, Status: store.StatusFailed, Error: "fetch " + token + " failed"})
	for i := 0; i < 2; i++ {
		select {
		case event := <-stream:
			if strings.Contains(string(event), token) {
				t.Fatalf("SSE leaked: %s", event)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	repo, err := st.Repo("user", "repo")
	if err != nil || repo.Latest == nil || strings.Contains(repo.Latest.Error, token) {
		t.Fatalf("latest leaked: %+v %v", repo, err)
	}
}
