package prepare

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
)

// Paths inside the prepare container. The exported inputs are the only bind
// mount; the working and home directories are created by SwiftProof before
// the container starts (scaffold) and belong to the container user.
const (
	InputsDir = "/swiftproof/inputs"
	WorkDir   = "/swiftproof/work"
	HomeDir   = "/swiftproof/home"
)

// Fixed container limits (the other limits come from the sandbox policy).
const (
	pidsLimit       = 256
	containerPrefix = "swiftproof-prepare-"
	diffLimit       = 64 << 10
	listLimit       = 64 << 10
	commitLimit     = 4 << 10
	createLimit     = 4 << 10
)

// rootCapabilities is the fixed capability set a user:root prepare container
// gets back after --cap-drop=ALL: what package managers need to install files
// owned by other users, and nothing that reaches beyond the container.
var rootCapabilities = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID"}

// wrapperScript copies the read-only inputs into the working directory and
// replaces itself with the policy's command. A copy failure exits 125, which
// is reported like a container that could not start the command.
const wrapperScript = `cp -R ` + InputsDir + `/. ` + WorkDir + `/ || { echo "swiftproof: could not copy the prepare inputs into ` + WorkDir + `" >&2; exit 125; }; exec "$@"`

// Docker runs, inspects and commits the prepare container. DockerCLI is the
// implementation; tests substitute fakes. Image inspection, listing and
// removal go through a dockerutil.Runner instead (see Options.Runner).
type Docker interface {
	// Run creates the container from args (which start with "create"), copies
	// the scaffold archive into its root with the container user's ownership,
	// then starts it attached, writing its output to log. started reports
	// whether the start was issued; exitCode is the container's exit status.
	// A cancelled ctx returns ctx's error; the caller removes the container.
	Run(ctx context.Context, name string, args []string, scaffold []byte, log io.Writer) (started bool, exitCode int, err error)
	// Changed returns `docker diff NAME`, at most 64 KiB; truncated reports a cut.
	Changed(ctx context.Context, name string) (diff []byte, truncated bool, err error)
	// Commit commits the container as tag with the given --change
	// instructions and returns the new image ID (^sha256:[0-9a-f]{64}$).
	Commit(ctx context.Context, name, tag string, changes []string) (string, error)
	// Remove force-removes the container; an absent container is not an error.
	Remove(ctx context.Context, name string) error
}

// DockerCLI runs the docker CLI with exec.CommandContext: no shell, no -i/-t,
// standard input only for the scaffold archive of `docker cp`.
type DockerCLI struct{}

// createArgs is the container profile (contract §2 F8, refinement R2):
// --pull=never, network none unless the build needs it, all capabilities
// dropped (the fixed set back for root only), no-new-privileges, the sandbox
// memory/CPU limits, a PID limit, one read-only bind mount of the exported
// inputs, HOME in the scaffold, exactly the policy env, no host environment,
// no Docker socket and no other mount. The root filesystem stays writable: the
// container's changes are the product.
func createArgs(name, inputsDir, imageID string, spec config.Prepare, network bool, memoryMB, cpus int) []string {
	mode := "none"
	if network {
		mode = "bridge"
	}
	root := spec.EffectiveUser() == config.PrepareUserRoot
	args := []string{"create", "--pull=never", "--name", name, "--network", mode, "--cap-drop=ALL"}
	if root {
		for _, c := range rootCapabilities {
			args = append(args, "--cap-add="+c)
		}
	}
	user := "--user=65534:65534"
	if root {
		user = "--user=0:0"
	}
	args = append(args, "--security-opt=no-new-privileges",
		fmt.Sprintf("--memory=%dm", memoryMB), fmt.Sprintf("--memory-swap=%dm", memoryMB), fmt.Sprintf("--cpus=%d", cpus),
		"--pids-limit="+strconv.Itoa(pidsLimit), "--ulimit=nofile=1024:1024", "--log-driver=none", "--no-healthcheck", user,
		"--mount", "type=bind,src="+inputsDir+",dst="+InputsDir+",readonly",
		"--workdir="+WorkDir, "--env=HOME="+HomeDir)
	for _, e := range sortedEnv(spec.Env) {
		args = append(args, "--env="+e[0]+"="+e[1])
	}
	args = append(args, "--entrypoint=/bin/sh", imageID, "-c", wrapperScript, "swiftproof")
	return append(args, spec.Command...)
}

