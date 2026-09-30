package report

// Evidence-only findings, shared by the SARIF and PR-comment exports.
//
// A finding exists only for a record that Finalize derived from recorded
// checks, and only while its cited evidence re-derives, from the same recorded
// checks, to the single status of its class (the mutation classes re-derive
// through verifyMutation). Every cited check ID must resolve exactly once in
// r.Checks together with r.Mutation.Checks, and every cited evidence ID
// exactly once in r.Evidence. Signals (including coverage and the lexical
// test-edit heuristics), review targets, impacted callers, prepare, cache and
// impact records, killed mutants, NOT_REPRODUCED, NOT_DIVERGED,
// PASSES_ON_CANDIDATE and INTENT_TEST_PASSED records, UNVERIFIED and DISMISSED
// hypotheses and every model judgment are never findings.
//
// Nothing here may present a finding as a confirmed defect, a regression or
// the revision that is right, and no export may read as an approval: an empty
// list of findings is not an approval, and no confidence, precision, security
// severity or percentage is ever emitted.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// Finding classes (§1.16). The order of findingClasses is the export order.
const (
	ClassReproduced                   = "reproduced"
	ClassBaseTestFailsOnCandidate     = "base_test_fails_on_candidate"
	ClassImpactedTestFailsOnCandidate = "impacted_test_fails_on_candidate"
	ClassFuzzDivergence               = "fuzz_divergence"
	ClassObservedDivergence           = "observed_divergence"
	ClassIntentTestFailed             = "intent_test_failed"
	ClassSurvivingMutant              = "surviving_mutant"
)

// SARIF levels.
const (
	levelError   = "error"
	levelWarning = "warning"
	levelNote    = "note"
)

// Bounds of the rendered findings.
const (
	maxFindings      = 1000 // after ordering, so the strongest are kept
	maxTitleBytes    = 256
	maxValueBytes    = 256
	maxReasonBytes   = 512
	maxLabelBytes    = 40
	maxDetails       = 8
	maxIDsPerList    = 20
	maxRowsInDetails = 2
	ellipsis         = "…"
)

// findingClass is the fixed wording, rule and level of one class. Every
// sentence here is constant; untrusted text only ever appears as a finding's
// title or detail value.
type findingClass struct {
	Class, Status, RuleID, RuleName string
	Rank                            int
	// Short, Full and Help are the SARIF rule texts; Short also opens every
	// SARIF result message of the class.
	Short, Full, Help string
	// Heading and Caveat open the class's group in the PR comment.
	Heading, Caveat string
	DefaultLevel    string
	// NeedsEvidence is false only for surviving mutants, which cite the
	// mutation ledger's checks and no evidence record.
	NeedsEvidence bool
	level         func(finding) string
}

func fixedLevel(level string) func(finding) string {
	return func(finding) string { return level }
}

