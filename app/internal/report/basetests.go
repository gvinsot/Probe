package report

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

// This file belongs to F3: baseline versions of changed tests run on candidate
// code (--base-tests). Every status of the base_tests section is re-derived
// here from the recorded checks; a base_test_differential record never
// supports a hypothesis status (see accepted) and never sets exit 1.

// Fixed texts of the base_tests section.
const (
	baseTestsHeading = "\n## Changed Baseline Tests on Candidate Code\n"
	// baseTestUnsupportedText replaces the reason of a test whose stored
	// FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE is not supported by verified
	// evidence.
	baseTestUnsupportedText = "the recorded evidence does not support this result"
	// baseTestNoResultText is the reason of a test recorded without a result
	// or a reason.
	baseTestNoResultText = "no recorded run pair gave this test a result"
	// baseTestsNoneSelectedText is the text of a no_candidates section.
	baseTestsNoneSelectedText = "No test was selected, so no baseline version was re-run. Only the tests declared in modified, deleted or renamed Go, TypeScript or JavaScript test files are considered; this says nothing about any other test."
	// baseTestsIntentText is rendered when an intent was supplied.
	baseTestsIntentText = "Intent was supplied; Probe does not decide whether a behavior change matches it."
	// maxPassesShown caps the PASSES_ON_CANDIDATE lines of the Markdown
	// section; the JSON keeps every test.
	maxPassesShown = 20
)

// verifyBaseTests re-derives the status of every base_test_differential
// evidence record from its recorded checks. It returns FAILS_ON_CANDIDATE or
// PASSES_ON_CANDIDATE for a record whose checks support exactly that stored
// status, and nothing for any other record:
//   - one verifiable test name, a verifiable path, runner go_test_json;
//   - a base_test_base check (base_check_id) and a base_test_hybrid check
//     (check_id), each recorded exactly once, the hybrid one never replayed;
//   - an identical command that targets the package (or file) of the path;
//   - harness.ClassifyExistingTest on those two checks gives the status;
//   - the live-baseline rule: FAILS_ON_CANDIDATE needs a baseline check that
//     was executed, PASSES_ON_CANDIDATE a baseline check that was executed or
//     replayed from two agreeing live runs.
func verifyBaseTests(r *model.Report, l *ledger) map[string]string {
	out := map[string]string{}
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceBaseTestDifferential {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok || e.Kind != model.EvidenceBaseTestDifferential {
			continue
		}
		if status := baseTestEvidenceStatus(e, l); status != "" && status == e.Status {
			out[e.ID] = status
		}
	}
	return out
}

// baseTestEvidenceStatus is the status the recorded checks of one
// base_test_differential record support, or "". A go_test_json record is
// classified from go test -json events, a jest_json record (a TS/JS test
// run by a Vitest or Jest template on its own file) from the two recorded
// JSON reports.
func baseTestEvidenceStatus(e model.Evidence, l *ledger) string {
	if len(e.TestNames) != 1 || !verifiableNames(e.TestNames) || e.Path == "" || !verifiableText(e.Path) {
		return ""
	}
	jest := e.Runner == harness.RunnerJest
	switch {
	case e.Runner != harness.RunnerGo && !jest:
		return ""
	case jest && (!harness.ValidJSTestName(e.TestNames[0]) || !harness.ScriptTestPath(e.Path)):
		return ""
	}
	base, baseOK := l.check(e.BaseCheckID)
	hybrid, hybridOK := l.check(e.CheckID)
	if !baseOK || !hybridOK || base.Kind != model.CheckBaseTestBase || hybrid.Kind != model.CheckBaseTestHybrid || hybrid.Replayed() {
		return ""
	}
	if jest && !scriptCommandTargets(base.Command, e.Path) || !jest && !baseTestCommandTargets(base.Command, e.Path) {
		return ""
	}
	status, _ := harness.ClassifyExistingTest(base, hybrid, e.TestNames[0])
	if jest {
		// The hybrid tree holds the baseline test file at its baseline path.
		status, _ = harness.ClassifyExistingJestTest(base, hybrid, e.Path, e.TestNames[0])
	}
	switch {
	case status == model.StatusFailsOnCandidate && positiveBaseline(base):
		return model.StatusFailsOnCandidate
	case status == model.StatusPassesOnCandidate && negativeBaseline(base):
		return model.StatusPassesOnCandidate
	}
	return ""
}

