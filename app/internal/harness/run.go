package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// runOptions tunes one sandbox run. The zero value is the v0.2 behavior: no
// payload channel, the policy timeout, the whole budget, the main ledger, and
// the execution cache consulted when the run is eligible.
type runOptions struct {
	capture     string        // fixed in-container payload path; "" = none
	timeout     time.Duration // > 0 tightens the per-run timeout, never loosens it; part of the cache key
	remaining   time.Duration // > 0 further bounds the launch timeout and the served duration, never the cache key; see below
	ceiling     time.Duration // > 0: budget limit for this run instead of MaxRuntime (§1.7.1)
	tee         io.Writer     // optional log copy, capped at teeLimit; see below
	teeOverflow *bool         // set to true when the tee cap was hit
	ledger      string        // "" -> h.checks, "check-N"; ledgerMutation -> h.mutationChecks, "mutation-check-N", artifact kind mutation_check_output
	live        bool          // never served from the execution cache; write-through still allowed (§1.11)
}

// runOptions.remaining is what remains of a stage's sub-cap (§1.7.1 rule 3).
// It varies with the time the stage's earlier runs took, so, like budget
// clamping, it stays out of the cache key: a stage passes its fixed sub-cap
// as timeout and the remainder as remaining, and the key of a run then
// repeats across reviews (§1.11 timeout_ms, Appendix C F2).

// ledgerMutation names the separate mutation check ledger.
const ledgerMutation = "mutation"

// teeLimit caps what one run forwards to runOptions.tee. The tee receives the
// raw, unredacted log stream, including what the recorded log drops past
// max_output_bytes. It exists only for progress and early abort: no verdict may
// depend on it, and consumers must parse it in constant memory with a 1 MiB
// line limit and must never display it unredacted.
const teeLimit = 64 << 20

const (
	checkPrefix         = "check-"
	mutationCheckPrefix = "mutation-check-"
)

// Fixed texts of runs that were recorded without being executed.
const (
	budgetExhaustedText = "Sandbox runtime budget exhausted."
	budgetReservedText  = "Sandbox runtime reserved for reviewer experiments."
	// deadlineText is recorded when the overall --deadline (context cause
	// ErrOverallDeadline) expired before the run started.
	deadlineText = "Overall deadline reached; the run was not started."
	// callerDeadlineText is recorded when another time limit of the calling
	// stage expired first, for example the reviewer time limit around a
	// reviewer-requested experiment; it does not blame --deadline.
	callerDeadlineText = "The time limit of the requesting stage (for example reviewer.timeout_seconds) expired before the run started; the run was not started."
)

// expiredText is the SKIPPED text of a run whose context deadline passed
// before it started.
func expiredText(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), ErrOverallDeadline) {
		return deadlineText
	}
	return callerDeadlineText
}

// auditExecutionCache is the audit tool name of a replayed baseline run.
const auditExecutionCache = model.AuditStagePrefix + "execution_cache"

func (h *Harness) run(ctx context.Context, kind, dir string, command []string) model.Check {
	c, _, _ := h.runWithOptions(ctx, kind, dir, command, runOptions{})
	return c
}

// runWith executes one command. When capture names an in-container file the
// container returns that file on its own bounded payload channel; the returned
// bytes are raw and unredacted so the payload can be parsed and hashed before
// any display transformation touches it.
func (h *Harness) runWith(ctx context.Context, kind, dir string, command []string, capture string) (model.Check, []byte, bool) {
	return h.runWithOptions(ctx, kind, dir, command, runOptions{capture: capture})
}

