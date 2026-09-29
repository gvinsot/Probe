package harness_test

// TS/JS differential fuzzing (F2c) with real Docker: selection, rendering and
// comparison from the fuzz package, run through Harness.RunObserved and
// Harness.AddFuzzEvidence with the sandbox isolation profile and a real
// Vitest or Jest run. Gated on PROBE_TEST_TS_IMAGE (a preloaded image
// with node, vitest, jest and ts-jest, for example probe-ts-test:local).

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

const tsCartBase = `export function discount(total: number, percent: number): number {
  return total - Math.floor((total * percent) / 100);
}

export function label(name: string): string {
  return name.trim();
}

export async function lookup(id: string): Promise<string> {
  if (id === "") {
    throw new Error("empty id");
  }
  return id.toUpperCase();
}

export function sorted(xs: number[]): number[] {
  return [...xs].sort((a, b) => a - b);
}

export function stamp(s: string): string {
  return s;
}

export const spin = (n: number): number => {
  return n;
};
`

const tsCartCandidate = `export function discount(total: number, percent: number): number {
  return Math.floor((total * (100 - percent)) / 100);
}

export function label(name: string): string {
  return name.replace(/^\s+|\s+$/g, "");
}

export async function lookup(id: string): Promise<string> {
  if (id === "") {
    return "";
  }
  return id.toUpperCase();
}

export function sorted(xs: number[]): number[] {
  return xs.sort((a, b) => a - b);
}

export function stamp(s: string): string {
  return s + Math.random();
}

export const spin = (n: number): number => {
  if (n === 7) {
    for (;;) {}
  }
  return n;
};
`

// scriptHarnessRunner is the adapter cli uses for a TS/JS harness, restated
// for this test.
type scriptHarnessRunner struct{ h *harness.Harness }

func (r scriptHarnessRunner) Observe(ctx context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	base, candidate, err := r.h.RunObserved(ctx, harness.ObservedRun{
		Path: req.Harness.Path, Content: req.Harness.Content, TestNames: req.Harness.TestNames(),
		Confirm: req.Confirm, SaveSource: req.SaveSource, Deadline: req.Deadline, Normalize: req.Harness.Normalize,
		Runner: req.Harness.EvidenceRunner(), Started: fuzz.StartedTests,
	})
	return fuzz.Side{Check: base.Check, OverflowSHA256: base.OverflowSHA256}, fuzz.Side{Check: candidate.Check, OverflowSHA256: candidate.OverflowSHA256}, err
}

func (r scriptHarnessRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	return r.h.AddFuzzEvidence(e)
}

