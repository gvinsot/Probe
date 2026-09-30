package watch

import (
	"archive/zip"
	"bytes"
	"context"
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

// docxBytes builds a minimal Word document. The comment changes the bytes
// without changing the content, like two exports of the same version.
func docxBytes(text, comment string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, part := range []struct{ name, content string }{
		{"[Content_Types].xml", `<Types/>`},
		{"word/document.xml", `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`},
	} {
		w, _ := zw.Create(part.name)
		w.Write([]byte(part.content))
	}
	zw.SetComment(comment)
	zw.Close()
	return buf.Bytes()
}

func writeDocx(t *testing.T, path, text string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(path, docxBytes(text, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func folderSettings(folders ...string) config.Settings {
	s := config.Defaults()
	for _, f := range folders {
		s.Sources = append(s.Sources, config.Source{Type: config.SourceFolder, Path: f})
	}
	s.Normalize()
	return s
}

func newWatcher(t *testing.T, folder string) *Watcher {
	t.Helper()
	s := folderSettings(folder)
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
	if err := w.Accept(context.Background(), d.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("Accept on a stale report = %v, want ErrStale", err)
	}
	w.Scan()
	if err := w.Accept(context.Background(), d.ID); err != nil {
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
	if err := w.Accept(context.Background(), d.ID); err != nil {
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
	s := folderSettings(folder)
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
	s := folderSettings(folder)
	w, _ := New(t.TempDir(), func() config.Settings { return s }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Scan()
	s.Sources = nil
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

func TestExplanationCarriedOverUnchangedElements(t *testing.T) {
	amount := office.Change{Kind: "modified", Location: "Paragraph 2", Before: "10 000 €", After: "1 000 €"}
	delay := office.Change{Kind: "modified", Location: "Paragraph 5", Before: "30 days", After: "90 days"}
	prev := &office.Report{Changes: []office.Change{amount, delay}}
	e := &Explanation{
		Text: "The amount and the delay changed.", Impacts: []string{"financial"}, Severity: office.High,
		Findings: []office.Finding{
			{Title: "Amount divided by ten", Location: "Paragraph 2"},
			{Title: "Longer payment delay", After: "90 days"},
			{Title: "General remark"},
		},
	}

	// Saved again with the same modifications: nothing is lost.
	same := &office.Report{Changes: []office.Change{amount, delay}}
	if got := carryExplanation(e, prev, same); got == nil || got.Outdated || len(got.Findings) != 3 || got.Severity != office.High {
		t.Fatalf("same modifications: %+v", got)
	}

	// A paragraph inserted above: locations shift, contents are unchanged.
	shifted := amount
	shifted.Location = "Paragraph 3"
	added := office.Change{Kind: "added", Location: "Paragraph 1", After: "New clause"}
	got := carryExplanation(e, prev, &office.Report{Changes: []office.Change{added, shifted, delay}})
	if got == nil || !got.Outdated || len(got.Findings) != 3 || got.Severity != office.High {
		t.Fatalf("new change added: %+v", got)
	}

	// The delay modified again: its finding and the impacts are dropped, the
	// finding about the unchanged amount is kept.
	delay2 := delay
	delay2.After = "120 days"
	got = carryExplanation(e, prev, &office.Report{Changes: []office.Change{amount, delay2}})
	if got == nil || !got.Outdated || got.Severity != "" || len(got.Impacts) != 0 {
		t.Fatalf("delay modified again: %+v", got)
	}
	if len(got.Findings) != 1 || got.Findings[0].Title != "Amount divided by ten" {
		t.Fatalf("kept findings = %+v", got.Findings)
	}
	if len(e.Findings) != 3 {
		t.Fatal("the earlier explanation was modified")
	}

	if carryExplanation(nil, prev, same) != nil {
		t.Fatal("no explanation to carry")
	}
}
