package harness

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Unit tests of the impacted-test stage (F6b) against a fake executor.

// itRun is one execution the fake executor received.
type itRun struct {
	side  string // base | candidate
	name  string
	args  []string
	names []string // the -run names
}

// itEvents renders go test -json events for the package of dir: name, action
// pairs.
func itEvents(dir string, pairs ...string) string {
	pkg := "example.test/m/" + dir
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n", pkg, pairs[i])
		fmt.Fprintf(&b, "{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", pairs[i+1], pkg, pairs[i])
	}
	return b.String()
}

// itMountDir returns the host directory mounted at /source.
func itMountDir(args []string) string {
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			for _, part := range strings.Split(args[i+1], ",") {
				if strings.HasPrefix(part, "src=") {
					return strings.TrimPrefix(part, "src=")
				}
			}
		}
	}
	return ""
}

// itRunNames returns the names of the -run expression ^(A|B)$.
func itRunNames(args []string) []string {
	for i, a := range args {
		if a == "-run" && i+1 < len(args) {
			return strings.Split(strings.TrimSuffix(strings.TrimPrefix(args[i+1], "^("), ")$"), "|")
		}
	}
	return nil
}

// itTarget returns the package or file argument of a go test command.
func itTarget(args []string) string {
	for i, a := range args {
		if a == "test" && i+1 < len(args) && i > 0 && args[i-1] == "go" {
			return args[i+1]
		}
	}
	return ""
}

// itExec installs a fake executor; respond returns the log and the result of
// the n-th run.
func itExec(t *testing.T, h *Harness, respond func(r itRun, n int) (string, execution)) *[]itRun {
	t.Helper()
	var runs []itRun
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		r := itRun{name: name, args: append([]string(nil), args...), names: itRunNames(args)}
		switch itMountDir(args) {
		case h.base:
			r.side = "base"
		case h.candidate:
			r.side = "candidate"
		default:
			t.Errorf("a run mounted neither snapshot: %q", args)
		}
		output, result := respond(r, len(runs))
		runs = append(runs, r)
		_, _ = io.WriteString(out, output)
		return result
	}
	return &runs
}

// itAll answers every run with one action per side for every requested name,
// failing the run when that action is fail.
func itAll(baseAction, candidateAction string) func(itRun, int) (string, execution) {
	return func(r itRun, _ int) (string, execution) {
		action := baseAction
		if r.side == "candidate" {
			action = candidateAction
		}
		dir := strings.TrimPrefix(itTarget(r.args), "./")
		var pairs []string
		for _, n := range r.names {
			pairs = append(pairs, n, action)
		}
		exit := 0
		if action == "fail" {
			exit = 1
		}
		return itEvents(dir, pairs...), execution{ExitCode: exit}
	}
}

const (
	itPkgTests   = "package pkg\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(\"V\")\n\t}\n}\n\nfunc TestB(t *testing.T) {}\n\nfunc helper() {}\n"
	itOtherTests = "package other\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n"
)

// itTrees returns a baseline and a candidate tree: pkg.V changed, the test
// files unchanged.
func itTrees() (base, candidate map[string]string) {
	base = map[string]string{
		"go.mod":          "module example.test/m\n\ngo 1.23\n",
		"pkg/v.go":        "package pkg\n\nfunc V() int { return 1 }\n",
		"pkg/a_test.go":   itPkgTests,
		"other/o.go":      "package other\n",
		"other/o_test.go": itOtherTests,
	}
	candidate = map[string]string{}
	for p, c := range base {
		candidate[p] = c
	}
	candidate["pkg/v.go"] = "package pkg\n\nfunc V() int { return 2 }\n"
	return base, candidate
}