// tsFuzzRun runs the cart fixture through selection, rendering, real runs and
// comparison with the given template and extra project files.
func tsFuzzRun(t *testing.T, image string, template []string, project map[string]string) (*harness.Harness, model.FuzzReport, func() []string, [2]string) {
	t.Helper()
	root := t.TempDir()
	base, candidate := filepath.Join(root, "base"), filepath.Join(root, "candidate")
	writeTree(t, base, project)
	writeTree(t, candidate, project)
	writeTree(t, base, map[string]string{"web/cart.ts": tsCartBase})
	writeTree(t, candidate, map[string]string{"web/cart.ts": tsCartCandidate})
	h, err := harness.New(harness.Options{
		CandidateDir: candidate, BaseDir: base, ArtifactDir: filepath.Join(root, "artifacts"), Image: image,
		Commands: map[string][]string{"generated_test": template},
		Timeout:  5 * time.Minute, MaxRuntime: 30 * time.Minute, MaxOutputBytes: 256 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	containers := harness.WatchContainers(h)
	t.Cleanup(func() {
		for _, name := range containers() {
			_ = exec.Command("docker", "rm", "-f", name).Run()
		}
	})
	snapBase, snapCandidate := harness.SnapshotDirs(h)
	limits := fuzz.Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: 500 * time.Millisecond, MaxRuntime: 25 * time.Minute}
	change := model.Change{Files: []model.ChangedFile{{Path: "web/cart.ts", Status: "M"}}}
	family := fuzz.ScriptFamily(template)
	plan, err := fuzz.SelectAll(context.Background(), snapBase, snapCandidate, change, nil, limits, family)
	if err != nil || plan.Targets() != 6 || len(plan.Skipped) != 0 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	started := time.Now()
	rep := fuzz.Run(context.Background(), scriptHarnessRunner{h}, plan, fuzz.Options{Limits: limits, ObservationsPath: harness.FuzzObservationsPath, PayloadLimit: harness.PayloadLimit(256 * 1024), ScriptFamily: family})
	t.Logf("fuzz stage: %s", time.Since(started).Round(time.Second))
	return h, rep, containers, [2]string{snapBase, snapCandidate}
}

// The cart fixture with real Vitest and real Jest (ts-jest) runs: discount,
// lookup (an async rejection against a resolution) and sorted (an array
// argument mutated after the call) diverge; label is not diverged; stamp is
// unstable and spin stops at its call timeout on the candidate, both
// inconclusive. Four checks pass; the snapshots keep no harness file and no
// container of the test remains.
func TestDockerTSFuzzObservedRun(t *testing.T) {
	image := os.Getenv("PROBE_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_TS_IMAGE to a preloaded image with node, vitest, jest and ts-jest (for example probe-ts-test:local)")
	}
	variants := map[string]struct {
		template []string
		project  map[string]string
	}{
		"vitest": {[]string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}, map[string]string{}},
		"jest": {[]string{"jest", "{file}", "--json", "--outputFile={results_out}"}, map[string]string{
			"package.json":   "{\"name\": \"cart\", \"private\": true}\n",
			"jest.config.js": "module.exports = { preset: \"ts-jest\", testEnvironment: \"node\" };\n",
			"tsconfig.json":  "{\"compilerOptions\": {\"target\": \"ES2020\", \"module\": \"commonjs\", \"strict\": true, \"esModuleInterop\": true}}\n",
		}},
	}
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			h, rep, containers, snaps := tsFuzzRun(t, image, v.template, v.project)
			checks := h.Checks()
			wantKinds := []string{model.CheckFuzzBase, model.CheckFuzzCandidate, model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm}
			if len(checks) != 4 {
				t.Fatalf("%d checks, want 4 (a first pair and one confirmation pair)", len(checks))
			}
			for i, c := range checks {
				t.Logf("%s %s %s exit %d, %d ms", c.ID, c.Kind, c.Status, c.ExitCode, c.DurationMS)
				if c.Kind != wantKinds[i] || c.Status != "PASS" || c.Results == "" || c.Replayed() {
					t.Fatalf("check %d: %s %s results %d bytes\n%s", i, c.Kind, c.Status, len(c.Results), c.Output)
				}
				if strings.Contains(strings.Join(c.Command, " "), "-run") || c.Command[0] != v.template[0] {
					t.Fatalf("command %q", c.Command)
				}
			}
			byName := map[string]model.FuzzFunction{}
			for _, f := range rep.Functions {
				byName[f.Symbol] = f
				t.Logf("%s: %s %q compared=%d diverged=%d unstable=%d", f.Symbol, f.Outcome, f.Reason, f.Compared, f.Diverged, f.Unstable)
			}
			if d := byName["discount"]; d.Outcome != model.FuzzDiverged || d.Counterexample == nil || d.Counterexample.Base == d.Counterexample.Candidate {
				t.Fatalf("discount = %+v %+v", d, d.Counterexample)
			}
			if l := byName["label"]; l.Outcome != model.FuzzNotDiverged || l.Compared != l.Inputs {
				t.Fatalf("label = %+v", l)
			}
			if l := byName["lookup"]; l.Outcome != model.FuzzDiverged || l.Diverged != 1 ||
				*l.Counterexample != (model.FuzzCounterexample{Index: 0, Input: `lookup("")`, Base: `rejected(error("Error", "empty id"))`, Candidate: `resolved("")`}) {
				t.Fatalf("lookup = %+v %+v", l, l.Counterexample)
			}
			if s := byName["sorted"]; s.Outcome != model.FuzzDiverged ||
				*s.Counterexample != (model.FuzzCounterexample{Index: 2, Input: "sorted([1, 0])", Base: "[0, 1]; arg 1 after call: [1, 0]", Candidate: "[0, 1]; arg 1 after call: [0, 1]"}) {
				t.Fatalf("sorted = %+v %+v", s, s.Counterexample)
			}
			if s := byName["stamp"]; s.Outcome != model.FuzzInconclusive || s.Reason != "all 64 inputs gave different observations on repeated evaluation" {
				t.Fatalf("stamp = %+v", s)
			}
			if s := byName["spin"]; s.Outcome != model.FuzzInconclusive || s.Reason != "the candidate stopped (timeout) while evaluating input 7: spin(7)" {
				t.Fatalf("spin = %+v", s)
			}
			statuses := []string{}
			for _, e := range h.Evidence() {
				if e.Kind != model.EvidenceDifferentialFuzz || e.Runner != harness.RunnerJest || e.CheckID != checks[1].ID || e.BaseCheckID != checks[0].ID || len(e.TestNames) != 1 {
					t.Fatalf("evidence %+v", e)
				}
				statuses = append(statuses, e.Status)
			}
			if got := strings.Join(statuses, ","); got != "DIVERGED,NOT_DIVERGED,DIVERGED,DIVERGED,UNVERIFIED,UNVERIFIED" {
				t.Fatalf("evidence statuses %s", got)
			}
			// Every recorded outcome is derived again from the four checks.
			recorded := fuzz.Checks{Base: &checks[0], Candidate: &checks[1], BaseConfirm: &checks[2], CandidateConfirm: &checks[3], Runner: harness.RunnerJest}
			for _, f := range rep.Functions {
				ev := fuzz.Evaluate(f.TestName, f.Inputs, recorded)
				if ev.Outcome != f.Outcome || ev.Reason != f.Reason || ev.Diverged != f.Diverged || ev.Compared != f.Compared {
					t.Fatalf("%s: derived again %+v", f.Symbol, ev)
				}
			}
			// Artifacts: the executed harness and each check's stream; hashes match.
			sources, streams := 0, 0
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
				case model.ArtifactFuzzObservations:
					streams++
				case model.ArtifactFuzzPayloadRejected:
					t.Fatalf("a stream was rejected: %s", b)
				}
			}
			if sources != 1 || streams != 4 {
				t.Fatalf("%d source and %d stream artifacts", sources, streams)
			}
			for _, dir := range snaps {
				if matches, _ := filepath.Glob(filepath.Join(dir, "web", "probe-fuzz-*")); len(matches) != 0 {
					t.Fatalf("harness left behind: %v", matches)
				}
			}
			assertContainersGone(t, containers(), 4)
		})
	}
}

