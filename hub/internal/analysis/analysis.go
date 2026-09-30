// Package analysis fetches a pushed commit and runs the Probe CLI on it.
//
// The hub never becomes a second implementation of the review: it prepares a
// disposable checkout, executes the trusted binary, and stores the confidence
// report the CLI produced. Lint and read-only AI review execute no repository
// code; full review runs the configured checks in the CLI's own
// Docker sandbox and therefore requires a deliberately mounted Docker socket.
package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/hub/internal/accounts"
	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/events"
	"github.com/gvinsot/Probe/hub/internal/forge"
	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// reportPath is where the CLI writes the machine-readable report.
const reportPath = ".probe/confidence-report.json"

// maxReportBytes bounds the report read back from the sandbox output.
const maxReportBytes = 64 << 20

// maxOutputBytes bounds the CLI output kept for diagnostics.
const maxOutputBytes = 16 << 10

// Triggers recorded on a run.
const (
	TriggerPush   = "push"
	TriggerManual = "manual"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Job is one analysis request.
type Job struct {
	UserKey string
	RepoKey string
	Commit  string
	Before  string
	Ref     string
	Message string
	Author  string
	Trigger string
	Variant string
	Intent  string
	// ready orders the queued event before workers can publish running/done.
	ready    chan struct{}
	queuedAt time.Time
}

// ErrBusy is returned when the queue is saturated; the caller should retry.
var ErrBusy = errors.New("analysis queue is full")

// ErrQuota is returned when one account already has as many analyses queued
// or running as the deployment allows, so it cannot starve the others.
var ErrQuota = errors.New("too many analyses in progress for this account")

// ErrNotQueued is returned when cancelling an analysis that is not waiting in
// the queue: it already started, finished or was never requested.
var ErrNotQueued = errors.New("this analysis is not queued")

// attempt is the in-memory state of a queued or running job.
type attempt struct {
	job       Job
	started   bool
	cancelled bool
}

// Runner owns the worker pool and the analysis pipeline.
type Runner struct {
	cfg      config.Config
	store    store.Store
	accounts *accounts.Manager
	events   *events.Broker
	log      *slog.Logger
	queue    chan Job
	mu       sync.Mutex
	active   map[string]*attempt // job key -> attempt queued or running
	perUser  map[string]int
	activity map[activityKey]Activity
}

// New builds a runner. Start must be called to process jobs.
func New(cfg config.Config, s store.Store, a *accounts.Manager, b *events.Broker, log *slog.Logger) *Runner {
	return &Runner{
		cfg: cfg, store: s, accounts: a, events: b, log: log,
		queue:    make(chan Job, cfg.QueueSize),
		active:   map[string]*attempt{},
		perUser:  map[string]int{},
		activity: map[activityKey]Activity{},
	}
}

// Start launches the workers and returns immediately.
func (r *Runner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				r.mu.Lock()
				r.pruneActivity(now)
				r.mu.Unlock()
			}
		}
	}()
	for i := 0; i < r.cfg.Workers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-r.queue:
					if job.ready != nil {
						<-job.ready
					}
					if !r.begin(job) {
						continue // cancelled while queued; already released
					}
					r.process(ctx, job)
					r.release(job)
				}
			}
		}()
	}
}

func jobKey(j Job) string {
	if j.Variant == "" {
		j.Variant = "normal"
	}
	return j.UserKey + "/" + j.RepoKey + "/" + j.Commit + "/" + j.Variant
}

// Enqueue schedules an analysis, ignoring a commit already queued or running.
// It returns ErrQuota when the account is at its quota and ErrBusy when the
// shared queue is full.
func (r *Runner) Enqueue(j Job) error {
	_, err := r.Submit(j)
	return err
}

