package mutation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// OutcomeFunc returns the single terminal action go test -json recorded for a
// top-level test name and its package, or ("", "") when the log does not
// record exactly one run and one terminal event of that name in one package.
// Production passes harness.GoTestOutcome; this package cannot import harness
// (import layering), and injecting it keeps one implementation of the rule.
type OutcomeFunc func(output, name string) (action, pkg string)

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

// logSummary is what one recorded go test -json log states about itself.
type logSummary struct {
	names       []string // top-level test names, in first-event order
	buildFailed bool     // a build-fail event, a FailedBuild field or a package-level build/setup marker
	timedOut    bool     // the test binary reported its own timeout
	unreadable  bool     // a line exceeded lineLimit
}

// summarize reads a recorded go test -json log. Test output cannot forge
// events: test2json frames it inside "output" events, whose text is looked at
// only for the timeout marker. A build or setup marker counts only in
// package-level output or in a non-JSON line (older toolchains print build
// errors as text), so a test printing it cannot turn its own failure into a
// build failure of the package. Either marker can only remove a claim.
func summarize(output string) logSummary {
	var s logSummary
	seen := map[string]bool{}
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
				s.buildFailed = true
			}
			if strings.Contains(text, timedOutMarker) {
				s.timedOut = true
			}
			continue
		}
		if event.Action == "build-fail" || event.FailedBuild != "" {
			s.buildFailed = true
		}
		if event.Action == "output" {
			if event.Test == "" && containsAny(event.Output, buildFailedMarkers) {
				s.buildFailed = true
			}
			if strings.Contains(event.Output, timedOutMarker) {
				s.timedOut = true
			}
		}
		if event.Test != "" && !strings.Contains(event.Test, "/") && !seen[event.Test] {
			seen[event.Test] = true
			s.names = append(s.names, event.Test)
		}
	}
	if scanner.Err() != nil {
		s.unreadable = true
	}
	return s
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

// Control checks an unmutated control run: PASS with exit code 0, a complete
// and readable log without a build failure or timeout marker, at least one
// top-level test whose recorded outcome is pass, none whose outcome is fail,
// and every recorded outcome in one package. It returns that package, or the
// reason the control is not valid.
func Control(control model.Check, outcome OutcomeFunc) (pkg string, reason string) {
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
	s := summarize(control.Output)
	switch {
	case s.unreadable:
		return "", fmt.Sprintf("the log of the control run %s has a line longer than 1 MiB", id)
	case s.buildFailed:
		return "", fmt.Sprintf("the control run %s recorded a build or setup failure", id)
	case s.timedOut:
		return "", fmt.Sprintf("the control run %s recorded a test timeout", id)
	}
	passed := 0
	for _, name := range s.names {
		action, p := outcome(control.Output, name)
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

// Classify derives a mutant's outcome from its recorded checks, in this order:
// an invalid control or differing commands (INCONCLUSIVE); a run that was not
// executed (NOT_RUN); a timeout (TIMEOUT); an infrastructure error, a
// truncated or unreadable log (INCONCLUSIVE); a build or vet failure
// (INVALID); tests of another package (INCONCLUSIVE); a failing named test
// with exit code 1..124 (KILLED); a pass with exit code 0, at least one
// passing named test and none failing (SURVIVED); anything else INCONCLUSIVE.
//
// report.verifyMutation re-runs it on the saved report, so a stored KILLED or
// SURVIVED is accepted only when the recorded checks still produce it.
func Classify(control, mutant model.Check, outcome OutcomeFunc) Verdict {
	pkg, why := Control(control, outcome)
	if why != "" {
		return Verdict{Status: model.MutantInconclusive, Reason: why}
	}
	if mutant.Kind != model.CheckMutant {
		return Verdict{Status: model.MutantInconclusive, Reason: fmt.Sprintf("check %s is not a mutant run", mutant.ID)}
	}
	if !equalStrings(control.Command, mutant.Command) {
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
	s := summarize(mutant.Output)
	switch {
	case s.unreadable:
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant log has a line longer than 1 MiB"}
	case s.timedOut:
		return Verdict{Status: model.MutantTimeout, Reason: "the test binary reported a timeout"}
	case s.buildFailed:
		return Verdict{Status: model.MutantInvalid, Reason: "the mutant did not build or did not pass go vet"}
	}
	var passed int
	var failed []string
	for _, name := range s.names {
		action, p := outcome(mutant.Output, name)
		if action == "" {
			continue
		}
		if p != pkg {
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