var findingClasses = []findingClass{
	{
		Class: ClassReproduced, Status: model.StatusReproduced, RuleID: "probe/reproduced", RuleName: "Reproduced", Rank: 1,
		Short:        "A generated test passed on the baseline and failed on the candidate.",
		Full:         "A model-written generated test passed on a live baseline run and failed on the candidate under the same command, and Probe re-derived both outcomes from the recorded named-test execution. The severity is the reviewer model's classification.",
		Help:         "Reproduced means only that this recorded experiment passed on the baseline and failed on the candidate. It does not confirm a defect and does not show that the test's assertion encodes the intended behavior; a human judges the assertion. Only a high or critical reproduced hypothesis produces exit code 1, which is also the only case this rule reports at level error.",
		Heading:      "Reproduced hypotheses",
		Caveat:       "A generated test passed on the baseline and failed on the candidate. This does not confirm a defect: a human judges whether the test's assertion is the intended behavior.",
		DefaultLevel: levelWarning, NeedsEvidence: true,
		level: func(f finding) string {
			if rank(f.Severity) >= rank("high") {
				return levelError
			}
			return levelWarning
		},
	},
	{
		Class: ClassBaseTestFailsOnCandidate, Status: model.StatusFailsOnCandidate, RuleID: "probe/base-test-fails-on-candidate", RuleName: "BaseTestFailsOnCandidate", Rank: 2,
		Short:        "The baseline version of a test that the change edited passed on the baseline and failed on candidate code.",
		Full:         "The baseline version of a test that the change modified or removed (a Go test function, or a TypeScript or JavaScript test call) passed on a live baseline run and failed on the candidate tree with its test files reverted to the baseline, under the same command.",
		Help:         "The baseline version of a test the change edited failed on candidate code after passing on the baseline; a human judges why. It is not a reproduced issue: the change may intend a different outcome, the baseline assertion may not be the intended behavior, a flaky test can produce it, and it does not show that the test edit is wrong or deliberate. It never produces exit code 1.",
		Heading:      "Changed baseline tests that fail on candidate code",
		Caveat:       "The baseline version of a test the change edited passed on the baseline and failed on candidate code. The change may intend this; it does not show that the test edit is wrong or deliberate.",
		DefaultLevel: levelWarning, NeedsEvidence: true, level: fixedLevel(levelWarning),
	},
	{
		Class: ClassImpactedTestFailsOnCandidate, Status: model.StatusFailsOnCandidate, RuleID: "probe/impacted-test-fails-on-candidate", RuleName: "ImpactedTestFailsOnCandidate", Rank: 3,
		Short:        "An existing test that the change did not edit, statically linked to a changed function, passed on the baseline and failed on the candidate.",
		Full:         "An existing test that the change did not modify, and that the approximate static index links to a changed function, passed on a live baseline run and failed on the candidate under the same command.",
		Help:         "The static link is approximate: the failure may come from any part of the change or from flakiness, and it does not show that the linked function caused it. It is not a reproduced issue and never produces exit code 1.",
		Heading:      "Impacted tests that fail on candidate code",
		Caveat:       "An unchanged test that the approximate static index links to a changed function passed on the baseline and failed on the candidate. The failure may come from any part of the change or from flakiness.",
		DefaultLevel: levelWarning, NeedsEvidence: true, level: fixedLevel(levelWarning),
	},
	{
		Class: ClassFuzzDivergence, Status: model.StatusDiverged, RuleID: "probe/fuzz-divergence", RuleName: "FuzzDivergence", Rank: 4,
		Short:        "Identical seeded inputs gave different recorded values on the baseline and the candidate.",
		Full:         "Differential fuzzing ran a changed function on identical seeded inputs on both revisions, and a confirmation pair, whose baseline run was live, recorded the same difference. No model chose the inputs.",
		Help:         "A divergence records a difference between recorded values for recorded inputs. It does not establish which revision is correct and is not a defect report; the values are bounded, redacted serializations. It never produces exit code 1.",
		Heading:      "Differential fuzzing divergences",
		Caveat:       "Identical seeded inputs gave different recorded values on the two revisions. This does not establish which revision is correct. The values shown are bounded, redacted serializations of what each run recorded.",
		DefaultLevel: levelWarning, NeedsEvidence: true, level: fixedLevel(levelWarning),
	},
	{
		Class: ClassObservedDivergence, Status: model.StatusDiverged, RuleID: "probe/observed-divergence", RuleName: "ObservedDivergence", Rank: 5,
		Short:        "A generated test recorded different values on the baseline and the candidate for the same inputs.",
		Full:         "A model-written generated test recorded values instead of asserting them, and a recorded key differed between the candidate and two baseline runs that agreed with each other, the second of them live.",
		Help:         "The reviewer model chose the inputs. A divergence records a difference, not which revision is correct, and is not a defect report; the values are bounded, redacted serializations. It never produces exit code 1.",
		Heading:      "Observed behavior divergences",
		Caveat:       "A model-written test recorded different values on the two revisions for the same inputs. This does not establish which revision is correct. The values shown are bounded, redacted serializations of what each run recorded.",
		DefaultLevel: levelWarning, NeedsEvidence: true, level: fixedLevel(levelWarning),
	},
	{
		Class: ClassIntentTestFailed, Status: model.StatusIntentTestFailed, RuleID: "probe/intent-test-failed", RuleName: "IntentTestFailed", Rank: 6,
		Short:        "A model-written test for an acceptance criterion failed on the candidate.",
		Full:         "A model-written test for one quoted acceptance criterion, which references symbols the change added or modified, failed on an assertion when it ran on the candidate only.",
		Help:         "There is no baseline control, and the test or its reading of the criterion may be wrong. It is not a reproduction and does not show that the change departs from the intent. It never produces exit code 1.",
		Heading:      "Intent test failures",
		Caveat:       "Each test below is model-written and ran on the candidate only.",
		DefaultLevel: levelNote, NeedsEvidence: true, level: fixedLevel(levelNote),
	},
	{
		Class: ClassSurvivingMutant, Status: model.MutantSurvived, RuleID: "probe/surviving-mutant", RuleName: "SurvivingMutant", Rank: 7,
		Short:        "A single mutation of an added line left the package's tests passing.",
		Full:         "A deterministic single change to an added line was applied in a private copy of the candidate, and the configured test command of its package (or, for TypeScript and JavaScript, of its source file) passed on it, as it did on the unmodified control run.",
		Help:         "The mutant may be semantically equivalent to the original code, and the tests of other packages were not run. It is not a defect, dead code or a missing test by itself, and no mutation score is computed. It does not request review on its own.",
		Heading:      "Surviving mutants",
		Caveat:       "The package's tests passed with this single change to an added line, as they did without it. The mutant may be equivalent to the original code.",
		DefaultLevel: levelNote, NeedsEvidence: false, level: fixedLevel(levelNote),
	},
}

