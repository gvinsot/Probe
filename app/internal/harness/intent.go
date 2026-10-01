package harness

// Candidate-only intent tests (F5). A reviewer may write a test for one
// acceptance criterion extracted from the intent and run it on the candidate
// snapshot only. There is no baseline control: the harness records what the
// run showed as intent_test evidence, and report.Finalize re-derives the status
// from the recorded check with the same function (IntentOutcome).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/acceptance"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
	symbolindex "github.com/gvinsot/Probe/app/internal/symbols"
)

// Reviewer tool names of the intent tests.
const IntentCreateTool = "create_intent_test"
const IntentRunTool = "run_intent_test"

// IsIntentTool reports whether name is one of the intent-test tools, which a
// reviewer is offered only when the run has acceptance criteria.
func IsIntentTool(name string) bool { return name == IntentCreateTool || name == IntentRunTool }

// MaxReferencedSymbols bounds Evidence.ReferencedSymbols (schema maxItems).
const MaxReferencedSymbols = 32

// maxSymbolFiles bounds the changed files read to find changed declarations.
const maxSymbolFiles = 2000

// intentState is the harness state of the intent tests. Caller holds h.mu.
// Everything derived from the change is computed once, on first use, so that
// the host-side work of any number of intent-test runs stays linear in the
// size of the diff.
type intentState struct {
	created int // intent tests created, deleted ones included
	// change is the recorded change parsed from Options.Diff.
	change *model.Change
	// decls caches the changed declarations of the candidate per language
	// ("go", or "js" for JavaScript and TypeScript).
	decls map[string][]string
	// words is the linking rule's word set of change (IntentWords).
	words *IntentWords
}

var errIntentUnavailable = errors.New("intent tests require acceptance criteria")

// Fixed texts of intent-test results.
const (
	// IntentNote accompanies every run_intent_test result.
	IntentNote = "Candidate-only experiment with no baseline control. A failure is weaker than a reproduced issue: the test and its reading of the criterion are model-written. A pass says nothing about whether the criterion holds."
	// Reasons appended to the description of an UNVERIFIED intent_test record.
	reasonNoReference       = "the intent test references no symbol the change added or modified"
	reasonNotOnAdded        = "no symbol the intent test references is named on an added line of a changed non-test file, so the report cannot re-derive the link to the change"
	reasonNotAssertion      = "the intent test did not fail on an assertion of its own file: a panic, an error thrown by the code under test, a runtime error or a failure outside the test is inconclusive"
	reasonInconclusive      = "the named intent test must start and end on the candidate with a pass or a failure that the runner output confirms; setup failures, skips, timeouts, truncated output and unrelated failures are inconclusive"
	intentReasonNotRetained = "the failing intent test could not be retained as an artifact"
	notePassed              = "a pass says nothing about whether the criterion holds"
	noteLexicalSymbols      = "referenced symbols are matched lexically for JavaScript and TypeScript"
	noteLexicalPython       = "referenced symbols are matched lexically for Python"
	noteNoBaseline          = "no baseline control"
)

// maxIntentTests caps intent tests at half of max_generated_tests, rounded
// up, so that differential experiments keep at least half of the shared
// generated-test budget.
func maxIntentTests(maxGenerated int) int { return (maxGenerated + 1) / 2 }

// intentRunner returns the verified runner, the named tests and the argv of an
// intent test, or "" when the generated_test template cannot establish that
// the named tests ran. The selection is runGenerated's.
func (h *Harness) intentRunner(t *generatedTest) (runner string, names, command []string) {
	command = h.testCommand(t.Path)
	if len(command) == 0 {
		return "", nil, nil
	}
	template := h.opts.Commands["generated_test"]
	switch {
	case len(t.GoTests) > 0 && verifiableGoTemplate(template):
		return RunnerGo, t.GoTests, selectGoTests(command, t.GoTests)
	case len(t.JSTests) > 0 && verifiableJSTemplate(template):
		return RunnerJest, t.JSTests, command
	case len(t.PyTests) > 0 && verifiablePytestTemplate(template):
		return RunnerPytest, t.PyTests, command
	}
	return "", nil, nil
}

