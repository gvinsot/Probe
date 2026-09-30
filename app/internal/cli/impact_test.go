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

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
)

// shopFixture is the F6 shop repository: price.Total is called by
// api.Checkout, and cart.Cart.Total may be reached through cart.Totaler from
// notify.Message. The candidate branch changes both bodies.
func shopFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "go.mod", "module example.test/shop\n\ngo 1.23\n")
	write(t, dir, "price/price.go", "package price\n\n// Total sums the items.\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, v := range items {\n\t\tsum += v\n\t}\n\treturn sum\n}\n")
	write(t, dir, "price/price_test.go", "package price\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total([]int{1, 2, 3}) != 6 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n")
	write(t, dir, "api/handler.go", "package api\n\nimport \"example.test/shop/price\"\n\n// Checkout charges the items.\nfunc Checkout(items []int) int {\n\treturn price.Total(items) + 1\n}\n")
	write(t, dir, "api/handler_test.go", "package api\n\nimport \"testing\"\n\nfunc TestCheckout(t *testing.T) {\n\tif Checkout([]int{1}) != 2 {\n\t\tt.Fatal(\"checkout\")\n\t}\n}\n")
	write(t, dir, "cart/cart.go", "package cart\n\n// Cart holds items.\ntype Cart struct{ Items []int }\n\n// Total sums the cart.\nfunc (c *Cart) Total() int {\n\tn := 0\n\tfor _, v := range c.Items {\n\t\tn += v\n\t}\n\treturn n\n}\n\n// Totaler has a total.\ntype Totaler interface{ Total() int }\n")
	write(t, dir, "notify/notify.go", "package notify\n\nimport \"example.test/shop/cart\"\n\n// Message reports a total.\nfunc Message(t cart.Totaler) int {\n\treturn t.Total()\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "price/price.go", "package price\n\n// Total sums the items.\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, v := range items[1:] {\n\t\tsum += v\n\t}\n\treturn sum\n}\n")
	write(t, dir, "cart/cart.go", "package cart\n\n// Cart holds items.\ntype Cart struct{ Items []int }\n\n// Total sums the cart.\nfunc (c *Cart) Total() int {\n\tn := 1\n\tfor _, v := range c.Items {\n\t\tn += v\n\t}\n\treturn n\n}\n\n// Totaler has a total.\ntype Totaler interface{ Total() int }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	return dir
}

func impactSignals(r model.Report) []model.Signal {
	var out []model.Signal
	for _, s := range r.Signals {
		if s.Kind == model.SignalImpactedCaller || s.Kind == model.SignalAnalysisLimited && s.Symbol == "impact_index" {
			out = append(out, s)
		}
	}
	return out
}

func TestLintEmitsImpactedCallersWithoutExecution(t *testing.T) {
	dir := shopFixture(t)
	forbidExecution(t)
	policy := v04Policy(t, absentImage, nil)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--config", policy)
	if code != 0 || len(r.Checks) != 0 {
		t.Fatalf("exit %d, checks %d", code, len(r.Checks))
	}
	if r.Impact == nil || r.Impact.Status != model.ImpactIndexed || r.Impact.IndexedFiles != 6 || len(r.Impact.ChangedFunctions) != 2 || r.Impact.TestsStatus != "" {
		t.Fatalf("impact %+v", r.Impact)
	}
	signals := impactSignals(r)
	if len(signals) != 2 {
		t.Fatalf("impact signals %+v", signals)
	}
	for _, s := range signals {
		if s.Kind != model.SignalImpactedCaller || s.Severity != "low" || s.ID == "" || (s.Path != "api/handler.go" && s.Path != "notify/notify.go") || s.Line != 7 {
			t.Fatalf("signal %+v", s)
		}
	}
	// The low targets land on unchanged code.
	found := false
	for _, target := range r.ReviewTargets {
		if target.Path == "api/handler.go" && target.StartLine == 7 && target.Severity == "low" && strings.Contains(strings.Join(target.Reasons, ";"), "Unchanged caller of a changed Go function") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no review target at the caller: %+v", r.ReviewTargets)
	}
	if !strings.Contains(output, "Impact analysis (static Go index, approximate): 2 changed Go functions; 2 caller sites in unchanged code and 2 reaching tests, counted per function.") {
		t.Fatalf("stdout: %s", output)
	}
}

// --impact=false removes the section and the signals, and the review surface
// does not depend on the index: callers are never on changed lines.
func TestImpactFlagDisablesAndKeepsReviewSurface(t *testing.T) {
	dir := shopFixture(t)
	forbidExecution(t)
	policy := v04Policy(t, absentImage, nil)
	_, on, _, _ := runReport(t, context.Background(), dir, "lint", "--config", policy)
	_, off, members, output := runReport(t, context.Background(), dir, "lint", "--config", policy, "--impact=false")
	if _, ok := members["impact"]; ok || len(impactSignals(off)) != 0 || strings.Contains(output, "Impact analysis") {
		t.Fatalf("--impact=false still analyzed: %+v %s", off.Impact, output)
	}
	if on.ReviewSurface != off.ReviewSurface {
		t.Fatalf("review surface changed: %+v vs %+v", on.ReviewSurface, off.ReviewSurface)
	}
	if len(on.Signals) != len(off.Signals)+2 {
		t.Fatalf("signals %d with impact, %d without", len(on.Signals), len(off.Signals))
	}
}

// A change without Go files builds no index and prints no impact line.
func TestImpactNotApplicableWithoutGoChange(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "README.md", "a\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "README.md", "b\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--config", v04Policy(t, absentImage, nil))
	if code != 0 || r.Impact == nil || r.Impact.Status != model.ImpactNotApplicable || strings.Contains(output, "Impact analysis") {
		t.Fatalf("exit %d impact %+v output %s", code, r.Impact, output)
	}
}

func TestImpactLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *model.Impact
		want string
	}{
		{"absent", nil, ""},
		{"not applicable", &model.Impact{Status: model.ImpactNotApplicable}, ""},
		{"unavailable", &model.Impact{Status: model.ImpactUnavailable, Reason: "no go.mod file in the committed tree"}, "Impact analysis: the static Go index is unavailable (no go.mod file in the committed tree)."},
		{"limited", &model.Impact{Status: model.ImpactLimited, ChangedFunctions: []model.ImpactFunction{{CallersTotal: 3, TestsTotal: 1}}}, "Impact analysis (static Go index, approximate): 1 changed Go functions; 3 caller sites in unchanged code and 1 reaching tests, counted per function. The index is limited; see the report."},
		{"impacted tests", &model.Impact{Status: model.ImpactIndexed, TestsStatus: model.ImpactTestsRan, ChangedFunctions: []model.ImpactFunction{
			{Tests: []model.ImpactTest{{Name: "TestA", Path: "a_test.go", Status: model.StatusFailsOnCandidate}, {Name: "TestB", Path: "a_test.go", Status: model.StatusPassesOnCandidate}}},
			{Tests: []model.ImpactTest{{Name: "TestA", Path: "a_test.go", Status: model.StatusFailsOnCandidate}, {Name: "TestC", Path: "a_test.go"}}},
		}}, "Impact analysis (static Go index, approximate): 2 changed Go functions; 0 caller sites in unchanged code and 0 reaching tests, counted per function. Impacted tests: ran; 1 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED."},
		{"impacted tests not run", &model.Impact{Status: model.ImpactUnavailable, Reason: "r", TestsStatus: model.ImpactTestsNotRun}, "Impact analysis: the static Go index is unavailable (r). Impacted tests: not_run."},
	} {
		if got := impactLine(tc.in); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A scripted reviewer's find_callers is answered by the static index, and the
// call is audited; lint never contacts the provider.
func TestReviewerFindCallersUsesIndex(t *testing.T) {
	dir := shopFixture(t)
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"find_callers","arguments":"{\"symbol\":\"price.Total\",\"depth\":2}"}},{"id":"c2","type":"function","function":{"name":"inspect_symbol","arguments":"{\"symbol\":\"cart.Totaler.Total\"}"}},{"id":"c3","type":"function","function":{"name":"find_references","arguments":"{\"symbol\":\"NotAFunction\"}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Done"}}]}`))
	}))
	defer server.Close()
	cfg := config.Default("go")
	cfg.Reviewer.Model = "test-model"
	cfg.Reviewer.Endpoint = server.URL + "/v1"
	cfg.Reviewer.APIKeyEnv = "PROBE_TEST_REVIEWER_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "test-reviewer-key")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)

	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"lint", "--repo", dir, "--config", policy, "--out", "report"}, &out, &errOut, "test"); code != 0 {
		t.Fatalf("lint exit %d: %s", code, errOut.String())
	}
	if len(requests) != 0 {
		t.Fatal("lint contacted the provider")
	}
	if code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--out", "report"}, &out, &errOut, "test"); code != 0 {
		t.Fatalf("review exit %d: %s", code, errOut.String())
	}
	if len(requests) != 4 { // two reminders of the unassessed signals follow the answer
		t.Fatalf("%d provider requests", len(requests))
	}
	tools := map[string]string{}
	for _, raw := range requests[1]["messages"].([]any) {
		m := raw.(map[string]any)
		if m["role"] == "tool" {
			tools[m["tool_call_id"].(string)] = m["content"].(string)
		}
	}
	var callers map[string]any
	if err := json.Unmarshal([]byte(tools["c1"]), &callers); err != nil {
		t.Fatalf("find_callers result %q: %v", tools["c1"], err)
	}
	listed, _ := json.Marshal(callers["callers"])
	if callers["method"] != "go_static_index" || callers["depth"] != float64(2) || !strings.Contains(string(listed), `"path":"api/handler.go"`) || !strings.Contains(string(listed), `"caller":"example.test/shop/api.TestCheckout"`) {
		t.Fatalf("find_callers result %s", tools["c1"])
	}
	if !strings.Contains(tools["c2"], `"implementations"`) || !strings.Contains(tools["c2"], "example.test/shop/cart.Cart.Total") {
		t.Fatalf("inspect_symbol result %s", tools["c2"])
	}
	if !strings.Contains(tools["c3"], "lexical") || !strings.Contains(tools["c3"], "static index") {
		t.Fatalf("fallback result %s", tools["c3"])
	}
	r := readReviewerReport(t, dir)
	audited := map[string]string{}
	for _, e := range r.Audit {
		audited[e.Tool] = e.Status
	}
	if audited["find_callers"] != "OK" || audited["inspect_symbol"] != "OK" || audited["find_references"] != "OK" || len(r.Evidence) != 0 || len(r.Checks) != 0 {
		t.Fatalf("audit %+v evidence %+v", r.Audit, r.Evidence)
	}
	if _, err := os.Stat(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md")); err != nil {
		t.Fatal(err)
	}
}
