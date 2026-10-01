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
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/hublogin"
)

func TestLoginStatusLogout(t *testing.T) {
	revoked := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/device/code":
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "BCDF-GHJK", "verification_uri_complete": server.URL + "/device.html?code=BCDF-GHJK", "expires_in": 600, "interval": 5})
		case "/api/device/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "probe_mcp.k.s", "expires_in": 3600, "endpoint": server.URL + "/llm/v1", "model": "served", "login": "octocat", "provider": "github"})
		case "/llm/v1/account":
			json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "provider": "github", "model": "served", "daily_tokens": 1000, "used_today": 10})
		case "/llm/v1/token":
			revoked = r.Header.Get("Authorization") == "Bearer probe_mcp.k.s"
			json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
		}
	}))
	defer server.Close()
	previous := newHubClient
	newHubClient = func() *hublogin.Client {
		c := hublogin.NewClient()
		c.Wait = func(context.Context, time.Duration) error { return nil }
		return c
	}
	defer func() { newHubClient = previous }()
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv(hublogin.CredentialsFileEnv, path)
	t.Setenv(hublogin.HubTokenEnv, "")

	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"login", "--hub", server.URL}, &out, &errOut, "test"); code != 0 {
		t.Fatalf("login = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "BCDF-GHJK") || !strings.Contains(out.String(), "Logged in to "+server.URL+" as octocat (github)") {
		t.Fatalf("login output: %s", out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credentials not saved: %v", err)
	}

	out.Reset()
	if code := Run(context.Background(), []string{"login", "--status"}, &out, &errOut, "test"); code != 0 || !strings.Contains(out.String(), "10 of 1000 tokens") {
		t.Fatalf("status = %d: %s %s", code, out.String(), errOut.String())
	}

	out.Reset()
	if code := Run(context.Background(), []string{"logout"}, &out, &errOut, "test"); code != 0 || !revoked {
		t.Fatalf("logout = %d, revoked=%v: %s", code, revoked, errOut.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credentials still on disk: %v", err)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"login", "--status"}, &out, &errOut, "test"); code != 1 || !strings.Contains(out.String(), "Not logged in") {
		t.Fatalf("status after logout = %d: %s", code, out.String())
	}
}

func TestLoginSwitchedOff(t *testing.T) {
	t.Setenv(hublogin.CredentialsFileEnv, "off")
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"login"}, &out, &errOut, "test"); code != 3 || !strings.Contains(errOut.String(), "nowhere to store") {
		t.Fatalf("login = %d: %s", code, errOut.String())
	}
}
