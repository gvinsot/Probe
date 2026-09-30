package harness

// Per-name outcomes of existing TypeScript and JavaScript tests, read from the
// Jest-compatible JSON report that a verifiable Vitest or Jest template writes
// to {results_out}. They mirror GoTestOutcome and ClassifyExistingTest for
// the stages that run existing tests (impacted tests, changed baseline tests,
// mutation).
//
// A test is named as the static index names it: its describe titles and its
// own title, joined by " > ", each title normalized as the index reads a
// string literal (see symbols.tsTitle). The report is matched on that name
// and on the exact file, and only a name with exactly one result in exactly
// one file entry has an outcome.

import (
	"context"
	"encoding/json"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/model"
)

// JSTestNameSeparator joins the describe titles and the title of a test.
const JSTestNameSeparator = " > "

// jsTitleLimit is the byte bound of one normalized title, the static index's.
const jsTitleLimit = 200

// jsTestNameLimit bounds a whole test name the stages accept.
const jsTestNameLimit = 2048

// NormalizeJSTitle normalizes one runtime title the way the static index
// normalizes the string literal it was written as: the characters the index
// trims off a literal are trimmed, runs of white space become one space, the
// result is cut at 200 bytes, and an empty title reads "(untitled)". A title
// whose literal held escapes or interpolation normalizes differently from the
// runtime title, so its test has no outcome rather than another test's.
func NormalizeJSTitle(title string) string {
	s := strings.Join(strings.Fields(strings.Trim(title, "'\"`${")), " ")
	if len(s) > jsTitleLimit {
		s = s[:jsTitleLimit]
	}
	if s == "" {
		s = "(untitled)"
	}
	return s
}

// JSTestName is the name of a report result: its normalized ancestor titles
// and title joined by JSTestNameSeparator.
func JSTestName(ancestors []string, title string) string {
	parts := make([]string, 0, len(ancestors)+1)
	for _, a := range ancestors {
		parts = append(parts, NormalizeJSTitle(a))
	}
	return strings.Join(append(parts, NormalizeJSTitle(title)), JSTestNameSeparator)
}