// A Jest project without a TypeScript transform cannot load the harness: the
// baseline run fails without writing a stream, which is a harness setup
// failure on trusted code (ERROR, exit 4), while the candidate run keeps
// FAIL. Nothing is compared.
func TestDockerTSFuzzHarnessSetupFailure(t *testing.T) {
	image := os.Getenv("PROBE_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_TS_IMAGE to a preloaded image with node, vitest, jest and ts-jest (for example probe-ts-test:local)")
	}
	h, rep, containers, _ := tsFuzzRun(t, image, []string{"jest", "{file}", "--json", "--outputFile={results_out}"}, map[string]string{"package.json": "{\"name\": \"cart\", \"private\": true}\n"})
	checks := h.Checks()
	if len(checks) != 2 || checks[0].Status != "ERROR" || checks[1].Status != "FAIL" || !strings.Contains(checks[0].Output, "the TS/JS fuzz harness did not load or start on the baseline") {
		for _, c := range checks {
			t.Logf("%s %s %s\n%s", c.ID, c.Kind, c.Status, c.Output)
		}
		t.Fatalf("checks %d", len(checks))
	}
	for _, f := range rep.Functions {
		if f.Outcome != model.FuzzInconclusive || f.Reason != "the baseline run ended with status ERROR" {
			t.Fatalf("%s: %s %q", f.Symbol, f.Outcome, f.Reason)
		}
	}
	assertContainersGone(t, containers(), 2)
}

func assertContainersGone(t *testing.T, names []string, want int) {
	t.Helper()
	if len(names) != want {
		t.Fatalf("%d containers started, want %d", len(names), want)
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
}
