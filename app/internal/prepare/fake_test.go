package prepare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
)

// fakeImage is an image of the fake daemon.
type fakeImage struct {
	ID     string
	Size   int64
	Labels map[string]string
	Layers []string
	Env    []string
	Tags   []string
}

// fakeDocker is an in-memory Docker daemon for the prepare stage: it
// implements Docker, and run implements the dockerutil.Runner calls the stage
// makes (image inspect, image ls, image rm).
type fakeDocker struct {
	mu     sync.Mutex
	images map[string]*fakeImage
	calls  []string
	next   int

	// Behavior of the next Run.
	runExit    int
	runErr     error
	notStarted bool
	runOutput  string
	block      bool
	onRun      func() // called when the container starts
	onCommit   func() // called after a commit
	// Behavior of Changed, Commit and Remove.
	diff       string
	diffErr    error
	commitErr  error
	commitID   string // returned instead of the committed ID when set
	added      int64  // bytes a committed image adds to its base
	dropEnv    bool   // commit without the container's env
	extraLayer bool   // commit two layers
	removeErr  error
	inspectErr error
	lsExtra    string // appended to every image ls answer

	// What the last Run saw.
	args     []string
	scaffold []byte
	inputs   map[string]string
	mountDir string
	live     map[string]bool // containers created and not removed
}

const fakeBaseID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		images: map[string]*fakeImage{fakeBaseID: {ID: fakeBaseID, Size: 1000, Labels: map[string]string{}, Layers: []string{"sha256:l1", "sha256:l2"}, Env: []string{"PATH=/usr/bin"}, Tags: []string{"golang:test"}}},
		diff:   "A /probe\nA /probe/inputs\nA /probe/work\nA /probe/work/go.mod\nA /probe/home\nC /go\nA /go/pkg\n",
		added:  4096,
		live:   map[string]bool{},
	}
}

func (f *fakeDocker) count(prefix string) int {
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

func (f *fakeDocker) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeDocker) lookup(ref string) *fakeImage {
	if img, ok := f.images[ref]; ok {
		return img
	}
	for _, img := range f.images {
		for _, tag := range img.Tags {
			if tag == ref {
				return img
			}
		}
	}
	return nil
}

func (f *fakeDocker) run(ctx context.Context, args []string, limit int) ([]byte, []byte, error) {
	f.record(strings.Join(args, " "))
	if err := ctx.Err(); err != nil { // as the real CLI runner
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	exit1 := errors.New("exit status 1")
	switch {
	case len(args) == 5 && args[0] == "image" && args[1] == "inspect":
		if f.inspectErr != nil {
			return nil, []byte("Cannot connect to the Docker daemon"), f.inspectErr
		}
		img := f.lookup(args[4])
		if img == nil {
			return nil, []byte("Error response from daemon: No such image: " + args[4]), exit1
		}
		b, err := json.Marshal(map[string]any{
			"Id": img.ID, "Os": "linux", "Architecture": "amd64", "Size": img.Size, "RepoTags": img.Tags,
			"Config": map[string]any{"Labels": img.Labels, "Env": img.Env}, "RootFS": map[string]any{"Layers": img.Layers},
		})
		return b, nil, err
	case len(args) > 2 && args[0] == "image" && args[1] == "ls":
		var reference, label string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--filter" {
				if v, ok := strings.CutPrefix(args[i+1], "reference="); ok {
					reference = v
				}
				if v, ok := strings.CutPrefix(args[i+1], "label="); ok {
					label = v
				}
			}
		}
		k, v, _ := strings.Cut(label, "=")
		var out strings.Builder
		for _, img := range f.images {
			tagged := false
			for _, tag := range img.Tags {
				tagged = tagged || tag == reference
			}
			if tagged && img.Labels[k] == v {
				out.WriteString(img.ID + "\n")
			}
		}
		out.WriteString(f.lsExtra)
		return []byte(out.String()), nil, nil
	case len(args) == 7 && args[0] == "image" && args[1] == "history":
		img := f.images[args[6]]
		if img == nil {
			return nil, []byte("No such image"), exit1
		}
		base := f.images[fakeBaseID]
		// Newest first: the committed layer, then the base layers.
		return []byte(fmt.Sprintf("%d\n%d\n0\n", img.Size-base.Size, base.Size)), nil, nil
	case len(args) == 3 && args[0] == "image" && args[1] == "rm":
		if _, ok := f.images[args[2]]; !ok {
			return nil, []byte("No such image"), exit1
		}
		delete(f.images, args[2])
		return nil, nil, nil
	}
	return nil, []byte("unexpected docker call"), fmt.Errorf("fake docker: unexpected %q", args)
}

