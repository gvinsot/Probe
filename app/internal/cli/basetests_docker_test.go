package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// baseTestsReview runs review --base-tests on a fixture and returns the exit
// code, the report, the output directory and the console output.
func baseTestsReview(t *testing.T, dir, policy string, extra ...string) (int, model.Report, string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report")
	args := append([]string{"review", "--repo", dir, "--base", "main", "--head", "candidate", "--config", policy, "--base-tests", "--reviewer=false", "--out", out}, extra...)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr, "integration")
	data, err := os.ReadFile(filepath.Join(out, "confidence-report.json"))
	if err != nil {
		t.Fatalf("exit %d, no report: %v\n%s", code, err, stderr.String())
	}
	var r model.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return code, r, out, stdout.String() + stderr.String()
}

func findCheck(r model.Report, id string) model.Check {
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	return model.Check{}
}

// TestDockerBaseTestsEndToEnd runs the baseline versions of changed tests in
// real sandboxes (scenarios A and B of the F3 design).
func TestDockerBaseTestsEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Commands = map[string][]string{"test": {"go", "test", "./..."}, "generated_test": {"go", "test", "{package}"}}
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)

	t.Run("A_test_edit_hides_a_behavior_change", func(t *testing.T) {
		dir := clampFixture(t)
		code, r, out, console := baseTestsReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2 (never 1): checks %+v\n%s", code, r.Checks, console)
		}
		if len(r.Checks) == 0 || r.Checks[0].Kind != "test" || r.Checks[0].Status != "PASS" {
			t.Fatalf("the edited suite should pass on the candidate: %+v", r.Checks)
		}
		b := r.BaseTests
		if b == nil || b.Status != model.BaseTestsRan || len(b.Tests) != 2 {
			t.Fatalf("base tests %+v", b)
		}
		upper, inside := b.Tests[0], b.Tests[1]
		if upper.Name != "TestClampUpper" || upper.Change != model.BaseTestModified || upper.Status != model.StatusFailsOnCandidate {
			t.Fatalf("TestClampUpper %+v", upper)
		}
		// TestClampInside compiles because extra_test.go is removed from the
		// hybrid tree. It passed inside the failed hybrid run, so it gets a run
		// pair of its own; on a loaded host the stage's 180 s sub-cap can run out
		// first, and the product limit stays: the test then records why.
		passes := 1
		switch {
		case inside.Name != "TestClampInside" || inside.Change != model.BaseTestRemoved:
			t.Fatalf("TestClampInside %+v", inside)
		case inside.Status == model.StatusPassesOnCandidate:
		case inside.Status == model.StatusUnverified && (strings.HasPrefix(inside.Reason, "the test passed inside a candidate-side run that failed as a whole") || strings.Contains(inside.Reason, "timed out")):
			t.Logf("the retry pair gave no result within the sub-cap: %s", inside.Reason)
			passes = 0
		default:
			t.Fatalf("TestClampInside %+v", inside)
		}
		for _, bt := range b.Tests {
			var e model.Evidence
			for _, candidate := range r.Evidence {
				if candidate.ID == bt.EvidenceID {
					e = candidate
				}
			}
			base, hybrid := findCheck(r, e.BaseCheckID), findCheck(r, e.CheckID)
			if e.Kind != model.EvidenceBaseTestDifferential || e.Runner != harness.RunnerGo || strings.Join(e.TestNames, ",") != bt.Name || base.Kind != model.CheckBaseTestBase || hybrid.Kind != model.CheckBaseTestHybrid || strings.Join(base.Command, " ") != strings.Join(hybrid.Command, " ") {
				t.Fatalf("%s: evidence %+v base %+v hybrid %+v", bt.Name, e, base, hybrid)
			}
			if base.Status != "PASS" {
				t.Fatalf("%s: baseline check %s", bt.Name, base.Status)
			}
		}
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("check %s is ERROR: %s", c.ID, c.Output)
			}
		}
		signals := map[string]bool{}
		for _, s := range r.Signals {
			if s.Path == "clamp_test.go" {
				signals[s.Kind] = true
			}
		}
		if !signals[model.SignalTestExpectationRelaxed] || !signals[model.SignalTestSkipAdded] {
			t.Fatalf("lexical signals %v", signals)
		}
		target := false
		for _, rt := range r.ReviewTargets {
			target = target || rt.Path == "clamp_test.go" && rt.Side == "old" && rt.Severity == "high" && rt.StartLine >= upper.Line && rt.EndLine <= upper.EndLine
		}
		if !target {
			t.Fatalf("no high old-side target on TestClampUpper: %+v", r.ReviewTargets)
		}
		if want := fmt.Sprintf("Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, %d PASSES_ON_CANDIDATE, %d UNVERIFIED", passes, 1-passes); !strings.Contains(console, want) {
			t.Fatalf("stdout:\n%s", console)
		}
		md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
		if err != nil || !strings.Contains(string(md), "## Changed Baseline Tests on Candidate Code") {
			t.Fatalf("markdown section missing: %v", err)
		}
		// Re-rendering gives the same files.
		again := filepath.Join(t.TempDir(), "again")
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{"report", "--input", filepath.Join(out, "confidence-report.json"), "--out", again}, &stdout, &stderr, "integration"); code != 0 {
			t.Fatalf("report exit %d: %s", code, stderr.String())
		}
		for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
			first, _ := os.ReadFile(filepath.Join(out, name))
			second, _ := os.ReadFile(filepath.Join(again, name))
			if !bytes.Equal(first, second) {
				t.Fatalf("%s differs after re-rendering", name)
			}
		}
		if status := git(t, dir, "status", "--porcelain"); status != "" {
			t.Fatalf("the checkout changed: %s", status)
		}
	})

	t.Run("G_build_tag", func(t *testing.T) {
		dir := buildConstraintFixture(t)
		code, r, _, console := baseTestsReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2: checks %+v\n%s", code, r.Checks, console)
		}
		if len(r.Checks) == 0 || r.Checks[0].Kind != "test" || r.Checks[0].Status != "PASS" {
			t.Fatalf("the candidate suite should pass without test files: %+v", r.Checks)
		}
		b := r.BaseTests
		if b == nil || b.Status != model.BaseTestsRan || len(b.Tests) != 3 {
			t.Fatalf("base tests %+v\n%s", b, console)
		}
		for _, bt := range b.Tests {
			if bt.Change != model.BaseTestSharedCodeChanged {
				t.Fatalf("test %+v", bt)
			}
			switch {
			case bt.Name == "TestClampUpper" && bt.Status == model.StatusFailsOnCandidate:
			case bt.Name == "TestClampUpper":
				t.Fatalf("TestClampUpper %+v", bt)
			case bt.Status == model.StatusPassesOnCandidate:
			case bt.Status == model.StatusUnverified && (strings.HasPrefix(bt.Reason, "the test passed inside a candidate-side run that failed as a whole") || strings.Contains(bt.Reason, "timed out")):
				t.Logf("the retry pair gave no result within the sub-cap: %s", bt.Reason)
			default:
				t.Fatalf("test %+v", bt)
			}
		}
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("check %s is ERROR: %s", c.ID, c.Output)
			}
		}
		if !strings.Contains(console, "Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE") {
			t.Fatalf("stdout:\n%s", console)
		}
	})

	t.Run("H_file_at_pkg", func(t *testing.T) {
		dir := testOnlyPackageFixture(t)
		code, r, _, console := baseTestsReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2 (a candidate layout is never operational): checks %+v\n%s", code, r.Checks, console)
		}
		b := r.BaseTests
		if b == nil || b.Status != model.BaseTestsRan || len(b.Tests) != 1 {
			t.Fatalf("base tests %+v\n%s", b, console)
		}
		if bt := b.Tests[0]; bt.Name != "TestUpperBound" || bt.Change != model.BaseTestFileDeleted || bt.Status != model.StatusFailsOnCandidate {
			t.Fatalf("test %+v", bt)
		}
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("check %s is ERROR: %s", c.ID, c.Output)
			}
		}
	})

	t.Run("B_api_change_is_unverified_not_operational", func(t *testing.T) {
		dir := t.TempDir()
		git(t, dir, "init", "-b", "main")
		git(t, dir, "config", "core.autocrlf", "false")
		write(t, dir, "go.mod", "module example.test/clamp\n\ngo 1.23\n")
		write(t, dir, "clamp.go", clampGo)
		write(t, dir, "clamp_test.go", clampTests)
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-m", "baseline")
		git(t, dir, "checkout", "-b", "candidate")
		write(t, dir, "clamp.go", strings.ReplaceAll(clampGo, "Clamp", "Limit"))
		write(t, dir, "clamp_test.go", strings.ReplaceAll(clampTests, "Clamp(", "Limit("))
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-m", "rename")
		code, r, _, console := baseTestsReview(t, dir, policy, "--ci")
		if code != 2 {
			t.Fatalf("exit %d, want 2 (not 4): %+v\n%s", code, r.Checks, console)
		}
		b := r.BaseTests
		if b == nil || b.Status != model.BaseTestsRan || len(b.Tests) != 3 {
			t.Fatalf("base tests %+v", b)
		}
		for _, bt := range b.Tests {
			if bt.Status != model.StatusUnverified || !strings.Contains(bt.Reason, "did not build") {
				t.Fatalf("test %+v", bt)
			}
		}
		for _, c := range r.Checks {
			if c.Status == "ERROR" {
				t.Fatalf("a compile failure became ERROR: %+v", c)
			}
			if c.Kind == model.CheckBaseTestHybrid && c.Status != "FAIL" {
				t.Fatalf("hybrid check %s", c.Status)
			}
		}
	})
}
