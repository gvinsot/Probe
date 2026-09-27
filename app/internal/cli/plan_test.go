package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

// planFixture is a Go repository whose exported calc.Discount is called by
// shop.Total, which TestTotal exercises.
func planFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/store\n\ngo 1.23\n")
	write(t, dir, "calc/calc.go", "package calc\n\n// Discount takes 10 off large totals.\nfunc Discount(total int) int {\n\tif total > 100 {\n\t\treturn total - 10\n\t}\n\treturn total\n}\n")
	write(t, dir, "shop/shop.go", "package shop\n\nimport \"example.test/store/calc\"\n\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, i := range items {\n\t\tsum += i\n\t}\n\treturn calc.Discount(sum)\n}\n")
	write(t, dir, "shop/shop_test.go", "package shop\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total([]int{50, 60}) != 100 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n")
	write(t, dir, "internal/auth/token.go", "package auth\n\nfunc Valid(token string) bool { return token != \"\" }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	return dir
}

// fakePlanner answers like a Chat Completions provider: it reads a file,
// asks for callers, then submits the plan. It records every request.
type fakePlanner struct {
	mu       sync.Mutex
	requests []map[string]any
	plan     string
}

func (f *fakePlanner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request map[string]any
	_ = json.NewDecoder(r.Body).Decode(&request)
	f.mu.Lock()
	f.requests = append(f.requests, request)
	n := len(f.requests)
	f.mu.Unlock()
	call := func(id, name, arguments string) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}}}}}})
		w.Write(b)
	}
	switch n {
	case 1:
		call("read", "read_file", `{"path":"calc/calc.go"}`)
	case 2:
		call("callers", "find_callers", `{"symbol":"Discount","depth":2}`)
	case 3:
		call("write", "create_test", `{"path":"x_test.go","content":"package x"}`)
	default:
		call("submit", "submit_plan", f.plan)
	}
}

const announcedPlan = `{"summary":"Let Discount take a rate.","steps":["Add a rate parameter to Discount","Test it"],
"files":[{"path":"calc/calc.go","change":"modify","reason":"new parameter"},{"path":"calc/calc_test.go","change":"add","reason":"tests"}],
"symbols":[{"path":"calc/calc.go","name":"Discount","change":"signature","reason":"rate parameter"}],
"dependencies":[],"assumptions":["callers keep the default rate"]}`

func planPolicy(t *testing.T, endpoint string) string {
	t.Helper()
	cfg := config.Default("go")
	cfg.Reviewer.Model = "planner-model"
	cfg.Reviewer.Endpoint = endpoint + "/v1"
	cfg.Reviewer.APIKeyEnv = "SWIFTPROOF_TEST_PLAN_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "test-plan-key")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	return policy
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, &out, &errOut, "test")
	return code, out.String(), errOut.String()
}