// classByName returns the class entry of name.
func classByName(name string) (findingClass, bool) {
	for _, c := range findingClasses {
		if c.Class == name {
			return c, true
		}
	}
	return findingClass{}, false
}

// Location sources. A model-chosen location is anchored at its line only when
// the diff recorded that line on the candidate side; the deterministic sources
// come from the harness or the candidate source and keep their lines.
const (
	locModel           = "model-chosen location"
	locChangedFunction = "changed function"
	locCandidateTest   = "candidate test range"
	locMutatedLine     = "mutated added line"
)

// detail is one labelled value of a finding. Label is a constant chosen by
// collector code; Value is untrusted.
type detail struct {
	Label, Value string
}

// findingLocation is where a collector places a finding before anchoring.
type findingLocation struct {
	Path          string
	Line, EndLine int
	Source        string
}

// findingAnchor is the location the exports emit. FileLevel anchors carry no
// line of their own (SARIF uses line 1 with location precision "file").
type findingAnchor struct {
	Path          string
	Line, EndLine int
	FileLevel     bool
	Source        string
}

// finding is one evidence-backed finding.
type finding struct {
	Class, Status string
	// Severity is the reviewer model's classification (reproduced only).
	Severity string
	// Title is model-written text (reproduced and intent classes).
	Title string
	// Statement is a class-specific fixed sentence with constant structure
	// (the intent class's text); its only variable part is an ID.
	Statement     string
	Location      *findingLocation
	Identity      string
	EvidenceIDs   []string
	CheckIDs      []string
	HypothesisIDs []string
	CriterionID   string
	MutantID      string
	Details       []detail
	Artifacts     []model.Artifact
	// Set by collectFindings.
	anchor      *findingAnchor
	replayed    []string
	fingerprint string
	truncated   bool
}

// exportVerification holds what the verifiers re-derive now: evidence ID to
// status, and mutant ID to status.
type exportVerification struct {
	evidence map[string]string
	mutants  map[string]string
}

// verifyExports re-derives the evidence and mutant statuses exactly as
// Finalize does. It does not modify r.
func verifyExports(r *model.Report) exportVerification {
	return exportVerification{evidence: verifyReport(r).verified, mutants: verifyMutation(r)}
}

// exportIndex resolves IDs the way the ledger does: an ID recorded more than
// once resolves to nothing. Checks come from r.Checks and r.Mutation.Checks
// together, so an ID present in both is also unresolved.
type exportIndex struct {
	v                 exportVerification
	checks            map[string]model.Check
	duplicateChecks   map[string]bool
	evidence          map[string]model.Evidence
	duplicateEvidence map[string]bool
	live              map[string]bool
	newLines          map[string]map[int]bool
	criteria          map[string]model.IntentCriterion
	artifacts         []model.Artifact
}

