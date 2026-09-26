package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const intentTestFile = "pkg/swiftproof_intent_ac1_test.go"

// goIntentOutput is a go test -json log of TestIntentAC1 ending with action
// and carrying the given output lines.
func goIntentOutput(action string, outputs ...string) string {
	var b strings.Builder
	event := func(fields map[string]string) {
		fields["Package"] = "example.test/pkg"
		data, _ := json.Marshal(fields)
		b.Write(data)
		b.WriteByte('\n')
	}
	event(map[string]string{"Action": "run", "Test": "TestIntentAC1"})
	for _, o := range outputs {
		event(map[string]string{"Action": "output", "Test": "TestIntentAC1", "Output": o})
	}
	event(map[string]string{"Action": action, "Test": "TestIntentAC1"})
	return b.String()
}

const intentAssertion = "    swiftproof_intent_ac1_test.go:7: Discount(100) = 100, want 90\n"

// intentProofReport holds one candidate-only intent test that failed on an
// assertion and references the new Discount, and a critical hypothesis citing
// it for AC-1.
func intentProofReport() *model.Report {
	command := []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestIntentAC1)$"}
	return &model.Report{
		Intent:         "## Acceptance criteria\n- Orders of 100 or more get 10 off\n- Orders of 50 or more ship free\n",
		IntentSHA256:   strings.Repeat("ab", 32),
		IntentCriteria: []model.IntentCriterion{{ID: "AC-1", Text: "Orders of 100 or more get 10 off", Line: 2}, {ID: "AC-2", Text: "Orders of 50 or more ship free", Line: 3}},
		Change: model.Change{Files: []model.ChangedFile{{Path: "pkg/cart.go", Status: "M", Additions: 2, Hunks: []model.Hunk{{NewStart: 5, NewLines: 2, Lines: []model.DiffLine{
			{Kind: "add", NewLine: 5, Content: "func Discount(total int) int {"},
			{Kind: "add", NewLine: 6, Content: "\tif total > 100 {"},
		}}}}}},
		Checks: []model.Check{{ID: "check-1", Kind: model.CheckGeneratedIntent, Status: "FAIL", ExitCode: 1, Command: command, Output: goIntentOutput("fail", intentAssertion)}},
		Evidence: []model.Evidence{{ID: "evidence-1", Kind: model.EvidenceIntentTest, Description: "100 gets 10 off", Path: intentTestFile, CheckID: "check-1", CriterionID: "AC-1",
			ReferencedSymbols: []string{"Discount"}, Status: model.StatusIntentTestFailed, Runner: "go_test_json", TestNames: []string{"TestIntentAC1"}}},
		Hypotheses: []model.Hypothesis{{ID: "hypothesis-1", Title: "Discount skips orders of exactly 100", Severity: "critical", Status: model.StatusIntentTestFailed, Rationale: "The intent test for AC-1 failed.", EvidenceIDs: []string{"evidence-1"}, Path: "pkg/cart.go", Line: 6, CriterionID: "AC-1"}},
	}
}

func TestFinalizeRequiresIntentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.Report)
	}{
		{"criterion mismatch", func(r *model.Report) { r.Hypotheses[0].CriterionID = "AC-2" }},
		{"unknown criterion", func(r *model.Report) { r.Hypotheses[0].CriterionID = "AC-3"; r.Evidence[0].CriterionID = "AC-3" }},
		{"duplicate criterion ID", func(r *model.Report) {
			r.IntentCriteria = append(r.IntentCriteria, model.IntentCriterion{ID: "AC-1", Text: "again", Line: 9})
		}},
		{"no criteria", func(r *model.Report) { r.IntentCriteria = nil }},
		{"evidence kind differential_test", func(r *model.Report) { r.Evidence[0].Kind = model.EvidenceDifferentialTest }},
		{"check kind generated_test_candidate", func(r *model.Report) { r.Checks[0].Kind = model.CheckGeneratedCandidate }},
		{"duplicate check", func(r *model.Report) { r.Checks = append(r.Checks, r.Checks[0]) }},
		{"duplicate evidence", func(r *model.Report) { r.Evidence = append(r.Evidence, r.Evidence[0]) }},
		{"missing check", func(r *model.Report) { r.Checks = nil }},
		{"empty command", func(r *model.Report) { r.Checks[0].Command = nil }},
		{"replayed check", func(r *model.Report) {
			r.Checks[0].Cache = &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("a", 64), LiveRuns: 2}
		}},
		{"truncated", func(r *model.Report) { r.Checks[0].Truncated = true }},
		{"exit 125", func(r *model.Report) { r.Checks[0].ExitCode = 125 }},
		{"timeout", func(r *model.Report) { r.Checks[0].Status = "TIMEOUT" }},
		{"setup failure", func(r *model.Report) { r.Checks[0].Status = "ERROR" }},
		{"unknown runner", func(r *model.Report) { r.Evidence[0].Runner = "pytest" }},
		{"unrelated events", func(r *model.Report) {
			r.Checks[0].Output = strings.ReplaceAll(r.Checks[0].Output, "TestIntentAC1", "TestOther")
		}},
		{"panic", func(r *model.Report) {
			r.Checks[0].Output = goIntentOutput("fail", intentAssertion, "panic: runtime error: invalid memory address or nil pointer dereference\n")
		}},
		{"no assertion message", func(r *model.Report) {
			r.Checks[0].Output = goIntentOutput("fail", "--- FAIL: TestIntentAC1 (0.00s)\n")
		}},
		{"assertion of another file", func(r *model.Report) {
			r.Checks[0].Output = goIntentOutput("fail", "    cart_test.go:7: elsewhere\n")
		}},
		{"stored status passed", func(r *model.Report) { r.Evidence[0].Status = model.StatusIntentTestPassed }},
		{"stored status unverified", func(r *model.Report) { r.Evidence[0].Status = model.StatusUnverified }},
		{"check passed", func(r *model.Report) {
			r.Checks[0].Status, r.Checks[0].ExitCode, r.Checks[0].Output = "PASS", 0, goIntentOutput("pass")
		}},
		{"no referenced symbols", func(r *model.Report) { r.Evidence[0].ReferencedSymbols = nil }},
		{"symbol not on an added line", func(r *model.Report) { r.Evidence[0].ReferencedSymbols = []string{"Total"} }},
		{"symbol not an identifier", func(r *model.Report) { r.Evidence[0].ReferencedSymbols = []string{"Discount", "a-b"} }},
		{"added line only in a test file", func(r *model.Report) { r.Change.Files[0].Path = "pkg/cart_test.go" }},
		{"deleted file", func(r *model.Report) { r.Change.Files[0].Status = "D" }},
		{"base check cited", func(r *model.Report) { r.Evidence[0].BaseCheckID = "check-1" }},
		{"repeat check cited", func(r *model.Report) { r.Evidence[0].RepeatCheckID = "check-1" }},
		{"secret-shaped path", func(r *model.Report) { r.Evidence[0].Path = "pkg/password=hunter22_test.go" }},
		{"redaction marker in a test name", func(r *model.Report) { r.Evidence[0].TestNames = []string{"Test[REDACTED]"} }},
		{"invented evidence ID", func(r *model.Report) { r.Hypotheses[0].EvidenceIDs = []string{"evidence-9"} }},
		{"no evidence ID", func(r *model.Report) { r.Hypotheses[0].EvidenceIDs = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := intentProofReport()
			tc.mutate(r)
			Finalize(r, true)
			if r.Hypotheses[0].Status != model.StatusUnverified || len(r.IntentTestFailures) != 0 || r.ExitCode != 2 {
				t.Fatalf("status %s, failures %d, exit %d", r.Hypotheses[0].Status, len(r.IntentTestFailures), r.ExitCode)
			}
		})
	}
	// The unmodified report is accepted, at critical severity, without exit 1.
	for _, ci := range []bool{true, false} {
		r := intentProofReport()
		Finalize(r, ci)
		h := r.Hypotheses[0]
		if h.Status != model.StatusIntentTestFailed || len(r.IntentTestFailures) != 1 || !reflect.DeepEqual(r.IntentTestFailures[0], h) || len(r.ReproducedIssues) != 0 {
			t.Fatalf("ci=%v: %+v / %+v", ci, h, r.IntentTestFailures)
		}
		want := 0
		if ci {
			want = 2
		}
		if r.ExitCode != want {
			t.Fatalf("ci=%v: exit %d, want %d", ci, r.ExitCode, want)
		}
	}
}

