// Package model defines the versioned, provider-independent review evidence format.
package model

import "time"

type DiffLine struct {
	Kind    string `json:"kind"`
	OldLine int    `json:"old_line,omitempty"`
	NewLine int    `json:"new_line,omitempty"`
	Content string `json:"content"`
}
type Hunk struct {
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
	Lines    []DiffLine `json:"lines"`
}
type ChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Binary    bool   `json:"binary"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Hunks     []Hunk `json:"hunks"`
}
type Change struct {
	BaseRef    string `json:"base_ref"`
	HeadRef    string `json:"head_ref"`
	BaseCommit string `json:"base_commit"`
	HeadCommit string `json:"head_commit"`
	// BaseRefCommit is the commit BaseRef resolved to. It differs from
	// BaseCommit when the comparison starts at the merge base.
	BaseRefCommit string        `json:"base_ref_commit,omitempty"`
	Files         []ChangedFile `json:"files"`
	Additions     int           `json:"additions"`
	Deletions     int           `json:"deletions"`
}

// Policy sources recorded in a report.
const (
	PolicyBaseRef  = "base_ref"
	PolicyExplicit = "explicit"
	PolicyDefault  = "default"
)

// Policy records where the trusted policy of a run came from.
type Policy struct {
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
	Path   string `json:"path,omitempty"`
}

// SignalScopeFile marks a signal about a whole file, such as a sensitive path
// or a missing test change. Its Line and Side only place it on the file's first
// changed line for tools that need a line; they say nothing about that line.
const SignalScopeFile = "file"

type Signal struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	EndLine  int    `json:"end_line,omitempty"`
	Side     string `json:"side,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Evidence string `json:"evidence"`
}
type Check struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Command    []string `json:"command,omitempty"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated"`
	// Results is the normalized structured output a verifiable runner returned on
	// the payload channel. It is kept apart from Output so that log text cannot
	// impersonate it; code executing in the sandbox can still write it.
	Results string      `json:"results,omitempty"`
	Cache   *CheckCache `json:"cache,omitempty"` // F7a: present only on base-side checks that were stored or replayed
}
type Evidence struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	Description       string   `json:"description"`
	Path              string   `json:"path,omitempty"`
	Output            string   `json:"output,omitempty"`
	CheckID           string   `json:"check_id,omitempty"`
	BaseCheckID       string   `json:"base_check_id,omitempty"`
	RepeatCheckID     string   `json:"repeat_check_id,omitempty"`    // F1: live baseline repeat
	CriterionID       string   `json:"criterion_id,omitempty"`       // F5
	ReferencedSymbols []string `json:"referenced_symbols,omitempty"` // F5: changed symbols the intent test references
	Status            string   `json:"status"`
	Runner            string   `json:"runner,omitempty"`
	TestNames         []string `json:"test_names"`
}
type Hypothesis struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Severity       string   `json:"severity"`
	Status         string   `json:"status"`
	Rationale      string   `json:"rationale"`
	EvidenceIDs    []string `json:"evidence_ids"`
	Path           string   `json:"path,omitempty"`
	Line           int      `json:"line,omitempty"`
	CriterionID    string   `json:"criterion_id,omitempty"`    // F5
	IntentJudgment string   `json:"intent_judgment,omitempty"` // F5: model judgment, never evidence; only on DIVERGED
	// Agents names the reviewer swarm agents that submitted the hypothesis
	// (several when agents submitted the same finding); empty for a single
	// reviewer. Attribution, never evidence.
	Agents []string `json:"agents,omitempty"`
}

// ReviewerAgent records one agent of a reviewer swarm: what it was asked to
// examine and how its investigation ended. It is a description of the run,
// never evidence.
type ReviewerAgent struct {
	Name       string   `json:"name"`
	Focus      string   `json:"focus"`
	Paths      []string `json:"paths,omitempty"` // the changed files it owned; empty: all
	Status     string   `json:"status"`          // completed or incomplete
	Hypotheses int      `json:"hypotheses"`      // hypotheses it submitted, before merging
	Note       string   `json:"note,omitempty"`
}

