package harness

// Impacted tests (F6b, --impacted-tests).
//
// Each selected test is an existing Go test function, declared in a test file
// that is byte-identical in the baseline and candidate snapshots, which the
// static impact index found reaching a changed function. The selected tests of
// one package directory (or of one test file, with a {file} template) run with
// one identical command on the baseline snapshot (check kind
// impacted_test_base) and on the candidate snapshot (impacted_test_candidate).
// Every test whose run pair was recorded gets one impacted_test_differential
// evidence record, classified by ClassifyExistingTest; report.Finalize
// re-derives the status from the recorded checks. The static link between a
// test and a changed function is approximate, so a failure is an outcome
// difference for a human to judge, never a reproduced issue. Unexported names
// carry an impacted prefix so that they never collide with another stage's
// file in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// impactedState is the per-harness state of the stage. The stage keeps no
// state between calls: every call plans and records its own runs.
type impactedState struct{}

const (
	// auditRunImpactedTests is the audit tool name of one unit of the stage.
	auditRunImpactedTests = model.AuditStagePrefix + "run_impacted_tests"
	// impactedSubCap is the stage's sub-cap inside the shared runtime budget
	// (§1.7.1): no run starts once the stage's runs have used it up, and each
	// run's timeout is at most what remains of it.
	impactedSubCap = 180 * time.Second
	// ImpactedMaxTests bounds the tests (distinct path and name) one review
	// runs.
	ImpactedMaxTests = 16
	// ImpactedMaxUnits bounds the units of one review: package directories
	// with a {package} template, test files with a {file} template.
	ImpactedMaxUnits = 4
	// impactedFileLimit bounds one test file compared and parsed on the host.
	// The static index does not read files above 2 MiB, so no selected test
	// comes from a larger file.
	impactedFileLimit = 4 << 20
	// impactedNothingRan is the section reason when no run of the stage started
	// because no selected test could run.
	impactedNothingRan = "no selected test could run; see the reason of each test"
	// impactedTemplateReason is the section reason for a generated_test
	// command that cannot establish which Go tests ran.
	impactedTemplateReason = "the generated_test command cannot establish which Go tests ran; configure it as go test {package}"
	// impactedPassedInsideFailure starts the reason of a test that passed in a
	// candidate run that failed as a whole and got no result from a pair of its
	// own.
	impactedPassedInsideFailure = "the test passed inside a candidate run that failed as a whole, which supports no result on its own"
)

// impactedNamePattern is what a selected name must look like: a Go test
// function identifier. It keeps -run expressions and reports unambiguous.
var impactedNamePattern = regexp.MustCompile(`^Test[\p{L}\p{N}_]*$`)

// ImpactedTests is the outcome of RunImpactedTests.
type ImpactedTests struct {
	// Status is model.ImpactTestsRan when at least one run started,
	// model.ImpactTestsNotRun when none did, and model.ImpactTestsNoCandidates
	// for an empty selection.
	Status string
	// Reason says why nothing ran; it is empty when a run started.
	Reason string
	// Tests are the input tests, in input order. A test that got a run pair
	// has an evidence ID and the status its evidence records (a reason only
	// when that status is UNVERIFIED); any other test has no status and a
	// reason that says why it got no run pair.
	Tests []model.ImpactTest
	// Capped counts the tests left out by the limits of ImpactedMaxTests
	// tests and ImpactedMaxUnits units per review.
	Capped int
	// Errors counts the stage's checks recorded as ERROR.
	Errors int
}

