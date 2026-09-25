package prepare

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
)

const goldenImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// The prepare container profile, argument for argument (contract §2 F8
// delta 2, refinement R2).
func TestCreateArgsGolden(t *testing.T) {
	sandbox := config.Prepare{Command: []string{"go", "mod", "download"}, Env: map[string]string{"NODE_PATH": "/swiftproof/work/node_modules", "GOMODCACHE": "/opt/go mod"}}
	got := createArgs("swiftproof-prepare-abc", "/tmp/in", goldenImage, sandbox, false, 512, 2)
	want := []string{
		"create", "--pull=never", "--name", "swiftproof-prepare-abc", "--network", "none", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--memory=512m", "--memory-swap=512m", "--cpus=2", "--pids-limit=256",
		"--ulimit=nofile=1024:1024", "--log-driver=none", "--no-healthcheck", "--user=65534:65534",
		"--mount", "type=bind,src=/tmp/in,dst=/swiftproof/inputs,readonly", "--workdir=/swiftproof/work", "--env=HOME=/swiftproof/home",
		"--env=GOMODCACHE=/opt/go mod", "--env=NODE_PATH=/swiftproof/work/node_modules",
		"--entrypoint=/bin/sh", goldenImage, "-c", wrapperScript, "swiftproof", "go", "mod", "download",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sandbox profile:\n got %q\nwant %q", got, want)
	}
	root := config.Prepare{Command: []string{"npm", "ci"}, User: "root"}
	got = createArgs("swiftproof-prepare-abc", "/tmp/in", goldenImage, root, true, 1024, 4)
	want = []string{
		"create", "--pull=never", "--name", "swiftproof-prepare-abc", "--network", "bridge", "--cap-drop=ALL",
		"--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE", "--cap-add=FOWNER", "--cap-add=FSETID", "--cap-add=SETGID", "--cap-add=SETUID",
		"--security-opt=no-new-privileges", "--memory=1024m", "--memory-swap=1024m", "--cpus=4", "--pids-limit=256",
		"--ulimit=nofile=1024:1024", "--log-driver=none", "--no-healthcheck", "--user=0:0",
		"--mount", "type=bind,src=/tmp/in,dst=/swiftproof/inputs,readonly", "--workdir=/swiftproof/work", "--env=HOME=/swiftproof/home",
		"--entrypoint=/bin/sh", goldenImage, "-c", wrapperScript, "swiftproof", "npm", "ci",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("root profile:\n got %q\nwant %q", got, want)
	}
	if wrapperScript != `cp -R /swiftproof/inputs/. /swiftproof/work/ || { echo "swiftproof: could not copy the prepare inputs into /swiftproof/work" >&2; exit 125; }; exec "$@"` {
		t.Fatalf("wrapper script changed: %s", wrapperScript)
	}
}

// Whatever the policy says, the profile never gains a host channel, and the
// policy argv reaches the container verbatim after the fixed "swiftproof"
// name, never through a shell of SwiftProof's making.
func TestCreateArgsIsolation(t *testing.T) {
	hostile := []string{"sh", "-c", "curl http://169.254.169.254; rm -rf /", "$(id)", "--privileged", "-v", "/var/run/docker.sock:/s"}
	for _, spec := range []config.Prepare{
		{Command: hostile},
		{Command: hostile, User: "root", Network: true, Env: map[string]string{"X": "--volume=/:/host"}},
	} {
		args := createArgs("swiftproof-prepare-x", "/tmp/in", goldenImage, spec, spec.Network, 256, 1)
		split := -1
		for i, a := range args {
			if a == "swiftproof" && i > 0 && args[i-1] == wrapperScript {
				split = i
				break
			}
		}
		if split < 0 || strings.Join(args[split+1:], "\x00") != strings.Join(spec.Command, "\x00") {
			t.Fatalf("argv not verbatim after the script: %q", args)
		}
		mounts := 0
		for i, a := range args[:split] {
			switch {
			case a == "-i" || a == "-t" || a == "-it" || a == "--interactive" || a == "--tty" || a == "--rm" || a == "--read-only" || a == "--privileged":
				t.Fatalf("profile has %s: %q", a, args)
			case a == "-v" || strings.HasPrefix(a, "--volume") || strings.HasPrefix(a, "--tmpfs") || strings.HasPrefix(a, "--device") || strings.HasPrefix(a, "--env-file") || strings.HasPrefix(a, "--volumes-from"):
				t.Fatalf("profile has a host channel %s: %q", a, args)
			case strings.Contains(a, "docker.sock"):
				t.Fatalf("profile mentions the Docker socket: %q", args)
			case a == "--mount":
				mounts++
				if args[i+1] != "type=bind,src=/tmp/in,dst=/swiftproof/inputs,readonly" {
					t.Fatalf("unexpected mount %q", args[i+1])
				}
			case a == "--pull=always" || a == "--pull=missing":
				t.Fatalf("profile may pull: %q", args)
			}
		}
		if mounts != 1 {
			t.Fatalf("%d mounts, want exactly one", mounts)
		}
		if args[split-3] != goldenImage || args[split-4] != "--entrypoint=/bin/sh" {
			t.Fatalf("image argument is not the resolved ID: %q", args)
		}
	}
}

func TestScaffoldArchive(t *testing.T) {
	r := tar.NewReader(bytes.NewReader(scaffold()))
	var got []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeDir || h.Uid != 0 || h.Gid != 0 || h.Size != 0 {
			t.Fatalf("unexpected entry %+v", h)
		}
		got = append(got, fmt.Sprintf("%s %o", h.Name, h.Mode))
	}
	if strings.Join(got, ",") != "swiftproof/work/ 755,swiftproof/home/ 700" {
		t.Fatalf("scaffold entries %q", got)
	}
}

