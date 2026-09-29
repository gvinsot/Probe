// Package watch follows the documents of the watched folders.
//
// Each document has a baseline: the last version a person reviewed (or the
// version found when the folder was first added). A scan compares every
// modified document with its baseline and keeps the report until someone
// marks the new version as reviewed, which makes it the next baseline.
//
// This is the local folder source. It reads the folders synchronized by the
// OneDrive and Google Drive clients; the cloud version history is a later
// source that will feed the same reports.
package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
)

// Status of a document relative to its baseline.
type Status string

const (
	// StatusClean: the document matches its reviewed baseline.
	StatusClean Status = "clean"
	// StatusChanged: the document differs from its baseline; a report is ready.
	StatusChanged Status = "changed"
	// StatusRemoved: the document disappeared since its baseline.
	StatusRemoved Status = "removed"
	// StatusCloudOnly: the file is not downloaded and has no baseline yet.
	StatusCloudOnly Status = "cloud-only"
	// StatusTooLarge: the file exceeds the configured size limit.
	StatusTooLarge Status = "too-large"
	// StatusError: the document could not be read or compared.
	StatusError Status = "error"
)

// Explanation is the optional AI reading of a report.
type Explanation struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
}

// Document is the state of one watched file.
type Document struct {
	ID           string         `json:"id"`
	Path         string         `json:"path"`
	Root         string         `json:"root"`
	Kind         office.Kind    `json:"kind"`
	Status       Status         `json:"status"`
	Size         int64          `json:"size"`
	ModTime      time.Time      `json:"mod_time"`
	BaselineHash string         `json:"baseline_hash,omitempty"`
	BaselineAt   time.Time      `json:"baseline_at,omitempty"`
	CurrentHash  string         `json:"current_hash,omitempty"`
	ChangedAt    time.Time      `json:"changed_at,omitempty"`
	Report       *office.Report `json:"report,omitempty"`
	Explanation  *Explanation   `json:"explanation,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// NeedsReview reports whether the document waits for a person.
func (d *Document) NeedsReview() bool {
	return d.Status == StatusChanged || d.Status == StatusRemoved
}

// Severity is the report severity, or "high" for a removed document.
func (d *Document) Severity() string {
	switch {
	case d.Status == StatusRemoved:
		return office.High
	case d.Report != nil:
		return d.Report.Severity
	}
	return office.None
}

// Summary is a document without its report, for lists.
type Summary struct {
	ID         string      `json:"id"`
	Path       string      `json:"path"`
	Name       string      `json:"name"`
	Folder     string      `json:"folder"`
	Root       string      `json:"root"`
	Kind       office.Kind `json:"kind"`
	Status     Status      `json:"status"`
	Severity   string      `json:"severity"`
	Findings   int         `json:"findings"`
	ModTime    time.Time   `json:"mod_time"`
	ChangedAt  time.Time   `json:"changed_at,omitempty"`
	BaselineAt time.Time   `json:"baseline_at,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// State is what the interface shows about the watcher.
type State struct {
	Scanning  bool      `json:"scanning"`
	LastScan  time.Time `json:"last_scan,omitempty"`
	ScanError string    `json:"scan_error,omitempty"`
	Total     int       `json:"total"`
	ToReview  int       `json:"to_review"`
	Documents []Summary `json:"documents"`
}

// Watcher scans the folders and keeps the document states.
type Watcher struct {
	dir      string
	settings func() config.Settings
	log      *slog.Logger
	onUpdate func(total, toReview int)

	mu        sync.Mutex
	docs      map[string]*Document
	scanning  bool
	lastScan  time.Time
	scanError string

	trigger chan struct{}
	scanMu  sync.Mutex // one scan at a time
}

// New loads the saved state of a data directory.
func New(dir string, settings func() config.Settings, log *slog.Logger) (*Watcher, error) {
	w := &Watcher{
		dir:      dir,
		settings: settings,
		log:      log,
		docs:     map[string]*Document{},
		trigger:  make(chan struct{}, 1),
		onUpdate: func(int, int) {},
	}
	if err := os.MkdirAll(w.baselineDir(), 0o700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(w.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var docs []*Document
		if err := json.Unmarshal(data, &docs); err != nil {
			// A corrupt state only costs the pending reports: baselines are
			// captured again on the next scan.
			log.Error("state.json unreadable, starting from scratch", "err", err)
		}
		for _, d := range docs {
			w.docs[d.ID] = d
		}
	}
	return w, nil
}

// OnUpdate registers a callback run after each scan and review, used by the
// tray icon to show the number of documents to review.
func (w *Watcher) OnUpdate(fn func(total, toReview int)) { w.onUpdate = fn }

func (w *Watcher) statePath() string   { return filepath.Join(w.dir, "state.json") }
func (w *Watcher) baselineDir() string { return filepath.Join(w.dir, "baselines") }
func (w *Watcher) baselinePath(id string) string {
	return filepath.Join(w.baselineDir(), id)
}

// Run scans at the configured interval until the stop channel closes.
func (w *Watcher) Run(stop <-chan struct{}) {
	for {
		w.Scan()
		delay := time.Duration(w.settings().ScanSeconds) * time.Second
		select {
		case <-stop:
			return
		case <-w.trigger:
		case <-time.After(delay):
		}
	}
}

// ScanNow asks the loop to scan without waiting for the interval.
func (w *Watcher) ScanNow() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Scan walks every watched folder once.
func (w *Watcher) Scan() {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("scan panic", "panic", r)
		}
	}()
	s := w.settings()
	w.mu.Lock()
	w.scanning = true
	w.mu.Unlock()
	started := time.Now()

	roots := map[string]bool{}
	for _, r := range s.Folders {
		roots[r] = true
	}
	seen := map[string]bool{}
	completed := map[string]bool{}
	var problems []string
	processed := 0
	for _, root := range s.Folders {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return err
				}
				return nil // an unreadable sub-folder does not stop the scan
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") || strings.EqualFold(name, "$RECYCLE.BIN")) {
					return filepath.SkipDir
				}
				return nil
			}
			// Lock and temporary files of Office and of the sync clients.
			if strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".~") || strings.HasPrefix(name, "._") || strings.HasPrefix(name, "~") {
				return nil
			}
			kind := office.KindOf(name)
			if kind == "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			id := docID(path)
			seen[id] = true
			w.process(id, root, path, kind, info, s)
			processed++
			if processed%200 == 0 {
				w.save()
			}
			return nil
		})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", root, err))
			w.log.Warn("folder unavailable", "folder", root, "err", err)
			continue
		}
		completed[root] = true
	}

	w.mu.Lock()
	for id, d := range w.docs {
		switch {
		case !roots[d.Root]:
			// The folder is no longer watched: forget its documents.
			delete(w.docs, id)
			os.Remove(w.baselinePath(id))
		case completed[d.Root] && !seen[id] && d.Status != StatusRemoved:
			if d.BaselineHash == "" {
				delete(w.docs, id)
				continue
			}
			d.Status, d.ChangedAt, d.Report, d.Explanation, d.Error = StatusRemoved, time.Now(), nil, nil, ""
		}
	}
	w.scanning = false
	w.lastScan = time.Now()
	w.scanError = strings.Join(problems, "; ")
	w.mu.Unlock()
	w.save()
	total, toReview := w.counts()
	w.log.Info("scan done", "documents", total, "to_review", toReview, "duration", time.Since(started).Round(time.Millisecond))
	w.onUpdate(total, toReview)
}