// runWithOptions records exactly one check in the ledger o.ledger names and
// returns it with the raw payload (nil without o.capture) and whether the
// payload was cut short. Caller holds h.mu.
//
// It is plan, executePlan and record in sequence; RunChecks (batch.go) is the
// only caller that runs several plans' executions at the same time.
//
// Budget (§1.7.1): the run reserves min(policy timeout, o.timeout,
// o.remaining, what the limit leaves after spent and reserved time) before
// launch, and releases the reservation and charges the elapsed time
// afterwards. The limit is MaxRuntime, or o.ceiling when that is lower. A
// replay reserves and charges nothing.
//
// Cache (§1.11): a baseline-side kind run on h.base without network, with a
// cache configured, is keyed by h.exec with min(policy timeout, o.timeout).
// Unless o.live, a servable entry is replayed instead of executed; its
// recorded duration must be below the launch timeout, which also honors
// o.remaining. A live result that may be stored is written
// through, which updates the entry's agreement count or evicts a contradicted
// entry.
func (h *Harness) runWithOptions(ctx context.Context, kind, dir string, command []string, o runOptions) (model.Check, []byte, bool) {
	p := h.plan(ctx, kind, dir, command, o, 0)
	executePlan(ctx, p)
	return h.record(p)
}

// runPlan is one run between plan and record. plan fills everything up to the
// launch while holding h.mu; executePlan touches only the launch fields and
// the result, never the harness; record completes the check under h.mu.
type runPlan struct {
	kind         string
	dir          string
	command      []string
	o            runOptions
	ledger       *[]model.Check
	artifactKind string
	started      time.Time
	check        model.Check // ID, kind, redacted command; Status and Output when decided without a launch

	// replay is the servable cache entry that record replays instead of an
	// execution; key is the run's cache key ("" when not eligible or skipped).
	replay *CacheEntry
	key    string

	// Launch inputs, fixed by plan when it reserved the budget for a
	// container: launched is then true and timeout is the reserved timeout.
	launched       bool
	timeout        time.Duration
	name           string
	args           []string
	out            *boundedWriter
	log            io.Writer
	payload        *boundedWriter
	execute        executor
	executeCapture captureExecutor

	// Set by executePlan; plan sets finished for a run it decided or
	// replays without a launch.
	result   execution
	finished time.Time
}

// launches reports whether executePlan starts a container for p.
func (p *runPlan) launches() bool { return p.launched }

// plan decides everything about one run up to its launch: the check ID (the
// ledger's next ID plus pending, the number of earlier plans of the same
// ledger that are not recorded yet), the gates that record a run without
// executing it, the cache consultation (a servable entry is replayed by
// record), and the budget reservation, in the order runWithOptions has always
// applied them. Caller holds h.mu.
func (h *Harness) plan(ctx context.Context, kind, dir string, command []string, o runOptions, pending int) *runPlan {
	p := &runPlan{kind: kind, dir: dir, command: command, o: o, started: time.Now()}
	ledger, prefix, artifactKind, ledgerOK := h.ledgerFor(o.ledger)
	p.ledger, p.artifactKind = ledger, artifactKind
	c := model.Check{ID: fmt.Sprintf("%s%d", prefix, len(*ledger)+1+pending), Kind: kind, Command: append([]string(nil), command...), ExitCode: -1}
	for i, arg := range c.Command {
		c.Command[i] = Redact(arg)
	}
	effective := h.opts.Timeout
	if o.timeout > 0 && o.timeout < effective {
		effective = o.timeout
	}
	launch := effective // effective is keyed; launch also honors o.remaining
	if o.remaining > 0 && o.remaining < launch {
		launch = o.remaining
	}
	script := wrapperScript
	switch {
	case o.capture != "" && !logDecidesError(kind):
		// A candidate-side command may pass the tested code's exit code
		// through; the script keeps 125 and above for sandbox failures.
		script = candidateCaptureScript(o.capture)
	case o.capture != "":
		script = captureScript(o.capture)
	}
	switch {
	case !ledgerOK:
		c.Status, c.Output = "ERROR", "unknown check ledger"
	case h.closed:
		c.Status, c.Output = "ERROR", "harness is closed"
	case len(command) == 0:
		c.Status, c.Output = "SKIPPED", "No command configured."
	case h.opts.Image == "":
		c.Status, c.Output = "SKIPPED", "No Docker image configured; repository code was not executed."
	case dir == "":
		c.Status, c.Output = "SKIPPED", "No baseline snapshot available."
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Status, c.Output = "SKIPPED", expiredText(ctx)
	default:
		key := h.cacheKey(kind, dir, command, script, effective)
		if key != "" && !o.live {
			if e, found := h.exec.cache.Get(key); found && e.Key == key && servable(e, launch) {
				p.replay, p.key, p.finished = &e, key, time.Now()
				return p
			}
			h.cacheCounts.Misses++
		}
		timeout, skipped := h.reserveRun(launch, o.ceiling)
		if skipped != "" {
			c.Status, c.Output = "SKIPPED", skipped
			break
		}
		p.launched, p.key, p.timeout = true, key, timeout
		p.name = "swiftproof-" + randomID()
		p.out = &boundedWriter{limit: h.opts.MaxOutputBytes}
		p.log = p.out
		if o.tee != nil {
			p.log = &teeLog{log: p.out, tee: &cappedWriter{w: o.tee, remaining: teeLimit, overflow: o.teeOverflow}}
		}
		p.args = h.dockerArgsScript(p.name, dir, script, command)
		if o.capture != "" {
			p.payload = &boundedWriter{limit: PayloadLimit(h.opts.MaxOutputBytes)}
		}
		p.execute, p.executeCapture = h.execute, h.executeCapture
	}
	p.check = c
	if !p.launches() {
		p.finished = time.Now()
	}
	return p
}

