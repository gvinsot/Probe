package graph

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store keeps graphs by commit so that one is built once: the graph of a base
// branch tip serves every review against it, and a review run again on the
// same head reads its graph instead of rebuilding it.
//
// Like the execution cache it lives outside the repository and the report
// directory, owner-only (execcache.ValidateDir). An entry is checked on every
// read (key, content hash, schema, commit, age) and deleted when a check
// fails. These checks detect corruption; they do not authenticate an entry:
// anyone who can write the directory can plant a graph, which is why its
// location is restricted and why graph answers are observations only.
type Store struct {
	dir      string
	identity string // the probe build: version and executable hash
}

// Store bounds.
const (
	maxEntries    = 200
	maxEntryAge   = 30 * 24 * time.Hour
	maxEntryBytes = 512 << 20 // uncompressed
	storeLayout   = "v1"
)

// OpenStore uses a validated cache directory (see execcache.ValidateDir);
// identity is the probe build (execcache.ToolIdentity).
func OpenStore(dir, identity string) *Store {
	return &Store{dir: filepath.Join(dir, storeLayout), identity: identity}
}

// Dir is the directory entries are written to.
func (s *Store) Dir() string { return s.dir }

// key identifies the graph of a commit built by this probe with these
// options: another build, other limits, or a graph without calls never
// reads it.
func (s *Store) key(commit string, calls bool, limits Limits) string {
	preimage, _ := json.Marshal(struct {
		Schema   string `json:"schema"`
		Identity string `json:"identity"`
		Commit   string `json:"commit"`
		Calls    bool   `json:"calls"`
		Limits   Limits `json:"limits"`
	}{Schema, s.identity, commit, calls, limits})
	sum := sha256.Sum256(preimage)
	return hex.EncodeToString(sum[:])
}

func (s *Store) path(key string) string {
	return filepath.Join(s.dir, key[:2], key+".json.gz")
}

// envelope is the stored form of a graph.
type envelope struct {
	Key           string          `json:"key"`
	ContentSHA256 string          `json:"content_sha256"`
	Graph         json.RawMessage `json:"graph"`
}

// Get returns the stored graph of a commit, if a valid one exists.
func (s *Store) Get(commit string, calls bool, limits Limits) (*Graph, bool) {
	key := s.key(commit, calls, limits)
	p := s.path(key)
	info, err := os.Lstat(p)
	if err != nil {
		return nil, false
	}
	bad := func() (*Graph, bool) {
		os.Remove(p)
		return nil, false
	}
	if !info.Mode().IsRegular() || time.Since(info.ModTime()) > maxEntryAge {
		return bad()
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	var data []byte
	zr, err := gzip.NewReader(f)
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(zr, maxEntryBytes+1))
	}
	// Closed before any removal: Windows does not delete an open file.
	f.Close()
	if err != nil || len(data) > maxEntryBytes {
		return bad()
	}
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.Key != key {
		return bad()
	}
	sum := sha256.Sum256(env.Graph)
	if hex.EncodeToString(sum[:]) != env.ContentSHA256 {
		return bad()
	}
	var g Graph
	dec := json.NewDecoder(bytes.NewReader(env.Graph))
	dec.DisallowUnknownFields()
	if dec.Decode(&g) != nil || g.Schema != Schema || g.Commit != commit || g.Calls != calls {
		return bad()
	}
	// A hit keeps the entry young, so that a base tip in use is not evicted.
	now := time.Now()
	os.Chtimes(p, now, now)
	return &g, true
}

// Put stores the graph of a commit, replacing any entry, then evicts the
// oldest entries beyond maxEntries.
func (s *Store) Put(g *Graph, limits Limits) error {
	body, err := json.Marshal(g)
	if err != nil {
		return err
	}
	if len(body) > maxEntryBytes {
		return fmt.Errorf("graph of %d bytes exceeds the cache entry limit", len(body))
	}
	key := s.key(g.Commit, g.Calls, limits)
	sum := sha256.Sum256(body)
	data, err := json.Marshal(envelope{Key: key, ContentSHA256: hex.EncodeToString(sum[:]), Graph: body})
	if err != nil {
		return err
	}
	p := s.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	_, werr := zw.Write(data)
	cerr := zw.Close()
	ferr := tmp.Close()
	if err := errors.Join(werr, cerr, ferr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.evict()
	return nil
}

// evict removes expired entries and the oldest beyond maxEntries.
func (s *Store) evict() {
	type entry struct {
		path string
		mod  time.Time
	}
	var entries []entry
	filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".gz" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			entries = append(entries, entry{p, info.ModTime()})
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	for i, e := range entries {
		if i >= maxEntries || time.Since(e.mod) > maxEntryAge {
			os.Remove(e.path)
		}
	}
}
