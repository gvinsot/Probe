package execcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// openTest opens a store in a fresh directory that is outside the fake
// repository and output directory, with a fixed clock.
func openTest(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "cache"), Options{RepoRoot: filepath.Join(root, "repo"), OutputDir: filepath.Join(root, "out"), ToolVersion: "test", Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	if s.DisabledReason() != "" {
		t.Fatalf("store disabled: %s", s.DisabledReason())
	}
	return s
}

// entryFor builds a valid entry whose preimage differs by kind.
func entryFor(t *testing.T, kind string) Entry {
	t.Helper()
	p := validPreimage()
	p.Kind = kind
	raw, key, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Key: key, Preimage: raw, Status: "PASS", ExitCode: 0, Output: "ok\xff\x00 raw bytes\n", DurationMS: 1200, Payload: []byte("frame\x00\x01"),
		LiveRuns: 2, RecordedAt: testNow.Add(-time.Hour), RecordedRun: "0123456789abcdef01234567", RecordedCheck: "check-3"}
}

func sameEntry(a, b Entry) bool {
	return a.Key == b.Key && bytes.Equal(a.Preimage, b.Preimage) && a.Status == b.Status && a.ExitCode == b.ExitCode && a.Output == b.Output &&
		a.Truncated == b.Truncated && a.DurationMS == b.DurationMS && bytes.Equal(a.Payload, b.Payload) && a.LiveRuns == b.LiveRuns &&
		a.Contradicted == b.Contradicted && a.RecordedAt.Equal(b.RecordedAt) && a.RecordedRun == b.RecordedRun && a.RecordedCheck == b.RecordedCheck
}

func TestStoreRoundTripKeepsExactBytes(t *testing.T) {
	s := openTest(t)
	e := entryFor(t, "generated_test_base")
	if _, ok := s.Get(e.Key); ok {
		t.Fatal("hit on an empty store")
	}
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(e.Key)
	if !ok || !sameEntry(got, e) {
		t.Fatalf("round trip: %v\n%+v\n%+v", ok, got, e)
	}
	path := s.path(e.Key)
	if filepath.Base(path) != e.Key+".json" || filepath.Base(filepath.Dir(path)) != e.Key[:2] {
		t.Fatalf("layout %s", path)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700, s.root: 0700} {
			info, err := os.Stat(p)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s mode %v, want %v (%v)", p, info.Mode().Perm(), want, err)
			}
		}
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("raw bytes")) {
		t.Fatal("the output is stored as plain text instead of exact base64 bytes")
	}
	// An agreeing update replaces the entry atomically.
	e.LiveRuns, e.Output = 3, "second"
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(e.Key); got.LiveRuns != 3 || got.Output != "second" {
		t.Fatalf("update %+v", got)
	}
	if err := s.Delete(e.Key); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(e.Key); ok {
		t.Fatal("hit after delete")
	}
	if err := s.Delete(e.Key); err != nil {
		t.Fatalf("deleting a missing entry: %v", err)
	}
	if st := s.Stats(); st.Rejected != 0 || st.Evicted != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// rewrite replaces the stored file of e with the output of edit, which gets