// RunImpactedTests runs each selected test on the baseline snapshot and on
// the candidate snapshot and records one impacted_test_differential evidence
// record per test (path and name) and recorded run pair. The input is copied;
// the result lists the same tests in the same order.
//
// Rules:
//   - The generated_test template must pass VerifiableGoTemplate, or nothing
//     runs and the stage is not_run.
//   - A test runs only when its name is a Go test name, its file is a regular
//     *_test.go file that is byte-identical in both snapshots, and the file
//     declares a top-level function of that name.
//   - Tests are grouped by package directory ({package}) or file ({file}), in
//     input order: at most ImpactedMaxTests tests and ImpactedMaxUnits units
//     per review; the rest is not run.
//   - A baseline run that does not pass as a whole gets no candidate run,
//     except that the tests it records as passed run once more on their own.
//   - When the candidate run fails as a whole, the tests it records as passed
//     get one more run pair on their own (a pass event inside a failed check
//     never supports PASSES_ON_CANDIDATE).
//   - A replayed baseline run never supports FAILS_ON_CANDIDATE: the baseline
//     runs again live with the same kind and command, and those tests are
//     classified against the live run (§1.11).
//   - Every run uses what remains of the 180 s sub-cap as its timeout ceiling
//     and, when a reviewer will run, MaxRuntime minus the reviewer reserve as
//     its budget limit (§1.7.1).
func (h *Harness) RunImpactedTests(ctx context.Context, selected []model.ImpactTest) ImpactedTests {
	h.mu.Lock()
	defer h.mu.Unlock()
	started := time.Now()
	tests := append([]model.ImpactTest{}, selected...)
	for i := range tests {
		tests[i].EvidenceID, tests[i].Status, tests[i].Reason = "", "", ""
	}
	result := ImpactedTests{Status: model.ImpactTestsNotRun, Tests: tests}
	if len(tests) == 0 {
		result.Status = model.ImpactTestsNoCandidates
		return result
	}
	if reason := h.impactedUnavailable(); reason != "" {
		result.Reason = reason
		for i := range tests {
			tests[i].Reason = "not run: " + reason
		}
		h.auditImpacted(started, map[string]any{"tests": len(tests), "reason": reason}, "SKIPPED")
		return result
	}
	units, capped := h.planImpactedUnits(tests)
	result.Capped = capped
	if len(units) == 0 {
		result.Reason = impactedNothingRan
		h.auditImpacted(started, map[string]any{"tests": len(tests), "reason": impactedNothingRan}, "SKIPPED")
		return result
	}
	run := &impactedRun{h: h, ctx: ctx, ceiling: h.impactedCeiling()}
	for n, u := range units {
		if reason := run.stopReason(); reason != "" {
			for _, rest := range units[n:] {
				for _, items := range rest.items {
					for _, i := range items {
						tests[i].Reason = reason
					}
				}
			}
			break
		}
		run.unit(u, tests)
	}
	result.Errors = run.errors
	switch {
	case run.executed:
		result.Status = model.ImpactTestsRan
	case run.skipped != "":
		result.Reason = "no run started: " + run.skipped
	default:
		result.Reason = impactedNothingRan
	}
	return result
}

// impactedUnavailable returns why the stage cannot run at all, or "".
func (h *Harness) impactedUnavailable() string {
	switch {
	case h.closed:
		return "the harness is closed"
	case h.base == "":
		return "no baseline snapshot is available"
	case !verifiableGoTemplate(h.opts.Commands["generated_test"]):
		return impactedTemplateReason
	}
	return ""
}

// impactedCeiling is the budget limit of the stage's runs: MaxRuntime minus
// the reviewer reserve when a reviewer will run, 0 (the whole budget)
// otherwise, and -1 when the reserve leaves nothing.
func (h *Harness) impactedCeiling() time.Duration {
	if h.opts.ReviewerReserve <= 0 {
		return 0
	}
	if c := h.opts.MaxRuntime - h.opts.ReviewerReserve; c > 0 {
		return c
	}
	return -1
}

// auditImpacted appends one stage:run_impacted_tests audit event. Caller
// holds h.mu.
func (h *Harness) auditImpacted(started time.Time, arguments map[string]any, status string) {
	b, _ := json.Marshal(arguments)
	h.audit = append(h.audit, model.AuditEvent{Time: started.UTC(), Tool: auditRunImpactedTests, Arguments: truncateUTF8(Redact(string(b)), 4096), Status: status, DurationMS: time.Since(started).Milliseconds()})
}

// --- units -------------------------------------------------------------------

// impactedKey identifies one test: its file and its name.
type impactedKey struct{ path, name string }

// impactedUnit is one baseline run and its candidate run: the tests of one
// package directory (or one test file with a {file} template).
type impactedUnit struct {
	key    string                // package directory ("." for the root) or test file
	target string                // path substituted into the generated_test template
	names  []string              // sorted, unique: the -run expression
	keys   []impactedKey         // the tests of the unit, in admission order
	items  map[impactedKey][]int // indexes of the input tests of each key
}

