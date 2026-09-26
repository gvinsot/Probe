package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const e2eCartBase = `export function discount(total: number, percent: number): number {
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

const e2eCartCandidate = `export function discount(total: number, percent: number): number {
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

const e2eUtilBase = `/**
 * Joins the non-empty parts with commas.
 * @param {string[]} parts
 * @returns {string}
 */
export function joinParts(parts) {
  return parts.filter((p) => p !== "").join(",");
}
`

const e2eUtilCandidate = `/**
 * Joins the non-empty parts with commas.
 * @param {string[]} parts
 * @returns {string}
 */
export function joinParts(parts) {
  let out = "";
  for (const p of parts) {
    if (p === "") {
      continue;
    }
    if (out !== "") {
      out += ",";
    }
    out += p;
  }
  return out;
}
`

// fuzzCartRepo is the F2c end-to-end fixture: a TypeScript module whose
// candidate rewrites six exported functions, and a JavaScript module with
// JSDoc types whose candidate rewrites one.
func fuzzCartRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "web/cart.ts", e2eCartBase)
	write(t, dir, "web/util.js", e2eUtilBase)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "web/cart.ts", e2eCartCandidate)
	write(t, dir, "web/util.js", e2eUtilCandidate)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// The F2c scenario through the whole CLI with real Docker and a real Vitest
