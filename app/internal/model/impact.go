package model

// Impact section statuses, impacted-test statuses, change classes and edge
// resolutions.
const (
	ImpactNotApplicable     = "not_applicable"
	ImpactIndexed           = "indexed"
	ImpactLimited           = "limited"
	ImpactUnavailable       = "unavailable"
	ImpactTestsRan          = "ran"
	ImpactTestsNoCandidates = "no_candidates"
	ImpactTestsNotRun       = "not_run"
	ChangeBodyChanged       = "body_changed"
	ChangeSignatureChanged  = "signature_changed"
	ResolutionStatic        = "static"
	ResolutionInterface     = "interface"
)

// ImpactNote is the fixed note of the impact section. It says what the static
// index is and what it does not establish.
const ImpactNote = "The index is built on the host from committed source by parsing and type-checking the Go packages of the repository with the Go standard library, without running repository code. Imports from outside the repository are not loaded, and files are selected with linux/amd64 build constraints. Calls through function values, reflection, go:linkname, assembly and generated code are not resolved, and interface edges are possible dispatch only. A listed caller is a place to review, not a defect, and an absent caller is not proof that none exists. A test that reaches a function is not evidence that it asserts the behavior of that function."

// ImpactCaller is a reference to a changed function from unchanged, non-test
// code, found by the static index. Depth is 1: callers are direct reference
// sites.
type ImpactCaller struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Symbol     string `json:"symbol"`
	Depth      int    `json:"depth"`
	Resolution string `json:"resolution"`
}

// ImpactTest is an existing TestX function that statically reaches a changed
// function within Depth references. Package is the import path of the test's
// directory.
type ImpactTest struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Package    string `json:"package"`
	Depth      int    `json:"depth"`
	Resolution string `json:"resolution"`
	EvidenceID string `json:"evidence_id,omitempty"`
	Status     string `json:"status,omitempty"` // set by Finalize from verified evidence; empty when not run
	Reason     string `json:"reason,omitempty"`
	// F6a additions: the declarations from the test to the changed function,
	// and whether the change touched the test's file.
	Via         []string `json:"via,omitempty"`
	FileChanged bool     `json:"file_changed,omitempty"`
}

// ImpactFunction is one changed function or method of a changed non-test Go
// file.
type ImpactFunction struct {
	Path         string         `json:"path"`
	Line         int            `json:"line"`
	EndLine      int            `json:"end_line"`
	Symbol       string         `json:"symbol"`
	Change       string         `json:"change"`
	Callers      []ImpactCaller `json:"callers"`       // at most 10 listed
	CallersTotal int            `json:"callers_total"` // found by the bounded index search
	Tests        []ImpactTest   `json:"tests"`
	// F6a additions: whether the index holds the function (false: its callers
	// and tests were not searched, and Reason says why; true with a Reason: a
	// search bound was reached, and Reason names it), and the number of
	// reaching tests found, of which at most 20 are listed.
	Indexed    bool   `json:"indexed"`
	Reason     string `json:"reason,omitempty"`
	TestsTotal int    `json:"tests_total"`
}

// Impact is the impact-analysis section. It is present in lint and review
// unless --impact=false; TestsStatus is present only with --impacted-tests.
type Impact struct {
	Status           string           `json:"status"`
	Reason           string           `json:"reason,omitempty"`
	IndexedFiles     int              `json:"indexed_files"`
	ChangedFunctions []ImpactFunction `json:"changed_functions"`
	TestsStatus      string           `json:"tests_status,omitempty"` // present only with --impacted-tests
	TestsReason      string           `json:"tests_reason,omitempty"`
	Note             string           `json:"note"`
}