func newExportIndex(r *model.Report, v exportVerification) *exportIndex {
	x := &exportIndex{
		v:                 v,
		checks:            map[string]model.Check{},
		duplicateChecks:   map[string]bool{},
		evidence:          map[string]model.Evidence{},
		duplicateEvidence: map[string]bool{},
		live:              map[string]bool{},
		newLines:          map[string]map[int]bool{},
		criteria:          criterionIndex(r.IntentCriteria),
		artifacts:         r.Artifacts,
	}
	all := append([]model.Check{}, r.Checks...)
	if r.Mutation != nil {
		all = append(all, r.Mutation.Checks...)
	}
	for _, c := range all {
		if _, seen := x.checks[c.ID]; seen {
			x.duplicateChecks[c.ID] = true
		}
		x.checks[c.ID] = c
	}
	for _, e := range r.Evidence {
		if _, seen := x.evidence[e.ID]; seen {
			x.duplicateEvidence[e.ID] = true
		}
		x.evidence[e.ID] = e
	}
	for _, f := range r.Change.Files {
		if f.Path == "" || f.Status == "D" || f.Binary {
			continue
		}
		x.live[f.Path] = true
		lines := x.newLines[f.Path]
		if lines == nil {
			lines = map[int]bool{}
			x.newLines[f.Path] = lines
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.NewLine > 0 && (l.Kind == "add" || l.Kind == "context") {
					lines[l.NewLine] = true
				}
			}
		}
	}
	return x
}

func (x *exportIndex) check(id string) (model.Check, bool) {
	c, ok := x.checks[id]
	if !ok || id == "" || x.duplicateChecks[id] {
		return model.Check{}, false
	}
	return c, true
}

func (x *exportIndex) item(id string) (model.Evidence, bool) {
	e, ok := x.evidence[id]
	if !ok || id == "" || x.duplicateEvidence[id] {
		return model.Evidence{}, false
	}
	return e, true
}

// accepted returns the evidence record id when it resolves, has kind and
// re-derives now to status.
func (x *exportIndex) accepted(id, kind, status string) (model.Evidence, bool) {
	e, ok := x.item(id)
	if !ok || e.Kind != kind || e.Status != status || x.v.evidence[id] != status {
		return model.Evidence{}, false
	}
	return e, true
}

// retainedTestArtifacts returns the retained test artifacts of kind whose file
// name the harness derived from testPath: "<run>-generated-test-<n>-<base
// name>" for a generated test and "<run>-intent-test-<n>-<base name>" for an
// intent test, where <run> is the hexadecimal run ID. The pattern is anchored
// at both ends, so a model-chosen file name that itself contains
// "-generated-test-<n>-" cannot match the artifact of another test. The
// evidence records no artifact ID, so a link is made only when exactly one
// artifact matches; otherwise nothing is linked.
func (x *exportIndex) retainedTestArtifacts(kind, testPath string) []model.Artifact {
	base := baseName(testPath)
	if base == "" {
		return nil
	}
	prefix := "generated"
	if kind == model.ArtifactIntentTest {
		prefix = "intent"
	}
	pattern := regexp.MustCompile(`^[0-9a-f]+-` + prefix + `-test-[1-9][0-9]*-` + regexp.QuoteMeta(base) + `$`)
	var out []model.Artifact
	for _, a := range x.artifacts {
		if a.Kind == kind && pattern.MatchString(baseName(a.Path)) {
			out = append(out, a)
		}
	}
	if len(out) != 1 {
		return nil
	}
	return out
}