// impactedFile is what the precheck learned about one test file.
type impactedFile struct {
	reason string          // why no test of the file can run, or ""
	funcs  map[string]bool // top-level functions without receiver
}

// planImpactedUnits prechecks the tests, groups those that can run into units
// in input order within the limits, and records a reason on every other
// test. It returns the units and the number of tests the limits left out.
// Caller holds h.mu.
func (h *Harness) planImpactedUnits(tests []model.ImpactTest) ([]*impactedUnit, int) {
	byFile := impactedFileTemplate(h.opts.Commands["generated_test"])
	capReason := fmt.Sprintf("not run: the stage runs at most %d tests from at most %d packages per review", ImpactedMaxTests, ImpactedMaxUnits)
	if byFile {
		capReason = fmt.Sprintf("not run: the stage runs at most %d tests from at most %d test files per review", ImpactedMaxTests, ImpactedMaxUnits)
	}
	files := map[string]impactedFile{}
	index := map[string]*impactedUnit{}
	var units []*impactedUnit
	admitted := map[impactedKey]bool{}
	capped := 0
	for i, t := range tests {
		if reason := h.impactedPrecheck(t, files); reason != "" {
			tests[i].Reason = reason
			continue
		}
		key := path.Dir(t.Path)
		if byFile {
			key = t.Path
		}
		k := impactedKey{t.Path, t.Name}
		u := index[key]
		if !admitted[k] {
			if len(admitted) >= ImpactedMaxTests || u == nil && len(units) >= ImpactedMaxUnits {
				tests[i].Reason = capReason
				capped++
				continue
			}
			admitted[k] = true
		}
		if u == nil {
			u = &impactedUnit{key: key, target: t.Path, items: map[impactedKey][]int{}}
			index[key] = u
			units = append(units, u)
		}
		if _, seen := u.items[k]; !seen {
			u.keys = append(u.keys, k)
			if !impactedContains(u.names, t.Name) {
				u.names = append(u.names, t.Name)
			}
		}
		u.items[k] = append(u.items[k], i)
	}
	for _, u := range units {
		sort.Strings(u.names)
	}
	return units, capped
}

// impactedContains reports whether list holds s.
func impactedContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// impactedFileTemplate reports whether the generated_test template targets a
// file ({file}) rather than a package ({package}).
func impactedFileTemplate(command []string) bool {
	for _, arg := range command {
		switch arg {
		case "{file}":
			return true
		case "{package}":
			return false
		}
	}
	return false
}

// impactedPrecheck returns why a selected test cannot run, or "". files
// caches the file checks. Caller holds h.mu.
func (h *Harness) impactedPrecheck(t model.ImpactTest, files map[string]impactedFile) string {
	if !impactedNamePattern.MatchString(t.Name) || !isGoTestName(t.Name) || !strings.HasSuffix(t.Path, "_test.go") {
		return "not run: not a Go test function of a Go test file"
	}
	f, ok := files[t.Path]
	if !ok {
		f = h.impactedCheckFile(t.Path)
		files[t.Path] = f
	}
	switch {
	case f.reason != "":
		return f.reason
	case !f.funcs[t.Name]:
		return "not run: the test file declares no top-level function of this name"
	}
	return ""
}

// impactedCheckFile checks that the test file p is a regular file of at most
// impactedFileLimit bytes, byte-identical in both snapshots, and parses it.
// Caller holds h.mu.
func (h *Harness) impactedCheckFile(p string) impactedFile {
	basePath, baseErr := safePath(h.base, p)
	candidatePath, candidateErr := safePath(h.candidate, p)
	if baseErr != nil || candidateErr != nil {
		return impactedFile{reason: "not run: the test file is excluded from the sandbox snapshots (sensitive or unsafe path)"}
	}
	baseData, reason := impactedReadFile(basePath, "baseline")
	if reason != "" {
		return impactedFile{reason: reason}
	}
	candidateData, reason := impactedReadFile(candidatePath, "candidate")
	if reason != "" {
		return impactedFile{reason: reason}
	}
	if !bytes.Equal(baseData, candidateData) {
		return impactedFile{reason: "not run: the test file differs between the baseline and candidate snapshots (changed test files are the subject of --base-tests)"}
	}
	file, err := parser.ParseFile(token.NewFileSet(), p, baseData, parser.SkipObjectResolution)
	if err != nil {
		return impactedFile{reason: "not run: the test file could not be parsed as Go"}
	}
	funcs := map[string]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			funcs[fn.Name.Name] = true
		}
	}
	return impactedFile{funcs: funcs}
}