func (f *fakeDocker) Run(ctx context.Context, name string, args []string, scaffold []byte, log io.Writer) (bool, int, error) {
	f.record("run " + name)
	f.mu.Lock()
	f.args, f.scaffold, f.live[name] = append([]string(nil), args...), scaffold, true
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			src := strings.TrimPrefix(strings.Split(args[i+1], ",")[1], "src=")
			f.mountDir = src
			f.inputs = map[string]string{}
			_ = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					b, _ := os.ReadFile(p)
					rel, _ := filepath.Rel(src, p)
					f.inputs[filepath.ToSlash(rel)] = string(b)
				}
				return nil
			})
		}
	}
	notStarted, block, output, exit, runErr, onRun := f.notStarted, f.block, f.runOutput, f.runExit, f.runErr, f.onRun
	f.mu.Unlock()
	if notStarted {
		return false, -1, runErr
	}
	if onRun != nil {
		onRun()
	}
	io.WriteString(log, output)
	if block {
		<-ctx.Done()
		return true, -1, ctx.Err()
	}
	return true, exit, runErr
}

func (f *fakeDocker) Changed(ctx context.Context, name string, line func(string, bool)) error {
	f.record("changed " + name)
	if f.diffErr != nil {
		return f.diffErr
	}
	return scanLines(strings.NewReader(f.diff), diffLineLimit, line)
}

func (f *fakeDocker) Commit(ctx context.Context, name, tag string, changes []string) (string, error) {
	f.record("commit " + name + " " + tag)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitErr != nil {
		return "", f.commitErr
	}
	f.next++
	sum := sha256.Sum256([]byte(fmt.Sprintf("image-%d", f.next)))
	base := f.images[fakeBaseID]
	img := &fakeImage{ID: "sha256:" + hex.EncodeToString(sum[:]), Size: base.Size + f.added, Labels: map[string]string{}, Tags: []string{tag}}
	img.Layers = append(append([]string(nil), base.Layers...), fmt.Sprintf("sha256:layer-%d", f.next))
	if f.extraLayer {
		img.Layers = append(img.Layers, "sha256:extra")
	}
	img.Env = append([]string(nil), base.Env...)
	if !f.dropEnv {
		for _, a := range f.args {
			if e, ok := strings.CutPrefix(a, "--env="); ok {
				img.Env = append(img.Env, e)
			}
		}
	}
	for _, c := range changes {
		kv, ok := strings.CutPrefix(c, "LABEL ")
		if !ok {
			return "", fmt.Errorf("fake docker: unexpected change %q", c)
		}
		k, v, _ := strings.Cut(kv, "=")
		img.Labels[k] = v
	}
	for _, other := range f.images { // a tag names one image
		var kept []string
		for _, t := range other.Tags {
			if t != tag {
				kept = append(kept, t)
			}
		}
		other.Tags = kept
	}
	f.images[img.ID] = img
	if f.onCommit != nil {
		f.onCommit()
	}
	if f.commitID != "" {
		return f.commitID, nil
	}
	return img.ID, nil
}

func (f *fakeDocker) Remove(ctx context.Context, name string) error {
	f.record("remove " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.live, name)
	return nil
}

// repoFixture is a Git repository whose base commit has go.mod, go.sum and a
// credential file, and whose head commit changes go.sum and adds a lockfile.
func repoFixture(t *testing.T) (repo *gitrepo.Repository, base, head string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-b", "main")
	run("config", "core.autocrlf", "false")
	write("go.mod", "module example.test/x\n\ngo 1.23\n")
	write("go.sum", "base-sum\n")
	write(".npmrc", "//registry.example/:_authToken=abc\n")
	write("main.go", "package x\n")
	run("add", "-A")
	run("commit", "--no-gpg-sign", "-m", "base")
	base = run("rev-parse", "HEAD")
	write("go.sum", "head-sum\n")
	write("extra.lock", "head-only\n")
	run("add", "-A")
	run("commit", "--no-gpg-sign", "-m", "head")
	head = run("rev-parse", "HEAD")
	repo, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, base, head
}
