package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/coverage"
	"github.com/gvinsot/SwiftProof/app/internal/execcache"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const (
	pinnedImage = "sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testServer  = "28.4.0 linux/x86_64"
)

// fakeDocker answers the two probe commands and counts them. inspect and info
// return the raw JSON (or error) of each command.
type fakeDocker struct {
	calls   int
	inspect func(ref string) (string, string, error) // stdout, stderr, error
	info    func() (string, error)
}

func (f *fakeDocker) run(_ context.Context, args []string, _ int) ([]byte, []byte, error) {
	f.calls++
	switch {
	case len(args) == 5 && args[0] == "image" && args[1] == "inspect" && args[2] == "--format" && args[3] == "{{json .}}":
		out, stderr, err := f.inspect(args[4])
		return []byte(out), []byte(stderr), err
	case len(args) == 3 && args[0] == "info" && args[1] == "--format" && args[2] == "{{json .}}":
		out, err := f.info()
		return []byte(out), nil, err
	}
	return nil, nil, fmt.Errorf("unexpected docker call %q", args)
}

func goodDocker() *fakeDocker {
	return &fakeDocker{
		inspect: func(string) (string, string, error) {
			return `{"Id":"` + pinnedImage + `","Os":"linux","Architecture":"amd64","Size":1}`, "", nil
		},
		info: func() (string, error) {
			return `{"ServerVersion":"28.4.0","OSType":"linux","Architecture":"x86_64","NCPU":8,"MemTotal":8589934592}`, nil
		},
	}
}

// useDocker installs f as the probe runner for the test.
func useDocker(t *testing.T, f *fakeDocker) {
	t.Helper()
	previous := dockerRunner
	dockerRunner = f.run
	t.Cleanup(func() { dockerRunner = previous })
}

// forbidDocker fails the test on any probe command.
func forbidDocker(t *testing.T) {
	t.Helper()
	previous := dockerRunner
	dockerRunner = func(_ context.Context, args []string, _ int) ([]byte, []byte, error) {
		t.Errorf("unexpected docker call %q", args)
		return nil, nil, errors.New("forbidden")
	}
	t.Cleanup(func() { dockerRunner = previous })
}

// identifiedCache is a memory cache that also reports a tool identity, as
// DiskCache does.
type identifiedCache struct {
	*memoryCache
	tool string
}

func (c identifiedCache) ToolVersion() string { return c.tool }

// unusableCache reports itself disabled, as a store whose directory failed.
type unusableCache struct{ *memoryCache }

func (unusableCache) DisabledReason() string { return "the cache layout could not be created" }

