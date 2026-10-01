package harness

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/pytestcmd"
)

// Python tests run through pytest, whose built-in JUnit XML report
// (--junitxml) is the only record of which tests ran, as the JSON report is
// for Vitest and Jest. No pytest plugin is needed.
//
// pytest names a test by its node ID, "<path>::<Class>::<function>[<id>]".
// Probe names a test by the part after the path, joined by "::"
// (PytestNameSeparator): "test_total", "TestCart::test_total",
// "test_total[0-0]". The JUnit report carries no file path: its classname is
// the path relative to pytest's rootdir with "/" written "." and ".py"
// dropped, followed by the class names. The rootdir depends on ini files and
// on the target argument, so a report is matched to a file by the dotted
// suffixes of the file's path (pytestCases).

// RunnerPytest is the verifier recorded on evidence whose tests ran through a
// verifiable pytest template.
const RunnerPytest = "pytest_junit"

// PytestNameSeparator joins the classes and the function of a pytest test.
const PytestNameSeparator = "::"

// pytestReportFormat marks a normalized pytest report.
const pytestReportFormat = "pytest_junit"

// pytestNameLimit bounds a selected pytest test name.
const pytestNameLimit = 1024

// pytestTopLevelTest matches a test function defined at column 0. Only ASCII
// identifiers are read; a test the expression misses is not extracted, and
// one it misreads can only make the outcome inconclusive, because validation
// requires every extracted name in the report for the generated file.
var pytestTopLevelTest = regexp.MustCompile(`(?m)^(?:async[ \t]+)?def[ \t]+(test[A-Za-z0-9_]*)[ \t]*\(`)

// isPytestCommand reports whether command runs pytest directly: pytest or
// py.test, or a Python interpreter with -m pytest (pytestcmd).
func isPytestCommand(command []string) bool { return pytestcmd.Is(command) }

// isPythonInterpreter reports whether a command name is a Python interpreter.
func isPythonInterpreter(base string) bool { return pytestcmd.IsInterpreter(base) }

// PytestArgs returns the arguments a pytest command passes to pytest itself,
// or nil for a command that does not run pytest (pytestcmd.Args).
func PytestArgs(command []string) []string { return pytestcmd.Args(command) }

// verifiablePytestTemplate requires pytest run directly (not through a shell,
// a package manager or a task runner, which run repository-defined commands),
// the target as one standalone {file} argument and the JUnit XML report
// written to {results_out} exactly once, through --junitxml or --junit-xml.
// --junit-prefix, which rewrites the report's classnames, and the options
// that deselect tests (pytestSelectionOption) are refused.
func verifiablePytestTemplate(command []string) bool {
	args := PytestArgs(command)
	if args == nil {
		return false
	}
	targets, results, reported := 0, 0, false
	for i, arg := range args {
		if arg == "{file}" {
			targets++
		} else if strings.Contains(arg, "{file}") || strings.Contains(arg, "{package}") {
			return false
		}
		if strings.HasPrefix(arg, "--junit-prefix") || strings.HasPrefix(arg, "--junitprefix") || pytestSelectionOption(arg) {
			return false
		}
		n := strings.Count(arg, config.ResultsPlaceholder)
		results += n
		if n == 0 {
			continue
		}
		switch {
		case arg == "--junitxml="+config.ResultsPlaceholder || arg == "--junit-xml="+config.ResultsPlaceholder:
			reported = true
		case arg == config.ResultsPlaceholder && i > 0 && (args[i-1] == "--junitxml" || args[i-1] == "--junit-xml"):
			reported = true
		}
	}
	return targets == 1 && results == 1 && reported
}

// pytestSelectionOption reports whether a pytest argument deselects tests:
// -k and -m expressions (also attached, -kexpr), --deselect, and the cache
// provider's --lf/--last-failed and --sw/--stepwise, which skip tests by what
// earlier runs recorded.
func pytestSelectionOption(arg string) bool {
	if !strings.HasPrefix(arg, "--") && (strings.HasPrefix(arg, "-k") || strings.HasPrefix(arg, "-m")) {
		return true
	}
	name, _, _ := strings.Cut(arg, "=")
	switch name {
	case "--deselect", "--lf", "--last-failed", "--sw", "--stepwise", "--sw-skip", "--stepwise-skip":
		return true
	}
	return false
}

