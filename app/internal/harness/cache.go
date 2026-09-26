package harness

// The opt-in baseline execution cache (contract §1.11), harness side. run.go
// holds the consult/store point, the replay, the write-through and the
// agreement counting; this file decides what a run's key is, pins the image,
// confirms a replayed baseline live before a reproduction, and summarizes the
// cache for the report.
//
// run.go relies on execState.cache, keyFor and preimage: keyFor records the
// preimage of every key it returns, and run.go stores it on each new entry so
// that the store can recompute the key from what it holds. Execution reports
// h.cacheCountersLocked(), the harness counters plus Stats().

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/execcache"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// CacheReasonNotRequested is the reason recorded for a review that did not
// pass --cache-dir: the cache is opt-in.
const CacheReasonNotRequested = "not requested (the execution cache is opt-in with --cache-dir)"

// Fixed placeholders of the docker argv that goes into a key, so that the
// random container name and the temporary snapshot path never change it.
const (
	cacheKeyContainer = "swiftproof-cachekey"
	cacheKeySource    = "/cachekey-source"
)

// Tree limits, the same as copySnapshot's.
const (
	treeMaxFiles     = 100000
	treeMaxFileBytes = 32 << 20
	treeMaxBytes     = 512 << 20
)

var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

type execState struct {
	cache     ExecutionCache
	preimages map[string]json.RawMessage // key -> the canonical preimage keyFor hashed

	requested  bool   // Options.Cache was set
	reason     string // why no cache is used; "" while one is
	imageID    string // the probed image ID; keyed runs, and every run after the first keyed one, use it
	server     string // Docker server version, OS type and architecture
	policy     string // policy_sha256: the execution settings of the trusted policy
	tool       string // tool_version of every key (the store's tool identity)
	baseCommit string // the base commit of the change, from Options.Diff

	// The manifest of the base snapshot as copied, computed at first use.
	pristine       map[string]treeEntry
	pristineSHA256 string
	pristineErr    error

	// parallel summarizes the initial checks' concurrency. RunChecks (F7b)
	// records the effective value and its note when it runs checks
	// concurrently; until then the initial checks run one at a time.
	parallel model.ExecutionParallelism
}

// treeEntry is one manifest entry: type "f" (file), "x" (executable file) or
// "d" (directory; no size or digest).
type treeEntry struct {
	typ    string
	size   int64
	sha256 string
}

func (e treeEntry) tuple(path string) []string {
	if e.typ == "d" {
		return []string{path, "d", "", ""}
	}
	return []string{path, e.typ, strconv.FormatInt(e.size, 10), e.sha256}
}

// toolIdentity and disabledCache are implemented by DiskCache: the tool
// identity that goes into every key, and a reason the store cannot be used.
type toolIdentity interface{ ToolVersion() string }
type disabledCache interface{ DisabledReason() string }

// newExecState is newExecStateContext without a caller context: the probe is
// bounded by probeTimeout alone. New calls it; see newExecStateContext.
func newExecState(opts Options) (execState, error) {
	return newExecStateContext(context.Background(), opts)
}

