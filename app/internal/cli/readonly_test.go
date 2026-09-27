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
	"sync/atomic"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
)

func TestReadOnlyReviewUsesDeploymentProviderWithoutExecution(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "deployment-model" || r.Header.Get("Authorization") != "Bearer deployment-key" {
			t.Error("repository controlled the provider")
		}
		if calls.Add(1) == 1 {
			w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"auth.go\"}"}},{"id":"run","type":"function","function":{"name":"run_tests","arguments":"{}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Done"}}]}`))
	}))
	defer server.Close()
	t.Setenv(config.EndpointEnv, server.URL)
	t.Setenv(config.ModelEnv, "deployment-model")
	t.Setenv("SWIFTPROOF_API_KEY", "deployment-key")
	t.Setenv("SWIFTPROOF_UNRELATED_SECRET", "must-not-be-sent")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	cfg := config.Default("go")
	cfg.Reviewer.Endpoint, cfg.Reviewer.Model, cfg.Reviewer.APIKeyEnv = "https://repository.invalid", "repository-model", "SWIFTPROOF_UNRELATED_SECRET"
	cfg.Reviewer.MaxIterations = 1 // Must not limit the deployment's investigation.
	cfg.Prepare = &config.Prepare{Command: []string{"sh", "-c", "exit 99"}, Inputs: []string{"go.mod"}, Network: true}
	cfg.Fuzz = &config.Fuzz{}
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	var output bytes.Buffer
	code := Run(context.Background(), []string{"review", "--read-only", "--repo", dir, "--config", policy, "--out", "report", "--ci"}, &output, &output, "test")
	if code != 2 || calls.Load() != 2 {
		t.Fatalf("exit=%d calls=%d: %s", code, calls.Load(), output.String())
	}
	r := readReviewerReport(t, dir)
	if r.AnalysisMode != "review-read-only" || len(r.Checks) != 0 || len(r.ReproducedIssues) != 0 || len(r.Evidence) == 0 {
		t.Fatalf("unexpected read-only report: %+v", r)
	}
	for _, e := range r.Evidence {
		if e.Kind != "source_observation" {
			t.Errorf("non-source evidence: %+v", e)
		}
	}
	md, err := os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if err != nil || !strings.Contains(string(md), "No repository code or tests were executed") {
		t.Fatalf("missing scope notice: %v\n%s", err, md)
	}
	// A model named in the repository must not enable requests without a
	// deployment model, even when the endpoint environment is still present.
	t.Setenv(config.ModelEnv, "")
	output.Reset()
	code = Run(context.Background(), []string{"review", "--read-only", "--repo", dir, "--config", policy}, &output, &output, "test")
	if code != 3 || calls.Load() != 2 {
		t.Fatalf("policy enabled the provider: exit=%d calls=%d %s", code, calls.Load(), output.String())
	}
}

func TestReadOnlyReviewerFailureIsIncomplete(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	t.Setenv(config.EndpointEnv, server.URL)
	t.Setenv(config.ModelEnv, "deployment-model")
	var output bytes.Buffer
	// Without a single completion no AI review happened: that is exit 4, not
	// a human-review verdict a caller would read as a finished analysis.
	code := Run(context.Background(), []string{"review", "--read-only", "--repo", dir, "--out", "report", "--ci"}, &output, &output, "test")
	if code != 4 || !strings.Contains(output.String(), "Read-only reviewer incomplete: reviewer endpoint returned HTTP 502") {
		t.Fatalf("exit=%d: %s", code, output.String())
	}
	if r := readReviewerReport(t, dir); r.ExitCode != 4 {
		t.Fatalf("report exit code %d", r.ExitCode)
	}
}

func TestReadOnlyRejectsExecutionFlags(t *testing.T) {
	forbidExecution(t)
	for _, flag := range []string{"--checks", "--reviewer=false", "--allow-network", "--allow-prepare-network", "--parallel=2", "--cache-dir=cache", "--fuzz", "--base-tests", "--impacted-tests"} {
		var output bytes.Buffer
		if code := Run(context.Background(), []string{"review", "--read-only", flag}, &output, &output, "test"); code != 3 || !strings.Contains(output.String(), "read-only") {
			t.Errorf("%s: exit=%d %s", flag, code, output.String())
		}
	}
}
