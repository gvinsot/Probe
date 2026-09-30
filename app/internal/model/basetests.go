package model

import "strings"

// Changed-baseline-test section statuses and per-test change classes.
const (
	BaseTestsNoCandidates     = "no_candidates"
	BaseTestsRan              = "ran"
	BaseTestsNotRun           = "not_run"
	BaseTestRemoved           = "removed"
	BaseTestModified          = "modified"
	BaseTestSharedCodeChanged = "shared_code_changed"
	BaseTestFileDeleted       = "file_deleted"
)

// BaseTestsNote is the fixed note of the base_tests section. It says what the
// two recorded runs of each test establish and what they do not.
const BaseTestsNote = "Each entry is the baseline version of a Go test function declared in a changed Go test file: a test that the change modified or removed, or a test whose file changed outside it (other declarations, imports, build constraints, compiler directives or the package clause, or a rename to another directory or platform suffix). " +
	"It ran on the baseline tree and on a hybrid tree: the candidate tree with the *_test.go files and testdata directory of the package of the test reverted to the baseline. " +
	"FAILS_ON_CANDIDATE means that the test passed on the baseline and failed on the hybrid tree, in one recorded run each: possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge. " +
	"It is not a reproduced issue, and it does not show which behavior is intended. " +
	"PASSES_ON_CANDIDATE means only that this one test passed in both recorded runs; it does not show that behavior is preserved or that the edited test is equivalent, and code executing in the sandbox can influence it. " +
	"Any other entry records why neither result was drawn, for example a compile failure after an API change. " +
	"Only tests declared in changed Go test files were considered; other tests were not selected."

// BaseTest is one selected test: the baseline version of a Go test function
// (Path, Line, EndLine on the baseline side) and, when the candidate still
// declares it, its edited version (Candidate*). Status is set by
// report.Finalize from verified evidence only.
type BaseTest struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	Line             int    `json:"line"`
	EndLine          int    `json:"end_line"`
	CandidatePath    string `json:"candidate_path,omitempty"`
	CandidateLine    int    `json:"candidate_line,omitempty"`
	CandidateEndLine int    `json:"candidate_end_line,omitempty"`
	Change           string `json:"change"`
	Status           string `json:"status"` // FAILS_ON_CANDIDATE | PASSES_ON_CANDIDATE | UNVERIFIED
	EvidenceID       string `json:"evidence_id,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// BaseTests is the changed-baseline-test section. It is present exactly when
// review ran with --base-tests.
type BaseTests struct {
	Status string     `json:"status"`
	Reason string     `json:"reason,omitempty"`
	Tests  []BaseTest `json:"tests"`
	Note   string     `json:"note"`
}

// BaseTestsScriptNote is added to BaseTestsNote when the section lists a
// TypeScript or JavaScript test.
const BaseTestsScriptNote = "A TypeScript or JavaScript entry is the baseline version of one test() or it() call of a changed TypeScript or JavaScript test file, named by its describe titles and title; a test whose file changed outside its calls (imports, helpers, describe titles, hooks) is selected as shared_code_changed. " +
	"Its hybrid tree reverts only that test file and its snapshot file (__snapshots__/<file>.snap) to the baseline, so the helpers and fixtures it imports are the candidate's; it ran through the Vitest or Jest template, and its results were read from the runner's JSON report."

// ScriptBaseTest reports whether a selected test is a TypeScript or
// JavaScript test rather than a Go test function.
func ScriptBaseTest(t BaseTest) bool { return !strings.HasSuffix(t.Path, ".go") }

// BaseTestsNoteFor is the fixed note of a section listing tests: BaseTestsNote,
// followed by BaseTestsScriptNote when one of them is a TypeScript or
// JavaScript test. It depends only on the listed paths, so re-rendering a
// report gives the same note.
func BaseTestsNoteFor(tests []BaseTest) string {
	for _, t := range tests {
		if ScriptBaseTest(t) {
			return BaseTestsNote + " " + BaseTestsScriptNote
		}
	}
	return BaseTestsNote
}
