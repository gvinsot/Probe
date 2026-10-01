package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// This file belongs to F5 (intent-linked candidate-only tests): the verifier
// of intent_test evidence, the normalization of hypothesis intent links, and
// the Markdown of the intent sections and links.

// Fixed texts of the intent sections.
const (
	intentFailuresIntro = "Candidate-only experiments. Each entry cites a test the reviewer wrote for one acceptance criterion: the test failed on an assertion on the candidate and names a symbol that a declaration the change added or modified also has, matched by name and not resolved. No baseline run acts as a control, and the test, its inputs and its reading of the criterion are model-written, so the test or the reading may be wrong. This is weaker than a reproduced issue and never sets exit code 1."
	noIntentFailureText = "No hypothesis rests on an accepted intent-test failure. This does not mean that the change does what the intent asks."
	noCriteriaText      = "No acceptance criteria were extracted: the intent contains no Markdown list item in scope (criteria grammar v1). This does not mean that the intent states no requirement. Intent tests require criteria."
	criteriaIntro       = "Acceptance criteria are Markdown list items copied verbatim from the supplied intent (criteria grammar v1%s). IDs are positional and belong to exactly this intent text. Extraction is not understanding of the intent. An intent test runs on the candidate only, with no baseline control; a test that ran without failing says nothing about whether its criterion holds."
	passedCaveat        = " (a test that ran without failing says nothing about whether the criterion holds)"
	judgmentSuffix      = ". It changes no status, severity, review target or exit code."
	// maxOutcomesShown caps the evidence IDs listed per criterion.
	maxOutcomesShown = 10
)

// verifyIntentTests re-derives the status of every intent_test evidence
// record from its recorded candidate check (F5). A record is re-derived only
// when its criterion occurs exactly once in the report's criteria, it cites no
// baseline or repeat check, its path and test names survive report sanitizing,
// and its check resolves exactly once, has kind generated_test_intent and a
// command, and was executed rather than replayed. The status then comes from
// harness.IntentOutcome over the check validated for the record's runner, path
// and names, with the record's referenced symbols and the recorded change:
// INTENT_TEST_FAILED needs an assertion failure of the test's own file and a
// referenced symbol named on an added line of a changed non-test file;
// INTENT_TEST_PASSED needs a validated pass. Only a re-derived status equal to
// the stored one is returned. UNVERIFIED is never returned: it supports
// nothing.
func verifyIntentTests(r *model.Report, l *ledger) map[string]string {
	out := map[string]string{}
	criteria := criterionIndex(r.IntentCriteria)
	// One word set per verification, built only when a record reaches the
	// linking rule: the cost stays linear in the size of the diff, whatever
	// the number of records.
	words := harness.NewIntentWords(r.Change)
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceIntentTest {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok || e.Kind != model.EvidenceIntentTest {
			continue
		}
		if status := intentTestStatus(e, l, criteria, words); status != "" && status == e.Status {
			out[e.ID] = status
		}
	}
	return out
}

// intentTestStatus is the positive status the recorded check of one intent
// test supports, or "".
func intentTestStatus(e model.Evidence, l *ledger, criteria map[string]model.IntentCriterion, words *harness.IntentWords) string {
	if _, known := criteria[e.CriterionID]; !known {
		return ""
	}
	if e.BaseCheckID != "" || e.RepeatCheckID != "" || e.Path == "" || !verifiableText(e.Path) || !verifiableNames(e.TestNames) {
		return ""
	}
	c, ok := l.check(e.CheckID)
	if !ok || c.Kind != model.CheckGeneratedIntent || len(c.Command) == 0 || c.Replayed() {
		return ""
	}
	c, known := harness.ValidateExecution(e.Runner, c, e.Path, e.TestNames)
	if !known {
		return ""
	}
	status, _ := harness.IntentOutcome(e.Runner, c, e.Path, e.TestNames, e.ReferencedSymbols, words)
	if status == model.StatusUnverified {
		return ""
	}
	return status
}

// normalizeIntentLink runs after a hypothesis's final status is set. It drops
// a criterion_id that is not in criteria, and drops intent_judgment unless the
// status is DIVERGED, the hypothesis keeps a criterion and the judgment is one
// of the two allowed values. Any drop returns one fixed Unverified note naming
// the hypothesis, "" otherwise. A second call finds nothing to drop, so
// Finalize stays idempotent. It never changes the status, the severity, the
// lists or the exit code: a judgment is model opinion, never evidence.
func normalizeIntentLink(h *model.Hypothesis, criteria map[string]model.IntentCriterion) string {
	dropped := false
	if h.CriterionID != "" {
		if _, known := criteria[h.CriterionID]; !known {
			h.CriterionID = ""
			dropped = true
		}
	}
	if h.IntentJudgment != "" {
		allowed := h.IntentJudgment == model.JudgmentExpectedChange || h.IntentJudgment == model.JudgmentUnexpectedChange
		if !allowed || h.Status != model.StatusDiverged || h.CriterionID == "" {
			h.IntentJudgment = ""
			dropped = true
		}
	}
	if !dropped {
		return ""
	}
	return fmt.Sprintf("The intent link of hypothesis %s was discarded: an intent judgment is kept only on a DIVERGED hypothesis with a known criterion, and a criterion only when the intent defines it.", redact.TruncateUTF8(h.ID, 64))
}

