package store

import (
	"fmt"
	"io/fs"
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
	mu     sync.Mutex
	loaded bool
	recent []RecentRun
	diskIndex
}
type diskIndex struct {
	Version        int       `json:"version"`
	Runs           []Run     `json:"runs"`
	EvictedThrough time.Time `json:"evicted_through"`
	Incomplete     bool      `json:"incomplete,omitempty"`
}

const indexFile = ".runs-index"
const dirtyFile = ".runs-dirty"

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
	return idx, filepath.Join(s.dir, "reports", userKey, repoKey), nil
}

// Migrate old stores once before accepting requests. Later starts read only
// compact indexes, except after an interrupted write or a damaged index.
func (s *Store) loadIndexes() error {
	root := filepath.Join(s.dir, "reports")
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 {
			return nil
		}
		idx, dir, err := s.index(parts[0], parts[1])
		if err != nil {
			return err
		}
		if err := idx.load(dir); err != nil {
			return err
		}
		return filepath.SkipDir
	})
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
	if os.IsNotExist(dirty) && readJSON(filepath.Join(dir, indexFile), &disk) == nil && disk.Version == 1 && len(disk.Runs) <= MaxRecords {
		valid := true
		for i, run := range disk.Runs {
			if !validRun(run) {
				valid = false
				break
			}
			disk.Runs[i] = indexedRun(run)
		}
		if valid {
			idx.diskIndex = disk
			idx.sort()
			idx.refreshRecent()
			idx.loaded = true
			return nil
		}
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		idx.diskIndex = diskIndex{Version: 1}
		idx.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, dirtyFile), true); err != nil {
		return err
	}
	// Keep the previous eviction watermark when recovering an interrupted write.
	idx.diskIndex = diskIndex{Version: 1}
	if readJSON(filepath.Join(dir, indexFile), &disk) == nil {
		idx.EvictedThrough, idx.Incomplete = disk.EvictedThrough, disk.Incomplete
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var run Run // Raw is intentionally not retained during the one-time migration.
		if err := readJSON(filepath.Join(dir, entry.Name()), &run); err != nil || !validRun(run) || recordName(run.Commit, run.Variant) != entry.Name() {
			idx.Incomplete = true
			continue
		}
		idx.Runs = append(idx.Runs, indexedRun(run))
		if err := idx.prune(dir); err != nil {
			return err
		}
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

func (idx *recordIndex) prune(dir string) error {
	idx.sort()
	for len(idx.Runs) > MaxRecords {
		run := idx.Runs[len(idx.Runs)-1]
		// Persist the watermark before deletion so crash recovery cannot hide a gap.
		if run.Variant != "plan" {
			recent := projectRecent(&run)
			if recent.activityAt().IsZero() || run.Status == StatusQueued || run.Status == StatusRunning {
				idx.Incomplete = true
			}
			if recent.activityAt().After(idx.EvictedThrough) {
				idx.EvictedThrough = recent.activityAt()
			}
		}
		watermark := diskIndex{Version: 1, EvictedThrough: idx.EvictedThrough, Incomplete: idx.Incomplete}
		if err := writeJSON(filepath.Join(dir, indexFile), &watermark); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, recordName(run.Commit, run.Variant))); err != nil && !os.IsNotExist(err) {
			return err
		}
		idx.Runs = idx.Runs[:len(idx.Runs)-1]
	}
	return nil
}
func (idx *recordIndex) save(dir string) error {
	if err := writeJSON(filepath.Join(dir, indexFile), &idx.diskIndex); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, dirtyFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PutRecord atomically replaces an artifact and updates its bounded index. The
// dirty marker makes an interrupted two-file update recoverable on next open.
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
	if err := writeJSON(filepath.Join(dir, dirtyFile), true); err != nil {
		return err
	}
	idx.loaded = false
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

// History reads a copy of indexed metadata, never full report artifacts.
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
