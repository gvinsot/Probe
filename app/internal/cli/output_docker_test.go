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
	"sync/atomic"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
)

// exportProvider is a scripted provider. Unless fabricate is set, it creates
// and runs a generated test, then submits a REPRODUCED hypothesis citing the
// real evidence ID at auth.go:3. With fabricate, it submits a REPRODUCED
// hypothesis citing an evidence ID that does not exist.
func exportProvider(t *testing.T, fabricate bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		n := calls.Add(1)
		var name string
		var args any
		switch {
		case fabricate && n == 1:
			name = "submit_hypothesis"
			args = map[string]any{"title": "Guest is allowed through authorization", "severity": "critical", "status": "REPRODUCED", "rationale": "Asserted without an experiment.", "evidence_ids": []string{"fabricated"}, "path": "auth.go", "line": 3}
		case !fabricate && n == 1:
			name = "create_test"
			args = map[string]any{"path": "probe_guest_test.go", "description": "Guest authorization must remain rejected", "content": "package fixture\nimport \"testing\"\nfunc TestProbeRejectGuest(t *testing.T) { if Allowed(\"guest\") { t.Fatal(\"guest was authorized\") } }\n"}
		case !fabricate && n == 2:
			name = "run_generated_test"
			args = map[string]any{"test_id": "generated-test-1"}
		case !fabricate && n == 3:
			var observation struct {
				Evidence model.Evidence `json:"evidence"`
			}
			if len(request.Messages) == 0 || json.Unmarshal([]byte(request.Messages[len(request.Messages)-1].Content), &observation) != nil || observation.Evidence.ID == "" {
				http.Error(w, "missing real evidence", 500)
				return
			}
			name = "submit_hypothesis"
			args = map[string]any{"title": "Guest is allowed through authorization", "severity": "high", "status": "REPRODUCED", "rationale": "The generated named test passes on the baseline and fails on the candidate.", "evidence_ids": []string{observation.Evidence.ID}, "path": "auth.go", "line": 3}
		default:
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Investigation completed."}}}})
			return
		}
		arguments, _ := json.Marshal(args)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call-%d", n), "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}}}})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func exportPolicy(t *testing.T, image, endpoint string) string {
	t.Helper()
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Reviewer.Endpoint = endpoint
	cfg.Reviewer.Model = "scripted-integration"
	cfg.Reviewer.APIKeyEnv = "PROBE_INTEGRATION_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "")
	policy := filepath.Join(t.TempDir(), "policy.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(policy, b, 0600); err != nil {
		t.Fatal(err)
	}
	return policy
}

