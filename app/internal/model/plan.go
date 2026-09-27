package model

import "time"

// Pre-change planning (swiftproof plan) and scope-drift control (review
// --plan). A model proposes a plan without touching the repository; SwiftProof
// evaluates the plan deterministically against the base commit, and a later
// review compares the real diff with the plan's contract. The model never
// judges risk: every category below is a fixed rule applied to the plan.

// PlanFormat identifies a PLAN.json document; PlanVersion is its version.
const (
	PlanFormat  = "swiftproof-plan"
	PlanVersion = 1
)

// Planned file changes.
const (
	PlanFileAdd    = "add"
	PlanFileModify = "modify"
	PlanFileDelete = "delete"
	PlanFileRename = "rename"
)

// Planned symbol changes.
const (
	PlanSymbolAdd       = "add"
	PlanSymbolBody      = "body"
	PlanSymbolSignature = "signature"
	PlanSymbolRemove    = "remove"
)

// Planned dependency changes.
const (
	PlanDependencyAdd     = "add"
	PlanDependencyUpgrade = "upgrade"
	PlanDependencyRemove  = "remove"
)

// Plan assessment categories.
const (
	PlanCategoryCriticalParts  = "critical_parts"
	PlanCategoryArchitecture   = "architecture"
	PlanCategoryRegressionRisk = "regression_risk"
	PlanCategoryOtherMajor     = "other_major"
)

// Plan signal kinds. Each belongs to exactly one category (PlanSignalCategory).
const (
	PlanSignalCriticalPath      = "plan_critical_path"
	PlanSignalSensitiveSymbol   = "plan_sensitive_symbol"
	PlanSignalExportedSignature = "plan_exported_signature"
	PlanSignalDependency        = "plan_dependency_change"
	PlanSignalNewPackage        = "plan_new_package"
	PlanSignalWideImpact        = "plan_wide_impact"
	PlanSignalUntestedImpact    = "plan_untested_impact"
	PlanSignalDeletion          = "plan_file_deletion"
	PlanSignalLargeScope        = "plan_large_scope"
	PlanSignalInconsistent      = "plan_inconsistent"
	PlanSignalUnmeasured        = "plan_unmeasured"
)

// PlanSignalCategory maps every plan signal kind to its category.
var PlanSignalCategory = map[string]string{
	PlanSignalCriticalPath:      PlanCategoryCriticalParts,
	PlanSignalSensitiveSymbol:   PlanCategoryCriticalParts,
	PlanSignalExportedSignature: PlanCategoryArchitecture,
	PlanSignalDependency:        PlanCategoryArchitecture,
	PlanSignalNewPackage:        PlanCategoryArchitecture,
	PlanSignalWideImpact:        PlanCategoryRegressionRisk,
	PlanSignalUntestedImpact:    PlanCategoryRegressionRisk,
	PlanSignalDeletion:          PlanCategoryOtherMajor,
	PlanSignalLargeScope:        PlanCategoryOtherMajor,
	PlanSignalInconsistent:      PlanCategoryOtherMajor,
	PlanSignalUnmeasured:        PlanCategoryOtherMajor,
}

// PlanNote is the fixed caveat of every plan document.
const PlanNote = "The plan is a model's proposal, written without executing or modifying anything. SwiftProof did not verify that the plan implements the intent. The assessment applies fixed rules to the files, symbols and dependencies the plan names, at the base commit; it is not a prediction of what the implementation will do, and a plan that raises nothing is not proof that the change is safe. Use review --plan to check that the implementation stays within the plan."

// PlanDriftNote is the fixed caveat of the plan_drift section of a report.
const PlanDriftNote = "Scope drift compares the recorded diff with the files, symbols, dependency manifests and critical paths a plan declared. It checks that the change stays within what was announced, not that it implements the intent or that the plan was right."

// Plan is the PLAN.json document written by swiftproof plan.
type Plan struct {
	Format       string         `json:"format"`
	Version      int            `json:"version"`
	ToolVersion  string         `json:"tool_version"`
	GeneratedAt  time.Time      `json:"generated_at"`
	Intent       string         `json:"intent"`
	IntentSHA256 string         `json:"intent_sha256"`
	BaseRef      string         `json:"base_ref"`
	BaseCommit   string         `json:"base_commit"`
	Policy       Policy         `json:"policy"`
	Model        string         `json:"model"`
	Proposal     PlanProposal   `json:"proposal"`
	Assessment   PlanAssessment `json:"assessment"`
	Contract     PlanContract   `json:"contract"`
	Unverified   []string       `json:"unverified"`
	Audit        []AuditEvent   `json:"audit"`
	Note         string         `json:"note"`
	ExitCode     int            `json:"exit_code"`
}

// PlanProposal is the model-written plan. It is untrusted data: SwiftProof
// validates its shape and measures what it names, never what it claims.
type PlanProposal struct {
	Summary      string              `json:"summary"`
	Steps        []string            `json:"steps"`
	Files        []PlannedFile       `json:"files"`
	Symbols      []PlannedSymbol     `json:"symbols"`
	Dependencies []PlannedDependency `json:"dependencies"`
	Assumptions  []string            `json:"assumptions"`
}

