package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

const (
	clampGo        = "package clamp\n\n// Clamp bounds n to [0, 10].\nfunc Clamp(n int) int {\n\tif n < 0 {\n\t\treturn 0\n\t}\n\tif n > 10 {\n\t\treturn 10\n\t}\n\treturn n\n}\n"
	clampUnbounded = "package clamp\n\n// Clamp bounds n below by 0.\nfunc Clamp(n int) int {\n\tif n < 0 {\n\t\treturn 0\n\t}\n\treturn n\n}\n"
	clampTests     = `package clamp

import "testing"

func TestClampNegative(t *testing.T) {
	if got := Clamp(-5); got != 0 {
		t.Fatalf("Clamp(-5) = %d", got)
	}
}

func TestClampUpper(t *testing.T) {
	if got := Clamp(50); got != 10 {
		t.Fatalf("Clamp(50) = %d", got)
	}
}

func TestClampInside(t *testing.T) {
	if got := Clamp(3); got != 3 {
		t.Fatalf("Clamp(3) = %d", got)
	}
}
`
	// clampTestsEdited adds a comment inside TestClampNegative (not selected),
	// loosens TestClampUpper, deletes TestClampInside and adds a skipped test.
	clampTestsEdited = `package clamp

import "testing"

func TestClampNegative(t *testing.T) {
	// Negative inputs are raised to zero.
	if got := Clamp(-5); got != 0 {
		t.Fatalf("Clamp(-5) = %d", got)
	}
}

func TestClampUpper(t *testing.T) {
	if got := Clamp(50); got < 10 {
		t.Fatalf("Clamp(50) = %d", got)
	}
}

func TestLater(t *testing.T) {
	t.Skip("later")
}
`
	// extraTests moves TestClampInside to a new candidate-only file: the hybrid
	// tree removes it, so the restored baseline file cannot collide with it.
	extraTests = `package clamp

import "testing"

func TestClampInside(t *testing.T) {
	if got := Clamp(3); got != 3 {
		t.Fatalf("Clamp(3) = %d", got)
	}
}
`
)

// baseTestsClampFixture is the scenario A repository: the candidate drops the upper
// bound and edits the tests that asserted it.
func baseTestsClampFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "go.mod", "module example.test/clamp\n\ngo 1.23\n")
	write(t, dir, "clamp.go", clampGo)
	write(t, dir, "clamp_test.go", clampTests)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "clamp.go", clampUnbounded)
	write(t, dir, "clamp_test.go", clampTestsEdited)
	write(t, dir, "extra_test.go", extraTests)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// buildConstraintFixture is scenario G: the candidate drops the upper bound
// and turns its only test file off with a build constraint, so its own
// go test ./... passes with no test file. No test function changed.
func buildConstraintFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "go.mod", "module example.test/clamp\n\ngo 1.23\n")
	write(t, dir, "clamp.go", clampGo)
	write(t, dir, "clamp_test.go", clampTests)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "clamp.go", clampUnbounded)
	write(t, dir, "clamp_test.go", "//go:build never\n\n"+clampTests)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// testOnlyPackageFixture is scenario H: a test-only package directory is
