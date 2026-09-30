package server

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/watch"
)

func writeDocx(t *testing.T, path, text string, when time.Time) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"word/document.xml":   `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`,
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestChangesAreExplainedAutomatically(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"explanation\":\"The delay was tripled.\"}"}}]}`))
	}))
	defer provider.Close()

	folder, dir := t.TempDir(), t.TempDir()
	path := filepath.Join(folder, "contract.docx")
	t0 := time.Now().Add(-time.Hour)
	writeDocx(t, path, "Payment is due within 30 days.", t0)

	st, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Get()
	s.Sources = []config.Source{{Type: config.SourceFolder, Path: folder}}
	s.Provider, s.BaseURL = config.ProviderOpenAI, provider.URL+"/v1"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := watch.New(dir, st.Get, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Deps{Settings: st, Watcher: w, Keys: memKeys{}, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.ln.Close()
	defer srv.Shutdown(t.Context())
	w.OnScanned(srv.Kick)

	explained := func() *watch.Explanation {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, sum := range w.State().Documents {
				if d, _ := w.Document(sum.ID); d.Explanation != nil && len(srv.auto.running()) == 0 {
					return d.Explanation
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	w.Scan() // baseline: nothing to explain
	writeDocx(t, path, "Payment is due within 90 days.", t0.Add(time.Minute))
	w.Scan()
	e := explained()
	if e == nil || e.Text != "The delay was tripled." || calls.Load() != 1 {
		t.Fatalf("explanation %+v after %d calls", e, calls.Load())
	}

	// Explained already: another scan does not call the provider again.
	w.Scan()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("%d calls, want 1", calls.Load())
	}

	// A failed version is not tried again at every scan.
	fail.Store(true)
	writeDocx(t, path, "Payment is due within 120 days.", t0.Add(2*time.Minute))
	w.Scan()
	time.Sleep(100 * time.Millisecond)
	w.Scan()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("%d calls after a failure, want 2", calls.Load())
	}
}