// createIntentTest registers a test for one acceptance criterion. It applies
// every create_test rule (paths, no overwrite, test names, the shared
// generated-test budget) and adds three: the criterion must occur exactly once
// in the run's criteria, intent tests may use at most half of the budget, and
// the policy must have a verified runner for the file, without which no
// evidence could be recorded. A refused call creates nothing. Caller holds h.mu.
func (h *Harness) createIntentTest(criterionID, path, content, description string) (any, error) {
	if len(h.opts.IntentCriteria) == 0 {
		return nil, errIntentUnavailable
	}
	c, ok := acceptance.Find(h.opts.IntentCriteria, criterionID)
	if !ok {
		return nil, errors.New("unknown criterion_id; use an ID from intent_criteria")
	}
	if h.intent.created >= maxIntentTests(h.opts.MaxGeneratedTests) {
		return nil, errors.New("intent test budget exhausted (at most half of max_generated_tests, rounded up)")
	}
	value, err := h.createTest(path, content, description)
	if err != nil {
		return nil, err
	}
	id := value.(map[string]any)["test_id"].(string)
	t := h.tests[id]
	delete(h.tests, id)
	if runner, _, _ := h.intentRunner(t); runner == "" {
		h.generated-- // nothing was created
		return nil, errors.New("intent tests need a named-test runner whose output Probe can check: a generated_test command such as go test {package} for a Go test file, or a Jest-compatible runner with {file} and {results_out} for a JavaScript/TypeScript test file with static top-level test titles")
	}
	h.intent.created++
	t.ID = fmt.Sprintf("intent-test-%d", h.intent.created)
	t.Criterion = c.ID
	h.tests[t.ID] = t
	return map[string]any{"test_id": t.ID, "path": t.Path, "criterion_id": c.ID, "criterion": Redact(c.Text), "runs_on": "candidate"}, nil
}

// runIntentTest runs a registered intent test on the candidate snapshot only
// and records one intent_test evidence record whose status IntentOutcome
// derives from the validated candidate check:
//
//   - INTENT_TEST_FAILED: the verified named execution failed (exit 1..124,
//     untruncated) on an assertion of the test's own file, not on a panic or a
//     runtime error, and the test names a changed declaration whose name an
//     added line of a changed non-test file contains;
//   - INTENT_TEST_PASSED: the verified named execution passed (exit 0,
//     untruncated) and the test names at least one changed declaration;
//   - UNVERIFIED: anything else, with the reason in the description.
//
// A failing test is retained as an intent_test artifact and cannot be
// deleted. The base snapshot is never touched. Caller holds h.mu.
func (h *Harness) runIntentTest(ctx context.Context, id string) (any, error) {
	if len(h.opts.IntentCriteria) == 0 {
		return nil, errIntentUnavailable
	}
	t, ok := h.tests[id]
	if !ok {
		return nil, errors.New("unknown intent test")
	}
	if t.Criterion == "" {
		return nil, errors.New("run_intent_test requires a test created by create_intent_test; use run_generated_test for differential tests")
	}
	runner, names, command := h.intentRunner(t)
	if runner == "" {
		return nil, errors.New("the generated_test command has no named-test runner whose output Probe can check for this intent test")
	}
	// Changed declarations are read before the test is staged. When ctx ends
	// first, no symbol is recorded; the run is then not started either.
	symbols, lexical := h.referencedSymbols(ctx, t)
	cleanup, err := stageEphemeral(h.candidate, t.Path, t.Content)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var c model.Check
	if capturesResults(runner) {
		c = h.runWithResultsOptions(ctx, model.CheckGeneratedIntent, h.candidate, command, runOptions{})
	} else {
		c, _, _ = h.runWithOptions(ctx, model.CheckGeneratedIntent, h.candidate, command, runOptions{})
	}
	c, _ = ValidateExecution(runner, c, t.Path, names)
	h.replaceCheck(c)
	// referencedSymbols returned symbols only after building the word set,
	// which IntentOutcome reads only for a failure with symbols.
	status, reason := IntentOutcome(runner, c, t.Path, names, symbols, h.intent.words)
	if status == model.StatusIntentTestFailed && !t.Reproduced {
		if err := h.saveArtifact(t.ID+"-"+filepath.Base(t.Path), model.ArtifactIntentTest, []byte(t.Content)); err != nil {
			status, reason = model.StatusUnverified, intentReasonNotRetained
		} else {
			t.Reproduced = true
		}
	}
	e := model.Evidence{
		Kind:              model.EvidenceIntentTest,
		Description:       strings.TrimSpace(t.Description + " (candidate-only intent test for " + t.Criterion + "; " + noteNoBaseline + ")"),
		Path:              t.Path,
		CheckID:           c.ID,
		CriterionID:       t.Criterion,
		ReferencedSymbols: symbols,
		Status:            status,
		Runner:            runner,
		TestNames:         names,
	}
	if lexical {
		e.Description += " (" + lexicalSymbolsNote(t.Path) + ")"
	}
	if status == model.StatusIntentTestPassed {
		e.Description += " (" + notePassed + ")"
	}
	if reason != "" {
		e.Description += " (" + reason + ")"
	}
	e = h.appendEvidence(e)
	return map[string]any{"evidence": e, "candidate_check": c, "note": IntentNote}, nil
}

