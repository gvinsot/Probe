package report

// SARIF 2.1.0 export (confidence-report.sarif), a minimal subset.
//
// Results are the anchored evidence-backed findings only (findings.go). Every
// result has exactly one location, in a changed, non-deleted file of the
// recorded change, and a plain-text message (message.text, never markdown).
// Findings without such a location, unverified areas, checks that did not
// pass, stages that did not run and the absence of any execution are
// tool-execution notifications, never results. No rule or result carries a
// precision, a security severity, a confidence or a percentage. An empty
// result list is not an approval, and GitHub's "fixed" state of an alert is not
// a SwiftProof claim.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

const (
	sarifSchema         = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion        = "2.1.0"
	swiftProofURI       = "https://github.com/gvinsot/SwiftProof"
	sourceRootBase      = "%SRCROOT%"
	reportDirBase       = "SWIFTPROOF_REPORT"
	fingerprintKey      = "swiftproof/v1"
	maxSARIFResults     = maxFindings
	maxSARIFMessage     = 1024
	maxNotifications    = 100
	maxNotificationText = 1024
	sarifRunNote        = "Only evidence-backed findings are listed as results. An empty result list is not an approval and does not establish that the change is correct; GitHub's fixed state of an alert is not a SwiftProof claim. A re-rendered or fork-produced file is not authenticated."
)

type sarifLog struct {
	Schema     string        `json:"$schema"`
	Version    string        `json:"version"`
	Runs       []sarifRun    `json:"runs"`
	Properties sarifLogProps `json:"properties"`
}

type sarifLogProps struct {
	Attribution string `json:"attribution"`
}