func itHarness(t *testing.T, base, candidate map[string]string, template []string) *Harness {
	t.Helper()
	b, c := t.TempDir(), t.TempDir()
	for root, files := range map[string]map[string]string{b: base, c: candidate} {
		for p, content := range files {
			full := filepath.Join(root, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	h, err := New(Options{CandidateDir: c, BaseDir: b, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxOutputBytes: 64 * 1024,
		Commands: map[string][]string{"test": {"go", "test", "./..."}, "generated_test": template}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func itSelected() []model.ImpactTest {
	return []model.ImpactTest{
		{Name: "TestA", Path: "pkg/a_test.go", Line: 5, Package: "example.test/m/pkg", Depth: 1, Resolution: model.ResolutionStatic},
		{Name: "TestB", Path: "pkg/a_test.go", Line: 11, Package: "example.test/m/pkg", Depth: 1, Resolution: model.ResolutionStatic},
		{Name: "TestC", Path: "other/o_test.go", Line: 5, Package: "example.test/m/other", Depth: 2, Resolution: model.ResolutionInterface},
	}
}

func itCheck(t *testing.T, h *Harness, id string) model.Check {
	t.Helper()
	for _, c := range h.Checks() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %s not recorded", id)
	return model.Check{}
}

func itEvidence(t *testing.T, h *Harness, id string) model.Evidence {
	t.Helper()
	for _, e := range h.Evidence() {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("evidence %s not recorded", id)
	return model.Evidence{}
}

func itSides(runs []itRun) string {
	var s []string
	for _, r := range runs {
		s = append(s, r.side+":"+itTarget(r.args)+":"+strings.Join(r.names, "|"))
	}
	return strings.Join(s, ",")
}

// The stage runs each package's selected tests on the baseline and the
// candidate with one identical command, gives a test that passed inside a
// failed candidate run a pair of its own, and records one evidence record per
// test that ClassifyExistingTest re-derives from the recorded checks.
func TestImpactedTestsRunPairsAndEvidence(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "candidate" && len(r.names) == 2 {
			return itEvents("pkg", "TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		return itAll("pass", "pass")(r, n)
	})
	res := h.RunImpactedTests(context.Background(), itSelected())
	if res.Status != model.ImpactTestsRan || res.Reason != "" || res.Capped != 0 || res.Errors != 0 || len(res.Tests) != 3 {
		t.Fatalf("result %+v", res)
	}
	if got := itSides(*runs); got != "base:./pkg:TestA|TestB,candidate:./pkg:TestA|TestB,base:./pkg:TestB,candidate:./pkg:TestB,base:./other:TestC,candidate:./other:TestC" {
		t.Fatalf("runs %s", got)
	}
	// Both runs of a pair use one identical command and the unchanged sandbox profile.
	commands := [][]string{
		{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestA|TestB)$"},
		{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestB)$"},
		{"go", "test", "./other", "-json", "-count=1", "-run", "^(TestC)$"},
	}
	for i, r := range *runs {
		if want := h.dockerArgs(r.name, itMountDir(r.args), commands[i/2]); strings.Join(r.args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("run %d args\n%q\nwant\n%q", i, r.args, want)
		}
	}
	checks := h.Checks()
	var kinds []string
	for _, c := range checks {
		kinds = append(kinds, c.Kind+":"+c.Status)
	}
	if got := strings.Join(kinds, ","); got != "impacted_test_base:PASS,impacted_test_candidate:FAIL,impacted_test_base:PASS,impacted_test_candidate:PASS,impacted_test_base:PASS,impacted_test_candidate:PASS" {
		t.Fatalf("checks %s", got)
	}
	want := []struct {
		status, base, candidate string
	}{
		{model.StatusFailsOnCandidate, "check-1", "check-2"},
		{model.StatusPassesOnCandidate, "check-3", "check-4"},
		{model.StatusPassesOnCandidate, "check-5", "check-6"},
	}
	for i, tt := range res.Tests {
		w := want[i]
		if tt.Status != w.status || tt.Reason != "" || tt.EvidenceID == "" || tt.Name != itSelected()[i].Name || tt.Line != itSelected()[i].Line || tt.Depth != itSelected()[i].Depth {
			t.Fatalf("test %d: %+v", i, tt)
		}
		e := itEvidence(t, h, tt.EvidenceID)
		if e.Kind != model.EvidenceImpactedTestDifferential || e.Runner != RunnerGo || strings.Join(e.TestNames, ",") != tt.Name || e.Path != tt.Path || e.BaseCheckID != w.base || e.CheckID != w.candidate || e.Status != w.status {
			t.Fatalf("evidence %+v", e)
		}
		if status, _ := ClassifyExistingTest(itCheck(t, h, e.BaseCheckID), itCheck(t, h, e.CheckID), tt.Name); status != w.status {
			t.Fatalf("recorded checks of %s classify as %s", tt.Name, status)
		}
		lower := strings.ToLower(e.Description)
		for _, word := range []string{"regression", "bug", "tested", "verified", "correct", "safe", "covers", "caused"} {
			if strings.Contains(lower, word) {
				t.Fatalf("description uses %q: %s", word, e.Description)
			}
		}
		if !strings.Contains(e.Description, "approximate") || !strings.Contains(e.Description, "did not modify") {
			t.Fatalf("description %q", e.Description)
		}
	}
	// One audit event per unit, under the reserved stage prefix, with the
	// worst status of the unit's checks: the pkg unit's candidate run failed
	// before its retry pair passed.
	var audited []model.AuditEvent
	for _, e := range h.Audit() {
		if e.Tool == auditRunImpactedTests {
			audited = append(audited, e)
		}
	}
	if len(audited) != 2 || !strings.Contains(audited[0].Arguments, `"unit":"pkg"`) || !strings.Contains(audited[0].Arguments, `"check-4"`) || audited[0].Status != "FAIL" ||
		!strings.Contains(audited[1].Arguments, `"unit":"other"`) || audited[1].Status != "PASS" {
		t.Fatalf("audit %+v", audited)
	}
	// The input was copied, not modified.
	if in := itSelected(); in[0].EvidenceID != "" || in[0].Status != "" {
		t.Fatal("the input was modified")
	}
}

func TestImpactedTestsOutcomes(t *testing.T) {
	type response struct {
		output string
		result execution
	}
	pass := response{itEvents("pkg", "TestA", "pass"), execution{}}
	fail := response{itEvents("pkg", "TestA", "fail"), execution{ExitCode: 1}}
	cases := []struct {
		name            string
		base, candidate response
		status          string // "" when no candidate run happened
		reason          string // substring
		runs            int
		candidateStatus string
	}{
		{"fails_on_candidate", pass, fail, model.StatusFailsOnCandidate, "", 2, "FAIL"},
		{"passes_on_candidate", pass, pass, model.StatusPassesOnCandidate, "", 2, "PASS"},
		{"baseline_failed", fail, pass, "", "the test failed on the baseline (check-1), so it was not run on candidate code", 1, ""},
		{"baseline_skipped", response{itEvents("pkg", "TestA", "skip"), execution{}}, pass, "", "was skipped on the baseline (check-1)", 1, ""},
		{"baseline_test_absent", response{"", execution{}}, pass, "", "does not record exactly one run and one result of this test", 1, ""},
		{"baseline_build_failure", response{"# example.test/m/pkg\nundefined: V\nFAIL\texample.test/m/pkg [build failed]\n", execution{ExitCode: 1}}, pass, "", "did not build", 1, ""},
		{"baseline_timeout", response{"", execution{ExitCode: -1, TimedOut: true, Err: context.DeadlineExceeded}}, pass, "", "timed out", 1, ""},
		{"compile_failure_on_candidate_is_fail_not_error", pass, response{"# example.test/m/pkg\npkg/v.go:3:5: undefined: W\nFAIL\texample.test/m/pkg [build failed]\n", execution{ExitCode: 1}}, model.StatusUnverified, reasonCandidateBuild, 2, "FAIL"},
		{"setup_failure_text_on_candidate", pass, response{"fork/exec /tmp/x: permission denied\n", execution{ExitCode: 1}}, model.StatusUnverified, reasonCandidateOutcome, 2, "FAIL"},
		{"candidate_timeout", pass, response{"", execution{ExitCode: -1, TimedOut: true, Err: context.DeadlineExceeded}}, model.StatusUnverified, reasonCandidateTimeout, 2, "TIMEOUT"},
		{"candidate_infrastructure_error", pass, response{itEvents("pkg", "TestA", "pass"), execution{ExitCode: 125}}, model.StatusUnverified, reasonCandidateIncomplete, 2, "ERROR"},
		{"candidate_skip", pass, response{itEvents("pkg", "TestA", "skip"), execution{}}, model.StatusUnverified, reasonCandidateOutcome, 2, "PASS"},
		{"candidate_truncated", pass, response{itEvents("pkg", "TestA", "pass") + strings.Repeat("x", 70*1024), execution{}}, model.StatusUnverified, reasonCandidateTrunc, 2, "PASS"},
		{"forged_pass_after_real_fail", pass, response{itEvents("pkg", "TestA", "fail") + "{\"Action\":\"pass\",\"Package\":\"example.test/m/pkg\",\"Test\":\"TestA\"}\n", execution{}}, model.StatusUnverified, reasonCandidateOutcome, 2, "PASS"},
		{"failed_run_carrying_a_pass_event", pass, response{itEvents("pkg", "TestA", "pass"), execution{ExitCode: 1}}, model.StatusUnverified, impactedPassedInsideFailure + "; no run pair of its own was attempted because the failed run held only this test", 2, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, candidate := itTrees()
			h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
			runs := itExec(t, h, func(r itRun, _ int) (string, execution) {
				if r.side == "candidate" {
					return tc.candidate.output, tc.candidate.result
				}
				return tc.base.output, tc.base.result
			})
			res := h.RunImpactedTests(context.Background(), itSelected()[:1])
			got := res.Tests[0]
			if got.Status != tc.status || !strings.Contains(got.Reason, tc.reason) || (tc.status != model.StatusUnverified && tc.status != "" && got.Reason != "") {
				t.Fatalf("test %+v, want %q (%q)", got, tc.status, tc.reason)
			}
			if len(*runs) != tc.runs || res.Status != model.ImpactTestsRan {
				t.Fatalf("%d runs (want %d), result %+v", len(*runs), tc.runs, res)
			}
			checks := h.Checks()
			if tc.candidateStatus == "" {
				// No candidate run: no evidence record either.
				if got.EvidenceID != "" || len(h.Evidence()) != 0 || len(checks) != 1 {
					t.Fatalf("a test without a candidate run has evidence %q / checks %+v", got.EvidenceID, checks)
				}
				return
			}
			// The raw check status is never overwritten by the classification.
			if checks[1].Status != tc.candidateStatus || checks[1].Kind != model.CheckImpactedTestCandidate {
				t.Fatalf("candidate check %+v, want %s", checks[1], tc.candidateStatus)
			}
			if wantErrors := map[bool]int{true: 1, false: 0}[tc.candidateStatus == "ERROR"]; res.Errors != wantErrors {
				t.Fatalf("errors %d, want %d", res.Errors, wantErrors)
			}
			e := itEvidence(t, h, got.EvidenceID)
			if e.Status != got.Status || e.CheckID != checks[1].ID || e.BaseCheckID != checks[0].ID {
				t.Fatalf("evidence %+v", e)
			}
		})
	}
}

// A baseline run that fails as a whole still runs the tests it records as
// passed, on their own; the test that failed on the baseline gets no pair.
func TestImpactedTestsBaselineFailureNarrowsToPassedTests(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "base" && len(r.names) == 2 {
			return itEvents("pkg", "TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		return itAll("pass", "fail")(r, n)
	})
	res := h.RunImpactedTests(context.Background(), itSelected()[:2])
	if got := itSides(*runs); got != "base:./pkg:TestA|TestB,base:./pkg:TestB,candidate:./pkg:TestB" {
		t.Fatalf("runs %s", got)
	}
	a, b := res.Tests[0], res.Tests[1]
	if a.Status != "" || a.EvidenceID != "" || !strings.Contains(a.Reason, "the test failed on the baseline (check-1)") {
		t.Fatalf("TestA %+v", a)
	}
	if e := itEvidence(t, h, b.EvidenceID); b.Status != model.StatusFailsOnCandidate || e.BaseCheckID != "check-2" || e.CheckID != "check-3" {
		t.Fatalf("TestB %+v, evidence %+v", b, e)
	}
}

// A test that passed inside a failed candidate run and gets no result from a
// pair of its own keeps the first pair's evidence, with a reason saying so;
// when every test of the failed run passed in it, no retry is attempted.
func TestImpactedTestsRetryReasons(t *testing.T) {
	base, candidate := itTrees()
	failedInsideFirst := func(r itRun, n int) (string, execution) {
		if r.side == "candidate" && len(r.names) == 2 {
			return itEvents("pkg", "TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		if r.side == "base" && len(r.names) == 1 {
			return itEvents("pkg", "TestB", "fail"), execution{ExitCode: 1}
		}
		return itAll("pass", "pass")(r, n)
	}
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, failedInsideFirst)
	res := h.RunImpactedTests(context.Background(), itSelected()[:2])
	b := res.Tests[1]
	if len(*runs) != 3 || res.Tests[0].Status != model.StatusFailsOnCandidate || b.Status != model.StatusUnverified || b.EvidenceID == "" ||
		!strings.HasPrefix(b.Reason, impactedPassedInsideFailure+"; its own run pair gave no result (the test failed on the baseline (check-3)") {
		t.Fatalf("%d runs; TestB %+v", len(*runs), b)
	}
	if e := itEvidence(t, h, b.EvidenceID); e.CheckID != "check-2" || e.Status != model.StatusUnverified {
		t.Fatalf("TestB evidence %+v", e)
	}
	// Every test of the failed run passed in it (for example TestMain exited 1).
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "candidate" {
			return itEvents("pkg", "TestA", "pass", "TestB", "pass"), execution{ExitCode: 1}
		}
		return itAll("pass", "pass")(r, n)
	})
	res = h.RunImpactedTests(context.Background(), itSelected()[:2])
	for _, tt := range res.Tests {
		if len(*runs) != 2 || tt.Status != model.StatusUnverified || tt.Reason != impactedPassedInsideFailure+"; no run pair of its own was attempted because every test of the failed run passed in it" {
			t.Fatalf("%d runs; %+v", len(*runs), tt)
		}
	}
	// The review ends while the first pair runs: no retry starts.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs = itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "candidate" {
			cancel()
		}
		return failedInsideFirst(r, n)
	})
	res = h.RunImpactedTests(ctx, itSelected())
	if b := res.Tests[1]; len(*runs) != 2 || b.Status != model.StatusUnverified || b.Reason != impactedPassedInsideFailure+"; its own run pair gave no result (the review was cancelled)" {
		t.Fatalf("%d runs; TestB %+v", len(*runs), b)
	}
	if c := res.Tests[2]; c.Status != "" || c.Reason != "not run: the review was cancelled" {
		t.Fatalf("TestC %+v", c)
	}
}

// A candidate run that does not start (the shared runtime budget ran out
// during the baseline run) gives its tests no run pair and no evidence record:
// nothing may describe a candidate run that never happened.
func TestImpactedTestsCandidateRunNotStarted(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, func(r itRun, n int) (string, execution) {
		h.spent = h.opts.MaxRuntime
		return itAll("pass", "pass")(r, n)
	})
	res := h.RunImpactedTests(context.Background(), itSelected()[:2])
	if len(*runs) != 1 || res.Status != model.ImpactTestsRan || len(h.Evidence()) != 0 {
		t.Fatalf("%d runs, result %+v, evidence %+v", len(*runs), res, h.Evidence())
	}
	var kinds []string
	for _, c := range h.Checks() {
		kinds = append(kinds, c.Kind+":"+c.Status)
	}
	if got := strings.Join(kinds, ","); got != "impacted_test_base:PASS,impacted_test_candidate:SKIPPED" {
		t.Fatalf("checks %s", got)
	}
	for _, tt := range res.Tests {
		if tt.Status != "" || tt.EvidenceID != "" || tt.Reason != "the candidate run check-2 did not start (Sandbox runtime budget exhausted), so no result was drawn" {
			t.Fatalf("test %+v", tt)
		}
	}
	if tools := auditTools(h); len(tools) != 1 || h.Audit()[0].Status != "SKIPPED" {
		t.Fatalf("audit %+v", h.Audit())
	}
	// The same in a retry pair: the test keeps the first pair's evidence and
	// says why its own pair gave no result.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "candidate" && len(r.names) == 2 {
			return itEvents("pkg", "TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		if n == 2 {
			h.spent = h.opts.MaxRuntime
		}
		return itAll("pass", "pass")(r, n)
	})
	res = h.RunImpactedTests(context.Background(), itSelected()[:2])
	b := res.Tests[1]
	if len(*runs) != 3 || res.Tests[0].Status != model.StatusFailsOnCandidate || b.Status != model.StatusUnverified ||
		b.Reason != impactedPassedInsideFailure+"; its own run pair gave no result (the candidate run check-4 did not start (Sandbox runtime budget exhausted), so no result was drawn)" {
		t.Fatalf("%d runs; TestB %+v", len(*runs), b)
	}
	if e := itEvidence(t, h, b.EvidenceID); e.CheckID != "check-2" || e.BaseCheckID != "check-1" || len(h.Evidence()) != 2 {
		t.Fatalf("TestB evidence %+v; %d records", e, len(h.Evidence()))
	}
}

