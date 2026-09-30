package server

import (
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
	body, _ := json.Marshal(map[string]any{"folders": []string{folder}, "provider": "anthropic", "api_key": "sk-ant-secret", "scan_seconds": 30})
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
	if !got.Keys["anthropic"] || got.ScanSeconds != 30 || len(got.Folders) != 1 {
		t.Fatalf("settings echo: %+v", got)
	}

	bad, _ := json.Marshal(map[string]any{"folders": []string{"relative/path"}})
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
	for _, k := range []string{"folders", "cloud_folders"} {
		if !strings.HasPrefix(string(raw[k]), "[") {
			t.Fatalf("%s = %s, want a JSON array", k, raw[k])
		}
	}
}
