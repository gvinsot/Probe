// Package watch follows the documents of the watched sources.
//
// Each document has a baseline: the last version a person reviewed (or the
// version found when the source was first added). A scan compares every
// modified document with its baseline and keeps the report until someone
// marks the new version as reviewed, which makes it the next baseline. The
// latest reviews keep their report, so a person can read again what they
// approved.
//
// The documents come from sources (package source): the folders of this
// computer, or a Google Drive read through its API. A source that keeps the
// version history records a baseline by reference and the watcher downloads
// it only once the document changed.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/msg"
	"github.com/gvinsot/Probe/desktop/internal/office"
	"github.com/gvinsot/Probe/desktop/internal/source"
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
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Text     string `json:"text"`
	// Findings are the extra risks the model raised. They are shown apart
	// from the report and do not change its severity.
	Findings []office.Finding `json:"findings,omitempty"`
	// Readings qualify the rule findings of the report: a title in the
	// words of the document and the consistency of the change.
	Readings []Reading `json:"readings,omitempty"`
	// Impacts are the legal or financial consequences the model stated, and
	// Severity the level they raise the document to: the model can raise the
	// severity of the report, never lower it.
	Impacts  []string  `json:"impacts,omitempty"`
	Severity string    `json:"severity,omitempty"`
	At       time.Time `json:"at"`
	// Outdated marks an explanation carried over from an earlier version of
	// the modifications: its findings about elements modified again were
	// dropped, and it does not cover the newest changes.
	Outdated bool `json:"outdated,omitempty"`
}

