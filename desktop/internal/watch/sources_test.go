package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
	"github.com/gvinsot/Probe/desktop/internal/source"
)

// fakeDoc is one document of a fake drive.
type fakeDoc struct {
	text string
	rev  string
	mod  time.Time
}

// fakeSource is a drive held in memory. Exported documents come out with
// different bytes at each read, like a Google Doc exported again.
type fakeSource struct {
	mu       sync.Mutex
	docs     map[string]*fakeDoc
	exported bool
	listErr  error
	reads    int
	exports  int
}

func (f *fakeSource) entry(key string, d *fakeDoc) source.Entry {
	size := int64(len(d.text))
	if f.exported {
		size = -1
	}
	return source.Entry{
		Key: key, Name: key, Folder: "Drive", Location: "Drive/" + key, Link: "https://docs.google.com/" + key,
		Kind: office.Word, Size: size, ModTime: d.mod, Version: d.rev, Revision: d.rev, Exported: f.exported,
	}
}

func (f *fakeSource) List(ctx context.Context, yield func(source.Entry)) error {
	f.mu.Lock()
	if f.listErr != nil {
		defer f.mu.Unlock()
		return f.listErr
	}
	var entries []source.Entry
	for k, d := range f.docs {
		entries = append(entries, f.entry(k, d))
	}
	f.mu.Unlock()
	for _, e := range entries {
		yield(e)
	}
	return nil
}

func (f *fakeSource) content(text string) []byte {
	f.exports++
	if f.exported {
		return docxBytes(text, fmt.Sprintf("export %d", f.exports))
	}
	return docxBytes(text, "")
}

func (f *fakeSource) Read(ctx context.Context, e source.Entry, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[e.Key]
	if !ok {
		return nil, source.ErrNotFound
	}
	f.reads++
	return f.content(d.text), nil
}

func (f *fakeSource) Stat(ctx context.Context, key string) (source.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[key]
	if !ok {
		return source.Entry{}, source.ErrNotFound
	}
	return f.entry(key, d), nil
}

func (f *fakeSource) set(key, text, rev string, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[key] = &fakeDoc{text: text, rev: rev, mod: mod}
}

// historySource also keeps the previous versions.
type historySource struct {
	*fakeSource
	revisions map[string]string // revision → text
}

func (h *historySource) ReadRevision(ctx context.Context, e source.Entry, rev string, max int64) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	text, ok := h.revisions[rev]
	if !ok {
		return nil, source.ErrRevisionGone
	}
	return h.content(text), nil
}

const driveSourceID = "gdrive-0123456789abcdef"