// newExecStateContext stores opts.Cache and validates opts.Parallel (0 means
// 1). It makes no Docker call without a cache. With one, it probes the sandbox
// image and the Docker server (probeDocker, at most probeTimeout in total and
// never beyond ctx); any failure to probe or pin the image, a store that
// reports itself unusable, or sandbox networking disables the cache with a
// recorded reason. It never fails for a cache reason.
//
// A harness constructor that has the review's work context (--deadline and
// cancellation) passes it here, so that the probe is bounded by it as well.
func newExecStateContext(ctx context.Context, opts Options) (execState, error) {
	if opts.Parallel < 0 || opts.Parallel > 4 {
		return execState{}, fmt.Errorf("parallel must be between 1 and 4, got %d", opts.Parallel)
	}
	requested := opts.Parallel
	if requested == 0 {
		requested = 1
	}
	s := execState{policy: policyDigest(opts), baseCommit: baseCommitOf(opts.Diff),
		parallel: model.ExecutionParallelism{Requested: requested, Effective: 1, Note: sequentialNote(requested)}}
	if opts.Cache == nil {
		s.reason = CacheReasonNotRequested
		return s, nil
	}
	s.requested = true
	if id, ok := opts.Cache.(toolIdentity); ok {
		s.tool = id.ToolVersion()
	}
	if d, ok := opts.Cache.(disabledCache); ok {
		if reason := d.DisabledReason(); reason != "" {
			s.reason = reason
			return s, nil
		}
	}
	switch {
	case opts.Network:
		s.reason = "sandbox networking is enabled for this review, and runs with network access are never cached"
		return s, nil
	case opts.Image == "":
		// Nothing executes without an image (every run is SKIPPED before the
		// cache is consulted), so there is nothing to probe or pin.
		s.cache = opts.Cache
		return s, nil
	}
	identity, err := probeDocker(ctx, dockerRunner, opts.Image)
	if err != nil {
		s.reason = truncateUTF8("the sandbox image could not be pinned to an image ID: "+Redact(err.Error()), 512)
		return s, nil
	}
	s.cache, s.imageID, s.server = opts.Cache, identity.ImageID, truncateUTF8(Redact(identity.Server), 256)
	return s, nil
}

// sequentialNote is the parallelism note of a harness whose initial checks run
// one at a time.
func sequentialNote(requested int) string {
	if requested > 1 {
		return "Initial checks run one at a time: this build does not run them concurrently."
	}
	return "Initial checks run one at a time."
}

// policyDigest is the policy_sha256 of the keys: the SHA-256 of the execution
// settings the trusted policy gave this harness (every command, the configured
// image reference, network, per-run timeout, runtime budget, output, memory
// and CPU limits, generated-test budget). Reviewer settings are not included:
// they do not change what a sandbox run executes.
func policyDigest(opts Options) string {
	raw, _ := json.Marshal(struct {
		Schema            string              `json:"schema"`
		Commands          map[string][]string `json:"commands"`
		Image             string              `json:"image"`
		Network           bool                `json:"network"`
		TimeoutMS         int64               `json:"timeout_ms"`
		MaxRuntimeMS      int64               `json:"max_runtime_ms"`
		MaxOutputBytes    int                 `json:"max_output_bytes"`
		MemoryMB          int                 `json:"memory_mb"`
		CPUs              int                 `json:"cpus"`
		MaxGeneratedTests int                 `json:"max_generated_tests"`
	}{"swiftproof-execpolicy/v1", opts.Commands, opts.Image, opts.Network, opts.Timeout.Milliseconds(), opts.MaxRuntime.Milliseconds(), opts.MaxOutputBytes, opts.MemoryMB, opts.CPUs, opts.MaxGeneratedTests})
	return sha256Hex(raw)
}

// baseCommitOf reads the base commit from Options.Diff, the JSON of the
// change the CLI passes; "" when the diff is not such a document.
func baseCommitOf(diff string) string {
	var change struct {
		BaseCommit string `json:"base_commit"`
	}
	if json.Unmarshal([]byte(diff), &change) != nil || !commitPattern.MatchString(change.BaseCommit) {
		return ""
	}
	return change.BaseCommit
}