// baseTestCommandTargets reports whether a recorded command targets the test
// file p: its package directory as the generated_test template substitutes it
// ("./dir", or "." for the root), or the file itself.
func baseTestCommandTargets(command []string, p string) bool {
	dir := path.Dir(p)
	pkg := "."
	if dir != "." {
		pkg = "./" + dir
	}
	for _, arg := range command {
		if arg == pkg || arg == p {
			return true
		}
	}
	return false
}

// finalizeBaseTests sets every base_tests item status from verified evidence
// and never from the stored status: FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE
// only when the item's evidence resolves, names this test and path, and was
// verified with that status; UNVERIFIED otherwise. It also restores the fixed
// note. It mutates only r.BaseTests, is idempotent, and returns true (a human
// must look) for status not_run and for any item that is not
// PASSES_ON_CANDIDATE.
func finalizeBaseTests(r *model.Report, l *ledger) bool {
	b := r.BaseTests
	if b == nil {
		return false
	}
	if b.Tests == nil {
		b.Tests = []model.BaseTest{}
	}
	b.Note = model.BaseTestsNoteFor(b.Tests)
	needsHuman := b.Status == model.BaseTestsNotRun
	for i := range b.Tests {
		t := &b.Tests[i]
		switch status := baseTestItemStatus(*t, l); status {
		case model.StatusFailsOnCandidate, model.StatusPassesOnCandidate:
			t.Status, t.Reason = status, ""
		default:
			switch {
			case t.Status != model.StatusUnverified:
				t.Reason = baseTestUnsupportedText
			case strings.TrimSpace(t.Reason) == "":
				t.Reason = baseTestNoResultText
			}
			t.Status = model.StatusUnverified
		}
		if t.Status != model.StatusPassesOnCandidate {
			needsHuman = true
		}
	}
	return needsHuman
}

// baseTestItemStatus is the verified status of one item's evidence, or "".
func baseTestItemStatus(t model.BaseTest, l *ledger) string {
	if t.EvidenceID == "" {
		return ""
	}
	e, ok := l.item(t.EvidenceID)
	if !ok || e.Kind != model.EvidenceBaseTestDifferential || len(e.TestNames) != 1 || e.TestNames[0] != t.Name || e.Path != t.Path {
		return ""
	}
	switch status := l.verified[t.EvidenceID]; status {
	case model.StatusFailsOnCandidate, model.StatusPassesOnCandidate:
		return status
	}
	return ""
}

// baseTestTargets returns high review targets for every FAILS_ON_CANDIDATE
// item: the baseline test function (old side) and, when the candidate still
// declares it, its edited version (new side).
func baseTestTargets(r *model.Report) []extraTarget {
	if r.BaseTests == nil {
		return nil
	}
	var out []extraTarget
	for _, t := range r.BaseTests.Tests {
		if t.Status != model.StatusFailsOnCandidate {
			continue
		}
		out = append(out, extraTarget{path: t.Path, side: "old", start: t.Line, end: t.EndLine, severity: "high",
			reason: "Baseline version of test " + t.Name + " failed on candidate code (FAILS_ON_CANDIDATE, one recorded run each: possibly a behavior change accompanied by a test edit, or flakiness; not a reproduced issue)"})
		if t.CandidatePath != "" && t.CandidateLine > 0 {
			out = append(out, extraTarget{path: t.CandidatePath, side: "new", start: t.CandidateLine, end: t.CandidateEndLine, severity: "high",
				reason: "Edited test " + t.Name + ": its baseline version failed on candidate code (FAILS_ON_CANDIDATE)"})
		}
	}
	return out
}