func (w *Watcher) counts() (total, toReview int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range w.docs {
		total++
		if d.NeedsReview() {
			toReview++
		}
	}
	return total, toReview
}

// process brings one document up to date. It never holds the lock while
// reading the file: a large document or a slow cloud download must not
// freeze the interface.
func (w *Watcher) process(id, root, path string, kind office.Kind, info fs.FileInfo, s config.Settings) {
	w.mu.Lock()
	prev, known := w.docs[id]
	var d Document
	if known {
		d = *prev
		if d.Size == info.Size() && d.ModTime.Equal(info.ModTime()) && d.Status != StatusError && d.Status != StatusRemoved {
			w.mu.Unlock()
			return
		}
	} else {
		d = Document{ID: id}
	}
	w.mu.Unlock()

	d.Path, d.Root, d.Kind = path, root, kind
	commit := func() {
		w.mu.Lock()
		w.docs[id] = &d
		w.mu.Unlock()
	}
	stamp := func() { d.Size, d.ModTime = info.Size(), info.ModTime() }

	if d.BaselineHash == "" && cloudOnly(info) && !s.DownloadCloudFiles {
		stamp()
		d.Status, d.Error = StatusCloudOnly, ""
		commit()
		return
	}
	if info.Size() > int64(s.MaxFileMB)<<20 {
		stamp()
		d.Status, d.Error = StatusTooLarge, fmt.Sprintf("larger than %d MB", s.MaxFileMB)
		commit()
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Size and time are left as they were so the next scan retries.
		d.Status, d.Error = StatusError, "cannot read the file: "+err.Error()
		commit()
		return
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	switch {
	case d.BaselineHash == "":
		if err := w.writeBaseline(id, data); err != nil {
			d.Status, d.Error = StatusError, "cannot store the baseline: "+err.Error()
			commit()
			return
		}
		stamp()
		d.BaselineHash, d.BaselineAt, d.CurrentHash = hash, time.Now(), hash
		d.Status, d.Report, d.Explanation, d.Error = StatusClean, nil, nil, ""
	case hash == d.BaselineHash:
		stamp()
		d.CurrentHash, d.Status, d.Report, d.Explanation, d.Error = hash, StatusClean, nil, nil, ""
	case hash == d.CurrentHash && d.Report != nil:
		// Touched (synchronized, opened and saved) without new content.
		stamp()
		d.Status, d.Error = StatusChanged, ""
	default:
		baseline, err := os.ReadFile(w.baselinePath(id))
		if err != nil {
			d.Status, d.Error = StatusError, "baseline missing: "+err.Error()
			commit()
			return
		}
		report, err := office.Compare(kind, baseline, data)
		if err != nil {
			// Most often the file is still being written or synchronized:
			// keep the previous size and time so the next scan tries again.
			d.Status, d.Error = StatusError, err.Error()
			commit()
			return
		}
		stamp()
		d.CurrentHash, d.Report, d.Explanation, d.Error = hash, report, nil, ""
		d.Status, d.ChangedAt = StatusChanged, time.Now()
	}
	commit()
}

func (w *Watcher) writeBaseline(id string, data []byte) error {
	return config.WriteFileAtomic(w.baselinePath(id), data)
}

// ErrStale reports a review of a report that no longer matches the file.
var ErrStale = errors.New("the document changed again since this report; it is being analyzed again")

// ErrNotFound reports an unknown document id.
var ErrNotFound = errors.New("unknown document")

// Accept marks the current version of a document as reviewed: it becomes
// the baseline for the next changes. The file is read again and must still
// be the version the report describes, so nobody approves unseen content.
func (w *Watcher) Accept(id string) error {
	w.mu.Lock()
	d, ok := w.docs[id]
	if !ok {
		w.mu.Unlock()
		return ErrNotFound
	}
	if d.Status == StatusRemoved {
		delete(w.docs, id)
		w.mu.Unlock()
		os.Remove(w.baselinePath(id))
		w.afterReview()
		return nil
	}
	path, expected := d.Path, d.CurrentHash
	w.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		w.ScanNow()
		return ErrStale
	}
	if err := w.writeBaseline(id, data); err != nil {
		return err
	}
	w.mu.Lock()
	if d, ok := w.docs[id]; ok {
		d.BaselineHash, d.BaselineAt = expected, time.Now()
		d.Status, d.Report, d.Explanation, d.Error = StatusClean, nil, nil, ""
	}
	w.mu.Unlock()
	w.afterReview()
	return nil
}