type sarifRun struct {
	Tool               sarifTool                        `json:"tool"`
	Invocations        []sarifInvocation                `json:"invocations"`
	OriginalURIBaseIDs map[string]sarifArtifactLocation `json:"originalUriBaseIds"`
	ColumnKind         string                           `json:"columnKind"`
	Results            []sarifResult                    `json:"results"`
	Properties         sarifRunProps                    `json:"properties"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name            string      `json:"name"`
	Version         string      `json:"version,omitempty"`
	SemanticVersion string      `json:"semanticVersion,omitempty"`
	InformationURI  string      `json:"informationUri"`
	Rules           []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	ShortDescription     sarifMessage   `json:"shortDescription"`
	FullDescription      sarifMessage   `json:"fullDescription"`
	Help                 sarifMessage   `json:"help"`
	DefaultConfiguration sarifConfig    `json:"defaultConfiguration"`
	Properties           sarifRuleProps `json:"properties"`
}

type sarifRuleProps struct {
	Tags []string `json:"tags"`
}

type sarifConfig struct {
	Level string `json:"level"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifInvocation struct {
	ExecutionSuccessful        bool                `json:"executionSuccessful"`
	ExitCode                   int                 `json:"exitCode"`
	ToolExecutionNotifications []sarifNotification `json:"toolExecutionNotifications"`
}

type sarifNotification struct {
	Level      string                 `json:"level"`
	Message    sarifMessage           `json:"message"`
	Properties sarifNotificationProps `json:"properties"`
}

type sarifNotificationProps struct {
	Kind string `json:"swiftproof_kind"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	RelatedLocations    []sarifLocation   `json:"relatedLocations,omitempty"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Properties          sarifResultProps  `json:"properties"`
}

type sarifLocation struct {
	ID               int           `json:"id,omitempty"`
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
	Message          *sarifMessage `json:"message,omitempty"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI         string        `json:"uri,omitempty"`
	URIBaseID   string        `json:"uriBaseId,omitempty"`
	Description *sarifMessage `json:"description,omitempty"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
}

type sarifResultProps struct {
	SwiftProof sarifFindingProps `json:"swiftproof"`
}

type sarifFindingProps struct {
	Class             string          `json:"class"`
	Status            string          `json:"status"`
	Severity          string          `json:"severity,omitempty"`
	SeveritySource    string          `json:"severity_source,omitempty"`
	Title             string          `json:"title,omitempty"`
	TitleSource       string          `json:"title_source,omitempty"`
	Statement         string          `json:"statement,omitempty"`
	LocationSource    string          `json:"location_source"`
	LocationPrecision string          `json:"location_precision"`
	EvidenceIDs       []string        `json:"evidence_ids"`
	CheckIDs          []string        `json:"check_ids"`
	ReplayedChecks    []string        `json:"replayed_checks,omitempty"`
	HypothesisIDs     []string        `json:"hypothesis_ids,omitempty"`
	CriterionID       string          `json:"criterion_id,omitempty"`
	MutantID          string          `json:"mutant_id,omitempty"`
	Details           []sarifDetail   `json:"details"`
	Artifacts         []sarifArtifact `json:"artifacts,omitempty"`
	Truncated         bool            `json:"truncated,omitempty"`
}

type sarifDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type sarifArtifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

type sarifRunProps struct {
	SwiftProof sarifRunSummary `json:"swiftproof"`
}

type sarifRunSummary struct {
	ReportVersion        int    `json:"report_version"`
	ToolVersion          string `json:"tool_version,omitempty"`
	GeneratedAt          string `json:"generated_at"`
	BaseRef              string `json:"base_ref,omitempty"`
	HeadRef              string `json:"head_ref,omitempty"`
	BaseCommit           string `json:"base_commit,omitempty"`
	HeadCommit           string `json:"head_commit,omitempty"`
	BaseRefCommit        string `json:"base_ref_commit,omitempty"`
	PolicySource         string `json:"policy_source,omitempty"`
	PolicyCommit         string `json:"policy_commit,omitempty"`
	ExitCode             int    `json:"exit_code"`
	Findings             int    `json:"findings"`
	UnanchoredFindings   int    `json:"unanchored_findings"`
	OmittedFindings      int    `json:"omitted_findings"`
	UnverifiedAreas      int    `json:"unverified_areas"`
	UnverifiedHypotheses int    `json:"unverified_hypotheses"`
	ChecksNotPassed      int    `json:"checks_not_passed"`
	StagesNotRun         int    `json:"stages_not_run"`
	RiskSignals          int    `json:"risk_signals"`
	ReviewTargets        int    `json:"review_targets"`
	Note                 string `json:"note"`
}

// renderSARIF renders the SARIF log of a sanitized, finalized report.
func renderSARIF(r *model.Report, set findingSet) ([]byte, error) {
	status := statusOf(r)
	var anchored []sarifResult
	var unanchored []finding
	used := map[string]bool{}
	var pending []struct {
		f   finding
		uri string
	}
	omitted := set.omitted
	for _, f := range set.findings {
		uri, ok := "", false
		if f.anchor != nil {
			uri, ok = artifactURI(f.anchor.Path)
		}
		if !ok {
			unanchored = append(unanchored, f)
			continue
		}
		if len(pending) == maxSARIFResults {
			omitted++
			continue
		}
		pending = append(pending, struct {
			f   finding
			uri string
		}{f, uri})
		used[f.Class] = true
	}
	var rules []sarifRule
	ruleIndex := map[string]int{}
	for _, cls := range findingClasses {
		if !used[cls.Class] {
			continue
		}
		ruleIndex[cls.Class] = len(rules)
		rules = append(rules, sarifRuleFor(cls))
	}
	for _, p := range pending {
		cls, _ := classByName(p.f.Class)
		anchored = append(anchored, sarifResultFor(p.f, cls, ruleIndex[cls.Class], p.uri))
	}
	if anchored == nil {
		anchored = []sarifResult{}
	}
	if rules == nil {
		rules = []sarifRule{}
	}
	summary := sarifRunSummary{
		ReportVersion: r.Version, ToolVersion: bounded(r.ToolVersion, 128), GeneratedAt: r.GeneratedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		BaseRef: bounded(r.Change.BaseRef, 256), HeadRef: bounded(r.Change.HeadRef, 256),
		BaseCommit: bounded(r.Change.BaseCommit, 128), HeadCommit: bounded(r.Change.HeadCommit, 128), BaseRefCommit: bounded(r.Change.BaseRefCommit, 128),
		PolicySource: bounded(r.Policy.Source, 64), PolicyCommit: bounded(r.Policy.Commit, 128),
		ExitCode: r.ExitCode, Findings: len(anchored), UnanchoredFindings: len(unanchored), OmittedFindings: omitted,
		UnverifiedAreas: status.UnverifiedAreas(), UnverifiedHypotheses: len(status.UnverifiedHypotheses),
		ChecksNotPassed: len(status.ChecksNotPassed), StagesNotRun: len(status.StagesNotRun),
		RiskSignals: status.RiskSignals, ReviewTargets: status.ReviewTargets, Note: sarifRunNote,
	}
	driver := sarifDriver{Name: "SwiftProof", Version: bounded(r.ToolVersion, 128), InformationURI: swiftProofURI, Rules: rules}
	if semanticVersion.MatchString(r.ToolVersion) {
		driver.SemanticVersion = strings.TrimPrefix(r.ToolVersion, "v")
	}
	log := sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: driver},
			Invocations: []sarifInvocation{{
				ExecutionSuccessful:        status.ExecutionSuccessful,
				ExitCode:                   r.ExitCode,
				ToolExecutionNotifications: sarifNotifications(status, unanchored, omitted),
			}},
			OriginalURIBaseIDs: map[string]sarifArtifactLocation{
				sourceRootBase: {Description: &sarifMessage{"The repository root at the candidate commit."}},
				reportDirBase:  {Description: &sarifMessage{"The SwiftProof report directory (--out), which holds the retained artifacts."}},
			},
			ColumnKind: "utf16CodeUnits",
			Results:    anchored,
			Properties: sarifRunProps{summary},
		}},
		Properties: sarifLogProps{Attribution: sarifAttribution(r.ToolVersion)},
	}
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var semanticVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// sarifAttribution is the attribution notice of NOTICE 7(b) as plain text.
func sarifAttribution(version string) string {
	version = strings.TrimSpace(bounded(version, 128))
	if version == "" {
		return "Generated by SwiftProof (" + swiftProofURI + ")."
	}
	return "Generated by SwiftProof " + version + " (" + swiftProofURI + ")."
}

