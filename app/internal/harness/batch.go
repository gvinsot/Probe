package harness

// Bounded parallel initial checks (F7b; contract §1.7.1 rule 5, §2 F7).
//
// RunChecks is the only place where sandbox runs of a harness overlap. It runs
// the initial checks in groups. Each group is planned in configured order (the
// check IDs, the gates and the budget reservations are exactly those a run one
// at a time would get), its launched containers execute at the same time, and
// once all of them have ended the group is recorded in configured order
// (checks, log artifacts, audit events). A group has more than one member only
// while the remaining budget covers the full per-run timeout of every member,
// so every concurrent run gets the policy timeout (an expiring context, such
// as the overall deadline, still ends it), and the sum of the timeouts in
// flight never exceeds the remaining budget. Every other run of the harness
// stays sequential under h.mu.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Fixed texts of execution.parallelism.note. They avoid apostrophes, which the
// Markdown renderer escapes as entities.
const (
	// parallelOneNote is the note when one initial check at a time was
	// requested. It equals the note newExecState (cache.go) records before
	// RunChecks runs.
	parallelOneNote = "Initial checks run one at a time."
	// parallelSerialNote opens the note when more were requested but the
	// initial checks that started a sandbox did so one at a time; the reasons
	// follow it.
	parallelSerialNote = "Initial checks ran one at a time."
	// parallelNoSandboxNote opens the note when more were requested and no
	// initial check started a sandbox (no image, a closed harness, a context
	// that had expired or was cancelled, an exhausted budget, no command); the
	// recorded checks say why each did not run.
	parallelNoSandboxNote = "No initial check started a sandbox."
	// parallelBudgetNote says that the budget rule made a group smaller.
	parallelBudgetNote = "The remaining sandbox runtime budget did not cover the full per-run timeout of every check of a group, so fewer checks ran at the same time."
	// parallelUnstartedNote says that a group had members recorded without a
	// container (no command, time limit reached, budget exhausted).
	parallelUnstartedNote = "Some checks of a group were recorded without starting a sandbox, so fewer ran at the same time."
	// parallelSemanticsNote closes the note whenever checks ran at the same
	// time. It states the rules, not an outcome: concurrent containers share
	// the Docker host.
	parallelSemanticsNote = "Checks that ran at the same time are recorded in configured order (check IDs, artifacts and audit events). " +
		"Each was started only while the remaining sandbox runtime budget covered the full per-run timeout of every check running with it, " +
		"so each had the per-run timeout of the policy, which the deadline of the review still bounds, and each is charged its own run time, as when checks run one at a time. " +
		"Concurrent sandboxes share the Docker host, so a check can take longer than it would alone and be charged more."
)

// RunChecks runs the initial checks of kinds on the candidate snapshot, in
// the given order, and returns their recorded checks in that order. Each kind
// is recorded as Run records it: one check with its log artifact, then one
// audit event "run_<kind>".
//
// With Options.Parallel above 1, up to that many run at the same time, capped
// by the number of kinds and by the Docker server's capacity for sandboxes of
// the configured CPUs and memory (one `docker info`, bounded by probeTimeout
// and ctx; a failure means one at a time). A group of n starts only while the
// budget left under MaxRuntime covers n full per-run timeouts, and shrinks
// otherwise, down to a single run, which takes what remains as always.
// Parallelism changes no rule: every run gets the gates, the timeout
// reservation, the classification and the budget charge rule of a run one at
// a time, and the audit events of one group share its start time, so a stable
// sort by time keeps the configured order. Outcomes can still differ from a
// run one at a time: concurrent runs share the Docker host, so they can take
// longer, reach their timeout and be charged more; and every member of a group
// passes the deadline gate when the group starts, so when ctx expires during a
// group, its members still running end TIMEOUT, where one at a time the later
// ones would have been SKIPPED as not started.
//
// RunChecks records the requested and effective concurrency, with a note, in
// the execution summary. Effective is the largest number of sandboxes of one
// group that ran at the same time (at least 1); when RunChecks is called more
// than once, the call with the largest value is kept.
func (h *Harness) RunChecks(ctx context.Context, kinds []string) []model.Check {
	h.mu.Lock()
	defer h.mu.Unlock()
	checks := make([]model.Check, 0, len(kinds))
	if len(kinds) == 0 {
		return checks
	}
	requested := h.opts.Parallel
	if requested < 1 {
		requested = 1
	}
	limit, reasons := h.initialCheckLimit(ctx, requested, len(kinds))
	// started counts the containers launched while ctx was live: a run
	// launched on a context that had already ended starts no container.
	widest, largest, started, shrunk := 1, 1, 0, false
	for next := 0; next < len(kinds); {
		allowed := min(limit, len(kinds)-next)
		n := h.groupSize(allowed)
		if n < allowed {
			shrunk = true
		}
		live := ctx.Err() == nil
		group, launched := h.runGroup(ctx, kinds[next:next+n])
		checks = append(checks, group...)
		largest = max(largest, n)
		if live {
			started += launched
			widest = max(widest, launched)
		}
		next += n
	}
	if started > 0 && shrunk {
		reasons = append(reasons, parallelBudgetNote)
	}
	if started > 0 && widest < largest {
		reasons = append(reasons, parallelUnstartedNote)
	}
	summary := model.ExecutionParallelism{Requested: requested, Effective: widest, Note: parallelismNote(requested, widest, started, reasons)}
	if summary.Effective >= h.exec.parallel.Effective {
		h.exec.parallel = summary
	}
	return checks
}

