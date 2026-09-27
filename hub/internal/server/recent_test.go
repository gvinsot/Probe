package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

func TestRepoListScaleSizeAndCompression(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(func(r *store.Repo) { r.HasPolicy = true })
	dir := filepath.Join(h.cfg.DataDir, "reports", h.userKey, repo.Key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Exercise a legacy store with >1000 artifacts and oversized optional fields.
	for i := 0; i < 1005; i++ {
		rec := store.Record{UserKey: h.userKey, RepoKey: repo.Key, Run: store.Run{Commit: fmt.Sprintf("commit-%06d", i), Status: store.StatusFailed, QueuedAt: now.Add(-time.Duration(i) * time.Minute), Message: strings.Repeat("m", 8192), Author: strings.Repeat("a", 8192), Intent: strings.Repeat("i", 8192), Error: "git fetch: secret-token"}, Raw: json.RawMessage(`{"payload":"` + strings.Repeat("x", 4096) + `"}`)}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rec.Commit+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			repo.Latest = &rec.Run
		}
	}
	if err := h.store.PutRepo(h.userKey, repo); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	h.store, h.server.store = reopened, reopened
	// Even the first HTTP request after restart needs no report-directory access.
	if err := os.Rename(dir, dir+"-offline"); err != nil {
		t.Fatal(err)
	}
	plain := h.do(http.MethodGet, "/api/repos", nil)
	if plain.Code != http.StatusOK {
		t.Fatalf("status=%d", plain.Code)
	}
	if plain.Body.Len() > 128<<10 {
		t.Fatalf("uncompressed listing grew to %d bytes", plain.Body.Len())
	}
	var payload struct {
		Repos []store.PublicRepo `json:"repos"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Repos) != 1 || len(payload.Repos[0].Recent) != store.MaxRecent || !payload.Repos[0].RecentIncomplete {
		t.Fatalf("unexpected bounded listing: %d repositories", len(payload.Repos))
	}
	for _, key := range []string{`"error":`, `"author":`, `"message":`, `"intent":`, `"raw":`, "secret-token"} {
		if strings.Contains(plain.Body.String(), key) {
			t.Errorf("listing exposes %s", key)
		}
	}
	for _, accept := range []string{"gzip", "br, gzip;q=0.7", "*;q=0.5", "gzip;q=0", "*;q=1, gzip;q=0", "br", "gzip;q=invalid"} {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		req.AddCookie(h.cookie)
		req.Header.Set("Accept-Encoding", accept)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("headers: %d %v", w.Code, w.Header())
		}
		compressed := accept == "gzip" || accept == "br, gzip;q=0.7" || accept == "*;q=0.5"
		body := w.Body.Bytes()
		if compressed {
			if w.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("no gzip for %q", accept)
			}
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			zr.Close()
			if w.Body.Len() >= plain.Body.Len() {
				t.Fatal("gzip did not reduce listing size")
			}
		} else if w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("unexpected compression for %q", accept)
		}
		if !bytes.Equal(body, plain.Body.Bytes()) {
			t.Fatal("compression changed the response")
		}
	}
	runs := h.decode(h.do(http.MethodGet, "/api/repos/"+repo.Key+"/runs?limit=0", nil))["runs"].([]any)
	if len(runs) != store.MaxHistory {
		t.Fatalf("limit=0 bypassed HTTP cap: %d", len(runs))
	}
}

func TestLegacyReportErrorsDoNotLeakThroughAPIs(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	repo := h.addRepo(func(r *store.Repo) { r.HasPolicy = true })
	rec := store.Record{UserKey: h.userKey, RepoKey: repo.Key, Run: store.Run{Commit: strings.Repeat("a", 40), Status: store.StatusFailed, QueuedAt: time.Now(), Error: "git fetch: Authorization: Basic secret-token"}, Raw: json.RawMessage(storedReport)}
	dir := filepath.Join(h.cfg.DataDir, "reports", h.userKey, repo.Key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.Commit+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	h.store, h.server.store = reopened, reopened
	for _, path := range []string{"/api/repos", "/api/repos/" + repo.Key + "/runs", "/api/repos/" + repo.Key + "/reports/" + rec.Commit} {
		w := h.do(http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret-token") {
			t.Fatalf("%s leaks an old diagnostic", path)
		}
	}
}
