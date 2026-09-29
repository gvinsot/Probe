// Package report decodes the SwiftProof confidence report (schema v1) and
// turns it into the view the web UI renders.
//
// The hub never re-derives conclusions: severities, statuses and counts are
// read from the report the CLI produced. It only regroups them into a single
// severity-ranked alert list, and attaches to every alert the modifications it
// concerns, so that the UI can show them without a second request.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity levels, ordered. The UI filter is a slider over these values.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// Levels lists severities from least to most severe.
var Levels = []string{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}

// Rank orders a severity; unknown values sort as low, as the CLI does.
func Rank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case SeverityCritical:
		return 3
	case SeverityHigh:
		return 2
	case SeverityMedium:
		return 1
	default:
		return 0
	}
}

// Normalize maps any recorded severity onto a known level.
func Normalize(severity string) string {
	return Levels[Rank(severity)]
}

// maxDiffLines bounds the diff carried by one view. A generated pull request
// can rewrite a whole tree; the browser still has to stay responsive.
const maxDiffLines = 40000

// Alert kinds, most conclusive first.
const (
	KindIssue  = "issue"  // a hypothesis, possibly reproduced by a differential test
	KindCheck  = "check"  // a configured check that did not pass
	KindSignal = "signal" // a deterministic risk signal from the linter
	KindFocus  = "focus"  // a prioritized review target
)

// DiffLine mirrors one line of a hunk.
type DiffLine struct {
	Kind    string `json:"kind"`
	OldLine int    `json:"old_line,omitempty"`
	NewLine int    `json:"new_line,omitempty"`
	Content string `json:"content"`
}

// Hunk mirrors one diff hunk.
type Hunk struct {
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
	Lines    []DiffLine `json:"lines"`
}

// ChangedFile mirrors one changed file of the analyzed range.
type ChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Binary    bool   `json:"binary"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Hunks     []Hunk `json:"hunks"`
}

// Change mirrors the analyzed range.
type Change struct {
	BaseRef    string        `json:"base_ref"`
	HeadRef    string        `json:"head_ref"`
	BaseCommit string        `json:"base_commit"`
	HeadCommit string        `json:"head_commit"`
	Files      []ChangedFile `json:"files"`
	Additions  int           `json:"additions"`
	Deletions  int           `json:"deletions"`
}

// ScopeFile marks what concerns a whole file rather than some of its lines.
const ScopeFile = "file"

// Signal mirrors a linter signal. A file-level signal (Scope "file") keeps a
// line only to place it on the file's first changed line; that line is not
// what the signal is about.
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

// legacyFileKinds are the kinds the CLI placed on a file's first changed line
// before it recorded a scope. Reports written then are still displayed.
var legacyFileKinds = map[string]bool{
	"sensitive_path": true, "binary_change": true, "dependency_change": true,
	"migration_change": true, "infrastructure_change": true, "file_deleted": true,
	"file_type_change": true, "large_change": true, "no_test_change": true,
	"branch_growth": true, "prepare_input_changed": true, "plan_drift": true,
}

// FileScoped reports a signal about a whole file.
func (s Signal) FileScoped() bool {
	return s.Scope == ScopeFile || s.Scope == "" && legacyFileKinds[s.Kind]
}

// Check mirrors one executed check.
type Check struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Command    []string `json:"command,omitempty"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated"`
}

// Evidence mirrors a recorded observation or differential test.
type Evidence struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Path        string   `json:"path,omitempty"`
	Output      string   `json:"output,omitempty"`
	CheckID     string   `json:"check_id,omitempty"`
	BaseCheckID string   `json:"base_check_id,omitempty"`
	Status      string   `json:"status"`
	Runner      string   `json:"runner,omitempty"`
	TestNames   []string `json:"test_names"`
}

// Hypothesis mirrors an investigated issue and the status the CLI derived.
type Hypothesis struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Status      string   `json:"status"`
	Rationale   string   `json:"rationale"`
	EvidenceIDs []string `json:"evidence_ids"`
	Path        string   `json:"path,omitempty"`
	Line        int      `json:"line,omitempty"`
}

// Signal assessment judgments, as the CLI records them. The CLI keeps no_risk
// only when a rationale cites a verified source observation.
const (
	JudgmentRisk      = "risk"
	JudgmentNoRisk    = "no_risk"
	JudgmentUncertain = "uncertain"
)

