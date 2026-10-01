package mutation

// Outcomes of TS/JS mutation runs, read from the Jest-compatible JSON report
// that a Vitest or Jest mutation command writes to {results_out} and that the
// harness records, normalized, in Check.Results.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// scriptReport is the subset of the normalized report classification reads.
type scriptReport struct {
	TestResults []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		AssertionResults []struct {
			AncestorTitles []string `json:"ancestorTitles"`
			Title          string   `json:"title"`
			Status         string   `json:"status"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// scriptCounts is what one report records.
type scriptCounts struct {
	files         int      // report entries
	passed        int      // passed test results
	failed        []string // failed test results: "<file>: <titles>"
	suiteFailures int      // entries that failed without any test result
}

// readScriptReport counts a recorded report; ok is false when it is missing
// or unreadable.
func readScriptReport(results string) (scriptCounts, bool) {
	if c, ok := readPytestReport(results); ok {
		return c, true
	}
	var r scriptReport
	if results == "" || json.Unmarshal([]byte(results), &r) != nil || r.TestResults == nil {
		return scriptCounts{}, false
	}
	var c scriptCounts
	for _, f := range r.TestResults {
		c.files++
		if f.Status == "failed" && len(f.AssertionResults) == 0 {
			c.suiteFailures++
		}
		for _, a := range f.AssertionResults {
			switch a.Status {
			case "passed":
				c.passed++
			case "failed":
				name := strings.Join(append(append([]string{}, a.AncestorTitles...), a.Title), " > ")
				c.failed = append(c.failed, strings.TrimPrefix(f.Name, "/workspace/")+": "+name)
			}
		}
	}
	return c, true
}

// NewControlFor is NewControl for a mutation command of either kind: a
// Vitest or Jest control (script) is valid when it is a mutation_control
// check with a recorded command, PASS with exit code 0, a complete log and a
// readable JSON report recording at least one passed test, no failed test and
// no test file that failed to load.
func NewControlFor(script bool, control model.Check) Control {
	if !script {
		return NewControl(control)
	}
	c := Control{check: control, script: true}
	id := control.ID
	switch {
	case control.Kind != model.CheckMutationControl:
		c.reason = fmt.Sprintf("check %s is not a mutation control run", id)
	case len(control.Command) == 0:
		c.reason = fmt.Sprintf("the control run %s has no recorded command", id)
	case control.Status != "PASS" || control.ExitCode != 0:
		c.reason = fmt.Sprintf("the unmutated control run %s did not pass (status %s, exit code %d)", id, control.Status, control.ExitCode)
	case control.Truncated:
		c.reason = fmt.Sprintf("the log of the control run %s was truncated", id)
	}
	if c.reason != "" {
		return c
	}
	counts, ok := readScriptReport(control.Results)
	switch {
	case !ok:
		c.reason = fmt.Sprintf("the control run %s recorded no readable JSON report", id)
	case counts.suiteFailures > 0:
		c.reason = fmt.Sprintf("the control run %s recorded a test file that failed to load", id)
	case len(counts.failed) > 0:
		c.reason = fmt.Sprintf("the control run %s recorded a failing test", id)
	case counts.passed == 0:
		c.reason = fmt.Sprintf("the control run %s recorded no passing test (for example no test file is related to the mutated file)", id)
	}
	return c
}

// ClassifyFor is Classify for a mutation command of either kind.
func ClassifyFor(script bool, control, mutant model.Check) Verdict {
	return NewControlFor(script, control).Classify(mutant)
}

// classifyScript derives a TS/JS mutant's outcome, in this order: a check of
// another kind or another command (INCONCLUSIVE); a run that was not executed
// (NOT_RUN); a timeout (TIMEOUT); an infrastructure error, a truncated log or
// a missing report (INCONCLUSIVE); a test file that failed to load while no
// test failed (INVALID: the mutant does not load, for example a syntax or type
// error); a failing test with exit code 1..124 (KILLED); a pass with exit code
// 0, at least one passed test and none failing (SURVIVED); anything else
// INCONCLUSIVE.
func (c Control) classifyScript(mutant model.Check) Verdict {
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
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant log was truncated"}
	}
	counts, ok := readScriptReport(mutant.Results)
	if !ok {
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run recorded no readable JSON report"}
	}
	if len(counts.failed) == 0 && counts.suiteFailures > 0 {
		return Verdict{Status: model.MutantInvalid, Reason: "a test file failed to load with the mutant and no test failed (for example the mutated file does not compile or throws when it is imported)"}
	}
	switch mutant.Status {
	case "FAIL":
		if mutant.ExitCode >= 1 && mutant.ExitCode <= 124 && len(counts.failed) > 0 {
			failed := counts.failed
			if len(failed) > maxFailedTests {
				failed = failed[:maxFailedTests]
			}
			return Verdict{Status: model.MutantKilled, FailedTests: failed}
		}
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run failed without a recorded failing test"}
	default: // PASS
		if mutant.ExitCode == 0 && len(counts.failed) == 0 && counts.passed > 0 {
			return Verdict{Status: model.MutantSurvived, TestsRun: counts.passed}
		}
		return Verdict{Status: model.MutantInconclusive, Reason: "the mutant run passed without a recorded passed test"}
	}
}

// pytestReport is the subset of a normalized pytest JUnit report (the
// harness's pytest_junit format) classification reads.
type pytestReport struct {
	Format    string `json:"format"`
	TestCases []struct {
		ClassName string `json:"classname"`
		Name      string `json:"name"`
		Status    string `json:"status"`
	} `json:"testcases"`
}

// readPytestReport counts a recorded pytest report: a passed testcase is a
// passed test; a failure, or an error of a test (a fixture or setup error),
// a failed test; an error without a classname, a collection error, a test
// file that failed to load. Skipped and xfail testcases are not counted. ok
// is false for a report in another format.
func readPytestReport(results string) (scriptCounts, bool) {
	var r pytestReport
	if results == "" || json.Unmarshal([]byte(results), &r) != nil || r.Format != "pytest_junit" || r.TestCases == nil {
		return scriptCounts{}, false
	}
	var c scriptCounts
	modules := map[string]bool{}
	for _, t := range r.TestCases {
		switch {
		case t.ClassName == "":
			modules[t.Name] = true
			if t.Status == "error" {
				c.suiteFailures++
			}
		case t.Status == "passed":
			modules[t.ClassName] = true
			c.passed++
		case t.Status == "failed" || t.Status == "error":
			modules[t.ClassName] = true
			c.failed = append(c.failed, t.ClassName+"::"+t.Name)
		default:
			modules[t.ClassName] = true
		}
	}
	c.files = len(modules)
	return c, true
}