// Submit is Enqueue that also returns the enqueue time of the attempt that
// will analyze the job: the new one, or the one already queued or running for
// the same commit and variant. A caller matches it against the run events and
// the activity list to tell that attempt apart from earlier ones.
func (r *Runner) Submit(j Job) (time.Time, error) {
	if j.Variant == "" {
		j.Variant = "normal"
	}
	if j.Variant != "normal" && j.Variant != "plan" {
		return time.Time{}, fmt.Errorf("invalid analysis variant")
	}
	if j.Variant == "plan" && (strings.TrimSpace(j.Intent) == "" || len(j.Intent) > 64<<10) {
		return time.Time{}, fmt.Errorf("plan intent is required (at most 64 KiB)")
	}
	if !commitPattern.MatchString(j.Commit) {
		return time.Time{}, fmt.Errorf("invalid commit %q", j.Commit)
	}
	r.mu.Lock()
	if current, busy := r.active[jobKey(j)]; busy {
		r.mu.Unlock()
		return current.job.queuedAt, nil
	}
	if r.cfg.UserQuota > 0 && r.perUser[j.UserKey] >= r.cfg.UserQuota {
		r.mu.Unlock()
		return time.Time{}, ErrQuota
	}
	j.ready = make(chan struct{})
	j.queuedAt = time.Now().UTC()
	r.active[jobKey(j)] = &attempt{job: j}
	r.perUser[j.UserKey]++
	r.mu.Unlock()
	select {
	case r.queue <- j:
		r.markQueued(j)
		close(j.ready)
		return j.queuedAt, nil
	default:
		r.release(j)
		return time.Time{}, ErrBusy
	}
}

func (r *Runner) release(j Job) {
	r.mu.Lock()
	r.releaseLocked(j)
	r.mu.Unlock()
}

// releaseLocked frees the slot of this exact attempt. A newer attempt of the
// same commit, submitted after a cancellation, keeps its own slot.
func (r *Runner) releaseLocked(j Job) {
	current, ok := r.active[jobKey(j)]
	if !ok || !current.job.queuedAt.Equal(j.queuedAt) {
		return
	}
	delete(r.active, jobKey(j))
	if r.perUser[j.UserKey]--; r.perUser[j.UserKey] <= 0 {
		delete(r.perUser, j.UserKey)
	}
}

// begin marks a dequeued job as started. It returns false for a job that was
// cancelled while it waited in the queue.
func (r *Runner) begin(j Job) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.active[jobKey(j)]
	if !ok || !current.job.queuedAt.Equal(j.queuedAt) || current.cancelled {
		return false
	}
	current.started = true
	return true
}

// Cancel withdraws an analysis still waiting in the queue. A running analysis
// cannot be cancelled: the CLI would leave no report to explain the stop.
// The withdrawn job stays in the channel and is skipped when dequeued, but its
// quota slot is freed at once so the commit can be requested again.
func (r *Runner) Cancel(userKey, repoKey, commit, variant string) error {
	j := Job{UserKey: userKey, RepoKey: repoKey, Commit: commit, Variant: variantOr(variant)}
	r.mu.Lock()
	current, ok := r.active[jobKey(j)]
	if !ok || current.started || current.cancelled {
		r.mu.Unlock()
		return ErrNotQueued
	}
	current.cancelled = true
	j = current.job
	r.releaseLocked(j)
	r.mu.Unlock()

	now := time.Now().UTC()
	run := store.Run{
		Commit: j.Commit, BaseCommit: j.Before, Ref: j.Ref, Message: firstLine(j.Message),
		Variant: j.Variant, Author: j.Author, Status: store.StatusCancelled, Trigger: j.Trigger,
		QueuedAt: j.queuedAt, FinishedAt: now,
	}
	r.rememberActivity(j, run)
	r.events.Publish(j.UserKey, map[string]any{"type": "run", "repo_key": j.RepoKey, "run": run})
	if j.Variant == "plan" {
		return nil
	}
	// The queued attempt had become the repository's latest run: fall back to
	// the latest stored result so the badge does not stay "queued".
	repo, err := r.store.UpdateRepo(j.UserKey, j.RepoKey, func(repo *store.Repo) error {
		if repo.Latest == nil || repo.Latest.Commit != j.Commit || !repo.Latest.QueuedAt.Equal(j.queuedAt) {
			return nil
		}
		repo.Latest = nil
		history, err := r.store.History(j.UserKey, j.RepoKey, 0)
		if err != nil {
			return err
		}
		for i := range history {
			if variantOr(history[i].Variant) == "normal" {
				repo.Latest = &history[i]
				break
			}
		}
		return nil
	})
	if err != nil {
		r.log.Error("store cancelled run", "repo", j.RepoKey, "error", err)
		return nil
	}
	r.events.Publish(j.UserKey, map[string]any{"type": "repo", "repo": repo.Public()})
	return nil
}

