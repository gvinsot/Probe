package fuzz

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/coverage"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// dockerRunner runs harnesses in real containers with the isolation flags of
// the sandbox (no network, read-only root, unprivileged user, tmpfs
// workspace, read-only source mount) and the same framed payload channel.
// The product's Runner is the harness (F2b); this one exists so the core can
// be exercised end to end on its own.
type dockerRunner struct {
	t        *testing.T
	image    string
	base     string
	cand     string
	checks   []model.Check
	evidence []model.Evidence
	names    []string
}

const dockerObservations = "/tmp/swiftproof-observations.jsonl"

// dockerCaptureScript mirrors the harness's capture wrapper: the command's
// output goes to standard error, and the observation file comes back as one
// length-declared frame on standard output.
var dockerCaptureScript = `cp -R /source/. /workspace/ 1>&2 || exit 125; "$@" >&2; s=$?; if [ -s ` + dockerObservations +
	` ]; then set -- $(wc -c < ` + dockerObservations + `); printf '` + coverage.FrameHeader + `%s\n' "$1"; cat ` +
	dockerObservations + `; printf '%s\n' '` + strings.TrimSuffix(coverage.FrameFooter, "\n") + `'; fi; exit $s`

func (d *dockerRunner) Observe(ctx context.Context, req Request) (Side, Side, error) {
	h := req.Harness
	for _, root := range []string{d.base, d.cand} {
		p := filepath.Join(root, filepath.FromSlash(h.Path))
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return Side{}, Side{}, err
		}
		_, werr := f.WriteString(h.Content)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		defer os.Remove(p)
		if werr != nil {
			return Side{}, Side{}, werr
		}
	}
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(h.Path)))
	command := []string{"go", "test", "./" + dir, "-json", "-count=1", "-run", "^(" + strings.Join(h.TestNames(), "|") + ")$"}
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	timeout := time.Until(req.Deadline)
	if timeout <= 0 {
		return Side{}, Side{}, errors.New("deadline reached")
	}
	return d.run(ctx, baseKind, d.base, command, h, timeout), d.run(ctx, candidateKind, d.cand, command, h, timeout), nil
}

func (d *dockerRunner) run(ctx context.Context, kind, dir string, command []string, h Harness, timeout time.Duration) Side {
	var id [6]byte
	_, _ = rand.Read(id[:])
	name := "swiftproof-f2atest-" + hex.EncodeToString(id[:])
	d.names = append(d.names, name)
	args := []string{"run", "--rm", "--pull=never", "--name", name, "--network", "none", "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--pids-limit=128", "--memory=1024m", "--memory-swap=1024m", "--cpus=2", "--user=65534:65534",
		"--tmpfs", "/workspace:rw,exec,nosuid,nodev,mode=1777,size=1024m", "--tmpfs", "/tmp:rw,exec,nosuid,nodev,mode=1777,size=1024m",
		"--mount", "type=bind,src=" + dir + ",dst=/source,readonly", "--workdir=/workspace", "--env=HOME=/tmp", "--env=TMPDIR=/tmp",
		"--env=GOCACHE=/tmp/go-build", "--env=GOTOOLCHAIN=local", "--env=GOPROXY=off", "--env=GOSUMDB=off",
		"--entrypoint=/bin/sh", d.image, "-c", dockerCaptureScript, "swiftproof"}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "docker", append(args, command...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		code = -1
	}
	c := model.Check{ID: "check-" + strconv.Itoa(len(d.checks)+1), Kind: kind, Command: command, ExitCode: code,
		DurationMS: time.Since(start).Milliseconds(), Output: stderr.String()}
	switch {
	case code == 0:
		c.Status = "PASS"
	case code >= 1 && code <= 124:
		c.Status = "FAIL"
	default:
		c.Status = "ERROR"
	}
	if payload, err := coverage.DecodeFrame(stdout.Bytes(), false); err == nil {
		results, err := h.Normalize(payload)
		if err != nil {
			d.t.Fatalf("%s: stream rejected: %v\n%s", kind, err, payload)
		}
		c.Results = results
	}
	d.checks = append(d.checks, c)
	return Side{Check: c}
}

func (d *dockerRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	e.ID = "evidence-" + strconv.Itoa(len(d.evidence)+1)
	d.evidence = append(d.evidence, e)
	return e, nil
}

