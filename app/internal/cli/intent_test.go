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
	if !strings.Contains(output, "Intent: 2 acceptance criteria extracted; 0 intent-test failures.") {
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
		want               string
	}{
		{0, 0, ""},
		{1, 0, "Intent: 1 acceptance criterion extracted; 0 intent-test failures."},
		{3, 1, "Intent: 3 acceptance criteria extracted; 1 intent-test failure (a model-written test failed on the candidate; no baseline control; weaker than a reproduced issue)."},
		{3, 2, "Intent: 3 acceptance criteria extracted; 2 intent-test failures (model-written tests failed on the candidate; no baseline control; weaker than a reproduced issue)."},
	} {
		r := &model.Report{IntentCriteria: make([]model.IntentCriterion, tc.criteria), IntentTestFailures: make([]model.Hypothesis, tc.failures)}
		if got := intentLine(r); got != tc.want {
			t.Errorf("%d/%d: %q", tc.criteria, tc.failures, got)
		}
	}
	// The line keeps its place among the stage lines.
	r := &model.Report{IntentCriteria: make([]model.IntentCriterion, 2), Divergences: []model.Divergence{{}}}
	lines := stdoutLines(r)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "1 recorded behavior divergence") || !strings.HasPrefix(lines[1], "Intent: 2 acceptance criteria") {
		t.Fatalf("stdout lines %q", lines)
	}
}