// SignalAssessment mirrors the reviewer model's plain-language reading of one
// linter signal. It is model judgment, never evidence.
type SignalAssessment struct {
	SignalID    string   `json:"signal_id"`
	Title       string   `json:"title"`
	Explanation string   `json:"explanation"`
	Judgment    string   `json:"judgment"`
	Rationale   string   `json:"rationale,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
	// AdjustedSeverity and SetAside record how the CLI lowered the signal's
	// severity after this reading (ai_impacts_criticality).
	AdjustedSeverity string `json:"adjusted_severity,omitempty"`
	SetAside         bool   `json:"set_aside,omitempty"`
}

// ReviewTarget mirrors one prioritized range of the review plan.
type ReviewTarget struct {
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Side      string   `json:"side"`
	Severity  string   `json:"severity"`
	Reasons   []string `json:"reasons"`
	SignalIDs []string `json:"signal_ids"`
}

// ReviewSurface mirrors the prioritization counters.
type ReviewSurface struct {
	ChangedLines int    `json:"changed_lines"`
	FocusedLines int    `json:"focused_lines"`
	Note         string `json:"note"`
}

// Coverage mirrors changed-line execution, never expressed as a percentage.
type Coverage struct {
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	AddedLines       int    `json:"added_lines"`
	ExecutedLines    int    `json:"executed_lines"`
	NotExecutedLines int    `json:"not_executed_lines"`
	NoBlockLines     int    `json:"no_block_lines"`
	NotMeasuredLines int    `json:"not_measured_lines"`
	RemovedLines     int    `json:"removed_lines"`
	Note             string `json:"note"`
}

// Report is the subset of the v1 confidence report the hub consumes. Unknown
// fields are ignored on purpose: a newer CLI must not break the UI.
type Report struct {
	Version          int            `json:"version"`
	ToolVersion      string         `json:"tool_version"`
	GeneratedAt      time.Time      `json:"generated_at"`
	Intent           string         `json:"intent,omitempty"`
	Change           Change         `json:"change"`
	Signals          []Signal       `json:"linter"`
	Checks           []Check        `json:"checks"`
	Hypotheses       []Hypothesis   `json:"hypotheses"`
	Evidence         []Evidence     `json:"evidence"`
	ReproducedIssues []Hypothesis   `json:"reproduced_issues"`
	Unverified       []string       `json:"unverified"`
	ReviewTargets    []ReviewTarget `json:"review_targets"`
	ReviewSurface    ReviewSurface  `json:"review_surface"`
	Coverage         Coverage       `json:"coverage"`
	ExitCode         int            `json:"exit_code"`
	// The reviewer model's reading of the linter signals and its closing
	// text. Both are model output: they never change the verdict.
	SignalAssessments []SignalAssessment `json:"signal_assessments"`
	ReviewerSummary   string             `json:"reviewer_summary,omitempty"`
}

// Decode parses a confidence report. Size is bounded by the caller.
func Decode(data []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("confidence report: %w", err)
	}
	if r.Version != 1 {
		return nil, fmt.Errorf("unsupported confidence report version %d", r.Version)
	}
	return &r, nil
}

// EvidenceRef is the trace shown under a reproduced or dismissed issue.
type EvidenceRef struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	Runner      string   `json:"runner,omitempty"`
	TestNames   []string `json:"test_names,omitempty"`
	Output      string   `json:"output,omitempty"`
}

// Alert is one entry of the severity-filtered list. Path/Line/EndLine point at
// the modifications it concerns; the UI resolves them against Files. An alert
// with Scope "file" concerns its whole file and has no line.
type Alert struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Severity string        `json:"severity"`
	Title    string        `json:"title"`
	Detail   string        `json:"detail,omitempty"`
	Path     string        `json:"path,omitempty"`
	Line     int           `json:"line,omitempty"`
	EndLine  int           `json:"end_line,omitempty"`
	Side     string        `json:"side,omitempty"`
	Scope    string        `json:"scope,omitempty"`
	Status   string        `json:"status,omitempty"`
	Reasons  []string      `json:"reasons,omitempty"`
	Evidence []EvidenceRef `json:"evidence,omitempty"`
	// The reviewer model's reading of a signal: Title then holds its plain
	// title, and OriginalTitle the linter's. When the CLI lowered the
	// signal's severity, Severity holds the adjusted one and
	// OriginalSeverity the linter's; SetAside moves it to View.Dismissed.
	OriginalTitle    string `json:"original_title,omitempty"`
	Explanation      string `json:"explanation,omitempty"`
	Judgment         string `json:"judgment,omitempty"`
	Rationale        string `json:"rationale,omitempty"`
	OriginalSeverity string `json:"original_severity,omitempty"`
	SetAside         bool   `json:"set_aside,omitempty"`
	// Members preserves each source signal when equivalent file changes are
	// presented together. The group is a display convenience, not a verdict.
	Members []Alert `json:"members,omitempty"`
}

// Counts holds how many alerts each severity carries.
type Counts struct {
	Low      int `json:"low"`
	Medium   int `json:"medium"`
	High     int `json:"high"`
	Critical int `json:"critical"`
	Total    int `json:"total"`
}

// Add records one alert of the given severity.
func (c *Counts) Add(severity string) {
	switch Normalize(severity) {
	case SeverityCritical:
		c.Critical++
	case SeverityHigh:
		c.High++
	case SeverityMedium:
		c.Medium++
	default:
		c.Low++
	}
	c.Total++
}

// AtLeast returns how many alerts reach the given severity.
func (c Counts) AtLeast(severity string) int {
	switch Rank(severity) {
	case 3:
		return c.Critical
	case 2:
		return c.Critical + c.High
	case 1:
		return c.Critical + c.High + c.Medium
	default:
		return c.Total
	}
}

// Verdicts derived from the CLI exit code, never from a model's claim.
const (
	VerdictBlocked = "blocked" // a high or critical issue was reproduced
	VerdictReview  = "review"  // something needs a human decision
	VerdictClear   = "clear"   // nothing reproduced and nothing left unverified
	VerdictFailed  = "failed"  // the analysis itself could not complete
)

// Verdict maps a CLI exit code. Clear is never a correctness claim: it only
// means this run reproduced no blocker and left no check unresolved.
func Verdict(exitCode int) string {
	switch exitCode {
	case 0:
		return VerdictClear
	case 1:
		return VerdictBlocked
	case 2:
		return VerdictReview
	default:
		return VerdictFailed
	}
}

// Summary is the compact repository-level result kept next to each commit.
type Summary struct {
	Verdict       string `json:"verdict"`
	ExitCode      int    `json:"exit_code"`
	Counts        Counts `json:"counts"`
	Reproduced    int    `json:"reproduced"`
	Unverified    int    `json:"unverified"`
	ChangedFiles  int    `json:"changed_files"`
	Additions     int    `json:"additions"`
	Deletions     int    `json:"deletions"`
	ChangedLines  int    `json:"changed_lines"`
	FocusedLines  int    `json:"focused_lines"`
	ChecksPassed  int    `json:"checks_passed"`
	ChecksFailed  int    `json:"checks_failed"`
	ToolVersion   string `json:"tool_version"`
	CoverageState string `json:"coverage_state"`
	// Suspicions counts the reviewer's unverified hypotheses, and Dismissed
	// what it set aside (see Report.Dismissed). Neither changes the verdict.
	Suspicions int `json:"suspicions"`
	Dismissed  int `json:"dismissed"`
}

// View is what the browser renders: a summary, the ranked alerts and the
// modifications each alert points into.
type View struct {
	Summary        Summary       `json:"summary"`
	Change         Change        `json:"change"`
	Alerts         []Alert       `json:"alerts"`
	Dismissed      []Alert       `json:"dismissed"`
	Files          []ChangedFile `json:"files"`
	Coverage       Coverage      `json:"coverage"`
	ReviewSurface  ReviewSurface `json:"review_surface"`
	Unverified     []string      `json:"unverified"`
	Checks         []Check       `json:"checks"`
	Intent         string        `json:"intent,omitempty"`
	DiffTruncated  bool          `json:"diff_truncated"`
	GeneratedAt    time.Time     `json:"generated_at"`
	ToolVersion    string        `json:"tool_version"`
	SeverityLevels []string      `json:"severity_levels"`
	// ReviewerSummary is the reviewer model's closing text, never evidence.
	ReviewerSummary string `json:"reviewer_summary,omitempty"`
}

// Summarize computes the compact result without building the full view.
func (r *Report) Summarize() Summary {
	s := Summary{
		Verdict:       Verdict(r.ExitCode),
		ExitCode:      r.ExitCode,
		Reproduced:    len(r.ReproducedIssues),
		Unverified:    len(r.Unverified),
		ChangedFiles:  len(r.Change.Files),
		Additions:     r.Change.Additions,
		Deletions:     r.Change.Deletions,
		ChangedLines:  r.ReviewSurface.ChangedLines,
		FocusedLines:  r.ReviewSurface.FocusedLines,
		ToolVersion:   r.ToolVersion,
		CoverageState: r.Coverage.Status,
	}
	for _, c := range r.Checks {
		if c.Status == "PASS" && c.ExitCode == 0 {
			s.ChecksPassed++
		} else {
			s.ChecksFailed++
		}
	}
	active, dismissed := r.alertLists()
	for _, a := range active {
		s.Counts.Add(a.Severity)
	}
	s.Dismissed = len(dismissed)
	for _, h := range r.Hypotheses {
		if strings.EqualFold(h.Status, "UNVERIFIED") {
			s.Suspicions++
		}
	}
	return s
}

// Alerts flattens issues, failed checks, signals and review targets into one
// ranked list. Ordering is by severity, then by conclusiveness, then by
// location, so the first screen always holds what matters most. What the
// reviewer set aside is listed by Dismissed instead.
func (r *Report) Alerts() []Alert {
	active, _ := r.alertLists()
	return active
}

// Dismissed lists what the reviewer model set aside: the low signals the CLI
// set aside after a no_risk reading (ai_impacts_criticality), and its DISMISSED
// hypotheses. They are shown apart, never deleted. The hub decides nothing
// here: it follows the set_aside and status fields the CLI recorded, and the
// verdict still comes from the CLI exit code.
func (r *Report) Dismissed() []Alert {
	_, dismissed := r.alertLists()
	return dismissed
}

// alertLists builds the active and the dismissed alerts, both ranked. Only
// the active list is deduplicated per line and grouped by file change: a
// dismissed alert hides nothing.
func (r *Report) alertLists() (active, dismissed []Alert) {
	all := r.allAlerts()
	active = make([]Alert, 0, len(all))
	for _, a := range all {
		if a.SetAside || a.Kind == KindIssue && strings.EqualFold(a.Status, "DISMISSED") {
			dismissed = append(dismissed, a)
			continue
		}
		active = append(active, a)
	}
	return r.groupFileSignals(dedupeLines(active)), dismissed
}

// allAlerts builds every alert, ranked and not deduplicated.
func (r *Report) allAlerts() []Alert {
	evidence := make(map[string]Evidence, len(r.Evidence))
	for _, e := range r.Evidence {
		evidence[e.ID] = e
	}
	alerts := make([]Alert, 0, len(r.Hypotheses)+len(r.Signals)+len(r.ReviewTargets))
	for _, h := range r.Hypotheses {
		a := Alert{
			ID:       "issue:" + h.ID,
			Kind:     KindIssue,
			Severity: Normalize(h.Severity),
			Title:    h.Title,
			Detail:   h.Rationale,
			Path:     h.Path,
			Line:     h.Line,
			EndLine:  h.Line,
			Side:     "new",
			Status:   h.Status,
		}
		for _, id := range h.EvidenceIDs {
			e, ok := evidence[id]
			if !ok {
				continue
			}
			a.Evidence = append(a.Evidence, EvidenceRef{
				ID: e.ID, Kind: e.Kind, Status: e.Status, Description: e.Description,
				Runner: e.Runner, TestNames: e.TestNames, Output: e.Output,
			})
		}
		alerts = append(alerts, a)
	}
	for _, c := range r.Checks {
		if c.Status == "PASS" && c.ExitCode == 0 {
			continue
		}
		severity := SeverityHigh
		if c.Status == "SKIPPED" {
			severity = SeverityMedium
		}
		alerts = append(alerts, Alert{
			ID:       "check:" + c.ID,
			Kind:     KindCheck,
			Severity: severity,
			Title:    checkTitle(c),
			Detail:   c.Output,
			Status:   c.Status,
		})
	}
	assessments := make(map[string]SignalAssessment, len(r.SignalAssessments))
	for _, a := range r.SignalAssessments {
		if _, dup := assessments[a.SignalID]; !dup && strings.TrimSpace(a.Title) != "" {
			assessments[a.SignalID] = a
		}
	}
	summaries := make(map[string]string, len(r.Signals))
	for _, s := range r.Signals {
		summaries[s.ID] = s.Summary
		a := Alert{
			ID:       "signal:" + s.ID,
			Kind:     KindSignal,
			Severity: Normalize(s.Severity),
			Title:    signalTitle(s),
			Detail:   s.Evidence,
			Path:     s.Path,
			Status:   "OBSERVED",
			Reasons:  []string{s.Kind},
		}
		if assessment, ok := assessments[s.ID]; ok {
			a.OriginalTitle, a.Title = a.Title, assessment.Title
			a.Explanation, a.Rationale = assessment.Explanation, assessment.Rationale
			a.Judgment = assessment.Judgment
			if a.Judgment != JudgmentRisk && a.Judgment != JudgmentNoRisk {
				a.Judgment = JudgmentUncertain
			}
			if assessment.AdjustedSeverity != "" {
				a.OriginalSeverity, a.Severity = a.Severity, Normalize(assessment.AdjustedSeverity)
			}
			a.SetAside = assessment.SetAside
		}
		if s.FileScoped() {
			// Its anchor line would single out a line it says nothing about.
			a.Scope = ScopeFile
		} else {
			a.Line, a.EndLine, a.Side = s.Line, s.EndLine, s.Side
			if a.EndLine < a.Line {
				a.EndLine = a.Line
			}
			if a.Side == "" {
				a.Side = "new"
			}
		}
		alerts = append(alerts, a)
	}
	for i, t := range r.ReviewTargets {
		reasons := targetReasons(t, summaries, r.Hypotheses)
		if len(reasons) == 0 {
			continue
		}
		a := Alert{
			ID:       fmt.Sprintf("focus:%d", i),
			Kind:     KindFocus,
			Severity: Normalize(t.Severity),
			Path:     t.Path,
			Reasons:  reasons,
		}
		if t.StartLine <= 0 {
			a.Title, a.Scope = t.Path, ScopeFile
		} else {
			a.Title = fmt.Sprintf("%s:%d-%d", t.Path, t.StartLine, t.EndLine)
			a.Line, a.EndLine, a.Side = t.StartLine, t.EndLine, t.Side
			if a.Side == "" {
				a.Side = "new"
			}
		}
		alerts = append(alerts, a)
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if ra, rb := Rank(a.Severity), Rank(b.Severity); ra != rb {
			return ra > rb
		}
		if ka, kb := kindRank(a), kindRank(b); ka != kb {
			return ka < kb
		}
		if sa, sb := statusRank(a.Status), statusRank(b.Status); sa != sb {
			return sa < sb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return alerts
}

// targetReasons keeps the reasons of a review target that no other alert
// shows. The CLI merges into one target the signals and issues of adjacent
// lines, and places file-level signals on their anchor line, so a target's
// reasons are not all about its lines; its signals and issues are alerts of
// their own, at their own location. A target left without a reason of its own
// is not shown.
func targetReasons(t ReviewTarget, summaries map[string]string, hypotheses []Hypothesis) []string {
	shown := map[string]bool{}
	for _, id := range t.SignalIDs {
		if summary, ok := summaries[id]; ok {
			shown[summary] = true
		}
	}
	for _, h := range hypotheses {
		if h.Path == t.Path {
			shown[h.Title] = true
		}
	}
	var out []string
	for _, reason := range t.Reasons {
		if !shown[reason] {
			out = append(out, reason)
		}
	}
	return out
}

// lineSpan is an inclusive range of lines of one side of one file.
type lineSpan struct{ start, end int }

// dedupeLines keeps a single alert per line of code: alerts arrive ranked
// most severe first, and each later alert keeps only the lines no earlier
// alert already reported. An alert whose lines are all reported is dropped;
// one whose range is partly reported is narrowed, or split into the parts
// still unreported. Alerts without a line (checks, and alerts about a whole
// file) are kept as they are and hold no line. An issue is never dropped or
// narrowed: it is a finding of its own (reproduced evidence, or a risk the
// reviewer described), not a pointer to lines; it still reports its lines
// to the alerts that follow.
// Only the list shrinks; the verdict still comes from the CLI exit code.
func dedupeLines(alerts []Alert) []Alert {
	covered := map[string][]lineSpan{}
	out := make([]Alert, 0, len(alerts))
	for _, a := range alerts {
		if a.Path == "" || a.Line <= 0 {
			out = append(out, a)
			continue
		}
		end := a.EndLine
		if end < a.Line {
			end = a.Line
		}
		key := a.Side + "\x00" + a.Path
		spans := uncovered(lineSpan{a.Line, end}, covered[key])
		covered[key] = append(covered[key], lineSpan{a.Line, end})
		if a.Kind == KindIssue {
			out = append(out, a)
			continue
		}
		if len(spans) == 0 {
			continue
		}
		for i, sp := range spans {
			part := a
			part.Line, part.EndLine = sp.start, sp.end
			if len(spans) > 1 {
				part.ID = fmt.Sprintf("%s#%d", a.ID, i+1)
			}
			if a.Kind == KindFocus {
				part.Title = fmt.Sprintf("%s:%d-%d", a.Path, sp.start, sp.end)
			}
			out = append(out, part)
		}
	}
	return out
}

// uncovered returns the parts of span that none of the covered spans holds.
func uncovered(span lineSpan, covered []lineSpan) []lineSpan {
	rest := []lineSpan{span}
	for _, c := range covered {
		next := rest[:0:0]
		for _, r := range rest {
			if c.end < r.start || c.start > r.end {
				next = append(next, r)
				continue
			}
			if r.start < c.start {
				next = append(next, lineSpan{r.start, c.start - 1})
			}
			if r.end > c.end {
				next = append(next, lineSpan{c.end + 1, r.end})
			}
		}
		rest = next
	}
	return rest
}

func checkTitle(c Check) string {
	name := c.Kind
	if name == "" {
		name = c.ID
	}
	return fmt.Sprintf("Check %s: %s (exit %d)", name, strings.ToLower(c.Status), c.ExitCode)
}

func signalTitle(s Signal) string {
	if s.Symbol != "" {
		return fmt.Sprintf("%s — %s", s.Summary, s.Symbol)
	}
	return s.Summary
}

func kindRank(a Alert) int {
	switch a.Kind {
	case KindIssue:
		return 0
	case KindCheck:
		return 1
	case KindSignal:
		return 2
	default:
		return 3
	}
}

// A reproduced issue outranks an unverified hypothesis of the same severity:
// one is evidence, the other is a question.
func statusRank(status string) int {
	switch strings.ToUpper(status) {
	case "REPRODUCED":
		return 0
	case "UNVERIFIED":
		return 1
	case "FAIL", "ERROR", "TIMEOUT":
		return 2
	case "NOT_REPRODUCED":
		return 4
	case "DISMISSED":
		return 5
	default:
		return 3
	}
}

// BuildView assembles everything the report page needs in one payload.
func (r *Report) BuildView() View {
	v := View{
		Summary:         r.Summarize(),
		Change:          Change{BaseRef: r.Change.BaseRef, HeadRef: r.Change.HeadRef, BaseCommit: r.Change.BaseCommit, HeadCommit: r.Change.HeadCommit, Additions: r.Change.Additions, Deletions: r.Change.Deletions},
		Coverage:        r.Coverage,
		ReviewSurface:   r.ReviewSurface,
		Unverified:      r.Unverified,
		Checks:          r.Checks,
		Intent:          r.Intent,
		GeneratedAt:     r.GeneratedAt,
		ToolVersion:     r.ToolVersion,
		SeverityLevels:  Levels,
		ReviewerSummary: strings.TrimSpace(r.ReviewerSummary),
	}
	v.Alerts, v.Dismissed = r.alertLists()
	if v.Dismissed == nil {
		v.Dismissed = []Alert{}
	}
	budget := maxDiffLines
	v.Files = make([]ChangedFile, 0, len(r.Change.Files))
	for _, f := range r.Change.Files {
		kept := ChangedFile{Path: f.Path, OldPath: f.OldPath, Status: f.Status, Binary: f.Binary, Additions: f.Additions, Deletions: f.Deletions}
		for _, h := range f.Hunks {
			if len(h.Lines) > budget {
				v.DiffTruncated = true
				break
			}
			budget -= len(h.Lines)
			kept.Hunks = append(kept.Hunks, h)
		}
		v.Files = append(v.Files, kept)
	}
	return v
}