func sarifRuleFor(cls findingClass) sarifRule {
	return sarifRule{
		ID: cls.RuleID, Name: cls.RuleName,
		ShortDescription:     sarifMessage{cls.Short},
		FullDescription:      sarifMessage{cls.Full},
		Help:                 sarifMessage{cls.Help},
		DefaultConfiguration: sarifConfig{cls.DefaultLevel},
		Properties:           sarifRuleProps{Tags: []string{"swiftproof", cls.Class}},
	}
}

func sarifResultFor(f finding, cls findingClass, index int, uri string) sarifResult {
	region := &sarifRegion{StartLine: 1, EndLine: 1}
	precision := "file"
	if !f.anchor.FileLevel {
		region = &sarifRegion{StartLine: f.anchor.Line, EndLine: f.anchor.EndLine}
		precision = "line"
	}
	props := sarifFindingProps{
		Class: f.Class, Status: f.Status, Statement: f.Statement,
		LocationSource: f.anchor.Source, LocationPrecision: precision,
		EvidenceIDs: nonNil(f.EvidenceIDs), CheckIDs: nonNil(f.CheckIDs), ReplayedChecks: f.replayed,
		HypothesisIDs: f.HypothesisIDs, CriterionID: f.CriterionID, MutantID: f.MutantID,
		Details: []sarifDetail{}, Truncated: f.truncated,
	}
	if f.Severity != "" {
		props.Severity, props.SeveritySource = f.Severity, "reviewer model"
	}
	if f.Title != "" {
		props.Title, props.TitleSource = f.Title, "reviewer model"
	}
	for _, d := range f.Details {
		props.Details = append(props.Details, sarifDetail{d.Label, d.Value})
	}
	result := sarifResult{
		RuleID: cls.RuleID, RuleIndex: index, Level: cls.level(f),
		Message: sarifMessage{sarifResultMessage(f, cls)},
		Locations: []sarifLocation{{PhysicalLocation: sarifPhysical{
			ArtifactLocation: sarifArtifactLocation{URI: uri, URIBaseID: sourceRootBase},
			Region:           region,
		}}},
		PartialFingerprints: map[string]string{fingerprintKey: f.fingerprint},
		Properties:          sarifResultProps{props},
	}
	for _, a := range f.Artifacts {
		props := sarifArtifact{Kind: bounded(a.Kind, 64), Path: bounded(a.Path, 512), SHA256: bounded(a.SHA256, 64)}
		result.Properties.SwiftProof.Artifacts = append(result.Properties.SwiftProof.Artifacts, props)
		if uri, ok := artifactURI(a.Path); ok {
			result.RelatedLocations = append(result.RelatedLocations, sarifLocation{
				ID:               len(result.RelatedLocations) + 1,
				PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifactLocation{URI: uri, URIBaseID: reportDirBase}},
				Message:          &sarifMessage{sarifText(fmt.Sprintf("Retained %s artifact, sha256 %s.", a.Kind, a.SHA256), maxValueBytes)},
			})
		}
	}
	return result
}