func TestIntentTestPassedIsRederived(t *testing.T) {
	r := intentProofReport()
	r.Checks[0].Status, r.Checks[0].ExitCode, r.Checks[0].Output = "PASS", 0, goIntentOutput("pass")
	r.Evidence[0].Status, r.Evidence[0].ReferencedSymbols = model.StatusIntentTestPassed, nil
	l := verifyReport(r)
	if l.verified["evidence-1"] != model.StatusIntentTestPassed {
		t.Fatalf("verified %+v", l.verified)
	}
	// A pass supports no hypothesis status.
	for _, claim := range allClaims {
		r := intentProofReport()
		r.Checks[0].Status, r.Checks[0].ExitCode, r.Checks[0].Output = "PASS", 0, goIntentOutput("pass")
		r.Evidence[0].Status = model.StatusIntentTestPassed
		r.Hypotheses[0].Status = claim
		r.Hypotheses[0].Rationale = "nonblank"
		Finalize(r, false)
		if got := r.Hypotheses[0].Status; got != model.StatusUnverified {
			t.Fatalf("claim %q citing INTENT_TEST_PASSED finalized as %s", claim, got)
		}
	}
	// Tampering a passing record's check makes it inconclusive.
	r = intentProofReport()
	r.Checks[0].Status, r.Checks[0].ExitCode, r.Checks[0].Output = "PASS", 0, goIntentOutput("skip")
	r.Evidence[0].Status = model.StatusIntentTestPassed
	if _, ok := verifyReport(r).verified["evidence-1"]; ok {
		t.Fatal("a skipped test was re-derived as a pass")
	}
}

func TestIntentEvidenceCannotProveOtherStatuses(t *testing.T) {
	for _, claim := range []string{model.StatusReproduced, model.StatusNotReproduced, model.StatusDismissed, model.StatusDiverged} {
		r := intentProofReport()
		r.Hypotheses[0].Status = claim
		Finalize(r, true)
		if r.Hypotheses[0].Status != model.StatusUnverified || len(r.ReproducedIssues) != 0 || len(r.IntentTestFailures) != 0 || r.ExitCode != 2 {
			t.Fatalf("%s citing intent evidence: %s, exit %d", claim, r.Hypotheses[0].Status, r.ExitCode)
		}
	}
}

func TestIntentFailureWithJestRunner(t *testing.T) {
	results := func(message string) string {
		b, _ := json.Marshal(map[string]any{"testResults": []any{map[string]any{"name": "/workspace/src/cart.intent.test.ts", "message": "", "assertionResults": []any{
			map[string]any{"ancestorTitles": []string{}, "title": "orders of 100 get 10 off", "status": "failed", "failureMessages": []string{message}},
		}}}})
		return string(b)
	}
	build := func(message string) *model.Report {
		r := intentProofReport()
		r.Change.Files[0] = model.ChangedFile{Path: "src/cart.ts", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "add", NewLine: 5, Content: "export function discount(total: number): number {"}}}}}
		r.Checks[0].Command = []string{"npx", "vitest", "run", "src/cart.intent.test.ts", "--reporter=json", "--outputFile=/tmp/swiftproof-test-results.json"}
		r.Checks[0].Output = "vitest output"
		r.Checks[0].Results = results(message)
		r.Evidence[0].Path, r.Evidence[0].Runner, r.Evidence[0].TestNames, r.Evidence[0].ReferencedSymbols = "src/cart.intent.test.ts", "jest_json", []string{"orders of 100 get 10 off"}, []string{"discount"}
		return r
	}
	r := build("AssertionError: expected 100 to be 90")
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusIntentTestFailed || r.ExitCode != 2 {
		t.Fatalf("jest failure: %s exit %d", r.Hypotheses[0].Status, r.ExitCode)
	}
	r = build("TypeError: discount is not a function")
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusUnverified {
		t.Fatalf("a runtime error supported the claim: %s", r.Hypotheses[0].Status)
	}
}