// IntentOutcome derives the status of an intent test from its candidate check,
// which the caller has already validated with ValidateExecution for runner,
// path and names. symbols are the changed symbols the test references, and
// words is the word set of the recorded change (NewIntentWords). It returns
// INTENT_TEST_PASSED for a verified pass (exit 0, untruncated) of a test whose
// symbols hold 1 to MaxReferencedSymbols recordable identifiers;
// INTENT_TEST_FAILED when the verified named execution failed (exit 1..124,
// untruncated) on an assertion of the test's own file (IntentAssertionFailed),
// symbols holds 1 to MaxReferencedSymbols recordable identifiers, and at least
// one of them is named on an added line of a changed non-test file
// (IntentWords.Named); UNVERIFIED with a fixed reason otherwise, including a
// pass or a failure of a test that names no changed declaration. The harness
// and report.Finalize both use it; words is read only for a failure that
// reaches the last rule. A nil words names nothing.
func IntentOutcome(runner string, c model.Check, path string, names, symbols []string, words *IntentWords) (status, reason string) {
	referenced := len(symbols) > 0 && len(symbols) <= MaxReferencedSymbols && recordableSymbols(symbols)
	switch {
	case c.Status == "PASS" && c.ExitCode == 0 && !c.Truncated && !referenced:
		// A pass of a test that names no changed declaration concerns code
		// the change did not touch: inconclusive, like such a failure.
		return model.StatusUnverified, reasonNoReference
	case c.Status == "PASS" && c.ExitCode == 0 && !c.Truncated:
		return model.StatusIntentTestPassed, ""
	case c.Status != "FAIL" || c.ExitCode < 1 || c.ExitCode > 124 || c.Truncated:
		return model.StatusUnverified, reasonInconclusive
	case !IntentAssertionFailed(runner, c, path, names):
		return model.StatusUnverified, reasonNotAssertion
	case !referenced:
		return model.StatusUnverified, reasonNoReference
	case !words.Named(symbols):
		return model.StatusUnverified, reasonNotOnAdded
	}
	return model.StatusIntentTestFailed, ""
}