// capitalize upper-cases the first letter of a constant ASCII label.
func capitalize(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// quoted wraps an already escaped fragment in double quotes; sarifText has
// turned every double quote inside it into a single quote.
func quoted(s string) string { return "\"" + s + "\"" }

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// sarifResultMessage opens with the class's constant sentence; every
// untrusted fragment follows a constant label, in quotes, escaped by
// sarifText.
func sarifResultMessage(f finding, cls findingClass) string {
	// A class-specific statement (the intent class's fixed text) replaces the
	// class sentence.
	parts := []string{cls.Short}
	if f.Statement != "" {
		parts = []string{sarifText(f.Statement, maxReasonBytes)}
	}
	if f.Severity != "" {
		parts = append(parts, fmt.Sprintf("Severity %s was assigned by the reviewer model.", f.Severity))
	}
	if f.Title != "" {
		parts = append(parts, "Reviewer-model title: "+quoted(sarifText(f.Title, maxTitleBytes))+".")
	}
	for _, d := range f.Details {
		parts = append(parts, capitalize(sarifText(d.Label, maxLabelBytes))+": "+quoted(sarifText(d.Value, maxValueBytes))+".")
	}
	if len(f.EvidenceIDs) > 0 {
		parts = append(parts, "Evidence: "+quoted(sarifText(strings.Join(f.EvidenceIDs, ", "), maxValueBytes))+".")
	}
	if len(f.replayed) > 0 {
		parts = append(parts, "Checks "+quoted(sarifText(strings.Join(f.replayed, ", "), maxValueBytes))+" were "+replayedChecksTxt+".")
	}
	switch {
	case f.anchor.FileLevel && f.anchor.Source == locModel:
		parts = append(parts, "Location: "+fileLevelText+".")
	case f.anchor.FileLevel:
		parts = append(parts, "Location: file-level.")
	default:
		parts = append(parts, "Location: "+f.anchor.Source+".")
	}
	text := strings.Join(parts, " ")
	if len(text) > maxSARIFMessage {
		text = redact.TruncateUTF8(text, maxSARIFMessage-len(ellipsis)) + ellipsis
	}
	return text
}

// sarifText neutralizes an untrusted fragment of a SARIF message: control and
// bidirectional characters become spaces, brackets are escaped so they cannot
// form a SARIF embedded link (a backslash before a bracket is escaped too),
// double quotes become single quotes so a fragment cannot close its quotes,
// and "://" and "www." are broken so viewers do not turn them into links. The
// fragment is cut to limit bytes first.
func sarifText(s string, limit int) string {
	s = bounded(s, limit)
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		switch r {
		case '\\':
			if i+1 < len(runes) && (runes[i+1] == '[' || runes[i+1] == ']') {
				b.WriteString(`\\`)
			} else {
				b.WriteRune(r)
			}
		case '[', ']':
			b.WriteRune('\\')
			b.WriteRune(r)
		case '"':
			b.WriteRune('\'')
		default:
			b.WriteRune(r)
		}
	}
	return breakLinks(b.String())
}

var wwwPrefix = regexp.MustCompile(`(?i)www\.`)

// zeroWidthSpace is U+200B ZERO WIDTH SPACE, encoded in UTF-8.
const zeroWidthSpace = "\xe2\x80\x8b"

