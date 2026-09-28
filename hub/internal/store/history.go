package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxRecords bounds retained artifacts per repository, including both variants.
// MaxRecent bounds the lightweight repository-list projection independently.
const MaxRecords = 1000
const MaxRecent = 200

// Only this repository's readers/writers share this lock. No report I/O holds
// the account/repository metadata lock. Loaded histories are immutable copies.
type recordIndex struct {
	mu      sync.Mutex
	loaded  bool
	pending int // successful artifact writes since the last checkpoint
	recent  []RecentRun
	diskIndex
}
type diskIndex struct {
	Version        int       `json:"version"`
	Runs           []Run     `json:"runs"`
	EvictedThrough time.Time `json:"evicted_through"`
	Incomplete     bool      `json:"incomplete,omitempty"`
}

const indexFile = ".runs-index"
const dirtyFile = ".runs-dirty" // legacy recovery marker, never written now
const watermarkFile = ".runs-watermark"
const checkpointWrites = 100

func (s *Store) index(userKey, repoKey string) (*recordIndex, string, error) {
	if !ValidKey(userKey) || !ValidKey(repoKey) {
		return nil, "", fmt.Errorf("invalid key")
	}
	key := userKey + "/" + repoKey
	s.indexesMu.Lock()
	defer s.indexesMu.Unlock()
	idx := s.indexes[key]
	if idx == nil {
		idx = &recordIndex{}
		s.indexes[key] = idx
	}
	dir, err := s.path("reports", userKey, repoKey)
	return idx, dir, err
}

func validRun(run Run) bool {
	return ValidKey(run.Commit) && ValidKey(strings.TrimSuffix(recordName(run.Commit, run.Variant), ".json")) &&
		(run.Variant == "" || run.Variant == "normal" || run.Variant == "plan")
}

// Index metadata is bounded even when migrating old, unbounded diagnostics.
func indexedRun(run Run) Run {
	run.Error = SafeError(run.Error)
	run.Intent = ""
	run.Message = bounded(run.Message, 1024)
	run.Author = bounded(run.Author, 256)
	run.Ref = bounded(run.Ref, 256)
	run.BaseCommit = bounded(run.BaseCommit, 120)
	run.Trigger = bounded(run.Trigger, 32)
	run.Mode = bounded(run.Mode, 32)
	run.ToolVersion = bounded(run.ToolVersion, 128)
	run.Status = bounded(run.Status, 16)
	run.Summary.ToolVersion = bounded(run.Summary.ToolVersion, 128)
	run.Summary.CoverageState = bounded(run.Summary.CoverageState, 32)
	run.Summary.Verdict = bounded(run.Summary.Verdict, 16)
	return run
}