func TestIntentJudgmentNeverChangesOutcome(t *testing.T) {
	// On a REPRODUCED claim the judgment is dropped with a note; nothing else moves.
	base := proofReport()
	base.IntentCriteria = []model.IntentCriterion{{ID: "AC-1", Text: "Guests are rejected", Line: 1}}
	judged := proofReport()
	judged.IntentCriteria = base.IntentCriteria
	judged.Hypotheses[0].CriterionID, judged.Hypotheses[0].IntentJudgment = "AC-1", model.JudgmentExpectedChange
	Finalize(base, true)
	Finalize(judged, true)
	h := judged.Hypotheses[0]
	if h.Status != model.StatusReproduced || judged.ExitCode != 1 || len(judged.ReproducedIssues) != 1 || h.IntentJudgment != "" || h.CriterionID != "AC-1" {
		t.Fatalf("judged %+v exit %d", h, judged.ExitCode)
	}
	if !reflect.DeepEqual(base.ReviewTargets, judged.ReviewTargets) || !reflect.DeepEqual(base.ReviewSurface, judged.ReviewSurface) || h.Severity != base.Hypotheses[0].Severity {
		t.Fatal("the judgment moved a target, the surface or the severity")
	}
	if len(judged.Unverified) != 1 || !strings.Contains(judged.Unverified[0], "The intent link of hypothesis h1 was discarded") {
		t.Fatalf("unverified %q", judged.Unverified)
	}
	md := string(Markdown(judged))
	if strings.Contains(md, "Model judgment") || !strings.Contains(section(t, md, "## Reproduced Issues"), "Related criterion (model-selected): AC-1 (intent line 1): \"Guests are rejected\"") {
		t.Fatalf("reproduced issue rendering:\n%s", md)
	}
	// On an accepted DIVERGED hypothesis it is kept, labelled, and inert.
	r := &model.Report{
		IntentCriteria: []model.IntentCriterion{{ID: "AC-1", Text: "Discounts round down", Line: 1}},
		Evidence:       []model.Evidence{{ID: "obs", Kind: model.EvidenceDifferentialObservation, Status: model.StatusDiverged}},
		Hypotheses:     []model.Hypothesis{{ID: "h1", Title: "Discount changed", Severity: "high", Status: model.StatusDiverged, Rationale: "r", EvidenceIDs: []string{"obs"}, CriterionID: "AC-1", IntentJudgment: model.JudgmentExpectedChange}},
	}
	plain := &model.Report{IntentCriteria: r.IntentCriteria, Evidence: r.Evidence, Hypotheses: []model.Hypothesis{r.Hypotheses[0]}}
	plain.Hypotheses[0].IntentJudgment = ""
	concludeFresh(r, verifiedLedger(r, "obs"), true, citedDivergences)
	concludeFresh(plain, verifiedLedger(plain, "obs"), true, citedDivergences)
	if r.Hypotheses[0].Status != model.StatusDiverged || r.Hypotheses[0].IntentJudgment != model.JudgmentExpectedChange || r.ExitCode != 2 || len(r.Unverified) != 0 {
		t.Fatalf("diverged %+v exit %d unverified %q", r.Hypotheses[0], r.ExitCode, r.Unverified)
	}
	if r.ExitCode != plain.ExitCode || !reflect.DeepEqual(r.ReviewTargets, plain.ReviewTargets) || !reflect.DeepEqual(r.Divergences, plain.Divergences) {
		t.Fatal("the judgment changed the outcome")
	}
	var b bytes.Buffer
	writeIntentLink(&b, r, r.Hypotheses[0])
	if got := b.String(); got != "  Related criterion (model-selected): AC-1 (intent line 1): \"Discounts round down\"\n  Model judgment (not evidence): expected change. It changes no status, severity, review target or exit code.\n" {
		t.Fatalf("intent link %q", got)
	}
}

