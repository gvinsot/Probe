package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/acceptance"
)

func TestFindKeys(t *testing.T) {
	cases := []struct {
		teams []string
		texts []string
		want  []string
	}{
		{nil, []string{"ana/eng-123-fix-login"}, []string{"ENG-123"}},
		{nil, []string{"release-2", "Fixes WEB-7 and ENG-9", "WEB-7 again"}, []string{"WEB-7", "ENG-9"}},
		{nil, []string{"utf-8 sha-256 v-2 feature-12"}, nil},
		{[]string{"ENG"}, []string{"web-4 eng-5"}, []string{"ENG-5"}},
		{nil, []string{"xeng-12a", "eng-0"}, nil},
		{nil, []string{"a-1 b-2 c-3 d-4 e-5 f-6"}, []string{"A-1", "B-2", "C-3", "D-4", "E-5"}},
	}
	for _, c := range cases {
		if got := FindKeys(c.teams, c.texts...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("FindKeys(%v, %q) = %q, want %q", c.teams, c.texts, got, c.want)
		}
	}
	for key, want := range map[string]bool{"ENG-1": true, "E2E-9": true, "eng-1": false, "ENG_X-1": false, "ENG-01": false, "../x": false} {
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
	if err != nil || c.Configured() || c.URL != DefaultURL {
		t.Fatalf("unset: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{APIKeyEnv: "lin_api_x", TeamsEnv: "eng, Web"}), files(nil))
	if err != nil || c.APIKey != "lin_api_x" || !reflect.DeepEqual(c.Teams, []string{"ENG", "WEB"}) {
		t.Fatalf("env: %+v %v", c, err)
	}
	c, err = FromEnv(env(nil), files(map[string]string{"/run/secrets/PROBE_LINEAR_API_KEY": "lin_api_s\n"}))
	if err != nil || c.APIKey != "lin_api_s" || c.KeySource != "/run/secrets/PROBE_LINEAR_API_KEY" {
		t.Fatalf("secret: %+v %v", c, err)
	}
	for name, values := range map[string]map[string]string{
		"http":    {URLEnv: "http://linear.test/graphql"},
		"scheme":  {URLEnv: "ftp://linear.test"},
		"creds":   {URLEnv: "https://u:p@linear.test"},
		"teams":   {TeamsEnv: "eng-1"},
		"file":    {APIKeyEnv + "_FILE": "/absent"},
		"boolean": {AllowInsecureHTTPEnv: "maybe"},
	} {
		if _, err := FromEnv(env(values), files(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const issueJSON = `{"data":{"issue":{"identifier":"ENG-42","title":"Only admins are allowed","url":"https://linear.app/acme/issue/ENG-42",
"description":"Context.\n\n# Acceptance criteria\n\n- [ ] Allowed returns false for non-admins\n- [x] Admins still pass\n\n# Notes\n\n- not a criterion",
"priorityLabel":"High","state":{"name":"In Progress"},"team":{"key":"ENG","name":"Engineering"},"project":{"name":"Auth"},
"parent":{"identifier":"ENG-40","title":"Harden auth"},"labels":{"nodes":[{"name":"security"}]}}}}`

func TestFetchAndIntent(t *testing.T) {
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if json.Unmarshal(body, &req) != nil || !strings.Contains(req.Query, "issue(id: $id)") {
			http.Error(w, "bad query", 400)
			return
		}
		switch req.Variables["id"] {
		case "ENG-42":
			w.Write([]byte(issueJSON))
		case "DENY-1":
			http.Error(w, `{"errors":[{"message":"Authentication required"}]}`, http.StatusUnauthorized)
		case "MOVE-1":
			http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusTemporaryRedirect)
		default:
			w.Write([]byte(`{"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INPUT_ERROR"}}],"data":null}`))
		}
	}))
	defer server.Close()

	client := &Client{Config: Config{URL: server.URL, APIKey: "lin_api_secret", AllowInsecureHTTP: true}}
	issue, err := client.Fetch(context.Background(), "ENG-42")
	if err != nil {
		t.Fatal(err)
	}
	if auth[0] != "lin_api_secret" {
		t.Fatalf("personal key header %q", auth[0])
	}
	want := Issue{Key: "ENG-42", URL: "https://linear.app/acme/issue/ENG-42", Title: "Only admins are allowed", Status: "In Progress", Priority: "High", Team: "Engineering", Project: "Auth", Labels: []string{"security"}, Parent: "ENG-40: Harden auth"}
	want.Description = issue.Description
	if !reflect.DeepEqual(issue, want) {
		t.Fatalf("issue %+v", issue)
	}
	text := issue.Intent()
	if !strings.HasPrefix(text, "Linear issue ENG-42: Only admins are allowed\n\nTeam: Engineering · Project: Auth · Status: In Progress · Priority: High · Parent: ENG-40: Harden auth · Labels: security · Link: https://linear.app/acme/issue/ENG-42\n") {
		t.Fatalf("intent:\n%s", text)
	}
	doc, err := acceptance.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Criteria) != 2 || doc.Criteria[0].Text != "Allowed returns false for non-admins" {
		t.Fatalf("criteria %+v\n%s", doc.Criteria, text)
	}

	oauth := &Client{Config: Config{URL: server.URL, APIKey: "oauth-token", AllowInsecureHTTP: true}}
	if _, err := oauth.Fetch(context.Background(), "NONE-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if auth[len(auth)-1] != "Bearer oauth-token" {
		t.Fatalf("oauth header %q", auth[len(auth)-1])
	}
	if _, err := client.Fetch(context.Background(), "DENY-1"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := client.Fetch(context.Background(), "MOVE-1"); err == nil || strings.Contains(err.Error(), "lin_api_secret") {
		t.Fatalf("redirect: %v", err)
	}
	if _, err := client.Fetch(context.Background(), "eng-42"); err == nil {
		t.Fatal("lower-case identifier sent")
	}
	if _, err := (&Client{}).Fetch(context.Background(), "ENG-1"); err == nil {
		t.Fatal("unconfigured client fetched")
	}
}