// §1.11: FAILS_ON_CANDIDATE never rests on a replayed baseline; the baseline
// runs again live with the same kind and command.
func TestImpactedTestsReplayedBaselineIsConfirmedLive(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	cache := useMemoryCache(h)
	candidateAction, liveBase := "pass", "pass"
	onCandidate := func() {}
	runs := itExec(t, h, func(r itRun, n int) (string, execution) {
		if r.side == "candidate" {
			onCandidate()
		}
		if r.side == "base" && n > 0 {
			return itAll(liveBase, "")(r, n)
		}
		return itAll("pass", candidateAction)(r, n)
	})
	selected := itSelected()[:1]
	if res := h.RunImpactedTests(context.Background(), selected); res.Tests[0].Status != model.StatusPassesOnCandidate {
		t.Fatalf("first run: %+v", res)
	}
	if keys := cache.keys(); len(keys) != 1 {
		t.Fatalf("cache keys %v (only the baseline-side run is cacheable)", keys)
	}
	cache.promote(2)
	// PASSES may rest on a replay of two agreeing live runs.
	before := len(*runs)
	res := h.RunImpactedTests(context.Background(), selected)
	e := itEvidence(t, h, res.Tests[0].EvidenceID)
	if res.Tests[0].Status != model.StatusPassesOnCandidate || !itCheck(t, h, e.BaseCheckID).Replayed() || len(*runs)-before != 1 {
		t.Fatalf("replayed PASSES: %+v, evidence %+v, %d executions", res.Tests[0], e, len(*runs)-before)
	}
	// FAILS on a replayed baseline triggers one live baseline run.
	candidateAction = "fail"
	before = len(*runs)
	res = h.RunImpactedTests(context.Background(), selected)
	e = itEvidence(t, h, res.Tests[0].EvidenceID)
	live := itCheck(t, h, e.BaseCheckID)
	if res.Tests[0].Status != model.StatusFailsOnCandidate || live.Replayed() || live.Kind != model.CheckImpactedTestBase || len(*runs)-before != 2 || (*runs)[len(*runs)-1].side != "base" {
		t.Fatalf("confirmed FAILS: %+v, base %+v, %d executions", res.Tests[0], live, len(*runs)-before)
	}
	replayed := 0
	for _, c := range h.Checks() {
		if c.Replayed() {
			replayed++
			if c.Kind != model.CheckImpactedTestBase {
				t.Fatalf("replayed %s", c.Kind)
			}
		}
	}
	if replayed != 2 {
		t.Fatalf("%d replayed checks kept in the ledger, want 2", replayed)
	}
	// A live baseline that no longer passes leaves the test UNVERIFIED.
	cache.promote(2)
	liveBase = "fail"
	res = h.RunImpactedTests(context.Background(), selected)
	if res.Tests[0].Status != model.StatusUnverified || !strings.Contains(res.Tests[0].Reason, "baseline run did not pass") {
		t.Fatalf("contradicted confirmation: %+v", res.Tests[0])
	}
	// The contradiction evicted the entry. Two agreeing runs again, then a
	// review that ends before the live re-run can start: UNVERIFIED with the
	// reason, resting on the replayed baseline.
	liveBase, candidateAction = "pass", "pass"
	h.RunImpactedTests(context.Background(), selected)
	cache.promote(2)
	candidateAction = "fail"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onCandidate = cancel
	before = len(*runs)
	res = h.RunImpactedTests(ctx, selected)
	e = itEvidence(t, h, res.Tests[0].EvidenceID)
	if len(*runs)-before != 1 || res.Tests[0].Status != model.StatusUnverified || !itCheck(t, h, e.BaseCheckID).Replayed() ||
		res.Tests[0].Reason != "the baseline run was replayed from the execution cache and could not be repeated live (the review was cancelled)" {
		t.Fatalf("no live re-run: %+v, evidence %+v, %d executions", res.Tests[0], e, len(*runs)-before)
	}
	// A live re-run that does not start (the shared budget ran out during the
	// candidate run) leaves the test UNVERIFIED on the replayed baseline; the
	// skipped check is not cited.
	cache.promote(2)
	onCandidate = func() { h.spent = h.opts.MaxRuntime }
	before, checksBefore := len(*runs), len(h.Checks())
	res = h.RunImpactedTests(context.Background(), selected)
	e = itEvidence(t, h, res.Tests[0].EvidenceID)
	checks := h.Checks()[checksBefore:]
	if len(*runs)-before != 1 || len(checks) != 3 || checks[2].Status != "SKIPPED" || res.Tests[0].Status != model.StatusUnverified || e.BaseCheckID != checks[0].ID || !checks[0].Replayed() ||
		res.Tests[0].Reason != "the baseline run was replayed from the execution cache and could not be repeated live ("+checks[2].ID+" did not start: Sandbox runtime budget exhausted)" {
		t.Fatalf("skipped live re-run: %+v, evidence %+v, checks %+v", res.Tests[0], e, checks)
	}
}

