package prepare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func testOptions(t *testing.T, f *fakeDocker, repo *gitrepo.Repository, commit string) Options {
	t.Helper()
	return Options{
		Spec:      config.Prepare{Command: []string{"go", "mod", "download"}, Inputs: []string{"go.mod", "go.sum"}},
		BaseImage: "golang:test", SourceCommit: commit, Repo: repo, MemoryMB: 512, CPUs: 1, MaxOutputBytes: 4096,
		ArtifactDir: t.TempDir(), ToolVersion: "test", Docker: f, Runner: f.run, Progress: &bytes.Buffer{},
	}
}

func assertFailed(t *testing.T, res Result, contains string) {
	t.Helper()
	if res.Ready() || res.Image != "" || res.Record.Status != model.PrepareFailed || !strings.Contains(res.Record.Reason, contains) || res.Record.ImageID != "" {
		t.Fatalf("result %+v, want failed with %q", res.Record, contains)
	}
	if res.Audit.Tool != "stage:prepare" || (res.Audit.Status != "ERROR" && res.Audit.Status != "TIMEOUT") {
		t.Fatalf("audit %+v", res.Audit)
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A first run builds, labels and commits; a second run with the same key and
// source commit reuses the image without starting any container.
func TestBuildThenReuseRunsNothing(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	res := Run(context.Background(), o)
	r := res.Record
	if !res.Ready() || r.Status != model.PrepareBuilt || r.ImageID != res.Image || r.Reason != "" || r.BaseImageID != fakeBaseID || r.SourceCommit != base {
		t.Fatalf("first run %+v", r)
	}
	if r.Key == "" || r.AddedBytes != 4096 || r.Network || r.User != "sandbox" || r.Note != model.PrepareNote || r.BaseImage != "golang:test" {
		t.Fatalf("record %+v", r)
	}
	if len(r.Inputs) != 2 || r.Inputs[0].Path != "go.mod" || r.Inputs[1] != (model.PreparedInput{Path: "go.sum", SHA256: sha("base-sum\n"), Size: 9}) {
		t.Fatalf("inputs %+v", r.Inputs)
	}
	img := f.images[res.Image]
	want := map[string]string{LabelSchema: Schema, LabelKey: r.Key, LabelSourceCommit: base, LabelBaseImageID: fakeBaseID, LabelToolVersion: "test", LabelOutputs: OutputsPersistent, LabelLogSHA256: r.LogSHA256}
	for k, v := range want {
		if img.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, img.Labels[k], v)
		}
	}
	if len(img.Tags) != 1 || img.Tags[0] != Tag(r.Key, base) {
		t.Fatalf("tags %v", img.Tags)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Kind != model.ArtifactPrepareOutput || res.Artifacts[0].SHA256 != r.LogSHA256 {
		t.Fatalf("artifacts %+v", res.Artifacts)
	}
	logData, err := os.ReadFile(res.Artifacts[0].Path)
	if err != nil || sha(string(logData)) != r.LogSHA256 {
		t.Fatalf("log artifact %v", err)
	}
	if res.Audit.Tool != "stage:prepare" || res.Audit.Status != "OK" || !strings.Contains(res.Audit.Arguments, `"status":"built"`) {
		t.Fatalf("audit %+v", res.Audit)
	}
	if len(f.live) != 0 {
		t.Fatalf("containers left: %v", f.live)
	}
	if _, err := os.Stat(f.mountDir); !os.IsNotExist(err) {
		t.Fatalf("inputs directory %s left behind: %v", f.mountDir, err)
	}
	if !res.Started || res.Shadowed {
		t.Fatalf("started %v shadowed %v", res.Started, res.Shadowed)
	}

	runs, commits := f.count("run "), f.count("commit ")
	o.ArtifactDir = t.TempDir()
	progress := &bytes.Buffer{}
	o.Progress = progress
	again := Run(context.Background(), o)
	if again.Record.Status != model.PrepareReused || again.Image != res.Image || again.Record.ImageID != res.Image || again.Record.Key != r.Key || again.Record.AddedBytes != 4096 {
		t.Fatalf("second run %+v", again.Record)
	}
	if f.count("run ") != runs || f.count("commit ") != commits || len(again.Artifacts) != 0 || again.Started || again.Record.LogSHA256 != "" {
		t.Fatal("reuse started a container or wrote a log")
	}
	if again.Audit.Status != "OK" || !strings.Contains(progress.String(), "Reusing prepared image") {
		t.Fatalf("audit %+v progress %q", again.Audit, progress.String())
	}
}

// Reuse needs exactly one tagged image whose labels equal the full key, this
// review's source commit and the base image ID, with the base layers plus one
// and the policy env; anything else rebuilds.
func TestUnverifiableImageIsRebuilt(t *testing.T) {
	for name, tamper := range map[string]func(f *fakeDocker, img *fakeImage){
		"different source commit": func(f *fakeDocker, img *fakeImage) { img.Labels[LabelSourceCommit] = strings.Repeat("e", 40) },
		"different key":           func(f *fakeDocker, img *fakeImage) { img.Labels[LabelKey] = strings.Repeat("f", 64) },
		"different base image": func(f *fakeDocker, img *fakeImage) {
			img.Labels[LabelBaseImageID] = "sha256:" + strings.Repeat("c", 64)
		},
		"other schema":           func(f *fakeDocker, img *fakeImage) { img.Labels[LabelSchema] = "swiftproof-prepare/v0" },
		"malformed outputs":      func(f *fakeDocker, img *fakeImage) { img.Labels[LabelOutputs] = "maybe" },
		"malformed tool version": func(f *fakeDocker, img *fakeImage) { img.Labels[LabelToolVersion] = "a b" },
		"malformed log hash":     func(f *fakeDocker, img *fakeImage) { img.Labels[LabelLogSHA256] = "x" },
		"one layer too many":     func(f *fakeDocker, img *fakeImage) { img.Layers = append(img.Layers, "sha256:x") },
		"not the base layers":    func(f *fakeDocker, img *fakeImage) { img.Layers[0] = "sha256:other" },
		"env missing":            func(f *fakeDocker, img *fakeImage) { img.Env = []string{"PATH=/usr/bin"} },
		"env value differs":      func(f *fakeDocker, img *fakeImage) { img.Env = append(img.Env, "GOMODCACHE=/elsewhere") },
		"over the size cap":      func(f *fakeDocker, img *fakeImage) { img.Size = 1000 + 2<<20 },
		"two images listed":      func(f *fakeDocker, img *fakeImage) { f.lsExtra = "sha256:" + strings.Repeat("9", 64) + "\n" },
	} {
		t.Run(name, func(t *testing.T) {
			repo, base, _ := repoFixture(t)
			f := newFakeDocker()
			o := testOptions(t, f, repo, base)
			o.Spec.Env = map[string]string{"GOMODCACHE": "/opt/gomod"}
			o.Spec.MaxAddedMB = 1
			first := Run(context.Background(), o)
			if first.Record.Status != model.PrepareBuilt {
				t.Fatalf("first run %+v", first.Record)
			}
			tamper(f, f.images[first.Image])
			o.ArtifactDir = t.TempDir()
			second := Run(context.Background(), o)
			if second.Record.Status != model.PrepareBuilt || f.count("run ") != 2 || second.Image == first.Image {
				t.Fatalf("tampered image reused: %+v (runs %d)", second.Record, f.count("run "))
			}
			if f.images[second.Image].Tags[0] != Tag(second.Record.Key, base) || len(f.images[first.Image].Tags) != 0 {
				t.Fatal("the rebuild did not take over the tag")
			}
		})
	}
}

// Without permission, a miss whose build needs network is not_permitted: no
// container runs, and SwiftProof never tries offline instead. An image that
// already exists for the key is still reused without the permission.
func TestNetworkPermission(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	o.Spec.Network = true
	res := Run(context.Background(), o)
	if res.Ready() || res.Record.Status != model.PrepareNotPermitted || res.Record.Network || f.count("run ") != 0 || res.Started {
		t.Fatalf("not permitted: %+v runs %d", res.Record, f.count("run "))
	}
	if !strings.Contains(res.Record.Reason, "--allow-prepare-network") || res.Audit.Status != "SKIPPED" || len(res.Artifacts) != 0 || res.Record.Key == "" || len(res.Record.Inputs) != 2 {
		t.Fatalf("record %+v audit %+v", res.Record, res.Audit)
	}
	o.AllowNetwork = true
	built := Run(context.Background(), o)
	if built.Record.Status != model.PrepareBuilt || !built.Record.Network || !hasArgs(f.args, "--network", "bridge") {
		t.Fatalf("network build %+v args %q", built.Record, f.args)
	}
	o.AllowNetwork = false
	reused := Run(context.Background(), o)
	if reused.Record.Status != model.PrepareReused || !reused.Record.Network || reused.Image != built.Image || f.count("run ") != 1 {
		t.Fatalf("reuse without permission %+v", reused.Record)
	}
}

func hasArgs(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestOfflineBuildNeedsNoPermission(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	res := Run(context.Background(), testOptions(t, f, repo, base))
	if res.Record.Status != model.PrepareBuilt || !hasArgs(f.args, "--network", "none") || res.Record.Network {
		t.Fatalf("offline build %+v args %q", res.Record, f.args)
	}
}

// Inputs come from the source commit (the base commit) only: never from the
// head commit, never a head-only file.
func TestInputsComeFromTheSourceCommitOnly(t *testing.T) {
	repo, base, head := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	o.Spec.Inputs = []string{"go.mod", "go.sum", "*.lock"}
	res := Run(context.Background(), o)
	if res.Record.Status != model.PrepareBuilt {
		t.Fatalf("%+v", res.Record)
	}
	if len(f.inputs) != 2 || f.inputs["go.sum"] != "base-sum\n" || f.inputs["go.mod"] == "" {
		t.Fatalf("container saw %v", f.inputs)
	}
	f2 := newFakeDocker()
	o2 := testOptions(t, f2, repo, head)
	o2.Spec.Inputs = o.Spec.Inputs
	other := Run(context.Background(), o2)
	if f2.inputs["go.sum"] != "head-sum\n" || f2.inputs["extra.lock"] != "head-only\n" || other.Record.Key == res.Record.Key {
		t.Fatalf("the export does not follow its source commit: %v", f2.inputs)
	}
}

func TestFailedCommandNeverCommits(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.runExit = 3
	f.runOutput = "fetching\npassword=hunter2\nnpm ERR! 404\n"
	res := Run(context.Background(), testOptions(t, f, repo, base))
	assertFailed(t, res, "the prepare command exited 3")
	if f.count("commit ") != 0 || f.count("remove ") != 1 || len(f.live) != 0 || !res.Started {
		t.Fatalf("calls %q", f.calls)
	}
	if len(res.Artifacts) != 1 || res.Record.LogSHA256 != res.Artifacts[0].SHA256 {
		t.Fatalf("log not retained: %+v", res.Artifacts)
	}
	data, err := os.ReadFile(res.Artifacts[0].Path)
	if err != nil || strings.Contains(string(data), "hunter2") || !strings.Contains(string(data), "npm ERR! 404") || sha(string(data)) != res.Record.LogSHA256 {
		t.Fatalf("log %q %v", data, err)
	}
	for _, code := range []int{125, 126, 127} {
		f := newFakeDocker()
		f.runExit = code
		assertFailed(t, Run(context.Background(), testOptions(t, f, repo, base)), "could not start the command")
	}
	f = newFakeDocker()
	f.notStarted, f.runErr = true, errors.New("docker create: exit status 125: bad mount")
	res = Run(context.Background(), testOptions(t, f, repo, base))
	assertFailed(t, res, "Docker could not run the prepare container")
	if res.Started || f.count("remove ") != 1 {
		t.Fatalf("started %v calls %q", res.Started, f.calls)
	}
}

func TestLogIsBoundedAndMarked(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.runOutput = strings.Repeat("x", 10000)
	o := testOptions(t, f, repo, base)
	o.MaxOutputBytes = 1000
	res := Run(context.Background(), o)
	data, _ := os.ReadFile(res.Artifacts[0].Path)
	if len(data) > 1100 || !strings.Contains(string(data), "prepare output truncated at 1000 bytes") {
		t.Fatalf("log of %d bytes: %q", len(data), data[len(data)-60:])
	}
}

func TestTimeoutAndDeadlineRemoveTheContainer(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.block = true
	o := testOptions(t, f, repo, base)
	o.Spec.TimeoutSeconds = 1
	res := Run(context.Background(), o)
	assertFailed(t, res, "prepare.timeout_seconds (1 s)")
	if res.Audit.Status != "TIMEOUT" || f.count("remove ") != 1 || len(f.live) != 0 || f.count("commit ") != 0 || len(res.Artifacts) != 1 {
		t.Fatalf("audit %+v calls %q", res.Audit, f.calls)
	}
	// The overall deadline (the parent context) expires while the command runs.
	f = newFakeDocker()
	f.block = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onRun = cancel
	res = Run(ctx, testOptions(t, f, repo, base))
	assertFailed(t, res, "overall --deadline")
	if f.count("remove ") != 1 {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestNoFilesystemChangeFails(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.diff = "A /swiftproof\nA /swiftproof/inputs\nA /swiftproof/work\nA /swiftproof/work/go.mod\nA /swiftproof/work/go.sum\nA /swiftproof/home\n"
	res := Run(context.Background(), testOptions(t, f, repo, base))
	assertFailed(t, res, "changed no file")
	if f.count("commit ") != 0 || f.count("remove ") != 1 {
		t.Fatalf("calls %q", f.calls)
	}
}

// Outputs only under /workspace, /tmp or the prepare HOME are shadowed by the
// check mounts: the image is built, the result says so, and a reuse repeats it.
func TestShadowedOutputs(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.diff = "A /swiftproof\nA /swiftproof/home\nA /swiftproof/home/.npm\nC /tmp\nA /tmp/node_modules\n"
	o := testOptions(t, f, repo, base)
	res := Run(context.Background(), o)
	if res.Record.Status != model.PrepareBuilt || !res.Shadowed || f.images[res.Image].Labels[LabelOutputs] != OutputsShadowed {
		t.Fatalf("%+v shadowed %v", res.Record, res.Shadowed)
	}
	o.ArtifactDir = t.TempDir()
	again := Run(context.Background(), o)
	if again.Record.Status != model.PrepareReused || !again.Shadowed {
		t.Fatalf("reuse %+v shadowed %v", again.Record, again.Shadowed)
	}
}

func TestCommitAndVerificationFailures(t *testing.T) {
	repo, base, _ := repoFixture(t)
	for name, tc := range map[string]struct {
		setup func(f *fakeDocker)
		want  string
	}{
		"commit error":        {func(f *fakeDocker) { f.commitErr = errors.New("disk full") }, "could not commit"},
		"diff error":          {func(f *fakeDocker) { f.diffErr = errors.New("gone") }, "could not list the prepare container's changes"},
		"committed ID absent": {func(f *fakeDocker) { f.commitID = "sha256:" + strings.Repeat("0", 64) }, "could not inspect the committed image"},
		"two layers":          {func(f *fakeDocker) { f.extraLayer = true }, "did not pass the image checks: its layers are not"},
		"env not baked":       {func(f *fakeDocker) { f.dropEnv = true }, "does not set GOMODCACHE"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			tc.setup(f)
			o := testOptions(t, f, repo, base)
			o.Spec.Env = map[string]string{"GOMODCACHE": "/opt/gomod"}
			res := Run(context.Background(), o)
			assertFailed(t, res, tc.want)
			if len(f.live) != 0 {
				t.Fatal("container left behind")
			}
			for id, img := range f.images {
				if id != fakeBaseID && len(img.Tags) > 0 && name != "committed ID absent" {
					t.Fatalf("a rejected image stayed tagged: %v", img.Tags)
				}
			}
		})
	}
}

// The derived image may add at most max_added_mb to the base image; over the
// cap it is removed and the stage fails.
func TestSizeCap(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.added = 1<<20 + 1
	o := testOptions(t, f, repo, base)
	o.Spec.MaxAddedMB = 1
	res := Run(context.Background(), o)
	assertFailed(t, res, "over prepare.max_added_mb (1 MiB); the image was removed")
	if res.Record.AddedBytes != 1<<20+1 || len(f.images) != 1 || f.count("image rm ") != 1 {
		t.Fatalf("record %+v images %d calls %q", res.Record, len(f.images), f.calls)
	}
	f = newFakeDocker()
	f.added = 1 << 20
	if res := Run(context.Background(), func() Options { o := testOptions(t, f, repo, base); o.Spec.MaxAddedMB = 1; return o }()); res.Record.Status != model.PrepareBuilt {
		t.Fatalf("exactly at the cap: %+v", res.Record)
	}
}

func TestBaseImageMustBeLocal(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	o.BaseImage = "golang:absent"
	res := Run(context.Background(), o)
	assertFailed(t, res, "is not available locally; SwiftProof never pulls images")
	if f.count("run ") != 0 || res.Record.BaseImageID != "" || len(res.Record.Inputs) != 0 {
		t.Fatalf("calls %q", f.calls)
	}
	f = newFakeDocker()
	f.inspectErr = errors.New("exec: docker: not found")
	assertFailed(t, Run(context.Background(), testOptions(t, f, repo, base)), "Docker could not inspect the sandbox image")
}

func TestInputRefusals(t *testing.T) {
	repo, base, _ := repoFixture(t)
	for name, tc := range map[string]struct {
		inputs []string
		want   string
	}{
		"credential file": {[]string{"go.mod", ".npmrc"}, "credential-bearing paths, which are never exported: .npmrc"},
		"credential glob": {[]string{".npm*"}, "credential-bearing"},
		"no match":        {[]string{"package.json"}, "prepare.inputs matched no file at the base commit"},
	} {
		f := newFakeDocker()
		o := testOptions(t, f, repo, base)
		o.Spec.Inputs = tc.inputs
		res := Run(context.Background(), o)
		assertFailed(t, res, tc.want)
		if f.count("run ") != 0 || f.count("image ls") != 0 {
			t.Fatalf("%s: calls %q", name, f.calls)
		}
	}
	f := newFakeDocker()
	o := testOptions(t, f, repo, "main")
	assertFailed(t, Run(context.Background(), o), "not a resolved commit identifier")
	if len(f.calls) != 0 {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestRecordRedactsAndSanitizes(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	o.Spec.Command = []string{"sh", "-c", "npm ci --password=hunter2"}
	o.ToolVersion = "v1 2;rm"
	res := Run(context.Background(), o)
	if strings.Contains(strings.Join(res.Record.Command, " "), "hunter2") {
		t.Fatalf("command not redacted: %q", res.Record.Command)
	}
	if f.images[res.Image].Labels[LabelToolVersion] != "unknown" {
		t.Fatalf("tool version label %q", f.images[res.Image].Labels[LabelToolVersion])
	}
	if f.args[len(f.args)-1] != "npm ci --password=hunter2" {
		t.Fatal("the container did not get the policy argv verbatim")
	}
}

func TestUnconfirmedRemovalFailsTheBuild(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	f.removeErr = errors.New("daemon timeout")
	res := Run(context.Background(), testOptions(t, f, repo, base))
	assertFailed(t, res, "could not confirm the removal of the prepare container swiftproof-prepare-")
	f = newFakeDocker()
	f.runExit, f.removeErr = 1, errors.New("daemon timeout")
	res = Run(context.Background(), testOptions(t, f, repo, base))
	assertFailed(t, res, "exited 1; see the prepare_output log; Docker could not confirm the removal")
}

// The scaffold archive and a container name of the reserved prefix reach
// Docker; the policy env is passed to the container.
func TestRunReceivesScaffoldAndEnv(t *testing.T) {
	repo, base, _ := repoFixture(t)
	f := newFakeDocker()
	o := testOptions(t, f, repo, base)
	o.Spec.Env = map[string]string{"NODE_PATH": "/swiftproof/work/node_modules"}
	o.Spec.User = "root"
	res := Run(context.Background(), o)
	if res.Record.Status != model.PrepareBuilt || res.Record.User != "root" {
		t.Fatalf("%+v", res.Record)
	}
	if !bytes.Equal(f.scaffold, scaffold()) || !hasArgs(f.args, "--name", strings.TrimPrefix(f.calls[len(f.calls)-1], "remove ")) {
		t.Fatalf("scaffold or name not passed: %q", f.args)
	}
	found := false
	for _, a := range f.args {
		found = found || a == "--env=NODE_PATH=/swiftproof/work/node_modules"
	}
	if !found || !strings.HasPrefix(strings.TrimPrefix(f.calls[len(f.calls)-1], "remove "), "swiftproof-prepare-") {
		t.Fatalf("args %q calls %q", f.args, f.calls)
	}
}