// artifactBySHA returns the artifacts of kind with this sha256, first by path.
func (x *exportIndex) artifactBySHA(kind, sum string) []model.Artifact {
	if sum == "" {
		return nil
	}
	var out []model.Artifact
	for _, a := range x.artifacts {
		if a.Kind == kind && a.SHA256 == sum {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > 1 {
		out = out[:1]
	}
	return out
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// findingCollectors lists every collector, in class order. Each reads only
// the Finalize-derived fields of its class (§1.10.6 of the v0.4 contract).
var findingCollectors = []func(*model.Report, *exportIndex) []finding{
	reproducedFindings,   // findings_reproduced.go
	baseTestFindings,     // findings_basetests.go
	impactedTestFindings, // findings_impact.go
	divergenceFindings,   // findings_divergences.go (fuzz and observed)
	intentFindings,       // findings_intent.go
	mutantFindings,       // findings_mutation.go
}

// findingSet is the ordered, bounded list of findings of one report.
type findingSet struct {
	findings []finding
	omitted  int
}

// collectFindings returns the evidence-backed findings of a finalized report.
// v is what the verifiers re-derive now (verifyExports); tests substitute a
// fixed one.
func collectFindings(r *model.Report, v exportVerification) findingSet {
	x := newExportIndex(r, v)
	var list []finding
	for _, collect := range findingCollectors {
		for _, f := range collect(r, x) {
			if g, ok := normalizeFinding(f, x); ok {
				list = append(list, g)
			}
		}
	}
	sortFindings(list)
	list = dedupeFindings(list)
	assignFingerprints(list)
	set := findingSet{findings: list}
	if len(list) > maxFindings {
		set.findings, set.omitted = list[:maxFindings], len(list)-maxFindings
	}
	return set
}

// normalizeFinding is the generic guard every finding passes. It refuses an
// unknown class, a status other than the class's, a finding without a
// resolving check, an evidence ID that does not resolve exactly once, and an
// empty identity; it bounds every string and list and anchors the location.
func normalizeFinding(f finding, x *exportIndex) (finding, bool) {
	cls, ok := classByName(f.Class)
	if !ok || f.Status != cls.Status || f.Identity == "" {
		return finding{}, false
	}
	if len(f.CheckIDs) == 0 || cls.NeedsEvidence && len(f.EvidenceIDs) == 0 {
		return finding{}, false
	}
	for _, id := range f.CheckIDs {
		c, ok := x.check(id)
		if !ok {
			return finding{}, false
		}
		if c.Replayed() {
			f.replayed = append(f.replayed, id)
		}
	}
	for _, id := range f.EvidenceIDs {
		if _, ok := x.item(id); !ok {
			return finding{}, false
		}
	}
	if f.Severity != "" {
		f.Severity = severity(f.Severity)
	}
	var cut bool
	f.Title, cut = boundText(f.Title, maxTitleBytes)
	f.truncated = f.truncated || cut
	if len(f.Details) > maxDetails {
		f.Details, f.truncated = f.Details[:maxDetails], true
	}
	details := make([]detail, 0, len(f.Details))
	for _, d := range f.Details {
		label, _ := boundText(d.Label, maxLabelBytes)
		value, cut := boundText(d.Value, maxValueBytes)
		f.truncated = f.truncated || cut
		details = append(details, detail{label, value})
	}
	f.Details = details
	for _, list := range []*[]string{&f.EvidenceIDs, &f.CheckIDs, &f.HypothesisIDs, &f.replayed} {
		*list = unique(*list)
		if len(*list) > maxIDsPerList {
			*list, f.truncated = (*list)[:maxIDsPerList], true
		}
	}
	f.anchor = anchorFinding(f.Location, x)
	return f, true
}

// anchorFinding applies the location rules: a location is emitted only when
// its path is a changed, non-deleted, non-binary file of the recorded change.
// A model-chosen line is kept only when the diff recorded it on the candidate
// side; otherwise the anchor is file-level when the file has recorded
// candidate-side lines, and absent when it has none. Deterministic locations
// keep their lines.
func anchorFinding(loc *findingLocation, x *exportIndex) *findingAnchor {
	if loc == nil || !x.live[loc.Path] {
		return nil
	}
	lines := x.newLines[loc.Path]
	a := &findingAnchor{Path: loc.Path, Source: loc.Source}
	lineKnown := loc.Line > 0 && (loc.Source != locModel || lines[loc.Line])
	switch {
	case lineKnown:
		a.Line, a.EndLine = loc.Line, loc.EndLine
		if loc.Source == locModel || a.EndLine < a.Line {
			a.EndLine = a.Line
		}
	case len(lines) > 0:
		a.FileLevel = true
	default:
		return nil
	}
	return a
}

func classRank(class string) int {
	if c, ok := classByName(class); ok {
		return c.Rank
	}
	return len(findingClasses) + 1
}

func anchorKey(f finding) (string, int) {
	if f.anchor == nil {
		return "", 0
	}
	return f.anchor.Path, f.anchor.Line
}

// sortFindings orders by class rank, then severity (highest first), anchored
// path and line, identity and the first evidence or check ID.
func sortFindings(list []finding) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := classRank(a.Class), classRank(b.Class); ra != rb {
			return ra < rb
		}
		if sa, sb := rank(a.Severity), rank(b.Severity); sa != sb {
			return sa > sb
		}
		pa, la := anchorKey(a)
		pb, lb := anchorKey(b)
		if pa != pb {
			return pa < pb
		}
		if la != lb {
			return la < lb
		}
		if a.Identity != b.Identity {
			return a.Identity < b.Identity
		}
		return firstID(a) < firstID(b)
	})
}

