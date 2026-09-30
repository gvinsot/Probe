package watch

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
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

func newWatcher(t *testing.T, folder string) *Watcher {
	t.Helper()
	s := config.Defaults()
	s.Folders = []string{folder}
	w, err := New(t.TempDir(), func() config.Settings { return s }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func only(t *testing.T, w *Watcher) Document {
	t.Helper()
	st := w.State()
	if len(st.Documents) != 1 {
		t.Fatalf("%d documents, want 1: %+v", len(st.Documents), st.Documents)
	}
	d, _ := w.Document(st.Documents[0].ID)
	return d
}

func TestLifecycle(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "contract.docx")
	t0 := time.Now().Add(-time.Hour)
	writeDocx(t, path, "Payment is due within 30 days.", t0)
	os.WriteFile(filepath.Join(folder, "~$contract.docx"), []byte("lock"), 0o600)
	os.WriteFile(filepath.Join(folder, "notes.txt"), []byte("ignored"), 0o600)

	w := newWatcher(t, folder)
	w.Scan()
	if d := only(t, w); d.Status != StatusClean || d.BaselineHash == "" {
		t.Fatalf("first scan: status %s, baseline %q", d.Status, d.BaselineHash)
	}

	writeDocx(t, path, "Payment is due within 90 days.", t0.Add(time.Minute))
	w.Scan()
	d := only(t, w)
	if d.Status != StatusChanged || d.Report == nil || len(d.Report.Findings) == 0 {
		t.Fatalf("after edit: status %s, report %+v", d.Status, d.Report)
	}

	// A new edit after the report: accepting must not approve unseen content.
	writeDocx(t, path, "Payment is due within 120 days.", t0.Add(2*time.Minute))
	if err := w.Accept(d.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("Accept on a stale report = %v, want ErrStale", err)
	}
	w.Scan()
	if err := w.Accept(d.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if d := only(t, w); d.Status != StatusClean || d.Report != nil {
		t.Fatalf("after accept: status %s", d.Status)
	}

	os.Remove(path)
	w.Scan()
	if d := only(t, w); d.Status != StatusRemoved {
		t.Fatalf("after delete: status %s", d.Status)
	}
	if err := w.Accept(d.ID); err != nil {
		t.Fatal(err)
	}
	if st := w.State(); st.Total != 0 {
		t.Fatalf("removed document still listed: %+v", st.Documents)
	}
}

func TestStatePersists(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "a.docx")
	writeDocx(t, path, "One", time.Now().Add(-time.Hour))
	dir := t.TempDir()
	s := config.Defaults()
	s.Folders = []string{folder}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, _ := New(dir, func() config.Settings { return s }, logger)
	w.Scan()
	writeDocx(t, path, "Two", time.Now())

	w2, err := New(dir, func() config.Settings { return s }, logger)
	if err != nil {
		t.Fatal(err)
	}
	w2.Scan()
	if d := only(t, w2); d.Status != StatusChanged {
		t.Fatalf("reloaded watcher: status %s, want changed", d.Status)
	}
}

func TestUnwatchedFolderIsForgotten(t *testing.T) {
	folder := t.TempDir()
	writeDocx(t, filepath.Join(folder, "a.docx"), "One", time.Now())
	s := config.Defaults()
	s.Folders = []string{folder}
	w, _ := New(t.TempDir(), func() config.Settings { return s }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Scan()
	s.Folders = nil
	w.Scan()
	if st := w.State(); st.Total != 0 {
		t.Fatalf("documents of an unwatched folder kept: %d", st.Total)
	}
}

func TestExplanationRaisesSeverityNeverLowers(t *testing.T) {
	d := Document{Status: StatusChanged, Report: &office.Report{Severity: office.Medium}}
	d.Explanation = &Explanation{Severity: office.Critical}
	if got := d.Severity(); got != office.Critical {
		t.Errorf("raised severity = %s, want critical", got)
	}
	d.Report.Severity, d.Explanation.Severity = office.High, office.Low
	if got := d.Severity(); got != office.High {
		t.Errorf("lowered severity = %s, want high", got)
	}
	d.Explanation.Severity = ""
	if got := d.Severity(); got != office.High {
		t.Errorf("no escalation = %s, want high", got)
	}
}