// At most 16 tests from at most 4 packages run, in input order; the others
// get the limit reason and are counted.
func TestImpactedTestsLimits(t *testing.T) {
	base := map[string]string{"go.mod": "module example.test/m\n"}
	var selected []model.ImpactTest
	for d := 1; d <= 6; d++ {
		dir := fmt.Sprintf("p%d", d)
		var src strings.Builder
		src.WriteString("package p\n\nimport \"testing\"\n")
		for n := 0; n < 5; n++ {
			name := fmt.Sprintf("Test%d%d", d, n)
			fmt.Fprintf(&src, "\nfunc %s(t *testing.T) {}\n", name)
			selected = append(selected, model.ImpactTest{Name: name, Path: dir + "/p_test.go", Line: 1, Depth: 1, Resolution: model.ResolutionStatic})
		}
		base[dir+"/p.go"] = "package p\n"
		base[dir+"/p_test.go"] = src.String()
	}
	// A duplicate of an admitted test shares its unit and result.
	selected = append(selected, selected[0])
	h := itHarness(t, base, base, []string{"go", "test", "{package}"})
	runs := itExec(t, h, itAll("pass", "pass"))
	res := h.RunImpactedTests(context.Background(), selected)
	if res.Status != model.ImpactTestsRan || res.Capped != len(selected)-1-ImpactedMaxTests {
		t.Fatalf("result status %s capped %d", res.Status, res.Capped)
	}
	var units []string
	for _, r := range *runs {
		if r.side == "base" {
			units = append(units, itTarget(r.args)+":"+strings.Join(r.names, "|"))
		}
	}
	if got := strings.Join(units, ","); got != "./p1:Test10|Test11|Test12|Test13|Test14,./p2:Test20|Test21|Test22|Test23|Test24,./p3:Test30|Test31|Test32|Test33|Test34,./p4:Test40" {
		t.Fatalf("units %s", got)
	}
	ran := 0
	for i, tt := range res.Tests {
		switch {
		case tt.Status == model.StatusPassesOnCandidate:
			ran++
		case i < len(selected)-1 && tt.Reason == "not run: the stage runs at most 16 tests from at most 4 packages per review":
		default:
			t.Fatalf("test %d %+v", i, tt)
		}
	}
	if ran != ImpactedMaxTests+1 || res.Tests[len(selected)-1].EvidenceID != res.Tests[0].EvidenceID {
		t.Fatalf("%d with a result; duplicate %+v vs %+v", ran, res.Tests[len(selected)-1], res.Tests[0])
	}
	if n := len(h.Evidence()); n != ImpactedMaxTests {
		t.Fatalf("%d evidence records, want one per distinct test", n)
	}
	// With a {file} template, one unit per test file.
	base, candidate := itTrees()
	base["pkg/b_test.go"] = "package pkg\n\nimport \"testing\"\n\nfunc TestD(t *testing.T) {}\n"
	candidate["pkg/b_test.go"] = base["pkg/b_test.go"]
	h = itHarness(t, base, candidate, []string{"go", "test", "{file}"})
	runs = itExec(t, h, itAll("pass", "pass"))
	selected = append(itSelected()[:2], model.ImpactTest{Name: "TestD", Path: "pkg/b_test.go", Line: 5, Depth: 1})
	h.RunImpactedTests(context.Background(), selected)
	units = nil
	for _, r := range *runs {
		if r.side == "base" {
			units = append(units, itTarget(r.args)+":"+strings.Join(r.names, "|"))
		}
	}
	if got := strings.Join(units, ","); got != "pkg/a_test.go:TestA|TestB,pkg/b_test.go:TestD" {
		t.Fatalf("file units %s", got)
	}
	if len(h.Evidence()) != 3 {
		t.Fatalf("%d evidence records", len(h.Evidence()))
	}
}