func firstID(f finding) string {
	if len(f.EvidenceIDs) > 0 {
		return f.EvidenceIDs[0]
	}
	if f.MutantID != "" {
		return f.MutantID
	}
	if len(f.CheckIDs) > 0 {
		return f.CheckIDs[0]
	}
	return ""
}

// dedupeFindings merges findings of the same class, identity and anchor: the
// first (strongest) keeps its texts, and the ID lists are merged.
func dedupeFindings(list []finding) []finding {
	index := map[string]int{}
	var out []finding
	for _, f := range list {
		p, l := anchorKey(f)
		key := strings.Join([]string{f.Class, f.Identity, p, fmt.Sprint(l)}, "\x00")
		if i, seen := index[key]; seen {
			g := &out[i]
			g.EvidenceIDs = capIDs(unique(append(g.EvidenceIDs, f.EvidenceIDs...)), &g.truncated)
			g.CheckIDs = capIDs(unique(append(g.CheckIDs, f.CheckIDs...)), &g.truncated)
			g.HypothesisIDs = capIDs(unique(append(g.HypothesisIDs, f.HypothesisIDs...)), &g.truncated)
			g.replayed = capIDs(unique(append(g.replayed, f.replayed...)), &g.truncated)
			continue
		}
		index[key] = len(out)
		out = append(out, f)
	}
	return out
}

func capIDs(ids []string, truncated *bool) []string {
	if len(ids) > maxIDsPerList {
		*truncated = true
		return ids[:maxIDsPerList]
	}
	return ids
}

// assignFingerprints sets a stable, line-independent fingerprint: the class,
// the anchored path and the identity, plus an occurrence number when several
// findings share them.
func assignFingerprints(list []finding) {
	seen := map[string]int{}
	for i := range list {
		f := &list[i]
		p, _ := anchorKey(*f)
		key := f.Class + "\x00" + p + "\x00" + f.Identity
		n := seen[key]
		seen[key] = n + 1
		input := "probe-finding/v1\x00" + key
		if n > 0 {
			input += fmt.Sprintf("\x00#%d", n)
		}
		sum := sha256.Sum256([]byte(input))
		f.fingerprint = hex.EncodeToString(sum[:])[:32]
	}
}

// identity joins its parts with NUL; parts never contain NUL after cleaning.
func identity(parts ...string) string {
	return strings.Join(parts, "\x00")
}

// isBidi reports the bidirectional formatting characters that can reorder
// displayed text.
func isBidi(r rune) bool {
	switch r {
	case 0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069:
		return true
	}
	return false
}

// plainText replaces control and bidirectional formatting characters with
// spaces and invalid UTF-8 with U+FFFD.
func plainText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidi(r) {
			return ' '
		}
		return r
	}, s)
}

// boundText is plainText cut to limit bytes on a UTF-8 boundary, with an
// ellipsis when it was cut.
func boundText(s string, limit int) (string, bool) {
	s = plainText(s)
	if len(s) <= limit {
		return s, false
	}
	return redact.TruncateUTF8(s, limit) + ellipsis, true
}

// bounded is boundText without the flag.
func bounded(s string, limit int) string {
	out, _ := boundText(s, limit)
	return out
}

// lineRange renders "path:line" or "path:start–end" for detail values.
func lineRange(path string, start, end int) string {
	switch {
	case path == "":
		return ""
	case start <= 0:
		return path
	case end > start:
		return fmt.Sprintf("%s:%d–%d", path, start, end)
	}
	return fmt.Sprintf("%s:%d", path, start)
}

// stageNotRun is a configured or requested stage that did not run.
type stageNotRun struct {
	Name, Status, Reason string
}

// exportStatus is what the exports report besides findings: never findings
// themselves, and never a reason to read the absence of findings as approval.
type exportStatus struct {
	ExitCode             int
	UnverifiedNotes      []string
	UnverifiedHypotheses []model.Hypothesis
	ChecksNotPassed      []model.Check
	StagesNotRun         []stageNotRun
	RiskSignals          int
	ReviewTargets        int
	NoExecution          bool
	ExecutionSuccessful  bool
}