type sarifResultView struct {
	RuleID    string `json:"ruleId"`
	Level     string `json:"level"`
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI string `json:"uri"`
			} `json:"artifactLocation"`
			Region struct {
				StartLine int `json:"startLine"`
			} `json:"region"`
		} `json:"physicalLocation"`
	} `json:"locations"`
	RelatedLocations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI string `json:"uri"`
			} `json:"artifactLocation"`
		} `json:"physicalLocation"`
	} `json:"relatedLocations"`
	Properties struct {
		Probe struct {
			EvidenceIDs []string `json:"evidence_ids"`
			Artifacts   []struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"artifacts"`
		} `json:"probe"`
	} `json:"properties"`
}

type sarifView struct {
	Runs []struct {
		Results     []sarifResultView `json:"results"`
		Invocations []struct {
			ExecutionSuccessful bool `json:"executionSuccessful"`
			ExitCode            int  `json:"exitCode"`
		} `json:"invocations"`
	} `json:"runs"`
}

// A real reproduced run through Docker: the SARIF result and the PR comment
// cite the recorded evidence, and a re-render reproduces both files.
func TestDockerReviewExports(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	dir := fixture(t)
	server, calls := exportProvider(t, false)
	policy := exportPolicy(t, image, server.URL)
	url := "https://example.invalid/runs/1"
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--ci", "--out", "report",
		"--format", "markdown,json,sarif,pr-comment", "--report-url", url}, &out, &errOut, "integration")
	if code != 1 {
		t.Fatalf("expected exit 1; got %d\n%s\n%s", code, out.String(), errOut.String())
	}
	if calls.Load() != 4 {
		t.Fatalf("unexpected provider calls %d", calls.Load())
	}
	var r model.Report
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "report", "confidence-report.json"))), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.ReproducedIssues) != 1 || len(r.Evidence) != 1 {
		t.Fatalf("missing evidence chain: %+v", r)
	}
	var retained model.Artifact
	for _, a := range r.Artifacts {
		if a.Kind == model.ArtifactGeneratedTest {
			retained = a
		}
	}
	if retained.Path == "" {
		t.Fatal("reproducing test was not retained")
	}
	raw := readFile(t, filepath.Join(dir, "report", "confidence-report.sarif"))
	var log sarifView
	if err := json.Unmarshal([]byte(raw), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Runs) != 1 || len(log.Runs[0].Results) != 1 || !log.Runs[0].Invocations[0].ExecutionSuccessful || log.Runs[0].Invocations[0].ExitCode != 1 {
		t.Fatalf("SARIF: %s", raw)
	}
	result := log.Runs[0].Results[0]
	if result.RuleID != "probe/reproduced" || result.Level != "error" || len(result.Locations) != 1 ||
		result.Locations[0].PhysicalLocation.ArtifactLocation.URI != "auth.go" || result.Locations[0].PhysicalLocation.Region.StartLine != 3 {
		t.Fatalf("result: %s", raw)
	}
	if len(result.Properties.Probe.EvidenceIDs) != 1 || result.Properties.Probe.EvidenceIDs[0] != r.Evidence[0].ID {
		t.Fatalf("evidence IDs %v, JSON %s", result.Properties.Probe.EvidenceIDs, r.Evidence[0].ID)
	}
	if len(result.RelatedLocations) != 1 || result.RelatedLocations[0].PhysicalLocation.ArtifactLocation.URI != retained.Path ||
		len(result.Properties.Probe.Artifacts) != 1 || result.Properties.Probe.Artifacts[0].SHA256 != retained.SHA256 {
		t.Fatalf("retained test not linked (%+v): %s", retained, raw)
	}
	for _, forbidden := range []string{"\"precision\"", "security-severity", "\"confidence\""} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("SARIF contains %q", forbidden)
		}
	}
	comment := readFile(t, filepath.Join(dir, "report", "PR_COMMENT.md"))
	for _, want := range []string{"## Probe: 1 evidence-backed finding", "No finding is not approval.", "Exit code 1: a reproduced high or critical hypothesis was recorded.",
		"](" + url + ")", "Evidence: " + r.Evidence[0].ID + "."} {
		if !strings.Contains(comment, want) {
			t.Fatalf("PR comment lacks %q:\n%s", want, comment)
		}
	}
	if !strings.HasPrefix(comment, model.PRCommentBegin+"\n") || !strings.HasSuffix(comment, "Generated by Probe integration (https://github.com/gvinsot/Probe).\n"+model.PRCommentEnd+"\n") {
		t.Fatalf("markers or attribution:\n%s", comment)
	}
	code = Run(context.Background(), []string{"report", "--input", filepath.Join(dir, "report", "confidence-report.json"), "--out", filepath.Join(dir, "rerender"),
		"--format", "sarif,pr-comment", "--report-url", url}, &out, &errOut, "integration")
	if code != 0 {
		t.Fatalf("re-render exit %d: %s", code, errOut.String())
	}
	for _, name := range []string{"confidence-report.sarif", "PR_COMMENT.md"} {
		if readFile(t, filepath.Join(dir, "rerender", name)) != readFile(t, filepath.Join(dir, "report", name)) {
			t.Fatalf("%s differs after a re-render", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "probe_guest_test.go")); !os.IsNotExist(err) {
		t.Fatal("generated test leaked into checkout")
	}
}

// A fabricated reproduction produces no finding in either export, and the PR
// comment counts the unverified hypothesis.
func TestDockerReviewExportsRefuseFabricatedEvidence(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	dir := fixture(t)
	server, _ := exportProvider(t, true)
	policy := exportPolicy(t, image, server.URL)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--ci", "--out", "report", "--format", "json,sarif,pr-comment"}, &out, &errOut, "integration")
	if code != 2 {
		t.Fatalf("expected exit 2; got %d\n%s\n%s", code, out.String(), errOut.String())
	}
	raw := readFile(t, filepath.Join(dir, "report", "confidence-report.sarif"))
	var log sarifView
	if err := json.Unmarshal([]byte(raw), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Runs[0].Results) != 0 || !strings.Contains(raw, `"results": []`) || !strings.Contains(raw, "unverified_hypothesis") {
		t.Fatalf("SARIF: %s", raw)
	}
	comment := readFile(t, filepath.Join(dir, "report", "PR_COMMENT.md"))
	for _, want := range []string{"## Probe: no evidence-backed finding recorded", "- Unverified areas: 1 (0 recorded notes and 1 hypothesis that stayed UNVERIFIED).", "No finding is not approval."} {
		if !strings.Contains(comment, want) {
			t.Fatalf("PR comment lacks %q:\n%s", want, comment)
		}
	}
}