// cachedFixture is fixture() with a cache passed through Options, so that New
// runs newExecState on it (probe and pin), and optional option edits.
func cachedFixture(t *testing.T, cache ExecutionCache, edit func(*Options)) *Harness {
	t.Helper()
	src, base := t.TempDir(), t.TempDir()
	for _, dir := range []string{src, base} {
		if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pkg", "main.go"), []byte("package pkg\nfunc Value() int { return 42 }\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "golang:1.26-bookworm", MaxGeneratedTests: 10, Cache: cache,
		Commands: map[string][]string{"test": {"go", "test", "./..."}, "generated_test": {"go", "test", "{package}"}}}
	if edit != nil {
		edit(&opts)
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// argsExec records the docker argv of every run and passes with plain output
// (and no payload on the capture path).
func argsExec(h *Harness) *[][]string {
	var seen [][]string
	h.execute = func(_ context.Context, _ string, args []string, out io.Writer) execution {
		seen = append(seen, append([]string(nil), args...))
		fmt.Fprint(out, "output\n")
		return execution{ExitCode: 0}
	}
	h.executeCapture = func(_ context.Context, _ string, args []string, log, _ io.Writer) execution {
		seen = append(seen, append([]string(nil), args...))
		fmt.Fprint(log, "output\n")
		return execution{ExitCode: 0}
	}
	return &seen
}

func imageOf(args []string) string {
	for i, arg := range args {
		if arg == "--entrypoint=/bin/sh" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestNoDockerCallWithoutACache(t *testing.T) {
	forbidDocker(t)
	h := cachedFixture(t, nil, nil)
	e := h.Execution()
	if e.Cache.Status != model.CacheDisabled || e.Cache.Reason != CacheReasonNotRequested || e.Cache.PolicySHA256 != "" || e.Cache.ImageID != "" {
		t.Fatalf("cache summary without --cache-dir: %+v", e.Cache)
	}
	if e.Cache.Scope != model.CacheScopeBaseline || e.Cache.Note != model.ExecutionCacheNote || e.ReplayBacked == nil || len(e.ReplayBacked) != 0 {
		t.Fatalf("summary %+v", e)
	}
	if e.Parallelism != (model.ExecutionParallelism{Requested: 1, Effective: 1, Note: "Initial checks run one at a time."}) {
		t.Fatalf("parallelism %+v", e.Parallelism)
	}
	seen := argsExec(h)
	runBase(h, runOptions{})
	if imageOf((*seen)[0]) != "golang:1.26-bookworm" {
		t.Fatal("the image was pinned without a cache")
	}
}

func TestCacheProbesAndPinsTheImage(t *testing.T) {
	docker := goodDocker()
	useDocker(t, docker)
	m := newMemoryCache()
	h := cachedFixture(t, m, nil)
	if docker.calls != 2 {
		t.Fatalf("probe made %d docker calls, want 2", docker.calls)
	}
	seen := argsExec(h)
	// The image is pinned when the harness is created: every run, the
	// initial checks included, executes the probed image ID.
	h.Run(context.Background(), "test")
	first := runBase(h, runOptions{})
	h.Run(context.Background(), "test")
	if got := []string{imageOf((*seen)[0]), imageOf((*seen)[1]), imageOf((*seen)[2])}; got[0] != pinnedImage || got[1] != pinnedImage || got[2] != pinnedImage {
		t.Fatalf("images %q", got)
	}
	if first.Cache == nil || first.Cache.Status != model.CacheStored {
		t.Fatalf("keyed run %+v", first.Cache)
	}
	entry, _ := m.entry(first.Cache.Key)
	p, err := execcache.DecodePreimage(entry.Preimage)
	if err != nil {
		t.Fatal(err)
	}
	if p.ImageID != pinnedImage || p.DockerServer != testServer {
		t.Fatalf("preimage identity %+v", p)
	}
	e := h.Execution()
	if e.Cache.Status != model.CacheEnabled || e.Cache.ImageID != pinnedImage || e.Cache.Runtime != testServer || !cacheKeyPattern.MatchString(e.Cache.PolicySHA256) || e.Cache.Reason != "" {
		t.Fatalf("enabled summary %+v", e.Cache)
	}
}

// A probe failure disables the cache with a reason and changes nothing else:
// runs execute as without a cache, on the configured image.
func TestProbeFailureDisablesOnlyTheCache(t *testing.T) {
	for name, docker := range map[string]*fakeDocker{
		"docker error": {inspect: func(string) (string, string, error) {
			return "", "Cannot connect to the Docker daemon", errors.New("exit status 1")
		}},
		"image absent": {inspect: func(ref string) (string, string, error) {
			return "", "Error response from daemon: No such image: " + ref, errors.New("exit status 1")
		}},
		"invalid ID": {inspect: func(string) (string, string, error) { return `{"Id":"golang:latest","Os":"linux"}`, "", nil }},
		"info error": {inspect: goodDocker().inspect, info: func() (string, error) { return "", errors.New("daemon gone") }},
		"OS mismatch": {inspect: func(string) (string, string, error) {
			return `{"Id":"` + pinnedImage + `","Os":"windows"}`, "", nil
		}, info: goodDocker().info},
	} {
		t.Run(name, func(t *testing.T) {
			useDocker(t, docker)
			m := newMemoryCache()
			h := cachedFixture(t, m, nil)
			seen := argsExec(h)
			for i := 0; i < 3; i++ {
				if c := runBase(h, runOptions{}); c.Cache != nil || c.Status != "PASS" {
					t.Fatalf("run %d: %+v", i, c)
				}
			}
			if m.gets+m.puts+m.deletes != 0 || len(*seen) != 3 || imageOf((*seen)[2]) != "golang:1.26-bookworm" {
				t.Fatalf("the cache was used or execution changed: gets %d puts %d runs %d", m.gets, m.puts, len(*seen))
			}
			e := h.Execution()
			if e.Cache.Status != model.CacheDisabled || !strings.Contains(e.Cache.Reason, "could not be pinned") || e.Cache.ImageID != "" || e.Cache.Hits+e.Cache.Stored+e.Cache.Misses != 0 {
				t.Fatalf("summary %+v", e.Cache)
			}
		})
	}
}

func TestNetworkOrUnusableStoreDisablesTheCacheWithoutProbing(t *testing.T) {
	forbidDocker(t)
	h := cachedFixture(t, newMemoryCache(), func(o *Options) { o.Network = true })
	if c := h.Execution().Cache; c.Status != model.CacheDisabled || !strings.Contains(c.Reason, "networking") {
		t.Fatalf("network: %+v", c)
	}
	h = cachedFixture(t, unusableCache{newMemoryCache()}, nil)
	if c := h.Execution().Cache; c.Status != model.CacheDisabled || c.Reason != "the cache layout could not be created" || c.PolicySHA256 == "" {
		t.Fatalf("unusable store: %+v", c)
	}
	// Without an image nothing executes, so nothing is probed; the cache is kept.
	h = cachedFixture(t, newMemoryCache(), func(o *Options) { o.Image = "" })
	if h.exec.cache == nil || h.Execution().Cache.Status != model.CacheEnabled {
		t.Fatal("a cache without an image was dropped")
	}
}

// The preimage records every §1.11 input, and the key changes with each.
func TestKeyPreimageRecordsEveryInput(t *testing.T) {
	useDocker(t, goodDocker())
	commit := strings.Repeat("c", 40)
	diff, _ := json.Marshal(model.Change{BaseCommit: commit})
	m := identifiedCache{newMemoryCache(), "v-test sha256:" + strings.Repeat("0", 64)}
	h := cachedFixture(t, m, func(o *Options) { o.Diff = string(diff) })
	argsExec(h)
	c := runBase(h, runOptions{timeout: 7 * time.Second})
	entry, _ := m.entry(c.Cache.Key)
	p, err := execcache.DecodePreimage(entry.Preimage)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshotManifest(h.opts.BaseDir)
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := json.Marshal(baseCommand)
	args, _ := json.Marshal(h.dockerArgsScript(cacheKeyContainer, cacheKeySource, wrapperScript, baseCommand))
	want := execcache.Preimage{Schema: execcache.PreimageSchema, ToolVersion: m.tool, Kind: model.CheckGeneratedBase, BaseCommit: commit,
		Tree: execcache.Tree{PristineSHA256: manifestDigest(manifest), Added: [][]string{}}, PolicySHA256: h.exec.policy, ImageID: pinnedImage,
		DockerServer: testServer, DockerArgsSHA256: sha256Hex(args), ArgvSHA256: sha256Hex(argv), Capture: "", TimeoutMS: 7000, MaxOutputBytes: h.opts.MaxOutputBytes}
	if got, _ := json.Marshal(p); string(got) != string(mustJSON(t, want)) {
		t.Fatalf("preimage\n got %s\nwant %s", got, mustJSON(t, want))
	}
	if strings.Contains(string(entry.Preimage), "go\",\"test") || strings.Contains(string(entry.Preimage), h.base) {
		t.Fatalf("the preimage holds raw argv or a temporary path: %s", entry.Preimage)
	}
	if _, ok := manifest["pkg/main.go"]; !ok || manifest["pkg"].typ != "d" {
		t.Fatalf("manifest %+v", manifest)
	}

	// Each input changes the key: a harness identical but for one option.
	keyWith := func(cache ExecutionCache, edit func(*Options), o runOptions) string {
		h := cachedFixture(t, cache, func(opts *Options) {
			opts.Diff = string(diff)
			if edit != nil {
				edit(opts)
			}
		})
		argsExec(h)
		return runBase(h, o).Cache.Key
	}
	reference := keyWith(identifiedCache{newMemoryCache(), m.tool}, nil, runOptions{timeout: 7 * time.Second})
	if reference != c.Cache.Key {
		t.Fatal("two identical harnesses computed different keys")
	}
	for name, key := range map[string]string{
		"tool version": keyWith(identifiedCache{newMemoryCache(), "other"}, nil, runOptions{timeout: 7 * time.Second}),
		"base commit": keyWith(identifiedCache{newMemoryCache(), m.tool}, func(o *Options) {
			d, _ := json.Marshal(model.Change{BaseCommit: strings.Repeat("d", 40)})
			o.Diff = string(d)
		}, runOptions{timeout: 7 * time.Second}),
		"policy command": keyWith(identifiedCache{newMemoryCache(), m.tool}, func(o *Options) { o.Commands["build"] = []string{"go", "build", "./..."} }, runOptions{timeout: 7 * time.Second}),
		"memory limit":   keyWith(identifiedCache{newMemoryCache(), m.tool}, func(o *Options) { o.MemoryMB = 2048 }, runOptions{timeout: 7 * time.Second}),
		"output limit":   keyWith(identifiedCache{newMemoryCache(), m.tool}, func(o *Options) { o.MaxOutputBytes = 64 * 1024 }, runOptions{timeout: 7 * time.Second}),
		"timeout":        keyWith(identifiedCache{newMemoryCache(), m.tool}, nil, runOptions{timeout: 8 * time.Second}),
		"capture":        keyWith(identifiedCache{newMemoryCache(), m.tool}, nil, runOptions{timeout: 7 * time.Second, capture: ResultsPath}),
	} {
		if key == reference || !cacheKeyPattern.MatchString(key) {
			t.Errorf("%s did not change the key", name)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Added entries are hashed into the key; a modified or removed pristine entry
// makes the run uncacheable.
func TestKeyTreeRules(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	argsExec(h)
	uncacheable := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cacheCounts.Uncacheable
	}
	plain := runBase(h, runOptions{})
	preimage := func(c model.Check) execcache.Preimage {
		t.Helper()
		e, _ := m.entry(c.Cache.Key)
		p, err := execcache.DecodePreimage(e.Preimage)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cleanup, err := stageEphemeral(h.base, "pkg/new/staged_test.go", generatedSource)
	if err != nil {
		t.Fatal(err)
	}
	staged := runBase(h, runOptions{})
	cleanup()
	added := preimage(staged).Tree.Added
	if staged.Cache.Key == plain.Cache.Key || len(added) != 2 || added[0][0] != "pkg/new" || added[0][1] != "d" ||
		added[1][0] != "pkg/new/staged_test.go" || added[1][1] != "f" || added[1][2] != fmt.Sprint(len(generatedSource)) || added[1][3] != sha256Hex([]byte(generatedSource)) {
		t.Fatalf("staged file: %v", added)
	}
	if again := runBase(h, runOptions{}); again.Cache.Key != plain.Cache.Key {
		t.Fatal("unstaging did not restore the pristine key")
	}
	if runtime.GOOS != "windows" {
		cleanup, _ = stageEphemeral(h.base, "pkg/tool.sh", "#!/bin/sh\n")
		os.Chmod(filepath.Join(h.base, "pkg", "tool.sh"), 0755)
		executable := runBase(h, runOptions{})
		cleanup()
		if a := preimage(executable).Tree.Added; len(a) != 1 || a[0][1] != "x" {
			t.Fatalf("executable bit: %v", a)
		}
	}
	main := filepath.Join(h.base, "pkg", "main.go")
	original, _ := os.ReadFile(main)
	gets := m.gets
	os.WriteFile(main, []byte("package pkg\nfunc Value() int { return 43 }\n"), 0644)
	if c := runBase(h, runOptions{}); c.Cache != nil || uncacheable() != 1 || m.gets != gets {
		t.Fatalf("a modified pristine file was keyed: %+v uncacheable %d", c.Cache, uncacheable())
	}
	os.Remove(main)
	if c := runBase(h, runOptions{}); c.Cache != nil || uncacheable() != 2 {
		t.Fatalf("a removed pristine file was keyed: %+v", c.Cache)
	}
	os.WriteFile(main, original, 0644)
	if c := runBase(h, runOptions{}); c.Cache == nil || c.Cache.Key != plain.Cache.Key {
		t.Fatal("restoring the pristine file did not restore its key")
	}
}

func TestCaptureOf(t *testing.T) {
	for script, want := range map[string]string{wrapperScript: "", captureScript(ResultsPath): ResultsPath, coverageScript: coverage.ProfilePath} {
		if got, ok := captureOf(script); !ok || got != want {
			t.Errorf("captureOf(%q) = %q, %v", script, got, ok)
		}
	}
	for _, script := range []string{"", "exec \"$@\"", strings.Replace(captureScript(ResultsPath), "exit $s", "exit 0", 1), "if [ -s  ]"} {
		if _, ok := captureOf(script); ok {
			t.Errorf("accepted %q", script)
		}
	}
}

// baseExec serves generated-test runs: a run whose mount is h.base writes the
// next baseline action (the last one repeats), any other run the candidate
// action. An action "garbled" passes without test events.
func baseExec(h *Harness, baseActions []string, candidateAction string) (baseCalls, candidateCalls *int) {
	baseCalls, candidateCalls = new(int), new(int)
	h.execute = func(_ context.Context, _ string, args []string, out io.Writer) execution {
		action := candidateAction
		if strings.Contains(strings.Join(args, " "), "src="+h.base+",") {
			action = baseActions[min(*baseCalls, len(baseActions)-1)]
			*baseCalls++
		} else {
			*candidateCalls++
		}
		switch action {
		case "garbled":
			fmt.Fprintln(out, "ok pkg 0.01s")
			return execution{ExitCode: 0}
		case "fail":
			writeGoEvents(out, "fail")
			return execution{ExitCode: 1}
		}
		writeGoEvents(out, "pass")
		return execution{ExitCode: 0}
	}
	return baseCalls, candidateCalls
}

// runExperiment runs generated-test-1 once and returns its evidence.
func runExperiment(t *testing.T, h *Harness) model.Evidence {
	t.Helper()
	var result struct {
		Evidence model.Evidence `json:"evidence"`
	}
	if err := json.Unmarshal(call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"}), &result); err != nil {
		t.Fatal(err)
	}
	return result.Evidence
}

func checkByID(t *testing.T, h *Harness, id string) model.Check {
	t.Helper()
	for _, c := range h.Checks() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return model.Check{}
}

func replayedChecks(h *Harness) []model.Check {
	var out []model.Check
	for _, c := range h.Checks() {
		if c.Replayed() {
			out = append(out, c)
		}
	}
	return out
}

// Nothing is served before two agreeing live runs; the third experiment's
// baseline is replayed, and because it would reproduce, the baseline runs
// again live and the evidence cites that live check (§1.11, R2).
func TestReplayedBaselineIsConfirmedLiveBeforeReproduction(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	baseCalls, candidateCalls := baseExec(h, []string{"pass"}, "fail")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource, "description": "value stays 42"})
	for i := 1; i <= 2; i++ {
		e := runExperiment(t, h)
		base := checkByID(t, h, e.BaseCheckID)
		if e.Status != model.StatusReproduced || base.Replayed() || base.Cache == nil || base.Cache.LiveRuns != i || *baseCalls != i {
			t.Fatalf("live run %d: evidence %+v base %+v", i, e, base.Cache)
		}
	}
	e := runExperiment(t, h)
	replayed := replayedChecks(h)
	if len(replayed) != 1 || replayed[0].Kind != model.CheckGeneratedBase || replayed[0].Status != "PASS" {
		t.Fatalf("replayed checks %+v", replayed)
	}
	live := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusReproduced || live.Replayed() || live.ID == replayed[0].ID || live.Kind != model.CheckGeneratedBase || live.Status != "PASS" {
		t.Fatalf("evidence %+v rests on %+v", e, live)
	}
	if live.Cache == nil || live.Cache.Status != model.CacheStored || live.Cache.LiveRuns != 3 || *baseCalls != 3 || *candidateCalls != 3 {
		t.Fatalf("live confirmation: cache %+v base calls %d candidate calls %d", live.Cache, *baseCalls, *candidateCalls)
	}
	if !equalStrings(live.Command, replayed[0].Command) || !strings.Contains(e.Description, "replayed from the execution cache, so the baseline was run again live as "+live.ID) {
		t.Fatalf("description %q", e.Description)
	}
	if entry, _ := m.entry(live.Cache.Key); entry.LiveRuns != 3 || entry.RecordedCheck != live.ID {
		t.Fatalf("entry after confirmation %+v", entry)
	}
	if c := h.Execution().Cache; c.Hits != 1 || c.Stored != 3 || c.Contradicted != 0 {
		t.Fatalf("counters %+v", c)
	}
}

// A live re-run that fails contradicts the replayed entry: no reproduction,
// and the entry is gone.
func TestContradictedReplayIsUnverified(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	baseExec(h, []string{"pass", "pass", "fail"}, "fail")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	runExperiment(t, h)
	runExperiment(t, h)
	e := runExperiment(t, h)
	live := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusUnverified || live.Replayed() || live.Status != "FAIL" || !strings.Contains(e.Description, "ended FAIL instead of PASS") {
		t.Fatalf("evidence %+v base %+v", e, live)
	}
	if len(m.keys()) != 0 {
		t.Fatalf("the contradicted entry was kept: %v", m.keys())
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 {
		t.Fatalf("counters %+v", c)
	}
}

// A live re-run that agrees on status and exit code but fails the named-test
// validation the replay passed also contradicts the entry.
func TestReplayContradictedByValidation(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	baseExec(h, []string{"pass", "pass", "garbled"}, "fail")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	runExperiment(t, h)
	runExperiment(t, h)
	e := runExperiment(t, h)
	live := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusUnverified || live.Status != "ERROR" || len(m.keys()) != 0 {
		t.Fatalf("evidence %+v base %+v keys %v", e, live, m.keys())
	}
	// The write-through had recorded the live run as agreeing before the
	// validation failed; its entry is gone, so the check carries no cache
	// provenance any more and the evidence says the entry was removed.
	if live.Cache != nil || !strings.Contains(e.Description, "ended ERROR instead of PASS, so no reproduction is recorded and the cache entry was removed") {
		t.Fatalf("live check cache %+v, description %q", live.Cache, e.Description)
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 || c.WriteFailures != 0 {
		t.Fatalf("counters %+v", c)
	}
}

// When the contradicted entry cannot be removed, the evidence says so and the
// live check keeps the provenance of the entry that is still recorded.
func TestContradictedEntryThatCannotBeRemoved(t *testing.T) {
	for name, action := range map[string]string{"validation": "garbled", "status": "fail"} {
		t.Run(name, func(t *testing.T) {
			h := fixture(t)
			m := useMemoryCache(h)
			baseExec(h, []string{"pass", "pass", action}, "fail")
			call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
			runExperiment(t, h)
			runExperiment(t, h)
			m.mu.Lock()
			m.deleteErr = errors.New("read-only cache directory")
			m.mu.Unlock()
			e := runExperiment(t, h)
			live := checkByID(t, h, e.BaseCheckID)
			if e.Status != model.StatusUnverified || !strings.Contains(e.Description, "and the cache entry could not be removed") || len(m.keys()) != 1 {
				t.Fatalf("evidence %+v keys %v", e, m.keys())
			}
			c := h.Execution().Cache
			if c.Contradicted != 1 || c.Evicted != 0 || c.WriteFailures == 0 {
				t.Fatalf("counters %+v", c)
			}
			if name == "validation" && (live.Cache == nil || live.Cache.Status != model.CacheStored || live.Cache.LiveRuns != 3) {
				t.Fatalf("the provenance of the entry that is still recorded was dropped: %+v", live.Cache)
			}
			if name == "status" && live.Cache != nil {
				t.Fatalf("a disagreeing live run carries provenance: %+v", live.Cache)
			}
		})
	}
}

// A live re-run that does not complete (here: the budget is exhausted) leaves
// the experiment UNVERIFIED and the entry in place.
func TestUnfinishedConfirmationKeepsTheEntry(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	baseExec(h, []string{"pass"}, "fail")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	runExperiment(t, h)
	runExperiment(t, h)
	candidate := h.execute
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		time.Sleep(60 * time.Millisecond) // the candidate run uses up the last 50 ms
		return candidate(ctx, name, args, out)
	}
	h.mu.Lock()
	h.spent = h.opts.MaxRuntime - 50*time.Millisecond
	h.mu.Unlock()
	e := runExperiment(t, h)
	live := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusUnverified || live.Status != "SKIPPED" || live.Output != budgetExhaustedText || !strings.Contains(e.Description, "ended SKIPPED") {
		t.Fatalf("evidence %+v base %+v", e, live)
	}
	if keys := m.keys(); len(keys) != 1 {
		t.Fatalf("keys %v", keys)
	}
	if c := h.Execution().Cache; c.Contradicted != 0 || c.Evicted != 0 {
		t.Fatalf("counters %+v", c)
	}
}

// A negative conclusion may rest on a replay after two agreeing live runs; no
// live re-run is made for it.
func TestNotReproducedMayRestOnAReplay(t *testing.T) {
	h := fixture(t)
	useMemoryCache(h)
	baseCalls, _ := baseExec(h, []string{"pass"}, "pass")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	runExperiment(t, h)
	runExperiment(t, h)
	e := runExperiment(t, h)
	base := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusNotReproduced || !base.Replayed() || base.Cache.LiveRuns != 2 || *baseCalls != 2 {
		t.Fatalf("evidence %+v base %+v calls %d", e, base.Cache, *baseCalls)
	}
	if strings.Contains(e.Description, "replayed from the execution cache") {
		t.Fatalf("a negative conclusion got a confirmation note: %q", e.Description)
	}
}

// A baseline FAIL recorded by two agreeing live runs is never replayed: the
// third baseline runs live, and when it now passes the experiment reproduces
// (a replayed FAIL would have left it UNVERIFIED without any live run).
func TestReplayedBaselineFailIsNeverServed(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	baseCalls, _ := baseExec(h, []string{"fail", "fail", "pass"}, "fail")
	call(t, h, "create_test", map[string]any{"path": "pkg/regression_test.go", "content": generatedSource})
	runExperiment(t, h)
	runExperiment(t, h)
	e := runExperiment(t, h)
	base := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusReproduced || base.Replayed() || base.Status != "PASS" || *baseCalls != 3 || len(replayedChecks(h)) != 0 || len(m.keys()) != 0 {
		t.Fatalf("evidence %+v base %+v calls %d keys %v", e, base, *baseCalls, m.keys())
	}
}

// The Jest-compatible path re-runs the baseline through the results channel.
func TestJestReplayIsConfirmedThroughTheResultsChannel(t *testing.T) {
	h := tsFixture(t)
	useMemoryCache(h)
	baseRuns := 0
	h.executeCapture = func(_ context.Context, _ string, args []string, log, payload io.Writer) execution {
		status, code := "failed", 1
		if strings.Contains(strings.Join(args, " "), "src="+h.base+",") {
			status, code = "passed", 0
			baseRuns++
		}
		fmt.Fprint(log, "vitest log\n")
		fmt.Fprint(payload, coverageFrame(jestResults("src/cart.test.ts", map[string]string{"applies the discount once": status})))
		return execution{ExitCode: code}
	}
	call(t, h, "create_test", map[string]any{"path": "src/cart.test.ts", "content": generatedTSSource})
	runExperiment(t, h)
	runExperiment(t, h)
	e := runExperiment(t, h)
	live := checkByID(t, h, e.BaseCheckID)
	if e.Status != model.StatusReproduced || e.Runner != RunnerJest || live.Replayed() || live.Results == "" || baseRuns != 3 || len(replayedChecks(h)) != 1 {
		t.Fatalf("evidence %+v base %+v runs %d", e, live, baseRuns)
	}
}

// The on-disk store works across harnesses (one per review): two agreeing
// live runs in earlier reviews, then a replay in a later one; a tampered file
// is rejected and replaced by a live run.
func TestDiskCacheAcrossReviews(t *testing.T) {
	useDocker(t, goodDocker())
	root := t.TempDir()
	open := func() *execcache.Store {
		s, err := execcache.Open(filepath.Join(root, "cache"), execcache.Options{RepoRoot: filepath.Join(root, "repo"), ToolVersion: "v-test"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	review := func() (*Harness, model.Check, *int) {
		h := cachedFixture(t, DiskCache(open()), nil)
		calls := countingExec(h, "base output\n", 0)
		return h, runBase(h, runOptions{}), calls
	}
	_, first, _ := review()
	_, second, _ := review()
	if first.Cache == nil || second.Cache == nil || first.Cache.Key != second.Cache.Key || second.Cache.LiveRuns != 2 || second.Replayed() {
		t.Fatalf("live reviews: %+v %+v", first.Cache, second.Cache)
	}
	third, replayed, calls := review()
	if !replayed.Replayed() || *calls != 0 || replayed.Cache.LiveRuns != 2 || replayed.Output != "base output\n" || replayed.Cache.RecordedCheck != second.ID {
		t.Fatalf("third review: %+v calls %d", replayed.Cache, *calls)
	}
	if b := third.Budget(); b.SpentMS != 0 {
		t.Fatalf("a replay was charged: %+v", b)
	}
	if e := third.Execution(); e.Cache.Status != model.CacheEnabled || e.Cache.Hits != 1 || e.Cache.ImageID != pinnedImage {
		t.Fatalf("summary %+v", e.Cache)
	}
	// Tamper with the stored file: the next review rejects it and runs live.
	path := filepath.Join(root, "cache", "v1", replayed.Cache.Key[:2], replayed.Cache.Key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	os.WriteFile(path, data, 0600)
	fourth, live, calls := review()
	if live.Replayed() || *calls != 1 || live.Cache == nil || live.Cache.LiveRuns != 1 {
		t.Fatalf("after tampering: %+v calls %d", live.Cache, *calls)
	}
	if c := fourth.Execution().Cache; c.Rejected != 1 || c.Stored != 1 || c.Hits != 0 {
		t.Fatalf("counters after tampering %+v", c)
	}
}

func TestDiskCacheOfNilStoreIsNil(t *testing.T) {
	if DiskCache(nil) != nil {
		t.Fatal("a nil store must give a nil cache, not a typed nil")
	}
}

func TestBaseCommitAndPolicyDigest(t *testing.T) {
	commit := strings.Repeat("e", 40)
	diff, _ := json.Marshal(model.Change{BaseCommit: commit})
	if got := baseCommitOf(string(diff)); got != commit {
		t.Fatalf("base commit %q", got)
	}
	for _, bad := range []string{"", "diff --git a/x b/x", `{"base_commit":"HEAD"}`} {
		if got := baseCommitOf(bad); got != "" {
			t.Errorf("%q gave %q", bad, got)
		}
	}
	a := Options{Commands: map[string][]string{"test": {"go", "test"}}, Image: "img", Timeout: time.Second}
	b := a
	b.Commands = map[string][]string{"test": {"go", "test", "-v"}}
	if policyDigest(a) == policyDigest(b) || policyDigest(a) != policyDigest(a) || !cacheKeyPattern.MatchString(policyDigest(a)) {
		t.Fatal("policy digest is not a deterministic digest of the commands")
	}
}

func TestParallelismSummary(t *testing.T) {
	h := cachedFixture(t, nil, func(o *Options) { o.Parallel = 3 })
	if p := h.Execution().Parallelism; p.Requested != 3 || p.Effective != 1 || !strings.Contains(p.Note, "one at a time") {
		t.Fatalf("parallelism %+v", p)
	}
}

// BenchmarkCacheKey measures what keying one eligible baseline run costs: the
// tree walk and SHA-256 of every file under h.base (1,000 files of 4 KiB here,
// plus one staged file), the argv digests and the preimage encoding. The
// pristine manifest is computed once, at the first key.
func BenchmarkCacheKey(b *testing.B) {
	base, head := b.TempDir(), b.TempDir()
	content := strings.Repeat("x", 4096)
	for i := 0; i < 1000; i++ {
		dir := filepath.Join(base, fmt.Sprintf("pkg%02d", i%50))
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.go", i)), []byte(content), 0644)
	}
	h, err := New(Options{CandidateDir: head, BaseDir: base, ArtifactDir: b.TempDir(), Image: "img:tag", Commands: map[string][]string{"generated_test": {"go", "test", "{package}"}}})
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	h.exec.cache = newMemoryCache()
	cleanup, err := stageEphemeral(h.base, "pkg00/staged_test.go", generatedSource)
	if err != nil {
		b.Fatal(err)
	}
	defer cleanup()
	h.mu.Lock()
	defer h.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if key, why := h.exec.keyFor(h, model.CheckGeneratedBase, h.base, baseCommand, wrapperScript, time.Minute); key == "" {
			b.Fatal(why)
		}
	}
}