func driveWatcher(t *testing.T, src source.Source) *Watcher {
	t.Helper()
	s := config.Defaults()
	s.GoogleAccounts = []string{"me@example.com"}
	s.Sources = []config.Source{{ID: driveSourceID, Type: config.SourceGoogleDrive, Account: "me@example.com"}}
	w, err := New(t.TempDir(), func() config.Settings { return s }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.SetFactory(func(cfg config.Source) (source.Source, error) { return src, nil })
	return w
}

func TestHistoryRecordsBaselineWithoutDownloading(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	fake := &fakeSource{docs: map[string]*fakeDoc{}}
	h := &historySource{fakeSource: fake, revisions: map[string]string{"r1": "Payment is due within 30 days."}}
	fake.set("contract", "Payment is due within 30 days.", "r1", t0)

	w := driveWatcher(t, h)
	w.Scan()
	d := only(t, w)
	if d.Status != StatusClean || d.BaselineRev != "r1" || d.BaselineHash != "" {
		t.Fatalf("first scan: status %s, rev %q, hash %q", d.Status, d.BaselineRev, d.BaselineHash)
	}
	if fake.reads != 0 {
		t.Fatalf("first scan downloaded %d documents, want none", fake.reads)
	}
	if d.Source != driveSourceID || d.Link == "" || d.Folder != "Drive" {
		t.Fatalf("document fields: %+v", d)
	}

	fake.set("contract", "Payment is due within 90 days.", "r2", t0.Add(time.Minute))
	h.revisions["r2"] = "Payment is due within 90 days."
	w.Scan()
	d = only(t, w)
	if d.Status != StatusChanged || d.Report == nil || len(d.Report.Findings) == 0 {
		t.Fatalf("after edit: status %s, error %q, report %+v", d.Status, d.Error, d.Report)
	}
	if d.BaselineRev != "" || d.BaselineHash == "" {
		t.Fatalf("baseline not downloaded: rev %q, hash %q", d.BaselineRev, d.BaselineHash)
	}
	if err := w.Accept(context.Background(), d.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if d := only(t, w); d.Status != StatusClean {
		t.Fatalf("after accept: status %s", d.Status)
	}
}

func TestHistoryLostBaselineAsksForAFullReading(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	fake := &fakeSource{docs: map[string]*fakeDoc{}}
	h := &historySource{fakeSource: fake, revisions: map[string]string{}}
	fake.set("contract", "One", "r1", t0)
	w := driveWatcher(t, h)
	w.Scan()

	fake.set("contract", "Two", "r2", t0.Add(time.Minute))
	w.Scan()
	d := only(t, w)
	if d.Status != StatusChanged || d.Report == nil || len(d.Report.Findings) != 1 || d.Report.Findings[0].Rule != "source.baseline-unavailable" {
		t.Fatalf("lost baseline: status %s, report %+v", d.Status, d.Report)
	}
	if d.Severity() != office.High {
		t.Fatalf("lost baseline severity %s, want high", d.Severity())
	}
	if err := w.Accept(context.Background(), d.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	d = only(t, w)
	if d.Status != StatusClean || d.BaselineHash == "" || d.BaselineRev != "" {
		t.Fatalf("after accept: %+v", d)
	}
}

func TestExportedDocumentReviewUsesTheAnalyzedCopy(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	fake := &fakeSource{docs: map[string]*fakeDoc{}, exported: true}
	fake.set("sheet", "Payment is due within 30 days.", "", t0)
	w := driveWatcher(t, fake)
	w.Scan()
	if d := only(t, w); d.Status != StatusClean {
		t.Fatalf("first scan: %s %s", d.Status, d.Error)
	}

	// Modified in the drive without a visible change: exported again, the
	// bytes differ but nothing is reported.
	fake.set("sheet", "Payment is due within 30 days.", "", t0.Add(time.Minute))
	w.Scan()
	if d := only(t, w); d.Status != StatusClean {
		t.Fatalf("same content exported again: status %s, report %+v", d.Status, d.Report)
	}

	fake.set("sheet", "Payment is due within 90 days.", "", t0.Add(2*time.Minute))
	w.Scan()
	d := only(t, w)
	if d.Status != StatusChanged {
		t.Fatalf("after edit: %s %s", d.Status, d.Error)
	}
	// Another edit after the report: the review must be refused.
	fake.set("sheet", "Payment is due within 120 days.", "", t0.Add(3*time.Minute))
	if err := w.Accept(context.Background(), d.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("Accept on a stale export = %v, want ErrStale", err)
	}
	w.Scan()
	d = only(t, w)
	// Exporting again gives other bytes: the review relies on the version
	// and approves the copy the report was computed from.
	if err := w.Accept(context.Background(), d.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if d := only(t, w); d.Status != StatusClean {
		t.Fatalf("after accept: %s", d.Status)
	}
	if _, err := os.Stat(w.pendingPath(d.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("analyzed copy kept after the review: %v", err)
	}
}

func TestIncompleteListingKeepsDocuments(t *testing.T) {
	fake := &fakeSource{docs: map[string]*fakeDoc{}}
	fake.set("a", "One", "", time.Now())
	w := driveWatcher(t, fake)
	w.Scan()
	fake.listErr = errors.New("network down")
	fake.docs = map[string]*fakeDoc{}
	w.Scan()
	if d := only(t, w); d.Status != StatusClean {
		t.Fatalf("document of an unavailable source: status %s", d.Status)
	}
	if st := w.State(); st.ScanError == "" {
		t.Fatal("no scan error reported")
	}
	fake.listErr = nil
	w.Scan()
	if d := only(t, w); d.Status != StatusRemoved {
		t.Fatalf("deleted document: status %s", d.Status)
	}
	if st := w.State(); st.ScanError != "" {
		t.Fatalf("scan error kept: %s", st.ScanError)
	}
}

// The state saved by a version that only watched folders keeps its
// documents and their baselines.
func TestLegacyStateIsMigrated(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "a.docx")
	writeDocx(t, path, "One", time.Now().Add(-time.Hour))
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)

	dir := t.TempDir()
	id := docID(path)
	os.MkdirAll(filepath.Join(dir, "baselines"), 0o700)
	os.WriteFile(filepath.Join(dir, "baselines", id), data, 0o600)
	legacy := fmt.Sprintf(`[{"id":%q,"path":%q,"root":%q,"kind":"word","status":"clean","size":%d,"mod_time":%q,"baseline_hash":%q,"current_hash":%q}]`,
		id, path, folder, info.Size(), info.ModTime().Format(time.RFC3339Nano), hashOf(data), hashOf(data))
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600)

	s := folderSettings(folder)
	w, err := New(dir, func() config.Settings { return s }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	writeDocx(t, path, "Two", time.Now())
	w.Scan()
	d := only(t, w)
	if d.ID != id || d.Source != config.FolderSourceID(folder) || d.Name != "a.docx" {
		t.Fatalf("migrated document: %+v", d)
	}
	if d.Status != StatusChanged {
		t.Fatalf("change against the legacy baseline: status %s %s", d.Status, d.Error)
	}
}

func TestDocumentIDsAreScopedBySource(t *testing.T) {
	a := config.Source{ID: "gdrive-0000000000000001", Type: config.SourceGoogleDrive}
	b := config.Source{ID: "gdrive-0000000000000002", Type: config.SourceGoogleDrive}
	if documentID(a, "file") == documentID(b, "file") {
		t.Fatal("two drive sources share a document id")
	}
	folder := config.Source{Type: config.SourceFolder, Path: "/x"}
	if documentID(folder, "/x/a.docx") != docID("/x/a.docx") {
		t.Fatal("folder document ids changed")
	}
}