func TestImpactedTestsPrechecksRunNothing(t *testing.T) {
	base, candidate := itTrees()
	base["gone/g_test.go"] = "package gone\n\nimport \"testing\"\n\nfunc TestGone(t *testing.T) {}\n"
	base["credentials/c_test.go"] = "package credentials\n\nimport \"testing\"\n\nfunc TestSecret(t *testing.T) {}\n"
	candidate["credentials/c_test.go"] = base["credentials/c_test.go"]
	base["edit/e_test.go"] = "package edit\n\nimport \"testing\"\n\nfunc TestEdit(t *testing.T) {}\n"
	candidate["edit/e_test.go"] = "package edit\n\nimport \"testing\"\n\nfunc TestEdit(t *testing.T) { t.Log(1) }\n"
	candidate["added/a_test.go"] = "package added\n\nimport \"testing\"\n\nfunc TestAdded(t *testing.T) {}\n"
	base["bad/b_test.go"] = "package bad\n\nfunc (\n"
	candidate["bad/b_test.go"] = base["bad/b_test.go"]
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, itAll("pass", "pass"))
	selected := []model.ImpactTest{
		{Name: "TestGone", Path: "gone/g_test.go"},
		{Name: "TestSecret", Path: "credentials/c_test.go"},
		{Name: "TestEdit", Path: "edit/e_test.go"},
		{Name: "TestAdded", Path: "added/a_test.go"},
		{Name: "TestBad", Path: "bad/b_test.go"},
		{Name: "TestMissing", Path: "pkg/a_test.go"},
		{Name: "helper", Path: "pkg/a_test.go"},
		{Name: "Testlower", Path: "pkg/a_test.go"},
		{Name: "TestX", Path: "pkg/v.go"},
		{Name: "TestA/sub", Path: "pkg/a_test.go"},
		{Name: "TestA", Path: "../pkg/a_test.go"},
	}
	res := h.RunImpactedTests(context.Background(), selected)
	if len(*runs) != 0 || len(h.Checks()) != 0 || res.Status != model.ImpactTestsNotRun || res.Reason != impactedNothingRan || res.Capped != 0 {
		t.Fatalf("result %+v after %d runs", res, len(*runs))
	}
	want := []string{
		"not in the candidate snapshot",
		"excluded from the sandbox snapshots",
		"differs between the baseline and candidate snapshots",
		"not in the baseline snapshot",
		"could not be parsed as Go",
		"declares no top-level function of this name",
		"not a Go test function",
		"not a Go test function",
		"not a Go test function",
		"not a Go test function",
		"excluded from the sandbox snapshots",
	}
	for i, tt := range res.Tests {
		if tt.Status != "" || tt.EvidenceID != "" || !strings.HasPrefix(tt.Reason, "not run: ") || !strings.Contains(tt.Reason, want[i]) {
			t.Errorf("%s: %+v, want reason %q", tt.Name, tt, want[i])
		}
	}
	if tools := auditTools(h); len(tools) != 1 || tools[0] != auditRunImpactedTests {
		t.Fatalf("audit %v", tools)
	}
}