// executePlan runs p's container, if plan launched one, under its reserved
// timeout and ctx. It is not a Harness method: it reads and writes only p (the
// executor was copied into it by plan), so RunChecks may run it for several
// plans at the same time while it holds h.mu.
func executePlan(ctx context.Context, p *runPlan) {
	if !p.launches() {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	if p.payload != nil {
		p.result = p.executeCapture(runCtx, p.name, p.args, p.log, p.payload)
	} else {
		p.result = p.execute(runCtx, p.name, p.args, p.log)
	}
	cancel()
	p.finished = time.Now()
}

// record completes p's check: a replay, or the classification of the
// execution, the budget release and charge, the duration, the log artifact,
// the cache write-through, and the append to the ledger. It returns the check
// with the raw payload and whether the payload was cut short, as
// runWithOptions does. Caller holds h.mu.
func (h *Harness) record(p *runPlan) (model.Check, []byte, bool) {
	if p.replay != nil {
		return h.replay(p.kind, p.command, p.key, *p.replay, p.o)
	}
	c := p.check
	if p.launches() {
		result := p.result
		c.ExitCode, c.Truncated, c.Output = result.ExitCode, p.out.truncated, Redact(string(p.out.data))
		switch {
		case result.TimedOut:
			c.Status = "TIMEOUT"
		case result.Err != nil:
			c.Status = "ERROR"
			// Redacted as a whole: two redacted parts can join into a new match.
			c.Output = Redact(c.Output + "\n" + result.Err.Error())
		case result.ExitCode == 0:
			c.Status = "PASS"
		case result.ExitCode >= 125:
			c.Status = "ERROR"
		case logDecidesError(p.kind) && strings.HasPrefix(p.kind, "generated_test_") && generatedSetupFailure(c.Output):
			c.Status = "ERROR"
		case logDecidesError(p.kind) && strings.Contains(c.Output, "fork/exec ") && (strings.Contains(c.Output, "permission denied") || strings.Contains(c.Output, "exec format error") || strings.Contains(c.Output, "no such file or directory")):
			c.Status = "ERROR"
		default:
			c.Status = "FAIL"
		}
		h.releaseRun(p.timeout, p.finished.Sub(p.started))
	}
	c.DurationMS = p.finished.Sub(p.started).Milliseconds()
	c.Output = truncateUTF8(c.Output, h.opts.MaxOutputBytes)
	if c.Output != "" {
		if err := h.saveArtifact(c.ID+".log", p.artifactKind, []byte(c.Output)); err != nil {
			c.Status = "ERROR"
			c.Output = truncateUTF8(Redact("Unable to retain check output: "+err.Error()+"\n"+c.Output), h.opts.MaxOutputBytes)
		}
	}
	var data []byte
	truncated := false
	if p.payload != nil {
		data, truncated = p.payload.data, p.payload.truncated
	}
	if p.key != "" {
		if storable(c, data, truncated) {
			h.recordCacheWrite(p.key, &c, data, truncated)
		} else {
			h.cacheCounts.Uncacheable++
		}
	}
	*p.ledger = append(*p.ledger, c)
	h.resultsBytes += len(c.Results)
	if p.payload == nil {
		return c, nil, false
	}
	return c, data, truncated
}

// logDecidesError reports whether the text of a failed run's log may turn the
// run into ERROR through the inherited v0.2 rules (a setup-failure marker in a
// generated_test_* log, or a fork/exec failure line in any log). It holds for
// the v0.2 kinds, for generated_test_intent (whose setup failures stay ERROR,
// §1.17) and for the baseline-side v0.4 kinds, whose logs candidate code does
// not write. It is false for the candidate-side v0.4 kinds and for any kind
// not listed: candidate code writes their logs, so for them ERROR comes only
// from an infrastructure cause (a Docker or executor error, exit code 125 or
// above, a lost log artifact) and a compile, setup or run failure stays FAIL.
// Exit code 125 or above then means a sandbox failure: a capture run of such a
// kind uses candidateCaptureScript, which reports a command status of 125 or
// above as 124 (a TS/JS runner passes a process.exit code through), and the
// other runs of these kinds are go test, which never passes a test binary's
// exit code through. Candidate content can then never force exit 4 through
// them (§1.17).
func logDecidesError(kind string) bool {
	switch kind {
	case model.CheckTest, model.CheckTypecheck, model.CheckBuild, model.CheckCoverage, model.CheckExistingTest,
		model.CheckGeneratedBase, model.CheckGeneratedCandidate, model.CheckGeneratedIntent, model.CheckGeneratedBaseRepeat,
		model.CheckFuzzBase, model.CheckFuzzBaseConfirm, model.CheckBaseTestBase, model.CheckImpactedTestBase:
		return true
	}
	return false
}

// ledgerFor resolves runOptions.ledger. An unknown name is recorded as an
// ERROR check in the main ledger rather than silently dropped.
func (h *Harness) ledgerFor(name string) (ledger *[]model.Check, prefix, artifactKind string, ok bool) {
	switch name {
	case "":
		return &h.checks, checkPrefix, "check_output", true
	case ledgerMutation:
		return &h.mutationChecks, mutationCheckPrefix, model.ArtifactMutationCheckOutput, true
	}
	return &h.checks, checkPrefix, "check_output", false
}

// reserveRun claims the timeout of one run from the remaining budget (§1.7.1
// rule 2). It returns the reserved timeout, or the fixed SKIPPED text when
// nothing remains under the applicable limit. Caller holds h.mu.
func (h *Harness) reserveRun(timeout, ceiling time.Duration) (time.Duration, string) {
	limit, ceilingLimited := h.opts.MaxRuntime, false
	if ceiling > 0 && ceiling < limit {
		limit, ceilingLimited = ceiling, true
	}
	avail := limit - h.spent - h.reserved
	if avail <= 0 {
		if ceilingLimited && h.opts.MaxRuntime-h.spent-h.reserved > 0 {
			return 0, budgetReservedText
		}
		return 0, budgetExhaustedText
	}
	if avail < timeout {
		timeout = avail
	}
	h.reserved += timeout
	return timeout, ""
}

// releaseRun returns a reservation and charges the time the run actually
// took. Caller holds h.mu.
func (h *Harness) releaseRun(reserved, elapsed time.Duration) {
	h.reserved -= reserved
	h.spent += elapsed
}

// MutationChecks returns a deep copy of the mutation ledger, like Checks().
func (h *Harness) MutationChecks() []model.Check {
	h.mu.Lock()
	defer h.mu.Unlock()
	return copyChecks(h.mutationChecks)
}

// Budget reports the sandbox runtime budget: the configured maximum, the time
// charged so far and the reviewer reserve. DeadlineReached is left to the
// caller, which owns the overall deadline context.
func (h *Harness) Budget() model.ExecutionBudget {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.budgetLocked()
}

// budgetLocked is Budget for a caller that holds h.mu.
func (h *Harness) budgetLocked() model.ExecutionBudget {
	return model.ExecutionBudget{MaxRuntimeMS: h.opts.MaxRuntime.Milliseconds(), SpentMS: h.spent.Milliseconds(), ReviewerReserveMS: h.opts.ReviewerReserve.Milliseconds()}
}

// --- execution cache consult/store point (§1.11) -----------------------------

var cacheKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cacheableKind reports whether kind names a baseline-side run of an
// experiment family, the only kinds the execution cache may serve.
func cacheableKind(kind string) bool { return strings.HasSuffix(kind, "_base") }

// cacheKey returns the execution-cache key of an eligible run, or "" when the
// run is not eligible. An eligible run that cannot be keyed counts as
// uncacheable. Caller holds h.mu.
func (h *Harness) cacheKey(kind, dir string, command []string, script string, timeout time.Duration) string {
	if h.exec.cache == nil || !cacheableKind(kind) || h.base == "" || dir != h.base || h.opts.Network {
		return ""
	}
	key, why := h.exec.keyFor(h, kind, dir, command, script, timeout)
	if why != "" || !cacheKeyPattern.MatchString(key) {
		h.cacheCounts.Uncacheable++
		return ""
	}
	return key
}

// servable reports whether an entry may be replayed for a run whose effective
// per-run timeout is timeout: two agreeing live runs, never contradicted, a
// baseline PASS, a recorded duration below the timeout, and a payload that is
// a Redact fixed point. A FAIL entry is stored (so that a later disagreeing
// live run contradicts it) but never served: a replayed baseline FAIL supports
// no positive status, so replaying it could only leave undecided an experiment
// that a live baseline would have decided, and nothing would run it again.
func servable(e CacheEntry, timeout time.Duration) bool {
	if e.LiveRuns < 2 || e.Contradicted {
		return false
	}
	if e.Status != "PASS" || e.ExitCode != 0 {
		return false
	}
	if e.DurationMS < 0 || time.Duration(e.DurationMS)*time.Millisecond >= timeout {
		return false
	}
	return len(e.Payload) == 0 || redact.IsFixedPoint(string(e.Payload))
}

// storable reports whether a live result may be written to the cache: a
// completed PASS or FAIL with exit code 0..124 whose log was retained (a
// retention failure is already ERROR), and a payload, if any, that is complete
// and a Redact fixed point.
func storable(c model.Check, payload []byte, truncated bool) bool {
	if c.Status != "PASS" && c.Status != "FAIL" || c.ExitCode < 0 || c.ExitCode > 124 {
		return false
	}
	if truncated {
		return false
	}
	return len(payload) == 0 || redact.IsFixedPoint(string(payload))
}

// replay records a servable entry as the current run's check without
// executing anything. Caller holds h.mu.
func (h *Harness) replay(kind string, command []string, key string, e CacheEntry, o runOptions) (model.Check, []byte, bool) {
	started := time.Now()
	ledger, prefix, artifactKind, _ := h.ledgerFor(o.ledger)
	c := model.Check{ID: fmt.Sprintf("%s%d", prefix, len(*ledger)+1), Kind: kind, Command: append([]string(nil), command...), Status: e.Status, ExitCode: e.ExitCode, Truncated: e.Truncated}
	for i, arg := range c.Command {
		c.Command[i] = Redact(arg)
	}
	output := Redact(e.Output)
	if len(output) > h.opts.MaxOutputBytes {
		c.Truncated = true
	}
	c.Output = truncateUTF8(output, h.opts.MaxOutputBytes)
	c.Cache = &model.CheckCache{Status: model.CacheHit, Key: key, RecordedAt: e.RecordedAt.UTC(), RecordedRun: e.RecordedRun, RecordedCheck: e.RecordedCheck, RecordedDurationMS: e.DurationMS, LiveRuns: e.LiveRuns}
	// The tee receives the recorded (redacted, bounded) log: nothing else of
	// the original execution exists any more.
	if o.tee != nil && c.Output != "" {
		_, _ = (&cappedWriter{w: o.tee, remaining: teeLimit, overflow: o.teeOverflow}).Write([]byte(c.Output))
	}
	if c.Output != "" {
		if err := h.saveArtifact(c.ID+".log", artifactKind, []byte(c.Output)); err != nil {
			c.Status = "ERROR"
			c.Output = truncateUTF8(Redact("Unable to retain check output: "+err.Error()+"\n"+c.Output), h.opts.MaxOutputBytes)
		}
	}
	*ledger = append(*ledger, c)
	h.cacheCounts.Hits++
	arguments, _ := json.Marshal(map[string]string{"check_id": c.ID, "key": key, "recorded_run": Redact(e.RecordedRun), "recorded_check": Redact(e.RecordedCheck)})
	h.audit = append(h.audit, model.AuditEvent{Time: started.UTC(), Tool: auditExecutionCache, Arguments: truncateUTF8(string(arguments), 4096), Status: "HIT", DurationMS: time.Since(started).Milliseconds()})
	if o.capture == "" {
		return c, nil, false
	}
	return c, append([]byte(nil), e.Payload...), false
}

// recordCacheWrite writes a storable live result through to the cache: a new
// entry starts at one live run, an agreeing entry (same status, exit code and
// truncation) gains one and keeps the latest output, and a disagreeing or
// contradicted entry is evicted without replacement. A successful store
// marks c with Cache{stored}. Caller holds h.mu.
func (h *Harness) recordCacheWrite(key string, c *model.Check, payload []byte, truncated bool) {
	if !storable(*c, payload, truncated) {
		return // never persist an incomplete result or a cut or unredactable payload
	}
	prior, found := h.exec.cache.Get(key)
	if found && prior.Key != key {
		found = false
	}
	if found && (prior.Contradicted || prior.Status != c.Status || prior.ExitCode != c.ExitCode || prior.Truncated != c.Truncated) {
		if !prior.Contradicted {
			h.cacheCounts.Contradicted++
		}
		if err := h.exec.cache.Delete(key); err != nil {
			h.cacheCounts.WriteFailures++
		} else {
			h.cacheCounts.Evicted++
		}
		return
	}
	now := time.Now().UTC()
	entry := CacheEntry{Key: key, Preimage: h.exec.preimage(key), Status: c.Status, ExitCode: c.ExitCode, Output: c.Output, Truncated: c.Truncated, DurationMS: c.DurationMS, LiveRuns: 1, RecordedAt: now, RecordedRun: h.runID, RecordedCheck: c.ID}
	if len(payload) > 0 {
		entry.Payload = append([]byte(nil), payload...)
	}
	if found {
		entry.LiveRuns = prior.LiveRuns + 1
		if len(entry.Preimage) == 0 {
			entry.Preimage = prior.Preimage
		}
	}
	if err := h.exec.cache.Put(entry); err != nil {
		h.cacheCounts.WriteFailures++
		return
	}
	h.cacheCounts.Stored++
	c.Cache = &model.CheckCache{Status: model.CacheStored, Key: key, RecordedAt: now, RecordedRun: h.runID, RecordedCheck: c.ID, RecordedDurationMS: c.DurationMS, LiveRuns: entry.LiveRuns}
}

// --- tee ---------------------------------------------------------------------

// teeLog fans the container log out to the recorded, bounded log and to the
// capped tee. It never returns an error, so a tee consumer can never disturb
// the recorded log or the command.
type teeLog struct {
	mu  sync.Mutex
	log io.Writer
	tee io.Writer
}

func (t *teeLog) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.log.Write(p)
	_, _ = t.tee.Write(p)
	return len(p), nil
}

// cappedWriter forwards at most remaining bytes to w, then drops data and sets
// *overflow. It never returns an error, and it stops forwarding after w fails.
type cappedWriter struct {
	mu        sync.Mutex
	w         io.Writer
	remaining int
	overflow  *bool
	failed    bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if len(p) > c.remaining {
		p = p[:c.remaining]
		if c.overflow != nil {
			*c.overflow = true
		}
	}
	c.remaining -= len(p)
	if len(p) > 0 && !c.failed {
		if _, err := c.w.Write(p); err != nil {
			c.failed = true
		}
	}
	return n, nil
}