func variantOr(variant string) string {
	if variant == "" {
		return "normal"
	}
	return variant
}

// Pending reports the queue depth, for the health endpoint.
func (r *Runner) Pending() int { return len(r.queue) }

func (r *Runner) markQueued(j Job) {
	run := store.Run{
		Commit: j.Commit, BaseCommit: j.Before, Ref: j.Ref, Message: firstLine(j.Message),
		Variant: j.Variant, Intent: j.Intent, Author: j.Author, Status: store.StatusQueued, Trigger: j.Trigger, QueuedAt: j.queuedAt,
	}
	r.publishRun(j, run)
}

// publishRun stores the run as the repository's latest state and streams it.
func (r *Runner) publishRun(j Job, run store.Run) {
	run.Error = store.SafeError(run.Error)
	r.rememberActivity(j, run)
	r.events.Publish(j.UserKey, map[string]any{"type": "run", "repo_key": j.RepoKey, "run": run})
	// A proposal must never become the repository badge or commit status.
	if j.Variant == "plan" {
		return
	}
	repo, err := r.store.UpdateRepo(j.UserKey, j.RepoKey, func(repo *store.Repo) error {
		// A late finish of an older commit must not overwrite a newer run.
		if repo.Latest != nil && repo.Latest.Commit != run.Commit && repo.Latest.QueuedAt.After(run.QueuedAt) {
			return nil
		}
		repo.Latest = &run
		return nil
	})
	if err != nil {
		r.log.Error("store run", "repo", j.RepoKey, "error", err)
		return
	}
	r.events.Publish(j.UserKey, map[string]any{"type": "repo", "repo": repo.Public()})
}

func (r *Runner) process(ctx context.Context, j Job) {
	started := time.Now().UTC()
	run := store.Run{
		Commit: j.Commit, BaseCommit: j.Before, Ref: j.Ref, Message: firstLine(j.Message),
		Variant: j.Variant, Intent: j.Intent, Author: j.Author, Status: store.StatusRunning, Trigger: j.Trigger,
		QueuedAt: j.queuedAt, StartedAt: started,
	}
	if run.QueuedAt.IsZero() {
		run.QueuedAt = started
	}
	r.publishRun(j, run)

	ctx, cancel := context.WithTimeout(ctx, r.cfg.AnalysisTimeout)
	defer cancel()

	rec, err := r.analyze(ctx, j, &run)
	run.FinishedAt = time.Now().UTC()
	run.DurationMS = run.FinishedAt.Sub(started).Milliseconds()
	if err != nil {
		run.Status = store.StatusFailed
		run.Error = store.SafeError(err.Error())
		r.log.Error("analysis failed", "repo", j.RepoKey, "commit", short(j.Commit), "error", run.Error)
	} else {
		run.Status = store.StatusDone
	}
	if rec == nil {
		rec = &store.Record{UserKey: j.UserKey, RepoKey: j.RepoKey}
	}
	rec.Run = run
	if err := r.store.PutRecord(rec); err != nil {
		r.log.Error("store report", "repo", j.RepoKey, "error", err)
	}
	r.publishRun(j, run)
	r.events.Publish(j.UserKey, map[string]any{
		"type": "report", "repo_key": j.RepoKey, "commit": j.Commit, "run": run,
	})
	r.publishStatus(ctx, j, run)
}

