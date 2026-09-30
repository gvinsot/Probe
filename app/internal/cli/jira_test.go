package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gvinsot/Probe/app/internal/jira"
)

// jiraServer serves SHOP-7 (REST API v3) and counts requests.
func jiraServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/rest/api/3/issue/SHOP-7" || r.Header.Get("Authorization") != "Bearer test-pat" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"key":"SHOP-7","fields":{"summary":"Only admins are allowed","issuetype":{"name":"Story"},"status":{"name":"In Review"},
"description":{"type":"doc","version":1,"content":[
 {"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"Acceptance criteria"}]},
 {"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Allowed returns false for non-admin users"}]}]}]}]}}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv(jira.URLEnv, server.URL)
	t.Setenv(jira.AllowInsecureHTTPEnv, "true")
	t.Setenv(jira.TokenEnv, "test-pat")
	t.Setenv(jira.EmailEnv, "")
	for _, name := range branchEnv {
		t.Setenv(name, "")
	}
	return &calls
}

func TestLintJiraIssueJoinsTheIntent(t *testing.T) {
	dir := fixture(t)
	calls := jiraServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--jira", "SHOP-7", "--intent", "Also keep the signature.")
	if code != 0 || calls.Load() != 1 {
		t.Fatalf("exit %d, Jira calls %d\n%s", code, calls.Load(), output)
	}
	if !strings.HasPrefix(r.Intent, "Jira issue SHOP-7: Only admins are allowed\n") || !strings.HasSuffix(r.Intent, "\n\nAlso keep the signature.") || !strings.Contains(r.Intent, "Link: ") {
		t.Fatalf("intent %q", r.Intent)
	}
	if len(r.IntentCriteria) != 1 || r.IntentCriteria[0].Text != "Allowed returns false for non-admin users" {
		t.Fatalf("criteria %+v", r.IntentCriteria)
	}
	doc, _ := parseIntent(r.Intent)
	if r.IntentSHA256 != doc.SHA256 {
		t.Fatal("intent hash does not cover the recorded intent")
	}
	if !strings.Contains(output, `Jira: SHOP-7 "Only admins are allowed" joins the intent.`) {
		t.Fatalf("output:\n%s", output)
	}
}

func TestLintJiraAutoFindsTheKey(t *testing.T) {
	dir := fixture(t)
	calls := jiraServer(t)
	// Neither the branch ("candidate") nor the first commit names an issue.
	write(t, dir, "note.txt", "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "Tighten admin check\n\nRefs SHOP-7")
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--jira", "auto")
	if code != 0 || calls.Load() != 1 || len(r.IntentCriteria) != 1 {
		t.Fatalf("exit %d, Jira calls %d, criteria %+v\n%s", code, calls.Load(), r.IntentCriteria, output)
	}

	// The branch name wins over commit messages.
	git(t, dir, "checkout", "-b", "feature/SHOP-8-other")
	code, _, stderr := runCLI(t, "lint", "--repo", dir, "--out", t.TempDir(), "--jira", "auto")
	if code != 3 || !strings.Contains(stderr, "SHOP-8: issue not found") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

func TestJiraAutoWithoutKeyContinues(t *testing.T) {
	dir := fixture(t)
	calls := jiraServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--jira", "auto")
	if code != 0 || calls.Load() != 0 || r.Intent != "" || !strings.Contains(output, "no issue key found") {
		t.Fatalf("exit %d, calls %d, intent %q\n%s", code, calls.Load(), r.Intent, output)
	}
}

func TestJiraErrorsExit3(t *testing.T) {
	dir := fixture(t)
	calls := jiraServer(t)
	for name, args := range map[string][]string{
		"invalid key": {"lint", "--repo", dir, "--jira", "shop-7"},
		"missing":     {"lint", "--repo", dir, "--jira", "SHOP-404"},
		"plan key":    {"plan", "--repo", dir, "--jira", "../x"},
		"plan no key": {"plan", "--repo", dir, "--jira", "auto"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != 3 || !strings.Contains(stderr, "jira") {
			t.Errorf("%s: exit %d: %s", name, code, stderr)
		}
	}
	t.Setenv(jira.URLEnv, "")
	code, _, stderr := runCLI(t, "lint", "--repo", dir, "--jira", "SHOP-7")
	if code != 3 || !strings.Contains(stderr, jira.URLEnv) {
		t.Fatalf("unconfigured: exit %d: %s", code, stderr)
	}
	if calls.Load() != 2 { // SHOP-404 on v3, then v2
		t.Fatalf("Jira calls %d", calls.Load())
	}
}

func TestFitJiraIntent(t *testing.T) {
	text := strings.Repeat("é", 100)
	got := fitJiraIntent(text, 120)
	if len(got) > 120 || !strings.HasSuffix(got, jiraTruncated) || !strings.HasPrefix(got, "é") {
		t.Fatalf("%q", got)
	}
	if fitJiraIntent(text, 10) != "" || fitJiraIntent("short", 10) != "short" {
		t.Fatal("limits")
	}
}
