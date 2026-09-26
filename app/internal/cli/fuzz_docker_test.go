package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const e2eCalcBase = `package calc

import (
	"fmt"
	"os"
)

// Cents is an amount of money.
type Cents int64

// Boundary reports the sandbox user and whether the source mount refused a write.
func Boundary(n int) string {
	err := os.WriteFile("/source/swiftproof-boundary", []byte("x"), 0o644)
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

const e2eCalcCandidate = `package calc

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Cents is an amount of money.
type Cents int64

// The unsuffixed harness names: the harness draws a random suffix, so these
// collide with nothing.
var swiftproofFuzz, swiftproofFuzz_run = 1, 2

// Boundary reports the sandbox user and whether the source mount refused a write.
func Boundary(n int) string {
	_ = n
	err := os.WriteFile("/source/swiftproof-boundary", []byte("x"), 0o644)
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

// fuzzCalcRepo is the F2 end-to-end fixture: main holds the baseline, the
// candidate branch rewrites the calc functions, breaks the build of package
// broken, declares the unsuffixed harness names in a package and a test file,
// and rewrites an exported TypeScript function.
func fuzzCalcRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/fuzzdemo\n\ngo 1.21\n")
	write(t, dir, "calc/calc.go", e2eCalcBase)
	write(t, dir, "broken/broken.go", "package broken\n\n// Double doubles n.\nfunc Double(n int) int {\n\treturn n * 2\n}\n")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "calc/calc.go", e2eCalcCandidate)
	write(t, dir, "calc/extra_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestSwiftProofFuzz(t *testing.T) {}\n")
	write(t, dir, "broken/broken.go", "package broken\n\n// Double doubles n.\nfunc Double(n int) int {\n\treturn n*2 + undefinedName\n}\n")
	write(t, dir, "web/price.ts", "export function price(n: number): number {\n  return n + 1;\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

func fuzzCheck(r model.Report, id string) *model.Check {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

// The F2 scenario through the whole CLI with real Docker: exit 2 with --ci
// (never 1, never 4); Percent and Discount diverge, Join and Boundary do not,
// Stamp, Halt and the function whose candidate package does not build are
// inconclusive with FAIL (never ERROR) candidate checks; the TypeScript
// function is listed as not fuzzed; the report re-renders identically; a
// tampered hash makes Discount inconclusive on re-render; the checkout stays
// clean, and nothing of the run remains in its private temporary directory or
// as a container.
func TestDockerFuzzReviewEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	dir := fuzzCalcRepo(t)
	head := git(t, dir, "rev-parse", "HEAD")
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.TimeoutSeconds, cfg.Sandbox.MaxRuntimeSeconds = 900, 3600
	cfg.Commands = map[string][]string{"test": {"go", "test", "./..."}, "generated_test": {"go", "test", "{package}"}}
	cfg.Fuzz = &config.Fuzz{MaxRuntimeSeconds: 3000}
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
	private := filepath.Join(scratch, "tmp")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(name, private)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", out}, &stdout, &stderr, "fuzz-e2e")
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
			t.Fatalf("ERROR check %s %s:\n%s", c.ID, c.Kind, c.Output)
		}
		if strings.HasPrefix(c.Kind, "fuzz_") {
			fuzzChecks++
		}
	}
	if fuzzChecks != 6 {
		t.Fatalf("%d fuzz checks, want 6 (calc: a pair and a confirmation pair; broken: a pair)", fuzzChecks)
	}
	f := r.Fuzz
	if f == nil || f.Status != model.FuzzRan || len(f.Functions) != 7 {
		t.Fatalf("fuzz %+v", f)
	}
	outcomes := map[string]model.FuzzFunction{}
	for _, fn := range f.Functions {
		outcomes[fn.Symbol] = fn
		t.Logf("%s: %s %q", fn.Symbol, fn.Outcome, fn.Reason)
	}
	for symbol, want := range map[string]string{
		"calc.Boundary": model.FuzzNotDiverged, "calc.Percent": model.FuzzDiverged, "calc.Join": model.FuzzNotDiverged,
		"calc.Discount": model.FuzzDiverged, "calc.Stamp": model.FuzzInconclusive, "calc.Halt": model.FuzzInconclusive, "broken.Double": model.FuzzInconclusive,
	} {
		if outcomes[symbol].Outcome != want {
			t.Errorf("%s: %s, want %s", symbol, outcomes[symbol].Outcome, want)
		}
	}
	double := outcomes["broken.Double"]
	if c := fuzzCheck(r, double.Checks.Candidate); c == nil || c.Status != "FAIL" || double.Checks.BaseConfirm != "" || !strings.Contains(double.Reason, "the candidate run failed without recording an observation stream") {
		t.Fatalf("the candidate run of the broken package: %+v (%q)", c, double.Reason)
	}
	if c := fuzzCheck(r, outcomes["calc.Halt"].Checks.Candidate); c == nil || c.Status != "FAIL" {
		t.Fatalf("the candidate run of calc: %+v", c)
	}
	if c := outcomes["calc.Discount"].Counterexample; c == nil || *c != (model.FuzzCounterexample{Index: 11, Input: "Discount(Cents(1000))", Base: "calc.Cents(900)", Candidate: "calc.Cents(1000)"}) {
		t.Fatalf("Discount counterexample %+v", c)
	}
	if f.SkippedTotal != 1 || f.Skipped[0].Path != "web/price.ts" || f.Skipped[0].Symbol != "price" || f.Skipped[0].Reason != fuzz.ReasonScriptNotImplemented {
		t.Fatalf("skipped %+v", f.Skipped)
	}
	statuses := map[string]int{}
	for _, e := range r.Evidence {
		if e.Kind != model.EvidenceDifferentialFuzz {
			t.Fatalf("evidence %+v", e)
		}
		statuses[e.Status]++
	}
	if statuses[model.StatusDiverged] != 2 || statuses[model.StatusNotDiverged] != 2 || statuses[model.StatusUnverified] != 3 {
		t.Fatalf("evidence statuses %v", statuses)
	}
	if len(r.ReproducedIssues) != 0 || len(r.Divergences) != 2 {
		t.Fatalf("reproduced %d, divergences %d", len(r.ReproducedIssues), len(r.Divergences))
	}
	for _, d := range r.Divergences {
		if d.Kind != model.EvidenceDifferentialFuzz || len(d.CheckIDs) != 4 || d.Path != "calc/calc.go" || d.AnchorSource != "changed_function" {
			t.Fatalf("divergence %+v", d)
		}
	}
	md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Differential Fuzzing",
		"Smallest divergent input tried: Discount\\(Cents\\(1000\\)\\); baseline calc.Cents\\(900\\); candidate calc.Cents\\(1000\\).",
		"  - Discount\\(Cents\\(1000\\)\\): baseline calc.Cents\\(900\\); candidate calc.Cents\\(1000\\)",
		"- web/price.ts:1 price: TS/JS differential fuzzing is not implemented in this build",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if !strings.Contains(stdout.String(), "Differential fuzzing: 7 changed functions planned, 7 with recorded fuzz checks; 2 diverged, 2 not diverged, 3 inconclusive; 1 skipped.") {
		t.Errorf("stdout:\n%s", stdout.String())
	}
	// The checkout is unchanged, the run's private temporary directory is empty,
	// and no container that mounted it remains.
	if status := git(t, dir, "status", "--porcelain"); status != "" || git(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("checkout changed: %s", status)
	}
	if entries, err := os.ReadDir(private); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v %v", entries, err)
	}
	// t.TempDir names a directory after the test with a random suffix, and
	// every harness snapshot of the run lives below it.
	assertNoContainerMounts(t, filepath.Base(filepath.Dir(scratch)))
	// The report re-renders identically.
	jsonPath := filepath.Join(out, "confidence-report.json")
	if code := Run(context.Background(), []string{"report", "--input", jsonPath, "--out", rerender}, &stdout, &stderr, "fuzz-e2e"); code != 0 {
		t.Fatalf("report exited %d: %s", code, stderr.String())
	}
	for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
		a, _ := os.ReadFile(filepath.Join(out, name))
		b, _ := os.ReadFile(filepath.Join(rerender, name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s changed on re-render", name)
		}
	}
	// One tampered hash at Discount's counterexample: re-rendered, Discount is
	// inconclusive and its divergence is gone.
	discount := outcomes["calc.Discount"]
	candidate := fuzzCheck(r, discount.Checks.Candidate)
	s, err := fuzz.ParseResults(candidate.Results)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Functions {
		if s.Functions[i].Test == discount.TestName {
			s.Functions[i].Records[discount.Counterexample.Index].SHA256 = strings.Repeat("0", 64)
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
	if code := Run(context.Background(), []string{"report", "--input", tamperedPath, "--out", again}, &stdout, &stderr, "fuzz-e2e"); code != 0 {
		t.Fatalf("report exited %d: %s", code, stderr.String())
	}
	r2 := readReport(t, filepath.Join(again, "confidence-report.json"))
	for _, fn := range r2.Fuzz.Functions {
		if fn.Symbol == "calc.Discount" && (fn.Outcome != model.FuzzInconclusive || fn.Counterexample != nil) {
			t.Fatalf("tampered Discount %+v", fn)
		}
		if fn.Symbol == "calc.Percent" && fn.Outcome != model.FuzzDiverged {
			t.Fatalf("Percent %+v", fn)
		}
	}
	if len(r2.Divergences) != 1 || r2.ExitCode != 2 {
		t.Fatalf("divergences %d, exit %d", len(r2.Divergences), r2.ExitCode)
	}
}

// assertNoContainerMounts fails when a swiftproof- container whose mounts name
// marker (a directory of this test) still exists. Containers of other jobs on
// the host are ignored.
func assertNoContainerMounts(t *testing.T, marker string) {
	t.Helper()
	ids, err := exec.Command("docker", "ps", "-a", "--filter", "name=swiftproof-", "--format", "{{.ID}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range strings.Fields(string(ids)) {
		mounts, err := exec.Command("docker", "inspect", "--format", "{{range .Mounts}}{{.Source}}|{{end}}", id).Output()
		if err != nil {
			continue // removed in the meantime
		}
		if strings.Contains(string(mounts), marker) {
			t.Fatalf("container %s of this run remains (mounts %s)", id, mounts)
		}
	}
}