func TestInvalidIntentLinkDiscardedIdempotently(t *testing.T) {
	r := intentProofReport()
	r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: "hypothesis-2", Title: "Unrelated", Severity: "low", Status: model.StatusUnverified, Rationale: "r", CriterionID: "AC-7", IntentJudgment: "correct"})
	Finalize(r, false)
	h := r.Hypotheses[1]
	if h.CriterionID != "" || h.IntentJudgment != "" || len(r.Unverified) != 1 || !strings.Contains(r.Unverified[0], "hypothesis-2") {
		t.Fatalf("%+v %q", h, r.Unverified)
	}
	first, _ := json.Marshal(r)
	exit := r.ExitCode
	Finalize(r, false)
	second, _ := json.Marshal(r)
	if string(first) != string(second) || exit != r.ExitCode {
		t.Fatalf("Finalize is not idempotent:\n%s\n%s", first, second)
	}
	// Write -> read -> Finalize -> Write is byte-identical, with the intent
	// sections, secret-shaped strings and the accepted failure.
	r = intentProofReport()
	r.Intent += "- token=ghp_0123456789abcdefghij must never leak\n"
	r.Hypotheses[0].Rationale = "password: swordfish"
	Finalize(r, true)
	dir := t.TempDir()
	data, md := writeAndRead(t, dir, r)
	var again model.Report
	if err := json.Unmarshal(data, &again); err != nil {
		t.Fatal(err)
	}
	Finalize(&again, true)
	data2, md2 := writeAndRead(t, t.TempDir(), &again)
	if string(data) != string(data2) || string(md) != string(md2) {
		t.Fatalf("re-render differs:\n%s\n%s", md, md2)
	}
	if again.Hypotheses[0].Status != model.StatusIntentTestFailed || len(again.IntentTestFailures) != 1 || again.ExitCode != 2 || strings.Contains(string(data), "swordfish") || strings.Contains(string(md), "ghp_0123456789") {
		t.Fatalf("round trip: %s exit %d", again.Hypotheses[0].Status, again.ExitCode)
	}
}