// the decoded body as a generic object; the content hash is recomputed unless
// keepHash is set, so that only the targeted check can reject it.
func rewrite(t *testing.T, s *Store, key string, edit func(body map[string]any), keepHash bool) {
	t.Helper()
	path := s.path(key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Body          json.RawMessage `json:"body"`
		ContentSHA256 string          `json:"content_sha256"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(env.Body, &b); err != nil {
		t.Fatal(err)
	}
	edit(b)
	raw, _ := json.Marshal(b)
	sum := SHA256Hex(raw)
	if keepHash {
		sum = env.ContentSHA256
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"body":%s,"content_sha256":%q}`, raw, sum)), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGetRejectsTamperedEntries(t *testing.T) {
	other := validPreimage()
	other.Kind = "fuzz_base"
	otherRaw, _, _ := other.Encode()
	for name, tamper := range map[string]func(t *testing.T, s *Store, e Entry){
		"flipped byte": func(t *testing.T, s *Store, e Entry) {
			data, _ := os.ReadFile(s.path(e.Key))
			i := bytes.Index(data, []byte(`"status":"PASS"`))
			data[i+10] = 'F'
			os.WriteFile(s.path(e.Key), data, 0600)
		},
		"status changed, hash kept": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["status"] = "FAIL"; b["exit_code"] = 1 }, true)
		},
		"another preimage, hash recomputed": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["preimage"] = otherRaw }, false)
		},
		"key field changed": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["key"] = digest("f") }, false)
		},
		"unknown field": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["served"] = true }, false)
		},
		"wrong schema": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["schema"] = "probe-execcache-entry/v0" }, false)
		},
		"pass with exit 1": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["exit_code"] = 1 }, false)
		},
		"infrastructure exit": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["status"] = "FAIL"; b["exit_code"] = 125 }, false)
		},
		"timeout status": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["status"] = "TIMEOUT"; b["exit_code"] = -1 }, false)
		},
		"negative duration": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["duration_ms"] = -1 }, false)
		},
		"zero live runs": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["live_runs"] = 0 }, false)
		},
		"recorded in the future": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["recorded_at"] = testNow.Add(time.Hour) }, false)
		},
		"malformed provenance": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["recorded_check"] = "check-1\n## Forged" }, false)
		},
		"oversized output": func(t *testing.T, s *Store, e Entry) {
			rewrite(t, s, e.Key, func(b map[string]any) { b["output"] = bytes.Repeat([]byte("x"), maxOutputBytes+1) }, false)
		},
		"trailing data": func(t *testing.T, s *Store, e Entry) {
			f, _ := os.OpenFile(s.path(e.Key), os.O_APPEND|os.O_WRONLY, 0600)
			f.WriteString(`{"body":{}}`)
			f.Close()
		},
		"entry filed under another key": func(t *testing.T, s *Store, e Entry) {
			moved := entryFor(t, "fuzz_base")
			os.MkdirAll(filepath.Dir(s.path(moved.Key)), 0700)
			data, _ := os.ReadFile(s.path(e.Key))
			os.WriteFile(s.path(moved.Key), data, 0600)
			os.Remove(s.path(e.Key))
			// The test then reads under moved.Key below.
		},
		"not a regular file": func(t *testing.T, s *Store, e Entry) {
			os.Remove(s.path(e.Key))
			os.Mkdir(s.path(e.Key), 0700)
		},
		"oversized file": func(t *testing.T, s *Store, e Entry) {
			os.WriteFile(s.path(e.Key), bytes.Repeat([]byte(" "), maxFileBytes+1), 0600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			e := entryFor(t, "generated_test_base")
			if err := s.Put(e); err != nil {
				t.Fatal(err)
			}
			tamper(t, s, e)
			key := e.Key
			if name == "entry filed under another key" {
				key = entryFor(t, "fuzz_base").Key
			}
			if got, ok := s.Get(key); ok {
				t.Fatalf("a tampered entry was returned: %+v", got)
			}
			if st := s.Stats(); st.Rejected != 1 {
				t.Fatalf("rejected %d, want 1", st.Rejected)
			}
			if _, err := os.Lstat(s.path(key)); !os.IsNotExist(err) {
				t.Fatalf("the rejected entry was not removed: %v", err)
			}
			// A fresh, valid entry can be stored again afterwards.
			if name != "entry filed under another key" {
				if err := s.Put(e); err != nil {
					t.Fatal(err)
				}
				if _, ok := s.Get(e.Key); !ok {
					t.Fatal("a valid entry was not stored after a rejection")
				}
			}
		})
	}
}