func TestImpactedTestsUnverifiableTemplateRunsNothing(t *testing.T) {
	for _, template := range [][]string{{"go", "test", "./..."}, {"npm", "test"}, {"go", "test", "-exec=x", "{package}"}, nil} {
		base, candidate := itTrees()
		h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
		if template == nil {
			delete(h.opts.Commands, "generated_test")
		} else {
			h.opts.Commands["generated_test"] = template
		}
		runs := itExec(t, h, itAll("pass", "pass"))
		res := h.RunImpactedTests(context.Background(), itSelected())
		if res.Status != model.ImpactTestsNotRun || res.Reason != impactedTemplateReason || len(*runs) != 0 || len(h.Checks()) != 0 {
			t.Fatalf("%v: result %+v, %d runs", template, res, len(*runs))
		}
		for _, tt := range res.Tests {
			if tt.Status != "" || tt.Reason != "not run: "+impactedTemplateReason || tt.EvidenceID != "" {
				t.Fatalf("%v: test %+v", template, tt)
			}
		}
		if tools := auditTools(h); len(tools) != 1 || tools[0] != auditRunImpactedTests {
			t.Fatalf("audit %v", tools)
		}
	}
	// A closed harness runs nothing either.
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, itAll("pass", "pass"))
	h.Close()
	if res := h.RunImpactedTests(context.Background(), itSelected()); res.Status != model.ImpactTestsNotRun || res.Reason != "the harness is closed" || len(*runs) != 0 {
		t.Fatalf("closed harness: %+v", res)
	}
	// An empty selection is no_candidates, without any audit event.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	if res := h.RunImpactedTests(context.Background(), nil); res.Status != model.ImpactTestsNoCandidates || len(res.Tests) != 0 || len(h.Audit()) != 0 {
		t.Fatalf("empty selection: %+v", res)
	}
}

