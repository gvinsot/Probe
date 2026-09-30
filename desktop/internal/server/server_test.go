package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/gdrive"
	"github.com/gvinsot/Probe/desktop/internal/instance"
	"github.com/gvinsot/Probe/desktop/internal/watch"
)

type memKeys map[string]string

func (m memKeys) Get(p string) (string, error) { return m[p], nil }
func (m memKeys) Set(p, k string) error {
	if k == "" {
		delete(m, p)
	} else {
		m[p] = k
	}
	return nil
}

type fixture struct {
	srv    *Server
	h      http.Handler
	keys   memKeys
	shown  atomic.Int32
	cookie *http.Cookie
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := watch.New(dir, st.Get, logger)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{keys: memKeys{}}
	f.srv, err = New(Deps{Settings: st, Watcher: w, Keys: f.keys, Log: logger, OnShow: func() { f.shown.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.srv.ln.Close() })
	f.h = f.srv.Handler()
	return f
}

func (f *fixture) do(method, path string, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = fmt.Sprintf("127.0.0.1:%d", f.srv.Port())
	if f.cookie != nil {
		r.AddCookie(f.cookie)
	}
	if method != http.MethodGet {
		r.Header.Set(requestHeader, "1")
	}
	if mutate != nil {
		mutate(r)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func (f *fixture) login(t *testing.T) {
	t.Helper()
	url := f.srv.LaunchURL()
	path := url[strings.Index(url, "/launch"):]
	rec := f.do(http.MethodGet, path, "", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("launch: %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("weak session cookie: %+v", c)
			}
			f.cookie = c
		}
	}
	if f.cookie == nil {
		t.Fatal("no session cookie")
	}
	// The code is single use.
	f.cookie = nil
	if rec := f.do(http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused launch code: %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			f.cookie = c
		}
	}
}

func TestRejectsForeignHost(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	rec := f.do(http.MethodGet, "/api/state", "", func(r *http.Request) { r.Host = "evil.example:80" })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", rec.Code)
	}
}

func TestRequiresSession(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodGet, "/api/state", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("page without session: %d", rec.Code)
	}
	f.login(t)
	rec := f.do(http.MethodGet, "/api/state", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("with session: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, "/", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Probe Desktop") {
		t.Fatalf("index: %d", rec.Code)
	}
}

func TestForgedRequestsAreRejected(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	noHeader := f.do(http.MethodPost, "/api/scan", "", func(r *http.Request) { r.Header.Del(requestHeader) })
	if noHeader.Code != http.StatusForbidden {
		t.Fatalf("missing header: %d", noHeader.Code)
	}
	crossOrigin := f.do(http.MethodPost, "/api/scan", "", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross origin: %d", crossOrigin.Code)
	}
	ok := f.do(http.MethodPost, "/api/scan", "", func(r *http.Request) { r.Header.Set("Origin", f.srv.origin()) })
	if ok.Code != http.StatusAccepted {
		t.Fatalf("same origin: %d", ok.Code)
	}
}

func TestControlToken(t *testing.T) {
	f := newFixture(t)
	bad := f.do(http.MethodPost, "/control/show", "", func(r *http.Request) { r.Header.Set(instance.ControlHeader, "nope") })
	if bad.Code != http.StatusForbidden {
		t.Fatalf("bad control token: %d", bad.Code)
	}
	good := f.do(http.MethodPost, "/control/show", "", func(r *http.Request) { r.Header.Set(instance.ControlHeader, f.srv.ControlToken()) })
	if good.Code != http.StatusNoContent {
		t.Fatalf("control: %d", good.Code)
	}
}