// impactedReadFile reads one regular file of at most impactedFileLimit bytes
// from the named snapshot, or returns why it cannot.
func impactedReadFile(p, snapshot string) ([]byte, string) {
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "not run: the test file is not in the " + snapshot + " snapshot"
	}
	if info.Size() > impactedFileLimit {
		return nil, fmt.Sprintf("not run: the test file is larger than %d MiB", impactedFileLimit>>20)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, "not run: the test file could not be read from the " + snapshot + " snapshot"
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, impactedFileLimit+1))
	if err != nil || len(data) > impactedFileLimit {
		return nil, "not run: the test file could not be read from the " + snapshot + " snapshot"
	}
	return data, ""
}

// --- runs --------------------------------------------------------------------

// impactedRun is the state of one stage execution: its sub-cap accounting,
// whether any run started, and the ERROR checks it recorded.
type impactedRun struct {
	h        *Harness
	ctx      context.Context
	ceiling  time.Duration
	spent    time.Duration // time of the stage's runs that were not replays
	executed bool          // at least one run was not SKIPPED
	skipped  string        // the recorded text of the first SKIPPED run
	errors   int           // runs recorded as ERROR
}

// stopReason returns why no further run may start, or "".
func (r *impactedRun) stopReason() string {
	switch err := r.ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return "not run: " + expiredText(r.ctx)
	case err != nil:
		return "not run: the review was cancelled"
	case r.ceiling < 0:
		return "not run: " + budgetReservedText
	case impactedSubCap-r.spent <= 0:
		return fmt.Sprintf("not run: the %d s time limit of this stage was used up", int(impactedSubCap/time.Second))
	}
	return ""
}

// exec records one run of the stage with what remains of the sub-cap as its
// timeout ceiling. Callers check stopReason first, so that timeout is positive.
func (r *impactedRun) exec(kind, dir string, command []string, live bool) model.Check {
	started := time.Now()
	c, _, _ := r.h.runWithOptions(r.ctx, kind, dir, command, runOptions{timeout: impactedSubCap - r.spent, ceiling: r.ceiling, live: live})
	if !c.Replayed() {
		r.spent += time.Since(started)
	}
	switch c.Status {
	case "SKIPPED":
		if r.skipped == "" {
			r.skipped = c.Output
		}
	case "ERROR":
		r.errors++
		r.executed = true
	default:
		r.executed = true
	}
	return c
}

// impactedVerdict is the classification of one test name and the run pair it
// rests on.
type impactedVerdict struct {
	status, reason  string
	base, candidate model.Check
}