func TestImpactedTestsBudget(t *testing.T) {
	base, candidate := itTrees()
	// The whole budget is spent: runs are recorded as SKIPPED and nothing executes.
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, itAll("pass", "pass"))
	h.spent = h.opts.MaxRuntime
	res := h.RunImpactedTests(context.Background(), itSelected())
	if len(*runs) != 0 || res.Status != model.ImpactTestsNotRun || res.Reason != "no run started: "+budgetExhaustedText {
		t.Fatalf("result %+v after %d runs", res, len(*runs))
	}
	for _, tt := range res.Tests {
		if tt.Status != "" || !strings.Contains(tt.Reason, "Sandbox runtime budget exhausted") {
			t.Fatalf("test %+v", tt)
		}
	}
	for _, c := range h.Checks() {
		if c.Status != "SKIPPED" || c.Kind != model.CheckImpactedTestBase {
			t.Fatalf("check %+v", c)
		}
	}
	// Half of the budget is reserved for the reviewer and the other half is
	// spent: the stage's ceiling stops it with the reserve text.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = itExec(t, h, itAll("pass", "pass"))
	h.opts.ReviewerReserve = h.opts.MaxRuntime / 2
	h.spent = h.opts.MaxRuntime / 2
	res = h.RunImpactedTests(context.Background(), itSelected())
	if len(*runs) != 0 || res.Reason != "no run started: "+budgetReservedText {
		t.Fatalf("reserve: result %+v after %d runs", res, len(*runs))
	}
	// A reserve that leaves nothing stops the stage before any run.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = itExec(t, h, itAll("pass", "pass"))
	h.opts.ReviewerReserve = h.opts.MaxRuntime
	res = h.RunImpactedTests(context.Background(), itSelected())
	if len(*runs) != 0 || len(h.Checks()) != 0 || res.Status != model.ImpactTestsNotRun || !strings.Contains(res.Tests[0].Reason, budgetReservedText) {
		t.Fatalf("full reserve: result %+v after %d runs", res, len(*runs))
	}
	// Every run is charged to the shared budget.
	h = itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	itExec(t, h, func(r itRun, n int) (string, execution) {
		time.Sleep(5 * time.Millisecond)
		return itAll("pass", "pass")(r, n)
	})
	h.RunImpactedTests(context.Background(), itSelected())
	if h.spent < 20*time.Millisecond || h.reserved != 0 {
		t.Fatalf("budget after the stage: spent %v reserved %v", h.spent, h.reserved)
	}
}

