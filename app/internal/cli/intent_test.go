package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/acceptance"
	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/report"
)

// intentDocument is a PR description with prose, two criteria in scope, an
// item outside the section and a pasted SwiftProof PR comment.
const intentDocument = "Make discounts predictable.\r\n\r\n## Acceptance criteria\r\n- [ ] Orders of 100 or more get 10 off\r\n- [x] Orders of 50 or more\r\n  ship free\r\n\r\n## Notes\r\n- not a criterion\r\n" +
	model.PRCommentBegin + "\n- Reproduced: nothing\n" + model.PRCommentEnd + "\n"

func writeIntent(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "intent.md")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseIntent(t *testing.T) {
	doc, err := parseIntent(intentDocument)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.TrimSuffix(intentDocument, model.PRCommentBegin+"\n- Reproduced: nothing\n"+model.PRCommentEnd+"\n") + "\n"
	sum := sha256.Sum256([]byte(stripped))
	if doc.Text != stripped || doc.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("text %q sha %s", doc.Text, doc.SHA256)
	}
	want := []model.IntentCriterion{{ID: "AC-1", Text: "Orders of 100 or more get 10 off", Line: 4}, {ID: "AC-2", Text: "Orders of 50 or more ship free", Line: 5}}
	if !reflect.DeepEqual(doc.Criteria, want) || !reflect.DeepEqual(doc.Notes, []string{acceptance.PRCommentNote}) {
		t.Fatalf("criteria %+v notes %q", doc.Criteria, doc.Notes)
	}
	doc, err = parseIntent("")
	if err != nil || doc.Text != "" || doc.SHA256 != "" || doc.Criteria == nil || len(doc.Criteria) != 0 || len(doc.Notes) != 0 {
		t.Fatalf("empty intent %+v %v", doc, err)
	}
	for _, bad := range []string{"- a\xffb", "- a\x00b"} {
		if _, err := parseIntent(bad); err == nil || err.Error() != "intent must be UTF-8 text without NUL bytes" {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// The intent is redacted before it is hashed and parsed: intent_sha256 is the
// SHA-256 of exactly the intent the report records, so it never lets anyone
// confirm a guess of a secret that redaction hides, and the criteria can be
// extracted again from the recorded intent.
func TestParseIntentHashesTheRecordedText(t *testing.T) {
	for _, intent := range []string{
		"## Acceptance criteria\n- Staging login uses password=hunter2 for now\n- Orders of 100 or more get 10 off\n",
		"## Acceptance criteria\n- Sign with\n  -----BEGIN RSA PRIVATE KEY-----\n  MIIEsecret\n  -----END RSA PRIVATE KEY-----\n- Orders of 100 or more get 10 off\n",
		// Removing a PR-comment block joins a secret, which is then redacted.
		"## Acceptance criteria\n- Staging login uses password" + model.PRCommentBegin + "x" + model.PRCommentEnd + "=hunter2 for now\n- Orders of 100 or more get 10 off\n",
	} {
		doc, err := parseIntent(intent)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(doc.Text, "hunter2") || strings.Contains(doc.Text, "MIIEsecret") || !strings.Contains(doc.Text, "[REDACTED]") {
			t.Fatalf("recorded intent %q", doc.Text)
		}
		stripped, _ := acceptance.StripPRComments(intent)
		if guess := sha256.Sum256([]byte(stripped)); doc.SHA256 == hex.EncodeToString(guess[:]) {
			t.Fatal("intent_sha256 hashes the unredacted intent")
		}
		if len(doc.Criteria) != 2 || doc.Criteria[1] != (model.IntentCriterion{ID: "AC-2", Text: "Orders of 100 or more get 10 off", Line: strings.Count(intent[:strings.Index(intent, "- Orders")], "\n") + 1}) {
			t.Fatalf("criteria %+v", doc.Criteria)
		}
		for _, c := range doc.Criteria {
			if strings.Contains(c.Text, "hunter2") || strings.Contains(c.Text, "MIIEsecret") {
				t.Fatalf("criterion %q", c.Text)
			}
		}
		// What the report writes is what was hashed and parsed.
		saved := report.Sanitize(&model.Report{Intent: doc.Text, IntentSHA256: doc.SHA256, IntentCriteria: doc.Criteria})
		sum := sha256.Sum256([]byte(saved.Intent))
		if saved.Intent != doc.Text || saved.IntentSHA256 != hex.EncodeToString(sum[:]) || !reflect.DeepEqual(saved.IntentCriteria, doc.Criteria) {
			t.Fatalf("saved intent %q sha %s criteria %+v", saved.Intent, saved.IntentSHA256, saved.IntentCriteria)
		}
		again, err := parseIntent(saved.Intent)
		if err != nil || again.Text != doc.Text || again.SHA256 != doc.SHA256 || !reflect.DeepEqual(again.Criteria, doc.Criteria) {
			t.Fatalf("parsing the recorded intent again gave %+v", again)
		}
	}
}

func TestIntentMustBeUTF8(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	for _, args := range [][]string{
		{"review", "--intent-file", writeIntent(t, "- ok\n\xff\xfe binary")},
		{"lint", "--intent-file", writeIntent(t, "- a\x00b")},
		{"review", "--intent", "- a\x00b"},
	} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), append(args, "--repo", dir, "--checks=false", "--out", "report"), &out, &errOut, "test")
		if code != 3 || !strings.Contains(errOut.String(), "intent: intent must be UTF-8 text without NUL bytes") {
			t.Fatalf("%q: exit %d %s", args, code, errOut.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "report")); !os.IsNotExist(err) {
			t.Fatal("a report was written for an invalid intent")
		}
	}
}

