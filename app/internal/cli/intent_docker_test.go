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
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// intentStep returns the next tool call from the previous tool result, or
// an empty name to stop.
type intentStep func(t *testing.T, previous map[string]any) (string, any)

// intentProvider is a scripted Chat Completions endpoint that plays steps in
// order, one tool call per completion, reading test and evidence IDs from the
// previous tool result.
func intentProvider(t *testing.T, steps []intentStep) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var request struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		previous := map[string]any{}
		if n := len(request.Messages); n > 0 && request.Messages[n-1].Role == "tool" {
			_ = json.Unmarshal([]byte(request.Messages[n-1].Content), &previous)
		}
		calls++
		if calls > len(steps) {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Investigation completed."}}}})
			return
		}
		name, args := steps[calls-1](t, previous)
		arguments, _ := json.Marshal(args)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call-%d", calls), "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}}}})
	}))
	t.Cleanup(server.Close)
	return server, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

func previousField(t *testing.T, previous map[string]any, path ...string) string {
	t.Helper()
	var value any = previous
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			t.Errorf("tool result %v has no %v", previous, path)
			return ""
		}
		value = m[key]
	}
	s, _ := value.(string)
	if s == "" {
		t.Errorf("tool result %v has no %v", previous, path)
	}
	return s
}

// intentScript creates and runs an intent test for AC-1 that fails, cites it,
// creates and runs one for AC-2 that passes, cites the pass as a failure
// (refused by Finalize), and submits a DIVERGED claim with a model judgment
// and an invented evidence ID. path is the changed file the claims name.
func intentScript(failing, passing map[string]any, path string) []intentStep {
	return []intentStep{
		func(*testing.T, map[string]any) (string, any) { return "create_intent_test", failing },
		func(t *testing.T, p map[string]any) (string, any) {
			return "run_intent_test", map[string]any{"test_id": previousField(t, p, "test_id")}
		},
		func(t *testing.T, p map[string]any) (string, any) {
			return "submit_hypothesis", map[string]any{"title": "Orders of exactly 100 get no discount", "severity": "high", "status": "INTENT_TEST_FAILED", "rationale": "The intent test for AC-1 failed on the candidate.", "evidence_ids": []string{previousField(t, p, "evidence", "id")}, "path": path, "line": 5, "criterion_id": "AC-1"}
		},
		func(*testing.T, map[string]any) (string, any) { return "create_intent_test", passing },
		func(t *testing.T, p map[string]any) (string, any) {
			return "run_intent_test", map[string]any{"test_id": previousField(t, p, "test_id")}
		},
		func(t *testing.T, p map[string]any) (string, any) {
			return "submit_hypothesis", map[string]any{"title": "Free shipping", "severity": "medium", "status": "INTENT_TEST_FAILED", "rationale": "Cites a passing intent test.", "evidence_ids": []string{previousField(t, p, "evidence", "id")}, "path": path, "line": 1, "criterion_id": "AC-2"}
		},
		func(*testing.T, map[string]any) (string, any) {
			return "submit_hypothesis", map[string]any{"title": "Discount changed as asked", "severity": "low", "status": "DIVERGED", "rationale": "Invented.", "evidence_ids": []string{"evidence-99"}, "path": path, "line": 5, "criterion_id": "AC-1", "intent_judgment": "expected_change"}
		},
	}
}

const shopIntent = "Make discounts predictable.\n\n## Acceptance criteria\n- [ ] Orders of 100 or more get 10 off\n- [ ] Orders of 50 or more ship free\n\n## Notes\n- not a criterion\n"

// runIntentReview runs review with the scripted provider on dir and returns
// the exit code and the report.
func runIntentReview(t *testing.T, dir, image string, template []string, steps []intentStep, ci bool) (int, model.Report, string) {
	t.Helper()
	server, calls := intentProvider(t, steps)
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.TimeoutSeconds = 300
	cfg.Commands["generated_test"] = template
	cfg.Reviewer.Endpoint = server.URL + "/v1"
	cfg.Reviewer.Model = "scripted-intent"
	cfg.Reviewer.APIKeyEnv = "SWIFTPROOF_INTENT_TEST_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	intentFile := writeIntent(t, shopIntent)
	out := filepath.Join(t.TempDir(), "report")
	args := []string{"review", "--repo", dir, "--base", "main", "--config", policy, "--intent-file", intentFile, "--checks=false", "--out", out}
	if ci {
		args = append(args, "--ci")
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr, "intent-integration")
	if calls() != len(steps)+1 {
		t.Fatalf("provider received %d calls, want %d\n%s", calls(), len(steps)+1, stderr.String())
	}
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Artifacts {
		if a.Kind == model.ArtifactIntentTest {
			data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(a.Path)))
			sum := sha256.Sum256(data)
			if err != nil || hex.EncodeToString(sum[:]) != a.SHA256 {
				t.Fatalf("intent_test artifact %s: %v", a.Path, err)
			}
		}
	}
	return code, r, stdout.String() + "\n" + string(md)
}