// unit runs one unit and records evidence for every test that received a run
// pair; the others get their reason. When the candidate run failed as a
// whole, the tests it records as passed get one more run pair on their own: a
// pass event inside a failed check never supports PASSES_ON_CANDIDATE, but the
// same test passing in a pair of its own does. Caller holds h.mu.
func (r *impactedRun) unit(u *impactedUnit, tests []model.ImpactTest) {
	h := r.h
	started := time.Now()
	checks := []string{}
	status := "SKIPPED"
	defer func() {
		h.auditImpacted(started, map[string]any{"unit": u.key, "tests": len(u.keys), "checks": checks}, status)
	}()
	record := func(c model.Check) model.Check {
		checks = append(checks, c.ID)
		status = c.Status
		return c
	}
	verdicts, marks := r.pair(u, u.names, true, record)
	var retry []string
	for _, n := range u.names {
		v, ok := verdicts[n]
		if !ok || v.status != model.StatusUnverified || !impactedCompletedFail(v.candidate) {
			continue
		}
		if action, _ := GoTestOutcome(v.candidate.Output, n); action == "pass" {
			retry = append(retry, n)
		}
	}
	switch {
	case len(retry) == 0:
	case len(retry) == len(verdicts):
		// A pair of its own would repeat the failed run: it held only this
		// test, or every test in it passed, so the run failed for a reason
		// outside these tests (for example the exit code of TestMain).
		why := "no run pair of its own was attempted because the failed run held only this test"
		if len(verdicts) > 1 {
			why = "no run pair of its own was attempted because every test of the failed run passed in it"
		}
		for _, n := range retry {
			v := verdicts[n]
			v.reason = impactedPassedInsideFailure + "; " + why
			verdicts[n] = v
		}
	default:
		// A retry that gives no result keeps the first pair's evidence, with a
		// reason that says why no result was drawn.
		again, againMarks := map[string]impactedVerdict{}, map[string]string{}
		if reason := r.stopReason(); reason != "" {
			for _, n := range retry {
				againMarks[n] = reason
			}
		} else {
			again, againMarks = r.pair(u, retry, false, record)
		}
		for _, n := range retry {
			if v, ok := again[n]; ok {
				verdicts[n] = v
				continue
			}
			why := strings.TrimPrefix(againMarks[n], "not run: ")
			if why == "" {
				why = "no reason was recorded"
			}
			v := verdicts[n]
			v.reason = impactedPassedInsideFailure + "; its own run pair gave no result (" + why + ")"
			verdicts[n] = v
		}
	}
	for _, k := range u.keys {
		v, ok := verdicts[k.name]
		if !ok {
			reason := marks[k.name]
			if reason == "" {
				reason = "not run: the stage stopped before this test ran"
			}
			for _, i := range u.items[k] {
				tests[i].Reason = reason
			}
			continue
		}
		e := h.appendEvidence(model.Evidence{
			Kind:        model.EvidenceImpactedTestDifferential,
			Description: impactedDescription(tests[u.items[k][0]], u.key),
			Path:        k.path,
			CheckID:     v.candidate.ID,
			BaseCheckID: v.base.ID,
			Status:      v.status,
			Runner:      RunnerGo,
			TestNames:   []string{k.name},
		})
		reason := ""
		if v.status == model.StatusUnverified {
			reason = v.reason
		}
		for _, i := range u.items[k] {
			tests[i].EvidenceID, tests[i].Status, tests[i].Reason = e.ID, v.status, reason
		}
	}
}

// impactedCompletedFail reports whether c is a completed FAIL: exit code
// 1..124 and a complete log, the only failed run whose events can be read.
func impactedCompletedFail(c model.Check) bool {
	return c.Status == "FAIL" && c.ExitCode >= 1 && c.ExitCode <= 124 && !c.Truncated
}

// pair runs one baseline run for names and, when it passed, one candidate run
// with the identical command, and classifies each name. With narrowBase, a
// baseline run that failed as a whole is repeated once for the names it
// records as passed. A replayed baseline that would support
// FAILS_ON_CANDIDATE is repeated live (§1.11) and those names are classified
// against the live run. Names that got no candidate run are returned in marks
// with their reason. Caller holds h.mu.
func (r *impactedRun) pair(u *impactedUnit, names []string, narrowBase bool, record func(model.Check) model.Check) (map[string]impactedVerdict, map[string]string) {
	h := r.h
	verdicts, marks := map[string]impactedVerdict{}, map[string]string{}
	markAll := func(set []string, reason func(name string) string) {
		for _, n := range set {
			marks[n] = reason(n)
		}
	}
	if reason := r.stopReason(); reason != "" {
		markAll(names, func(string) string { return reason })
		return verdicts, marks
	}
	command := selectGoTests(h.testCommand(u.target), names)
	base := record(r.exec(model.CheckImpactedTestBase, h.base, command, false))
	if narrowBase && impactedCompletedFail(base) {
		var passed, other []string
		for _, n := range names {
			if action, _ := GoTestOutcome(base.Output, n); action == "pass" {
				passed = append(passed, n)
			} else {
				other = append(other, n)
			}
		}
		if len(passed) > 0 && len(other) > 0 {
			failed := base
			markAll(other, func(n string) string { return impactedBaselineReason(failed, n) })
			if reason := r.stopReason(); reason != "" {
				markAll(passed, func(string) string { return reason })
				return verdicts, marks
			}
			names = passed
			command = selectGoTests(h.testCommand(u.target), names)
			base = record(r.exec(model.CheckImpactedTestBase, h.base, command, false))
		}
	}
	if base.Status != "PASS" || base.ExitCode != 0 || base.Truncated {
		markAll(names, func(n string) string { return impactedBaselineReason(base, n) })
		return verdicts, marks
	}
	// A test that did not pass on the baseline can only end UNVERIFIED, so a
	// candidate run in which no selected test can get a result is not started.
	anyPassed := false
	for _, n := range names {
		if action, _ := GoTestOutcome(base.Output, n); action == "pass" {
			anyPassed = true
			break
		}
	}
	if !anyPassed {
		markAll(names, func(n string) string { return impactedBaselineReason(base, n) })
		return verdicts, marks
	}
	if reason := r.stopReason(); reason != "" {
		markAll(names, func(string) string { return reason })
		return verdicts, marks
	}
	candidate := record(r.exec(model.CheckImpactedTestCandidate, h.candidate, command, false))
	confirm := false
	for _, n := range names {
		s, reason := ClassifyExistingTest(base, candidate, n)
		verdicts[n] = impactedVerdict{s, reason, base, candidate}
		confirm = confirm || s == model.StatusFailsOnCandidate && base.Replayed()
	}
	if !confirm {
		return verdicts, marks
	}
	// §1.11: a replayed baseline never supports FAILS_ON_CANDIDATE. The
	// baseline runs again live with the same kind and command; the replayed
	// check stays in the ledger.
	reason := r.stopReason()
	var live model.Check
	if reason == "" {
		live = record(r.exec(model.CheckImpactedTestBase, h.base, command, true))
	}
	for _, n := range names {
		if verdicts[n].status != model.StatusFailsOnCandidate {
			continue
		}
		if reason != "" {
			verdicts[n] = impactedVerdict{model.StatusUnverified, "the baseline run was replayed from the execution cache and could not be repeated live (" + strings.TrimPrefix(reason, "not run: ") + ")", base, candidate}
			continue
		}
		s, why := ClassifyExistingTest(live, candidate, n)
		verdicts[n] = impactedVerdict{s, why, live, candidate}
	}
	return verdicts, marks
}