// template: exit 2 with --ci (never 1, never 4); discount, lookup and sorted
// diverge, label and joinParts do not, stamp (unstable) and spin (a call
// timeout on the candidate) are inconclusive; the evidence is jest_json; the
// report re-renders identically; a tampered hash at lookup's counterexample
// makes only lookup inconclusive on re-render; the checkout stays clean and
// no container of the run remains.
func TestDockerTSFuzzReviewEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_TS_IMAGE to a preloaded image with node and vitest (for example swiftproof-ts-test:local)")
	}
	dir := fuzzCartRepo(t)
	head := git(t, dir, "rev-parse", "HEAD")
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.TimeoutSeconds, cfg.Sandbox.MaxRuntimeSeconds = 600, 3600
	cfg.Commands = map[string][]string{"generated_test": {"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}}
	cfg.Fuzz = &config.Fuzz{MaxRuntimeSeconds: 3000, CallTimeoutMS: 500}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	policy := filepath.Join(scratch, "policy.json")
	if err := os.WriteFile(policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, rerender := filepath.Join(scratch, "report"), filepath.Join(scratch, "rerender")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", out}, &stdout, &stderr, "fuzz-ts-e2e")
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	for _, c := range r.Checks {
		t.Logf("%s %s %s exit %d, %d ms", c.ID, c.Kind, c.Status, c.ExitCode, c.DurationMS)
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2: unverified %q\n%s", code, r.Unverified, stderr.String())
	}
	fuzzChecks := 0
	for _, c := range r.Checks {
		if c.Status != "PASS" {
			t.Fatalf("check %s %s is %s:\n%s", c.ID, c.Kind, c.Status, c.Output)
		}
		if strings.HasPrefix(c.Kind, "fuzz_") {
			fuzzChecks++
		}
	}
	if fuzzChecks != 6 {
		t.Fatalf("%d fuzz checks, want 6 (cart: a pair and a confirmation pair; util: a pair)", fuzzChecks)
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 7 || f.SkippedTotal != 0 {
		t.Fatalf("fuzz %+v", f)
	}
	outcomes := map[string]model.FuzzFunction{}
	for _, fn := range f.Functions {
		outcomes[fn.Symbol] = fn
		t.Logf("%s: %s %q", fn.Symbol, fn.Outcome, fn.Reason)
	}
	for symbol, want := range map[string]string{
		"discount": model.FuzzDiverged, "label": model.FuzzNotDiverged, "lookup": model.FuzzDiverged, "sorted": model.FuzzDiverged,
		"stamp": model.FuzzInconclusive, "spin": model.FuzzInconclusive, "joinParts": model.FuzzNotDiverged,
	} {
		if outcomes[symbol].Outcome != want {
			t.Errorf("%s: %s, want %s", symbol, outcomes[symbol].Outcome, want)
		}
	}
	if c := outcomes["lookup"].Counterexample; c == nil || *c != (model.FuzzCounterexample{Index: 0, Input: `lookup("")`, Base: `rejected(error("Error", "empty id"))`, Candidate: `resolved("")`}) {
		t.Fatalf("lookup counterexample %+v", c)
	}
	if outcomes["joinParts"].Signature != "(parts) with JSDoc types (string[])" || outcomes["spin"].Reason != "the candidate stopped (timeout) while evaluating input 7: spin(7)" {
		t.Fatalf("joinParts %+v, spin %+v", outcomes["joinParts"], outcomes["spin"])
	}
	for _, e := range r.Evidence {
		if e.Kind != model.EvidenceDifferentialFuzz || e.Runner != harness.RunnerJest {
			t.Fatalf("evidence %+v", e)
		}
	}
	if len(r.ReproducedIssues) != 0 || len(r.Divergences) != 3 {
		t.Fatalf("reproduced %d, divergences %d", len(r.ReproducedIssues), len(r.Divergences))
	}
	for _, d := range r.Divergences {
		if d.Kind != model.EvidenceDifferentialFuzz || len(d.CheckIDs) != 4 || d.Path != "web/cart.ts" || d.AnchorSource != "changed_function" || !strings.HasPrefix(d.TestPath, "web/swiftproof-fuzz-") {
			t.Fatalf("divergence %+v", d)
		}
	}
	md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Differential Fuzzing",
		"Seeded inputs (swiftproof-fuzz/v1) were planned for 7 changed TS/JS functions, 7 of them with recorded fuzz checks",
		"Smallest divergent input tried: sorted\\(\\[1, 0\\]\\); baseline \\[0, 1\\]; arg 1 after call: \\[1, 0\\]; candidate \\[0, 1\\]; arg 1 after call: \\[0, 1\\].",
		"Smallest divergent input tried: lookup\\(&\\#34;&\\#34;\\); baseline rejected\\(error\\(&\\#34;Error&\\#34;, &\\#34;empty id&\\#34;\\)\\); candidate resolved\\(&\\#34;&\\#34;\\).",
		"- **not diverged** joinParts (web/util.js:6): 64 of 64 inputs compared",
	} {
		if !strings.Contains(string(md), want) {
			section := string(md)
			if i := strings.Index(section, "## Differential Fuzzing"); i >= 0 {
				section = section[i:]
			}
			t.Errorf("Markdown lacks %q:\n%s", want, section)
		}
	}
	for _, want := range []string{
		"Running differential fuzzing of 7 changed TS/JS functions in 2 modules in isolated Docker sandboxes...",
		"Differential fuzzing: 7 changed functions planned, 7 with recorded fuzz checks; 3 diverged, 2 not diverged, 2 inconclusive; 0 skipped.",
	} {
		if !strings.Contains(stdout.String()+stderr.String(), want) {
			t.Errorf("output lacks %q:\n%s%s", want, stdout.String(), stderr.String())
		}
	}
	// The checkout is unchanged and no container that mounted this test's
	// directories remains.
	if status := git(t, dir, "status", "--porcelain"); status != "" || git(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("checkout changed: %s", status)
	}
	assertNoContainerMounts(t, filepath.Base(filepath.Dir(scratch)))
	// The report re-renders identically.
	jsonPath := filepath.Join(out, "confidence-report.json")
	if code := Run(context.Background(), []string{"report", "--input", jsonPath, "--out", rerender}, &stdout, &stderr, "fuzz-ts-e2e"); code != 0 {
		t.Fatalf("report exited %d: %s", code, stderr.String())
	}
	for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
		a, _ := os.ReadFile(filepath.Join(out, name))
		b, _ := os.ReadFile(filepath.Join(rerender, name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s changed on re-render", name)
		}
	}
	// One tampered hash at lookup's counterexample: re-rendered, lookup is
	// inconclusive and its divergence is gone; the others keep theirs.
	lookup := outcomes["lookup"]
	candidate := fuzzCheck(r, lookup.Checks.Candidate)
	s, err := fuzz.ParseResults(candidate.Results)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Functions {
		if s.Functions[i].Test == lookup.TestName {
			s.Functions[i].Records[lookup.Counterexample.Index].SHA256 = strings.Repeat("0", 64)
		}
	}
	edited, err := json.MarshalIndent(s, "", "")
	if err != nil {
		t.Fatal(err)
	}
	candidate.Results = string(edited)
	tampered, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(scratch, "tampered.json")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(scratch, "tampered")
	if code := Run(context.Background(), []string{"report", "--input", tamperedPath, "--out", again}, &stdout, &stderr, "fuzz-ts-e2e"); code != 0 {
		t.Fatalf("report exited %d: %s", code, stderr.String())
	}
	r2 := readReport(t, filepath.Join(again, "confidence-report.json"))
	for _, fn := range r2.Fuzz.Functions {
		if fn.Symbol == "lookup" && (fn.Outcome != model.FuzzInconclusive || fn.Counterexample != nil) {
			t.Fatalf("tampered lookup %+v", fn)
		}
		if (fn.Symbol == "discount" || fn.Symbol == "sorted") && fn.Outcome != model.FuzzDiverged {
			t.Fatalf("%s %+v", fn.Symbol, fn)
		}
	}
	if len(r2.Divergences) != 2 || r2.ExitCode != 2 {
		t.Fatalf("divergences %d, exit %d", len(r2.Divergences), r2.ExitCode)
	}
}

const e2eExitBase = `export function run(args: string[]): number {
  if (args.length === 0) {
    process.exit(0);
  }
  return args.length;
}

export function twice(n: number): number {
  return n * 2;
}
`

const e2eExitCandidate = `export function run(args: string[]): number {
  if (args.length === 0) {
    process.exit(0);
  }
  return args.length + 0;
}

export function twice(n: number): number {
  return n + n;
}
`

// Under Jest, a baseline function that calls process.exit(0) while it is
// fuzzed ends the runner with exit code 0 and leaves its test without a done
// record. That is behavior of the baseline code, not a harness failure: no
// check is ERROR, both functions are inconclusive with a reason that names
// the input being evaluated, and the review exits 2 with --ci, never 4.
func TestDockerTSFuzzJestProcessExit(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_TS_IMAGE to a preloaded image with node, jest and ts-jest (for example swiftproof-ts-test:local)")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "package.json", "{\"name\": \"cli\", \"private\": true}\n")
	write(t, dir, "jest.config.js", "module.exports = { preset: \"ts-jest\", testEnvironment: \"node\" };\n")
	write(t, dir, "tsconfig.json", "{\"compilerOptions\": {\"target\": \"ES2020\", \"module\": \"commonjs\", \"strict\": true, \"esModuleInterop\": true}}\n")
	write(t, dir, "web/cli.ts", e2eExitBase)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "web/cli.ts", e2eExitCandidate)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.TimeoutSeconds, cfg.Sandbox.MaxRuntimeSeconds = 600, 3600
	cfg.Commands = map[string][]string{"generated_test": {"jest", "{file}", "--json", "--outputFile={results_out}"}}
	cfg.Fuzz = &config.Fuzz{MaxRuntimeSeconds: 1200, CallTimeoutMS: 500}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	policy := filepath.Join(scratch, "policy.json")
	if err := os.WriteFile(policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(scratch, "report")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", out}, &stdout, &stderr, "fuzz-ts-exit")
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	for _, c := range r.Checks {
		t.Logf("%s %s %s exit %d, %d ms", c.ID, c.Kind, c.Status, c.ExitCode, c.DurationMS)
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2: unverified %q\n%s", code, r.Unverified, stderr.String())
	}
	fuzzChecks := 0
	for _, c := range r.Checks {
		if c.Status == "ERROR" {
			t.Fatalf("check %s %s is ERROR:\n%s", c.ID, c.Kind, c.Output)
		}
		if strings.HasPrefix(c.Kind, "fuzz_") {
			fuzzChecks++
			if c.Status != "PASS" || c.ExitCode != 0 || c.Results == "" {
				t.Fatalf("check %s %s: %s exit %d, results %d bytes", c.ID, c.Kind, c.Status, c.ExitCode, len(c.Results))
			}
		}
	}
	if fuzzChecks != 2 {
		t.Fatalf("%d fuzz checks, want 2 (one pair, no difference to confirm)", fuzzChecks)
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 2 {
		t.Fatalf("fuzz %+v", f)
	}
	for _, fn := range f.Functions {
		t.Logf("%s: %s %q", fn.Symbol, fn.Outcome, fn.Reason)
		if fn.Outcome != model.FuzzInconclusive || fn.Checks == nil {
			t.Fatalf("%s: %+v", fn.Symbol, fn)
		}
	}
	if !strings.HasPrefix(f.Functions[0].Reason, "the baseline process ended while evaluating input ") || !strings.Contains(f.Functions[0].Reason, "run([])") ||
		f.Functions[1].Reason != "the baseline process ended before this function was evaluated" {
		t.Fatalf("reasons %q, %q", f.Functions[0].Reason, f.Functions[1].Reason)
	}
	if len(r.ReproducedIssues) != 0 || len(r.Divergences) != 0 {
		t.Fatalf("reproduced %d, divergences %d", len(r.ReproducedIssues), len(r.Divergences))
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
	assertNoContainerMounts(t, filepath.Base(filepath.Dir(scratch)))
}
