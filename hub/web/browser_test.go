package web

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCommitGraphInBrowser(t *testing.T) {
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("Chromium is required for the dashboard browser test")
	}
	assets, err := fs.Sub(Assets, "public")
	if err != nil {
		t.Fatal(err)
	}
	html, err := fs.ReadFile(assets, "app.html")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/browser.js")
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Replace(string(html), `<script src="app.js"`, `<script src="fixture.js" defer></script><script src="app.js"`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'")
		switch r.URL.Path {
		case "/app.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(page))
		case "/fixture.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(fixture)
		default:
			http.FileServer(http.FS(assets)).ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chromium, "--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--window-size=1440,900", "--user-data-dir="+t.TempDir(), "--dump-dom", "--virtual-time-budget=5000", server.URL+"/app.html")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("Chromium: %v", err)
	}
	if !strings.Contains(string(output), `data-test-result="PASS"`) {
		t.Fatalf("dashboard test did not pass:\n%s", output)
	}
}