// keyFor returns the cache key of an eligible run (a baseline-side kind on
// h.base without network; run.go checks that), or why it cannot be keyed.
// The key is the SHA-256 of the canonical preimage (execcache.Preimage): tool
// identity, kind, base commit, the tree the run sees, the policy digest, the
// pinned image ID, the Docker server, the digest of the complete docker argv
// built with fixed placeholders, the argv digest, the capture path, the
// per-run timeout and the output limit. Caller holds h.mu.
//
// The first keyed run pins h.opts.Image to the probed image ID, so that every
// keyed run (and every run after it) executes exactly the keyed image. A
// cache installed on a harness without newExecState (tests only) has no
// probed identity: its keys use the configured image reference.
func (s *execState) keyFor(h *Harness, kind, dir string, argv []string, script string, timeout time.Duration) (key, uncacheableReason string) {
	image := s.imageID
	if image == "" {
		image = h.opts.Image
	}
	if image == "" {
		return "", "no sandbox image is configured"
	}
	if s.imageID != "" && h.opts.Image != s.imageID {
		h.opts.Image = s.imageID
	}
	capture, ok := captureOf(script)
	if !ok {
		return "", "the run uses an unrecognized wrapper script"
	}
	tree, err := s.treeOf(h, dir)
	if err != nil {
		return "", "the baseline tree cannot be keyed: " + Redact(err.Error())
	}
	args, err := json.Marshal(h.dockerArgsScript(cacheKeyContainer, cacheKeySource, script, argv))
	if err != nil {
		return "", "the sandbox arguments could not be encoded"
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return "", "the command could not be encoded"
	}
	raw, key, err := execcache.Preimage{
		Schema: execcache.PreimageSchema, ToolVersion: s.tool, Kind: kind, BaseCommit: s.baseCommit, Tree: tree,
		PolicySHA256: s.policy, ImageID: image, DockerServer: s.server, DockerArgsSHA256: sha256Hex(args),
		ArgvSHA256: sha256Hex(argvJSON), Capture: capture, TimeoutMS: timeout.Milliseconds(), MaxOutputBytes: h.opts.MaxOutputBytes,
	}.Encode()
	if err != nil {
		return "", "the key preimage could not be encoded: " + err.Error()
	}
	if s.preimages == nil {
		s.preimages = map[string]json.RawMessage{}
	}
	s.preimages[key] = raw
	return key, ""
}

// preimage returns the canonical preimage keyFor hashed into key, or nil when
// this harness did not compute key. run.go stores it on every new entry so the
// store can recompute the key from what it holds.
func (s *execState) preimage(key string) json.RawMessage {
	if raw, ok := s.preimages[key]; ok {
		return append(json.RawMessage(nil), raw...)
	}
	return nil
}

// captureOf returns the in-container payload path of a wrapper script built by
// captureScript, "" for wrapperScript, and false for any other script.
func captureOf(script string) (string, bool) {
	if script == wrapperScript {
		return "", true
	}
	const open, closing = "if [ -s ", " ]"
	i := strings.Index(script, open)
	if i < 0 {
		return "", false
	}
	rest := script[i+len(open):]
	j := strings.Index(rest, closing)
	if j <= 0 {
		return "", false
	}
	path := rest[:j]
	if captureScript(path) != script {
		return "", false
	}
	return path, true
}

// treeOf describes the tree under dir (h.base) for a key: the digest of the
// pristine manifest of the base snapshot, and every entry a caller added
// (staged tests, harness files). A pristine entry that was modified or
// removed, a symlink or special file, or a tree beyond the snapshot limits
// makes the run uncacheable. Caller holds h.mu.
func (s *execState) treeOf(h *Harness, dir string) (execcache.Tree, error) {
	if s.pristine == nil && s.pristineErr == nil {
		s.pristine, s.pristineErr = snapshotManifest(h.opts.BaseDir)
		if s.pristineErr == nil {
			s.pristineSHA256 = manifestDigest(s.pristine)
		}
	}
	if s.pristineErr != nil {
		return execcache.Tree{}, fmt.Errorf("the base snapshot could not be hashed: %w", s.pristineErr)
	}
	seen := make(map[string]bool, len(s.pristine))
	added := [][]string{}
	files, total := 0, int64(0)
	buf := make([]byte, 64<<10)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		var entry treeEntry
		switch {
		case d.IsDir():
			entry.typ = "d"
		case d.Type().IsRegular():
			files++
			if entry, err = hashFile(path, buf); err != nil {
				return err
			}
			total += entry.size
			if files > treeMaxFiles || entry.size > treeMaxFileBytes || total > treeMaxBytes {
				return errors.New("the tree exceeds the snapshot limits (100,000 files, 32 MiB per file, 512 MiB in total)")
			}
		default:
			return fmt.Errorf("%s is a symlink or special file", rel)
		}
		if pristine, ok := s.pristine[rel]; ok {
			if pristine != entry {
				return fmt.Errorf("the pristine baseline entry %s was modified", rel)
			}
			seen[rel] = true
			return nil
		}
		added = append(added, entry.tuple(rel))
		return nil
	})
	if err != nil {
		return execcache.Tree{}, err
	}
	if len(seen) != len(s.pristine) {
		for _, path := range sortedPaths(s.pristine) {
			if !seen[path] {
				return execcache.Tree{}, fmt.Errorf("the pristine baseline entry %s was removed", path)
			}
		}
	}
	return execcache.Tree{PristineSHA256: s.pristineSHA256, Added: added}, nil
}