// impactedBaselineReason says why a test got no candidate run after the
// baseline run base.
func impactedBaselineReason(base model.Check, name string) string {
	const tail = ", so it was not run on candidate code"
	switch {
	case base.Status == "SKIPPED":
		return fmt.Sprintf("the baseline run %s did not start (%s)%s", base.ID, strings.TrimSuffix(strings.TrimSpace(base.Output), "."), tail)
	case base.Status == "TIMEOUT":
		return fmt.Sprintf("the baseline run %s timed out%s", base.ID, tail)
	case base.Status == "ERROR":
		return fmt.Sprintf("the baseline run %s did not complete (ERROR)%s", base.ID, tail)
	case base.Truncated:
		return fmt.Sprintf("the baseline log of %s was truncated (raise sandbox.max_output_bytes)%s", base.ID, tail)
	}
	switch action, _ := GoTestOutcome(base.Output, name); action {
	case "fail":
		return fmt.Sprintf("the test failed on the baseline (%s)%s", base.ID, tail)
	case "skip":
		return fmt.Sprintf("the test was skipped on the baseline (%s)%s", base.ID, tail)
	case "pass":
		return fmt.Sprintf("the baseline run %s failed although this test passed in it%s", base.ID, tail)
	}
	if base.Status == "FAIL" && goBuildFailure(base.Output) {
		return fmt.Sprintf("the baseline package did not build or set up in %s%s", base.ID, tail)
	}
	return fmt.Sprintf("the baseline log of %s does not record exactly one run and one result of this test (for example a build constraint excluded its file)%s", base.ID, tail)
}

// impactedDescription is the evidence description of one test. It states
// the static link the index found and nothing about causes.
func impactedDescription(t model.ImpactTest, unit string) string {
	where := "package directory " + unit
	switch {
	case unit == ".":
		where = "the package at the repository root"
	case strings.HasSuffix(unit, "_test.go"):
		where = "test file " + unit
	}
	reach := ""
	if t.Depth > 0 {
		reach = fmt.Sprintf(" The static index found it reaching a changed function at depth %d (%s resolution, approximate).", t.Depth, t.Resolution)
	}
	return truncateUTF8(Redact(fmt.Sprintf("Existing test %s from %s, a file the change did not modify, run on the baseline and on the candidate with the selected tests of %s.%s", t.Name, t.Path, where, reach)), 1024)
}
