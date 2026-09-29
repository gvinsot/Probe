package mutation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// lineLimit bounds one go test -json line, as harness.GoTestOutcome does. A
// longer line makes the whole log unreadable rather than silently skipped.
const lineLimit = 1 << 20

// maxFailedTests caps the failing test names a KILLED mutant records.
const maxFailedTests = 5

// Markers of a recorded log. They are read only from the recorded check
// output, never from a live stream: no verdict depends on tee data.
var (
	buildFailedMarkers = []string{"[build failed]", "[setup failed]"}
	timedOutMarker     = "panic: test timed out after"
)

// goTestLog is what one recorded go test -json log states, read in a single
// pass: classifying a mutant costs time linear in the size of its two logs,
// whatever the number of test names in them.
//
// The per-name outcome applies exactly the rule of harness.GoTestOutcome (this
// package cannot import harness; events_test.go checks the two against each
// other on every recorded fixture and on generated logs).
//
// The log is not tamper-proof. Code executing in the sandbox writes it: a test
// can print lines that test2json turns into events (the framing marker that
// go test -json uses since go1.24, or raw JSON written to the go process's
// standard output), so it can add, hide or contradict test events. That can
// only change which mutant status is recorded; mutation creates no evidence
// and changes the exit code only through an incomplete or not_run section.
type goTestLog struct {
	names       []string               // top-level test names, in first-event order
	tests       map[string]*testEvents // every test name with at least one counted event
	buildFailed bool                   // a build-fail event, a FailedBuild field or a package-level build/setup marker
	timedOut    bool                   // the test binary reported its own timeout
	unreadable  bool                   // a line exceeded lineLimit
}

// testEvents tallies the events of one test name as harness.GoTestOutcome
// counts them.
type testEvents struct {
	runs, terminals int
	runSeen         bool
	orderOK         bool // no terminal event before the first run event
	terminal, pkg   string
	conflict        bool // an event without a package, or of a second package
}

// outcomeEvent holds the only fields harness.GoTestOutcome decodes.
type outcomeEvent struct {
	Action  string `json:"Action"`
	Test    string `json:"Test"`
	Package string `json:"Package"`
}

// readLog reads a recorded go test -json log once. A build or setup marker
// counts only in package-level output or in a non-JSON line (older toolchains
// print build errors as text), so a test printing it cannot turn its own
// failure into a build failure of the package. Either marker can only remove a
// claim.
func readLog(output string) *goTestLog {
	l := &goTestLog{tests: map[string]*testEvents{}}
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), lineLimit)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Action      string `json:"Action"`
			Package     string `json:"Package"`
			Test        string `json:"Test"`
			Output      string `json:"Output"`
			FailedBuild string `json:"FailedBuild"`
		}
		if line[0] != '{' || json.Unmarshal(line, &event) != nil {
			text := string(line)
			if containsAny(text, buildFailedMarkers) {
				l.buildFailed = true
			}
			if strings.Contains(text, timedOutMarker) {
				l.timedOut = true
			}
			// GoTestOutcome decodes only Action, Test and Package: a line whose
			// other fields have the wrong JSON type still counts for it.
			var e outcomeEvent
			if line[0] == '{' && json.Unmarshal(line, &e) == nil {
				l.count(e)
			}
			continue
		}
		if event.Action == "build-fail" || event.FailedBuild != "" {
			l.buildFailed = true
		}
		if event.Action == "output" {
			if event.Test == "" && containsAny(event.Output, buildFailedMarkers) {
				l.buildFailed = true
			}
			if strings.Contains(event.Output, timedOutMarker) {
				l.timedOut = true
			}
		}
		l.count(outcomeEvent{Action: event.Action, Test: event.Test, Package: event.Package})
	}
	if scanner.Err() != nil {
		l.unreadable = true
	}
	return l
}