// assertIntentReport checks the evidence chain of intentScript's run.
func assertIntentReport(t *testing.T, dir string, r model.Report, output, runner string) {
	t.Helper()
	if len(r.IntentCriteria) != 2 || r.IntentCriteria[0].Text != "Orders of 100 or more get 10 off" || r.IntentCriteria[1].Text != "Orders of 50 or more ship free" {
		t.Fatalf("criteria %+v", r.IntentCriteria)
	}
	sum := sha256.Sum256([]byte(shopIntent))
	if r.IntentSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("intent sha %s", r.IntentSHA256)
	}
	if len(r.Checks) != 2 || r.Checks[0].Kind != model.CheckGeneratedIntent || r.Checks[1].Kind != model.CheckGeneratedIntent || r.Checks[0].Status != "FAIL" || r.Checks[1].Status != "PASS" {
		t.Fatalf("checks %+v", r.Checks)
	}
	if len(r.Evidence) != 2 || r.Evidence[0].Status != model.StatusIntentTestFailed || r.Evidence[1].Status != model.StatusIntentTestPassed {
		t.Fatalf("evidence %+v", r.Evidence)
	}
	for _, e := range r.Evidence {
		if e.Kind != model.EvidenceIntentTest || e.BaseCheckID != "" || e.Runner != runner || len(e.TestNames) != 1 {
			t.Fatalf("evidence %+v", e)
		}
	}
	if len(r.IntentTestFailures) != 1 || r.IntentTestFailures[0].CriterionID != "AC-1" || len(r.ReproducedIssues) != 0 {
		t.Fatalf("failures %+v reproduced %+v", r.IntentTestFailures, r.ReproducedIssues)
	}
	statuses := []string{}
	for _, h := range r.Hypotheses {
		statuses = append(statuses, h.Status)
	}
	if strings.Join(statuses, ",") != "INTENT_TEST_FAILED,UNVERIFIED,UNVERIFIED" || r.Hypotheses[2].IntentJudgment != "" || r.Hypotheses[2].CriterionID != "AC-1" {
		t.Fatalf("hypotheses %+v", r.Hypotheses)
	}
	if !hasEntry(r.Unverified, "The intent link of hypothesis hypothesis-3 was discarded: an intent judgment is kept only on a DIVERGED hypothesis with a known criterion, and a criterion only when the intent defines it.") {
		t.Fatalf("unverified %q", r.Unverified)
	}
	retained := 0
	for _, a := range r.Artifacts {
		if a.Kind == model.ArtifactIntentTest {
			retained++
		}
	}
	if retained != 1 {
		t.Fatalf("%d intent_test artifacts", retained)
	}
	for _, want := range []string{"Intent: 2 acceptance criteria extracted; 1 intent-test failure", "## Intent Test Failures", "## Intent Criteria", "ran without failing", "says nothing about whether the criterion holds", "Candidate check: check-1 (candidate-only; no baseline control)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if i, j := strings.Index(output, "## Reproduced Issues"), strings.Index(output, "## Intent Test Failures"); i < 0 || j < i {
		t.Fatal("section order")
	}
	if strings.Contains(output, "not a criterion\" —") {
		t.Fatal("an item outside the criteria section was extracted")
	}
	if status := strings.TrimSpace(git(t, dir, "status", "--porcelain")); status != "" {
		t.Fatalf("checkout changed: %s", status)
	}
}

