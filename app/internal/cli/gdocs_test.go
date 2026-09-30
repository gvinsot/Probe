package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/gdocs"
)

const googleDoc = "1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcde"

// googleDocsServer serves one design document with one criterion, for an
// access token.
func googleDocsServer(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v3/files/"+googleDoc, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.test" {
			http.Error(w, `{"error":{"message":"unauthenticated"}}`, 401)
			return
		}
		fmt.Fprint(w, `{"name":"Auth design","mimeType":"application/vnd.google-apps.document","webViewLink":"https://docs.google.com/document/d/`+googleDoc+`/edit"}`)
	})
	mux.HandleFunc("/drive/v3/files/"+googleDoc+"/export", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# Design\n\n* Roles come from the identity provider\n\n# Acceptance criteria\n\n* Only admins are allowed\n")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv(gdocs.APIURLEnv, server.URL)
	t.Setenv(gdocs.AllowInsecureHTTPEnv, "true")
	t.Setenv(gdocs.AccessTokenEnv, "ya29.test")
	t.Setenv(gdocs.CredentialsEnv, "")
	t.Setenv(gdocs.ApplicationCredsEnv, "")
	t.Setenv(gdocs.APIKeyEnv, "")
}

func TestLintGoogleDocJoinsTheIntent(t *testing.T) {
	dir := fixture(t)
	googleDocsServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--gdoc", "https://docs.google.com/document/d/"+googleDoc+"/edit", "--intent", "Keep the signature.")
	if code != 0 || !strings.HasPrefix(r.Intent, "Google Doc: Auth design\n") || !strings.HasSuffix(r.Intent, "\n\nKeep the signature.") {
		t.Fatalf("exit %d, intent %q\n%s", code, r.Intent, output)
	}
	// The design bullet is context; only the document's criterion counts.
	if len(r.IntentCriteria) != 1 || r.IntentCriteria[0].Text != "Only admins are allowed" || !strings.Contains(r.Intent, "• Roles come from the identity provider") {
		t.Fatalf("criteria %+v, intent %q", r.IntentCriteria, r.Intent)
	}
	if !strings.Contains(output, `Google Docs: document "Auth design" joins the intent.`) {
		t.Fatalf("output:\n%s", output)
	}
}

func TestLintGoogleDocWithJiraAndNotion(t *testing.T) {
	dir := fixture(t)
	googleDocsServer(t)
	jiraServer(t)
	notionServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--jira", "SHOP-7", "--notion", notionPage, "--gdoc", googleDoc)
	if code != 0 || !strings.HasPrefix(r.Intent, "Jira issue SHOP-7") {
		t.Fatalf("exit %d, intent %q\n%s", code, r.Intent, output)
	}
	jira, notion, doc := strings.Index(r.Intent, "Jira issue"), strings.Index(r.Intent, "Notion page:"), strings.Index(r.Intent, "Google Doc:")
	if !(jira < notion && notion < doc) || len(r.IntentCriteria) != 3 {
		t.Fatalf("order %d %d %d, criteria %+v", jira, notion, doc, r.IntentCriteria)
	}
}

func TestGoogleDocErrorsExit3(t *testing.T) {
	dir := fixture(t)
	googleDocsServer(t)
	for name, args := range map[string][]string{
		"not a doc URL": {"lint", "--repo", dir, "--gdoc", "https://evil.example/document/d/" + googleDoc},
		"missing":       {"lint", "--repo", dir, "--gdoc", strings.Repeat("m", 44)},
		"plan badref":   {"plan", "--repo", dir, "--gdoc", "x"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != 3 || !strings.Contains(stderr, "gdoc") {
			t.Errorf("%s: exit %d: %s", name, code, stderr)
		}
	}
	saved := gdocsEnv
	gdocsEnv = func() (gdocs.Config, error) { return gdocs.FromEnv(func(string) string { return "" }, nil) }
	defer func() { gdocsEnv = saved }()
	if code, _, stderr := runCLI(t, "lint", "--repo", dir, "--gdoc", googleDoc); code != 3 || !strings.Contains(stderr, gdocs.AccessTokenEnv) {
		t.Fatalf("unconfigured: exit %d: %s", code, stderr)
	}
}