func TestPlanThenReviewAgainstThePlan(t *testing.T) {
	dir := planFixture(t)
	planner := &fakePlanner{plan: announcedPlan}
	server := httptest.NewServer(planner)
	defer server.Close()
	policy := planPolicy(t, server.URL)
	intent := filepath.Join(t.TempDir(), "demande.md")
	write(t, filepath.Dir(intent), "demande.md", "Let callers choose the discount rate.\n\n- [ ] Discount takes a rate\n")

	code, stdout, stderr := runCLI(t, "plan", "--repo", dir, "--config", policy, "--intent-file", intent, "--out", "out", "--ci")
	if code != 2 {
		t.Fatalf("plan exited %d, want 2 (an exported signature change is flagged)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Flagged categories: architecture") {
		t.Errorf("stdout does not name the flagged category: %s", stdout)
	}
	// The planner saw only read tools and submit_plan, and a write attempt was refused.
	first := planner.requests[0]
	var names []string
	for _, tool := range first["tools"].([]any) {
		names = append(names, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	for _, forbidden := range []string{"create_test", "run_tests", "run_generated_test", "get_diff"} {
		if strings.Contains(strings.Join(names, ","), forbidden) {
			t.Errorf("the planner was offered %s: %v", forbidden, names)
		}
	}
	messages := planner.requests[1]["messages"].([]any)
	if last := messages[len(messages)-1].(map[string]any)["content"].(string); !strings.Contains(last, "func Discount(total int) int") {
		t.Errorf("read_file did not return the base source: %s", last)
	}
	messages = planner.requests[3]["messages"].([]any)
	if last := messages[len(messages)-1].(map[string]any)["content"].(string); !strings.Contains(last, "tool is not available") {
		t.Errorf("a write tool must be refused: %s", last)
	}

	var plan model.Plan
	data, err := os.ReadFile(filepath.Join(dir, "out", "PLAN.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Format != model.PlanFormat || plan.Model != "planner-model" || plan.ExitCode != 2 || !plan.Assessment.Major {
		t.Fatalf("plan header = %+v", plan)
	}
	if len(plan.Assessment.Symbols) != 1 {
		t.Fatalf("symbols = %+v", plan.Assessment.Symbols)
	}
	s := plan.Assessment.Symbols[0]
	if !s.Found || !s.Exported || s.CallersTotal != 1 || s.Callers[0].Path != "shop/shop.go" || s.TestsTotal != 1 || s.Tests[0].Name != "TestTotal" {
		t.Errorf("Discount measure = %+v", s)
	}
	kinds := map[string]bool{}
	for _, sig := range plan.Assessment.Signals {
		kinds[sig.Kind] = true
	}
	if !kinds[model.PlanSignalExportedSignature] || kinds[model.PlanSignalCriticalPath] {
		t.Errorf("plan signals = %+v", plan.Assessment.Signals)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "out", "PLAN.md"))
	for _, want := range []string{"# Change Plan Assessment", "| Architecture | **yes** |", "model-written, not evidence", "review --plan"} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("PLAN.md lacks %q", want)
		}
	}

	// The implementation announces Discount's new signature, but also edits
	// the caller and an auth file the plan never mentioned.
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "calc/calc.go", "package calc\n\n// Discount takes rate off large totals.\nfunc Discount(total, rate int) int {\n\tif total > 100 {\n\t\treturn total - rate\n\t}\n\treturn total\n}\n")
	write(t, dir, "calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif Discount(200, 20) != 180 {\n\t\tt.Fatal(\"rate\")\n\t}\n}\n")
	write(t, dir, "shop/shop.go", "package shop\n\nimport \"example.test/store/calc\"\n\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, i := range items {\n\t\tsum += i\n\t}\n\treturn calc.Discount(sum, 10)\n}\n")
	write(t, dir, "internal/auth/token.go", "package auth\n\nfunc Valid(token string) bool { return true }\n")
	git(t, dir, "add", "calc", "shop", "internal")
	git(t, dir, "commit", "-m", "implementation")

	code, stdout, stderr = runCLI(t, "lint", "--repo", dir, "--config", policy, "--head", "candidate", "--plan", filepath.Join(dir, "out", "PLAN.json"), "--out", "report", "--ci")
	if code != 2 {
		t.Fatalf("lint --plan exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Plan conformance: drifted") || !strings.Contains(stdout, "Plan gate: human review required") {
		t.Errorf("stdout lacks the conformance and gate lines: %s", stdout)
	}
	r := readReport(t, filepath.Join(dir, "report", "confidence-report.json"))
	if a := r.PlanDrift.Assessment; a.Status != model.PlanAssessed || !a.Major || strings.Join(a.FlaggedCategories, ",") != model.PlanCategoryArchitecture {
		t.Errorf("review-time assessment = %+v", a)
	}
	if r.PlanDrift.Decision != model.PlanDecisionReviewRequired || !strings.Contains(strings.Join(r.PlanDrift.DecisionReasons, "|"), "the plan raised risk categories: architecture") {
		t.Errorf("gate = %s %v", r.PlanDrift.Decision, r.PlanDrift.DecisionReasons)
	}
	if r.PlanDrift == nil || r.PlanDrift.Status != model.PlanDriftDrifted || !r.PlanDrift.BaseMatches {
		t.Fatalf("plan_drift = %+v", r.PlanDrift)
	}
	items := map[string]string{}
	for _, it := range r.PlanDrift.Items {
		items[it.Kind+" "+it.Path] = it.Severity
	}
	for key, severity := range map[string]string{
		"unplanned_file shop/shop.go":                      "medium",
		"unplanned_file internal/auth/token.go":            "medium",
		"unannounced_critical_path internal/auth/token.go": "high",
	} {
		if items[key] != severity {
			t.Errorf("drift %q = %q, want %q (all: %v)", key, items[key], severity, items)
		}
	}
	for key := range items {
		if strings.HasPrefix(key, model.DriftUnannouncedExported) {
			t.Errorf("the announced signature change was reported as drift: %v", items)
		}
	}
	driftSignals := 0
	for _, sig := range r.Signals {
		if sig.Kind == model.SignalPlanDrift {
			driftSignals++
		}
	}
	if driftSignals != 3 {
		t.Errorf("plan_drift signals = %d, want 3: %+v", driftSignals, r.PlanDrift.Items)
	}
	md, _ = os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if !bytes.Contains(md, []byte("## Plan Conformance")) || !bytes.Contains(md, []byte("Status: **drifted**")) {
		t.Errorf("the report lacks the Plan Conformance section:\n%s", md)
	}

	// A forged section is recomputed on re-render.
	var raw map[string]any
	data, _ = os.ReadFile(filepath.Join(dir, "report", "confidence-report.json"))
	_ = json.Unmarshal(data, &raw)
	drift := raw["plan_drift"].(map[string]any)
	drift["status"], drift["items"] = "conforming", []any{}
	forged, _ := json.Marshal(raw)
	input := filepath.Join(t.TempDir(), "forged.json")
	if err := os.WriteFile(input, forged, 0600); err != nil {
		t.Fatal(err)
	}
	rendered := t.TempDir()
	if code, _, stderr := runCLI(t, "report", "--input", input, "--out", rendered); code != 0 {
		t.Fatalf("report exited %d: %s", code, stderr)
	}
	again := readReport(t, filepath.Join(rendered, "confidence-report.json"))
	if again.PlanDrift.Status != model.PlanDriftDrifted || len(again.PlanDrift.Items) != len(r.PlanDrift.Items) {
		t.Errorf("a forged conforming section survived re-rendering: %+v", again.PlanDrift)
	}
}

func TestPlanRequiresAProviderAndAnIntent(t *testing.T) {
	dir := planFixture(t)
	intent := filepath.Join(t.TempDir(), "intent.md")
	write(t, filepath.Dir(intent), "intent.md", "Do something.\n")
	noModel := filepath.Join(t.TempDir(), "policy.json")
	cfg := config.Default("go")
	cfg.Reviewer.APIKeyEnv = ""
	writeReviewerPolicy(t, noModel, cfg)
	for name, args := range map[string][]string{
		"no provider":       {"plan", "--repo", dir, "--config", noModel, "--intent-file", intent},
		"reviewer=false":    {"plan", "--repo", dir, "--config", noModel, "--intent-file", intent, "--reviewer=false"},
		"no intent":         {"plan", "--repo", dir, "--config", noModel},
		"both intents":      {"plan", "--repo", dir, "--config", noModel, "--intent-file", intent, "--intent", "x"},
		"positional":        {"plan", "--repo", dir, "--config", noModel, "--intent-file", intent, "main..HEAD"},
		"bad plan file":     {"lint", "--repo", dir, "--config", noModel, "--plan", intent},
		"missing plan file": {"lint", "--repo", dir, "--config", noModel, "--plan", filepath.Join(dir, "absent.json")},
	} {
		if code, _, stderr := runCLI(t, args...); code != 3 {
			t.Errorf("%s: exit %d, want 3 (%s)", name, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".swiftproof", "PLAN.json")); !os.IsNotExist(err) {
		t.Error("a refused plan must write nothing")
	}
}

func TestPlanWithoutSubmittedPlanIsOperational(t *testing.T) {
	dir := planFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"The change looks safe."}}]}`))
	}))
	defer server.Close()
	policy := planPolicy(t, server.URL)
	code, _, stderr := runCLI(t, "plan", "--repo", dir, "--config", policy, "--intent", "Add a rate")
	if code != 4 || !strings.Contains(stderr, "no valid plan") {
		t.Errorf("a model that submits no plan: exit %d, %s", code, stderr)
	}
}

// lowRiskPlan changes the body of Discount, which one caller uses and one
// test reaches: no category is flagged.
const lowRiskPlan = `{"summary":"Take 11 off large totals.","steps":["Change the constant"],
"files":[{"path":"calc/calc.go","change":"modify"}],
"symbols":[{"path":"calc/calc.go","name":"Discount","change":"body"}],
"dependencies":[],"assumptions":[]}`

func TestPlanGateOfTheProcess(t *testing.T) {
	dir := planFixture(t)
	server := httptest.NewServer(&fakePlanner{plan: lowRiskPlan})
	defer server.Close()
	policy := planPolicy(t, server.URL)
	intent := filepath.Join(t.TempDir(), "task.md")
	write(t, filepath.Dir(intent), "task.md", "Take 11 off large totals.\n")

	// 1. Plan: nothing is flagged.
	if code, stdout, stderr := runCLI(t, "plan", "--repo", dir, "--config", policy, "--intent-file", intent, "--out", "out"); code != 0 || !strings.Contains(stdout, "Flagged categories: none") {
		t.Fatalf("plan exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	planPath := filepath.Join(dir, "out", "PLAN.json")

	// 2. An agent implements the plan, and only the plan.
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "calc/calc.go", "package calc\n\n// Discount takes 11 off large totals.\nfunc Discount(total int) int {\n\tif total > 100 {\n\t\treturn total - 11\n\t}\n\treturn total\n}\n")
	git(t, dir, "commit", "-am", "implementation")

	// 3. Conformance: the change conforms to a low-risk plan. lint runs no
	// check, so that is the one reason left for a human; review with passing
	// checks would clear it.
	code, stdout, stderr := runCLI(t, "lint", "--repo", dir, "--config", policy, "--head", "candidate", "--plan", planPath, "--out", "report", "--ci")
	if code != 2 {
		t.Fatalf("lint --plan exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	r := readReport(t, filepath.Join(dir, "report", "confidence-report.json"))
	d := r.PlanDrift
	if d.Status != model.PlanDriftConforming || d.Assessment.Status != model.PlanAssessed || d.Assessment.Major || len(d.Assessment.Gaps) != 0 {
		t.Fatalf("plan_drift = %+v", d)
	}
	if d.Decision != model.PlanDecisionReviewRequired || len(d.DecisionReasons) != 1 || !strings.HasPrefix(d.DecisionReasons[0], "no check ran") {
		t.Errorf("gate = %s %q, want only the missing checks", d.Decision, d.DecisionReasons)
	}

	// A PLAN.json whose contract and assessment were edited after planning
	// cannot widen the scope: the contract is re-derived from the proposal.
	var raw map[string]any
	data, _ := os.ReadFile(planPath)
	_ = json.Unmarshal(data, &raw)
	contract := raw["contract"].(map[string]any)
	contract["files"] = []any{"calc/calc.go", "internal/auth/token.go"}
	contract["critical_files"] = []any{"internal/auth/token.go"}
	raw["assessment"].(map[string]any)["major"] = false
	forged, _ := json.Marshal(raw)
	forgedPath := filepath.Join(t.TempDir(), "PLAN.json")
	if err := os.WriteFile(forgedPath, forged, 0600); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "internal/auth/token.go", "package auth\n\nfunc Valid(token string) bool { return true }\n")
	git(t, dir, "commit", "-am", "widen")
	if code, stdout, stderr = runCLI(t, "lint", "--repo", dir, "--config", policy, "--head", "candidate", "--plan", forgedPath, "--out", "forged", "--ci"); code != 2 {
		t.Fatalf("lint --plan with a forged plan exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	r = readReport(t, filepath.Join(dir, "forged", "confidence-report.json"))
	kinds := map[string]bool{}
	for _, it := range r.PlanDrift.Items {
		kinds[it.Kind+" "+it.Path] = true
	}
	if !kinds["unplanned_file internal/auth/token.go"] || !kinds["unannounced_critical_path internal/auth/token.go"] || r.PlanDrift.Status != model.PlanDriftDrifted {
		t.Errorf("the forged contract widened the scope: %+v", r.PlanDrift.Items)
	}
}
