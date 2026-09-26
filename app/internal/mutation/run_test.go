package mutation_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/mutation"
)

const discount = `package price

import "errors"

func Discount(total int) (int, error) {
	if total < 0 {
		return 0, ErrNegative
	}
	if total >= 100 {
		return total - 10, nil
	}
	return total, nil
}

// ErrNegative is returned for a negative total.
var ErrNegative = errors.New("negative total")
`

// goLog renders a go test -json log of one package: each named test runs and
// ends with its action, then the package ends with pass unless a test failed.
func goLog(pkg string, tests ...[2]string) string {
	var b strings.Builder
	result := "pass"
	fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", pkg)
	for _, tc := range tests {
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", pkg, tc[0])
		fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Test":%q,"Output":"=== RUN   %s\n"}`+"\n", pkg, tc[0], tc[0])
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`+"\n", tc[1], pkg, tc[0])
		if tc[1] == "fail" {
			result = "fail"
		}
	}
	fmt.Fprintf(&b, `{"Action":%q,"Package":%q}`+"\n", result, pkg)
	return b.String()
}

// fakeWorkspace records runs like the harness: IDs mutation-check-N, kinds,
// commands and durations. Outcomes are scripted from the mutated source.
type fakeWorkspace struct {
	files       map[string]string
	checks      []model.Check
	patches     map[string][]byte
	duration    time.Duration
	timeouts    []time.Duration
	control     func(pkg string) (status string, exit int, output string)
	survives    func(mutated string) bool
	mutantState func(id string) (status string, exit int, output string, ok bool)
	patchErr    error
	restoreFail string // mutant ID whose restore fails
	aborted     bool
}

func newFake() *fakeWorkspace {
	return &fakeWorkspace{
		files:    map[string]string{"price/price.go": discount, "price/price_test.go": "package price\n"},
		patches:  map[string][]byte{},
		duration: 2 * time.Second,
		control: func(pkg string) (string, int, string) {
			return "PASS", 0, goLog("example.test/shop/price", [2]string{"TestClamp", "pass"}, [2]string{"TestDiscount", "pass"})
		},
		// The Discount tests assert only Discount(200) == 190 and an error
		// for Discount(-5): boundary and increment mutants survive.
		survives: func(mutated string) bool {
			for _, s := range []string{"total <= 0", "total < (0+1)", "total > 100", "total >= (100+1)"} {
				if strings.Contains(mutated, s) {
					return true
				}
			}
			return false
		},
	}
}

func (f *fakeWorkspace) ReadSource(rel string) ([]byte, error) {
	if s, ok := f.files[rel]; ok {
		return []byte(s), nil
	}
	return nil, errors.New("missing")
}

func (f *fakeWorkspace) HasTestFile(dir string) (bool, error) {
	for name := range f.files {
		if path.Dir(name) == dir && strings.HasSuffix(name, "_test.go") {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeWorkspace) record(kind, status string, exit int, output string, command []string) model.Check {
	c := model.Check{ID: fmt.Sprintf("mutation-check-%d", len(f.checks)+1), Kind: kind, Status: status, ExitCode: exit, Command: append([]string(nil), command...), Output: output, DurationMS: f.duration.Milliseconds()}
	if status == "SKIPPED" {
		c.DurationMS = 0
	}
	f.checks = append(f.checks, c)
	return c
}

func (f *fakeWorkspace) RunControl(_ context.Context, pkg string, command []string, timeout time.Duration) (model.Check, error) {
	if f.aborted {
		return model.Check{}, errors.New("aborted")
	}
	f.timeouts = append(f.timeouts, timeout)
	status, exit, output := f.control(pkg)
	return f.record(model.CheckMutationControl, status, exit, output, command), nil
}

func (f *fakeWorkspace) RunMutant(_ context.Context, id, rel string, original, mutated []byte, command []string, timeout time.Duration) (model.Check, error) {
	if f.aborted {
		return model.Check{}, errors.New("aborted")
	}
	if string(original) != f.files[rel] {
		f.aborted = true
		return model.Check{}, errors.New("stale")
	}
	f.timeouts = append(f.timeouts, timeout)
	if f.mutantState != nil {
		if status, exit, output, ok := f.mutantState(id); ok {
			c := f.record(model.CheckMutant, status, exit, output, command)
			if id == f.restoreFail {
				f.aborted = true
				return c, errors.New("restore failed")
			}
			return c, nil
		}
	}
	var c model.Check
	if f.survives(string(mutated)) {
		c = f.record(model.CheckMutant, "PASS", 0, goLog("example.test/shop/price", [2]string{"TestClamp", "pass"}, [2]string{"TestDiscount", "pass"}), command)
	} else {
		c = f.record(model.CheckMutant, "FAIL", 1, goLog("example.test/shop/price", [2]string{"TestClamp", "pass"}, [2]string{"TestDiscount", "fail"}), command)
	}
	if id == f.restoreFail {
		f.aborted = true
		return c, errors.New("restore failed")
	}
	return c, nil
}

func (f *fakeWorkspace) SavePatch(name string, data []byte) (string, error) {
	if f.patchErr != nil {
		return "", f.patchErr
	}
	f.patches[name] = data
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

var policyCommand = []string{"go", "test", "-json", "-count=1", "-failfast", "{package}"}

func run(t *testing.T, f *fakeWorkspace, maxMutants, maxRuntime int) mutation.Result {
	t.Helper()
	return mutation.Run(context.Background(), f, mutation.Config{Command: policyCommand, Limits: model.MutationLimits{MaxMutants: maxMutants, TimeoutSeconds: 60, MaxRuntimeSeconds: maxRuntime}, Outcome: harness.GoTestOutcome}, discountChange())
}

func discountChange() model.Change {
	f := model.ChangedFile{Path: "price/price.go", Status: "M"}
	h := model.Hunk{}
	for line := 5; line <= 13; line++ {
		h.Lines = append(h.Lines, model.DiffLine{Kind: "add", NewLine: line})
	}
	f.Hunks = []model.Hunk{h}
	return model.Change{Files: []model.ChangedFile{f, {Path: "price/fast_windows.go", Status: "A", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "add", NewLine: 1}}}}}}}
}

// forbidden are words and marks no mutation output may use about its results.
var forbidden = regexp.MustCompile(`(?i)\b(tested|verified|covered|safe|correct|score|complete)\b|%`)

func TestDiscountKilledAndSurvivors(t *testing.T) {
	f := newFake()
	res := run(t, f, 10, 400)
	m := res.Section
	if m.Status != model.MutationRan || m.Generated != 8 || len(m.Mutants) != 8 || m.Dropped != 0 {
		t.Fatalf("section %+v", m)
	}
	if m.Killed != 4 || m.Survived != 4 || m.Invalid+m.TimedOut+m.Inconclusive+m.NotRun != 0 {
		t.Fatalf("counts killed %d survived %d", m.Killed, m.Survived)
	}
	if len(f.checks) != 9 || f.checks[0].Kind != model.CheckMutationControl || f.checks[0].Command[5] != "./price" {
		t.Fatalf("runs %+v", f.checks)
	}
	if res.Unverified != "" || res.Operational {
		t.Fatalf("unverified %q operational %v", res.Unverified, res.Operational)
	}
	survivors := map[string]bool{}
	for i, mu := range m.Mutants {
		if mu.ID != fmt.Sprintf("mutant-%d", i+1) || mu.Package != "./price" || mu.ControlCheckID != "mutation-check-1" || mu.Symbol != "Discount" {
			t.Fatalf("mutant %+v", mu)
		}
		switch mu.Status {
		case model.MutantSurvived:
			survivors[mu.Operator+":"+mu.Original] = true
			patch, ok := f.patches[mu.ID+".patch"]
			sum := sha256.Sum256(patch)
			if !ok || hex.EncodeToString(sum[:]) != mu.PatchSHA256 || mu.TestsRun != 2 || len(mu.FailedTests) != 0 {
				t.Fatalf("survivor %+v", mu)
			}
			if !strings.Contains(string(patch), "--- a/price/price.go\n+++ b/price/price.go\n@@ -"+fmt.Sprint(mu.Line)+",1 +") || !strings.Contains(string(patch), "Command: go test -json -count=1 -failfast ./price") {
				t.Fatalf("patch:\n%s", patch)
			}
		case model.MutantKilled:
			if len(mu.FailedTests) != 1 || mu.FailedTests[0] != "TestDiscount" || mu.PatchSHA256 != "" {
				t.Fatalf("killed %+v", mu)
			}
		default:
			t.Fatalf("unexpected %+v", mu)
		}
	}
	for _, want := range []string{"boundary:<", "increment_constant:0", "boundary:>=", "increment_constant:100"} {
		if !survivors[want] {
			t.Fatalf("missing survivor %s in %v", want, survivors)
		}
	}
	if len(res.Signals) != 4 {
		t.Fatalf("%d signals", len(res.Signals))
	}
	for _, s := range res.Signals {
		if s.Kind != model.SignalSurvivingMutant || s.Severity != "medium" || s.Side != "new" || (s.Line != 6 && s.Line != 9) || s.Symbol != "Discount" {
			t.Fatalf("signal %+v", s)
		}
		if forbidden.MatchString(s.Summary + " " + s.Evidence) {
			t.Fatalf("forbidden wording in %q / %q", s.Summary, s.Evidence)
		}
		if !strings.Contains(s.Evidence, "semantically equivalent") || !strings.Contains(s.Evidence, "not evidence of a defect") {
			t.Fatalf("evidence lacks the caveat: %q", s.Evidence)
		}
	}
	files := map[string]string{}
	for _, file := range m.Files {
		files[file.Path] = file.Status
	}
	if files["price/fast_windows.go"] != model.MutationFileSkipped || files["price/price.go"] != model.MutationFileEligible {
		t.Fatalf("files %+v", m.Files)
	}
	// The note may state the explicit negation "There is no mutation score."
	if forbidden.MatchString(strings.ReplaceAll(model.MutationNote, "There is no mutation score.", "")) {
		t.Fatalf("forbidden wording in the note")
	}
	// Determinism: the same plan gives the same IDs, operators and statuses.
	again := run(t, newFake(), 10, 400).Section
	for i := range m.Mutants {
		a, b := m.Mutants[i], again.Mutants[i]
		if a.ID != b.ID || a.Operator != b.Operator || a.Status != b.Status || a.Line != b.Line || a.Column != b.Column {
			t.Fatalf("run %d differs: %+v vs %+v", i, a, b)
		}
	}
}

func TestMaxMutantsDroppedIsIncomplete(t *testing.T) {
	res := run(t, newFake(), 3, 400)
	m := res.Section
	if m.Status != model.MutationIncomplete || m.Dropped != 5 || len(m.Mutants) != 3 {
		t.Fatalf("section %+v", m)
	}
	if !strings.HasPrefix(res.Unverified, "Mutation analysis is incomplete: 5 of 8 candidate mutants were not run because of max_mutants (3)") {
		t.Fatalf("unverified %q", res.Unverified)
	}
}

func TestControlFailureMarksPackageNotRun(t *testing.T) {
	f := newFake()
	f.control = func(string) (string, int, string) {
		return "FAIL", 1, goLog("example.test/shop/price", [2]string{"TestClamp", "fail"})
	}
	res := run(t, f, 10, 400)
	m := res.Section
	if m.Status != model.MutationIncomplete || m.NotRun != 8 || len(f.checks) != 1 {
		t.Fatalf("section %+v, %d runs", m, len(f.checks))
	}
	for _, mu := range m.Mutants {
		if mu.Status != model.MutantNotRun || mu.ControlCheckID != "mutation-check-1" || !strings.Contains(mu.Reason, "did not pass") {
			t.Fatalf("mutant %+v", mu)
		}
	}
	if res.Unverified != "Mutation analysis is incomplete: 8 of 8 selected mutants have no outcome (8 not run, 0 inconclusive)" || len(res.Signals) != 0 || res.Operational {
		t.Fatalf("unverified %q signals %d operational %v", res.Unverified, len(res.Signals), res.Operational)
	}
}

// The sub-cap stops launching when the remaining time is below the control's
// duration, and a mutant's timeout never exceeds three control durations plus
// the slack, the policy timeout or the remaining sub-cap.
func TestRuntimeCapStops(t *testing.T) {
	f := newFake()
	f.duration = 20 * time.Second
	res := run(t, f, 10, 70) // control 20 s, then 2 mutants (40 s), then 10 s < 20 s remain
	m := res.Section
	if len(f.checks) != 3 || m.NotRun != 6 || m.Status != model.MutationIncomplete {
		t.Fatalf("%d runs, section %+v", len(f.checks), m)
	}
	if m.Mutants[2].Reason != "mutation.max_runtime_seconds does not leave room for another run of this package" {
		t.Fatalf("reason %q", m.Mutants[2].Reason)
	}
	if f.timeouts[0] != 60*time.Second || f.timeouts[1] != 50*time.Second || f.timeouts[2] != 30*time.Second {
		t.Fatalf("timeouts %v", f.timeouts)
	}
	f = newFake()
	f.duration = time.Second
	run(t, f, 10, 400)
	for _, timeout := range f.timeouts[1:] {
		if timeout != 33*time.Second {
			t.Fatalf("mutant timeout %v, want 3 x 1 s + 30 s", timeout)
		}
	}
}

// A run the harness did not execute (budget, reviewer reserve, deadline) stops
// the stage; with nothing executed the section is not_run.
func TestSharedBudgetSkippedStops(t *testing.T) {
	f := newFake()
	f.control = func(string) (string, int, string) { return "SKIPPED", -1, "Sandbox runtime budget exhausted." }
	res := run(t, f, 10, 400)
	m := res.Section
	if m.Status != model.MutationNotRun || m.NotRun != 8 || res.Unverified != "Mutation analysis did not run: the control run mutation-check-1 was not executed: Sandbox runtime budget exhausted." {
		t.Fatalf("section %+v unverified %q", m, res.Unverified)
	}
	f = newFake()
	f.mutantState = func(id string) (string, int, string, bool) {
		if id == "mutant-3" {
			return "SKIPPED", -1, "Sandbox runtime reserved for reviewer experiments.", true
		}
		return "", 0, "", false
	}
	res = run(t, f, 10, 400)
	if res.Section.Status != model.MutationIncomplete || res.Section.NotRun != 6 || len(f.checks) != 4 {
		t.Fatalf("section %+v, %d runs", res.Section, len(f.checks))
	}
}

func TestRestoreFailureAbortsAndIsOperational(t *testing.T) {
	f := newFake()
	f.restoreFail = "mutant-2"
	res := run(t, f, 10, 400)
	m := res.Section
	if !res.Operational || m.Status != model.MutationIncomplete || len(f.checks) != 3 {
		t.Fatalf("operational %v section %+v runs %d", res.Operational, m, len(f.checks))
	}
	if m.Mutants[1].Status != model.MutantInconclusive || m.Mutants[1].CheckID != "mutation-check-3" {
		t.Fatalf("aborted mutant %+v", m.Mutants[1])
	}
	for _, mu := range m.Mutants[2:] {
		if mu.Status != model.MutantNotRun || !strings.Contains(mu.Reason, "could not be verified or restored") {
			t.Fatalf("mutant after abort %+v", mu)
		}
	}
}

func TestInfraErrorIsOperational(t *testing.T) {
	f := newFake()
	f.mutantState = func(id string) (string, int, string, bool) {
		if id == "mutant-1" {
			return "ERROR", 125, "docker: Error response from daemon", true
		}
		return "", 0, "", false
	}
	res := run(t, f, 10, 400)
	if !res.Operational || res.Section.Mutants[0].Status != model.MutantInconclusive || res.Section.Status != model.MutationIncomplete {
		t.Fatalf("operational %v section %+v", res.Operational, res.Section)
	}
	// A control with an infrastructure error is operational too; when nothing
	// else ran, the sandbox never ran the command and the stage is not_run.
	f = newFake()
	f.control = func(string) (string, int, string) { return "ERROR", 125, "Unable to find image" }
	res = run(t, f, 10, 400)
	if !res.Operational || res.Section.Status != model.MutationNotRun || res.Section.NotRun != 8 {
		t.Fatalf("control error: %+v", res)
	}
	if res.Unverified != "Mutation analysis did not run: the unmutated control run mutation-check-1 did not pass (status ERROR, exit code 125)" {
		t.Fatalf("unverified %q", res.Unverified)
	}
}

// A run that the review's time limit ended is no TIMEOUT outcome of its
// mutant: it is INCONCLUSIVE, and nothing runs after it.
func TestDeadlineDuringMutantIsInconclusive(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.mutantState = func(id string) (string, int, string, bool) {
		if id == "mutant-2" {
			cancel()
			return "TIMEOUT", -1, "", true
		}
		return "", 0, "", false
	}
	res := mutation.Run(ctx, f, mutation.Config{Command: policyCommand, Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 400}, Outcome: harness.GoTestOutcome}, discountChange())
	m := res.Section
	if m.TimedOut != 0 || m.Mutants[1].Status != model.MutantInconclusive || !strings.Contains(m.Mutants[1].Reason, "stopped this run") {
		t.Fatalf("mutant %+v", m.Mutants[1])
	}
	if len(f.checks) != 3 || m.NotRun != 6 || m.Status != model.MutationIncomplete || res.Operational {
		t.Fatalf("%d runs, section %+v", len(f.checks), m)
	}
}

func TestPatchArtifactFailureDowngrades(t *testing.T) {
	f := newFake()
	f.patchErr = errors.New("disk full")
	res := run(t, f, 10, 400)
	m := res.Section
	if m.Survived != 0 || m.Inconclusive != 4 || len(res.Signals) != 0 || m.Status != model.MutationIncomplete {
		t.Fatalf("section %+v signals %d", m, len(res.Signals))
	}
	for _, mu := range m.Mutants {
		if mu.Status == model.MutantInconclusive && (mu.TestsRun != 0 || mu.PatchSHA256 != "" || !strings.Contains(mu.Reason, "patch artifact")) {
			t.Fatalf("mutant %+v", mu)
		}
	}
}

func TestNoCandidates(t *testing.T) {
	f := newFake()
	delete(f.files, "price/price_test.go")
	res := run(t, f, 10, 400)
	if res.Section.Status != model.MutationNoCandidates || res.Unverified != "" || len(f.checks) != 0 || res.Operational {
		t.Fatalf("section %+v runs %d", res.Section, len(f.checks))
	}
	if res.Section.Mutants == nil || res.Section.Files == nil || res.Signals == nil {
		t.Fatal("nil slices in a no_candidates result")
	}
	f = newFake()
	cov := mutation.Run(context.Background(), f, mutation.Config{Command: policyCommand, Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 400}, Outcome: harness.GoTestOutcome, NotExecuted: func(string) []int { return []int{6, 7, 9, 10} }}, discountChange())
	if cov.Section.Status != model.MutationNoCandidates || cov.Section.CoverageSkipped != 8 || !strings.Contains(cov.Section.Reason, "coverage run did not execute") {
		t.Fatalf("coverage-only section %+v", cov.Section)
	}
}

func TestCancelledContextRunsNothing(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := mutation.Run(ctx, f, mutation.Config{Command: policyCommand, Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 400}, Outcome: harness.GoTestOutcome}, discountChange())
	if len(f.checks) != 0 || res.Section.Status != model.MutationNotRun || res.Section.NotRun != 8 {
		t.Fatalf("section %+v runs %d", res.Section, len(f.checks))
	}
}

func TestCountersMatchMutants(t *testing.T) {
	f := newFake()
	f.mutantState = func(id string) (string, int, string, bool) {
		switch id {
		case "mutant-1":
			return "TIMEOUT", -1, "", true
		case "mutant-2":
			return "FAIL", 1, `{"Action":"build-fail"}` + "\n", true
		}
		return "", 0, "", false
	}
	m := run(t, f, 10, 400).Section
	counts := map[string]int{}
	for _, mu := range m.Mutants {
		counts[mu.Status]++
	}
	if counts[model.MutantTimeout] != m.TimedOut || counts[model.MutantInvalid] != m.Invalid || counts[model.MutantKilled] != m.Killed || counts[model.MutantSurvived] != m.Survived || m.TimedOut != 1 || m.Invalid != 1 {
		t.Fatalf("counts %v vs %+v", counts, m)
	}
	// TIMEOUT and INVALID are outcomes: the section still ran.
	if m.Status != model.MutationRan {
		t.Fatalf("status %s", m.Status)
	}
}