// ValidJSTestName reports whether a selected name can be run and matched:
// valid UTF-8, bounded, without control characters, and with no empty title.
func ValidJSTestName(name string) bool {
	if name == "" || len(name) > jsTestNameLimit || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(name, JSTestNameSeparator) {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

// ScriptTestPath reports whether a repository path is a TypeScript or
// JavaScript test file by the conventions the static index uses: *.test.*,
// *.spec.* or a file under __tests__/, outside node_modules.
func ScriptTestPath(p string) bool {
	lower := strings.ToLower(p)
	base := path.Base(lower)
	switch path.Ext(base) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
	default:
		return false
	}
	if strings.HasSuffix(strings.TrimSuffix(base, path.Ext(base)), ".d") {
		return false
	}
	slashed := "/" + lower
	if strings.Contains(slashed, "/node_modules/") {
		return false
	}
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains(slashed, "/__tests__/")
}

// JestTestOutcome returns "pass", "fail" or "skip" for the one result named
// name in the report entry of exactly the file p, and "" when the report is
// unreadable, holds no entry or several entries for the file, or holds no
// result or several results of that name in it. Jest's "pending", "todo" and
// "disabled" and Vitest's "skipped" are "skip".
func JestTestOutcome(results, p, name string) string {
	var report jestReport
	if results == "" || json.Unmarshal([]byte(results), &report) != nil {
		return ""
	}
	want := "/workspace/" + filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	var file *jestFile
	for i := range report.TestResults {
		if report.TestResults[i].Name == want {
			if file != nil {
				return ""
			}
			file = &report.TestResults[i]
		}
	}
	if file == nil {
		return ""
	}
	outcome, found := "", 0
	for _, a := range file.AssertionResults {
		if JSTestName(a.AncestorTitles, a.Title) != name {
			continue
		}
		found++
		switch a.Status {
		case "passed":
			outcome = "pass"
		case "failed":
			outcome = "fail"
		case "pending", "skipped", "todo", "disabled":
			outcome = "skip"
		default:
			outcome = ""
		}
	}
	if found != 1 {
		return ""
	}
	return outcome
}

// Reason texts specific to Jest-compatible reports; the others are shared
// with ClassifyExistingTest.
const (
	reasonJestBaselineOutcome  = "the baseline report does not record exactly one passed result of this test in the report entry of its file (for example the test was skipped, or its title is computed at run time)"
	reasonJestCandidateOutcome = "the candidate-side report does not record exactly one result of this test in the report entry of its file"
	reasonJestCandidateSuite   = "the candidate-side test file did not load or set up, so the test did not run"
)

// ClassifyExistingJestTest is ClassifyExistingTest for a test of the file p
// run by a verifiable Vitest or Jest template: the outcomes come from the
// recorded JSON reports (Check.Results) instead of go test -json events.
// Rules (all must hold, else UNVERIFIED):
//   - base.Command and candidate.Command are equal and non-empty;
//   - baseline: Status PASS, ExitCode 0, !Truncated, JestTestOutcome == "pass";
//   - FAILS:  candidate Status FAIL, ExitCode 1..124, !Truncated, JestTestOutcome == "fail";
//   - PASSES: candidate Status PASS, ExitCode 0, !Truncated, JestTestOutcome == "pass".
//
// It does not look at Replayed(): the live-baseline rule (§1.11) belongs to
// callers and verifiers.
func ClassifyExistingJestTest(base, candidate model.Check, p, name string) (status, reason string) {
	if len(base.Command) == 0 || !equalStrings(base.Command, candidate.Command) {
		return model.StatusUnverified, reasonCommandMismatch
	}
	if base.Status != "PASS" || base.ExitCode != 0 || base.Truncated {
		return model.StatusUnverified, reasonBaselineNotPassed
	}
	if JestTestOutcome(base.Results, p, name) != "pass" {
		return model.StatusUnverified, reasonJestBaselineOutcome
	}
	switch candidate.Status {
	case "PASS", "FAIL":
	case "TIMEOUT":
		return model.StatusUnverified, reasonCandidateTimeout
	default:
		return model.StatusUnverified, reasonCandidateIncomplete
	}
	if candidate.Truncated {
		return model.StatusUnverified, reasonCandidateTrunc
	}
	action := JestTestOutcome(candidate.Results, p, name)
	switch candidate.Status {
	case "FAIL":
		if candidate.ExitCode < 1 || candidate.ExitCode > 124 {
			return model.StatusUnverified, reasonCandidateExit
		}
		if action == "fail" {
			return model.StatusFailsOnCandidate, reasonFailsOnCandidate
		}
		if jestSuiteFailure(candidate.Results, p) {
			return model.StatusUnverified, reasonJestCandidateSuite
		}
		return model.StatusUnverified, reasonJestCandidateOutcome
	default: // PASS
		if candidate.ExitCode != 0 {
			return model.StatusUnverified, reasonCandidateExit
		}
		if action == "pass" {
			return model.StatusPassesOnCandidate, reasonPassesOnCandidate
		}
		return model.StatusUnverified, reasonJestCandidateOutcome
	}
}

// jestSuiteFailure reports whether the report entry of p failed without any
// test result, the shape of a file that did not load. It only selects a
// reason text.
func jestSuiteFailure(results, p string) bool {
	var report jestReport
	if json.Unmarshal([]byte(results), &report) != nil {
		return false
	}
	want := "/workspace/" + filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	for _, f := range report.TestResults {
		if f.Name == want && f.Status == "failed" && len(f.AssertionResults) == 0 {
			return true
		}
	}
	return false
}

// selectJSTests appends a Vitest and Jest -t filter that selects the named
// tests of the one file the command targets. Both runners match it against
// the describe titles and the title joined by single spaces (Jest ignores
// case); each normalized title matches its runtime title up to white space.
// The filter only narrows the run: every outcome is still read from the
// report by exact name, so a filter that selects more tests, or none, can
// never give a test another test's result.
func selectJSTests(command, names []string) []string {
	alternatives := make([]string, len(names))
	for i, name := range names {
		parts := strings.Split(name, JSTestNameSeparator)
		for j, part := range parts {
			parts[j] = strings.ReplaceAll(regexp.QuoteMeta(part), " ", `\s+`)
		}
		alternatives[i] = strings.Join(parts, `\s+`)
	}
	return append(command, "-t", "^(?:"+strings.Join(alternatives, "|")+")$")
}

// existingRunner is the verifier of a stage that runs existing tests:
// RunnerGo (also when empty) for go test events, RunnerJest for the JSON
// report of a Vitest or Jest run.
type existingRunner string

// existingRunnerFor returns the verifier the generated_test template
// supports, or "" when it supports none.
func existingRunnerFor(command []string) existingRunner {
	switch {
	case verifiableGoTemplate(command):
		return RunnerGo
	case verifiableJSTemplate(command):
		return RunnerJest
	}
	return ""
}

// jest reports whether the runner reads Jest-compatible JSON reports.
func (x existingRunner) jest() bool { return x == RunnerJest }

// evidenceRunner is the verifier recorded on evidence.
func (x existingRunner) evidenceRunner() string {
	if x.jest() {
		return RunnerJest
	}
	return RunnerGo
}

// outcome is the terminal action ("pass", "fail", "skip" or "") the check
// records for the test name of the file p.
func (x existingRunner) outcome(c model.Check, p, name string) string {
	if x.jest() {
		return JestTestOutcome(c.Results, p, name)
	}
	action, _ := GoTestOutcome(c.Output, name)
	return action
}

// classify is ClassifyExistingTest or ClassifyExistingJestTest.
func (x existingRunner) classify(base, candidate model.Check, p, name string) (string, string) {
	if x.jest() {
		return ClassifyExistingJestTest(base, candidate, p, name)
	}
	return ClassifyExistingTest(base, candidate, name)
}

// command is the generated_test template for target with a filter that
// selects names: -run for go test, -t for Vitest and Jest.
func (x existingRunner) command(h *Harness, target string, names []string) []string {
	if x.jest() {
		return selectJSTests(h.testCommand(target), names)
	}
	return selectGoTests(h.testCommand(target), names)
}

// run records one run; a Vitest or Jest run captures its JSON report, the
// only record of which tests ran. Caller holds h.mu.
func (x existingRunner) run(h *Harness, ctx context.Context, kind, dir string, command []string, o runOptions) model.Check {
	if x.jest() {
		return h.runWithResultsOptions(ctx, kind, dir, command, o)
	}
	c, _, _ := h.runWithOptions(ctx, kind, dir, command, o)
	return c
}

// suiteFailure reports whether a failed run did not load or build the tests
// of p at all. It only selects a reason text.
func (x existingRunner) suiteFailure(c model.Check, p string) bool {
	if x.jest() {
		return jestSuiteFailure(c.Results, p)
	}
	return goBuildFailure(c.Output)
}
