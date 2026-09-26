package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// clampFixture has a baseline Clamp that floors negatives at zero, a candidate
// branch that drops the floor, and a benign branch that only adds a function.
func clampFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/clamp\n\ngo 1.23\n")
	write(t, dir, "clamp.go", "package clamp\n\n// Clamp floors negative values at zero.\nfunc Clamp(n int) int {\n\tif n < 0 {\n\t\treturn 0\n\t}\n\treturn n\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "clamp.go", "package clamp\n\n// Clamp floors negative values at zero.\nfunc Clamp(n int) int {\n\treturn n\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")
	git(t, dir, "checkout", "main")
	git(t, dir, "checkout", "-b", "benign")
	write(t, dir, "double.go", "package clamp\n\n// Double returns twice n.\nfunc Double(n int) int { return 2 * n }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "benign")
	git(t, dir, "checkout", "main")
	return dir
}

// scriptedClampProvider is a stateless scripted reviewer: it chooses its reply
// from the number of tool results in the conversation, so every review gets
// the same generated test and cites the evidence the harness returned.
func scriptedClampProvider(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		tools := 0
		for _, m := range request.Messages {
			if m.Role == "tool" {
				tools++
			}
		}
		var name string
		var args any
		switch tools {
		case 0:
			name = "create_test"
			args = map[string]any{"path": "clamp_swiftproof_test.go", "description": "negative inputs are floored at zero",
				"content": "package clamp\n\nimport \"testing\"\n\nfunc TestSwiftProofClampNegative(t *testing.T) {\n\tif got := Clamp(-1); got != 0 {\n\t\tt.Fatalf(\"Clamp(-1) = %d\", got)\n\t}\n}\n"}
		case 1:
			name = "run_generated_test"
			args = map[string]any{"test_id": "generated-test-1"}
		case 2:
			var observation struct {
				Evidence model.Evidence `json:"evidence"`
			}
			last := request.Messages[len(request.Messages)-1].Content
			if json.Unmarshal([]byte(last), &observation) != nil || observation.Evidence.ID == "" {
				http.Error(w, "missing evidence", 500)
				return
			}
			name = "submit_hypothesis"
			args = map[string]any{"title": "Negative values are no longer floored", "severity": "high", "status": observation.Evidence.Status,
				"rationale": "The generated named test ran on both revisions.", "evidence_ids": []string{observation.Evidence.ID}, "path": "clamp.go", "line": 4}
		default:
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Done."}}}})
			return
		}
		arguments, _ := json.Marshal(args)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant",
			"tool_calls": []any{map[string]any{"id": fmt.Sprintf("call-%d", tools), "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}}}})
	}))
}

type cacheRun struct {
	code   int
	report model.Report
	md     string
	stdout string
}