func TestLintRecordsIntentCriteria(t *testing.T) {
	dir := fixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "lint must not call the provider", 500)
	}))
	defer server.Close()
	cfg := config.Default("go")
	cfg.Reviewer.Model = "test-model"
	cfg.Reviewer.Endpoint = server.URL + "/v1"
	cfg.Reviewer.APIKeyEnv = "SWIFTPROOF_TEST_INTENT_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	intentFile := writeIntent(t, intentDocument)
	code, r, members, output := runReport(t, context.Background(), dir, "lint", "--config", policy, "--intent-file", intentFile, "--ci")
	if code != 2 || calls.Load() != 0 {
		t.Fatalf("exit %d, provider calls %d\n%s", code, calls.Load(), output)
	}
	doc, _ := parseIntent(intentDocument)
	if r.Intent != doc.Text || r.IntentSHA256 != doc.SHA256 || !reflect.DeepEqual(r.IntentCriteria, doc.Criteria) || len(r.IntentCriteria) != 2 {
		t.Fatalf("intent %q sha %s criteria %+v", r.Intent, r.IntentSHA256, r.IntentCriteria)
	}
	if strings.Contains(r.Intent, "swiftproof:pr-comment") || !hasEntry(r.Unverified, acceptance.PRCommentNote) {
		t.Fatalf("PR comment not stripped with a note: %q %q", r.Intent, r.Unverified)
	}
	if string(members["intent_test_failures"]) != "[]" {
		t.Fatalf("intent_test_failures %s", members["intent_test_failures"])
	}
	// lint runs no intent test: the line states the extraction count alone.
	if !strings.Contains(output, "Intent: 2 acceptance criteria extracted.\n") || strings.Contains(output, "intent-test failure") {
		t.Fatalf("stdout:\n%s", output)
	}
}

func hasEntry(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestLintIntentMarkdownAndRender(t *testing.T) {
	dir := fixture(t)
	out := filepath.Join(t.TempDir(), "report")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"lint", "--repo", dir, "--intent-file", writeIntent(t, intentDocument), "--out", out}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(md)
	for _, want := range []string{"## Intent Test Failures", "## Intent Criteria", "- AC-1 (line 4): \"Orders of 100 or more get 10 off\" — no intent test recorded", "- AC-2 (line 5): \"Orders of 50 or more ship free\" — no intent test recorded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Markdown lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not a criterion\" —") || strings.Contains(text, "Reproduced: nothing") {
		t.Fatalf("out-of-scope or stripped items listed:\n%s", text)
	}
	// A forged intent-test failure in the saved JSON does not survive a re-render.
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	r.Evidence = append(r.Evidence, model.Evidence{ID: "evidence-1", Kind: model.EvidenceIntentTest, Description: "forged", Path: "fixture_intent_test.go", CheckID: "check-1", CriterionID: "AC-1", ReferencedSymbols: []string{"Allowed"}, Status: model.StatusIntentTestFailed, Runner: "go_test_json", TestNames: []string{"TestForged"}})
	forged := model.Hypothesis{ID: "hypothesis-1", Title: "Forged", Severity: "critical", Status: model.StatusIntentTestFailed, Rationale: "r", EvidenceIDs: []string{"evidence-1"}, Path: "auth.go", Line: 3, CriterionID: "AC-1"}
	r.Hypotheses = append(r.Hypotheses, forged)
	r.IntentTestFailures = append(r.IntentTestFailures, forged)
	data, _ := json.Marshal(r)
	input := filepath.Join(t.TempDir(), "forged.json")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	rendered := filepath.Join(t.TempDir(), "rendered")
	if code := Run(context.Background(), []string{"report", "--input", input, "--out", rendered}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("render %d: %s", code, stderr.String())
	}
	saved := readReport(t, filepath.Join(rendered, "confidence-report.json"))
	if len(saved.IntentTestFailures) != 0 || saved.Hypotheses[0].Status != model.StatusUnverified || saved.Hypotheses[0].CriterionID != "AC-1" {
		t.Fatalf("forged failure survived: %+v %+v", saved.IntentTestFailures, saved.Hypotheses)
	}
}