// referencedSymbols returns the changed declarations the intent test names.
// For a Go test: the identifiers and selector names of the test (go/ast),
// intersected with the top-level declarations that contain an added line of a
// changed non-test Go file. For a JavaScript/TypeScript test: its lexical
// identifiers intersected with the changed top-level declarations found
// lexically (lexical is then true). Names are matched, not resolved. The
// symbols that an added line names come first, and at most
// MaxReferencedSymbols recordable identifiers are kept. When ctx ends before
// the changed declarations and the word set are known, it returns no symbol.
// Caller holds h.mu.
func (h *Harness) referencedSymbols(ctx context.Context, t *generatedTest) (symbols []string, lexical bool) {
	var used []string
	language := "go"
	switch {
	case isJSTestPath(t.Path):
		language, lexical = "js", true
		used = acceptance.JSIdentifiers(t.Content)
	case isPyTestPath(t.Path):
		language, lexical = "py", true
		used = symbolindex.PythonIdentifiers([]byte(t.Content))
	default:
		var err error
		if used, err = acceptance.GoIdentifiers(t.Path, []byte(t.Content)); err != nil {
			return nil, false
		}
	}
	decls, ok := h.changedDeclarations(ctx, language)
	words := h.intentWords(ctx)
	if !ok || words == nil {
		return nil, lexical
	}
	var named, other []string
	for _, s := range acceptance.Intersect(used, decls) {
		switch {
		case !recordableSymbol(s):
		case words.Named([]string{s}):
			named = append(named, s)
		default:
			other = append(other, s)
		}
	}
	symbols = append(named, other...)
	if len(symbols) > MaxReferencedSymbols {
		symbols = symbols[:MaxReferencedSymbols]
	}
	return symbols, lexical
}

// intentChange returns the recorded change, parsed from Options.Diff once.
// Caller holds h.mu.
func (h *Harness) intentChange() model.Change {
	if h.intent.change == nil {
		var change model.Change
		_ = json.Unmarshal([]byte(h.opts.Diff), &change)
		h.intent.change = &change
	}
	return *h.intent.change
}

// intentWords returns the word set of the recorded change, built once, or nil
// when ctx ends before it is complete (nothing is cached then). Caller holds
// h.mu.
func (h *Harness) intentWords(ctx context.Context) *IntentWords {
	if h.intent.words == nil {
		change := h.intentChange()
		words, err := addedWords(ctx, change)
		if err != nil {
			return nil
		}
		h.intent.words = &IntentWords{change: change, words: words}
	}
	return h.intent.words
}

// changedDeclarations returns, once per language, the top-level declarations
// of the candidate snapshot that contain an added line of a changed non-test
// file. ctx is checked before each file; when it ends, ok is false and nothing
// is cached. Caller holds h.mu.
func (h *Harness) changedDeclarations(ctx context.Context, language string) (names []string, ok bool) {
	if names, ok := h.intent.decls[language]; ok {
		return names, true
	}
	set := map[string]bool{}
	files := 0
	for _, f := range h.intentChange().Files {
		if f.Binary || f.Status == "D" || intentTestFile(f.Path) || sensitivePath(f.Path) {
			continue
		}
		if language == "go" && !strings.HasSuffix(f.Path, ".go") || language == "js" && !isJSSourcePath(f.Path) || language == "py" && !strings.HasSuffix(strings.ToLower(f.Path), ".py") {
			continue
		}
		added := addedLines(f)
		if len(added) == 0 {
			continue
		}
		if files++; files > maxSymbolFiles {
			break
		}
		if ctx.Err() != nil {
			return nil, false
		}
		path, err := safePath(h.candidate, f.Path)
		if err != nil {
			continue
		}
		src, err := readBounded(path)
		if err != nil {
			continue
		}
		var names []string
		switch language {
		case "go":
			names = acceptance.GoChangedDeclarations(f.Path, src, added)
		case "py":
			names = symbolindex.PythonChangedDeclarations(f.Path, src, added)
		default:
			names = acceptance.JSChangedDeclarations(string(src), added)
		}
		for _, n := range names {
			set[n] = true
		}
	}
	names = make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	if h.intent.decls == nil {
		h.intent.decls = map[string][]string{}
	}
	h.intent.decls[language] = names
	return names, true
}