func (w *Watcher) afterReview() {
	w.save()
	total, toReview := w.counts()
	w.onUpdate(total, toReview)
}

// SetExplanation stores the AI explanation of a report, unless the document
// changed while the model was answering.
func (w *Watcher) SetExplanation(id, forHash string, e Explanation) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.docs[id]
	if !ok {
		return ErrNotFound
	}
	if d.CurrentHash != forHash || d.Report == nil {
		return ErrStale
	}
	d.Explanation = &e
	go w.save()
	return nil
}

// Document returns a copy of a document with its report.
func (w *Watcher) Document(id string) (Document, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.docs[id]
	if !ok {
		return Document{}, false
	}
	return *d, true
}

// State returns the document list and the scan status.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := State{Scanning: w.scanning, LastScan: w.lastScan, ScanError: w.scanError, Documents: []Summary{}}
	for _, d := range w.docs {
		st.Total++
		if d.NeedsReview() {
			st.ToReview++
		}
		sum := Summary{
			ID: d.ID, Path: d.Path, Name: filepath.Base(d.Path), Root: d.Root,
			Folder: relFolder(d.Root, d.Path), Kind: d.Kind, Status: d.Status,
			Severity: d.Severity(), ModTime: d.ModTime, ChangedAt: d.ChangedAt,
			BaselineAt: d.BaselineAt, Error: d.Error,
		}
		if d.Report != nil {
			sum.Findings = len(d.Report.Findings)
		}
		st.Documents = append(st.Documents, sum)
	}
	sort.Slice(st.Documents, func(i, j int) bool {
		a, b := st.Documents[i], st.Documents[j]
		if ra, rb := office.Rank(a.Severity), office.Rank(b.Severity); ra != rb {
			return ra > rb
		}
		if !a.ChangedAt.Equal(b.ChangedAt) {
			return a.ChangedAt.After(b.ChangedAt)
		}
		return strings.ToLower(a.Path) < strings.ToLower(b.Path)
	})
	return st
}

func relFolder(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return filepath.Base(root)
	}
	return filepath.Join(filepath.Base(root), rel)
}

func (w *Watcher) save() {
	w.mu.Lock()
	docs := make([]*Document, 0, len(w.docs))
	for _, d := range w.docs {
		c := *d
		docs = append(docs, &c)
	}
	w.mu.Unlock()
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	data, err := json.Marshal(docs)
	if err != nil {
		w.log.Error("encode state", "err", err)
		return
	}
	if err := config.WriteFileAtomic(w.statePath(), data); err != nil {
		w.log.Error("save state", "err", err)
	}
}

// docID derives a stable id from the path. Windows and macOS file systems
// ignore case, so the path is folded there.
func docID(path string) string {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:10])
}