// PRSummary is a model-written, natural-language summary of the change, for
// a pull request description. It is generated after the verdict and changes
// nothing in it: model output, never evidence.
type PRSummary struct {
	Title           string            `json:"title"`
	Overview        string            `json:"overview"`
	Changes         []PRSummaryChange `json:"changes"`          // always an array
	BehaviorChanges []string          `json:"behavior_changes"` // always an array
	Risks           []string          `json:"risks"`            // always an array
	ReviewFocus     []string          `json:"review_focus"`     // always an array
	Testing         string            `json:"testing,omitempty"`
	Intents         []PRSummaryIntent `json:"intents,omitempty"`
	Model           string            `json:"model"`
}

// PRSummaryChange is one area of a PR summary.
type PRSummaryChange struct {
	Area    string   `json:"area"`
	Summary string   `json:"summary"`
	Files   []string `json:"files"` // changed files only; always an array
}

// PRSummaryIntent groups the report's linter signals and hypotheses under the
// developer intention that produced the code they point at ("Add agent
// sorting", "Test agent sorting"). It cites recorded IDs only, and each ID
// belongs to at most one intent. A reading of the change, never evidence.
type PRSummaryIntent struct {
	Intent        string   `json:"intent"`
	SignalIDs     []string `json:"signal_ids"`     // always an array
	HypothesisIDs []string `json:"hypothesis_ids"` // always an array
}

// Reviewer agent statuses.
const (
	AgentCompleted  = "completed"
	AgentIncomplete = "incomplete"
)

// Signal assessment judgments.
const (
	AssessmentRisk      = "risk"
	AssessmentNoRisk    = "no_risk"
	AssessmentUncertain = "uncertain"
)

// SignalAssessment is the reviewer model's plain-language reading of one
// linter signal. It is a model judgment, never evidence: it changes no
// hypothesis status. Finalize keeps no_risk only when a non-blank rationale
// cites a verified source observation; with Report.AIImpactsCriticality, a
// kept no_risk lowers the signal's severity by one level, and sets a low
// signal aside.
type SignalAssessment struct {
	SignalID    string   `json:"signal_id"`
	Title       string   `json:"title"`
	Explanation string   `json:"explanation"`
	Judgment    string   `json:"judgment"`
	Rationale   string   `json:"rationale,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
	// AdjustedSeverity is the severity Finalize derives conclusions from
	// when Report.AIImpactsCriticality lowered the signal's by one level;
	// SetAside marks a low signal read as no_risk, which is no review target.
	// The signal itself keeps the linter's severity.
	AdjustedSeverity string `json:"adjusted_severity,omitempty"`
	SetAside         bool   `json:"set_aside,omitempty"`
}
type ReviewTarget struct {
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Side      string   `json:"side"`
	Severity  string   `json:"severity"`
	Reasons   []string `json:"reasons"`
	SignalIDs []string `json:"signal_ids"`
}

// CoverageFile records how the added lines of one changed file resolved
// against a recorded coverage profile.
type CoverageFile struct {
	Path             string `json:"path"`
	Status           string `json:"status"`
	AddedLines       int    `json:"added_lines"`
	ExecutedLines    int    `json:"executed_lines"`
	NotExecutedLines int    `json:"not_executed_lines"`
	NoBlockLines     int    `json:"no_block_lines"`
	NotMeasuredLines int    `json:"not_measured_lines"`
}

// Coverage records which added lines a recorded run executed. Executed means a
// line ran at least once; it is never a claim that behavior is asserted,
// correct or safe. Removed lines have no candidate-side coordinate and are
// reported beside the four states rather than inside them.
type Coverage struct {
	Status           string         `json:"status"`
	Format           string         `json:"format,omitempty"`
	Reason           string         `json:"reason,omitempty"`
	CheckID          string         `json:"check_id,omitempty"`
	ProfileSHA256    string         `json:"profile_sha256,omitempty"`
	Command          []string       `json:"command,omitempty"`
	AddedLines       int            `json:"added_lines"`
	ExecutedLines    int            `json:"executed_lines"`
	NotExecutedLines int            `json:"not_executed_lines"`
	NoBlockLines     int            `json:"no_block_lines"`
	NotMeasuredLines int            `json:"not_measured_lines"`
	RemovedLines     int            `json:"removed_lines"`
	Files            []CoverageFile `json:"files"`
	Note             string         `json:"note"`
}
type Artifact struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
}
type AuditEvent struct {
	Time       time.Time `json:"time"`
	Tool       string    `json:"tool"`
	Arguments  string    `json:"arguments"`
	Status     string    `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	// Agent names the reviewer swarm agent whose call this was; empty for
	// harness stages and for a single reviewer.
	Agent string `json:"agent,omitempty"`
}
type ReviewSurface struct {
	ChangedLines int `json:"changed_lines"`
	FocusedLines int `json:"focused_lines"`
	// Note is no longer written; it is kept so that older reports still read.
	Note string `json:"note,omitempty"`
}