// breakLinks inserts a zero-width space into "://" and "www." so that neither
// becomes a link in a viewer that autolinks plain text.
func breakLinks(s string) string {
	s = strings.ReplaceAll(s, "://", ":"+zeroWidthSpace+"//")
	return wwwPrefix.ReplaceAllStringFunc(s, func(m string) string { return m[:3] + zeroWidthSpace + "." })
}

// artifactURI turns a repository-relative (or report-relative) slash path into
// a relative URI reference, percent-encoding each segment. An absolute path,
// an empty, "." or ".." segment, or a NUL refuses the path.
func artifactURI(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || len(p) > 4096 {
		return "", false
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		if s == "" || s == "." || s == ".." {
			return "", false
		}
		// PathEscape keeps ':', which would make a first segment read as a URI
		// scheme (and "C:" as a drive).
		segments[i] = strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
	}
	return strings.Join(segments, "/"), true
}

// sarifNotifications lists, at level warning, everything an empty or short
// result list must not hide: an operational failure, checks that did not
// pass, unverified areas and hypotheses, stages that did not run, the absence
// of any execution, findings without a location in the changed files, and
// findings the cap omitted.
func sarifNotifications(s exportStatus, unanchored []finding, omitted int) []sarifNotification {
	var out []sarifNotification
	add := func(kind, text string) {
		out = append(out, sarifNotification{Level: levelWarning, Message: sarifMessage{text}, Properties: sarifNotificationProps{kind}})
	}
	if s.ExitCode == 4 {
		add("operational_failure", exitSentence(4))
	}
	if s.NoExecution {
		add("no_execution", noExecutionText)
	}
	for _, c := range s.ChecksNotPassed {
		add("check_not_passed", "Check "+quoted(sarifText(c.ID, 64))+" ("+sarifText(c.Kind, 64)+") is "+sarifText(c.Status, 32)+"; inspect its recorded output.")
	}
	for _, st := range s.StagesNotRun {
		text := fmt.Sprintf("Stage did not run: %s (%s).", st.Name, sarifText(st.Status, 32))
		if strings.TrimSpace(st.Reason) != "" {
			text = "Stage did not run: " + st.Name + " (" + sarifText(st.Status, 32) + "): " + quoted(sarifText(st.Reason, maxReasonBytes)) + "."
		}
		add("stage_not_run", text)
	}
	for _, u := range s.UnverifiedNotes {
		add("unverified_area", "Unverified area: "+sarifText(u, maxReasonBytes))
	}
	for _, h := range s.UnverifiedHypotheses {
		add("unverified_hypothesis", "Unverified hypothesis "+quoted(sarifText(h.ID, 64))+"; reviewer-model title: "+quoted(sarifText(h.Title, maxTitleBytes))+".")
	}
	for _, f := range unanchored {
		cls, _ := classByName(f.Class)
		add("unanchored_finding", cls.Short+" "+unanchoredText+" Evidence: "+quoted(sarifText(strings.Join(f.EvidenceIDs, ", "), maxValueBytes))+
			"; checks: "+quoted(sarifText(strings.Join(f.CheckIDs, ", "), maxValueBytes))+". It is listed in PR_COMMENT.md, and its records are in confidence-report.json.")
	}
	if omitted > 0 {
		add("omitted_findings", fmt.Sprintf("%d further evidence-backed findings are not listed because of the result limit; their records are in confidence-report.json.", omitted))
	}
	if len(out) > maxNotifications {
		rest := len(out) - (maxNotifications - 1)
		out = append(out[:maxNotifications-1], sarifNotification{Level: levelWarning, Message: sarifMessage{fmt.Sprintf("%d further notifications are not listed; see confidence-report.json.", rest)}, Properties: sarifNotificationProps{"omitted_notifications"}})
	}
	for i := range out {
		if len(out[i].Message.Text) > maxNotificationText {
			out[i].Message.Text = redact.TruncateUTF8(out[i].Message.Text, maxNotificationText-len(ellipsis)) + ellipsis
		}
	}
	if out == nil {
		out = []sarifNotification{}
	}
	return out
}
