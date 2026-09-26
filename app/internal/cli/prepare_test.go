package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/prepare"
)

const fakePrepareBaseID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fakePrepareDocker stands in for the Docker daemon of the prepare stage (the
// harness that may run afterwards still uses the real docker CLI; its image is
// the fake derived ID, so no repository code can run there).
type fakePrepareDocker struct {
	mu        sync.Mutex
	calls     []string
	baseRef   string // the only image present before the first build
	runExit   int
	images    map[string]map[string]string // ID -> labels
	tags      map[string]string            // tag -> ID
	env       map[string][]string          // ID -> env
	lastArgs  []string
	committed int
}

var _ prepare.Docker = (*fakePrepareDocker)(nil)

func newFakePrepareDocker(baseRef string) *fakePrepareDocker {
	return &fakePrepareDocker{baseRef: baseRef, images: map[string]map[string]string{}, tags: map[string]string{}, env: map[string][]string{}}
}

// install swaps the prepare stage's Docker client for f during the test.
func (f *fakePrepareDocker) install(t *testing.T) {
	t.Helper()
	docker, runner := prepareDocker, prepareRunner
	prepareDocker, prepareRunner = f, f.run
	t.Cleanup(func() { prepareDocker, prepareRunner = docker, runner })
}

func (f *fakePrepareDocker) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakePrepareDocker) run(ctx context.Context, args []string, limit int) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	missing := errors.New("exit status 1")
	switch {
	case len(args) == 5 && args[1] == "inspect":
		ref := args[4]
		if ref == f.baseRef || ref == fakePrepareBaseID {
			return []byte(`{"Id":"` + fakePrepareBaseID + `","Size":1000,"Config":{"Env":["PATH=/usr/bin"]},"RootFS":{"Layers":["sha256:l1"]}}`), nil, nil
		}
		if labels, ok := f.images[ref]; ok {
			b, _ := json.Marshal(map[string]any{"Id": ref, "Size": 1100, "Config": map[string]any{"Labels": labels, "Env": f.env[ref]}, "RootFS": map[string]any{"Layers": []string{"sha256:l1", "sha256:" + ref[7:]}}})
			return b, nil, nil
		}
		return nil, []byte("Error: No such image: " + ref), missing
	case len(args) > 2 && args[1] == "ls":
		var tag string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "reference="); ok {
				tag = v
			}
		}
		if id, ok := f.tags[tag]; ok {
			return []byte(id + "\n"), nil, nil
		}
		return nil, nil, nil
	case len(args) > 2 && args[1] == "history":
		return []byte("100\n0\n"), nil, nil
	case len(args) == 3 && args[1] == "rm":
		delete(f.images, args[2])
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected docker call %q", args)
}

func (f *fakePrepareDocker) Run(ctx context.Context, name string, args []string, scaffold []byte, log io.Writer) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "run "+name)
	f.lastArgs = append([]string(nil), args...)
	io.WriteString(log, "prepared\n")
	return true, f.runExit, nil
}

func (f *fakePrepareDocker) Changed(ctx context.Context, name string, line func(string, bool)) error {
	line("C /go", false)
	line("A /go/pkg", false)
	return nil
}

func (f *fakePrepareDocker) Commit(ctx context.Context, name, tag string, changes []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "commit "+tag)
	f.committed++
	sum := sha256.Sum256([]byte(fmt.Sprintf("derived-%d", f.committed)))
	id := "sha256:" + hex.EncodeToString(sum[:])
	labels := map[string]string{}
	for _, c := range changes {
		k, v, _ := strings.Cut(strings.TrimPrefix(c, "LABEL "), "=")
		labels[k] = v
	}
	env := []string{"PATH=/usr/bin"}
	for _, a := range f.lastArgs {
		if e, ok := strings.CutPrefix(a, "--env="); ok {
			env = append(env, e)
		}
	}
	f.images[id], f.tags[tag], f.env[id] = labels, id, env
	return id, nil
}

func (f *fakePrepareDocker) Remove(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove "+name)
	return nil
}