// intentTestFile reports a test file for the linking rules: the v0.2 test
// file names (isTestPath), plus JavaScript and TypeScript sources named
// <name>.test.<ext> or <name>.spec.<ext> for every extension isJSSourcePath
// accepts (.jsx, .mjs, .cjs, .mts and .cts included) or placed in a
// __tests__ directory. A declaration or an added line of such a file never
// links an intent test to the change.
func intentTestFile(path string) bool {
	if isTestPath(path) || strings.EqualFold(filepath.Base(path), "conftest.py") {
		return true
	}
	if !isJSSourcePath(path) {
		return false
	}
	slashed := strings.ToLower(filepath.ToSlash(path))
	base := slashed[strings.LastIndexByte(slashed, '/')+1:]
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec") || strings.HasPrefix(slashed, "__tests__/") || strings.Contains(slashed, "/__tests__/")
}

// isJSSourcePath reports a JavaScript or TypeScript source file name.
func isJSSourcePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// addedDiffLine reports whether a diff line kind is an addition.
func addedDiffLine(kind string) bool {
	switch kind {
	case "add", "addition", "+":
		return true
	}
	return false
}

// addedLines returns the candidate-side numbers of the added lines of f.
func addedLines(f model.ChangedFile) []int {
	var lines []int
	for _, hunk := range f.Hunks {
		for _, l := range hunk.Lines {
			if addedDiffLine(l.Kind) && l.NewLine > 0 {
				lines = append(lines, l.NewLine)
			}
		}
	}
	return lines
}

// recordableSymbol reports a symbol that can be recorded and read back
// unchanged: an identifier that report sanitizing leaves as it is.
func recordableSymbol(s string) bool {
	return acceptance.IsIdentifier(s) && redact.IsFixedPoint(s) && !strings.Contains(s, redact.Marker)
}

// recordableSymbols applies recordableSymbol to every symbol.
func recordableSymbols(symbols []string) bool {
	for _, s := range symbols {
		if !recordableSymbol(s) {
			return false
		}
	}
	return true
}

// verifiablePath reports a path that report sanitizing leaves unchanged and
// did not produce: a fixed point of redact.Redact without the marker.
func verifiablePath(path string) bool {
	return redact.IsFixedPoint(path) && !strings.Contains(path, redact.Marker)
}

// IntentWords holds the linking rule of INTENT_TEST_FAILED for one recorded
// change: the whole words on the added lines of its changed, non-deleted,
// non-test files (intentTestFile). Binary and secret-bearing files, and files
// whose path or old path redaction would alter, are skipped, and lines are
// read redacted, so the answer is the same before and after report
// sanitizing. The set is built once, on first use, in one pass over the added
// lines, so that the cost of any number of lookups stays linear in the size of
// the diff. An IntentWords is not safe for concurrent use.
type IntentWords struct {
	change model.Change
	words  map[string]bool // nil until built
}

// NewIntentWords returns the linking rule of change; the word set is built on
// the first call of Named.
func NewIntentWords(change model.Change) *IntentWords { return &IntentWords{change: change} }

// Named reports whether at least one recordable symbol of symbols occurs as a
// whole word on an added line of a file the rule reads. A nil IntentWords
// names nothing.
func (w *IntentWords) Named(symbols []string) bool {
	if w == nil {
		return false
	}
	if w.words == nil {
		w.words, _ = addedWords(context.Background(), w.change)
	}
	for _, s := range symbols {
		if recordableSymbol(s) && w.words[s] {
			return true
		}
	}
	return false
}

// IntentSymbolsNamed is NewIntentWords(change).Named(symbols), for a single
// question about a change.
func IntentSymbolsNamed(change model.Change, symbols []string) bool {
	return NewIntentWords(change).Named(symbols)
}

// intentLinkedFile reports a changed file whose added lines the linking rule
// reads.
func intentLinkedFile(f model.ChangedFile) bool {
	return !f.Binary && f.Status != "D" && verifiablePath(f.Path) && (f.OldPath == "" || verifiablePath(f.OldPath)) &&
		!intentTestFile(f.Path) && !sensitivePath(f.Path) && !sensitivePath(f.OldPath)
}

