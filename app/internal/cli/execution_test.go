package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func TestOpenExecutionCache(t *testing.T) {
	root := t.TempDir()
	repo, output := filepath.Join(root, "repo"), filepath.Join(root, "repo", ".swiftproof")
	os.MkdirAll(repo, 0700)
	var errOut bytes.Buffer
	if cache, err := openExecutionCache(repo, output, "", "test", &errOut); cache != nil || err != nil || errOut.Len() != 0 {
		t.Fatalf("no --cache-dir: %v %v %q", cache, err, errOut.String())
	}
	dir := filepath.Join(root, "cache")
	cache, err := openExecutionCache(repo, output, dir, "test", &errOut)
	if err != nil || cache == nil {
		t.Fatalf("valid directory: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "v1")); err != nil || !info.IsDir() {
		t.Fatalf("layout not created: %v", err)
	}
	if !strings.Contains(errOut.String(), "(baseline-side runs only)") {
		t.Fatalf("stderr %q", errOut.String())
	}
	for name, bad := range map[string]string{
		"inside the repository": filepath.Join(repo, "cache"),
		"inside the output":     filepath.Join(output, "cache"),
		"the repository":        repo,
		"containing the repo":   root,
	} {
		if cache, err := openExecutionCache(repo, output, bad, "test", &errOut); err == nil || cache != nil || !strings.HasPrefix(err.Error(), "--cache-dir: ") {
			t.Errorf("%s: %v %v", name, cache, err)
		}
	}
}

// A cache directory that a checkout or a report could populate is refused
// with exit 3 before any container starts, even when it already holds
// well-formed entries (poisoning).
func TestPlantedCacheLocationsExitThree(t *testing.T) {
	dir := fixture(t)
	planted := filepath.Join(dir, ".swiftproof-cache")
	entry := filepath.Join(planted, "v1", "ab", strings.Repeat("ab", 32)+".json")
	os.MkdirAll(filepath.Dir(entry), 0700)
	os.WriteFile(entry, []byte(`{"body":{},"content_sha256":"`+strings.Repeat("0", 64)+`"}`), 0600)
	outside := t.TempDir()
	cases := map[string][]string{
		"inside the repository":        {"review", "--cache-dir", planted, "--out", filepath.Join(outside, "report")},
		"inside the output":            {"review", "--cache-dir", filepath.Join(outside, "report", "cache"), "--out", filepath.Join(outside, "report")},
		"relative into the repository": {"review", "--cache-dir", ".swiftproof-cache", "--out", filepath.Join(outside, "report")},
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err == nil {
		cases["a symlinked directory"] = []string{"review", "--cache-dir", link, "--out", filepath.Join(outside, "report")}
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			forbidExecution(t)
			if strings.HasPrefix(name, "relative") {
				wd, _ := os.Getwd()
				if err := os.Chdir(dir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chdir(wd) })
			}
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), append(args, "--repo", dir), &stdout, &stderr, "test"); code != 3 {
				t.Fatalf("exit %d, want 3: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--cache-dir") {
				t.Fatalf("stderr %q", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(outside, "report", "confidence-report.json")); !os.IsNotExist(err) {
				t.Fatal("an exit-3 path wrote a report")
			}
		})
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("the refused directory was modified: %v", err)
	}
}

// A valid --cache-dir on a review that executes nothing opens the directory
// and makes no Docker call; the report has no execution object.
func TestCacheDirWithoutExecution(t *testing.T) {
	dir := fixture(t)
	forbidExecution(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	code, r, members, output := runReport(t, context.Background(), dir, "review", "--checks=false", "--reviewer=false", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	if _, ok := members["execution"]; ok || r.Execution != nil {
		t.Fatal("an execution object without a harness")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "v1")); err != nil {
		t.Fatalf("the cache directory was not opened: %v", err)
	}
}

func TestExecutionSummaryAndLine(t *testing.T) {
	if executionSummary(nil, context.Background()) != nil || executionLine(nil) != "" {
		t.Fatal("no harness must give no execution object and no line")
	}
	h, err := harness.New(harness.Options{CandidateDir: t.TempDir(), Parallel: 3, MaxRuntime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	e := executionSummary(h, context.Background())
	if e == nil || e.Cache.Status != model.CacheDisabled || e.Cache.Reason != harness.CacheReasonNotRequested || e.Budget.MaxRuntimeMS != 60000 || e.Budget.DeadlineReached {
		t.Fatalf("summary %+v", e)
	}
	if got := executionLine(e); got != "Initial checks: up to 1 at a time (requested 3)." {
		t.Fatalf("line %q", got)
	}
	e.Parallelism.Requested = 1
	if got := executionLine(e); got != "" {
		t.Fatalf("a review without cache or parallelism printed %q", got)
	}
	e.Cache.Reason = "the sandbox image could not be pinned to an image ID: gone"
	if got := executionLine(e); got != "Execution cache: disabled (the sandbox image could not be pinned to an image ID: gone)." {
		t.Fatalf("line %q", got)
	}
	e.Cache = model.ExecutionCache{Status: model.CacheEnabled, Hits: 2, Stored: 1}
	if got := executionLine(e); got != "Execution cache: 2 baseline results replayed (not executed in this run), 1 recorded; candidate-side runs always execute." {
		t.Fatalf("line %q", got)
	}

	// Only the overall --deadline (its cancellation cause) counts as reached.
	expired, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), harness.ErrOverallDeadline)
	defer cancel()
	if !executionSummary(h, expired).Budget.DeadlineReached {
		t.Fatal("the overall deadline was not reported")
	}
	other, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	if executionSummary(h, other).Budget.DeadlineReached {
		t.Fatal("another expired deadline was reported as the overall --deadline")
	}
}