// prepareFixture is a repository whose candidate edits go.sum, a declared
// prepare input, and a source file.
func prepareFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/fixture\n\ngo 1.23\n")
	write(t, dir, "go.sum", "example.test/dep v1.0.0 h1:base=\n")
	write(t, dir, "auth.go", "package fixture\n\nfunc Allowed(user string) bool { return user == \"admin\" }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "go.sum", "example.test/dep v1.1.0 h1:head=\n")
	write(t, dir, "auth.go", "package fixture\n\nfunc Allowed(user string) bool { return true }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// preparePolicyFile writes a Go policy whose only v0.4 key is prepare.
func preparePolicyFile(t *testing.T, image string, spec config.Prepare) string {
	t.Helper()
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Prepare = &spec
	path := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, path, cfg)
	return path
}

var goPrepare = config.Prepare{Command: []string{"go", "mod", "download"}, Inputs: []string{"go.mod", "go.sum"}}

func hasPrepareSignal(r model.Report, path string) bool {
	for _, s := range r.Signals {
		if s.Kind == model.SignalPrepareInputChanged && s.Path == path && s.Severity == "medium" {
			return true
		}
	}
	return false
}

func unverifiedWith(r model.Report, fragment string) bool {
	for _, u := range r.Unverified {
		if strings.Contains(u, fragment) {
			return true
		}
	}
	return false
}

// lint never prepares and makes no Docker call, but the prepare_input_changed
// signal is deterministic and present.
func TestLintNeverPreparesButSignals(t *testing.T) {
	dir := prepareFixture(t)
	f := newFakePrepareDocker("golang:test")
	f.install(t)
	forbidExecution(t)
	policy := preparePolicyFile(t, "golang:test", goPrepare)
	code, r, members, _ := runReport(t, context.Background(), dir, "lint", "--config", policy)
	if code != 0 || len(f.calls) != 0 {
		t.Fatalf("exit %d, docker calls %q", code, f.calls)
	}
	if _, ok := members["prepare"]; ok || !hasPrepareSignal(r, "go.sum") || hasPrepareSignal(r, "auth.go") {
		t.Fatalf("prepare %v signals %+v", ok, r.Signals)
	}
}

// A preparation that fails runs nothing else, falls back to nothing and
// exits 4, whatever --ci says.
func TestPrepareFailureExecutesNothing(t *testing.T) {
	dir := prepareFixture(t)
	f := newFakePrepareDocker("golang:present")
	f.install(t)
	calls := countExecution(t)
	policy := preparePolicyFile(t, "golang:absent", goPrepare)
	for _, ci := range []bool{false, true} {
		args := []string{"review", "--config", policy, "--reviewer=false"}
		if ci {
			args = append(args, "--ci")
		}
		code, r, _, output := runReport(t, context.Background(), dir, args...)
		if code != 4 || len(r.Checks) != 0 || f.count("run ") != 0 {
			t.Fatalf("exit %d checks %v runs %d", code, checkKinds(r), f.count("run "))
		}
		if r.Prepare == nil || r.Prepare.Status != model.PrepareFailed || !strings.Contains(r.Prepare.Reason, "golang:absent is not available locally; SwiftProof never pulls images") {
			t.Fatalf("prepare %+v", r.Prepare)
		}
		if !unverifiedWith(r, "Dependency preparation failed: the sandbox image golang:absent is not available locally; SwiftProof never pulls images. The prepare command did not run; no repository code was executed and no check ran.") || unverifiedWith(r, "explicitly disabled") {
			t.Fatalf("unverified %q", r.Unverified)
		}
		if len(r.Audit) == 0 || r.Audit[0].Tool != "stage:prepare" || r.Audit[0].Status != "ERROR" {
			t.Fatalf("audit %+v", r.Audit)
		}
		if !strings.Contains(output, "Dependency preparation failed: the sandbox image golang:absent") {
			t.Fatalf("stderr %q", output)
		}
	}
	if *calls != 2 {
		t.Fatalf("execution hook called %d times, want once per review (prepare only)", *calls)
	}
}

// Network needs the policy and --allow-prepare-network; --no-network wins.
// A build needing network without the permission is not_permitted (exit 4)
// and starts no container.
func TestPrepareNetworkNeedsPolicyAndFlag(t *testing.T) {
	dir := prepareFixture(t)
	spec := goPrepare
	spec.Network = true
	policy := preparePolicyFile(t, "golang:test", spec)
	for _, extra := range [][]string{nil, {"--allow-prepare-network", "--no-network"}} {
		f := newFakePrepareDocker("golang:test")
		f.install(t)
		code, r, _, _ := runReport(t, context.Background(), dir, append([]string{"review", "--config", policy, "--reviewer=false"}, extra...)...)
		if code != 4 || f.count("run ") != 0 || len(r.Checks) != 0 {
			t.Fatalf("%v: exit %d runs %d checks %v", extra, code, f.count("run "), checkKinds(r))
		}
		if r.Prepare.Status != model.PrepareNotPermitted || r.Prepare.Network || !unverifiedWith(r, "Dependency preparation was not permitted") || r.Audit[0].Status != "SKIPPED" {
			t.Fatalf("%v: prepare %+v unverified %q", extra, r.Prepare, r.Unverified)
		}
	}
	f := newFakePrepareDocker("golang:test")
	f.install(t)
	_, r, _, _ := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--allow-prepare-network")
	if r.Prepare.Status != model.PrepareBuilt || !r.Prepare.Network || f.count("run ") != 1 {
		t.Fatalf("prepare %+v runs %d", r.Prepare, f.count("run "))
	}
	for i, a := range f.lastArgs {
		if a == "--network" && f.lastArgs[i+1] != "bridge" {
			t.Fatalf("network %s", f.lastArgs[i+1])
		}
	}
}

// A built image feeds the review: the prepare audit and log come first, the
// candidate's edit of a declared input adds the Unverified entry, and a second
// review reuses the image without a container.
func TestPrepareBuildThenReuseAcrossReviews(t *testing.T) {
	dir := prepareFixture(t)
	f := newFakePrepareDocker("golang:test")
	f.install(t)
	policy := preparePolicyFile(t, "golang:test", goPrepare)
	_, r, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	if r.Prepare == nil || r.Prepare.Status != model.PrepareBuilt || r.Prepare.ImageID == "" || r.Prepare.SourceCommit != r.Change.BaseCommit || len(r.Prepare.Inputs) != 2 {
		t.Fatalf("prepare %+v", r.Prepare)
	}
	if !strings.Contains(output, "Preparing dependencies in an isolated Docker container (network: disabled, user: sandbox)...") {
		t.Fatalf("stderr %q", output)
	}
	if len(r.Artifacts) == 0 || r.Artifacts[0].Kind != model.ArtifactPrepareOutput || !strings.HasPrefix(r.Artifacts[0].Path, "artifacts/prepare-") {
		t.Fatalf("artifacts %+v", r.Artifacts)
	}
	if len(r.Audit) < 2 || r.Audit[0].Tool != "stage:prepare" || r.Audit[0].Status != "OK" || !strings.HasPrefix(r.Audit[1].Tool, "run_") {
		t.Fatalf("audit order %+v", r.Audit)
	}
	if len(r.Checks) == 0 {
		t.Fatal("no check ran on the prepared image")
	}
	// The entry is neutral: it attributes no failure to the dependency change.
	if !unverifiedWith(r, "Candidate changes dependency-preparation inputs (go.sum)") || !unverifiedWith(r, "checks may fail or behave differently for that reason alone; SwiftProof attributes no check result to it.") || unverifiedWith(r, "failures they cause are expected") || !hasPrepareSignal(r, "go.sum") {
		t.Fatalf("unverified %q", r.Unverified)
	}
	_, again, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	if again.Prepare.Status != model.PrepareReused || again.Prepare.ImageID != r.Prepare.ImageID || f.count("run ") != 1 || !strings.Contains(output, "Reusing prepared image") {
		t.Fatalf("second review %+v runs %d", again.Prepare, f.count("run "))
	}
	for _, a := range again.Artifacts {
		if a.Kind == model.ArtifactPrepareOutput {
			t.Fatal("a reused image wrote a prepare log")
		}
	}
}

// Nothing to execute means nothing to prepare: prepare records not_run and
// Docker is never called.
func TestPrepareNotRunWithoutExecution(t *testing.T) {
	dir := prepareFixture(t)
	f := newFakePrepareDocker("golang:test")
	f.install(t)
	forbidExecution(t)
	policy := preparePolicyFile(t, "golang:test", goPrepare)
	code, r, _, _ := runReport(t, context.Background(), dir, "review", "--config", policy, "--checks=false", "--reviewer=false")
	if code != 0 || len(f.calls) != 0 || r.Prepare == nil || r.Prepare.Status != model.PrepareNotRun || r.Prepare.Reason != reasonExecutionDisabled {
		t.Fatalf("exit %d calls %q prepare %+v", code, f.calls, r.Prepare)
	}
	if unverifiedWith(r, "Candidate changes dependency-preparation inputs") {
		t.Fatal("a review that executed nothing claimed checks used prepared dependencies")
	}
}

// The candidate cannot add a prepare policy: the policy comes from the base
// ref, which has none.
func TestCandidateCannotAddPrepare(t *testing.T) {
	dir := prepareFixture(t)
	cfg := config.Default("go")
	cfg.Prepare = &goPrepare
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, config.Filename, string(data))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate policy")
	f := newFakePrepareDocker("golang:test")
	f.install(t)
	forbidExecution(t)
	_, r, members, _ := runReport(t, context.Background(), dir, "review", "--checks=false", "--reviewer=false")
	if _, ok := members["prepare"]; ok || len(f.calls) != 0 || hasPrepareSignal(r, "go.sum") {
		t.Fatalf("candidate policy took effect: prepare %v calls %q", ok, f.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, config.Filename)); err != nil {
		t.Fatal(err)
	}
}