// End to end with real Git, real Docker and a scripted provider, as the CLI
// runs it: nothing is replayed before two agreeing live runs; the third review
// replays the baseline and runs it again live before recording REPRODUCED; a
// benign head rests NOT_REPRODUCED on the replay and lists it in
// replay_backed; a tampered entry is rejected and replaced by a live run.
func TestDockerExecutionCacheEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	pinned, found, err := dockerutil.InspectImage(context.Background(), nil, image)
	if err != nil || !found {
		t.Fatalf("inspect %s: %v", image, err)
	}
	repo := clampFixture(t)
	server := scriptedClampProvider(t)
	defer server.Close()
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.CPUs = 1
	cfg.Reviewer.Endpoint = server.URL
	cfg.Reviewer.Model = "scripted-cache"
	cfg.Reviewer.APIKeyEnv = "SWIFTPROOF_INTEGRATION_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "")
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	outputs := t.TempDir()
	review := func(n int, head string, ci bool) cacheRun {
		t.Helper()
		out := filepath.Join(outputs, fmt.Sprintf("r%d", n))
		args := []string{"review", "--repo", repo, "--config", policy, "--base", "main", "--head", head, "--checks=false", "--cache-dir", cacheDir, "--out", out}
		if ci {
			args = append(args, "--ci")
		}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, &stdout, &stderr, "integration")
		data, err := os.ReadFile(filepath.Join(out, "confidence-report.json"))
		if err != nil {
			t.Fatalf("review %d wrote no report (exit %d): %v\n%s", n, code, err, stderr.String())
		}
		var r model.Report
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		md, _ := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
		return cacheRun{code, r, string(md), stdout.String()}
	}
	kinds := func(r model.Report) string {
		var out []string
		for _, c := range r.Checks {
			tag := ""
			if c.Cache != nil {
				tag = fmt.Sprintf("[%s %d]", c.Cache.Status, c.Cache.LiveRuns)
			}
			out = append(out, c.Kind+":"+c.Status+tag)
		}
		return strings.Join(out, ",")
	}
	check := func(r model.Report, id string) model.Check {
		for _, c := range r.Checks {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no check %s", id)
		return model.Check{}
	}

	// R1, R2: two live baseline runs agree; nothing is served yet.
	for n := 1; n <= 2; n++ {
		run := review(n, "candidate", true)
		want := fmt.Sprintf("generated_test_base:PASS[stored %d],generated_test_candidate:FAIL", n)
		if run.code != 1 || kinds(run.report) != want || len(run.report.ReproducedIssues) != 1 {
			t.Fatalf("review %d: exit %d checks %s", n, run.code, kinds(run.report))
		}
		c := run.report.Execution.Cache
		if c.Status != model.CacheEnabled || c.ImageID != pinned.ID || c.Hits != 0 || c.Stored != 1 || c.Runtime == "" {
			t.Fatalf("review %d cache %+v", n, c)
		}
		if !strings.Contains(run.stdout, "Execution cache: 0 baseline results replayed (not executed in this run), 1 recorded") {
			t.Fatalf("review %d stdout:\n%s", n, run.stdout)
		}
	}
	// R3: the baseline is replayed, then run again live before REPRODUCED.
	run := review(3, "candidate", true)
	if run.code != 1 || kinds(run.report) != "generated_test_base:PASS[hit 2],generated_test_candidate:FAIL,generated_test_base:PASS[stored 3]" {
		t.Fatalf("review 3: exit %d checks %s", run.code, kinds(run.report))
	}
	e := run.report.Evidence[0]
	if live := check(run.report, e.BaseCheckID); e.Status != model.StatusReproduced || live.Replayed() || live.ID != run.report.Checks[2].ID ||
		!strings.Contains(e.Description, "so the baseline was run again live as "+live.ID) {
		t.Fatalf("review 3 evidence %+v", e)
	}
	if h := run.report.Hypotheses[0]; h.Status != model.StatusReproduced || len(run.report.Execution.ReplayBacked) != 0 {
		t.Fatalf("review 3 hypothesis %s replay_backed %v", h.Status, run.report.Execution.ReplayBacked)
	}
	if !strings.Contains(run.md, "Replayed from the execution cache, not executed in this run") || run.report.Execution.Cache.Hits != 1 {
		t.Fatalf("review 3 Markdown/cache:\n%s", run.md)
	}
	// R4: a benign head with the same baseline experiment: NOT_REPRODUCED rests
	// on the replay (three agreeing live runs), no live re-run, no review request.
	run = review(4, "benign", true)
	if run.code != 0 || kinds(run.report) != "generated_test_base:PASS[hit 3],generated_test_candidate:PASS" {
		t.Fatalf("review 4: exit %d checks %s", run.code, kinds(run.report))
	}
	e = run.report.Evidence[0]
	if e.Status != model.StatusNotReproduced || run.report.Hypotheses[0].Status != model.StatusNotReproduced || strings.Join(run.report.Execution.ReplayBacked, ",") != e.ID {
		t.Fatalf("review 4: evidence %+v replay_backed %v", e, run.report.Execution.ReplayBacked)
	}
	if !strings.Contains(run.md, "Negative conclusions resting on a replayed baseline") {
		t.Fatalf("review 4 Markdown:\n%s", run.md)
	}
	// R5: a tampered entry is rejected and removed; the baseline runs live.
	entries, _ := filepath.Glob(filepath.Join(cacheDir, "v1", "*", "*.json"))
	if len(entries) != 1 {
		t.Fatalf("cache entries %v", entries)
	}
	data, _ := os.ReadFile(entries[0])
	data[len(data)/2] ^= 0x01
	os.WriteFile(entries[0], data, 0600)
	run = review(5, "benign", true)
	if run.code != 0 || kinds(run.report) != "generated_test_base:PASS[stored 1],generated_test_candidate:PASS" {
		t.Fatalf("review 5: exit %d checks %s", run.code, kinds(run.report))
	}
	if c := run.report.Execution.Cache; c.Rejected != 1 || c.Hits != 0 || len(run.report.Execution.ReplayBacked) != 0 {
		t.Fatalf("review 5 cache %+v replay_backed %v", c, run.report.Execution.ReplayBacked)
	}
	// The checkout is untouched and no generated file leaked into it.
	if status := git(t, repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("checkout changed:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(repo, "clamp_swiftproof_test.go")); !os.IsNotExist(err) {
		t.Fatal("the generated test leaked into the checkout")
	}
}