func TestClassifyChanges(t *testing.T) {
	inputs := []gitrepo.ExportedFile{{Path: "go.mod"}, {Path: "app/go.sum"}}
	own := "A /swiftproof\nA /swiftproof/inputs\nA /swiftproof/work\nA /swiftproof/work/go.mod\nA /swiftproof/work/app\nA /swiftproof/work/app/go.sum\nA /swiftproof/home\n"
	for _, tc := range []struct {
		name                 string
		diff                 string
		cut                  bool
		persistent, shadowed string
	}{
		{name: "nothing beyond SwiftProof's own paths", diff: own},
		{name: "empty"},
		{name: "go module cache", diff: own + "C /go\nA /go/pkg\nA /go/pkg/mod\n", persistent: "/go,/go/pkg,/go/pkg/mod"},
		{name: "work outputs are persistent", diff: own + "A /swiftproof/work/node_modules\n", persistent: "/swiftproof/work/node_modules"},
		{name: "shadowed", diff: own + "A /swiftproof/home/.cache\nC /tmp\nA /tmp/x\nA /workspace/y\n", shadowed: "/swiftproof/home/.cache,/tmp,/tmp/x,/workspace/y"},
		{name: "prefix is not a parent", diff: "A /tmpfoo\nA /workspacex\n", persistent: "/tmpfoo,/workspacex"},
		{name: "deletions count", diff: "D /usr/share/doc\n", persistent: "/usr/share/doc"},
		{name: "cut listing drops the partial line", diff: own + "A /opt/lib\nA /opt/li", cut: true, persistent: "/opt/lib"},
		{name: "CRLF", diff: "A /opt/x\r\n", persistent: "/opt/x"},
		{name: "space in path", diff: "A /opt/a b\n", persistent: "/opt/a b"},
		{name: "input mount content", diff: "A /swiftproof/inputs/go.mod\n"},
		{name: "a work file that is not an input", diff: "A /swiftproof/work/go.sum\n", persistent: "/swiftproof/work/go.sum"},
	} {
		c := classifyChanges([]byte(tc.diff), tc.cut, inputs)
		if strings.Join(c.persistent, ",") != tc.persistent || strings.Join(c.shadowed, ",") != tc.shadowed {
			t.Errorf("%s: persistent %q shadowed %q", tc.name, c.persistent, c.shadowed)
		}
	}
}

func TestLabelChangesSorted(t *testing.T) {
	got := labelChanges(map[string]string{LabelSourceCommit: "c", LabelKey: "k", LabelSchema: Schema})
	want := []string{"LABEL " + LabelKey + "=k", "LABEL " + LabelSchema + "=" + Schema, "LABEL " + LabelSourceCommit + "=c"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", got)
	}
	for _, v := range []string{"v0.4.0", "F8-e2e", "1.2.3+build.4"} {
		if toolVersionLabel(v) != v {
			t.Errorf("%q sanitized", v)
		}
	}
	for _, v := range []string{"", "v 1", "a;b", "a\nb", strings.Repeat("v", 65), "ü"} {
		if toolVersionLabel(v) != "unknown" {
			t.Errorf("%q kept", v)
		}
	}
}

type recordingRunner struct {
	args   [][]string
	stdout string
	stderr string
	err    error
}

func (r *recordingRunner) run(ctx context.Context, args []string, limit int) ([]byte, []byte, error) {
	r.args = append(r.args, append([]string(nil), args...))
	return []byte(r.stdout), []byte(r.stderr), r.err
}

func TestListImagesAndRemoveImage(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	r := &recordingRunner{stdout: id + "\n" + id + "\n\n" + other + "\n"}
	ids, err := listImages(context.Background(), r.run, "swiftproof-prepared:x-y", "k")
	if err != nil || strings.Join(ids, ",") != id+","+other {
		t.Fatalf("ids %v %v", ids, err)
	}
	if got := strings.Join(r.args[0], " "); got != "image ls --no-trunc --format {{.ID}} --filter reference=swiftproof-prepared:x-y --filter label=org.swiftproof.prepare.key=k" {
		t.Fatalf("list args %q", got)
	}
	for _, bad := range []string{"abc\n", "sha256:ABC\n", id + " extra\n"} {
		if _, err := listImages(context.Background(), (&recordingRunner{stdout: bad}).run, "t", "k"); err == nil {
			t.Errorf("accepted listing %q", bad)
		}
	}
	if _, err := listImages(context.Background(), (&recordingRunner{err: errors.New("exit status 1"), stderr: "daemon down"}).run, "t", "k"); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("list error %v", err)
	}
	r = &recordingRunner{}
	if err := removeImage(context.Background(), r.run, id); err != nil || strings.Join(r.args[0], " ") != "image rm "+id {
		t.Fatalf("remove %v %q", err, r.args)
	}
	if err := removeImage(context.Background(), r.run, "swiftproof-prepared:x"); err == nil {
		t.Fatal("removed an image by tag")
	}
	r = &recordingRunner{stdout: "20480\n4096\n0\n"}
	if size, err := layerSize(context.Background(), r.run, id); err != nil || size != 20480 || strings.Join(r.args[0], " ") != "image history --no-trunc --human=false --format {{.Size}} "+id {
		t.Fatalf("layer size %d %v %q", size, err, r.args)
	}
	for _, bad := range []string{"", "20kB\n", "-1\n"} {
		if _, err := layerSize(context.Background(), (&recordingRunner{stdout: bad}).run, id); err == nil {
			t.Errorf("accepted history %q", bad)
		}
	}
	if _, err := layerSize(context.Background(), r.run, "swiftproof-prepared:x"); err == nil {
		t.Fatal("history by tag")
	}
}