// groupSize returns how many of the next allowed checks start together:
// allowed, reduced while the budget left under MaxRuntime does not cover the
// full per-run timeout of each of them. It is at least 1: a single run
// reserves what remains, or is SKIPPED, exactly as a run one at a time.
// Caller holds h.mu.
func (h *Harness) groupSize(allowed int) int {
	n := max(allowed, 1)
	avail := h.opts.MaxRuntime - h.spent - h.reserved
	for n > 1 && time.Duration(n)*h.opts.Timeout > avail {
		n--
	}
	return n
}

// runGroup plans kinds in order, executes the launched plans at the same
// time, waits for every one of them, and records them in order, each followed
// by its audit event. It returns the checks and how many containers ran at
// the same time. Caller holds h.mu for the whole group: no other run and no
// reader of the ledgers can interleave, and the executions touch only their
// own plans.
func (h *Harness) runGroup(ctx context.Context, kinds []string) ([]model.Check, int) {
	started := time.Now()
	plans := make([]*runPlan, len(kinds))
	for i, kind := range kinds {
		plans[i] = h.plan(ctx, kind, h.candidate, h.opts.Commands[kind], runOptions{}, i)
	}
	var wg sync.WaitGroup
	launched := 0
	for _, p := range plans {
		if !p.launches() {
			continue
		}
		launched++
		wg.Add(1)
		go func(p *runPlan) {
			defer wg.Done()
			executePlan(ctx, p)
		}(p)
	}
	wg.Wait()
	checks := make([]model.Check, len(plans))
	for i, p := range plans {
		recording := time.Now()
		c, _, _ := h.record(p)
		// The duration is the check's own: from the group start to the end of
		// its execution, plus its recording.
		duration := p.finished.Sub(started) + time.Since(recording)
		h.audit = append(h.audit, model.AuditEvent{Time: started.UTC(), Tool: "run_" + kinds[i], Status: c.Status, DurationMS: duration.Milliseconds()})
		checks[i] = c
	}
	return checks, launched
}

// initialCheckLimit returns how many initial checks may run at the same time
// and the reasons it is below requested. It asks the Docker server for its
// capacity only when more than one check could run at the same time and a
// container can start at all. Caller holds h.mu.
func (h *Harness) initialCheckLimit(ctx context.Context, requested, count int) (int, []string) {
	if requested <= 1 {
		return 1, nil
	}
	limit, reasons := requested, []string(nil)
	if count < limit {
		limit = count
		if count == 1 {
			reasons = append(reasons, "Only one initial check was requested.")
		} else {
			reasons = append(reasons, fmt.Sprintf("Only %d initial checks were requested.", count))
		}
	}
	if limit <= 1 {
		return 1, reasons
	}
	switch {
	case h.closed:
		return 1, append(reasons, "The sandbox harness was closed.")
	case h.opts.Image == "":
		return 1, append(reasons, "No sandbox image is configured.")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return 1, append(reasons, "The time limit was reached before the initial checks started.")
	case ctx.Err() != nil:
		return 1, append(reasons, "The review was cancelled before the initial checks started.")
	}
	probe, cancel := context.WithTimeout(ctx, probeTimeout)
	info, err := dockerutil.ServerInfo(probe, dockerRunner)
	cancel()
	if err == nil && (info.NCPU < 1 || info.MemTotal < 1) {
		err = fmt.Errorf("docker info reported %d CPUs and %d bytes of memory", info.NCPU, info.MemTotal)
	}
	if err != nil {
		return 1, append(reasons, "The capacity of the Docker server could not be read: "+truncateUTF8(strings.TrimSpace(Redact(err.Error())), 256))
	}
	if capacity := sandboxCapacity(info, h.opts.CPUs, h.opts.MemoryMB); capacity < limit {
		// The note gives the capacity the probe found. The limit stays at
		// least 1: the checks then run one at a time, as with --parallel 1.
		limit = max(capacity, 1)
		server := fmt.Sprintf("The Docker server reports %s and %d MiB of memory", plural(info.NCPU, "CPU"), info.MemTotal>>20)
		if capacity == 0 {
			reasons = append(reasons, fmt.Sprintf("%s: no room for a single sandbox of %s and %d MiB.", server, plural(h.opts.CPUs, "CPU"), h.opts.MemoryMB))
		} else {
			reasons = append(reasons, fmt.Sprintf("%s: room for %s of %s and %d MiB at a time.", server, plural(capacity, "sandbox"), plural(h.opts.CPUs, "CPU"), h.opts.MemoryMB))
		}
	}
	return limit, reasons
}

// plural returns "1 noun" or "n nouns" (nouns ending in x take "es").
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "x") {
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// sandboxCapacity is how many sandboxes of cpus CPUs and memoryMB MiB the
// Docker server can hold at the same time without oversubscribing its CPUs or
// its memory. It can be 0.
func sandboxCapacity(info dockerutil.Info, cpus, memoryMB int) int {
	if cpus < 1 || memoryMB < 1 {
		return 0
	}
	byMemory := info.MemTotal / (int64(memoryMB) << 20)
	return int(min(int64(info.NCPU/cpus), byMemory))
}

// parallelismNote composes execution.parallelism.note from fixed texts: the
// one-at-a-time note, or what happened (no initial check started a sandbox,
// or they started one at a time) and the reasons the concurrency stayed below
// what was requested, followed by the rules concurrent checks ran under.
// started is the number of containers the initial checks launched.
func parallelismNote(requested, effective, started int, reasons []string) string {
	if requested <= 1 {
		return parallelOneNote
	}
	parts := []string{}
	switch {
	case started == 0:
		parts = append(parts, parallelNoSandboxNote)
	case effective <= 1:
		parts = append(parts, parallelSerialNote)
	}
	parts = append(parts, reasons...)
	if effective > 1 {
		parts = append(parts, parallelSemanticsNote)
	}
	return strings.Join(parts, " ")
}