// snapshotManifest describes what copySnapshot copies from root: every
// directory and regular file, except secret-bearing names and symlinks, with
// the executable bit, size and SHA-256 of each file.
func snapshotManifest(root string) (map[string]treeEntry, error) {
	if root == "" {
		return nil, errors.New("no base snapshot")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the base snapshot is not a real directory")
	}
	manifest := map[string]treeEntry{}
	files, total := 0, int64(0)
	buf := make([]byte, 64<<10)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if sensitivePath(rel) || d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			manifest[rel] = treeEntry{typ: "d"}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		entry, err := hashFile(path, buf)
		if err != nil {
			return err
		}
		files++
		total += entry.size
		if files > treeMaxFiles || entry.size > treeMaxFileBytes || total > treeMaxBytes {
			return errors.New("the base snapshot exceeds the snapshot limits")
		}
		manifest[rel] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// hashFile returns the manifest entry of one regular file, reading through
// buf so that a walk of many files reuses one buffer.
func hashFile(path string, buf []byte) (treeEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return treeEntry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return treeEntry{}, err
	}
	digest := sha256.New()
	n, err := io.CopyBuffer(digest, io.LimitReader(f, treeMaxFileBytes+1), buf)
	if err != nil {
		return treeEntry{}, err
	}
	typ := "f"
	if info.Mode()&0111 != 0 {
		typ = "x"
	}
	return treeEntry{typ: typ, size: n, sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// manifestDigest is the SHA-256 of the manifest's [path, type, size, sha256]
// tuples in path order.
func manifestDigest(manifest map[string]treeEntry) string {
	tuples := make([][]string, 0, len(manifest))
	for _, path := range sortedPaths(manifest) {
		tuples = append(tuples, manifest[path].tuple(path))
	}
	raw, _ := json.Marshal(tuples)
	return sha256Hex(raw)
}

func sortedPaths(manifest map[string]treeEntry) []string {
	paths := make([]string, 0, len(manifest))
	for path := range manifest {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// confirmBaseline runs the baseline of a generated experiment again, live,
// when its first baseline result was replayed from the cache and the
// experiment would otherwise be recorded as REPRODUCED (§1.11: a replay never
// supports a positive status). The re-run keeps the kind generated_test_base,
// the staged file and the command, and is never served from the cache; its
// result is written through (agreement or eviction). The replayed check stays
// in the ledger. It returns the live check, which the evidence then cites, the
// status and a note for the evidence description:
//   - live PASS (validated): REPRODUCED;
//   - live run completed but did not pass (validated): UNVERIFIED, and the
//     entry that was replayed is removed as contradicted. When the removal
//     succeeds, the live check loses the cache provenance the write-through
//     gave it (it no longer describes a recorded entry), and the note says
//     whether the entry was removed;
//   - live run did not complete (SKIPPED, TIMEOUT, ERROR): UNVERIFIED.
//
// Caller holds h.mu.
func (h *Harness) confirmBaseline(ctx context.Context, runner, path string, names, command []string, base model.Check) (model.Check, string, string) {
	var live model.Check
	if runner == RunnerJest {
		live = h.runWithResultsOptions(ctx, model.CheckGeneratedBase, h.base, command, runOptions{live: true})
	} else {
		live, _, _ = h.runWithOptions(ctx, model.CheckGeneratedBase, h.base, command, runOptions{live: true})
	}
	executed := live // as run.go recorded and wrote it through, before the named-test validation
	completed := executed.Status == "PASS" || executed.Status == "FAIL"
	if runner != "" {
		live, _ = ValidateExecution(runner, live, path, names)
		h.replaceCheck(live)
	}
	if live.Status == "PASS" {
		return live, model.StatusReproduced, fmt.Sprintf(" (the baseline result %s was replayed from the execution cache, so the baseline was run again live as %s before this reproduction was recorded)", base.ID, live.ID)
	}
	if completed && base.Cache != nil {
		outcome := "the cache entry could not be removed"
		if h.evictContradicted(base.Cache.Key, executed) {
			outcome = "the cache entry was removed"
			if live.Cache != nil {
				live.Cache = nil
				h.replaceCheck(live)
			}
		}
		return live, model.StatusUnverified, fmt.Sprintf(" (the baseline result %s was replayed from the execution cache; its live re-run %s ended %s instead of PASS, so no reproduction is recorded and %s)", base.ID, live.ID, live.Status, outcome)
	}
	return live, model.StatusUnverified, fmt.Sprintf(" (the baseline result %s was replayed from the execution cache; its live re-run %s ended %s, so no reproduction is recorded)", base.ID, live.ID, live.Status)
}

// evictContradicted removes the entry stored under key after a completed live
// re-run (executed, before validation) disagreed with the replay, and reports
// whether no entry remains. run.go's write-through already removed and
// counted an entry whose status, exit code or truncation differed from
// executed; an entry that still agrees with executed was extended by that
// write-through although the live run then failed the named-test validation
// the replay passed, which is a contradiction counted here. A removal that
// fails counts as a write failure and leaves the entry. Caller holds h.mu.
func (h *Harness) evictContradicted(key string, executed model.Check) bool {
	if h.exec.cache == nil || !cacheKeyPattern.MatchString(key) {
		return false
	}
	prior, found := h.exec.cache.Get(key)
	if found && prior.Key == key && prior.Status == executed.Status && prior.ExitCode == executed.ExitCode && prior.Truncated == executed.Truncated {
		h.cacheCounts.Contradicted++
	}
	if err := h.exec.cache.Delete(key); err != nil {
		h.cacheCounts.WriteFailures++
		return false
	}
	if found {
		h.cacheCounts.Evicted++
	}
	return true
}

// Execution summarizes the cache, the initial checks' parallelism and the
// budget. The cache counters are h.cacheCountersLocked() (the harness's own
// plus the store's); the status, scope, identity and note are filled here.
// ReplayBacked is left empty: report.Finalize fills it from verified evidence.
// The caller sets Budget.DeadlineReached, which depends on its context.
func (h *Harness) Execution() model.Execution {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cacheCountersLocked()
	c.Scope, c.Note = model.CacheScopeBaseline, model.ExecutionCacheNote
	reason := h.exec.reason
	if h.exec.cache != nil {
		if d, ok := h.exec.cache.(disabledCache); ok {
			if r := d.DisabledReason(); r != "" {
				reason = r
			}
		}
	}
	if h.exec.requested {
		c.PolicySHA256 = h.exec.policy
	}
	if h.exec.cache != nil && reason == "" {
		c.Status = model.CacheEnabled
		if dockerutil.ValidImageID(h.exec.imageID) {
			c.ImageID = h.exec.imageID
		}
		c.Runtime = h.exec.server
	} else {
		c.Status, c.Reason = model.CacheDisabled, reason
	}
	return model.Execution{Cache: c, Parallelism: h.exec.parallel, Budget: h.budgetLocked(), ReplayBacked: []string{}}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiskCache adapts the on-disk store to ExecutionCache; it returns nil for a
// nil store. It also exposes the store's tool identity and disabled reason to
// newExecState and Execution.
func DiskCache(s *execcache.Store) ExecutionCache {
	if s == nil {
		return nil
	}
	return diskCache{s}
}

type diskCache struct{ s *execcache.Store }

func (d diskCache) Get(key string) (CacheEntry, bool) {
	e, ok := d.s.Get(key)
	return CacheEntry(e), ok
}
func (d diskCache) Put(e CacheEntry) error      { return d.s.Put(execcache.Entry(e)) }
func (d diskCache) Delete(key string) error     { return d.s.Delete(key) }
func (d diskCache) Stats() model.ExecutionCache { return d.s.Stats() }
func (d diskCache) ToolVersion() string         { return d.s.ToolVersion() }
func (d diskCache) DisabledReason() string      { return d.s.DisabledReason() }
