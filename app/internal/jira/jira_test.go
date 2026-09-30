package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/acceptance"
)

func TestFindKey(t *testing.T) {
	cases := []struct {
		texts []string
		want  string
	}{
		{[]string{"feature/PROJ-12-login"}, "PROJ-12"},
		{[]string{"main", "PROJ-7: fix the login"}, "PROJ-7"},
		{[]string{"feature/login", "Use UTF-8 SHA-256 AB_C-9"}, "AB_C-9"},
		{[]string{"PROJ-12-login/OPS-3"}, "PROJ-12"},
		{[]string{"XPROJ-12a", "proj-4", "PROJ-0"}, ""},
		{[]string{"[OPS-3] deploy", "PROJ-1"}, "OPS-3"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := FindKey(c.texts...); got != c.want {
			t.Errorf("FindKey(%q) = %q, want %q", c.texts, got, c.want)
		}
	}
	for key, want := range map[string]bool{"PROJ-1": true, "A-9": true, "PROJ-1/x": false, "../x": false, "PROJ-01": false, "": false} {
		if ValidKey(key) != want {
			t.Errorf("ValidKey(%q) != %v", key, want)
		}
	}
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func files(values map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := values[name]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestFromEnv(t *testing.T) {
	c, err := FromEnv(env(nil), files(nil))
	if err != nil || c.Configured() {
		t.Fatalf("unset: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{URLEnv: "https://acme.atlassian.net/", EmailEnv: "dev@acme.test", TokenEnv: "tok\n"}), files(nil))
	if err != nil || c.BaseURL != "https://acme.atlassian.net" || c.Token != "tok" || c.Email != "dev@acme.test" {
		t.Fatalf("cloud: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{URLEnv: "https://jira.acme.test"}), files(map[string]string{"/run/secrets/PROBE_JIRA_TOKEN": "pat\n"}))
	if err != nil || c.Token != "pat" || c.TokenSource != "/run/secrets/PROBE_JIRA_TOKEN" {
		t.Fatalf("secret: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{URLEnv: "http://localhost:8080", AllowInsecureHTTPEnv: "true"}), files(nil))
	if err != nil || c.Token != "" {
		t.Fatalf("anonymous http: %+v %v", c, err)
	}
	bad := map[string]map[string]string{
		"http":             {URLEnv: "http://jira.acme.test"},
		"scheme":           {URLEnv: "ftp://jira.acme.test"},
		"credentials":      {URLEnv: "https://u:p@jira.acme.test"},
		"relative":         {URLEnv: "jira.acme.test"},
		"field":            {URLEnv: "https://jira.acme.test", CriteriaFieldEnv: "summary"},
		"email only":       {URLEnv: "https://jira.acme.test", EmailEnv: "dev@acme.test"},
		"missing file":     {URLEnv: "https://jira.acme.test", TokenEnv + "_FILE": "/run/secrets/absent"},
		"insecure boolean": {URLEnv: "https://jira.acme.test", AllowInsecureHTTPEnv: "maybe"},
	}
	for name, values := range bad {
		if _, err := FromEnv(env(values), files(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const adfDescription = `{"type":"doc","version":1,"content":[
 {"type":"paragraph","content":[{"type":"text","text":"Checkout applies "},{"type":"text","text":"discounts","marks":[{"type":"strong"}]},{"type":"text","text":"."}]},
 {"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Acceptance criteria"}]},
 {"type":"bulletList","content":[
  {"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Orders of 100 or more get 10 off"}]},
   {"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"including tax"}]}]}]}]},
  {"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Orders of 50 or more ship free"}]}]}]},
 {"type":"taskList","content":[{"type":"taskItem","attrs":{"state":"DONE"},"content":[{"type":"text","text":"Reviewed by "},{"type":"mention","attrs":{"text":"@ana"}}]}]},
 {"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"- not a list"}]}
]}`

func TestADFToMarkdown(t *testing.T) {
	var doc adfNode
	if err := json.Unmarshal([]byte(adfDescription), &doc); err != nil {
		t.Fatal(err)
	}
	got := ADFToMarkdown(doc)
	want := "Checkout applies discounts.\n\n## Acceptance criteria\n\n- Orders of 100 or more get 10 off\n  - including tax\n- Orders of 50 or more ship free\n\n- [x] Reviewed by @ana\n\n```go\n- not a list\n```\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestADFDepthIsBounded(t *testing.T) {
	n := adfNode{Type: "text", Text: "deep"}
	for i := 0; i < 500; i++ {
		n = adfNode{Type: "blockquote", Content: []adfNode{n}}
	}
	_ = ADFToMarkdown(adfNode{Type: "doc", Content: []adfNode{n}})
}

func TestWikiToMarkdown(t *testing.T) {
	in := "h1. Goal\r\nShip [the docs|https://acme.test/docs].\n\nh3. Acceptance criteria\n* one\n** nested\n# first\n----\n{code:go}\n* kept\n{code}\nbq. quoted"
	want := "# Goal\nShip the docs (https://acme.test/docs).\n\n### Acceptance criteria\n- one\n  - nested\n1. first\n\n```\n* kept\n```\nquoted\n"
	if got := WikiToMarkdown(in); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestIntentScopesCriteria(t *testing.T) {
	issue := Issue{Key: "SHOP-9", URL: "https://acme.test/browse/SHOP-9", Summary: "Export the acceptance criteria", Type: "Story", Status: "In Progress", Labels: []string{"checkout"},
		Description: "Context.\n\n# Acceptance criteria\n\n- from the description\n\n- another"}
	doc, err := acceptance.Parse(issue.Intent())
	if err != nil {
		t.Fatal(err)
	}
	// The description's own criteria section supplies criteria; the summary
	// is not a heading and does not scope anything.
	if len(doc.Criteria) != 2 || doc.Criteria[0].Text != "from the description" {
		t.Fatalf("criteria %+v\n%s", doc.Criteria, issue.Intent())
	}
	if !strings.HasPrefix(issue.Intent(), "Jira issue SHOP-9: Export the acceptance criteria\n\nType: Story · Status: In Progress · Labels: checkout · Link: https://acme.test/browse/SHOP-9\n") {
		t.Fatalf("header:\n%s", issue.Intent())
	}
	// A configured criteria field is the only source of criteria.
	issue.Criteria = "- from the field"
	doc, err = acceptance.Parse(issue.Intent())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Criteria) != 1 || doc.Criteria[0].Text != "from the field" {
		t.Fatalf("criteria %+v\n%s", doc.Criteria, issue.Intent())
	}
	if !strings.Contains(issue.Intent(), "### criteria\n") {
		t.Fatalf("description heading not nested and renamed:\n%s", issue.Intent())
	}
}

func TestFetchCloudServerAndErrors(t *testing.T) {
	var auth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/CLOUD-1", func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if !strings.Contains(r.URL.Query().Get("fields"), "customfield_10035") {
			t.Errorf("fields %q", r.URL.RawQuery)
		}
		w.Write([]byte(`{"key":"CLOUD-1","fields":{"summary":"Discounts","issuetype":{"name":"Story"},"status":{"name":"To Do"},"labels":["pay"],"description":` + adfDescription + `,"customfield_10035":"* field criterion"}}`))
	})
	mux.HandleFunc("/rest/api/2/issue/DC-2", func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Write([]byte(`{"key":"DC-2","fields":{"summary":"Server","description":"* wiki item","issuetype":null}}`))
	})
	mux.HandleFunc("/rest/api/3/issue/DENY-1", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) })
	mux.HandleFunc("/rest/api/3/issue/MOVE-1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/steal", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cloud := &Client{Config: Config{BaseURL: server.URL, Email: "dev@acme.test", Token: "secret-token", CriteriaField: "customfield_10035", AllowInsecureHTTP: true}}
	issue, err := cloud.Fetch(context.Background(), "CLOUD-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.APIVersion != 3 || issue.Summary != "Discounts" || issue.Type != "Story" || issue.Criteria != "- field criterion" || !strings.Contains(issue.Description, "- Orders of 50 or more ship free") || issue.URL != server.URL+"/browse/CLOUD-1" {
		t.Fatalf("issue %+v", issue)
	}
	if auth[0] != "Basic ZGV2QGFjbWUudGVzdDpzZWNyZXQtdG9rZW4=" {
		t.Fatalf("basic auth %q", auth[0])
	}

	server2 := &Client{Config: Config{BaseURL: server.URL, Token: "pat"}}
	issue, err = server2.Fetch(context.Background(), "DC-2")
	if err != nil || issue.APIVersion != 2 || issue.Description != "- wiki item" {
		t.Fatalf("v2 fallback: %+v %v", issue, err)
	}
	if auth[len(auth)-1] != "Bearer pat" {
		t.Fatalf("bearer %q", auth[len(auth)-1])
	}

	if _, err := server2.Fetch(context.Background(), "NONE-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
	if _, err := server2.Fetch(context.Background(), "DENY-1"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := cloud.Fetch(context.Background(), "MOVE-1"); err == nil || !strings.Contains(err.Error(), "another origin") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("cross-origin redirect: %v", err)
	}
	if _, err := server2.Fetch(context.Background(), "../admin"); err == nil {
		t.Fatal("invalid key fetched")
	}
	if _, err := (&Client{}).Fetch(context.Background(), "A-1"); err == nil {
		t.Fatal("unconfigured client fetched")
	}
}