func TestIntentMarkdownSectionsOrderAndWording(t *testing.T) {
	// Without an intent the sections are absent.
	r := proofReport()
	Finalize(r, false)
	if md := string(Markdown(r)); strings.Contains(md, "## Intent") {
		t.Fatalf("intent sections without an intent:\n%s", md)
	}
	r = intentProofReport()
	// A second, passing intent test for AC-2 and an inconclusive one for AC-1.
	command := r.Checks[0].Command
	r.Checks = append(r.Checks,
		model.Check{ID: "check-2", Kind: model.CheckGeneratedIntent, Status: "PASS", Command: command, Output: goIntentOutput("pass")},
		model.Check{ID: "check-3", Kind: model.CheckGeneratedIntent, Status: "TIMEOUT", ExitCode: -1, Command: command},
	)
	r.Evidence = append(r.Evidence,
		model.Evidence{ID: "evidence-2", Kind: model.EvidenceIntentTest, Description: "free shipping", Path: intentTestFile, CheckID: "check-2", CriterionID: "AC-2", Status: model.StatusIntentTestPassed, Runner: "go_test_json", TestNames: []string{"TestIntentAC1"}},
		model.Evidence{ID: "evidence-3", Kind: model.EvidenceIntentTest, Description: "again", Path: intentTestFile, CheckID: "check-3", CriterionID: "AC-1", Status: model.StatusUnverified, Runner: "go_test_json", TestNames: []string{"TestIntentAC1"}},
	)
	r.IntentCriteria = append(r.IntentCriteria,
		model.IntentCriterion{ID: "AC-3", Text: "Totals *never* go [negative](x) | `#1` <b>_", Line: 4},
		model.IntentCriterion{ID: "AC-0", Text: "invalid ID", Line: 5},
	)
	Finalize(r, true)
	md := string(Markdown(r))
	order := []string{"## Reproduced Issues", "## Behavior Divergences", "## Intent Test Failures", "## Intent Criteria", "## Unverified Areas", "## Recorded Evidence"}
	last := -1
	for _, heading := range order {
		i := strings.Index(md, "\n"+heading+"\n")
		if i <= last {
			t.Fatalf("%q out of order:\n%s", heading, md)
		}
		last = i
	}
	if stray := strayHeadings(md); len(stray) > 0 {
		t.Fatalf("unexpected headings %q", stray)
	}
	failures := section(t, md, "## Intent Test Failures")
	for _, want := range []string{
		intentFailuresIntro,
		"- **critical** Discount skips orders of exactly 100 — pkg/cart.go:6\n",
		"  Criterion AC-1 (intent line 2): \"Orders of 100 or more get 10 off\"\n",
		"  Evidence: evidence-1\n",
		"  Test pkg/swiftproof\\_intent\\_ac1\\_test.go (TestIntentAC1) failed on an assertion and references the changed symbols Discount.\n",
	} {
		if !strings.Contains(failures, want) {
			t.Errorf("failures section lacks %q:\n%s", want, failures)
		}
	}
	criteria := section(t, md, "## Intent Criteria")
	for _, want := range []string{
		"(criteria grammar v1; SHA-256 " + strings.Repeat("ab", 32) + ")",
		"- AC-1 (line 2): \"Orders of 100 or more get 10 off\" — failed on an assertion on the candidate: evidence-1; inconclusive: evidence-3\n",
		"- AC-2 (line 3): \"Orders of 50 or more ship free\" — ran without failing: evidence-2" + passedCaveat + "\n",
		"- AC-3 (line 4): \"Totals \\*never\\* go \\[negative\\]\\(x\\) \\| \\`\\#1\\` &lt;b&gt;\\_\" — no intent test recorded\n",
		"- AC-0 (line 5): \"invalid ID\" — the ID is invalid or not unique, so nothing refers to it\n",
	} {
		if !strings.Contains(criteria, want) {
			t.Errorf("criteria section lacks %q:\n%s", want, criteria)
		}
	}
	// The Investigation Summary links the accepted failure to its criterion.
	if !strings.Contains(section(t, md, "## Investigation Summary"), "  Criterion: AC-1 (intent line 2): \"Orders of 100 or more get 10 off\"\n") {
		t.Fatalf("investigation link missing:\n%s", md)
	}
	if !strings.Contains(section(t, md, "## Recorded Evidence"), "Candidate check: check-1 (candidate-only; no baseline control)") {
		t.Fatal("recorded evidence line")
	}
	// No affirmative or ratio wording about criteria anywhere in the intent sections.
	intentText := failures + criteria
	deny := regexp.MustCompile(`(?i)\b(satisfied|satisfies|met|meets|verified|contradicts?|contradicted|contradiction|implemented|accepted criteria|tested|correct|approved|bug|regression)\b|%|✓|✔|\b\d+\s*/\s*\d+\b|\d+ of \d+ criteria`)
	if m := deny.FindString(intentText); m != "" {
		t.Fatalf("intent sections say %q:\n%s", m, intentText)
	}
}

func TestIntentSectionsEmptyStates(t *testing.T) {
	r := &model.Report{Intent: "Just prose, no list.", IntentCriteria: []model.IntentCriterion{}}
	Finalize(r, false)
	md := string(Markdown(r))
	if !strings.Contains(section(t, md, "## Intent Test Failures"), noIntentFailureText) || !strings.Contains(section(t, md, "## Intent Criteria"), noCriteriaText) {
		t.Fatalf("empty states:\n%s", md)
	}
	// More than ten intent tests for one criterion are summarized.
	r = intentProofReport()
	for i := 2; i <= 13; i++ {
		r.Evidence = append(r.Evidence, model.Evidence{ID: fmt.Sprintf("evidence-%d", i), Kind: model.EvidenceIntentTest, CriterionID: "AC-2", Status: model.StatusUnverified, Runner: "go_test_json", Path: intentTestFile, CheckID: "check-1", TestNames: []string{"TestIntentAC1"}})
	}
	Finalize(r, false)
	criteria := section(t, string(Markdown(r)), "## Intent Criteria")
	if !strings.Contains(criteria, "inconclusive: evidence-11; and 2 more\n") || strings.Contains(criteria, "evidence-12") {
		t.Fatalf("outcome cap:\n%s", criteria)
	}
}