// isPyTestPath reports whether a path names a pytest test module by pytest's
// default python_files patterns: test_*.py or *_test.py.
func isPyTestPath(p string) bool {
	b := strings.ToLower(path.Base(filepath.ToSlash(p)))
	if !strings.HasSuffix(b, ".py") {
		return false
	}
	return strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py")
}

// generatedPyTests extracts the names of the test functions a generated
// Python module defines at column 0, outside any class.
func generatedPyTests(source string) ([]string, error) {
	names := []string{}
	seen := map[string]bool{}
	for _, m := range pytestTopLevelTest.FindAllStringSubmatch(source, -1) {
		name := m[1]
		if seen[name] {
			// pytest keeps only the last definition of a name.
			return nil, fmt.Errorf("duplicate generated test function %q", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("generated Python file must define at least one test function at column 0, outside any class (def test_name(): ...)")
	}
	return names, nil
}

// selectPytestTests replaces the target argument p of command with the node
// IDs of the named tests, so pytest runs exactly those. The selection only
// narrows the run: every outcome is still read from the report by exact name.
func selectPytestTests(command []string, p string, names []string) []string {
	out := make([]string, 0, len(command)+len(names))
	for _, arg := range command {
		if arg != p {
			out = append(out, arg)
			continue
		}
		for _, name := range names {
			out = append(out, p+PytestNameSeparator+name)
		}
	}
	return out
}

// ValidPytestTestName reports whether a selected pytest name can be run and
// matched: valid UTF-8, bounded, without control characters, with no empty
// part and no part that would read as another argument.
func ValidPytestTestName(name string) bool {
	if name == "" || len(name) > pytestNameLimit || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(name, PytestNameSeparator) {
		if part == "" || strings.HasPrefix(part, "-") {
			return false
		}
	}
	return true
}

// pytestReport is a normalized pytest JUnit XML report: one entry per
// testcase element, in report order.
type pytestReport struct {
	Format    string       `json:"format"`
	TestCases []pytestCase `json:"testcases"`
}

// pytestCase is one testcase: its classname and name as reported, a status
// ("passed", "failed", "error" or "skipped") and the bounded messages of its
// failure, error or skipped elements.
type pytestCase struct {
	ClassName string   `json:"classname"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Messages  []string `json:"messages,omitempty"`
}

// junitMessageLimit bounds each kept message, as for Jest reports.
const junitMessageLimit = 4096

// junitTextLimit bounds the element text read for one message.
const junitTextLimit = 1 << 20

// junitMessage is one kept message: the element's message attribute, then the
// end of its text (the traceback), whose last line names the file, line and
// exception where the failure was raised. Both are redacted and bounded
// together by junitMessageLimit; the middle of a long traceback is dropped.
func junitMessage(head, text string) string {
	head = truncateUTF8(Redact(strings.TrimSpace(head)), junitMessageLimit/4)
	text = Redact(strings.TrimSpace(text))
	if text == "" {
		return head
	}
	room := junitMessageLimit - len(head) - len("\n…\n")
	if len(text) > room {
		cut := len(text) - room
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		text = "…\n" + text[cut:]
	}
	if head == "" {
		return text
	}
	return head + "\n" + text
}

// junitTestCaseLimit bounds the testcases one report may hold.
const junitTestCaseLimit = 20000

// normalizePytestReport reads a JUnit XML report and keeps what verification
// and a reviewer need. Every kept string is redacted and messages are
// bounded. A testcase with a failure is "failed", one with an error (a
// fixture or collection error) "error", one with only skipped elements
// "skipped" (pytest also reports an expected failure, xfail, so), and any
// other "passed". A failure or error decides over a skip.
func normalizePytestReport(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", errors.New("test results are not valid UTF-8")
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = true
	report := pytestReport{Format: pytestReportFormat, TestCases: []pytestCase{}}
	var current *pytestCase
	var message *strings.Builder
	head := ""
	depth, rootSeen := 0, false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("test results are not a JUnit XML report: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				if t.Name.Local != "testsuites" && t.Name.Local != "testsuite" {
					return "", fmt.Errorf("test results are not a JUnit XML report: root element %q", t.Name.Local)
				}
				rootSeen = true
			}
			switch {
			case t.Name.Local == "testcase":
				if current != nil {
					return "", errors.New("test results are not a JUnit XML report: nested testcase")
				}
				if len(report.TestCases) >= junitTestCaseLimit {
					return "", errors.New("test results hold too many testcases")
				}
				current = &pytestCase{ClassName: attr(t, "classname"), Name: attr(t, "name"), Status: "passed"}
			case current != nil && (t.Name.Local == "failure" || t.Name.Local == "error" || t.Name.Local == "skipped"):
				switch {
				case t.Name.Local == "failure" && current.Status != "error":
					current.Status = "failed"
				case t.Name.Local == "error":
					current.Status = "error"
				case t.Name.Local == "skipped" && current.Status == "passed":
					current.Status = "skipped"
				}
				message, head = &strings.Builder{}, attr(t, "message")
			}
		case xml.CharData:
			if message != nil && message.Len() < junitTextLimit {
				message.Write(t)
			}
		case xml.EndElement:
			depth--
			switch {
			case t.Name.Local == "testcase" && current != nil:
				report.TestCases = append(report.TestCases, *current)
				current = nil
			case message != nil && (t.Name.Local == "failure" || t.Name.Local == "error" || t.Name.Local == "skipped"):
				current.Messages = append(current.Messages, junitMessage(head, message.String()))
				message, head = nil, ""
			}
		}
	}
	if !rootSeen {
		return "", errors.New("test results are not a JUnit XML report: no testsuites element")
	}
	for i := range report.TestCases {
		c := &report.TestCases[i]
		c.ClassName = Redact(c.ClassName)
		c.Name = Redact(c.Name)
	}
	b, err := json.Marshal(report)
	return string(b), err
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// parsePytestReport decodes a normalized pytest report, or returns false.
func parsePytestReport(results string) (pytestReport, bool) {
	var report pytestReport
	if results == "" || json.Unmarshal([]byte(results), &report) != nil || report.Format != pytestReportFormat {
		return pytestReport{}, false
	}
	return report, true
}

// pytestModules returns the dotted forms of the path p that pytest can report
// as a classname prefix, longest first: "tests.unit.test_cart",
// "unit.test_cart", "test_cart" for tests/unit/test_cart.py, one per rootdir
// at or above the file's directory.
func pytestModules(p string) []string {
	clean := path.Clean(filepath.ToSlash(p))
	clean = strings.TrimSuffix(clean, ".py")
	parts := strings.Split(clean, "/")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[i:], "."))
	}
	return out
}

// scriptCase is one test of a pytest report, as read for one file: its name
// (the node ID after the path), whether it is top-level (in no class), its
// outcome ("passed", "failed" or "skipped") and its messages.
type scriptCase struct {
	Name     string
	TopLevel bool
	Status   string
	// Error marks a failure reported as an error (a fixture or setup error)
	// rather than a failure of the test body.
	Error    bool
	Messages []string
}

// scriptFile is the report of one file: its tests, and whether the module
// failed to load (a collection error).
type scriptFile struct {
	Cases      []scriptCase
	LoadFailed bool
}

// pytestFile reads the tests of the file p from a normalized pytest report.
// A testcase belongs to p when its classname is one of p's dotted forms
// (pytestModules), or one followed by "." and class names; the longest form
// that matches decides where the classes begin. A collection error of p is a
// testcase with an empty classname named by one of those forms. An error is
// read as a failure, as Jest reports a failing hook. It returns false when
// the report is unreadable or holds nothing for p.
func pytestFile(results, p string) (scriptFile, bool) {
	report, ok := parsePytestReport(results)
	if !ok {
		return scriptFile{}, false
	}
	modules := pytestModules(p)
	var file scriptFile
	found := false
	for _, c := range report.TestCases {
		if c.ClassName == "" {
			for _, m := range modules {
				if c.Name == m && c.Status == "error" {
					file.LoadFailed, found = true, true
				}
			}
			continue
		}
		for _, m := range modules {
			classes := ""
			switch {
			case c.ClassName == m:
			case strings.HasPrefix(c.ClassName, m+"."):
				classes = strings.TrimPrefix(c.ClassName, m+".")
			default:
				continue
			}
			name := c.Name
			if classes != "" {
				name = strings.ReplaceAll(classes, ".", PytestNameSeparator) + PytestNameSeparator + c.Name
			}
			status := c.Status
			if status == "error" {
				status = "failed"
			}
			file.Cases = append(file.Cases, scriptCase{Name: name, TopLevel: classes == "", Status: status, Error: c.Status == "error", Messages: c.Messages})
			found = true
			break
		}
	}
	return file, found
}

// ValidatePytestExecution is ValidateJestExecution for a pytest report: the
// report must hold the file of the generated module and, in it, exactly one
// top-level result for each generated name. A passing check needs every name
// passed; a failing check needs one of them failed. Skipped, missing,
// duplicated and unrelated results are inconclusive.
func ValidatePytestExecution(check model.Check, p string, names []string) model.Check {
	if check.Status != "PASS" && check.Status != "FAIL" {
		return check
	}
	if check.Truncated || check.Results == "" {
		check.Status = "ERROR"
		return check
	}
	if _, ok := parsePytestReport(check.Results); !ok {
		check.Status = "ERROR"
		return check
	}
	file, found := pytestFile(check.Results, p)
	if !found {
		check.Status = "ERROR"
		if model.ModelWrittenCheck(check.Kind) {
			check.ErrorCause = model.ErrorCauseTest
		}
		return check
	}
	status := map[string]string{}
	count := map[string]int{}
	for _, c := range file.Cases {
		if c.TopLevel {
			status[c.Name] = c.Status
			count[c.Name]++
		}
	}
	allPass := len(names) > 0
	failed := false
	for _, name := range names {
		if count[name] != 1 {
			allPass = false
			continue
		}
		allPass = allPass && status[name] == "passed"
		failed = failed || status[name] == "failed"
	}
	if check.Status == "PASS" && !allPass || check.Status == "FAIL" && !failed {
		check.Status = "ERROR"
		if model.ModelWrittenCheck(check.Kind) {
			check.ErrorCause = model.ErrorCauseTest
		}
	}
	return check
}

// capturesResults reports whether a runner's outcomes are read from a report
// written to {results_out}: Vitest and Jest JSON, pytest JUnit XML.
func capturesResults(runner string) bool {
	return runner == RunnerJest || runner == RunnerPytest
}

// PytestTestOutcome returns "pass", "fail" or "skip" for the test named name
// in the report of the file p, and "" when the report is unreadable or does
// not establish one outcome. A test reported once under exactly that name
// has its own outcome. A parametrized test is reported once per variant
// ("name[id]") instead: it failed when a variant failed, passed when every
// variant passed, and was skipped when every variant was skipped; any other
// mix is "".
func PytestTestOutcome(results, p, name string) string {
	file, ok := pytestFile(results, p)
	if !ok {
		return ""
	}
	var exact, variants []string
	for _, c := range file.Cases {
		switch {
		case c.Name == name:
			exact = append(exact, c.Status)
		case strings.HasPrefix(c.Name, name+"[") && strings.HasSuffix(c.Name, "]"):
			variants = append(variants, c.Status)
		}
	}
	switch {
	case len(exact) == 1 && len(variants) == 0:
		return pytestAction(exact[0])
	case len(exact) > 0 || len(variants) == 0:
		return ""
	}
	counts := map[string]int{}
	for _, status := range variants {
		counts[pytestAction(status)]++
	}
	switch {
	case counts[""] > 0:
		return ""
	case counts["fail"] > 0:
		return "fail"
	case counts["pass"] == len(variants):
		return "pass"
	case counts["skip"] == len(variants):
		return "skip"
	}
	return ""
}

func pytestAction(status string) string {
	switch status {
	case "passed":
		return "pass"
	case "failed":
		return "fail"
	case "skipped":
		return "skip"
	}
	return ""
}

// pytestLoadFailure reports whether the report records a collection error
// of the module p. It only selects a reason text.
func pytestLoadFailure(results, p string) bool {
	file, ok := pytestFile(results, p)
	return ok && file.LoadFailed
}

// ClassifyExistingPytestTest is ClassifyExistingJestTest for a test of the
// module p run by a verifiable pytest template: the outcomes come from the
// recorded JUnit reports (PytestTestOutcome).
func ClassifyExistingPytestTest(base, candidate model.Check, p, name string) (status, reason string) {
	return classifyScriptTest(base, candidate, func(results string) string { return PytestTestOutcome(results, p, name) }, func(results string) bool { return pytestLoadFailure(results, p) })
}

// PyTestPath reports whether p names a pytest test module (test_*.py or
// *_test.py), as the stages that run existing tests require.
func PyTestPath(p string) bool { return isPyTestPath(p) }

// PytestSelectionOption is pytestSelectionOption, for the report verifier.
func PytestSelectionOption(arg string) bool { return pytestSelectionOption(arg) }

// pytestFailureLine matches the last line of a pytest traceback: the file,
// the line and the exception where the failure was raised.
var pytestFailureLine = regexp.MustCompile(`^(.+):[0-9]+: ([A-Za-z_][A-Za-z0-9_.]*)$`)

// pytestAssertionFailed is IntentAssertionFailed for a pytest report: the
// module p loaded, a named top-level test failed (a setup error is not a
// failure of the test), and every failed named test failed on an assertion
// of p (pytestAssertionMessage).
func pytestAssertionFailed(results, p string, names []string) bool {
	file, ok := pytestFile(results, p)
	if !ok || file.LoadFailed {
		return false
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	asserted := false
	for _, c := range file.Cases {
		if !c.TopLevel || !wanted[c.Name] || c.Status != "failed" {
			continue
		}
		if c.Error || len(c.Messages) == 0 {
			return false
		}
		for _, m := range c.Messages {
			if !pytestAssertionMessage(m, p) {
				return false
			}
		}
		asserted = true
	}
	return asserted
}

// pytestAssertionMessage reports a failure message whose traceback ends at a
// line of the module p with AssertionError (assert statements and unittest
// assertions) or Failed (pytest.fail, and pytest.raises when the exception
// was not raised). The traceback names the module relative to pytest's
// rootdir, or by an absolute path. An exception of the code under test, an
// assertion of a helper module, or a message without the default traceback
// (--tb=no or --tb=line) is not one.
func pytestAssertionMessage(m, p string) bool {
	m = strings.TrimRight(m, " \t\r\n")
	last := m[strings.LastIndexByte(m, '\n')+1:]
	match := pytestFailureLine.FindStringSubmatch(strings.TrimSpace(last))
	if match == nil || match[2] != "AssertionError" && match[2] != "Failed" {
		return false
	}
	file := path.Clean(filepath.ToSlash(match[1]))
	p = path.Clean(filepath.ToSlash(p))
	return file == p || strings.HasSuffix(p, "/"+file) || strings.HasSuffix(file, "/"+p)
}

// VerifiablePytestTemplate reports whether a generated_test template can
// establish which generated Python tests ran (verifiablePytestTemplate).
func VerifiablePytestTemplate(cmd []string) bool { return verifiablePytestTemplate(cmd) }
