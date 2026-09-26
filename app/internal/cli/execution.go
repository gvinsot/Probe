package cli

// The opt-in baseline execution cache and the execution summary (F7a). No
// cache is ever used without an explicit --cache-dir, and the directory is
// validated before dependency preparation and before any container starts.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gvinsot/SwiftProof/app/internal/execcache"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// openExecutionCache validates --cache-dir (location, ownership, symlinks) and
// opens the cache. It returns nil, nil when cacheDir is "" and makes no Docker
// call in any case. Its error exits 3: the directory is inside, equal to or
// contains the repository or the output directory, is a link or reparse point,
// is not a directory, cannot be created, or (on Unix) is not owned by the
// effective user or grants group or other permissions.
//
// A problem that does not concern the location (the running executable cannot
// be hashed for the tool identity, the layout cannot be created) disables the
// cache with a recorded reason instead: the review runs without it, and the
// exit code is unchanged.
func openExecutionCache(repoRoot, outputDir, cacheDir, version string, errOut io.Writer) (harness.ExecutionCache, error) {
	if cacheDir == "" {
		return nil, nil
	}
	tool, toolErr := execcache.ToolIdentity(version)
	store, err := execcache.Open(cacheDir, execcache.Options{RepoRoot: repoRoot, OutputDir: outputDir, ToolVersion: tool})
	if err != nil {
		return nil, fmt.Errorf("--cache-dir: %w", err) // every Open error is a location error (execcache.ErrLocation)
	}
	if toolErr != nil {
		store = execcache.Disabled(store.Dir(), "the running executable could not be hashed for the cache key: "+toolErr.Error())
	}
	if reason := store.DisabledReason(); reason != "" {
		fmt.Fprintf(errOut, "Execution cache disabled: %s\n", harness.Redact(reason))
	} else {
		fmt.Fprintf(errOut, "Execution cache: %s (baseline-side runs only).\n", store.Dir())
	}
	return harness.DiskCache(store), nil
}

// initialChecks returns the configured initial check kinds in their fixed
// order: test, typecheck, build.
func initialChecks(commands map[string][]string) []string {
	var kinds []string
	for _, kind := range []string{"test", "typecheck", "build"} {
		if _, ok := commands[kind]; ok {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// executionSummary returns the report's execution object (cache, parallelism,
// budget); nil when no harness ran. Budget.DeadlineReached is true when work
// ended because the overall --deadline expired (its cancellation cause).
// replay_backed is filled by report.Finalize from verified evidence.
func executionSummary(h *harness.Harness, work context.Context) *model.Execution {
	if h == nil {
		return nil
	}
	e := h.Execution()
	e.Budget.DeadlineReached = work != nil && errors.Is(work.Err(), context.DeadlineExceeded) && errors.Is(context.Cause(work), harness.ErrOverallDeadline)
	return &e
}

// executionLine is the stdout line of the execution summary. It is empty when
// no harness ran, or when the cache was not requested and the initial checks
// were not asked to run concurrently: a review that uses neither feature
// prints what it printed before.
func executionLine(e *model.Execution) string {
	if e == nil {
		return ""
	}
	parallel := ""
	if e.Parallelism.Requested > 1 {
		parallel = fmt.Sprintf(" Initial checks: up to %d at a time (requested %d).", e.Parallelism.Effective, e.Parallelism.Requested)
	}
	c := e.Cache
	switch {
	case c.Status == model.CacheEnabled:
		return fmt.Sprintf("Execution cache: %d baseline results replayed (not executed in this run), %d recorded; candidate-side runs always execute.%s", c.Hits, c.Stored, parallel)
	case c.Hits+c.Stored+c.Misses+c.Uncacheable+c.Rejected+c.WriteFailures+c.Evicted+c.Contradicted > 0:
		// The store disabled itself during the run: what it replayed before
		// must still show.
		return fmt.Sprintf("Execution cache: disabled during the run (%s) after %d baseline results replayed (not executed in this run), %d recorded; candidate-side runs always execute.%s", c.Reason, c.Hits, c.Stored, parallel)
	case c.Reason == harness.CacheReasonNotRequested:
		if parallel == "" {
			return ""
		}
		return parallel[1:]
	}
	return fmt.Sprintf("Execution cache: disabled (%s).%s", c.Reason, parallel)
}