// UnverifiedAreas counts the recorded unverified notes and the hypotheses that
// stayed UNVERIFIED.
func (s exportStatus) UnverifiedAreas() int {
	return len(s.UnverifiedNotes) + len(s.UnverifiedHypotheses)
}

// statusOf summarizes a finalized report for the exports. Checks that did not
// pass come from r.Checks only: the mutation ledger holds mutants, which are
// expected to fail. Execution is not successful when the exit code is 4, when a
// check in r.Checks is ERROR, SKIPPED or TIMEOUT, or when the overall deadline
// was reached.
func statusOf(r *model.Report) exportStatus {
	s := exportStatus{ExitCode: r.ExitCode, RiskSignals: len(r.Signals), ReviewTargets: len(r.ReviewTargets)}
	s.UnverifiedNotes = append(s.UnverifiedNotes, r.Unverified...)
	for _, h := range r.Hypotheses {
		if h.Status == model.StatusUnverified {
			s.UnverifiedHypotheses = append(s.UnverifiedHypotheses, h)
		}
	}
	s.ExecutionSuccessful = r.ExitCode != 4
	for _, c := range r.Checks {
		if c.Status != "PASS" {
			s.ChecksNotPassed = append(s.ChecksNotPassed, c)
		}
		switch c.Status {
		case "ERROR", "SKIPPED", "TIMEOUT":
			s.ExecutionSuccessful = false
		}
	}
	if r.Execution != nil && r.Execution.Budget.DeadlineReached {
		s.ExecutionSuccessful = false
	}
	if r.Prepare != nil && (r.Prepare.Status == model.PrepareFailed || r.Prepare.Status == model.PrepareNotPermitted) {
		s.StagesNotRun = append(s.StagesNotRun, stageNotRun{"dependency preparation", r.Prepare.Status, r.Prepare.Reason})
	}
	if r.BaseTests != nil && r.BaseTests.Status == model.BaseTestsNotRun {
		s.StagesNotRun = append(s.StagesNotRun, stageNotRun{"changed baseline tests", r.BaseTests.Status, r.BaseTests.Reason})
	}
	if r.Impact != nil && r.Impact.TestsStatus == model.ImpactTestsNotRun {
		s.StagesNotRun = append(s.StagesNotRun, stageNotRun{"impacted tests", r.Impact.TestsStatus, r.Impact.TestsReason})
	}
	if r.Fuzz != nil && r.Fuzz.Status == model.FuzzNotRun {
		s.StagesNotRun = append(s.StagesNotRun, stageNotRun{"differential fuzzing", r.Fuzz.Status, r.Fuzz.Reason})
	}
	if r.Mutation != nil && r.Mutation.Status == model.MutationNotRun {
		s.StagesNotRun = append(s.StagesNotRun, stageNotRun{"mutation of added lines", r.Mutation.Status, r.Mutation.Reason})
	}
	s.NoExecution = len(r.Checks) == 0 && (r.Mutation == nil || len(r.Mutation.Checks) == 0)
	return s
}

// exitSentence explains an exit code without implying approval.
func exitSentence(code int) string {
	switch code {
	case 0:
		return "Exit code 0: no reproduced high or critical hypothesis was recorded."
	case 1:
		return "Exit code 1: a reproduced high or critical hypothesis was recorded."
	case 2:
		return "Exit code 2: human review requested."
	case 4:
		return "Exit code 4: operational failure; the run did not finish as configured, so findings may be missing."
	}
	return fmt.Sprintf("Exit code %d.", code)
}

// Fixed sentences shared by the exports.
const (
	notApprovalText   = "No finding is not approval."
	noExecutionText   = "No checks or experiments ran, so the absence of findings carries no information."
	evidenceOnlyText  = "Only findings backed by recorded sandbox evidence are listed; signals, review ranges, unverified hypotheses and model judgments are never findings."
	unanchoredText    = "No location in the changed files is recorded for this finding."
	fileLevelText     = "file-level: the model-chosen line is not a candidate-side line of the recorded diff"
	replayedChecksTxt = "replayed from the execution cache (not a fresh execution)"

	// unanchoredRecordsText closes an unanchored finding's SARIF notification;
	// the PR comment is not always rendered, and its caps can leave a finding
	// out.
	unanchoredRecordsText = "Its records are in confidence-report.json, and the PR-comment export (--format pr-comment) lists it unless the comment's size limits leave it out."
)