func TestGetRejectsSymlinkedEntry(t *testing.T) {
	s := openTest(t)
	e := entryFor(t, "generated_test_base")
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	data, _ := os.ReadFile(s.path(e.Key))
	os.WriteFile(target, data, 0600)
	os.Remove(s.path(e.Key))
	if err := os.Symlink(target, s.path(e.Key)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := s.Get(e.Key); ok || s.Stats().Rejected != 1 {
		t.Fatal("an entry reached through a symlink was served")
	}
}

func TestExpiredEntriesAreEvicted(t *testing.T) {
	s := openTest(t)
	e := entryFor(t, "generated_test_base")
	e.RecordedAt = testNow.Add(-entryTTL - time.Minute)
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(e.Key); ok {
		t.Fatal("an expired entry was served")
	}
	if st := s.Stats(); st.Evicted != 1 || st.Rejected != 0 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := os.Stat(s.path(e.Key)); !os.IsNotExist(err) {
		t.Fatal("the expired entry was kept")
	}
}

func TestPutRefusesInvalidEntriesAndCapsWrites(t *testing.T) {
	s := openTest(t)
	valid := entryFor(t, "generated_test_base")
	for name, edit := range map[string]func(*Entry){
		"key without preimage":   func(e *Entry) { e.Preimage = nil },
		"preimage of other key":  func(e *Entry) { e.Key = digest("a") },
		"invalid key":            func(e *Entry) { e.Key = "../../escape" },
		"error status":           func(e *Entry) { e.Status, e.ExitCode = "ERROR", 125 },
		"future recording":       func(e *Entry) { e.RecordedAt = testNow.Add(time.Hour) },
		"unrecorded time":        func(e *Entry) { e.RecordedAt = time.Time{} },
		"oversized payload":      func(e *Entry) { e.Payload = make([]byte, maxPayloadBytes+1) },
		"newline in provenance":  func(e *Entry) { e.RecordedRun = "run\n" },
		"live runs out of range": func(e *Entry) { e.LiveRuns = 0 },
	} {
		e := valid
		edit(&e)
		if err := s.Put(e); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	previous := maxPutsPerRun
	maxPutsPerRun = 2
	t.Cleanup(func() { maxPutsPerRun = previous })
	for i := 0; i < 2; i++ {
		if err := s.Put(valid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(valid); err == nil || !strings.Contains(err.Error(), "per-run write limit") {
		t.Fatalf("third put: %v", err)
	}
}

func TestRepeatedFailuresDisableTheStore(t *testing.T) {
	s := openTest(t)
	e := entryFor(t, "generated_test_base")
	// A regular file where the shard directory belongs makes every put fail.
	if err := os.WriteFile(filepath.Join(s.root, e.Key[:2]), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFailures; i++ {
		if err := s.Put(e); err == nil {
			t.Fatal("a put into a blocked shard succeeded")
		}
	}
	if !strings.Contains(s.DisabledReason(), "became unusable") {
		t.Fatalf("reason %q", s.DisabledReason())
	}
	if _, ok := s.Get(e.Key); ok {
		t.Fatal("a disabled store served an entry")
	}
}

func TestOpenTrimsExpiredTemporaryAndExcessFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	open := func() *Store {
		s, err := Open(dir, Options{RepoRoot: filepath.Join(root, "repo"), ToolVersion: "test", Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	var keys []string
	for i, kind := range []string{"a_base", "b_base", "c_base", "d_base", "e_base"} {
		e := entryFor(t, kind)
		if err := s.Put(e); err != nil {
			t.Fatal(err)
		}
		// Older entries have older modification times.
		os.Chtimes(s.path(e.Key), testNow.Add(-time.Duration(10-i)*time.Hour), testNow.Add(-time.Duration(10-i)*time.Hour))
		keys = append(keys, e.Key)
	}
	expired := entryFor(t, "old_base")
	s.Put(expired)
	os.Chtimes(s.path(expired.Key), testNow.Add(-entryTTL-time.Hour), testNow.Add(-entryTTL-time.Hour))
	oldTemp := filepath.Join(s.root, keys[0][:2], ".tmp-old")
	freshTemp := filepath.Join(s.root, keys[0][:2], ".tmp-fresh")
	os.WriteFile(oldTemp, []byte("x"), 0600)
	os.WriteFile(freshTemp, []byte("x"), 0600)
	os.Chtimes(oldTemp, testNow.Add(-2*time.Hour), testNow.Add(-2*time.Hour))
	os.Chtimes(freshTemp, testNow, testNow)

	previous := maxEntries
	maxEntries = 4
	t.Cleanup(func() { maxEntries = previous })
	s = open()
	// Expired: 1. Then 5 entries > 4: the oldest go until at most 90% of 4
	// (3) remain, so two more.
	if st := s.Stats(); st.Evicted != 3 {
		t.Fatalf("evicted %d, want 3", st.Evicted)
	}
	for i, key := range keys {
		_, err := os.Stat(s.path(key))
		if kept := err == nil; kept != (i >= 2) {
			t.Errorf("entry %d kept=%v", i, kept)
		}
	}
	if _, err := os.Stat(s.path(expired.Key)); !os.IsNotExist(err) {
		t.Error("the expired entry was kept")
	}
	if _, err := os.Stat(oldTemp); !os.IsNotExist(err) {
		t.Error("a stale temporary file was kept")
	}
	if _, err := os.Stat(freshTemp); err != nil {
		t.Error("a fresh temporary file (another process's write in progress) was removed")
	}
}

// Readers never see a partial entry while writers replace it: every Get is a
// miss or a complete, valid entry (run with -race).
func TestConcurrentPutAndGet(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	stores := make([]*Store, 3)
	for i := range stores {
		s, err := Open(dir, Options{RepoRoot: filepath.Join(root, "repo"), ToolVersion: "test", Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}
	e := entryFor(t, "generated_test_base")
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				if i == 0 {
					w := e
					w.LiveRuns = j + 1
					_ = s.Put(w) // a Windows rename conflict is a write failure, never a torn file
					continue
				}
				if got, ok := s.Get(e.Key); ok && (got.Key != e.Key || got.Output != e.Output) {
					t.Errorf("partial entry %+v", got)
				}
			}
		}(i, s)
	}
	wg.Wait()
	for _, s := range stores {
		if st := s.Stats(); st.Rejected != 0 {
			t.Fatalf("a concurrent reader rejected a complete entry: %+v", st)
		}
	}
}

func TestDisabledStoreServesNothing(t *testing.T) {
	s := Disabled("/nowhere", "the running executable could not be hashed")
	e := entryFor(t, "generated_test_base")
	if err := s.Put(e); err == nil {
		t.Fatal("a disabled store accepted a write")
	}
	if _, ok := s.Get(e.Key); ok {
		t.Fatal("a disabled store served an entry")
	}
	if err := s.Delete(e.Key); err != nil {
		t.Fatalf("delete on a disabled store: %v", err)
	}
	if s.DisabledReason() == "" || s.Dir() != "/nowhere" {
		t.Fatal("reason or directory lost")
	}
}

func TestToolIdentityHashesTheExecutable(t *testing.T) {
	id, err := ToolIdentity("v9")
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v9 sha256:"+SHA256Hex(data) {
		t.Fatalf("identity %q", id)
	}
}
