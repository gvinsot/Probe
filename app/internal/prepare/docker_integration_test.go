package prepare

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// countingDocker counts the containers the real client starts.
type countingDocker struct {
	DockerCLI
	mu    sync.Mutex
	names []string
}

func (c *countingDocker) Run(ctx context.Context, name string, args []string, scaffold []byte, log io.Writer) (bool, int, error) {
	c.mu.Lock()
	c.names = append(c.names, name)
	c.mu.Unlock()
	return c.DockerCLI.Run(ctx, name, args, scaffold, log)
}

func dockerRepo(t *testing.T) (*gitrepo.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "core.autocrlf", "false"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "deps.txt"), []byte("dep v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "-C", dir, "add", "-A")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c = exec.Command("git", "-C", dir, "commit", "--no-gpg-sign", "-m", "base")
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

// runCheck runs command as a sandbox check in image and returns its status.
func runCheck(t *testing.T, image, command string) model.Check {
	t.Helper()
	candidate := t.TempDir()
	if err := os.WriteFile(filepath.Join(candidate, "README"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{CandidateDir: candidate, ArtifactDir: t.TempDir(), Image: image, Timeout: 2 * time.Minute, MaxRuntime: 5 * time.Minute,
		Commands: map[string][]string{"test": {"sh", "-c", command}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	return h.Run(context.Background(), "test")
}

func removeImages(t *testing.T, ids ...string) {
	for _, id := range ids {
		if id != "" {
			_ = exec.Command("docker", "image", "rm", id).Run()
		}
	}
}

func assertNoContainers(t *testing.T, names []string) {
	t.Helper()
	for _, name := range names {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}").Output()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("container %s remains (%v): %s", name, err, out)
		}
	}
}

// The real offline path: a sandbox-user prepare with no network commits an
// image whose layers are the base layers plus one and whose environment
// carries the policy env; a check in that image sees the prepared file, runs
// without the prepare container's /tmp, and the same check fails on the base
// image. The prepare container had no network interface other than loopback,
// ran as 65534 in /swiftproof/work, and saw no host environment. A second run
// reuses the image without starting a container. A root prepare gets exactly
// the fixed capability set.
func TestDockerPrepareOfflineCommitAndReuse(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	t.Setenv("SWIFTPROOF_HOST_SECRET", "must-not-reach-prepare")
	repo, commit := dockerRepo(t)
	dir := "/go/pkg/swiftproof-prepare-test-" + randomHex(4)
	script := `set -e; d="$PREPARED_DIR"; mkdir -p "$d"; cp deps.txt "$d/deps.txt"; id -u > "$d/uid"; pwd > "$d/pwd"; echo "$HOME" > "$d/home"; ls /sys/class/net > "$d/net"; env > "$d/env"; echo scratch > /tmp/scratch; echo prepared`
	docker := &countingDocker{}
	o := Options{
		Spec:      config.Prepare{Command: []string{"sh", "-c", script}, Inputs: []string{"deps.txt"}, Env: map[string]string{"PREPARED_DIR": dir}},
		BaseImage: image, SourceCommit: commit, Repo: repo, MemoryMB: 512, CPUs: 1, MaxOutputBytes: 32 << 10,
		ArtifactDir: t.TempDir(), ToolVersion: "docker-test", Docker: docker, Runner: dockerutil.DefaultRunner, Progress: &bytes.Buffer{},
	}
	first := Run(context.Background(), o)
	defer removeImages(t, first.Image)
	if first.Record.Status != model.PrepareBuilt || !first.Ready() {
		t.Fatalf("first run %+v", first.Record)
	}
	if len(first.Artifacts) != 1 {
		t.Fatalf("artifacts %+v", first.Artifacts)
	}
	log, err := os.ReadFile(first.Artifacts[0].Path)
	if err != nil || !strings.Contains(string(log), "prepared") {
		t.Fatalf("prepare log %q %v", log, err)
	}
	base, _, err := dockerutil.InspectImage(context.Background(), nil, image)
	if err != nil {
		t.Fatal(err)
	}
	derived, found, err := dockerutil.InspectImage(context.Background(), nil, first.Image)
	if err != nil || !found {
		t.Fatalf("derived image: %v %v", found, err)
	}
	if len(derived.Layers) != len(base.Layers)+1 || derived.Labels[LabelKey] != first.Record.Key || derived.Labels[LabelSourceCommit] != commit || derived.Labels[LabelOutputs] != OutputsPersistent {
		t.Fatalf("derived image layers %d/%d labels %v", len(derived.Layers), len(base.Layers), derived.Labels)
	}
	if first.Record.AddedBytes <= 0 || first.Shadowed {
		t.Fatalf("added %d shadowed %v", first.Record.AddedBytes, first.Shadowed)
	}
	envOK := false
	for _, e := range derived.Env {
		envOK = envOK || e == "PREPARED_DIR="+dir
	}
	if !envOK {
		t.Fatalf("policy env not baked: %v", derived.Env)
	}
	check := `test "$(cat "$PREPARED_DIR/deps.txt")" = "dep v1" && test "$(cat "$PREPARED_DIR/uid")" = 65534 && test "$(cat "$PREPARED_DIR/pwd")" = /swiftproof/work && test "$(cat "$PREPARED_DIR/home")" = /swiftproof/home && test "$(cat "$PREPARED_DIR/net")" = lo && ! grep -q SWIFTPROOF_HOST_SECRET "$PREPARED_DIR/env" && test ! -e /tmp/scratch`
	if c := runCheck(t, first.Image, check); c.Status != "PASS" {
		t.Fatalf("check on the prepared image: %s %q", c.Status, c.Output)
	}
	if c := runCheck(t, image, `test -f "`+dir+`/deps.txt"`); c.Status != "FAIL" {
		t.Fatalf("check on the base image: %s %q", c.Status, c.Output)
	}

	started := len(docker.names)
	o.ArtifactDir = t.TempDir()
	again := Run(context.Background(), o)
	if again.Record.Status != model.PrepareReused || again.Image != first.Image || len(docker.names) != started {
		t.Fatalf("second run %+v (containers %d -> %d)", again.Record, started, len(docker.names))
	}

	root := o
	root.Spec = config.Prepare{Command: []string{"sh", "-c", `mkdir -p /opt/swiftproof-prepare-test && grep CapEff /proc/self/status > /opt/swiftproof-prepare-test/caps && id -u > /opt/swiftproof-prepare-test/uid`}, Inputs: []string{"deps.txt"}, User: "root"}
	root.ArtifactDir = t.TempDir()
	rooted := Run(context.Background(), root)
	defer removeImages(t, rooted.Image)
	if rooted.Record.Status != model.PrepareBuilt || rooted.Record.User != "root" {
		t.Fatalf("root run %+v", rooted.Record)
	}
	if c := runCheck(t, rooted.Image, `grep -q "00000000000000db$" /opt/swiftproof-prepare-test/caps && test "$(cat /opt/swiftproof-prepare-test/uid)" = 0`); c.Status != "PASS" {
		caps := runCheck(t, rooted.Image, `cat /opt/swiftproof-prepare-test/caps`)
		t.Fatalf("root capabilities: %s %q", c.Status, caps.Output)
	}
	assertNoContainers(t, docker.names)
}

// A prepare command that exits non-zero commits nothing, retains its log and
// leaves no container; one that only writes where checks never look is
// built with the shadowed marker.
func TestDockerPrepareFailureAndShadowedOutputs(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	repo, commit := dockerRepo(t)
	docker := &countingDocker{}
	o := Options{
		Spec:      config.Prepare{Command: []string{"sh", "-c", "echo password=abc123secret; echo failing >&2; exit 7"}, Inputs: []string{"deps.txt"}},
		BaseImage: image, SourceCommit: commit, Repo: repo, MemoryMB: 256, CPUs: 1, MaxOutputBytes: 4096,
		ArtifactDir: t.TempDir(), ToolVersion: "docker-test", Docker: docker, Progress: &bytes.Buffer{},
	}
	failed := Run(context.Background(), o)
	if failed.Record.Status != model.PrepareFailed || failed.Ready() || !strings.Contains(failed.Record.Reason, "exited 7") || !failed.Started {
		t.Fatalf("failed run %+v", failed.Record)
	}
	if len(failed.Artifacts) != 1 {
		t.Fatalf("artifacts %+v", failed.Artifacts)
	}
	log, err := os.ReadFile(failed.Artifacts[0].Path)
	if err != nil || !strings.Contains(string(log), "failing") || strings.Contains(string(log), "abc123secret") {
		t.Fatalf("log %q %v", log, err)
	}
	o.Spec.Command = []string{"sh", "-c", "echo cache > $HOME/cache && echo tmp > /tmp/only-here"}
	o.ArtifactDir = t.TempDir()
	shadowed := Run(context.Background(), o)
	defer removeImages(t, shadowed.Image)
	if shadowed.Record.Status != model.PrepareBuilt || !shadowed.Shadowed {
		t.Fatalf("shadowed run %+v shadowed %v", shadowed.Record, shadowed.Shadowed)
	}
	o.Spec.Command = []string{"sh", "-c", "true"}
	o.ArtifactDir = t.TempDir()
	nothing := Run(context.Background(), o)
	if nothing.Record.Status != model.PrepareFailed || !strings.Contains(nothing.Record.Reason, "changed no file") {
		t.Fatalf("no-change run %+v", nothing.Record)
	}
	assertNoContainers(t, docker.names)
}