func (idx *recordIndex) load(dir string) error {
	if idx.loaded {
		return nil
	}
	_, dirty := os.Stat(filepath.Join(dir, dirtyFile))
	var disk diskIndex
	if os.IsNotExist(dirty) && readJSON(filepath.Join(dir, indexFile), &disk) == nil && (disk.Version == 1 || disk.Version == 2) && len(disk.Runs) <= MaxRecords {
		valid := true
		for i, run := range disk.Runs {
			if !validRun(run) {
				valid = false
				break
			}
			disk.Runs[i] = indexedRun(run)
		}
		if valid {
			// Upgrade the legacy combined watermark before invalidating its
			// checkpoint on the first subsequent write.
			if disk.Version == 1 {
				watermark := diskIndex{Version: 1, EvictedThrough: disk.EvictedThrough, Incomplete: disk.Incomplete}
				if err := writeJSON(filepath.Join(dir, watermarkFile), &watermark); err != nil {
					return err
				}
			}
			disk.Version = 2
			idx.diskIndex = disk
			idx.sort()
			idx.refreshRecent()
			idx.loaded = true
			return nil
		}
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		idx.diskIndex = diskIndex{Version: 2}
		idx.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	readJSON(filepath.Join(dir, indexFile), &disk) // retain legacy watermark
	// Keep the previous eviction watermark when recovering an interrupted write.
	idx.diskIndex = diskIndex{Version: 2}
	idx.EvictedThrough, idx.Incomplete = disk.EvictedThrough, disk.Incomplete
	var watermark diskIndex
	if err := readJSON(filepath.Join(dir, watermarkFile), &watermark); err == nil {
		if watermark.EvictedThrough.After(idx.EvictedThrough) {
			idx.EvictedThrough = watermark.EvictedThrough
		}
		idx.Incomplete = idx.Incomplete || watermark.Incomplete
	} else if err != ErrNotFound {
		idx.Incomplete = true
	}
	// Preserve legacy retention metadata before removing the old snapshot.
	if disk.Version == 1 {
		watermark := diskIndex{Version: 1, EvictedThrough: idx.EvictedThrough, Incomplete: idx.Incomplete}
		if err := writeJSON(filepath.Join(dir, watermarkFile), &watermark); err != nil {
			return err
		}
	}
	// Invalidate any old checkpoint before changing artifacts. Recovery uses
	// artifacts plus the independent eviction watermark, never a stale snapshot.
	if err := invalidateIndex(dir); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			idx.Incomplete = true
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var run Run // Raw is intentionally not retained during the one-time migration.
		if err := readJSON(filepath.Join(dir, entry.Name()), &run); err != nil || !validRun(run) || recordName(run.Commit, run.Variant) != entry.Name() {
			idx.Incomplete = true
			continue
		}
		idx.Runs = append(idx.Runs, indexedRun(run))
	}
	if len(idx.Runs) <= MaxRecords && idx.Incomplete {
		watermark := diskIndex{Version: 1, EvictedThrough: idx.EvictedThrough, Incomplete: true}
		if err := writeJSON(filepath.Join(dir, watermarkFile), &watermark); err != nil {
			return err
		}
	}
	if err := idx.prune(dir); err != nil {
		return err
	}
	if err := idx.save(dir); err != nil {
		return err
	}
	idx.refreshRecent()
	idx.loaded = true
	return nil
}

func (idx *recordIndex) sort() {
	sort.Slice(idx.Runs, func(i, j int) bool {
		a, b := idx.Runs[i], idx.Runs[j]
		if a.QueuedAt.Equal(b.QueuedAt) {
			return recordName(a.Commit, a.Variant) < recordName(b.Commit, b.Variant)
		}
		return a.QueuedAt.After(b.QueuedAt)
	})
}

