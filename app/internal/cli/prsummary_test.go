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

	"github.com/gvinsot/Probe/app/internal/config"
)

// summaryServer answers the investigation with a closing text and the
// summary request with a summary.
func summaryServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	var summaries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		content := "Done"
		if strings.HasPrefix(request.Messages[0].Content, "You write the description of a pull request") {
			summaries.Add(1)
			if !strings.Contains(request.Messages[1].Content, "candidate") {
				t.Errorf("the commit messages are missing from the summary input")
			}
			content = `{"title":"Let every user pass the admin check","overview":"Allowed now returns true for any user.","changes":[{"area":"Authorization","summary":"The role comparison was removed.","files":["auth.go"]}],"behavior_changes":["Non-admins are allowed"],"risks":["Unverified: privilege escalation"],"review_focus":["auth.go line 3"],"testing":"Nothing was executed."}`
		}
		reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}}})
		w.Write(reply)
	}))
	t.Cleanup(server.Close)
	t.Setenv(config.EndpointEnv, server.URL)
	t.Setenv(config.ModelEnv, "deployment-model")
	t.Setenv("PROBE_API_KEY", "deployment-key")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	return &summaries
}

func TestReviewWritesThePRSummary(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	summaries := summaryServer(t)
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"review", "--read-only", "--repo", dir, "--out", "report", "--ci"}, &output, &output, "test"); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	if summaries.Load() != 1 || !strings.Contains(output.String(), "Writing the pull request summary") {
		t.Fatalf("summary calls %d\n%s", summaries.Load(), output.String())
	}
	r := readReviewerReport(t, dir)
	if r.PRSummary == nil || r.PRSummary.Title != "Let every user pass the admin check" || r.PRSummary.Model != "deployment-model" {
		t.Fatalf("pr_summary %+v", r.PRSummary)
	}
	found := false
	for _, e := range r.Audit {
		found = found || e.Tool == "pr_summary_completion"
	}
	if !found {
		t.Fatal("the summary completion is not audited")
	}
	data, err := os.ReadFile(filepath.Join(dir, "report", "PR_SUMMARY.md"))
	if err != nil || !strings.HasPrefix(string(data), "# Let every user pass the admin check") {
		t.Fatalf("PR_SUMMARY.md %v:\n%s", err, data)
	}
}

func TestPRSummaryFlag(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	summaries := summaryServer(t)
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"review", "--read-only", "--pr-summary=false", "--repo", dir, "--out", "report", "--ci"}, &output, &output, "test"); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	if summaries.Load() != 0 || readReviewerReport(t, dir).PRSummary != nil {
		t.Fatal("--pr-summary=false still summarized")
	}
	for _, args := range [][]string{
		{"lint", "--repo", dir, "--pr-summary"},
		{"review", "--repo", dir, "--reviewer=false", "--checks=false", "--pr-summary"},
	} {
		output.Reset()
		if code := Run(context.Background(), args, &output, &output, "test"); code != 3 || !strings.Contains(output.String(), "--pr-summary") {
			t.Errorf("%v: exit %d: %s", args, code, output.String())
		}
	}
}