// PlannedFile is one file the plan intends to add, modify, delete or rename.
type PlannedFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Change  string `json:"change"`
	Reason  string `json:"reason,omitempty"`
}

// PlannedSymbol is one declaration the plan intends to change. Name is "F" or
// "T.M", as declared in Path.
type PlannedSymbol struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Change string `json:"change"`
	Reason string `json:"reason,omitempty"`
}

// PlannedDependency is one dependency the plan intends to change in Manifest.
type PlannedDependency struct {
	Manifest string `json:"manifest"`
	Name     string `json:"name"`
	Change   string `json:"change"`
	Version  string `json:"version,omitempty"`
}

// PlanAssessment is SwiftProof's deterministic evaluation of the proposal.
type PlanAssessment struct {
	Major           bool                   `json:"major"`
	Categories      []PlanCategory         `json:"categories"`
	Signals         []Signal               `json:"signals"`
	CriticalPaths   []PlanCriticalPath     `json:"critical_paths"`
	Symbols         []PlanSymbolImpact     `json:"symbols"`
	Manifests       []string               `json:"manifests"`
	NewPackages     []string               `json:"new_packages"`
	Index           PlanIndex              `json:"index"`
	Thresholds      PlanAssessmentBoundary `json:"thresholds"`
	Inconsistencies []string               `json:"inconsistencies"`
}

// PlanCategory says whether one category is flagged, and by which signals.
// A category is flagged when one of its signals is medium or higher.
type PlanCategory struct {
	Name      string   `json:"name"`
	Flagged   bool     `json:"flagged"`
	SignalIDs []string `json:"signal_ids"`
}

// PlanCriticalPath is a planned path matching a configured sensitive path.
type PlanCriticalPath struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

// PlanSymbolImpact is the static measure of one planned symbol at the base
// commit: callers in non-test code and tests reaching it, from the same
// approximate index as impact analysis.
type PlanSymbolImpact struct {
	Path         string         `json:"path"`
	Name         string         `json:"name"`
	Change       string         `json:"change"`
	Found        bool           `json:"found"`
	Symbol       string         `json:"symbol,omitempty"`
	Line         int            `json:"line,omitempty"`
	Exported     bool           `json:"exported"`
	Signature    string         `json:"signature,omitempty"`
	CallersTotal int            `json:"callers_total"`
	Callers      []ImpactCaller `json:"callers"`
	TestsTotal   int            `json:"tests_total"`
	Tests        []ImpactTest   `json:"tests"`
	Complete     bool           `json:"complete"`
	Reason       string         `json:"reason,omitempty"`
}

// PlanIndex records the static index behind the symbol measures.
type PlanIndex struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// PlanAssessmentBoundary records the thresholds the rules used.
type PlanAssessmentBoundary struct {
	WideImpactCallers int `json:"wide_impact_callers"`
	LargeScopeFiles   int `json:"large_scope_files"`
}

// PlanContract is what review --plan holds the implementation to.
type PlanContract struct {
	BaseCommit    string               `json:"base_commit"`
	Files         []string             `json:"files"`
	Symbols       []PlanContractSymbol `json:"symbols"`
	CriticalFiles []string             `json:"critical_files"`
	Manifests     []string             `json:"manifests"`
	Dependencies  bool                 `json:"dependencies"`
	NewPackages   []string             `json:"new_packages"`
}

// PlanContractSymbol is one announced symbol change.
type PlanContractSymbol struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Change string `json:"change"`
}

// Plan drift statuses.
const (
	PlanDriftConforming = "conforming"
	PlanDriftDrifted    = "drifted"
)

// Plan drift kinds.
const (
	DriftUnplannedFile       = "unplanned_file"
	DriftUnplannedTestFile   = "unplanned_test_file"
	DriftPlannedUntouched    = "planned_file_untouched"
	DriftUnannouncedExported = "unannounced_exported_change"
	DriftUnannouncedCritical = "unannounced_critical_path"
	DriftUnannouncedManifest = "unannounced_dependency_change"
)

// SignalPlanDrift is the linter-list kind of a drift entry that points into
// the diff, so that consumers rendering signals show it without recomputing.
const SignalPlanDrift = "plan_drift"

// PlanDrift is the report section written by review --plan. Report.Finalize
// recomputes Status and Items from Contract, CriticalGlobs, the change and
// the public_api_change signals, so an edited section is corrected on
// re-render.
type PlanDrift struct {
	Status        string          `json:"status"`
	PlanSHA256    string          `json:"plan_sha256"`
	IntentSHA256  string          `json:"intent_sha256,omitempty"`
	BaseMatches   bool            `json:"base_matches"`
	CriticalGlobs []string        `json:"critical_globs"`
	Contract      PlanContract    `json:"contract"`
	Items         []PlanDriftItem `json:"items"`
	Note          string          `json:"note"`
}

// PlanDriftItem is one difference between the diff and the plan.
type PlanDriftItem struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Symbol   string `json:"symbol,omitempty"`
	Summary  string `json:"summary"`
}
