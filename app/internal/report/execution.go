package report

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// This file belongs to F7a (execution cache): the replay-backed list, the
// per-check cache notes and the execution summary of "## Automated Checks".

// negativeStatuses are the evidence statuses that may rest on a replayed
// baseline once two agreeing live runs recorded it (§1.11). Every other
// status needs a live baseline and never appears in replay_backed.
var negativeStatuses = map[string]bool{
	model.StatusNotReproduced:     true,
	model.StatusNotDiverged:       true,
	model.StatusPassesOnCandidate: true,
}

// finalizeExecution lists, sorted, every verified evidence ID whose negative
// conclusion rests on a replayed baseline in r.Execution.ReplayBacked, and
// normalizes the execution notes. It may mutate only r.Execution and never
// changes a status or the exit code (F7a). It is idempotent: the list is
// recomputed from the ledger every time, never read back.
//
// An evidence record is replay-backed when the ledger verified a negative
// status for it and one of the checks it cites (base, candidate or repeat) is
// a replay. The verifiers accept a replayed baseline for a negative status
// only with two agreeing live runs, so every listed record has one.
func finalizeExecution(r *model.Report, l *ledger) {
	if r.Execution == nil {
		return
	}
	e := r.Execution
	e.Cache.Scope, e.Cache.Note = model.CacheScopeBaseline, model.ExecutionCacheNote
	if e.Cache.Status != model.CacheEnabled {
		e.Cache.Status = model.CacheDisabled
	}
	if e.Parallelism.Requested < 1 {
		e.Parallelism.Requested = 1
	}
	if e.Parallelism.Effective < 1 {
		e.Parallelism.Effective = 1
	}
	if strings.TrimSpace(e.Parallelism.Note) == "" {
		e.Parallelism.Note = fmt.Sprintf("Initial checks run up to %d at a time.", e.Parallelism.Effective)
	}
	backed := []string{}
	seen := map[string]bool{}
	for _, recorded := range r.Evidence {
		id := recorded.ID
		status, ok := l.verified[id]
		if !ok || !negativeStatuses[status] || seen[id] {
			continue
		}
		item, ok := l.item(id)
		if !ok {
			continue
		}
		for _, checkID := range []string{item.BaseCheckID, item.CheckID, item.RepeatCheckID} {
			if c, ok := l.check(checkID); ok && c.Replayed() {
				backed = append(backed, id)
				seen[id] = true
				break
			}
		}
	}
	sort.Strings(backed)
	e.ReplayBacked = backed
}

// cacheNote returns the note rendered under a stored or replayed check in
// "## Automated Checks", or "" (F7a). It returns plain, unescaped text:
// renderMarkdown indents it and passes it through inline().
func cacheNote(c model.Check) string {
	if c.Cache == nil {
		return ""
	}
	switch c.Cache.Status {
	case model.CacheHit:
		note := fmt.Sprintf("Replayed from the execution cache, not executed in this run: the recorded result of check %s of run %s (recorded %s, %d ms; %d agreeing live runs).",
			c.Cache.RecordedCheck, c.Cache.RecordedRun, c.Cache.RecordedAt.UTC().Format(time.RFC3339), c.Cache.RecordedDurationMS, c.Cache.LiveRuns)
		if c.Status != "PASS" {
			// Every conclusion needs a baseline PASS, and nothing runs a
			// replayed non-PASS baseline again.
			note += " A replayed baseline that did not pass supports no conclusion, and the baseline was not run again: a live run might now pass and decide the experiment."
		}
		return note
	case model.CacheStored:
		return fmt.Sprintf("Executed live in this run and recorded in the execution cache (%d agreeing live runs of this key).", c.Cache.LiveRuns)
	}
	return ""
}

// writeExecution renders the execution summary at the end of "## Automated
// Checks", including the replay-backed evidence IDs when there are any (F7a).
// Every string goes through inline().
func writeExecution(b *bytes.Buffer, r *model.Report) {
	e := r.Execution
	if e == nil {
		return
	}
	b.WriteByte('\n')
	c := e.Cache
	if c.Status == model.CacheEnabled {
		image := "not pinned"
		if c.ImageID != "" {
			image = c.ImageID
		}
		fmt.Fprintf(b, "Execution cache: enabled for baseline-side runs only (image %s). %d replayed, %d recorded, %d misses, %d uncacheable, %d rejected, %d write failures, %d evicted, %d contradicted.\n\n",
			inline(image), c.Hits, c.Stored, c.Misses, c.Uncacheable, c.Rejected, c.WriteFailures, c.Evicted, c.Contradicted)
	} else {
		reason := c.Reason
		if strings.TrimSpace(reason) == "" {
			reason = "no reason was recorded"
		}
		if counted(c) {
			// The store disabled itself during the run, after it had served or
			// recorded results: the counters of the whole run still show.
			fmt.Fprintf(b, "Execution cache: disabled (%s). In this run: %d replayed (not executed in this run), %d recorded, %d misses, %d uncacheable, %d rejected, %d write failures, %d evicted, %d contradicted.\n\n",
				inline(reason), c.Hits, c.Stored, c.Misses, c.Uncacheable, c.Rejected, c.WriteFailures, c.Evicted, c.Contradicted)
		} else {
			fmt.Fprintf(b, "Execution cache: disabled (%s).\n\n", inline(reason))
		}
	}
	if c.Status == model.CacheEnabled || counted(c) {
		line(b, inline(c.Note)+"\n")
	}
	fmt.Fprintf(b, "Initial checks: up to %d at a time (requested %d). %s\n\n", e.Parallelism.Effective, e.Parallelism.Requested, inline(e.Parallelism.Note))
	fmt.Fprintf(b, "Sandbox runtime charged: %d of %d ms (reviewer reserve %d ms).", e.Budget.SpentMS, e.Budget.MaxRuntimeMS, e.Budget.ReviewerReserveMS)
	if e.Budget.DeadlineReached {
		b.WriteString(" The overall --deadline was reached.")
	}
	b.WriteString("\n")
	if len(e.ReplayBacked) > 0 {
		fmt.Fprintf(b, "\nNegative conclusions resting on a replayed baseline (recorded by two agreeing live runs; not executed in this run): %s.\n", inline(strings.Join(e.ReplayBacked, ", ")))
	}
}

// counted reports whether the cache served, recorded or judged anything in
// this run, whatever its final status: a store that disabled itself during
// the run keeps the counts of what it did before.
func counted(c model.ExecutionCache) bool {
	return c.Hits+c.Stored+c.Misses+c.Uncacheable+c.Rejected+c.WriteFailures+c.Evicted+c.Contradicted > 0
}
