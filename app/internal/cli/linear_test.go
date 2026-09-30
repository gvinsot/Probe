package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/linear"
)

// linearServer serves ENG-42 and records the identifiers asked for.
func linearServer(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	asked := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]string `json:"variables"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		*asked = append(*asked, req.Variables["id"])
		mu.Unlock()
		if r.Header.Get("Authorization") != "lin_api_test" || req.Variables["id"] != "ENG-42" {
			w.Write([]byte(`{"errors":[{"message":"Entity not found: Issue"}],"data":null}`))
			return
		}
		w.Write([]byte(`{"data":{"issue":{"identifier":"ENG-42","title":"Only admins are allowed","url":"https://linear.app/acme/issue/ENG-42",
"description":"## Acceptance criteria\n\n- Allowed returns false for non-admin users","state":{"name":"Todo"},"team":{"key":"ENG","name":"Engineering"}}}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv(linear.URLEnv, server.URL)
	t.Setenv(linear.AllowInsecureHTTPEnv, "true")
	t.Setenv(linear.APIKeyEnv, "lin_api_test")
	t.Setenv(linear.TeamsEnv, "")
	for _, name := range branchEnv {
		t.Setenv(name, "")
	}
	return asked
}

func TestLintLinearIssueJoinsTheIntent(t *testing.T) {
	dir := fixture(t)
	asked := linearServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--linear", "ENG-42", "--intent", "Keep the signature.")
	if code != 0 || len(*asked) != 1 {
		t.Fatalf("exit %d, asked %v\n%s", code, *asked, output)
	}
	if !strings.HasPrefix(r.Intent, "Linear issue ENG-42: Only admins are allowed\n") || !strings.HasSuffix(r.Intent, "\n\nKeep the signature.") {
		t.Fatalf("intent %q", r.Intent)
	}
	if len(r.IntentCriteria) != 1 || r.IntentCriteria[0].Text != "Allowed returns false for non-admin users" {
		t.Fatalf("criteria %+v", r.IntentCriteria)
	}
	if !strings.Contains(output, `Linear: ENG-42 "Only admins are allowed" joins the intent.`) {
		t.Fatalf("output:\n%s", output)
	}
}

func TestLintLinearAutoSkipsUnknownCandidates(t *testing.T) {
	dir := fixture(t)
	asked := linearServer(t)
	// Linear's lower-case branch name; "release-2" is ignored, "web-3" is
	// tried and unknown, then the commit message names the issue.
	git(t, dir, "checkout", "-b", "ana/web-3-release-2")
	write(t, dir, "note.txt", "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "Tighten admin check\n\nFixes ENG-42")
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--linear", "auto")
	if code != 0 || len(r.IntentCriteria) != 1 || strings.Join(*asked, ",") != "WEB-3,ENG-42" {
		t.Fatalf("exit %d, asked %v, criteria %+v\n%s", code, *asked, r.IntentCriteria, output)
	}

	// No usable candidate: continue without an issue.
	*asked = nil
	git(t, dir, "checkout", "-q", "-b", "plain", "main")
	code, r, _, output = runReport(t, context.Background(), dir, "lint", "--linear", "auto")
	if code != 0 || r.Intent != "" || !strings.Contains(output, "no issue found") {
		t.Fatalf("exit %d, intent %q\n%s", code, r.Intent, output)
	}
}

func TestLintJiraAndLinearTogether(t *testing.T) {
	dir := fixture(t)
	jiraServer(t)
	linearServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--jira", "SHOP-7", "--linear", "ENG-42")
	if code != 0 || !strings.HasPrefix(r.Intent, "Jira issue SHOP-7") || !strings.Contains(r.Intent, "\n\nLinear issue ENG-42") {
		t.Fatalf("exit %d, intent %q\n%s", code, r.Intent, output)
	}
	if len(r.IntentCriteria) != 2 {
		t.Fatalf("criteria %+v", r.IntentCriteria)
	}
}

func TestLinearErrorsExit3(t *testing.T) {
	dir := fixture(t)
	linearServer(t)
	for name, args := range map[string][]string{
		"invalid":  {"lint", "--repo", dir, "--linear", "eng-42"},
		"missing":  {"lint", "--repo", dir, "--linear", "ENG-404"},
		"plan key": {"plan", "--repo", dir, "--linear", "../x"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != 3 || !strings.Contains(stderr, "linear") {
			t.Errorf("%s: exit %d: %s", name, code, stderr)
		}
	}
	// No key in the environment nor in a secret file.
	saved := linearEnv
	linearEnv = func() (linear.Config, error) { return linear.FromEnv(func(string) string { return "" }, nil) }
	defer func() { linearEnv = saved }()
	code, _, stderr := runCLI(t, "lint", "--repo", dir, "--linear", "ENG-42")
	if code != 3 || !strings.Contains(stderr, linear.APIKeyEnv) {
		t.Fatalf("unconfigured: exit %d: %s", code, stderr)
	}
}