// Reading is the AI reading of the rule finding at index Finding of the
// report; Rule and Location identify that finding, so a reading is never
// shown against another one.
type Reading struct {
	Finding     int    `json:"finding"`
	Rule        string `json:"rule"`
	Location    string `json:"location,omitempty"`
	Title       string `json:"title"`
	Consistency string `json:"consistency,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Document is the state of one watched document.
type Document struct {
	ID string `json:"id"`
	// Source is the id of the configured source, Key the id of the document
	// within it (the file path of a folder, the file id of a drive).
	Source string `json:"source"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Folder string `json:"folder"`
	// Path is the full location shown to the user: a file path, or the
	// place of the document in its drive.
	Path string `json:"path"`
	// Link opens a document that has no local path.
	Link string `json:"link,omitempty"`
	// Root is the watched folder, as saved by the versions that only
	// watched folders; it is only read to migrate their state.
	Root     string      `json:"root,omitempty"`
	Kind     office.Kind `json:"kind"`
	Status   Status      `json:"status"`
	Size     int64       `json:"size"`
	ModTime  time.Time   `json:"mod_time"`
	Version  string      `json:"version,omitempty"`
	Exported bool        `json:"exported,omitempty"`
	// BaselineHash is the hash of the local copy of the reviewed version.
	// BaselineRev references the reviewed version in the history of the
	// source instead, while it has not been downloaded.
	BaselineHash string         `json:"baseline_hash,omitempty"`
	BaselineRev  string         `json:"baseline_rev,omitempty"`
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

// Severity is the report severity, raised by the impacts the AI explanation
// stated, or "high" for a removed document.
func (d *Document) Severity() string {
	switch {
	case d.Status == StatusRemoved:
		return office.High
	case d.Report != nil:
		if d.Explanation != nil && office.Rank(d.Explanation.Severity) > office.Rank(d.Report.Severity) {
			return d.Explanation.Severity
		}
		return d.Report.Severity
	}
	return office.None
}

func (d *Document) hasBaseline() bool { return d.BaselineHash != "" || d.BaselineRev != "" }

// Summary is a document without its report, for lists.
type Summary struct {
	ID         string      `json:"id"`
	Source     string      `json:"source"`
	Path       string      `json:"path"`
	Name       string      `json:"name"`
	Folder     string      `json:"folder"`
	Kind       office.Kind `json:"kind"`
	Status     Status      `json:"status"`
	Severity   string      `json:"severity"`
	Findings   int         `json:"findings"`
	ModTime    time.Time   `json:"mod_time"`
	ChangedAt  time.Time   `json:"changed_at,omitempty"`
	BaselineAt time.Time   `json:"baseline_at,omitempty"`
	// ExplainedAt is the time of the AI explanation, if any.
	ExplainedAt time.Time `json:"explained_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// State is what the interface shows about the watcher.
type State struct {
	Scanning  bool      `json:"scanning"`
	LastScan  time.Time `json:"last_scan,omitempty"`
	ScanError string    `json:"scan_error,omitempty"`
	Total     int       `json:"total"`
	ToReview  int       `json:"to_review"`
	// Reviewed is the number of reviews kept in the history.
	Reviewed  int       `json:"reviewed"`
	Documents []Summary `json:"documents"`
}

// MaxHistory is the number of reviews kept, most recent first.
const MaxHistory = 50

// Review is a change a person marked as reviewed (or a deletion they
// acknowledged), kept with the report and the AI explanation they saw so
// they can read it again. It is a record only: it changes no baseline.
type Review struct {
	ID     string      `json:"id"`
	DocID  string      `json:"doc_id"`
	Source string      `json:"source"`
	Name   string      `json:"name"`
	Folder string      `json:"folder"`
	Path   string      `json:"path"`
	Link   string      `json:"link,omitempty"`
	Kind   office.Kind `json:"kind"`
	// Status is the status the document had when it was reviewed: changed
	// or removed.
	Status   Status `json:"status"`
	Severity string `json:"severity"`
	// ChangedAt is when the change was detected, BaselineAt the time of the
	// version it was compared with, ReviewedAt when it was approved.
	ChangedAt   time.Time      `json:"changed_at,omitempty"`
	BaselineAt  time.Time      `json:"baseline_at,omitempty"`
	ReviewedAt  time.Time      `json:"reviewed_at"`
	Report      *office.Report `json:"report,omitempty"`
	Explanation *Explanation   `json:"explanation,omitempty"`
}

// ReviewSummary is a review without its report, for lists.
type ReviewSummary struct {
	ID         string      `json:"id"`
	DocID      string      `json:"doc_id"`
	Name       string      `json:"name"`
	Folder     string      `json:"folder"`
	Path       string      `json:"path"`
	Kind       office.Kind `json:"kind"`
	Status     Status      `json:"status"`
	Severity   string      `json:"severity"`
	Findings   int         `json:"findings"`
	ChangedAt  time.Time   `json:"changed_at,omitempty"`
	ReviewedAt time.Time   `json:"reviewed_at"`
}

// Factory builds the source of a configured location.
type Factory func(cfg config.Source) (source.Source, error)

// FolderFactory builds folder sources only.
func FolderFactory(cfg config.Source) (source.Source, error) {
	if cfg.Type != config.SourceFolder {
		return nil, fmt.Errorf("unsupported source type %q", cfg.Type)
	}
	return source.NewFolder(cfg.Path), nil
}

// Watcher scans the sources and keeps the document states.
type Watcher struct {
	dir      string
	settings func() config.Settings
	log      *slog.Logger
	onUpdate func(total, toReview int)
	factory  Factory
	// onScanned runs after each scan, for the automatic AI explanations.
	onScanned func()

	mu        sync.Mutex
	docs      map[string]*Document
	history   []*Review // most recent first
	scanning  bool
	lastScan  time.Time
	problems  map[string]string // by source id
	scanError string

	srcMu  sync.Mutex
	active map[string]*activeSource

	trigger chan struct{}
	scanMu  sync.Mutex // one scan at a time
}

type activeSource struct {
	cfg config.Source
	src source.Source
}

// New loads the saved state of a data directory. It reads folder sources
// until SetFactory adds the other types.
func New(dir string, settings func() config.Settings, log *slog.Logger) (*Watcher, error) {
	w := &Watcher{
		dir:      dir,
		settings: settings,
		log:      log,
		factory:  FolderFactory,
		docs:     map[string]*Document{},
		problems: map[string]string{},
		active:   map[string]*activeSource{},
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
			migrate(d)
			w.docs[d.ID] = d
		}
	}
	w.loadHistory()
	return w, nil
}

// loadHistory reads the saved reviews. The history is only a record: an
// unreadable file costs the past reports, never a baseline.
func (w *Watcher) loadHistory() {
	data, err := os.ReadFile(w.historyPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.log.Error("reviewed.json unreadable", "err", err)
		}
		return
	}
	var history []*Review
	if err := json.Unmarshal(data, &history); err != nil {
		w.log.Error("reviewed.json unreadable, starting a new history", "err", err)
		return
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].ReviewedAt.After(history[j].ReviewedAt) })
	if len(history) > MaxHistory {
		history = history[:MaxHistory]
	}
	w.history = history
}

// migrate fills the fields of a document saved by a version that only
// watched folders. Its id is unchanged: folder documents keep the id derived
// from their path.
func migrate(d *Document) {
	if d.Source != "" || d.Root == "" {
		return
	}
	d.Source = config.FolderSourceID(d.Root)
	d.Key = d.Path
	d.Name = filepath.Base(d.Path)
	d.Folder = source.RelFolder(d.Root, d.Path)
	d.Root = ""
}

// SetFactory sets how the sources are built; call it before Run.
func (w *Watcher) SetFactory(f Factory) {
	w.srcMu.Lock()
	defer w.srcMu.Unlock()
	w.factory = f
	w.active = map[string]*activeSource{}
}

// OnUpdate registers a callback run after each scan and review, used by the
// tray icon to show the number of documents to review.
func (w *Watcher) OnUpdate(fn func(total, toReview int)) { w.onUpdate = fn }

// OnScanned sets the function run after each scan. Set it before the first
// scan starts.
func (w *Watcher) OnScanned(fn func()) { w.onScanned = fn }

func (w *Watcher) statePath() string   { return filepath.Join(w.dir, "state.json") }
func (w *Watcher) historyPath() string { return filepath.Join(w.dir, "reviewed.json") }
func (w *Watcher) baselineDir() string { return filepath.Join(w.dir, "baselines") }
func (w *Watcher) baselinePath(id string) string {
	return filepath.Join(w.baselineDir(), id)
}

// pendingPath keeps the content an exported document had when its report
// was computed: a review approves that copy, since exporting again could
// give different bytes for the same version.
func (w *Watcher) pendingPath(id string) string {
	return filepath.Join(w.baselineDir(), id+".next")
}

// Run scans each source at its interval until the stop channel closes.
func (w *Watcher) Run(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()
	next := map[string]time.Time{}
	all := true
	for {
		now := time.Now()
		var only map[string]bool
		if !all {
			only = map[string]bool{}
			for _, src := range w.settings().Sources {
				if !now.Before(next[src.ID]) {
					only[src.ID] = true
				}
			}
		}
		if all || len(only) > 0 {
			scanned := w.scan(ctx, only)
			done := time.Now()
			s := w.settings()
			for _, src := range s.Sources {
				if scanned[src.ID] {
					next[src.ID] = done.Add(time.Duration(s.Interval(src)) * time.Second)
				}
			}
		}
		all = false

		s := w.settings()
		wait := time.Duration(s.ScanSeconds) * time.Second
		for _, src := range s.Sources {
			if d := time.Until(next[src.ID]); d < wait {
				wait = d
			}
		}
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-stop:
			return
		case <-w.trigger:
			all = true
		case <-time.After(wait):
		}
	}
}

// ScanNow asks the loop to scan every source without waiting.
func (w *Watcher) ScanNow() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Scan reads every source once.
func (w *Watcher) Scan() { w.scan(context.Background(), nil) }

// scan reads the sources of only, or all of them when only is nil, and
// returns the ids of those it read.
func (w *Watcher) scan(ctx context.Context, only map[string]bool) map[string]bool {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	scanned := map[string]bool{}
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("scan panic", "panic", r)
			w.mu.Lock()
			w.scanning = false
			w.mu.Unlock()
		}
	}()
	s := w.settings()
	w.mu.Lock()
	w.scanning = true
	w.mu.Unlock()
	started := time.Now()

	configured := map[string]config.Source{}
	for _, cfg := range s.Sources {
		configured[cfg.ID] = cfg
	}
	w.pruneSources(configured)

	seen := map[string]bool{}
	completed := map[string]bool{}
	processed := 0
	for _, cfg := range s.Sources {
		if only != nil && !only[cfg.ID] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		scanned[cfg.ID] = true
		src, err := w.sourceFor(cfg)
		if err == nil {
			err = src.List(ctx, func(e source.Entry) {
				id := documentID(cfg, e.Key)
				seen[id] = true
				w.process(ctx, cfg, src, id, e, s)
				processed++
				if processed%200 == 0 {
					w.save()
				}
			})
		}
		w.mu.Lock()
		if err != nil {
			w.problems[cfg.ID] = fmt.Sprintf("%s: %v", cfg.Label(), err)
			w.log.Warn("source unavailable", "source", cfg.Label(), "err", err)
		} else {
			delete(w.problems, cfg.ID)
			completed[cfg.ID] = true
		}
		w.mu.Unlock()
	}

	w.mu.Lock()
	for id := range w.problems {
		if _, ok := configured[id]; !ok {
			delete(w.problems, id)
		}
	}
	historyChanged := w.pruneHistory(configured)
	for id, d := range w.docs {
		_, watched := configured[d.Source]
		switch {
		case !watched:
			// The source is no longer watched: forget its documents.
			delete(w.docs, id)
			w.removeCopies(id)
		case completed[d.Source] && !seen[id] && d.Status != StatusRemoved:
			if !d.hasBaseline() {
				delete(w.docs, id)
				continue
			}
			d.Status, d.ChangedAt, d.Report, d.Explanation, d.Error = StatusRemoved, time.Now(), nil, nil, ""
		}
	}
	problems := make([]string, 0, len(w.problems))
	for _, p := range w.problems {
		problems = append(problems, p)
	}
	sort.Strings(problems)
	w.scanning = false
	w.lastScan = time.Now()
	w.scanError = strings.Join(problems, "; ")
	w.mu.Unlock()
	w.save()
	if historyChanged {
		w.saveHistory()
	}
	total, toReview := w.counts()
	w.log.Info("scan done", "sources", len(scanned), "documents", total, "to_review", toReview, "duration", time.Since(started).Round(time.Millisecond))
	w.onUpdate(total, toReview)
	w.onScanned()
	return scanned
}

// sourceFor returns the source of a configuration, built once and kept
// while the configuration does not change (a drive keeps its cache).
func (w *Watcher) sourceFor(cfg config.Source) (source.Source, error) {
	w.srcMu.Lock()
	defer w.srcMu.Unlock()
	if a, ok := w.active[cfg.ID]; ok && a.cfg == cfg {
		return a.src, nil
	}
	src, err := w.factory(cfg)
	if err != nil {
		return nil, err
	}
	w.active[cfg.ID] = &activeSource{cfg: cfg, src: src}
	return src, nil
}

func (w *Watcher) pruneSources(configured map[string]config.Source) {
	w.srcMu.Lock()
	defer w.srcMu.Unlock()
	for id := range w.active {
		if _, ok := configured[id]; !ok {
			delete(w.active, id)
		}
	}
}

func (w *Watcher) configuredSource(id string) (config.Source, bool) {
	for _, cfg := range w.settings().Sources {
		if cfg.ID == id {
			return cfg, true
		}
	}
	return config.Source{}, false
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

// sameVersion reports whether an entry is the version already processed.
func sameVersion(d *Document, e source.Entry) bool {
	return d.Size == e.Size && d.ModTime.Equal(e.ModTime) && d.Version == e.Version
}

// process brings one document up to date. It never holds the lock while
// reading: a large document or a slow download must not freeze the
// interface.
func (w *Watcher) process(ctx context.Context, cfg config.Source, src source.Source, id string, e source.Entry, s config.Settings) {
	w.mu.Lock()
	prev, known := w.docs[id]
	var d Document
	if known {
		if sameVersion(prev, e) && prev.Status != StatusError && prev.Status != StatusRemoved {
			// A document renamed or moved in its drive keeps its id: only
			// what is shown changes.
			prev.Name, prev.Folder, prev.Path, prev.Link = e.Name, e.Folder, e.Location, e.Link
			w.mu.Unlock()
			return
		}
		d = *prev
	} else {
		d = Document{ID: id}
	}
	w.mu.Unlock()

	d.Source, d.Key, d.Name, d.Folder, d.Path, d.Link = cfg.ID, e.Key, e.Name, e.Folder, e.Location, e.Link
	d.Kind, d.Exported = e.Kind, e.Exported
	commit := func() {
		w.mu.Lock()
		w.docs[id] = &d
		w.mu.Unlock()
	}
	stamp := func() { d.Size, d.ModTime, d.Version = e.Size, e.ModTime, e.Version }
	fail := func(msg string) {
		// Size and time are left as they were so the next scan retries.
		d.Status, d.Error = StatusError, msg
		commit()
	}
	tooLarge := func() {
		stamp()
		d.Status, d.Error = StatusTooLarge, fmt.Sprintf(msg.M("larger than %d MB"), s.MaxFileMB)
		commit()
	}
	limit := int64(s.MaxFileMB) << 20
	history, hasHistory := src.(source.History)

	if !d.hasBaseline() && hasHistory && e.Revision != "" {
		// The source keeps the versions: remember which one is the
		// reference and download it only if the document changes.
		stamp()
		d.BaselineRev, d.BaselineAt, d.CurrentHash = e.Revision, time.Now(), ""
		d.Status, d.Report, d.Explanation, d.Error = StatusClean, nil, nil, ""
		commit()
		return
	}
	if !d.hasBaseline() && e.CloudOnly && !s.DownloadCloudFiles {
		stamp()
		d.Status, d.Error = StatusCloudOnly, ""
		commit()
		return
	}
	if e.Size > limit {
		tooLarge()
		return
	}
	data, err := src.Read(ctx, e, limit)
	if errors.Is(err, source.ErrTooLarge) {
		tooLarge()
		return
	}
	if err != nil {
		fail(fmt.Sprintf(msg.M("cannot read the document: %v"), err))
		return
	}
	hash := hashOf(data)

	if d.BaselineHash == "" && d.BaselineRev != "" {
		var base []byte
		err := source.ErrRevisionGone
		if hasHistory {
			base, err = history.ReadRevision(ctx, e, d.BaselineRev, limit)
		}
		switch {
		case errors.Is(err, source.ErrRevisionGone):
			// Nothing left to compare with: the person must read the whole
			// document, which a review then makes the baseline.
			if e.Exported {
				if err := config.WriteFileAtomic(w.pendingPath(id), data); err != nil {
					fail(fmt.Sprintf(msg.M("cannot store the analyzed copy: %v"), err))
					return
				}
			}
			stamp()
			d.CurrentHash, d.Report, d.Explanation, d.Error = hash, lostBaselineReport(e.Kind), nil, ""
			d.Status, d.ChangedAt = StatusChanged, time.Now()
			commit()
			return
		case errors.Is(err, source.ErrTooLarge):
			tooLarge()
			return
		case err != nil:
			fail(fmt.Sprintf(msg.M("cannot read the reviewed version: %v"), err))
			return
		}
		if err := w.writeBaseline(id, base); err != nil {
			fail(fmt.Sprintf(msg.M("cannot store the baseline: %v"), err))
			return
		}
		d.BaselineHash, d.BaselineRev = hashOf(base), ""
	}

	switch {
	case d.BaselineHash == "":
		if err := w.writeBaseline(id, data); err != nil {
			fail(fmt.Sprintf(msg.M("cannot store the baseline: %v"), err))
			return
		}
		stamp()
		d.BaselineHash, d.BaselineAt, d.CurrentHash = hash, time.Now(), hash
		d.Status, d.Report, d.Explanation, d.Error = StatusClean, nil, nil, ""
	case hash == d.BaselineHash:
		stamp()
		d.CurrentHash, d.Status, d.Report, d.Explanation, d.Error = hash, StatusClean, nil, nil, ""
		os.Remove(w.pendingPath(id))
	case hash == d.CurrentHash && d.Report != nil:
		// Touched (synchronized, opened and saved) without new content.
		stamp()
		d.Status, d.Error = StatusChanged, ""
	default:
		baseline, err := os.ReadFile(w.baselinePath(id))
		if err != nil {
			fail(fmt.Sprintf(msg.M("baseline missing: %v"), err))
			return
		}
		report, err := office.Compare(e.Kind, baseline, data)
		if err != nil {
			// Most often the file is still being written or synchronized:
			// keep the previous size and time so the next scan tries again.
			fail(err.Error())
			return
		}
		if e.Exported && report.ChangeCount == 0 {
			// Exported again without a visible modification.
			stamp()
			d.CurrentHash, d.Status, d.Report, d.Explanation, d.Error = hash, StatusClean, nil, nil, ""
			os.Remove(w.pendingPath(id))
			commit()
			return
		}
		if e.Exported {
			if err := config.WriteFileAtomic(w.pendingPath(id), data); err != nil {
				fail(fmt.Sprintf(msg.M("cannot store the analyzed copy: %v"), err))
				return
			}
		}
		stamp()
		d.Explanation = carryExplanation(d.Explanation, d.Report, report)
		d.CurrentHash, d.Report, d.Error = hash, report, ""
		d.Status, d.ChangedAt = StatusChanged, time.Now()
	}
	commit()
}

// lostBaselineReport stands for a comparison that cannot be made because
// the source no longer keeps the reviewed version.
func lostBaselineReport(kind office.Kind) *office.Report {
	return &office.Report{
		Kind:     kind,
		Severity: office.High,
		Findings: []office.Finding{{
			Severity: office.High,
			Rule:     "source.baseline-unavailable",
			Title:    msg.M("The reviewed version is no longer kept by the source: read the whole document before marking it as reviewed"),
		}},
		Changes: []office.Change{},
	}
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// carryExplanation keeps what an earlier AI review said about the elements
// that were not modified again when a document changes once more, so a new
// save does not discard an analysis that still holds.
//
// Changes are matched on their content (kind, before and after), not on their
// location, which shifts when a paragraph or a row is inserted above them. An
// AI finding is tied to the earlier changes at its location or quoting its
// excerpts, and kept when all of them are still present. A finding tied to
// nothing is kept only when every earlier change is still present. The
// impacts, which were read from the whole answer, are kept on the same
// condition; the model can only raise the severity from them.
func carryExplanation(e *Explanation, prev, cur *office.Report) *Explanation {
	if e == nil || prev == nil || cur == nil {
		return nil
	}
	present := map[string]int{}
	for _, c := range cur.Changes {
		present[changeKey(c)]++
	}
	stillThere := func(c office.Change) bool { return present[changeKey(c)] > 0 }

	allKept := !prev.Truncated
	for _, c := range prev.Changes {
		allKept = allKept && stillThere(c)
	}
	if allKept && !cur.Truncated && len(cur.Changes) == len(prev.Changes) {
		// Same modifications, saved again: the explanation still holds.
		kept := *e
		return &kept
	}

	kept := *e
	kept.Outdated = true
	kept.Findings = nil
	// The readings follow the order of the earlier findings.
	kept.Readings = nil
	for _, f := range e.Findings {
		tied := tiedChanges(f, prev.Changes)
		keep := allKept
		if len(tied) > 0 {
			keep = true
			for _, c := range tied {
				keep = keep && stillThere(c)
			}
		}
		if keep {
			kept.Findings = append(kept.Findings, f)
		}
	}
	if !allKept {
		kept.Impacts, kept.Severity = nil, ""
	}
	return &kept
}

// changeKey identifies a change by its content.
func changeKey(c office.Change) string {
	return c.Kind + "\x00" + c.Before + "\x00" + c.After
}

// tiedChanges returns the changes an AI finding refers to: those at its
// location, or whose excerpts it quotes.
func tiedChanges(f office.Finding, changes []office.Change) []office.Change {
	loc := strings.ToLower(strings.TrimSpace(f.Location))
	var out []office.Change
	for _, c := range changes {
		cl := strings.ToLower(strings.TrimSpace(c.Location))
		sameLoc := loc != "" && (cl == loc || strings.HasSuffix(cl, " "+loc))
		quoted := (f.Before != "" && f.Before == c.Before) || (f.After != "" && f.After == c.After)
		if sameLoc || quoted {
			out = append(out, c)
		}
	}
	return out
}

func (w *Watcher) writeBaseline(id string, data []byte) error {
	return config.WriteFileAtomic(w.baselinePath(id), data)
}

func (w *Watcher) removeCopies(id string) {
	os.Remove(w.baselinePath(id))
	os.Remove(w.pendingPath(id))
}

// ErrStale reports a review of a report that no longer matches the file.
var ErrStale = errors.New(msg.M("the document changed again since this report; it is being analyzed again"))

// ErrNotFound reports an unknown document id.
var ErrNotFound = errors.New(msg.M("unknown document"))

// Accept marks the current version of a document as reviewed: it becomes
// the baseline for the next changes. The document is read again and must
// still be the version the report describes, so nobody approves unseen
// content. An exported document is checked by its version, and the copy
// analyzed for the report becomes the baseline.
func (w *Watcher) Accept(ctx context.Context, id string) error {
	w.mu.Lock()
	d, ok := w.docs[id]
	if !ok {
		w.mu.Unlock()
		return ErrNotFound
	}
	if d.Status == StatusRemoved {
		w.record(d)
		delete(w.docs, id)
		w.mu.Unlock()
		w.removeCopies(id)
		w.saveHistory()
		w.afterReview()
		return nil
	}
	snapshot := *d
	w.mu.Unlock()
	if snapshot.CurrentHash == "" {
		w.ScanNow()
		return ErrStale
	}

	cfg, ok := w.configuredSource(snapshot.Source)
	if !ok {
		return ErrNotFound
	}
	src, err := w.sourceFor(cfg)
	if err != nil {
		return err
	}
	e, err := src.Stat(ctx, snapshot.Key)
	if errors.Is(err, source.ErrNotFound) {
		w.ScanNow()
		return ErrStale
	}
	if err != nil {
		return err
	}
	var data []byte
	if e.Exported {
		if !sameVersion(&snapshot, e) {
			w.ScanNow()
			return ErrStale
		}
		data, err = os.ReadFile(w.pendingPath(id))
		if err != nil {
			// The analyzed copy is gone: analyze the document again.
			w.mu.Lock()
			if d, ok := w.docs[id]; ok {
				d.Status, d.Error = StatusError, msg.M("the analyzed copy is missing")
			}
			w.mu.Unlock()
			w.ScanNow()
			return ErrStale
		}
	} else {
		data, err = src.Read(ctx, e, int64(w.settings().MaxFileMB)<<20)
		if err != nil {
			return err
		}
	}
	if hashOf(data) != snapshot.CurrentHash {
		w.ScanNow()
		return ErrStale
	}
	if err := w.writeBaseline(id, data); err != nil {
		return err
	}
	os.Remove(w.pendingPath(id))
	w.mu.Lock()
	if d, ok := w.docs[id]; ok {
		// The report approved is the one of this version: record it before
		// the new baseline clears it.
		if d.CurrentHash == snapshot.CurrentHash {
			w.record(d)
		}
		d.BaselineHash, d.BaselineRev, d.BaselineAt = snapshot.CurrentHash, "", time.Now()
		d.Status, d.Report, d.Explanation, d.Error = StatusClean, nil, nil, ""
	}
	w.mu.Unlock()
	w.saveHistory()
	w.afterReview()
	return nil
}

// record adds the review of a document to the history; w.mu must be held.
func (w *Watcher) record(d *Document) {
	now := time.Now()
	r := &Review{
		ID:    fmt.Sprintf("%s-%d", d.ID, now.UnixNano()),
		DocID: d.ID, Source: d.Source, Name: d.Name, Folder: d.Folder, Path: d.Path, Link: d.Link,
		Kind: d.Kind, Status: d.Status, Severity: d.Severity(),
		ChangedAt: d.ChangedAt, BaselineAt: d.BaselineAt, ReviewedAt: now,
		Report: d.Report,
	}
	if d.Explanation != nil {
		e := *d.Explanation
		r.Explanation = &e
	}
	w.history = append([]*Review{r}, w.history...)
	if len(w.history) > MaxHistory {
		w.history = w.history[:MaxHistory]
	}
}

// pruneHistory forgets the reviews of the sources no longer watched, as
// their documents are: the reports hold excerpts. w.mu must be held.
func (w *Watcher) pruneHistory(configured map[string]config.Source) bool {
	kept := w.history[:0]
	for _, r := range w.history {
		if _, ok := configured[r.Source]; ok {
			kept = append(kept, r)
		}
	}
	changed := len(kept) != len(w.history)
	clear(w.history[len(kept):])
	w.history = kept
	return changed
}

// History lists the latest reviews, most recent first.
func (w *Watcher) History() []ReviewSummary {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ReviewSummary, len(w.history))
	for i, r := range w.history {
		out[i] = ReviewSummary{
			ID: r.ID, DocID: r.DocID, Name: r.Name, Folder: r.Folder, Path: r.Path,
			Kind: r.Kind, Status: r.Status, Severity: r.Severity,
			ChangedAt: r.ChangedAt, ReviewedAt: r.ReviewedAt,
		}
		if r.Report != nil {
			out[i].Findings = len(r.Report.Findings)
		}
	}
	return out
}

// Review returns a review of the history with its report.
func (w *Watcher) Review(id string) (Review, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.history {
		if r.ID == id {
			return *r, true
		}
	}
	return Review{}, false
}

func (w *Watcher) saveHistory() {
	w.mu.Lock()
	data, err := json.Marshal(w.history)
	w.mu.Unlock()
	if err != nil {
		w.log.Error("encode history", "err", err)
		return
	}
	if err := config.WriteFileAtomic(w.historyPath(), data); err != nil {
		w.log.Error("save history", "err", err)
	}
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

// Pending is a changed document waiting for its AI explanation.
type Pending struct {
	ID   string
	Hash string
}

// NeedingExplanation lists the changed documents without an explanation of
// their current version, most severe first: never explained, or explained
// for an earlier version of the modifications.
func (w *Watcher) NeedingExplanation() []Pending {
	w.mu.Lock()
	defer w.mu.Unlock()
	var docs []*Document
	for _, d := range w.docs {
		if d.Status == StatusChanged && d.Report != nil && (d.Explanation == nil || d.Explanation.Outdated) {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool {
		if ri, rj := office.Rank(docs[i].Severity()), office.Rank(docs[j].Severity()); ri != rj {
			return ri > rj
		}
		return docs[i].ChangedAt.Before(docs[j].ChangedAt)
	})
	out := make([]Pending, len(docs))
	for i, d := range docs {
		out[i] = Pending{ID: d.ID, Hash: d.CurrentHash}
	}
	return out
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
	st := State{Scanning: w.scanning, LastScan: w.lastScan, ScanError: w.scanError, Reviewed: len(w.history), Documents: []Summary{}}
	for _, d := range w.docs {
		st.Total++
		if d.NeedsReview() {
			st.ToReview++
		}
		sum := Summary{
			ID: d.ID, Source: d.Source, Path: d.Path, Name: d.Name, Folder: d.Folder,
			Kind: d.Kind, Status: d.Status, Severity: d.Severity(), ModTime: d.ModTime,
			ChangedAt: d.ChangedAt, BaselineAt: d.BaselineAt, Error: d.Error,
		}
		if d.Report != nil {
			sum.Findings = len(d.Report.Findings)
		}
		if d.Explanation != nil {
			sum.ExplainedAt = d.Explanation.At
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

// documentID derives a stable id for a document. A folder document keeps
// the id the versions that only watched folders derived from its path; the
// documents of the other sources are scoped by their source.
func documentID(cfg config.Source, key string) string {
	if cfg.Type == config.SourceFolder {
		return docID(key)
	}
	sum := sha256.Sum256([]byte(cfg.ID + "\x00" + key))
	return hex.EncodeToString(sum[:10])
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
