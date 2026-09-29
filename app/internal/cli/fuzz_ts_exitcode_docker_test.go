package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
)

const e2eHighExitBase = `export function run(args: string[]): number {
  return args.length;
}
`

const e2eHighExitCandidate = `export function run(args: string[]): number {
  if (args.length === 0) {
    process.exit(200);
  }
  return args.length + 0;
}
`

// §1.17: candidate code cannot make a fuzz check ERROR (exit 4) through its
// own exit code. Under Jest, a process.exit(200) in the candidate module ends
// the runner with exit code 200; the candidate capture script reports it as
// 124, so the check is FAIL, the function inconclusive, and the review exits
// 2 with --ci.
func TestDockerTSFuzzCandidateHighExitCodeIsNotError(t *testing.T) {
	image := os.Getenv("PROBE_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_TS_IMAGE to a preloaded image with node, jest and ts-jest (for example probe-ts-test:local)")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "package.json", "{\"name\": \"cli\", \"private\": true}\n")
	write(t, dir, "jest.config.js", "module.exports = { preset: \"ts-jest\", testEnvironment: \"node\" };\n")
	write(t, dir, "tsconfig.json", "{\"compilerOptions\": {\"target\": \"ES2020\", \"module\": \"commonjs\", \"strict\": true, \"esModuleInterop\": true}}\n")
	write(t, dir, "web/cli.ts", e2eHighExitBase)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "web/cli.ts", e2eHighExitCandidate)
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
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", out}, &stdout, &stderr, "fuzz-ts-high-exit")
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	candidateFail := false
	for _, c := range r.Checks {
		t.Logf("%s %s %s exit %d, %d ms", c.ID, c.Kind, c.Status, c.ExitCode, c.DurationMS)
		if c.Status == "ERROR" {
			t.Errorf("check %s %s is ERROR:\n%s", c.ID, c.Kind, c.Output)
		}
		if c.Kind == model.CheckFuzzCandidate && c.Status == "FAIL" && c.ExitCode == 124 {
			candidateFail = true
		}
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2: unverified %q\n%s", code, r.Unverified, stderr.String())
	}
	if !candidateFail {
		t.Fatal("no fuzz_candidate check FAILed with the reported exit code 124")
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 1 || f.Functions[0].Outcome != model.FuzzInconclusive {
		t.Fatalf("fuzz %+v", f)
	}
	t.Logf("run: %s %q", f.Functions[0].Outcome, f.Functions[0].Reason)
	if len(r.ReproducedIssues) != 0 || len(r.Divergences) != 0 {
		t.Fatalf("reproduced %d, divergences %d", len(r.ReproducedIssues), len(r.Divergences))
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
	assertNoContainerMounts(t, filepath.Base(filepath.Dir(scratch)))
}
