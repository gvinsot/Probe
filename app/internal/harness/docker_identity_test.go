package harness

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/dockerutil"
	"github.com/gvinsot/Probe/app/internal/model"
)

func TestProbeDockerIdentity(t *testing.T) {
	docker := goodDocker()
	identity, err := probeDocker(context.Background(), docker.run, "golang:1.26-bookworm")
	if err != nil {
		t.Fatal(err)
	}
	if identity != (dockerIdentity{ImageID: pinnedImage, Server: testServer, NCPU: 8, MemTotal: 8589934592}) || docker.calls != 2 {
		t.Fatalf("identity %+v calls %d", identity, docker.calls)
	}
	absent := &fakeDocker{inspect: func(ref string) (string, string, error) {
		return "", "Error: No such image: " + ref, fmt.Errorf("exit status 1")
	}}
	if _, err := probeDocker(context.Background(), absent.run, "missing:tag"); err == nil || !strings.Contains(err.Error(), "not present locally") || absent.calls != 1 {
		t.Fatalf("absent image: %v (calls %d)", err, absent.calls)
	}
	if _, err := probeDocker(context.Background(), goodDocker().run, "-rm"); err == nil {
		t.Fatal("an option-like reference was probed")
	}
	// Each command is bounded in time.
	slow := func(ctx context.Context, args []string, _ int) ([]byte, []byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > probeTimeout {
			t.Errorf("probe command %q is not bounded by %v", args, probeTimeout)
		}
		return nil, nil, ctx.Err()
	}
	probeDocker(context.Background(), slow, "golang:1.26-bookworm")

	// Both commands share one probeTimeout: the second one never gets a later
	// deadline than the first.
	var deadlines []time.Time
	shared := func(ctx context.Context, args []string, limit int) ([]byte, []byte, error) {
		deadline, _ := ctx.Deadline()
		deadlines = append(deadlines, deadline)
		return goodDocker().run(ctx, args, limit)
	}
	if _, err := probeDocker(context.Background(), shared, "golang:1.26-bookworm"); err != nil || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("the probe commands have separate deadlines %v (%v)", deadlines, err)
	}
	// The caller's context bounds the probe too: a cancelled context (the
	// review's --deadline or an interrupt) stops it at once.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	stopped := func(ctx context.Context, _ []string, _ int) ([]byte, []byte, error) {
		calls++
		return nil, nil, ctx.Err()
	}
	if _, err := probeDocker(ended, stopped, "golang:1.26-bookworm"); err == nil || calls > 1 {
		t.Fatalf("a cancelled context did not stop the probe: %v (calls %d)", err, calls)
	}
}

// With a context, the probe of newExecStateContext is bounded by it: an ended
// context disables only the cache, with a reason.
func TestExecStateProbeHonoursTheCallerContext(t *testing.T) {
	var seen context.Context
	previous := dockerRunner
	dockerRunner = func(ctx context.Context, _ []string, _ int) ([]byte, []byte, error) {
		seen = ctx
		return nil, nil, ctx.Err()
	}
	t.Cleanup(func() { dockerRunner = previous })
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := newExecStateContext(ended, Options{Image: "golang:1.26-bookworm", Cache: newMemoryCache()})
	if err != nil || s.cache != nil || !strings.Contains(s.reason, "could not be pinned") || seen == nil || seen.Err() == nil {
		t.Fatalf("state %+v, err %v", s, err)
	}
}

// With a real Docker daemon: the cache pins the preloaded image to its ID,
// `docker run` accepts that ID, and a third identical baseline run is replayed
// after two agreeing live runs.
func TestDockerExecutionCachePinsAndReplays(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	want, found, err := dockerutil.InspectImage(context.Background(), nil, image)
	if err != nil || !found {
		t.Fatalf("inspect %s: %v found=%v", image, err, found)
	}
	base, head := t.TempDir(), t.TempDir()
	for _, dir := range []string{base, head} {
		os.MkdirAll(filepath.Join(dir, "pkg"), 0755)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/cache\n\ngo 1.23\n"), 0644)
		os.WriteFile(filepath.Join(dir, "pkg", "value.go"), []byte("package pkg\n\nfunc Value() int { return 42 }\n"), 0644)
	}
	m := newMemoryCache()
	h, err := New(Options{BaseDir: base, CandidateDir: head, ArtifactDir: t.TempDir(), Image: image, Cache: m, Timeout: 3 * time.Minute, MaxRuntime: 20 * time.Minute, MaxOutputBytes: 64 * 1024,
		Commands: map[string][]string{"test": {"go", "vet", "./..."}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if e := h.Execution(); e.Cache.Status != model.CacheEnabled || e.Cache.ImageID != want.ID || e.Cache.Runtime == "" {
		t.Fatalf("probe: %+v", e.Cache)
	}
	command := []string{"go", "vet", "./..."}
	var logs []string
	for i := 1; i <= 3; i++ {
		c, _, _ := runLocked(h, context.Background(), model.CheckGeneratedBase, h.base, command, runOptions{})
		logs = append(logs, c.Output)
		switch {
		case c.Status != "PASS":
			t.Fatalf("run %d on %s: %s %d\n%s", i, h.opts.Image, c.Status, c.ExitCode, c.Output)
		case i < 3 && (c.Replayed() || c.Cache == nil || c.Cache.LiveRuns != i):
			t.Fatalf("live run %d: %+v", i, c.Cache)
		case i == 3 && (!c.Replayed() || c.DurationMS != 0):
			t.Fatalf("third run was not replayed: %+v", c.Cache)
		}
	}
	if h.opts.Image != want.ID {
		t.Fatalf("keyed runs used %q, not the pinned ID %q", h.opts.Image, want.ID)
	}
	// The candidate side always executes, on the pinned image.
	var out strings.Builder
	h.execute = func(ctx context.Context, name string, args []string, w io.Writer) execution {
		out.WriteString(imageOf(args))
		return dockerExecute(ctx, name, args, w)
	}
	if c := h.Run(context.Background(), "test"); c.Cache != nil || c.Status != "PASS" || c.Replayed() {
		t.Fatalf("candidate run %s %+v: %s", c.Status, c.Cache, c.Output)
	}
	if out.String() != want.ID {
		t.Fatalf("candidate run image %q, want the pinned %q", out.String(), want.ID)
	}
	if len(m.keys()) != 1 {
		t.Fatalf("the candidate run touched the cache: %v", m.keys())
	}
}
