package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/notion"
)

const notionPage = "0123456789abcdef0123456789abcdef"

// notionServer serves one page: an architecture note with one criterion.
func notionServer(t *testing.T) {
	t.Helper()
	id := "01234567-89ab-cdef-0123-456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/"+id, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"page","url":"https://www.notion.so/Auth-`+notionPage+`","properties":{"title":{"type":"title","title":[{"plain_text":"Auth architecture"}]}}}`)
	})
	mux.HandleFunc("/v1/blocks/"+id+"/children", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[
{"id":"a","type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Roles come from the identity provider"}]}},
{"id":"b","type":"heading_2","heading_2":{"rich_text":[{"plain_text":"Acceptance criteria"}]}},
{"id":"c","type":"to_do","to_do":{"rich_text":[{"plain_text":"Only admins are allowed"}],"checked":false}}],"has_more":false}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv(notion.URLEnv, server.URL)
	t.Setenv(notion.AllowInsecureHTTPEnv, "true")
	t.Setenv(notion.TokenEnv, "ntn_test")
}

func TestLintNotionPageJoinsTheIntent(t *testing.T) {
	dir := fixture(t)
	notionServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--notion", "https://www.notion.so/acme/Auth-"+notionPage+"?pvs=4", "--intent", "Keep the signature.")
	if code != 0 || !strings.HasPrefix(r.Intent, "Notion page: Auth architecture\n") || !strings.HasSuffix(r.Intent, "\n\nKeep the signature.") {
		t.Fatalf("exit %d, intent %q\n%s", code, r.Intent, output)
	}
	// The architecture bullet is context; only the page's criterion counts.
	if len(r.IntentCriteria) != 1 || r.IntentCriteria[0].Text != "Only admins are allowed" {
		t.Fatalf("criteria %+v", r.IntentCriteria)
	}
	if !strings.Contains(r.Intent, "• Roles come from the identity provider") || !strings.Contains(output, `Notion: page "Auth architecture" joins the intent.`) {
		t.Fatalf("intent %q\n%s", r.Intent, output)
	}
}

func TestLintNotionWithLinear(t *testing.T) {
	dir := fixture(t)
	notionServer(t)
	linearServer(t)
	code, r, _, output := runReport(t, context.Background(), dir, "lint", "--linear", "ENG-42", "--notion", notionPage)
	if code != 0 || !strings.HasPrefix(r.Intent, "Linear issue ENG-42") || !strings.Contains(r.Intent, "\n\nNotion page: Auth architecture") || len(r.IntentCriteria) != 2 {
		t.Fatalf("exit %d, criteria %+v, intent %q\n%s", code, r.IntentCriteria, r.Intent, output)
	}
}

func TestNotionErrorsExit3(t *testing.T) {
	dir := fixture(t)
	notionServer(t)
	for name, args := range map[string][]string{
		"not a page":  {"lint", "--repo", dir, "--notion", "https://evil.example/" + notionPage},
		"missing":     {"lint", "--repo", dir, "--notion", strings.Repeat("f", 32)},
		"too many":    {"lint", "--repo", dir, "--notion", "1" + notionPage[1:] + ",2" + notionPage[1:] + ",3" + notionPage[1:] + ",4" + notionPage[1:] + ",5" + notionPage[1:] + ",6" + notionPage[1:]},
		"plan badref": {"plan", "--repo", dir, "--notion", "x"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != 3 || !strings.Contains(stderr, "notion") {
			t.Errorf("%s: exit %d: %s", name, code, stderr)
		}
	}
	t.Setenv(notion.TokenEnv, "")
	saved := notionEnv
	notionEnv = func() (notion.Config, error) { return notion.FromEnv(func(string) string { return "" }, nil) }
	defer func() { notionEnv = saved }()
	if code, _, stderr := runCLI(t, "lint", "--repo", dir, "--notion", notionPage); code != 3 || !strings.Contains(stderr, notion.TokenEnv) {
		t.Fatalf("unconfigured: exit %d: %s", code, stderr)
	}
}
