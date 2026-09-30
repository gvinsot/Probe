package model

import "path"

// Mutation section statuses, file statuses and mutant statuses.
const (
	MutationNotRun       = "not_run"
	MutationNoCandidates = "no_candidates"
	MutationRan          = "ran"
	MutationIncomplete   = "incomplete"
	MutationFileEligible = "eligible"
	MutationFileSkipped  = "skipped"
	MutantKilled         = "KILLED"
	MutantSurvived       = "SURVIVED"
	MutantInvalid        = "INVALID"
	MutantTimeout        = "TIMEOUT"
	MutantInconclusive   = "INCONCLUSIVE"
	MutantNotRun         = "NOT_RUN"
)

// MutationNote is the fixed note of the mutation section (no apostrophe, so
// the Markdown renders it without entities). It states what a
// mutant outcome records and what it does not establish.
const MutationNote = "Each mutant is one deterministic change to an added line of a changed non-test Go file. It runs the go test command of the policy for the package of that file only, in a private copy of the candidate, after an unmutated control run of the same command passed with at least one named test. A surviving mutant records only that no test this command ran for that package failed with the change and at least one passed (skipped tests are not counted): the mutant may be semantically equivalent to the original code, tests of other packages were not run, and it is not evidence of a defect, of a missing test or of dead code. A killed mutant records only that a named test failed with the change; it is counted, never listed, and is no reassurance about the tests. Outcomes are read from test logs that code running in the sandbox can also write. Deleted lines, test files and non-Go files are not mutated. There is no mutation score."

type MutationLimits struct {
	MaxMutants        int `json:"max_mutants"`
	TimeoutSeconds    int `json:"timeout_seconds"`
	MaxRuntimeSeconds int `json:"max_runtime_seconds"`
}
type MutationFile struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	AddedLines   int    `json:"added_lines"`
	MutatedLines int    `json:"mutated_lines"`
}
type Mutant struct {
	ID             string   `json:"id"` // mutant-N
	Path           string   `json:"path"`
	Line           int      `json:"line"`
	Package        string   `json:"package"`
	Operator       string   `json:"operator"`
	Original       string   `json:"original"` // display, ≤256 bytes
	Mutated        string   `json:"mutated"`  // display, ≤256 bytes
	Status         string   `json:"status"`
	CheckID        string   `json:"check_id,omitempty"`
	ControlCheckID string   `json:"control_check_id,omitempty"`
	PatchSHA256    string   `json:"patch_sha256,omitempty"` // sha256 of the retained mutant_patch artifact
	TestsRun       int      `json:"tests_run,omitempty"`
	FailedTests    []string `json:"failed_tests,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	// F4 additions (not in Appendix A): where the replaced span starts and ends
	// and the function that encloses it.
	Column  int    `json:"column,omitempty"`   // 1-based byte column of the replaced span on Line
	EndLine int    `json:"end_line,omitempty"` // last line of a replaced span that covers several lines
	Symbol  string `json:"symbol,omitempty"`   // enclosing function, "Recv.Name" for a method
}

// Mutation is the mutation section. It is present exactly when the policy has
// mutation in review mode, whatever --checks says.
type Mutation struct {
	Status       string         `json:"status"`
	Reason       string         `json:"reason,omitempty"`
	Command      []string       `json:"command"`
	Limits       MutationLimits `json:"limits"`
	Files        []MutationFile `json:"files"`
	Mutants      []Mutant       `json:"mutants"`
	Generated    int            `json:"generated"`
	Dropped      int            `json:"dropped"` // candidates not run because of max_mutants
	Killed       int            `json:"killed"`
	Survived     int            `json:"survived"`
	Invalid      int            `json:"invalid"`
	TimedOut     int            `json:"timed_out"`
	Inconclusive int            `json:"inconclusive"`
	NotRun       int            `json:"not_run"`
	Checks       []Check        `json:"checks"` // mutation ledger: mutation-check-N
	Note         string         `json:"note"`
	// CoverageSkipped counts candidate mutants on added lines that a passing,
	// measured coverage run reported as not executed; they were not generated
	// (F4 addition, not in Appendix A).
	CoverageSkipped int `json:"coverage_skipped,omitempty"`
}

// MutationScriptNote is the note of a mutation section whose command runs
// Vitest or Jest on TypeScript and JavaScript sources.
const MutationScriptNote = "Each mutant is one deterministic change to an added line of a changed TypeScript or JavaScript source (not a test, declaration or minified file), found lexically without a parser or type checker: only operators written with white space on both sides, conditions of if and while, integer operands of comparisons and boolean results of return and arrow functions are mutated, inside function and method bodies. It runs the Vitest or Jest command of the policy for that source file, which selects the tests related to it, in a private copy of the candidate, after an unmutated control run of the same command passed with at least one passed test. Outcomes are read from the JSON report of each run. A surviving mutant records only that no test this command ran for that file failed with the change and at least one passed (skipped tests are not counted): the mutant may be semantically equivalent to the original code, tests the command did not relate to the file were not run, and it is not evidence of a defect, of a missing test or of dead code. A mutant with which a test file failed to load and no test failed is invalid, not killed. A killed mutant records only that a test failed with the change; it is counted, never listed, and is no reassurance about the tests. The reports can also be written by code running in the sandbox. There is no mutation score."

// MutationNoteFor is the fixed note of a mutation section run with command.
func MutationNoteFor(command []string) string {
	if len(command) > 0 && path.Base(command[0]) != "go" {
		return MutationScriptNote
	}
	return MutationNote
}