func TestSettingsStoreKeyWithoutExposingIt(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	folder := t.TempDir()
	body, _ := json.Marshal(map[string]any{"sources": []map[string]string{{"type": "folder", "path": folder}}, "provider": "anthropic", "api_key": "sk-ant-secret", "scan_seconds": 30})
	rec := f.do(http.MethodPut, "/api/settings", string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if f.keys["anthropic"] != "sk-ant-secret" {
		t.Fatalf("key not stored: %v", f.keys)
	}
	if strings.Contains(rec.Body.String(), "sk-ant-secret") {
		t.Fatal("settings response leaks the API key")
	}
	var got settingsResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Keys["anthropic"] || got.ScanSeconds != 30 || len(got.Sources) != 1 || got.Sources[0].ID != config.FolderSourceID(folder) {
		t.Fatalf("settings echo: %+v", got)
	}

	bad, _ := json.Marshal(map[string]any{"sources": []map[string]string{{"type": "folder", "path": "relative/path"}}})
	if rec := f.do(http.MethodPut, "/api/settings", string(bad), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("relative folder accepted: %d", rec.Code)
	}
}

// A fresh install has no folder: the interface spreads and maps these lists,
// so they must be JSON arrays, never null.
func TestSettingsListsAreArraysOnFreshInstall(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	rec := f.do(http.MethodGet, "/api/settings", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sources", "cloud_folders", "google_accounts"} {
		if !strings.HasPrefix(string(raw[k]), "[") {
			t.Fatalf("%s = %s, want a JSON array", k, raw[k])
		}
	}
}

type fakeGoogle struct {
	disconnected []string
}

func (g *fakeGoogle) Connect() error                      { return nil }
func (g *fakeGoogle) ConnectStatus() gdrive.ConnectStatus { return gdrive.ConnectStatus{} }
func (g *fakeGoogle) Disconnect(a string) error {
	g.disconnected = append(g.disconnected, a)
	return nil
}
func (g *fakeGoogle) Drives(ctx context.Context, a string) ([]gdrive.Drive, error) {
	return []gdrive.Drive{{Name: "My Drive"}}, nil
}
func (g *fakeGoogle) LookupFolder(ctx context.Context, a, ref string) (gdrive.Folder, error) {
	return gdrive.Folder{ID: ref, Name: "Legal"}, nil
}

// The connected accounts only change through the connection flow: the
// interface cannot add one, and an account in use cannot be disconnected.
func TestGoogleAccounts(t *testing.T) {
	f := newFixture(t)
	g := &fakeGoogle{}
	f.srv.deps.Google = g
	f.login(t)
	if err := f.srv.deps.Settings.Update(func(s *config.Settings) { s.GoogleAccounts = []string{"me@example.com"} }); err != nil {
		t.Fatal(err)
	}

	forged, _ := json.Marshal(map[string]any{"google_accounts": []string{"intruder@example.com"}})
	if rec := f.do(http.MethodPut, "/api/settings", string(forged), nil); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if got := f.srv.deps.Settings.Get().GoogleAccounts; len(got) != 1 || got[0] != "me@example.com" {
		t.Fatalf("accounts changed by the interface: %v", got)
	}

	src, _ := json.Marshal(map[string]any{"sources": []map[string]string{{"type": "gdrive", "account": "me@example.com", "folder_id": "abc", "folder_name": "Legal"}}})
	if rec := f.do(http.MethodPut, "/api/settings", string(src), nil); rec.Code != http.StatusOK {
		t.Fatalf("drive source: %d %s", rec.Code, rec.Body)
	}
	unknown, _ := json.Marshal(map[string]any{"sources": []map[string]string{{"type": "gdrive", "account": "other@example.com"}}})
	if rec := f.do(http.MethodPut, "/api/settings", string(unknown), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("source of an unknown account: %d", rec.Code)
	}

	if rec := f.do(http.MethodDelete, "/api/google/accounts/me@example.com", "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("disconnect an account in use: %d", rec.Code)
	}
	f.do(http.MethodPut, "/api/settings", `{"sources":[]}`, nil)
	if rec := f.do(http.MethodDelete, "/api/google/accounts/me@example.com", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body)
	}
	if len(g.disconnected) != 1 || len(f.srv.deps.Settings.Get().GoogleAccounts) != 0 {
		t.Fatalf("disconnected %v, accounts %v", g.disconnected, f.srv.deps.Settings.Get().GoogleAccounts)
	}
	if rec := f.do(http.MethodGet, "/api/google/drives?account=me@example.com", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("drives of a disconnected account: %d", rec.Code)
	}
}

func TestGoogleUnavailable(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	if rec := f.do(http.MethodPost, "/api/google/connect", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("connect without Google: %d", rec.Code)
	}
}

func TestGoogleLink(t *testing.T) {
	for link, want := range map[string]bool{
		"https://docs.google.com/document/d/1/edit": true,
		"https://drive.google.com/file/d/1/view":    true,
		"http://docs.google.com/document/d/1":       false,
		"https://evil.example/docs.google.com":      false,
		"file:///C:/Windows/System32/calc.exe":      false,
		"https://user@docs.google.com/x":            false,
	} {
		if got := googleLink(link); got != want {
			t.Errorf("googleLink(%q) = %v, want %v", link, got, want)
		}
	}
}

func TestReviewedHistory(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodGet, "/api/reviewed", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("history without session: %d", rec.Code)
	}
	f.login(t)
	rec := f.do(http.MethodGet, "/api/reviewed", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty history: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, "/api/reviewed/unknown", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown review: %d", rec.Code)
	}
}