// replaced by a regular file of the same name while the bound is dropped.
func testOnlyPackageFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "go.mod", "module example.test/clamp\n\ngo 1.23\n")
	write(t, dir, "clamp.go", clampGo)
	write(t, dir, "itest/bounds_test.go", "package itest\n\nimport (\n\t\"testing\"\n\n\t\"example.test/clamp\"\n)\n\nfunc TestUpperBound(t *testing.T) {\n\tif got := clamp.Clamp(50); got != 10 {\n\t\tt.Fatalf(\"Clamp(50) = %d\", got)\n\t}\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "clamp.go", clampUnbounded)
	git(t, dir, "rm", "-q", "-r", "itest")
	write(t, dir, "itest", "integration tests moved elsewhere\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// stageHarness builds a harness on the base and candidate snapshots of repo
// without starting Docker (an empty image records every run as SKIPPED).
func stageHarness(t *testing.T, repo *gitrepo.Repository, change model.Change, template []string) *harness.Harness {
	t.Helper()
	temp := t.TempDir()
	candidate, base := filepath.Join(temp, "candidate"), filepath.Join(temp, "base")
	if err := repo.Snapshot(context.Background(), change.HeadCommit, candidate); err != nil {
		t.Fatal(err)
	}
	if err := repo.Snapshot(context.Background(), change.BaseCommit, base); err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{CandidateDir: candidate, BaseDir: base, ArtifactDir: filepath.Join(temp, "artifacts"),
		Commands: map[string][]string{"test": {"go", "test", "./..."}, "generated_test": template}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func openChange(t *testing.T, dir, base, head string) (*gitrepo.Repository, model.Change) {
	t.Helper()
	repo, err := gitrepo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	change, err := repo.Analyze(context.Background(), base, head, true)
	if err != nil {
		t.Fatal(err)
	}
	return repo, change
}

// Selection and recording without Docker: the right tests are selected and,
// with no image, every run is SKIPPED, the section is not_run with that reason,
// and the Unverified entry names it.
func TestRunBaseTestsSelectsAndRecords(t *testing.T) {
	dir := baseTestsClampFixture(t)
	repo, change := openChange(t, dir, "main", "candidate")
	h := stageHarness(t, repo, change, []string{"go", "test", "{package}"})
	r := &model.Report{}
	var errOut bytes.Buffer
	if runBaseTests(context.Background(), repo, change, h, r, &errOut) {
		t.Fatal("reported an operational failure")
	}
	if !strings.Contains(errOut.String(), "Running the baseline versions of 2 changed tests on candidate code") {
		t.Fatalf("progress line: %q", errOut.String())
	}
	s := r.BaseTests
	if s == nil || s.Status != model.BaseTestsNotRun || s.Reason != "no run started: No Docker image configured; repository code was not executed." {
		t.Fatalf("section %+v", s)
	}
	var got []string
	for _, bt := range s.Tests {
		got = append(got, bt.Name+":"+bt.Change)
		if bt.Status != model.StatusUnverified || bt.EvidenceID != "" || !strings.Contains(bt.Reason, "did not start") {
			t.Fatalf("test %+v", bt)
		}
	}
	if strings.Join(got, ",") != "TestClampUpper:modified,TestClampInside:removed" {
		t.Fatalf("selected %v", got)
	}
	if len(r.Unverified) != 1 || r.Unverified[0] != baseTestsUnverifiedPrefix+s.Reason {
		t.Fatalf("unverified %q", r.Unverified)
	}
	if line := baseTestsLine(s); line != "Changed baseline tests on candidate code: not run (no run started: No Docker image configured; repository code was not executed)." {
		t.Fatalf("stdout line %q", line)
	}
}

// An edit that only adds a build constraint to a test file changes no test
// function, and still selects every test of the file.
func TestRunBaseTestsSelectsFileLevelEdits(t *testing.T) {
	dir := buildConstraintFixture(t)
	repo, change := openChange(t, dir, "main", "candidate")
	h := stageHarness(t, repo, change, []string{"go", "test", "{package}"})
	r := &model.Report{}
	if runBaseTests(context.Background(), repo, change, h, r, &bytes.Buffer{}) {
		t.Fatal("reported an operational failure")
	}
	var got []string
	for _, bt := range r.BaseTests.Tests {
		got = append(got, bt.Name+":"+bt.Change)
	}
	if strings.Join(got, ",") != "TestClampNegative:shared_code_changed,TestClampUpper:shared_code_changed,TestClampInside:shared_code_changed" {
		t.Fatalf("selected %v (section %+v)", got, r.BaseTests)
	}
}

func TestRunBaseTestsWithoutVerifiableTemplate(t *testing.T) {
	dir := baseTestsClampFixture(t)
	repo, change := openChange(t, dir, "main", "candidate")
	h := stageHarness(t, repo, change, []string{"go", "test", "./..."})
	r := &model.Report{}
	if runBaseTests(context.Background(), repo, change, h, r, &bytes.Buffer{}) {
		t.Fatal("reported an operational failure")
	}
	if r.BaseTests.Status != model.BaseTestsNotRun || !strings.Contains(r.BaseTests.Reason, "generated_test") || len(h.Checks()) != 0 {
		t.Fatalf("section %+v, checks %d", r.BaseTests, len(h.Checks()))
	}
}

func TestRunBaseTestsNothingSelected(t *testing.T) {
	dir := fixture(t) // changes auth.go only
	repo, change := openChange(t, dir, "main", "candidate")
	h := stageHarness(t, repo, change, []string{"go", "test", "{package}"})
	r := &model.Report{}
	if runBaseTests(context.Background(), repo, change, h, r, &bytes.Buffer{}) || r.BaseTests.Status != model.BaseTestsNoCandidates || len(r.Unverified) != 0 || len(h.Checks()) != 0 {
		t.Fatalf("section %+v unverified %q", r.BaseTests, r.Unverified)
	}
	if line := baseTestsLine(r.BaseTests); line != "Changed baseline tests on candidate code: none selected (only the tests declared in modified, deleted or renamed Go, TypeScript, JavaScript or Python test files are considered)." {
		t.Fatalf("stdout line %q", line)
	}
	// A changed test file that cannot be analyzed is not "nothing selected".
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "auth_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAllowed(t *testing.T) {\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "broken baseline test")
	git(t, dir, "checkout", "-q", "-b", "edit")
	write(t, dir, "auth_test.go", "package fixture\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "edit")
	repo, change = openChange(t, dir, "main", "edit")
	r = &model.Report{}
	if runBaseTests(context.Background(), repo, change, h, r, &bytes.Buffer{}) {
		t.Fatal("reported an operational failure")
	}
	if r.BaseTests.Status != model.BaseTestsNotRun || len(r.Unverified) != 2 || !strings.Contains(r.Unverified[0], "could not be parsed as Go") {
		t.Fatalf("section %+v unverified %q", r.BaseTests, r.Unverified)
	}
}

func TestRunBaseTestsCancelledPlanning(t *testing.T) {
	dir := baseTestsClampFixture(t)
	repo, change := openChange(t, dir, "main", "candidate")
	h := stageHarness(t, repo, change, []string{"go", "test", "{package}"})
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), harness.ErrOverallDeadline)
	defer cancel()
	r := &model.Report{}
	if runBaseTests(ctx, repo, change, h, r, &bytes.Buffer{}) {
		t.Fatal("a deadline is not an operational failure")
	}
	if r.BaseTests.Status != model.BaseTestsNotRun || r.BaseTests.Reason != "the overall deadline was reached before the changed tests were selected" || len(h.Checks()) != 0 {
		t.Fatalf("section %+v", r.BaseTests)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	r = &model.Report{}
	runBaseTests(cancelled, repo, change, h, r, &bytes.Buffer{})
	if r.BaseTests.Reason != "the review was cancelled before the changed tests were selected" {
		t.Fatalf("section %+v", r.BaseTests)
	}
}

func TestBaseTestsLine(t *testing.T) {
	if baseTestsLine(nil) != "" {
		t.Fatal("a line without a section")
	}
	s := &model.BaseTests{Status: model.BaseTestsRan, Tests: []model.BaseTest{{Status: model.StatusFailsOnCandidate}, {Status: model.StatusPassesOnCandidate}, {Status: model.StatusPassesOnCandidate}, {Status: model.StatusUnverified}}}
	want := "Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, 2 PASSES_ON_CANDIDATE, 1 UNVERIFIED (a failure is an outcome difference for a human to judge, not a reproduced issue; see base_tests)."
	if got := baseTestsLine(s); got != want {
		t.Fatalf("line %q", got)
	}
	for _, word := range []string{"regression", "bug", "defect", "masked", "%"} {
		if strings.Contains(strings.ToLower(want), word) {
			t.Fatalf("line uses %q", word)
		}
	}
	if got := baseTestsLine(&model.BaseTests{Status: model.BaseTestsNoCandidates, Reason: "no changed files"}); got != "Changed baseline tests on candidate code: none selected (no changed files)." {
		t.Fatalf("no_candidates line %q", got)
	}
	if got := baseTestsLine(&model.BaseTests{Status: model.BaseTestsNotRun}); got != "Changed baseline tests on candidate code: not run (no reason was recorded)." {
		t.Fatalf("not_run line %q", got)
	}
	// The line comes first among the stage lines.
	if lines := stdoutLines(&model.Report{BaseTests: s, Divergences: []model.Divergence{{}}}); len(lines) != 2 || lines[0] != want {
		t.Fatalf("stdout lines %q", lines)
	}
}

// Flag rules from the command line: nothing starts and nothing is written.
func TestBaseTestsFlagExitsThree(t *testing.T) {
	dir := baseTestsClampFixture(t)
	for _, args := range [][]string{
		{"lint", "--base-tests"},
		{"review", "--base-tests", "--checks=false"},
		{"review", "--base-tests", "--checks=false", "--reviewer=false"},
	} {
		forbidExecution(t)
		out := filepath.Join(t.TempDir(), "report")
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), append(args, "--repo", dir, "--out", out), &stdout, &stderr, "test"); code != 3 {
			t.Fatalf("%v: exit %d, want 3: %s", args, code, stderr.String())
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("%v wrote a report", args)
		}
	}
}

// Lint records no base_tests section and still reports the lexical test
// signals; re-rendering a report without the section keeps it absent.
func TestLintAndRenderWithoutBaseTests(t *testing.T) {
	dir := baseTestsClampFixture(t)
	forbidExecution(t)
	code, r, members, _ := runReport(t, context.Background(), dir, "lint", "--base", "main", "--head", "candidate")
	if code != 0 {
		t.Fatalf("lint exit %d", code)
	}
	if _, ok := members["base_tests"]; ok || r.BaseTests != nil {
		t.Fatal("lint recorded a base_tests section")
	}
	found := map[string]bool{}
	for _, s := range r.Signals {
		if s.Path == "clamp_test.go" {
			found[s.Kind] = true
		}
	}
	for _, kind := range []string{model.SignalTestExpectationRelaxed, model.SignalTestSkipAdded} {
		if !found[kind] {
			t.Errorf("lint lacks %s on clamp_test.go: %v", kind, found)
		}
	}
	// Re-render a report without the section.
	input := filepath.Join(t.TempDir(), "old.json")
	data, err := json.Marshal(model.Report{Version: 1, ToolVersion: "v0.2.0", Change: model.Change{Files: []model.ChangedFile{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rendered")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"report", "--input", input, "--out", out}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("report exit %d: %s", code, stderr.String())
	}
	rendered, err := os.ReadFile(filepath.Join(out, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "base_tests") {
		t.Fatal("re-rendering invented a base_tests section")
	}
}