// analyze does the work and returns the stored record when a report exists.
func (r *Runner) analyze(ctx context.Context, j Job, run *store.Run) (record *store.Record, resultErr error) {
	var secrets []string
	defer func() {
		if resultErr != nil {
			resultErr = errors.New(store.SafeError(resultErr.Error(), secrets...))
		}
	}()
	repo, err := r.store.Repo(j.UserKey, j.RepoKey)
	if err != nil {
		return nil, fmt.Errorf("repository: %w", err)
	}
	user, err := r.store.User(j.UserKey)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	provider, err := r.accounts.Provider(repo.Provider)
	if err != nil {
		return nil, err
	}
	token, err := r.accounts.Token(ctx, user)
	secrets = []string{provider.GitAuthHeader(token), token.AccessToken, token.RefreshToken}
	if err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp("", "probe-hub-")
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	defer os.RemoveAll(work)

	g := &gitRunner{dir: work, env: gitEnv(work, repo.CloneURL, provider.GitAuthHeader(token))}
	if err := g.prepare(ctx, repo.CloneURL); err != nil {
		return nil, err
	}
	branch := strings.TrimPrefix(j.Ref, "refs/heads/")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if err := g.fetch(ctx, branch, j.Commit, r.cfg.CloneDepth); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, "checkout", "--detach", j.Commit); err != nil {
		return nil, err
	}
	base := g.resolveBase(ctx, j.Before, j.Commit, r.cfg.CloneDepth)
	run.BaseCommit = base
	// Only artifacts freshly written by the trusted CLI may become results.
	// Remove a tracked output directory or symlink before either variant runs.
	if err := os.RemoveAll(filepath.Join(work, ".probe")); err != nil {
		return nil, fmt.Errorf("prepare analysis output: %w", err)
	}
	if j.Variant == "plan" {
		return r.analyzePlan(ctx, work, base, j, run, repo.FullName)
	}
	mode := r.modeFor(ctx, g, repo, base)
	run.Mode = mode

	output, exitCode, runErr := r.runCLI(ctx, work, mode, base, j.Commit)
	data, readErr := readBounded(filepath.Join(work, reportPath), maxReportBytes)
	if readErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("probe %s exited %d: %s", mode, exitCode, tail(output))
		}
		return nil, fmt.Errorf("no confidence report was produced: %w", readErr)
	}
	parsed, err := report.Decode(data)
	if err != nil {
		return nil, err
	}
	summary := parsed.Summarize()
	run.Summary = summary
	run.ToolVersion = parsed.ToolVersion
	// Exit codes 0, 1 and 2 are review outcomes, not failures; 3 and 4 mean
	// the run itself could not be completed and the report is incomplete.
	if exitCode < 0 || exitCode >= 3 {
		return &store.Record{
			UserKey: j.UserKey, RepoKey: j.RepoKey, RepoName: repo.FullName, Raw: json.RawMessage(data),
		}, fmt.Errorf("probe %s could not complete (exit %d): %s", mode, exitCode, tail(output))
	}
	return &store.Record{
		UserKey: j.UserKey, RepoKey: j.RepoKey, RepoName: repo.FullName, Raw: json.RawMessage(data),
	}, nil
}

// modeFor picks the mode of one analysis. Review executes the checks of the
// policy committed on the base commit, which is exactly what the CLI reads, so
// it is only used when the operator validated that repository with that
// policy digest. Everything else is linted, which never runs repository code.
func (r *Runner) modeFor(ctx context.Context, g *gitRunner, repo *store.Repo, base string) string {
	if r.cfg.Mode == config.ModeReadOnly {
		return config.ModeReadOnly
	}
	if r.cfg.Mode != config.ModeReview {
		return config.ModeLint
	}
	policy, err := g.blob(ctx, base, forge.PolicyPath)
	if err != nil {
		r.log.Info("no base policy, analyzing in lint mode", "repo", repo.FullName)
		return config.ModeLint
	}
	sum := sha256.Sum256(policy)
	digest := hex.EncodeToString(sum[:])
	if !r.cfg.ReviewAllowed(repo.Provider, repo.FullName, digest) {
		r.log.Info("policy not validated for review, analyzing in lint mode", "repo", repo.FullName, "policy_sha256", digest)
		return config.ModeLint
	}
	return config.ModeReview
}