// writeBaseTests renders "## Changed Baseline Tests on Candidate Code". Tests
// are listed FAILS_ON_CANDIDATE first, then UNVERIFIED, then
// PASSES_ON_CANDIDATE (at most maxPassesShown of those). Every repository
// string goes through inline().
func writeBaseTests(b *bytes.Buffer, r *model.Report) {
	s := r.BaseTests
	if s == nil {
		return
	}
	line(b, baseTestsHeading)
	fails, passes, other := 0, 0, 0
	for _, t := range s.Tests {
		switch t.Status {
		case model.StatusFailsOnCandidate:
			fails++
		case model.StatusPassesOnCandidate:
			passes++
		default:
			other++
		}
	}
	switch s.Status {
	case model.BaseTestsNoCandidates:
		line(b, baseTestsNoneSelectedText)
		if strings.TrimSpace(s.Reason) != "" {
			fmt.Fprintf(b, "\nReason: %s.\n", inline(strings.TrimSuffix(strings.TrimSpace(s.Reason), ".")))
		}
	case model.BaseTestsNotRun:
		reason := strings.TrimSuffix(strings.TrimSpace(s.Reason), ".")
		if reason == "" {
			reason = "no reason was recorded"
		}
		fmt.Fprintf(b, "Not run: %s.\n", inline(reason))
	default:
		fmt.Fprintf(b, "%d baseline versions of changed %s selected: %d %s, %d %s, %d %s.\n", len(s.Tests), baseTestsKind(s.Tests), fails, inline(model.StatusFailsOnCandidate), passes, inline(model.StatusPassesOnCandidate), other, inline(model.StatusUnverified))
	}
	if len(s.Tests) > 0 {
		evidence := map[string]model.Evidence{}
		for _, e := range r.Evidence {
			if _, seen := evidence[e.ID]; !seen {
				evidence[e.ID] = e
			}
		}
		b.WriteByte('\n')
		for _, t := range s.Tests {
			if t.Status == model.StatusFailsOnCandidate {
				line(b, baseTestLine(t, evidence))
			}
		}
		for _, t := range s.Tests {
			if t.Status != model.StatusFailsOnCandidate && t.Status != model.StatusPassesOnCandidate {
				line(b, baseTestLine(t, evidence))
			}
		}
		shown := 0
		for _, t := range s.Tests {
			if t.Status != model.StatusPassesOnCandidate {
				continue
			}
			if shown == maxPassesShown {
				fmt.Fprintf(b, "- … %d more %s results in confidence-report.json\n", passes-maxPassesShown, inline(model.StatusPassesOnCandidate))
				break
			}
			shown++
			line(b, baseTestLine(t, evidence))
		}
	}
	fmt.Fprintf(b, "\n%s\n", inline(model.BaseTestsNoteFor(s.Tests)))
	if r.Intent != "" {
		fmt.Fprintf(b, "\n%s\n", inline(baseTestsIntentText))
	}
}

// baseTestLine renders one test of the section.
func baseTestLine(t model.BaseTest, evidence map[string]model.Evidence) string {
	head := fmt.Sprintf("%s — %s:%d–%d (%s)", inline(t.Name), inline(t.Path), t.Line, t.EndLine, inline(harness.BaseTestChangeText(t.Change)))
	if t.CandidatePath != "" && t.CandidateLine > 0 {
		head += fmt.Sprintf("; edited version at %s:%d–%d", inline(t.CandidatePath), t.CandidateLine, t.CandidateEndLine)
	}
	checks := ""
	if e, ok := evidence[t.EvidenceID]; ok && t.EvidenceID != "" {
		checks = fmt.Sprintf(" (baseline check %s, hybrid-tree check %s)", inline(e.BaseCheckID), inline(e.CheckID))
	}
	switch t.Status {
	case model.StatusFailsOnCandidate:
		return fmt.Sprintf("- **%s** %s. It passed on the baseline tree and failed on the candidate tree with %s reverted to the baseline%s; %s. One recorded run each: possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge.",
			inline(t.Status), head, baseTestReverted(t), checks, inline(t.EvidenceID))
	case model.StatusPassesOnCandidate:
		return fmt.Sprintf("- **%s** %s. It passed on the baseline tree and on the candidate tree with %s reverted to the baseline%s; %s.",
			inline(t.Status), head, baseTestReverted(t), checks, inline(t.EvidenceID))
	}
	reason := strings.TrimSuffix(strings.TrimSpace(t.Reason), ".")
	if reason == "" {
		reason = baseTestNoResultText
	}
	suffix := ""
	if t.EvidenceID != "" {
		suffix = "; " + inline(t.EvidenceID)
	}
	return fmt.Sprintf("- **%s** %s: %s%s.", inline(model.StatusUnverified), head, inline(reason), suffix)
}

// baseTestsKind names the tests a section lists: Go tests, TypeScript and
// JavaScript tests, or both.
func baseTestsKind(tests []model.BaseTest) string {
	scripts := 0
	for _, t := range tests {
		if model.ScriptBaseTest(t) {
			scripts++
		}
	}
	switch scripts {
	case 0:
		return "Go tests"
	case len(tests):
		return "TypeScript/JavaScript tests"
	}
	return "Go, TypeScript and JavaScript tests"
}

// baseTestReverted says what the hybrid tree of a test reverted.
func baseTestReverted(t model.BaseTest) string {
	if model.ScriptBaseTest(t) {
		return "its test file and snapshot file"
	}
	return "its package's test files"
}
