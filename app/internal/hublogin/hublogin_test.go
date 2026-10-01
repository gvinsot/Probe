package hublogin

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func filesOf(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		data, ok := files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(data), nil
	}
}

func TestNormalizeHub(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hub.example/":       "https://hub.example",
		"http://127.0.0.1:8080":      "http://127.0.0.1:8080",
		"http://localhost:8080/hub/": "http://localhost:8080/hub",
	} {
		if got, err := NormalizeHub(raw); err != nil || got != want {
			t.Errorf("NormalizeHub(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://hub.example", "https://user:pw@hub.example", "https://hub.example?x=1", "hub.example", ""} {
		if _, err := NormalizeHub(raw); err == nil {
			t.Errorf("NormalizeHub(%q) accepted", raw)
		}
	}
}

func TestPath(t *testing.T) {
	if got := Path(envOf(map[string]string{CredentialsFileEnv: "OFF", "HOME": "/home/a", "AppData": `C:\a`})); got != "" {
		t.Fatalf("off = %q", got)
	}
	if got := Path(envOf(map[string]string{CredentialsFileEnv: "/etc/probe.json"})); got != "/etc/probe.json" {
		t.Fatalf("explicit = %q", got)
	}
	if got := Path(envOf(nil)); got != "" {
		t.Fatalf("no configuration directory = %q", got)
	}
	var env map[string]string
	var want string
	switch runtime.GOOS {
	case "windows":
		env, want = map[string]string{"AppData": `C:\Users\a\AppData\Roaming`}, `C:\Users\a\AppData\Roaming\probe\credentials.json`
	case "darwin":
		env, want = map[string]string{"HOME": "/Users/a"}, "/Users/a/Library/Application Support/probe/credentials.json"
	default:
		env, want = map[string]string{"HOME": "/home/a"}, "/home/a/.config/probe/credentials.json"
	}
	if got := Path(envOf(env)); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func credentialsJSON(c Credentials) string {
	data, _ := json.Marshal(c)
	return string(data)
}

func TestResolve(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	good := Credentials{Hub: "https://hub.example", Endpoint: "https://hub.example/llm/v1", Model: "m", Token: "probe_mcp.x.y", Login: "octocat", ExpiresAt: now.Add(time.Hour)}
	env := map[string]string{CredentialsFileEnv: "/c.json"}

	g, found, err := Resolve(envOf(env), filesOf(map[string]string{"/c.json": credentialsJSON(good)}), now)
	if err != nil || !found || g.Endpoint != good.Endpoint || g.Token != good.Token || g.Model != "m" || !strings.Contains(g.Source, "octocat") {
		t.Fatalf("Resolve = %+v, %v, %v", g, found, err)
	}
	if strings.Contains(g.Source, good.Token) {
		t.Fatal("the source line must never show the token")
	}

	expired := good
	expired.ExpiresAt = now.Add(-time.Second)
	if _, found, err := Resolve(envOf(env), filesOf(map[string]string{"/c.json": credentialsJSON(expired)}), now); found || err != nil {
		t.Fatalf("an expired login must be skipped: %v, %v", found, err)
	}

	elsewhere := good
	elsewhere.Endpoint = "https://attacker.example/llm/v1"
	if _, _, err := Resolve(envOf(env), filesOf(map[string]string{"/c.json": credentialsJSON(elsewhere)}), now); err == nil {
		t.Fatal("a gateway outside the hub must be refused")
	}

	if _, found, err := Resolve(envOf(env), filesOf(nil), now); found || err != nil {
		t.Fatalf("no file: %v, %v", found, err)
	}

	off := map[string]string{CredentialsFileEnv: "off"}
	if _, found, _ := Resolve(envOf(off), filesOf(map[string]string{"off": credentialsJSON(good)}), now); found {
		t.Fatal("PROBE_CREDENTIALS_FILE=off must disable the file")
	}

	token := map[string]string{HubTokenEnv: "probe_mcp.ci.secret", HubURLEnv: "https://hub.example/", CredentialsFileEnv: "/c.json"}
	g, found, err = Resolve(envOf(token), filesOf(map[string]string{"/c.json": credentialsJSON(good)}), now)
	if err != nil || !found || g.Endpoint != "https://hub.example/llm/v1" || g.Token != "probe_mcp.ci.secret" || g.Model != PlaceholderModel {
		t.Fatalf("Resolve with %s = %+v, %v, %v", HubTokenEnv, g, found, err)
	}
	token[HubURLEnv] = "http://hub.example"
	if _, _, err := Resolve(envOf(token), filesOf(nil), now); err == nil {
		t.Fatal("a token must not be sent over plain HTTP")
	}
}

func TestSaveIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe", "credentials.json")
	c := Credentials{Hub: "https://hub.example", Endpoint: "https://hub.example/llm/v1", Model: "m", Token: "t"}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	got, _, found, err := Load(envOf(map[string]string{CredentialsFileEnv: path}), os.ReadFile)
	if err != nil || !found || got.Token != "t" {
		t.Fatalf("Load = %+v, %v, %v", got, found, err)
	}
	if err := Save("", c); err == nil {
		t.Fatal("saving nowhere must fail")
	}
}

// fakeHub serves the device flow: pending, slow_down, then the token.
type fakeHub struct {
	mu      sync.Mutex
	polls   int
	deny    bool
	revoked bool
	server  *httptest.Server
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	base := h.server.URL
	switch r.URL.Path {
	case "/api/device/code":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["client"] != "test client" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "BCDF-GHJK", "verification_uri": base + "/device.html", "verification_uri_complete": base + "/device.html?code=BCDF-GHJK", "expires_in": 600, "interval": 5})
	case "/api/device/token":
		h.polls++
		switch {
		case h.deny:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
		case h.polls == 1:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		case h.polls == 2:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"access_token": "probe_mcp.k.s", "expires_in": 3600, "endpoint": base + "/llm/v1", "model": "served", "login": "octocat", "provider": "github"})
		}
	case "/llm/v1/account":
		if r.Header.Get("Authorization") != "Bearer probe_mcp.k.s" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "provider": "github", "model": "served", "daily_tokens": 1000, "used_today": 10})
	case "/llm/v1/token":
		h.revoked = r.Method == http.MethodDelete && r.Header.Get("Authorization") == "Bearer probe_mcp.k.s"
		json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestLoginFlow(t *testing.T) {
	hub := &fakeHub{}
	hub.server = httptest.NewServer(hub)
	defer hub.server.Close()

	var waits []time.Duration
	client := NewClient()
	client.Wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	var prompt Prompt
	creds, err := client.Login(context.Background(), hub.server.URL, "test client", func(p Prompt) { prompt = p })
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if prompt.UserCode != "BCDF-GHJK" || !strings.HasSuffix(prompt.CompleteURI, "/device.html?code=BCDF-GHJK") {
		t.Fatalf("prompt %+v", prompt)
	}
	if creds.Token != "probe_mcp.k.s" || creds.Endpoint != hub.server.URL+"/llm/v1" || creds.Login != "octocat" || creds.ExpiresAt.IsZero() {
		t.Fatalf("credentials %+v", creds)
	}
	// slow_down added five seconds to the next wait.
	if len(waits) != 3 || waits[0] != 5*time.Second || waits[2] != 10*time.Second {
		t.Fatalf("waits %v", waits)
	}

	account, err := client.Account(context.Background(), creds)
	if err != nil || account.Login != "octocat" || account.UsedToday != 10 {
		t.Fatalf("Account = %+v, %v", account, err)
	}
	if err := client.Revoke(context.Background(), creds); err != nil || !hub.revoked {
		t.Fatalf("Revoke: %v, revoked=%v", err, hub.revoked)
	}
	creds.Token = "other"
	if _, err := client.Account(context.Background(), creds); err != ErrUnauthorized {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestLoginDenied(t *testing.T) {
	hub := &fakeHub{deny: true}
	hub.server = httptest.NewServer(hub)
	defer hub.server.Close()
	client := NewClient()
	client.Wait = func(context.Context, time.Duration) error { return nil }
	if _, err := client.Login(context.Background(), hub.server.URL, "test client", func(Prompt) {}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("Login = %v", err)
	}
}

func TestLoginWithoutGateway(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if _, err := NewClient().Login(context.Background(), server.URL, "test client", func(Prompt) {}); err == nil || !strings.Contains(err.Error(), "does not lend") {
		t.Fatalf("Login = %v", err)
	}
}