// runCLI executes the trusted binary on the prepared checkout.
func (r *Runner) runCLI(ctx context.Context, work, mode, base, head string) (string, int, error) {
	readOnly := mode == config.ModeReadOnly
	if readOnly {
		if strings.TrimSpace(os.Getenv(config.EndpointEnvName)) == "" || strings.TrimSpace(os.Getenv(config.ModelEnvName)) == "" {
			return "", 3, fmt.Errorf("read-only AI review requires a deployment-configured endpoint and model")
		}
		mode = config.ModeReview
	} else if mode != config.ModeReview {
		mode = config.ModeLint
	}
	args := []string{
		mode,
		"--repo", work,
		"--base", base,
		"--head", head,
		"--exact",
		"--format", "json,markdown",
		"--out", ".probe",
		"--ci",
	}
	if readOnly {
		args = append(args, "--read-only")
	}
	if mode == config.ModeReview {
		// The hub holds no provider credential of its own: LLM investigation
		// stays a deployment decision made through the CLI's own environment.
		if os.Getenv(config.EndpointEnvName) == "" {
			args = append(args, "--reviewer=false")
		}
	}
	return r.executeCLI(ctx, work, args)
}

// publishStatus reports the outcome back onto the commit, when enabled.
func (r *Runner) publishStatus(ctx context.Context, j Job, run store.Run) {
	if j.Variant == "plan" || !r.cfg.CommitStatus {
		return
	}
	repo, err := r.store.Repo(j.UserKey, j.RepoKey)
	if err != nil || !repo.Monitored {
		return
	}
	user, err := r.store.User(j.UserKey)
	if err != nil {
		return
	}
	provider, err := r.accounts.Provider(repo.Provider)
	if err != nil {
		return
	}
	token, err := r.accounts.Token(ctx, user)
	if err != nil {
		return
	}
	state, description := forge.StateSuccess, statusDescription(run)
	switch {
	case run.Status == store.StatusFailed:
		state = forge.StateError
	case run.Summary.ExitCode == 1:
		state = forge.StateFailure
	case run.Summary.ExitCode == 2:
		// A human decision is required, which is exactly what a failed status
		// asks for; the hub never auto-approves.
		state = forge.StateFailure
	}
	target := r.cfg.BaseURL + "/app.html#/repo/" + repo.Key + "/commit/" + run.Commit
	// The status is best effort: a missing scope must not fail the analysis.
	if err := provider.SetStatus(ctx, token, forge.Repo{ID: repo.ID, FullName: repo.FullName}, run.Commit, state, description, target); err != nil {
		r.log.Warn("commit status", "repo", repo.FullName, "error", err)
	}
}

func statusDescription(run store.Run) string {
	if run.Status == store.StatusFailed {
		return "Probe could not complete this analysis"
	}
	c := run.Summary.Counts
	switch run.Summary.Verdict {
	case report.VerdictBlocked:
		return fmt.Sprintf("%d reproduced issue(s); %d high or critical alert(s)", run.Summary.Reproduced, c.AtLeast(report.SeverityHigh))
	case report.VerdictReview:
		return fmt.Sprintf("Human review required: %d alert(s), %d focused of %d changed lines", c.Total, run.Summary.FocusedLines, run.Summary.ChangedLines)
	default:
		return fmt.Sprintf("No reproduced blocker; %d alert(s) recorded", c.Total)
	}
}

// cliEnv gives the CLI a minimal environment. Docker and provider settings are
// forwarded so an operator can enable review mode without patching the image.
func cliEnv(work string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + work,
		"TMPDIR=" + filepath.Join(work, "tmp"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", config.EndpointEnvName, config.ModelEnvName, config.AllowInsecureHTTPEnvName, "PROBE_API_KEY", "PROBE_API_KEY_FILE"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	_ = os.MkdirAll(filepath.Join(work, "tmp"), 0o700)
	return env
}

func readBounded(path string, max int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("confidence report exceeds %d bytes", max)
	}
	return os.ReadFile(path)
}

func tail(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > maxOutputBytes {
		output = "…" + output[len(output)-maxOutputBytes:]
	}
	return output
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:199] + "…"
	}
	return s
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