// The sub-cap: each run's timeout is at most what remains of 180 s, and no run
// starts once it is used up.
func TestImpactedTestsSubCap(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	var remaining time.Duration
	h.execute = func(ctx context.Context, _ string, _ []string, out io.Writer) execution {
		remaining = deadlineRemaining(t, ctx)
		return execution{}
	}
	run := &impactedRun{h: h, ctx: context.Background(), spent: impactedSubCap - 2*time.Second}
	if reason := run.stopReason(); reason != "" {
		t.Fatalf("stopped early: %s", reason)
	}
	h.mu.Lock()
	run.exec(model.CheckImpactedTestBase, h.base, []string{"go", "test", "./pkg"}, false)
	h.mu.Unlock()
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("run timeout %s, want at most the 2 s left of the sub-cap", remaining)
	}
	run.spent = impactedSubCap
	if reason := run.stopReason(); reason != "not run: the 180 s time limit of this stage was used up" {
		t.Fatalf("stop reason %q", reason)
	}
}

func TestImpactedTestsDeadlineStopsBeforeRunning(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := itExec(t, h, itAll("pass", "pass"))
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), ErrOverallDeadline)
	defer cancel()
	res := h.RunImpactedTests(ctx, itSelected())
	if len(*runs) != 0 || res.Status != model.ImpactTestsNotRun || !strings.Contains(res.Tests[0].Reason, deadlineText) {
		t.Fatalf("result %+v after %d runs", res, len(*runs))
	}
}