// sortedEnv returns the policy env as (name, value) pairs sorted by name.
func sortedEnv(env map[string]string) [][2]string {
	out := make([][2]string, 0, len(env))
	for k, v := range env {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// scaffold is the archive `docker cp -a` extracts at the container root before
// it starts: the working and home directories, owned by the container user
// (-a gives extracted entries the container user's ownership), inside a
// root-owned /swiftproof that Docker creates for the inputs mount point.
func scaffold() []byte {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, d := range []struct {
		name string
		mode int64
	}{{strings.TrimPrefix(WorkDir, "/") + "/", 0755}, {strings.TrimPrefix(HomeDir, "/") + "/", 0700}} {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d.name, Mode: d.mode, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// Run implements Docker.
func (DockerCLI) Run(ctx context.Context, name string, args []string, archive []byte, log io.Writer) (bool, int, error) {
	if len(args) == 0 || args[0] != "create" {
		return false, -1, errors.New("prepare container arguments must start with create")
	}
	if _, stderr, err := docker(ctx, nil, createLimit, args...); err != nil {
		return false, -1, cliError("docker create", err, stderr)
	}
	if _, stderr, err := docker(ctx, bytes.NewReader(archive), createLimit, "cp", "-a", "-", name+":/"); err != nil {
		return false, -1, cliError("docker cp", err, stderr)
	}
	cmd := exec.CommandContext(ctx, "docker", "start", "--attach", name)
	cmd.Stdout, cmd.Stderr = log, log
	err := cmd.Run()
	if ctx.Err() != nil {
		return true, -1, ctx.Err()
	}
	if err == nil {
		return true, 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return true, exit.ExitCode(), nil
	}
	return true, -1, err
}

// Changed implements Docker.
func (DockerCLI) Changed(ctx context.Context, name string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "diff", name)
	out := &capWriter{limit: diffLimit}
	stderr := &capWriter{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		return nil, false, cliError("docker diff", err, stderr.bytes())
	}
	return out.bytes(), out.cut, nil
}

// Commit implements Docker.
func (DockerCLI) Commit(ctx context.Context, name, tag string, changes []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := []string{"commit"}
	for _, c := range changes {
		args = append(args, "--change", c)
	}
	stdout, stderr, err := docker(ctx, nil, commitLimit, append(args, name, tag)...)
	if err != nil {
		return "", cliError("docker commit", err, stderr)
	}
	id := strings.TrimSpace(string(stdout))
	if !dockerutil.ValidImageID(id) {
		return "", errors.New("docker commit returned no valid image ID")
	}
	return id, nil
}

// Remove implements Docker.
func (DockerCLI) Remove(ctx context.Context, name string) error {
	_, stderr, err := docker(ctx, nil, commitLimit, "rm", "-f", name)
	if err != nil && !strings.Contains(strings.ToLower(string(stderr)), "no such container") {
		return cliError("docker rm", err, stderr)
	}
	return nil
}

// docker runs the CLI with an optional standard input, standard output bounded
// by limit (an overflow is an error) and 8 KiB of standard error.
func docker(ctx context.Context, stdin io.Reader, limit int, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdout := &capWriter{limit: limit}
	stderr := &capWriter{limit: 8 << 10}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, stderr.bytes(), ctx.Err()
	}
	if err == nil && stdout.cut {
		err = dockerutil.ErrOutputLimit
	}
	return stdout.bytes(), stderr.bytes(), err
}

func cliError(what string, err error, stderr []byte) error {
	detail := strings.TrimSpace(strings.ToValidUTF8(string(stderr), "?"))
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %s", what, err, detail)
}

// capWriter keeps at most limit bytes and drops the rest, recording the cut.
type capWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - w.buf.Len()
	if len(p) > remaining {
		w.cut = true
		if remaining > 0 {
			w.buf.Write(p[:remaining])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// listImages returns the distinct IDs of the images tagged tag that carry the
// key label. A prepared image is found through its tag, so an older image
// whose tag moved to a rebuild is no longer listed.
func listImages(ctx context.Context, run dockerutil.Runner, tag, key string) ([]string, error) {
	if run == nil {
		run = dockerutil.DefaultRunner
	}
	stdout, stderr, err := run(ctx, []string{"image", "ls", "--no-trunc", "--format", "{{.ID}}", "--filter", "reference=" + tag, "--filter", "label=" + LabelKey + "=" + key}, listLimit)
	if err != nil {
		return nil, cliError("docker image ls", err, stderr)
	}
	seen := map[string]bool{}
	var ids []string
	for _, l := range strings.Split(string(stdout), "\n") {
		id := strings.TrimSpace(l)
		if id == "" {
			continue
		}
		if !dockerutil.ValidImageID(id) {
			return nil, errors.New("docker image ls returned an invalid image ID")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// layerSize returns the size Docker reports for the newest layer of image id,
// the one `docker commit` added on top of the base image: the first entry of
// `docker image history`. `docker image inspect` Size cannot be used for the
// difference: on the containerd image store it is the compressed content size,
// which for a derived image can be smaller than for its base (observed with a
// 20 KiB layer: base 296,880,031 bytes, derived 296,873,728 bytes).
func layerSize(ctx context.Context, run dockerutil.Runner, id string) (int64, error) {
	if run == nil {
		run = dockerutil.DefaultRunner
	}
	if !dockerutil.ValidImageID(id) {
		return 0, errors.New("image history needs an image ID")
	}
	stdout, stderr, err := run(ctx, []string{"image", "history", "--no-trunc", "--human=false", "--format", "{{.Size}}", id}, listLimit)
	if err != nil {
		return 0, cliError("docker image history", err, stderr)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(stdout)), "\n")
	size, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || size < 0 {
		return 0, errors.New("docker image history returned no layer size")
	}
	return size, nil
}

// removeImage removes a derived image this run committed and rejected.
func removeImage(ctx context.Context, run dockerutil.Runner, id string) error {
	if run == nil {
		run = dockerutil.DefaultRunner
	}
	if !dockerutil.ValidImageID(id) {
		return errors.New("refusing to remove an image that is not identified by its ID")
	}
	_, stderr, err := run(ctx, []string{"image", "rm", id}, commitLimit)
	if err != nil {
		return cliError("docker image rm", err, stderr)
	}
	return nil
}