// addedWords returns the identifier words of the redacted added lines of the
// files intentLinkedFile accepts (acceptance.AddWords). ctx is checked every
// 1024 added lines; when it ends, the partial set is discarded.
func addedWords(ctx context.Context, change model.Change) (map[string]bool, error) {
	words := map[string]bool{}
	n := 0
	for _, f := range change.Files {
		if !intentLinkedFile(f) {
			continue
		}
		for _, hunk := range f.Hunks {
			for _, l := range hunk.Lines {
				if !addedDiffLine(l.Kind) {
					continue
				}
				if n++; n%1024 == 0 && ctx.Err() != nil {
					return nil, ctx.Err()
				}
				acceptance.AddWords(words, redact.Redact(l.Content))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return words, nil
}

// goAssertionLine matches the "<file>:<line>: <message>" line the testing
// package writes for t.Error, t.Fatal and t.Log.
var goAssertionLine = regexp.MustCompile(`^\s*([^\s:]+\.go):[0-9]+: (.*)$`)

// IntentAssertionFailed reports whether a failing, validated intent-test
// check failed on an assertion rather than on a panic, a runtime error or a
// setup failure:
//
//   - go_test_json: a named test ended with a fail event, one of its output
//     events (or one of its subtests') is a "<file>:<line>: <message>" line of
//     the intent test's own file with a non-empty message, and no output event
//     of the run contains "panic:";
//   - jest_json: in the report for exactly the intent test's file, whose
//     file-level message is empty, a named top-level test failed with a
//     non-empty failure message, and every non-empty failure message of a
//     failed named test starts with an assertion-error header
//     (jsAssertionMessage): an Error or custom error thrown by the code under
//     test, a runtime error or a timeout is not an assertion failure;
//   - pytest_junit: in the report of the intent test's module, which did not
//     fail to load, a named top-level test failed (not a setup error), and
//     the traceback of every failed named test ends at a line of the test's
//     own module with AssertionError (assert statements, unittest
//     assertions) or Failed (pytest.fail, pytest.raises without the
//     exception) (pytestAssertionMessage): an exception raised by the code
//     under test, or an assertion in a helper module, is not an assertion
//     failure of the test.
//
// Code running in the sandbox writes both channels: the rule tells kinds of
// failure apart, it does not authenticate them.
func IntentAssertionFailed(runner string, check model.Check, path string, names []string) bool {
	if check.Status != "FAIL" || check.Truncated || len(names) == 0 {
		return false
	}
	switch runner {
	case RunnerGo:
		return goAssertionFailed(check.Output, filepath.Base(filepath.FromSlash(path)), names)
	case RunnerJest:
		return jestAssertionFailed(check.Results, path, names)
	case RunnerPytest:
		return pytestAssertionFailed(check.Results, path, names)
	}
	return false
}

func goAssertionFailed(output, file string, names []string) bool {
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	// owner maps a test or subtest name to the named top-level test.
	owner := func(test string) string {
		if i := strings.IndexByte(test, '/'); i >= 0 {
			test = test[:i]
		}
		if wanted[test] {
			return test
		}
		return ""
	}
	failed, asserted := map[string]bool{}, map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxFileBytes)
	for scanner.Scan() {
		var event struct{ Action, Test, Output string }
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Action == "output" && strings.Contains(event.Output, "panic:") {
			return false
		}
		name := owner(event.Test)
		if name == "" {
			continue
		}
		switch event.Action {
		case "fail":
			if event.Test == name {
				failed[name] = true
			}
		case "output":
			if m := goAssertionLine.FindStringSubmatch(strings.TrimRight(event.Output, "\r\n")); m != nil && m[1] == file && strings.TrimSpace(m[2]) != "" {
				asserted[name] = true
			}
		}
	}
	if scanner.Err() != nil {
		return false
	}
	for _, n := range names {
		if failed[n] && asserted[n] {
			return true
		}
	}
	return false
}

func jestAssertionFailed(results, path string, names []string) bool {
	var report jestReport
	if results == "" || json.Unmarshal([]byte(results), &report) != nil {
		return false
	}
	want := "/workspace/" + filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	var file *jestFile
	for i := range report.TestResults {
		if report.TestResults[i].Name == want {
			if file != nil {
				return false
			}
			file = &report.TestResults[i]
		}
	}
	if file == nil || strings.TrimSpace(file.Message) != "" {
		return false
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	asserted := false
	for _, a := range file.AssertionResults {
		if len(a.AncestorTitles) != 0 || !wanted[a.Title] || a.Status != "failed" {
			continue
		}
		for _, m := range a.FailureMessages {
			if strings.TrimSpace(m) == "" {
				continue
			}
			if !jsAssertionMessage(m) {
				return false
			}
			asserted = true
		}
	}
	return asserted
}

// ansiColor matches the terminal color sequences some runners write into
// failure messages.
var ansiColor = regexp.MustCompile("\x1b\\[[0-9;]*m")

// jsAssertionMessage reports a failure message whose first line, without
// color sequences, is the header of an assertion error: "AssertionError:" or
// "AssertionError [" (Vitest and Chai expect/assert, node:assert), or a Jest
// matcher header "Error: expect(" or "Error: expect." (expect(...).toBe,
// expect.assertions). Anything else, such as an Error, a custom error class,
// a runtime error (TypeError, ReferenceError, ...), a thrown non-error value
// or a test timeout, is not an assertion failure.
func jsAssertionMessage(m string) bool {
	first, _, _ := strings.Cut(ansiColor.ReplaceAllString(m, ""), "\n")
	first = strings.TrimSpace(first)
	for _, header := range []string{"AssertionError:", "AssertionError [", "Error: expect(", "Error: expect."} {
		if strings.HasPrefix(first, header) {
			return true
		}
	}
	return false
}

// intentToolDefinitions returns the reviewer tool definitions of the intent
// tests, in the shape ToolDefinitions uses. The reviewer offers them only when
// the run has acceptance criteria.
func intentToolDefinitions() []map[string]any {
	str := func(description string) any { return map[string]any{"type": "string", "description": description} }
	definition := func(name, description string, properties map[string]any, required []string) map[string]any {
		sort.Strings(required)
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
	}
	return []map[string]any{
		definition(IntentCreateTool, "Create a test for one acceptance criterion from intent_criteria. The file rules of create_test apply, and the policy must have a named-test runner whose output Probe can check for the file (Go named tests, a Jest-compatible JSON report with static top-level test titles, or a pytest JUnit XML report with top-level test functions failing on assert statements in the test module). The test runs on the candidate only, with no baseline control. Intent tests share the generated-test budget and may use at most half of it.",
			map[string]any{"criterion_id": map[string]any{"type": "string", "pattern": `^AC-[1-9][0-9]{0,2}$`, "description": "ID of the acceptance criterion, for example AC-1"}, "path": str("New test path, e.g. pkg/probe_intent_ac1_test.go"), "content": str("Exact test source"), "description": str("What the test checks for the criterion")},
			[]string{"criterion_id", "path", "content"}),
		definition(IntentRunTool, "Run an intent test on the candidate snapshot only. Its intent_test evidence is INTENT_TEST_FAILED only when the named test ran and failed on an assertion of its own file and names a declaration the change added or modified (matched by name, not resolved) whose name an added line contains; INTENT_TEST_PASSED when the named test ran and passed and names at least one declaration the change added or modified, which says nothing about whether the criterion holds; UNVERIFIED otherwise.",
			map[string]any{"test_id": str("ID returned by create_intent_test")},
			[]string{"test_id"}),
	}
}

// lexicalSymbolsNote is the description note of an intent test whose
// referenced symbols were matched lexically.
func lexicalSymbolsNote(path string) string {
	if isPyTestPath(path) {
		return noteLexicalPython
	}
	return noteLexicalSymbols
}