// invalidateIndex syncs only an actual checkpoint removal. Ordinary writes
// between checkpoints need neither a dirty marker nor another directory sync.
func invalidateIndex(dir string) error {
	if err := os.Remove(filepath.Join(dir, indexFile)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (idx *recordIndex) prune(dir string) error {
	idx.sort()
	if len(idx.Runs) <= MaxRecords {
		return nil
	}
	evicted := idx.Runs[MaxRecords:]
	for _, run := range evicted {
		if run.Variant != "plan" {
			recent := projectRecent(&run)
			if recent.activityAt().IsZero() || run.Status == StatusQueued || run.Status == StatusRunning {
				idx.Incomplete = true
			}
			if recent.activityAt().After(idx.EvictedThrough) {
				idx.EvictedThrough = recent.activityAt()
			}
		}
	}
	// One small watermark per eviction batch, persisted before any deletion.
	watermark := diskIndex{Version: 1, EvictedThrough: idx.EvictedThrough, Incomplete: idx.Incomplete}
	if err := writeJSON(filepath.Join(dir, watermarkFile), &watermark); err != nil {
		return err
	}
	for _, run := range evicted {
		if err := os.Remove(filepath.Join(dir, recordName(run.Commit, run.Variant))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	idx.Runs = idx.Runs[:MaxRecords]
	return nil
}
func (idx *recordIndex) save(dir string) error {
	if err := writeJSON(filepath.Join(dir, indexFile), &idx.diskIndex); err != nil {
		return err
	}
	idx.pending = 0
	if err := os.Remove(filepath.Join(dir, dirtyFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PutRecord atomically replaces an artifact and updates its in-memory index.
// Checkpoints are amortized over 100 writes; an absent checkpoint is rebuilt
// lazily from artifacts after restart. A failed write never forces a hot rescan.
func (s *Store) PutRecord(rec *Record) error {
	if !validRun(rec.Run) {
		return fmt.Errorf("invalid commit or analysis variant")
	}
	idx, dir, err := s.index(rec.UserKey, rec.RepoKey)
	if err != nil {
		return err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.load(dir); err != nil {
		return err
	}
	if idx.pending == 0 {
		if err := invalidateIndex(dir); err != nil {
			return err
		}
	}
	clean := *rec
	clean.Error = SafeError(clean.Error)
	if err := writeJSON(filepath.Join(dir, recordName(rec.Commit, rec.Variant)), &clean); err != nil {
		return err
	}
	name := recordName(rec.Commit, rec.Variant)
	runs := idx.Runs[:0]
	for _, run := range idx.Runs {
		if recordName(run.Commit, run.Variant) != name {
			runs = append(runs, run)
		}
	}
	idx.Runs = append(runs, indexedRun(clean.Run))
	idx.pending++
	defer idx.refreshRecent()
	if err := idx.prune(dir); err != nil {
		return err
	}
	if idx.pending >= checkpointWrites {
		return idx.save(dir)
	}
	return nil
}

// History reads a copy of indexed metadata, rebuilding from artifacts only
// on first access when no valid checkpoint exists.
// Zero returns all retained metadata (at most MaxRecords); HTTP adds its own cap.
func (s *Store) History(userKey, repoKey string, limit int) ([]Run, error) {
	idx, dir, err := s.index(userKey, repoKey)
	if err != nil {
		return nil, err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.load(dir); err != nil {
		return nil, err
	}
	n := len(idx.Runs)
	if limit > 0 && limit < n {
		n = limit
	}
	return append([]Run(nil), idx.Runs[:n]...), nil
}

// Recent stops at the cutoff and returns only bounded aggregation metadata.
// The incomplete flag covers both projection limits and retention in the window.
func (s *Store) Recent(userKey, repoKey string, since time.Time) ([]RecentRun, bool, error) {
	idx, dir, err := s.index(userKey, repoKey)
	if err != nil {
		return nil, true, err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.load(dir); err != nil {
		return nil, true, err
	}
	incomplete := idx.Incomplete || (!idx.EvictedThrough.IsZero() && !idx.EvictedThrough.Before(since))
	runs := make([]RecentRun, 0, MaxRecent)
	for _, run := range idx.recent {
		if !run.inWindow(since) {
			break
		}
		if len(runs) == MaxRecent {
			incomplete = true
			break
		}
		runs = append(runs, run)
	}
	return runs, incomplete, nil
}

// Pending/undated results are always relevant; the remainder is ordered by
// latest activity, allowing Recent to stop as soon as it reaches the cutoff.
func (idx *recordIndex) refreshRecent() {
	idx.recent = nil
	for _, run := range idx.Runs {
		if run.Variant != "plan" {
			idx.recent = append(idx.recent, projectRecent(&run))
		}
	}
	sort.Slice(idx.recent, func(i, j int) bool {
		a, b := idx.recent[i], idx.recent[j]
		always := func(r RecentRun) bool {
			return r.Status == StatusQueued || r.Status == StatusRunning || r.activityAt().IsZero()
		}
		if always(a) != always(b) {
			return always(a)
		}
		if a.activityAt().Equal(b.activityAt()) {
			return a.Commit < b.Commit
		}
		return a.activityAt().After(b.activityAt())
	})
}