// TestDockerFuzzCalcFixture runs the F2 design's calc fixture through
// selection, rendering, four real container runs, normalization and
// comparison.
func TestDockerFuzzCalcFixture(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image to run Docker-gated tests")
	}
	base, candidate := calcTrees(t)
	plan, err := Select(base, candidate, modified("calc/calc.go"), nil, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	d := &dockerRunner{t: t, image: image, base: base, cand: candidate}
	defer func() {
		for _, name := range d.names {
			_ = exec.Command("docker", "rm", "-f", name).Run()
		}
	}()
	o := Options{Limits: defaultLimits(), ObservationsPath: dockerObservations, PayloadLimit: harness.PayloadLimit(32 * 1024)}
	o.Limits.MaxRuntime = 15 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rep := Run(ctx, d, plan, o)

	if len(d.checks) != 4 {
		t.Fatalf("%d checks, want 4 (a first pair and one confirmation pair)", len(d.checks))
	}
	for i, c := range d.checks {
		t.Logf("%s %s exit %d, %d ms", c.ID, c.Kind, c.ExitCode, c.DurationMS)
		wantStatus := "PASS"
		if i%2 == 1 {
			wantStatus = "FAIL" // Halt(7) ends the candidate process
		}
		if c.Status != wantStatus || c.Results == "" {
			t.Fatalf("%s: status %s, results %d bytes\n%s", c.Kind, c.Status, len(c.Results), c.Output)
		}
	}
	byName := map[string]model.FuzzFunction{}
	for _, f := range rep.Functions {
		byName[f.Symbol] = f
		t.Logf("%s: %s %q compared=%d diverged=%d unstable=%d unconfirmed=%d not_recorded=%d", f.Symbol, f.Outcome, f.Reason, f.Compared, f.Diverged, f.Unstable, f.Unconfirmed, f.NotRecorded)
	}
	percent := byName["calc.Percent"]
	if percent.Outcome != model.FuzzDiverged || *percent.Counterexample != (model.FuzzCounterexample{Index: 0, Input: "Percent(0, 0)", Base: "int(0)", Candidate: `panic(error("runtime error: integer divide by zero"))`}) {
		t.Fatalf("Percent = %+v %+v", percent, percent.Counterexample)
	}
	discount := byName["calc.Discount"]
	if discount.Outcome != model.FuzzDiverged || discount.Diverged != 1 ||
		*discount.Counterexample != (model.FuzzCounterexample{Index: 11, Input: "Discount(Cents(1000))", Base: "calc.Cents(900)", Candidate: "calc.Cents(1000)"}) {
		t.Fatalf("Discount = %+v %+v", discount, discount.Counterexample)
	}
	if join := byName["calc.Join"]; join.Outcome != model.FuzzNotDiverged || join.Compared != join.Inputs {
		t.Fatalf("Join = %+v", join)
	}
	if stamp := byName["calc.Stamp"]; stamp.Outcome != model.FuzzInconclusive || stamp.Reason != "all 64 inputs gave different observations on repeated evaluation" {
		t.Fatalf("Stamp = %+v", stamp)
	}
	if halt := byName["calc.Halt"]; halt.Outcome != model.FuzzInconclusive || halt.Reason != "the candidate process ended while evaluating input 5: Halt(7)" {
		t.Fatalf("Halt = %+v", halt)
	}
	statuses := []string{}
	for _, e := range d.evidence {
		statuses = append(statuses, e.Status)
	}
	if strings.Join(statuses, ",") != "DIVERGED,NOT_DIVERGED,DIVERGED,UNVERIFIED,UNVERIFIED" {
		t.Fatalf("evidence statuses %v", statuses)
	}
	// Every recorded outcome is re-derived from the four recorded checks.
	checks := Checks{Base: &d.checks[0], Candidate: &d.checks[1], BaseConfirm: &d.checks[2], CandidateConfirm: &d.checks[3]}
	for _, f := range rep.Functions {
		ev := Evaluate(f.TestName, f.Inputs, checks)
		if ev.Outcome != f.Outcome || ev.Reason != f.Reason || ev.Diverged != f.Diverged || ev.Compared != f.Compared {
			t.Fatalf("%s: re-derived %+v", f.Symbol, ev)
		}
	}
	// The harness file is gone from both snapshots and no container remains.
	for _, root := range []string{base, candidate} {
		matches, _ := filepath.Glob(filepath.Join(root, "calc", "swiftproof_fuzz_*"))
		if len(matches) != 0 {
			t.Fatalf("harness left behind: %v", matches)
		}
	}
	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=swiftproof-f2atest-", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range d.names {
		if strings.Contains(string(out), name) {
			t.Fatalf("container %s remains", name)
		}
	}
}
