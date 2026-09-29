package harness_test

// The differential fuzzing harness with real Docker: the fuzz package's
// selection, rendering and comparison, run through Harness.RunObserved and
// Harness.AddFuzzEvidence with the sandbox isolation profile. This is an
// external test package because the fuzz package imports harness.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/fuzz"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

// dockerCalcBase and dockerCalcCandidate are the F2 design's calc fixture
// plus Boundary, whose value shows the sandbox user and that the source mount
// refuses writes. Its body changes without changing its value.
const dockerCalcBase = `package calc

import (
	"fmt"
	"os"
)

// Cents is an amount of money.
type Cents int64

// Boundary reports the sandbox user and whether the source mount refused a write.
func Boundary(n int) string {
	err := os.WriteFile("/source/probe-boundary", []byte("x"), 0o644)
	return fmt.Sprintf("uid=%d source-write-refused=%t", os.Getuid(), err != nil)
}

// Percent returns part as a percentage of total.
func Percent(part, total int) int {
	if total == 0 {
		return 0
	}
	return part * 100 / total
}

// Join joins the non-empty parts with commas.
func Join(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += ","
		}
		out += p
	}
	return out
}

// Discount takes ten percent off from 1000 cents.
func Discount(c Cents) Cents {
	if c >= 1000 {
		return c * 9 / 10
	}
	return c
}

// Stamp returns its label.
func Stamp(label string) string {
	return label
}

// Halt returns n.
func Halt(n int) int {
	return n
}
`

const dockerCalcCandidate = `package calc

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Cents is an amount of money.
type Cents int64

// Boundary reports the sandbox user and whether the source mount refused a write.
func Boundary(n int) string {
	_ = n
	err := os.WriteFile("/source/probe-boundary", []byte("x"), 0o644)
	return fmt.Sprintf("uid=%d source-write-refused=%t", os.Getuid(), err != nil)
}

// Percent returns part as a percentage of total.
func Percent(part, total int) int {
	return part * 100 / total
}

// Join joins the non-empty parts with commas.
func Join(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(p)
	}
	return b.String()
}

// Discount takes ten percent off above 1000 cents.
func Discount(c Cents) Cents {
	if c > 1000 {
		return c * 9 / 10
	}
	return c
}

// Stamp returns its label and the current time.
func Stamp(label string) string {
	return label + time.Now().Format(time.RFC3339Nano)
}

// Halt returns n, and ends the process at 7.
func Halt(n int) int {
	if n == 7 {
		os.Exit(3)
	}
	return n
}
`

// harnessRunner is the adapter cli uses, restated for this test.
type harnessRunner struct{ h *harness.Harness }

func (r harnessRunner) Observe(ctx context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	base, candidate, err := r.h.RunObserved(ctx, harness.ObservedRun{
		Path: req.Harness.Path, Content: req.Harness.Content, TestNames: req.Harness.TestNames(),
		Confirm: req.Confirm, SaveSource: req.SaveSource, Deadline: req.Deadline, Normalize: req.Harness.Normalize,
	})
	return fuzz.Side{Check: base.Check, OverflowSHA256: base.OverflowSHA256}, fuzz.Side{Check: candidate.Check, OverflowSHA256: candidate.OverflowSHA256}, err
}

