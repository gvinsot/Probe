package execcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Store limits. An entry holds at most one recorded log and one payload, each
// bounded by the harness at 4 MiB before base64 encoding.
const (
	layoutDir        = "v1"
	entrySchema      = "swiftproof-execcache-entry/v1"
	maxFileBytes     = 16 << 20
	maxOutputBytes   = 4 << 20
	maxPayloadBytes  = 4<<20 + 64 // the framed payload's header and footer
	maxDurationMS    = int64(24 * time.Hour / time.Millisecond)
	maxLiveRuns      = 1 << 20
	maxProvenance    = 128
	entryTTL         = 30 * 24 * time.Hour
	futureSkew       = 5 * time.Minute
	tempMaxAge       = time.Hour
	maxPutBytesTotal = 256 << 20
	maxFailures      = 3
)

// Size limits of the directory and of one run's writes. They are variables
// only so that tests can lower them.
var (
	maxEntries            = 20000
	maxTotalBytes   int64 = 512 << 20
	maxScannedFiles       = 40000
	maxPutsPerRun         = 256
)

var (
	provenancePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]*$`)
	entryFileName     = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)
)

// Entry mirrors harness.CacheEntry field for field and in the same order, so
// the harness converts between the two directly while this package stays
// below it in the import layering.
type Entry struct {
	Key           string
	Preimage      json.RawMessage
	Status        string
	ExitCode      int
	Output        string
	Truncated     bool
	DurationMS    int64
	Payload       []byte
	LiveRuns      int
	Contradicted  bool
	RecordedAt    time.Time
	RecordedRun   string
	RecordedCheck string
}

// body is the on-disk form of an entry. Output, payload and preimage are
// base64 so that their exact bytes survive, invalid UTF-8 included.
type body struct {
	Schema        string    `json:"schema"`
	Key           string    `json:"key"`
	Preimage      []byte    `json:"preimage"`
	Status        string    `json:"status"`
	ExitCode      int       `json:"exit_code"`
	Output        []byte    `json:"output"`
	Truncated     bool      `json:"truncated"`
	DurationMS    int64     `json:"duration_ms"`
	Payload       []byte    `json:"payload"`
	LiveRuns      int       `json:"live_runs"`
	Contradicted  bool      `json:"contradicted"`
	RecordedAt    time.Time `json:"recorded_at"`
	RecordedRun   string    `json:"recorded_run"`
	RecordedCheck string    `json:"recorded_check"`
}

// envelope is the file: the body's exact bytes and their SHA-256.
type envelope struct {
	Body          json.RawMessage `json:"body"`
	ContentSHA256 string          `json:"content_sha256"`
}

// Store is the on-disk cache in one validated directory. It is safe for
// concurrent use, and several processes may share a directory: entries are
// only ever published by renaming a complete temporary file.
type Store struct {
	mu       sync.Mutex
	dir      string // canonical cache directory
	root     string // dir/v1
	tool     string
	disabled string
	now      func() time.Time

	rejected, evicted int
	puts              int
	putBytes          int64
	failures          int // consecutive I/O failures
}

// Options configures Open.
type Options struct {
	RepoRoot, OutputDir string
	// ToolVersion is ToolIdentity's result; the harness puts it in every key.
	ToolVersion string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Open validates dir (ValidateDir; its error wraps ErrLocation), creates the
// v1 layout and trims expired, temporary and excess files. A failure after
// validation does not fail Open: the returned store is disabled with the
// reason, and every Get misses.
func Open(dir string, o Options) (*Store, error) {
	canonical, err := ValidateDir(dir, o.RepoRoot, o.OutputDir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: canonical, root: filepath.Join(canonical, layoutDir), tool: o.ToolVersion, now: o.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if err := os.Mkdir(s.root, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		s.disabled = "the cache layout could not be created: " + err.Error()
		return s, nil
	}
	if info, err := os.Lstat(s.root); err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		s.disabled = "the cache layout directory is not a plain directory"
		return s, nil
	}
	s.trim()
	return s, nil
}

// Disabled returns a store that serves and records nothing, for a directory
// that passed validation but cannot be used, with the recorded reason.
func Disabled(dir, reason string) *Store {
	return &Store{dir: dir, disabled: reason, now: time.Now}
}

// Dir is the canonical cache directory.
func (s *Store) Dir() string { return s.dir }

// ToolVersion is the tool identity the store was opened with.
func (s *Store) ToolVersion() string { return s.tool }

// DisabledReason is "" while the store is usable, else why it serves and
// records nothing (set at Open or after repeated I/O failures).
func (s *Store) DisabledReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabled
}

// Stats reports what the store observed on its own: entries it rejected on
// Get and entries it removed by age or size (trim, expiry). The harness counts
// hits, misses, stores, write failures and contradictions itself.
func (s *Store) Stats() model.ExecutionCache {
	s.mu.Lock()
	defer s.mu.Unlock()
	return model.ExecutionCache{Rejected: s.rejected, Evicted: s.evicted}
}

func (s *Store) path(key string) string {
	return filepath.Join(s.root, key[:2], key+".json")
}

// Get returns the entry stored under key. It is false on a miss, on an
// expired entry (removed, counted evicted) and on any integrity or bounds
// failure (removed, counted rejected): the content hash, the key recomputed
// from the stored preimage, strict decoding, the file name, the status and
// exit code, the sizes, the age and the provenance fields must all hold.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled != "" || !ValidKey(key) {
		return Entry{}, false
	}
	path := s.path(key)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false
	}
	if err != nil {
		s.failed(err)
		return Entry{}, false
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		s.reject(path)
		return Entry{}, false
	}
	data, err := readBounded(path)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			s.reject(path)
		} else {
			s.failed(err)
		}
		return Entry{}, false
	}
	e, err := decodeEntry(data, key)
	if err != nil {
		s.reject(path)
		return Entry{}, false
	}
	now := s.now()
	if e.RecordedAt.After(now.Add(futureSkew)) {
		s.reject(path)
		return Entry{}, false
	}
	if now.Sub(e.RecordedAt) > entryTTL {
		if os.Remove(path) == nil {
			s.evicted++
		}
		return Entry{}, false
	}
	s.failures = 0
	return e, true
}

// Put validates e as Get would and publishes it atomically under its key,
// replacing any previous entry. Its errors count as write failures in the
// harness and never fail a run. One run stores at most 256 entries and
// 256 MiB.
func (s *Store) Put(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled != "" {
		return errors.New("the execution cache is disabled: " + s.disabled)
	}
	if err := validateEntry(e, e.Key); err != nil {
		return fmt.Errorf("refusing to store an invalid entry: %w", err)
	}
	if e.RecordedAt.After(s.now().Add(futureSkew)) {
		return errors.New("refusing to store an entry recorded in the future")
	}
	data, err := encodeEntry(e)
	if err != nil {
		return err
	}
	if s.puts >= maxPutsPerRun || s.putBytes+int64(len(data)) > maxPutBytesTotal {
		return errors.New("the per-run write limit of the execution cache (256 entries, 256 MiB) was reached")
	}
	if len(data) > maxFileBytes {
		return errors.New("the entry exceeds the 16 MiB file limit")
	}
	shard := filepath.Dir(s.path(e.Key))
	if err := os.Mkdir(shard, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		s.failed(err)
		return err
	}
	if info, err := os.Lstat(shard); err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		err = fmt.Errorf("the cache shard %s is not a plain directory", shard)
		s.failed(err)
		return err
	}
	tmp, err := os.CreateTemp(shard, ".tmp-*")
	if err != nil {
		s.failed(err)
		return err
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmp.Name(), s.path(e.Key))
	}
	if writeErr != nil {
		_ = os.Remove(tmp.Name())
		s.failed(writeErr)
		return writeErr
	}
	s.puts++
	s.putBytes += int64(len(data))
	s.failures = 0
	return nil
}

// Delete removes the entry stored under key; a missing entry is not an error.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidKey(key) {
		return errors.New("invalid cache key")
	}
	if s.root == "" {
		return nil
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.failed(err)
		return err
	}
	return nil
}

// reject removes a file that failed validation and counts it. Caller holds s.mu.
func (s *Store) reject(path string) {
	s.rejected++
	_ = os.Remove(path)
}

// failed counts a consecutive I/O failure; after maxFailures the store
// disables itself with the last error as the reason. Caller holds s.mu.
func (s *Store) failed(err error) {
	s.failures++
	if s.failures >= maxFailures && s.disabled == "" {
		s.disabled = "the cache directory became unusable during the run: " + err.Error()
	}
}

var errTooLarge = errors.New("entry file exceeds its size limit")

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, errTooLarge
	}
	return data, nil
}

// encodeEntry writes the envelope around the body's exact bytes.
func encodeEntry(e Entry) ([]byte, error) {
	raw, err := json.Marshal(body{Schema: entrySchema, Key: e.Key, Preimage: e.Preimage, Status: e.Status, ExitCode: e.ExitCode, Output: []byte(e.Output),
		Truncated: e.Truncated, DurationMS: e.DurationMS, Payload: e.Payload, LiveRuns: e.LiveRuns, Contradicted: e.Contradicted,
		RecordedAt: e.RecordedAt.UTC(), RecordedRun: e.RecordedRun, RecordedCheck: e.RecordedCheck})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`{"body":`)
	b.Write(raw)
	b.WriteString(`,"content_sha256":"`)
	b.WriteString(SHA256Hex(raw))
	b.WriteString(`"}`)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// decodeEntry strictly decodes a file and validates the entry for key.
func decodeEntry(data []byte, key string) (Entry, error) {
	var env envelope
	if err := strictDecode(data, &env); err != nil {
		return Entry{}, err
	}
	if !hexDigest.MatchString(env.ContentSHA256) || SHA256Hex(env.Body) != env.ContentSHA256 {
		return Entry{}, errors.New("content hash mismatch")
	}
	var b body
	if err := strictDecode(env.Body, &b); err != nil {
		return Entry{}, err
	}
	if b.Schema != entrySchema {
		return Entry{}, errors.New("unknown entry schema")
	}
	e := Entry{Key: b.Key, Preimage: json.RawMessage(b.Preimage), Status: b.Status, ExitCode: b.ExitCode, Output: string(b.Output), Truncated: b.Truncated,
		DurationMS: b.DurationMS, Payload: b.Payload, LiveRuns: b.LiveRuns, Contradicted: b.Contradicted, RecordedAt: b.RecordedAt, RecordedRun: b.RecordedRun, RecordedCheck: b.RecordedCheck}
	if len(e.Payload) == 0 {
		e.Payload = nil
	}
	return e, validateEntry(e, key)
}

// validateEntry checks an entry against the key it is filed under: the key
// and the preimage digest agree, the preimage is well formed, the result is a
// completed PASS or FAIL, and every field is within its bounds.
func validateEntry(e Entry, key string) error {
	switch {
	case !ValidKey(key) || e.Key != key:
		return errors.New("the entry key does not match its file")
	case SHA256Hex(e.Preimage) != key:
		return errors.New("the stored preimage does not hash to the key")
	case e.Status != "PASS" && e.Status != "FAIL":
		return errors.New("the stored status is not PASS or FAIL")
	case e.Status == "PASS" && e.ExitCode != 0, e.Status == "FAIL" && (e.ExitCode < 1 || e.ExitCode > 124):
		return errors.New("the stored exit code does not match the status")
	case e.DurationMS < 0 || e.DurationMS > maxDurationMS:
		return errors.New("the stored duration is out of bounds")
	case e.LiveRuns < 1 || e.LiveRuns > maxLiveRuns:
		return errors.New("the stored live-run count is out of bounds")
	case len(e.Output) > maxOutputBytes || len(e.Payload) > maxPayloadBytes:
		return errors.New("the stored output or payload exceeds its bound")
	case e.RecordedAt.IsZero():
		return errors.New("the entry has no recording time")
	case len(e.RecordedRun) > maxProvenance || len(e.RecordedCheck) > maxProvenance || !provenancePattern.MatchString(e.RecordedRun) || !provenancePattern.MatchString(e.RecordedCheck):
		return errors.New("the stored provenance is malformed")
	}
	_, err := DecodePreimage(e.Preimage)
	return err
}

func strictDecode(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// trim runs once at Open: it removes stray temporary files older than an hour
// and entries older than the 30-day age limit, then the oldest entries until
// at most 90% of 20,000 entries and 512 MiB remain. It inspects at most
// 40,000 files; beyond that it removes the whole layout. Everything it removes
// counts as evicted. Errors are ignored: trimming is best effort.
func (s *Store) trim() {
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var entries []file
	var total int64
	scanned := 0
	now := s.now()
	overflow := errors.New("scan limit")
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != s.root && filepath.Dir(path) != s.root {
				return filepath.SkipDir
			}
			return nil
		}
		scanned++
		if scanned > maxScannedFiles {
			return overflow
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			if now.Sub(info.ModTime()) > tempMaxAge {
				_ = os.Remove(path)
			}
		case entryFileName.MatchString(name):
			if now.Sub(info.ModTime()) > entryTTL {
				if os.Remove(path) == nil {
					s.evicted++
				}
				return nil
			}
			entries = append(entries, file{path, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if errors.Is(err, overflow) {
		if os.RemoveAll(s.root) == nil {
			s.evicted += len(entries)
			_ = os.Mkdir(s.root, 0700)
		}
		return
	}
	if len(entries) <= maxEntries && total <= maxTotalBytes {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
	remaining := len(entries)
	for _, f := range entries {
		if remaining <= maxEntries*9/10 && total <= maxTotalBytes*9/10 {
			break
		}
		if os.Remove(f.path) == nil {
			s.evicted++
		}
		remaining--
		total -= f.size
	}
}