// count applies one decoded event to its test name.
func (l *goTestLog) count(e outcomeEvent) {
	if e.Test == "" {
		return
	}
	t := l.tests[e.Test]
	if t == nil {
		t = &testEvents{orderOK: true}
		l.tests[e.Test] = t
		if !strings.Contains(e.Test, "/") {
			l.names = append(l.names, e.Test)
		}
	}
	if t.conflict {
		return
	}
	if e.Package == "" || t.pkg != "" && e.Package != t.pkg {
		t.conflict = true
		return
	}
	t.pkg = e.Package
	switch e.Action {
	case "run":
		t.runs++
		t.runSeen = true
	case "pass", "fail", "skip":
		t.terminals++
		t.terminal = e.Action
		if !t.runSeen {
			t.orderOK = false
		}
	}
}

// outcome is harness.GoTestOutcome(output, name) for the log read by readLog:
// the single terminal action and the package of name, or ("", "") unless the
// log is readable and records exactly one run and one later terminal event of
// name, all in one non-empty package.
func (l *goTestLog) outcome(name string) (action, pkg string) {
	t := l.tests[name]
	if name == "" || l.unreadable || t == nil || t.conflict || t.runs != 1 || t.terminals != 1 || !t.orderOK || t.pkg == "" {
		return "", ""
	}
	return t.terminal, t.pkg
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Verdict is the outcome of one mutant run, derived from the recorded control
// and mutant checks only.
type Verdict struct {
	Status      string // model.MutantKilled, MutantSurvived, MutantInvalid, MutantTimeout, MutantInconclusive or MutantNotRun
	TestsRun    int    // top-level tests whose recorded outcome is pass (SURVIVED)
	FailedTests []string
	Reason      string
}

// Control is one recorded control run, read once. Every mutant of its package
// is classified against it, so a control log is read once per package rather
// than once per mutant.
type Control struct {
	check  model.Check
	pkg    string
	reason string
}

// NewControl checks an unmutated control run: kind mutation_control, a
// recorded command, PASS with exit code 0, a complete and readable log without
// a build failure or timeout marker, at least one top-level test whose recorded
// outcome is pass, none whose outcome is fail, and every recorded outcome in
// one package.
func NewControl(control model.Check) Control {
	c := Control{check: control}
	c.pkg, c.reason = validateControl(control)
	if c.reason != "" {
		c.pkg = ""
	}
	return c
}

// Package is the Go import path of the control's tests ("" when invalid).
func (c Control) Package() string { return c.pkg }

// Reason says why the control is not valid ("" when it is).
func (c Control) Reason() string { return c.reason }

func validateControl(control model.Check) (pkg string, reason string) {
	id := control.ID
	switch {
	case control.Kind != model.CheckMutationControl:
		return "", fmt.Sprintf("check %s is not a mutation control run", id)
	case len(control.Command) == 0:
		return "", fmt.Sprintf("the control run %s has no recorded command", id)
	case control.Status != "PASS" || control.ExitCode != 0:
		return "", fmt.Sprintf("the unmutated control run %s did not pass (status %s, exit code %d)", id, control.Status, control.ExitCode)
	case control.Truncated:
		return "", fmt.Sprintf("the log of the control run %s was truncated; sandbox.max_output_bytes must hold the package's go test -json log", id)
	}
	l := readLog(control.Output)
	switch {
	case l.unreadable:
		return "", fmt.Sprintf("the log of the control run %s has a line longer than 1 MiB", id)
	case l.buildFailed:
		return "", fmt.Sprintf("the control run %s recorded a build or setup failure", id)
	case l.timedOut:
		return "", fmt.Sprintf("the control run %s recorded a test timeout", id)
	}
	passed := 0
	for _, name := range l.names {
		action, p := l.outcome(name)
		if action == "" {
			continue
		}
		if pkg == "" {
			pkg = p
		}
		if p != pkg {
			return "", fmt.Sprintf("the control run %s recorded tests of more than one package", id)
		}
		switch action {
		case "pass":
			passed++
		case "fail":
			return "", fmt.Sprintf("the control run %s recorded a failing test", id)
		}
	}
	if passed == 0 || pkg == "" {
		return "", fmt.Sprintf("the control run %s recorded no passing named test (for example no test ran, or TestMain exited early)", id)
	}
	return pkg, ""
}

// Classify derives a mutant's outcome from its recorded check and the
// control, in this order: an invalid control or differing commands
// (INCONCLUSIVE); a run that was not executed (NOT_RUN); a timeout (TIMEOUT);
// an infrastructure error, a truncated or unreadable log (INCONCLUSIVE); a
// build or vet failure (INVALID); tests of another package (INCONCLUSIVE); a
// failing named test with exit code 1..124 (KILLED); a pass with exit code 0,
// at least one passing named test and none failing (SURVIVED); anything else
// INCONCLUSIVE.
//
// report.verifyMutation re-runs it on the saved report, so a stored KILLED or
// SURVIVED is accepted only when the recorded checks still produce it.
func (c Control) Classify(mutant model.Check) Verdict {
	if c.reason != "" {
		return Verdict{Status: model.MutantInconclusive, Reason: c.reason}
	}
	if mutant.Kind != model.CheckMutant {
		return Verdict{Status: model.MutantInconclusive, Reason: fmt.Sprintf("check %s is not a mutant run", mutant.ID)}
	}
	if !equalStrings(c.check.Command, mutant.Command) {
		return Verdict{Status: model.MutantInconclusive, Reason: "the control and mutant runs did not use one identical recorded command"}
	}
	switch mutant.Status {
	case "SKIPPED":
		return Verdict{Status: model.MutantNotRun, Reason: "the mutant run was not executed: " + redact.TruncateUTF8(strings.TrimSpace(mutant.Output), 200)}
	case "TIMEOUT":
		return Verdict{Status: model.MutantTimeout, Reason: "the mutant run reached its time limit"}
	case "ERROR":
		return Verdict{Status: model.MutantInconclusive, Reason: fmt.Sprintf("the mutant run ended in an infrastructure error (exit code %d)", mutant.ExitCode)}
	case "PASS", "FAIL":
	default:
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run has an unknown status"}
	}
	if mutant.Truncated {
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant log was truncated; sandbox.max_output_bytes must hold the package's go test -json log"}
	}
	l := readLog(mutant.Output)
	switch {
	case l.unreadable:
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant log has a line longer than 1 MiB"}
	case l.timedOut:
		return Verdict{Status: model.MutantTimeout, Reason: "the test binary reported a timeout"}
	case l.buildFailed:
		return Verdict{Status: model.MutantInvalid, Reason: "the mutant did not build or did not pass go vet"}
	}
	var passed int
	var failed []string
	for _, name := range l.names {
		action, p := l.outcome(name)
		if action == "" {
			continue
		}
		if p != c.pkg {
			return Verdict{Status: model.MutantInconclusive, Reason: "the mutant log records tests of another package than the control run"}
		}
		switch action {
		case "pass":
			passed++
		case "fail":
			failed = append(failed, name)
		}
	}
	switch mutant.Status {
	case "FAIL":
		if mutant.ExitCode >= 1 && mutant.ExitCode <= 124 && len(failed) > 0 {
			if len(failed) > maxFailedTests {
				failed = failed[:maxFailedTests]
			}
			return Verdict{Status: model.MutantKilled, FailedTests: failed}
		}
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run failed without a recorded failing named test"}
	default: // PASS
		if mutant.ExitCode == 0 && len(failed) == 0 && passed > 0 {
			return Verdict{Status: model.MutantSurvived, TestsRun: passed}
		}
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run passed without a recorded passing named test (for example TestMain exited early)"}
	}
}

// Classify is NewControl(control).Classify(mutant).
func Classify(control, mutant model.Check) Verdict {
	return NewControl(control).Classify(mutant)
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