// Report is the confidence report. The JSON object of an opt-in feature
// (prepare, base_tests, mutation, fuzz, impact, execution) is present exactly
// when that feature was requested or configured for the run; its status then
// says what happened. intent_criteria, divergences and intent_test_failures
// always serialize as arrays.
type Report struct {
	Version        int               `json:"version"` // stays 1 (additive change)
	ToolVersion    string            `json:"tool_version"`
	AnalysisMode   string            `json:"analysis_mode,omitempty"`
	GeneratedAt    time.Time         `json:"generated_at"`
	Intent         string            `json:"intent,omitempty"`
	IntentSHA256   string            `json:"intent_sha256,omitempty"` // F5
	IntentCriteria []IntentCriterion `json:"intent_criteria"`         // F5, always an array
	// CodingRules are the team coding rules given to the reviewer with
	// --rules or --rules-file; recorded only when a reviewer ran.
	CodingRules       string `json:"coding_rules,omitempty"`
	CodingRulesSHA256 string `json:"coding_rules_sha256,omitempty"`
	// TeamFeedback is the team's feedback given to the reviewer with
	// --feedback-file; recorded only when a reviewer ran.
	TeamFeedback       *TeamFeedback `json:"team_feedback,omitempty"`
	TeamFeedbackSHA256 string        `json:"team_feedback_sha256,omitempty"`
	// Knowledge records the codebase knowledge base given to the reviewer
	// and the updates it proposed; present only when a reviewer ran.
	Knowledge *Knowledge `json:"knowledge,omitempty"`
	// ContextRepos lists the other repositories the reviewer could read as
	// cross-repository context (policy "context"); present when a reviewer ran.
	ContextRepos []ContextRepo `json:"context_repos,omitempty"`
	// Graph is the repository-graph section; nil with --graph=false.
	Graph              *Graph             `json:"graph,omitempty"`
	Change             Change             `json:"change"`
	Policy             Policy             `json:"policy"`
	Prepare            *Prepare           `json:"prepare,omitempty"` // F8
	Signals            []Signal           `json:"linter"`
	Checks             []Check            `json:"checks"`
	Hypotheses         []Hypothesis       `json:"hypotheses"`
	SignalAssessments  []SignalAssessment `json:"signal_assessments"`         // model judgment, always an array
	ReviewerSummary    string             `json:"reviewer_summary,omitempty"` // the model's closing text, never evidence
	ReviewerAgents     []ReviewerAgent    `json:"reviewer_agents,omitempty"`  // the reviewer swarm, when one ran
	PRSummary          *PRSummary         `json:"pr_summary,omitempty"`       // the model's narrative of the change, never evidence
	Evidence           []Evidence         `json:"evidence"`
	ReproducedIssues   []Hypothesis       `json:"reproduced_issues"`
	BaseTests          *BaseTests         `json:"base_tests,omitempty"` // F3
	Divergences        []Divergence       `json:"divergences"`          // F1/F2, always an array
	IntentTestFailures []Hypothesis       `json:"intent_test_failures"` // F5, always an array
	Unverified         []string           `json:"unverified"`
	ReviewTargets      []ReviewTarget     `json:"review_targets"`
	ReviewSurface      ReviewSurface      `json:"review_surface"`
	Coverage           Coverage           `json:"coverage"`
	Mutation           *Mutation          `json:"mutation,omitempty"`   // F4
	Fuzz               *FuzzReport        `json:"fuzz,omitempty"`       // F2
	Impact             *Impact            `json:"impact,omitempty"`     // F6a
	Execution          *Execution         `json:"execution,omitempty"`  // F7a
	PlanDrift          *PlanDrift         `json:"plan_drift,omitempty"` // review --plan
	Artifacts          []Artifact         `json:"artifacts"`
	Audit              []AuditEvent       `json:"audit"`
	ExitCode           int                `json:"exit_code"`
	// AIImpactsCriticality records --ai-impacts-criticality: a no_risk reading
	// lowers its signal's severity (see SignalAssessment).
	AIImpactsCriticality bool `json:"ai_impacts_criticality,omitempty"`
}