// writeIntentSections renders "## Intent Test Failures" and "## Intent
// Criteria" (F5). The failures come from r.IntentTestFailures, which only
// Finalize fills; the per-criterion outcomes use the evidence statuses Finalize
// accepts. Nothing is counted as a ratio, and a test that ran without failing
// is never presented as the criterion holding. Every string goes through
// inline().
func writeIntentSections(b *bytes.Buffer, r *model.Report) {
	l := verifyReport(r)
	criteria := criterionIndex(r.IntentCriteria)
	line(b, "\n## Intent Test Failures\n")
	line(b, intentFailuresIntro+"\n")
	if len(r.IntentTestFailures) == 0 {
		line(b, noIntentFailureText)
	}
	for _, h := range r.IntentTestFailures {
		location := "no location"
		if h.Path != "" {
			location = inline(h.Path)
			if h.Line > 0 {
				location += fmt.Sprintf(":%d", h.Line)
			}
		}
		fmt.Fprintf(b, "- **%s** %s — %s\n", inline(h.Severity), inline(h.Title), location)
		if c, ok := criteria[h.CriterionID]; ok {
			fmt.Fprintf(b, "  Criterion %s (intent line %d): \"%s\"\n", inline(c.ID), c.Line, inline(c.Text))
		}
		fmt.Fprintf(b, "  Evidence: %s\n", inline(strings.Join(h.EvidenceIDs, ", ")))
		for _, id := range h.EvidenceIDs {
			e, ok := l.item(id)
			if !ok || e.Kind != model.EvidenceIntentTest || e.CriterionID != h.CriterionID || l.verified[id] != model.StatusIntentTestFailed {
				continue
			}
			lexical := ""
			switch e.Runner {
			case harness.RunnerJest:
				lexical = "; read lexically for JavaScript/TypeScript"
			case harness.RunnerPytest:
				lexical = "; read lexically for Python"
			}
			fmt.Fprintf(b, "  Test %s (%s) failed on an assertion; names it shares with declarations the change added or modified (matched by name, not resolved%s): %s.\n", inline(e.Path), inline(strings.Join(e.TestNames, ", ")), lexical, inline(strings.Join(e.ReferencedSymbols, ", ")))
		}
	}
	line(b, "\n## Intent Criteria\n")
	if len(r.IntentCriteria) == 0 {
		line(b, noCriteriaText)
		return
	}
	sha := ""
	if r.IntentSHA256 != "" {
		sha = "; SHA-256 " + inline(r.IntentSHA256)
	}
	line(b, fmt.Sprintf(criteriaIntro, sha)+"\n")
	for _, c := range r.IntentCriteria {
		prefix := fmt.Sprintf("- %s (line %d): \"%s\"", inline(c.ID), c.Line, inline(c.Text))
		if _, ok := criteria[c.ID]; !ok {
			line(b, prefix+" — the ID is invalid or not unique, so nothing refers to it")
			continue
		}
		line(b, prefix+" — "+criterionOutcomes(r, l.verified, c.ID))
	}
}

// criterionOutcomes lists, in evidence order, what the intent tests of one
// criterion showed: at most maxOutcomesShown evidence IDs, then a count.
func criterionOutcomes(r *model.Report, verified map[string]string, id string) string {
	var outcomes []string
	passed := false
	for _, e := range r.Evidence {
		if e.Kind != model.EvidenceIntentTest || e.CriterionID != id {
			continue
		}
		switch verified[e.ID] {
		case model.StatusIntentTestFailed:
			outcomes = append(outcomes, "failed on an assertion on the candidate: "+inline(e.ID))
		case model.StatusIntentTestPassed:
			outcomes = append(outcomes, "ran without failing: "+inline(e.ID))
			passed = true
		default:
			outcomes = append(outcomes, "inconclusive: "+inline(e.ID))
		}
	}
	if len(outcomes) == 0 {
		return "no intent test recorded"
	}
	more := ""
	if len(outcomes) > maxOutcomesShown {
		more = fmt.Sprintf("; and %d more", len(outcomes)-maxOutcomesShown)
		outcomes = outcomes[:maxOutcomesShown]
	}
	s := strings.Join(outcomes, "; ") + more
	if passed {
		s += passedCaveat
	}
	return s
}

// writeIntentLink renders a hypothesis's criterion link, quoting the criterion
// from the report's criteria (never model text), and its intent_judgment with
// the fixed label "Model judgment (not evidence)", after the hypothesis's own
// lines (F5). A judgment is rendered only on a DIVERGED hypothesis with a
// known criterion, which is all Finalize keeps.
func writeIntentLink(b *bytes.Buffer, r *model.Report, h model.Hypothesis) {
	if h.CriterionID == "" {
		return
	}
	c, ok := criterionIndex(r.IntentCriteria)[h.CriterionID]
	if !ok {
		return
	}
	label := "Related criterion (model-selected)"
	if h.Status == model.StatusIntentTestFailed {
		label = "Criterion"
	}
	fmt.Fprintf(b, "  %s: %s (intent line %d): \"%s\"\n", label, inline(c.ID), c.Line, inline(c.Text))
	if h.Status != model.StatusDiverged {
		return
	}
	switch h.IntentJudgment {
	case model.JudgmentExpectedChange:
		line(b, "  Model judgment (not evidence): expected change"+judgmentSuffix)
	case model.JudgmentUnexpectedChange:
		line(b, "  Model judgment (not evidence): unexpected change"+judgmentSuffix)
	}
}