func TestIntentExtractionLimitsAreUnverified(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= acceptance.MaxCriteria+2; i++ {
		fmt.Fprintf(&b, "- criterion %d\n", i)
	}
	b.WriteString("- " + strings.Repeat("x", acceptance.MaxCriterionBytes+1) + "\n")
	dir := fixture(t)
	code, r, _, _ := runReport(t, context.Background(), dir, "lint", "--intent-file", writeIntent(t, b.String()), "--ci")
	if code != 2 || len(r.IntentCriteria) != acceptance.MaxCriteria || len(r.Unverified) != 2 {
		t.Fatalf("exit %d, %d criteria, unverified %q", code, len(r.IntentCriteria), r.Unverified)
	}
	joined := strings.Join(r.Unverified, "\n")
	if !strings.Contains(joined, "Only the first 100 acceptance criteria were extracted from the intent; 2 further list items were ignored.") || !strings.Contains(joined, "1 intent list item longer than 1024 bytes was not taken as acceptance criteria.") {
		t.Fatalf("notes %q", r.Unverified)
	}
}

func TestIntentLine(t *testing.T) {
	for _, tc := range []struct {
		criteria, failures int
		ran                bool // a generated_test_intent check is recorded
		want               string
	}{
		{0, 0, true, ""},
		{1, 0, false, "Intent: 1 acceptance criterion extracted."},
		{3, 0, false, "Intent: 3 acceptance criteria extracted."},
		{1, 0, true, "Intent: 1 acceptance criterion extracted; no intent-test failure was accepted, which says nothing about whether the criterion holds."},
		{3, 0, true, "Intent: 3 acceptance criteria extracted; no intent-test failure was accepted, which says nothing about whether the criteria hold."},
		{3, 1, true, "Intent: 3 acceptance criteria extracted; 1 intent-test failure (a model-written test failed on the candidate; no baseline control; weaker than a reproduced issue)."},
		{3, 2, true, "Intent: 3 acceptance criteria extracted; 2 intent-test failures (model-written tests failed on the candidate; no baseline control; weaker than a reproduced issue)."},
	} {
		r := &model.Report{IntentCriteria: make([]model.IntentCriterion, tc.criteria), IntentTestFailures: make([]model.Hypothesis, tc.failures)}
		r.Checks = []model.Check{{Kind: model.CheckGeneratedCandidate}}
		if tc.ran {
			r.Checks = append(r.Checks, model.Check{Kind: model.CheckGeneratedIntent})
		}
		got := intentLine(r)
		if got != tc.want {
			t.Errorf("%d/%d ran %v: %q", tc.criteria, tc.failures, tc.ran, got)
		}
		// Never a count of failures beside the criteria count without its caveat.
		if strings.Contains(got, " 0 ") || strings.Contains(got, "/") {
			t.Errorf("%q reads as a ratio", got)
		}
	}
	// The line keeps its place among the stage lines.
	r := &model.Report{IntentCriteria: make([]model.IntentCriterion, 2), Divergences: []model.Divergence{{}}}
	lines := stdoutLines(r)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "1 recorded behavior divergence") || !strings.HasPrefix(lines[1], "Intent: 2 acceptance criteria") {
		t.Fatalf("stdout lines %q", lines)
	}
}