func (r harnessRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	return r.h.AddFuzzEvidence(e)
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDockerFuzzObservedRun(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded Go image to run Docker-gated tests")
	}
	root := t.TempDir()
	base, candidate := filepath.Join(root, "base"), filepath.Join(root, "candidate")
	gomod := "module example.test/fuzzdemo\n\ngo 1.21\n"
	writeTree(t, base, map[string]string{"go.mod": gomod, "calc/calc.go": dockerCalcBase})
	writeTree(t, candidate, map[string]string{"go.mod": gomod, "calc/calc.go": dockerCalcCandidate})
	h, err := harness.New(harness.Options{
		CandidateDir: candidate, BaseDir: base, ArtifactDir: filepath.Join(root, "artifacts"), Image: image,
		Commands: map[string][]string{"generated_test": {"go", "test", "{package}"}},
		Timeout:  10 * time.Minute, MaxRuntime: 40 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	containers := harness.WatchContainers(h)
	defer func() {
		for _, name := range containers() {
			_ = exec.Command("docker", "rm", "-f", name).Run()
		}
	}()
	snapBase, snapCandidate := harness.SnapshotDirs(h)
	limits := fuzz.Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: 35 * time.Minute}
	change := model.Change{Files: []model.ChangedFile{{Path: "calc/calc.go", Status: "M"}}}
	plan, err := fuzz.Select(snapBase, snapCandidate, change, nil, limits)
	if err != nil || plan.Targets() != 6 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	started := time.Now()
	rep := fuzz.Run(context.Background(), harnessRunner{h}, plan, fuzz.Options{Limits: limits, ObservationsPath: harness.FuzzObservationsPath, PayloadLimit: harness.PayloadLimit(32 * 1024)})
	t.Logf("fuzz stage: %s", time.Since(started).Round(time.Second))

	checks := h.Checks()
	wantKinds := []string{model.CheckFuzzBase, model.CheckFuzzCandidate, model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm}
	if len(checks) != 4 {
		t.Fatalf("%d checks, want 4 (a first pair and one confirmation pair)", len(checks))
	}
	for i, c := range checks {
		t.Logf("%s %s %s exit %d, %d ms", c.ID, c.Kind, c.Status, c.ExitCode, c.DurationMS)
		want := "PASS"
		if i%2 == 1 {
			want = "FAIL" // Halt(7) ends the candidate process: FAIL, never ERROR
		}
		if c.Kind != wantKinds[i] || c.Status != want || c.Results == "" || c.Replayed() {
			t.Fatalf("check %d: %s %s results %d bytes\n%s", i, c.Kind, c.Status, len(c.Results), c.Output)
		}
	}
	byName := map[string]model.FuzzFunction{}
	for _, f := range rep.Functions {
		byName[f.Symbol] = f
		t.Logf("%s: %s %q compared=%d diverged=%d unstable=%d", f.Symbol, f.Outcome, f.Reason, f.Compared, f.Diverged, f.Unstable)
	}
	boundary := byName["calc.Boundary"]
	if boundary.Outcome != model.FuzzNotDiverged || boundary.Compared != boundary.Inputs {
		t.Fatalf("Boundary = %+v", boundary)
	}
	baseStream, err := fuzz.ParseResults(checks[0].Results)
	if err != nil {
		t.Fatal(err)
	}
	fs, ok := baseStream.Function(boundary.TestName)
	if !ok || len(fs.Records) == 0 || fs.Records[0].Display != `string("uid=65534 source-write-refused=true")` {
		t.Fatalf("the sandbox boundary was not observed as expected: %+v", fs)
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
	for _, e := range h.Evidence() {
		if e.Kind != model.EvidenceDifferentialFuzz || e.Runner != harness.RunnerGo || e.CheckID != checks[1].ID || e.BaseCheckID != checks[0].ID || len(e.TestNames) != 1 {
			t.Fatalf("evidence %+v", e)
		}
		statuses = append(statuses, e.Status)
	}
	if got := strings.Join(statuses, ","); got != "NOT_DIVERGED,DIVERGED,NOT_DIVERGED,DIVERGED,UNVERIFIED,UNVERIFIED" {
		t.Fatalf("evidence statuses %s", got)
	}
	// Every recorded outcome is derived again from the four recorded checks.
	recorded := fuzz.Checks{Base: &checks[0], Candidate: &checks[1], BaseConfirm: &checks[2], CandidateConfirm: &checks[3]}
	for _, f := range rep.Functions {
		ev := fuzz.Evaluate(f.TestName, f.Inputs, recorded)
		if ev.Outcome != f.Outcome || ev.Reason != f.Reason || ev.Diverged != f.Diverged || ev.Compared != f.Compared {
			t.Fatalf("%s: derived again %+v", f.Symbol, ev)
		}
	}
	// Artifacts: the source is the executed harness, and each stream artifact
	// is its check's results; every hash matches its file.
	var sources, streams int
	results := map[string]bool{}
	for _, c := range checks {
		results[c.Results] = true
	}
	for _, a := range h.Artifacts() {
		b, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			t.Fatalf("%s does not match its hash", a.Path)
		}
		switch a.Kind {
		case model.ArtifactFuzzHarness:
			sources++
			if !strings.Contains(string(b), "func "+boundary.TestName+"(") {
				t.Fatalf("the source artifact is not the harness that ran")
			}
		case model.ArtifactFuzzObservations:
			streams++
			if !results[string(b)] {
				t.Fatalf("stream artifact %s is not a check's results", a.Path)
			}
		case model.ArtifactFuzzPayloadRejected:
			t.Fatalf("a stream was rejected: %s", b)
		}
	}
	if sources != 1 || streams != 4 {
		t.Fatalf("%d source and %d stream artifacts", sources, streams)
	}
	// No harness file stays in any snapshot, and no container of this test remains.
	for _, dir := range []string{base, candidate, snapBase, snapCandidate} {
		matches, _ := filepath.Glob(filepath.Join(dir, "calc", "probe_fuzz_*"))
		if len(matches) != 0 {
			t.Fatalf("harness left behind: %v", matches)
		}
	}
	names := containers()
	if len(names) != 4 {
		t.Fatalf("%d containers started, want 4", len(names))
	}
	for _, name := range names {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("container %s remains", name)
		}
	}
	if audit := h.Audit(); len(audit) != 2 || audit[0].Tool != "stage:run_fuzz" || audit[1].Tool != "stage:run_fuzz_confirm" {
		t.Fatalf("audit %+v", audit)
	}
}