func shopRepo(t *testing.T, files, candidate map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	for name, data := range files {
		write(t, dir, name, data)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	for name, data := range candidate {
		write(t, dir, name, data)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

// A real review of a Go change: the intent test for AC-1 fails on an assertion
// and becomes an INTENT_TEST_FAILED hypothesis, which requests review (exit 2
// with --ci, 0 without, never 1); the passing AC-2 test supports nothing; the
// judgment on an unsupported claim is dropped with a note.
func TestDockerIntentEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	base := "package shop\n\nfunc Total(xs []int) int {\n\tsum := 0\n\tfor _, x := range xs {\n\t\tsum += x\n\t}\n\treturn sum\n}\n"
	candidate := base + "\nfunc Discount(total int) int {\n\tif total > 100 {\n\t\treturn total - 10\n\t}\n\treturn total\n}\n\nfunc FreeShipping(total int) bool { return total >= 50 }\n"
	dir := shopRepo(t, map[string]string{"go.mod": "module example.test/shop\n\ngo 1.23\n", "shop.go": base}, map[string]string{"shop.go": candidate})
	failing := map[string]any{"criterion_id": "AC-1", "path": "swiftproof_intent_ac1_test.go", "description": "Orders of exactly 100 get 10 off",
		"content": "package shop\n\nimport \"testing\"\n\nfunc TestSwiftProofIntentDiscountAt100(t *testing.T) {\n\tif got := Discount(100); got != 90 {\n\t\tt.Errorf(\"Discount(100) = %d, want 90\", got)\n\t}\n}\n"}
	passing := map[string]any{"criterion_id": "AC-2", "path": "swiftproof_intent_ac2_test.go", "description": "Orders of 50 ship free",
		"content": "package shop\n\nimport \"testing\"\n\nfunc TestSwiftProofIntentFreeShippingAt50(t *testing.T) {\n\tif !FreeShipping(50) {\n\t\tt.Error(\"FreeShipping(50) = false\")\n\t}\n}\n"}
	steps := intentScript(failing, passing, "shop.go")
	code, r, output := runIntentReview(t, dir, image, []string{"go", "test", "{package}"}, steps, true)
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, output)
	}
	assertIntentReport(t, dir, r, output, "go_test_json")
	if !strings.Contains(output, "failed on an assertion; names it shares with declarations the change added or modified (matched by name, not resolved): Discount.") {
		t.Fatalf("referenced symbols not rendered:\n%s", output)
	}
	// Without --ci the same run requests nothing through the exit code: 0.
	code, r, output = runIntentReview(t, dir, image, []string{"go", "test", "{package}"}, intentScript(failing, passing, "shop.go"), false)
	if code != 0 || r.ExitCode != 0 {
		t.Fatalf("without --ci: exit %d (report %d), want 0\n%s", code, r.ExitCode, output)
	}
	assertIntentReport(t, dir, r, output, "go_test_json")
}

// The same review of a TypeScript change with a Vitest template (image
// SWIFTPROOF_TEST_TS_IMAGE): the failure is read from the Jest-compatible
// report and the referenced symbols are matched lexically.
func TestDockerTSIntentEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_TS_IMAGE to a preloaded image with vitest on PATH (for example swiftproof-ts-test:local)")
	}
	base := "export function total(xs: number[]): number {\n  return xs.reduce((a, b) => a + b, 0);\n}\n"
	candidate := base + "\nexport function discount(total: number): number {\n  return total > 100 ? total - 10 : total;\n}\n\nexport function freeShipping(total: number): boolean {\n  return total >= 50;\n}\n"
	dir := shopRepo(t, map[string]string{"src/cart.ts": base}, map[string]string{"src/cart.ts": candidate})
	failing := map[string]any{"criterion_id": "AC-1", "path": "src/cart.intent1.test.ts", "description": "Orders of exactly 100 get 10 off",
		"content": "import { expect, test } from \"vitest\";\nimport { discount } from \"./cart\";\n\ntest(\"orders of 100 get 10 off\", () => {\n  expect(discount(100)).toBe(90);\n});\n"}
	passing := map[string]any{"criterion_id": "AC-2", "path": "src/cart.intent2.test.ts", "description": "Orders of 50 ship free",
		"content": "import { expect, test } from \"vitest\";\nimport { freeShipping } from \"./cart\";\n\ntest(\"orders of 50 ship free\", () => {\n  expect(freeShipping(50)).toBe(true);\n});\n"}
	steps := intentScript(failing, passing, "src/cart.ts")
	code, r, output := runIntentReview(t, dir, image, []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"}, steps, true)
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, output)
	}
	assertIntentReport(t, dir, r, output, "jest_json")
	if !strings.Contains(r.Evidence[0].Description, "matched lexically") {
		t.Fatalf("description %q", r.Evidence[0].Description)
	}
	if !strings.Contains(output, "(matched by name, not resolved; read lexically for JavaScript/TypeScript): discount.") {
		t.Fatalf("lexical label not rendered:\n%s", output)
	}
}
