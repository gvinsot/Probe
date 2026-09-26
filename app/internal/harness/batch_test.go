package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// batchCommands are the initial checks of the batch tests; the last argument
// names the check, so an executor can tell them apart.
var batchCommands = map[string][]string{
	"test":      {"go", "test", "./...", "test"},
	"typecheck": {"go", "vet", "./...", "typecheck"},
	"build":     {"go", "build", "./...", "build"},
	"lint":      {"go", "vet", "-vettool=x", "lint"},
}

var initialKinds = []string{"test", "typecheck", "build"}

// batchFixture is a harness with the batch commands and the requested
// parallelism; edit may change the options before New.
func batchFixture(t *testing.T, parallel int, edit func(*Options)) *Harness {
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
	opts := Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: "test-image:local", Commands: batchCommands, Parallel: parallel}
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

// checkName returns the name the batch commands end with.
func checkName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

// capacityDocker answers `docker info` with the given capacity.
func capacityDocker(ncpu int, memTotal int64) *fakeDocker {
	return &fakeDocker{info: func() (string, error) {
		return fmt.Sprintf(`{"ServerVersion":"28.4.0","OSType":"linux","Architecture":"x86_64","NCPU":%d,"MemTotal":%d}`, ncpu, memTotal), nil
	}}
}

// concurrency tracks how many executions are in flight at once.
type concurrency struct {
	inFlight, peak atomic.Int32
}

func (c *concurrency) enter() {
	n := c.inFlight.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (c *concurrency) leave() { c.inFlight.Add(-1) }

func artifactNames(h *Harness) []string {
	var names []string
	for _, a := range h.Artifacts() {
		names = append(names, a.Kind+":"+strings.TrimPrefix(filepath.Base(a.Path), h.runID+"-"))
	}
	return names
}

// Checks that finish in reverse order are still recorded, saved and audited in
// configured order, and they did run at the same time.
func TestRunChecksRecordsConcurrentChecksInConfiguredOrder(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	h := batchFixture(t, 3, nil)
	var c concurrency
	all := make(chan struct{})
	var once sync.Once
	delays := map[string]time.Duration{"test": 60 * time.Millisecond, "typecheck": 30 * time.Millisecond, "build": 0}
	h.execute = func(ctx context.Context, _ string, args []string, out io.Writer) execution {
		c.enter()
		defer c.leave()
		if c.inFlight.Load() == 3 {
			once.Do(func() { close(all) })
		}
		select { // every check waits until all three are in flight
		case <-all:
		case <-time.After(10 * time.Second):
			return execution{ExitCode: -1, Err: errors.New("the checks did not run at the same time")}
		}
		time.Sleep(delays[checkName(args)])
		fmt.Fprintf(out, "%s output\n", checkName(args))
		if checkName(args) == "typecheck" {
			return execution{ExitCode: 1}
		}
		return execution{ExitCode: 0}
	}
	checks := h.RunChecks(context.Background(), initialKinds)
	want := []struct{ id, kind, status string }{{"check-1", "test", "PASS"}, {"check-2", "typecheck", "FAIL"}, {"check-3", "build", "PASS"}}
	if len(checks) != len(want) {
		t.Fatalf("checks %+v", checks)
	}
	for i, w := range want {
		if c := checks[i]; c.ID != w.id || c.Kind != w.kind || c.Status != w.status || c.Output != w.kind+" output\n" || c.Cache != nil {
			t.Fatalf("check %d: %+v, want %+v", i, c, w)
		}
	}
	if got := h.Checks(); len(got) != 3 || got[0].ID != "check-1" || got[2].ID != "check-3" {
		t.Fatalf("ledger %+v", got)
	}
	if c.peak.Load() != 3 {
		t.Fatalf("peak concurrency %d, want 3", c.peak.Load())
	}
	if got := strings.Join(artifactNames(h), ","); got != "check_output:check-1.log,check_output:check-2.log,check_output:check-3.log" {
		t.Fatalf("artifacts %s", got)
	}
	audit := h.Audit()
	if len(audit) != 3 {
		t.Fatalf("audit %+v", audit)
	}
	for i, e := range audit {
		if e.Tool != "run_"+want[i].kind || e.Status != want[i].status || !e.Time.Equal(audit[0].Time) || e.Arguments != "" {
			t.Fatalf("audit event %d: %+v", i, e)
		}
	}
	if audit[0].DurationMS < 60 || audit[2].DurationMS >= audit[0].DurationMS {
		t.Fatalf("audit durations are not each check's own: %+v", audit)
	}
	p := h.Execution().Parallelism
	if p.Requested != 3 || p.Effective != 3 || p.Note != parallelSemanticsNote {
		t.Fatalf("parallelism %+v", p)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reserved != 0 || h.spent < 90*time.Millisecond {
		t.Fatalf("reserved %v spent %v: every check's own time must be charged", h.reserved, h.spent)
	}
}

// Never more checks in flight than the effective limit.
func TestRunChecksBoundsConcurrency(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	h := batchFixture(t, 2, nil)
	var c concurrency
	h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
		c.enter()
		defer c.leave()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(out, "ok\n")
		return execution{ExitCode: 0}
	}
	kinds := []string{"test", "typecheck", "build", "lint"}
	checks := h.RunChecks(context.Background(), kinds)
	if len(checks) != 4 || c.peak.Load() != 2 {
		t.Fatalf("%d checks, peak %d, want 4 and 2", len(checks), c.peak.Load())
	}
	for i, check := range checks {
		if check.ID != fmt.Sprintf("check-%d", i+1) || check.Kind != kinds[i] || check.Status != "PASS" {
			t.Fatalf("check %d %+v", i, check)
		}
	}
	audit := h.Audit()
	if !audit[0].Time.Equal(audit[1].Time) || !audit[2].Time.Equal(audit[3].Time) || !audit[2].Time.After(audit[1].Time) {
		t.Fatalf("two groups of two: %+v", audit)
	}
	if p := h.Execution().Parallelism; p.Requested != 2 || p.Effective != 2 {
		t.Fatalf("parallelism %+v", p)
	}
}

// With --parallel 4, the timeouts in flight never exceed what the budget has
// left, every concurrent check gets the full policy timeout, and a group
// shrinks when the budget cannot cover a full timeout for each member.
func TestRunChecksParallelFourNeverExceedsTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRuntime time.Duration
		groups     []int
		effective  int
		shrunk     bool
	}{
		{"budget_covers_four", 10 * time.Second, []int{4}, 4, false},
		// Two timeouts fit and three do not; after the first group, two
		// still fit unless its runs took more than 450 ms each.
		{"budget_covers_two", 2900 * time.Millisecond, []int{2, 2}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDocker(t, capacityDocker(16, 64<<30))
			h := batchFixture(t, 4, func(o *Options) {
				o.Timeout, o.MaxRuntime = time.Second, tc.maxRuntime
			})
			var mu sync.Mutex
			var inFlight []time.Duration // reserved + spent seen by each execution
			var timeouts []time.Duration
			var c concurrency
			h.execute = func(ctx context.Context, _ string, _ []string, out io.Writer) execution {
				c.enter()
				defer c.leave()
				deadline, _ := ctx.Deadline()
				mu.Lock()
				// plan wrote these before the executions started, and nothing
				// writes them while a group executes.
				inFlight = append(inFlight, h.reserved+h.spent)
				timeouts = append(timeouts, time.Until(deadline))
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				fmt.Fprint(out, "ok\n")
				return execution{ExitCode: 0}
			}
			checks := h.RunChecks(context.Background(), []string{"test", "typecheck", "build", "lint"})
			for _, check := range checks {
				if check.Status != "PASS" {
					t.Fatalf("check %+v", check)
				}
			}
			for i, seen := range inFlight {
				if seen > tc.maxRuntime {
					t.Fatalf("execution %d: %v reserved and spent exceed the %v budget", i, seen, tc.maxRuntime)
				}
			}
			for i, d := range timeouts {
				if d > time.Second || d < 500*time.Millisecond {
					t.Fatalf("execution %d had a %v timeout, want the full 1s policy timeout", i, d)
				}
			}
			if int(c.peak.Load()) != tc.effective {
				t.Fatalf("peak concurrency %d, want %d", c.peak.Load(), tc.effective)
			}
			audit := h.Audit()
			var groups []int
			for i := range audit {
				if i == 0 || !audit[i].Time.Equal(audit[i-1].Time) {
					groups = append(groups, 0)
				}
				groups[len(groups)-1]++
			}
			if fmt.Sprint(groups) != fmt.Sprint(tc.groups) {
				t.Fatalf("groups %v, want %v", groups, tc.groups)
			}
			p := h.Execution().Parallelism
			if p.Requested != 4 || p.Effective != tc.effective || strings.Contains(p.Note, parallelBudgetNote) != tc.shrunk || !strings.Contains(p.Note, parallelSemanticsNote) {
				t.Fatalf("parallelism %+v", p)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.reserved != 0 || h.spent > tc.maxRuntime {
				t.Fatalf("after the checks: reserved %v, spent %v of %v", h.reserved, h.spent, tc.maxRuntime)
			}
		})
	}
}

// When the budget cannot cover two full timeouts, the checks run one at a time
// and end exactly as they do sequentially: the first takes what remains, the
// rest are skipped. No check can then run beside another, so the capacity of
// the Docker server is not read.
func TestRunChecksExhaustedBudgetMatchesOneAtATime(t *testing.T) {
	forbidDocker(t)
	h := batchFixture(t, 3, func(o *Options) { o.Timeout, o.MaxRuntime = 100*time.Millisecond, 50*time.Millisecond })
	var c concurrency
	h.execute = func(ctx context.Context, _ string, _ []string, _ io.Writer) execution {
		c.enter()
		defer c.leave()
		<-ctx.Done()
		return execution{ExitCode: -1, TimedOut: true}
	}
	checks := h.RunChecks(context.Background(), initialKinds)
	if checks[0].Status != "TIMEOUT" || checks[1].Status != "SKIPPED" || checks[1].Output != budgetExhaustedText || checks[2].Status != "SKIPPED" || c.peak.Load() != 1 {
		t.Fatalf("checks %+v (peak %d)", checks, c.peak.Load())
	}
	p := h.Execution().Parallelism
	if p.Effective != 1 || p.Note != parallelSerialNote+" "+parallelBudgetNote {
		t.Fatalf("parallelism %+v", p)
	}

	// A budget used up before the initial checks: every check is SKIPPED, as
	// one at a time, no container can start, and the note does not say that
	// any of them ran.
	spentBefore := batchFixture(t, 3, func(o *Options) { o.Timeout, o.MaxRuntime = 100*time.Millisecond, 50*time.Millisecond })
	calls := countingExec(spentBefore, "ok\n")
	spentBefore.mu.Lock()
	spentBefore.spent = 50 * time.Millisecond
	spentBefore.mu.Unlock()
	for _, check := range spentBefore.RunChecks(context.Background(), initialKinds) {
		if check.Status != "SKIPPED" || check.Output != budgetExhaustedText {
			t.Fatalf("check after the budget was used up %+v", check)
		}
	}
	if p := spentBefore.Execution().Parallelism; *calls != 0 || p.Effective != 1 || p.Note != parallelNoSandboxNote+" "+parallelBudgetUsedNote {
		t.Fatalf("parallelism %+v, executor calls %d", p, *calls)
	}
}

// The capacity of the Docker server is read only when the budget left covers
// two full per-run timeouts: just below that, every check runs alone, with no
// Docker call; at exactly two, one call, and two checks run together.
func TestRunChecksProbesOnlyWhenTheBudgetCoversTwoTimeouts(t *testing.T) {
	quick := func(h *Harness, c *concurrency) {
		h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
			c.enter()
			defer c.leave()
			time.Sleep(10 * time.Millisecond)
			fmt.Fprint(out, "ok\n")
			return execution{ExitCode: 0}
		}
	}

	t.Run("below_two_timeouts", func(t *testing.T) {
		forbidDocker(t)
		h := batchFixture(t, 3, func(o *Options) { o.Timeout, o.MaxRuntime = 10*time.Second, 20*time.Second-time.Millisecond })
		var c concurrency
		quick(h, &c)
		for _, check := range h.RunChecks(context.Background(), initialKinds) {
			if check.Status != "PASS" {
				t.Fatalf("check %+v", check)
			}
		}
		if p := h.Execution().Parallelism; c.peak.Load() != 1 || p.Effective != 1 || p.Note != parallelSerialNote+" "+parallelBudgetNote {
			t.Fatalf("parallelism %+v (peak %d)", p, c.peak.Load())
		}
	})

	t.Run("two_timeouts", func(t *testing.T) {
		docker := capacityDocker(8, 16<<30)
		useDocker(t, docker)
		h := batchFixture(t, 3, func(o *Options) { o.Timeout, o.MaxRuntime = 10*time.Second, 20*time.Second })
		var c concurrency
		quick(h, &c)
		for _, check := range h.RunChecks(context.Background(), initialKinds) {
			if check.Status != "PASS" {
				t.Fatalf("check %+v", check)
			}
		}
		if p := h.Execution().Parallelism; docker.calls != 1 || p.Effective != 2 || p.Note != parallelBudgetNote+" "+parallelSemanticsNote {
			t.Fatalf("parallelism %+v, docker calls %d", p, docker.calls)
		}
	})
}

// normalizedRecords lists what a run of the initial checks recorded, without
// times and durations.
func normalizedRecords(h *Harness) string {
	var b strings.Builder
	for _, c := range h.Checks() {
		fmt.Fprintf(&b, "check %s %s %s %d %v %q %q\n", c.ID, c.Kind, c.Status, c.ExitCode, c.Truncated, c.Output, c.Command)
	}
	for _, a := range artifactNames(h) {
		fmt.Fprintf(&b, "artifact %s\n", a)
	}
	for _, e := range h.Audit() {
		fmt.Fprintf(&b, "audit %s %s %q\n", e.Tool, e.Status, e.Arguments)
	}
	return b.String()
}

// RunChecks records the same checks, artifacts and audit events as Run called
// once per kind, one at a time or concurrently: statuses, exit codes,
// truncation, redaction and log retention do not depend on parallelism.
func TestRunChecksRecordsWhatRunRecords(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	executor := func(ctx context.Context, _ string, args []string, out io.Writer) execution {
		switch checkName(args) {
		case "test":
			fmt.Fprint(out, "api_key=abcdefghijklmnop\n"+strings.Repeat("x", 300)+"\n")
			return execution{ExitCode: 1}
		case "typecheck":
			fmt.Fprint(out, "fork/exec /usr/local/go/bin/go: permission denied\n")
			return execution{ExitCode: 2}
		case "build":
			return execution{ExitCode: 125}
		}
		return execution{ExitCode: 0}
	}
	edit := func(o *Options) { o.MaxOutputBytes = 256 }
	sequential := batchFixture(t, 0, edit)
	sequential.execute = executor
	for _, kind := range append(initialKinds, "missing") {
		sequential.Run(context.Background(), kind)
	}
	want := normalizedRecords(sequential)
	for _, parallel := range []int{1, 3, 4} {
		h := batchFixture(t, parallel, edit)
		h.execute = executor
		h.RunChecks(context.Background(), append(initialKinds, "missing"))
		if got := normalizedRecords(h); got != want {
			t.Fatalf("--parallel %d recorded\n%s\nRun recorded\n%s", parallel, got, want)
		}
		// With four at a time, the check without a command shares the group
		// but starts no sandbox: three ran at the same time.
		p := h.Execution().Parallelism
		if wantEffective := min(parallel, 3); p.Effective != wantEffective || strings.Contains(p.Note, parallelUnstartedNote) != (parallel == 4) {
			t.Fatalf("--parallel %d: parallelism %+v", parallel, p)
		}
	}
	for _, want := range []string{"check-1 test FAIL 1 true", "[REDACTED]", "check-2 typecheck ERROR 2", "check-3 build ERROR 125", `check-4 missing SKIPPED -1 false "No command configured."`} {
		if !strings.Contains(normalizedRecords(sequential), want) {
			t.Fatalf("the reference run lacks %q:\n%s", want, normalizedRecords(sequential))
		}
	}
}

// The capacity probe caps the concurrency by the Docker server's CPUs and
// memory; it runs only when more than one check could run at a time, and a
// probe failure means one at a time.
func TestRunChecksParallelismCappedByDockerCapacity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		docker    *fakeDocker
		parallel  int
		cpus, mem int
		effective int
		note      []string
		exact     string // the whole note, when set
	}{
		{"cpus", capacityDocker(2, 32<<30), 3, 2, 1024, 1, nil, parallelSerialNote + " The Docker server reports 2 CPUs and 32768 MiB of memory: room for 1 sandbox of 2 CPUs and 1024 MiB at a time."},
		{"cpus_two", capacityDocker(2, 32<<30), 3, 1, 1024, 2, nil, "The Docker server reports 2 CPUs and 32768 MiB of memory: room for 2 sandboxes of 1 CPU and 1024 MiB at a time. " + parallelSemanticsNote},
		{"memory", capacityDocker(16, 3<<30), 4, 1, 2048, 1, []string{parallelSerialNote, "3072 MiB of memory: room for 1 sandbox"}, ""},
		// A capacity of 0 is reported as such: the checks still run one at a
		// time, as with --parallel 1, but the probe found no room.
		{"no_room_cpus", capacityDocker(1, 32<<30), 3, 2, 1024, 1, nil, parallelSerialNote + " The Docker server reports 1 CPU and 32768 MiB of memory: no room for a single sandbox of 2 CPUs and 1024 MiB."},
		{"no_room_memory", capacityDocker(2, 30865<<20), 3, 1, 32768, 1, nil, parallelSerialNote + " The Docker server reports 2 CPUs and 30865 MiB of memory: no room for a single sandbox of 1 CPU and 32768 MiB."},
		{"enough", capacityDocker(16, 64<<30), 3, 2, 1024, 3, nil, parallelSemanticsNote},
		{"zero_cpus", capacityDocker(0, 64<<30), 3, 2, 1024, 1, []string{parallelSerialNote, "could not be read: docker info reported 0 CPUs"}, ""},
		{"probe_error", &fakeDocker{info: func() (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }}, 3, 2, 1024, 1, []string{parallelSerialNote, "The capacity of the Docker server could not be read: docker info"}, ""},
		{"server_error", &fakeDocker{info: func() (string, error) {
			return `{"ServerVersion":"28.4.0","NCPU":8,"MemTotal":1,"ServerErrors":["daemon unhealthy"]}`, nil
		}}, 3, 2, 1024, 1, []string{"could not be read", "daemon unhealthy"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDocker(t, tc.docker)
			h := batchFixture(t, tc.parallel, func(o *Options) { o.CPUs, o.MemoryMB = tc.cpus, tc.mem })
			var c concurrency
			h.execute = func(context.Context, string, []string, io.Writer) execution {
				c.enter()
				defer c.leave()
				time.Sleep(20 * time.Millisecond)
				return execution{ExitCode: 0}
			}
			h.RunChecks(context.Background(), initialKinds)
			p := h.Execution().Parallelism
			if p.Requested != tc.parallel || p.Effective != tc.effective || int(c.peak.Load()) > tc.effective || tc.docker.calls != 1 {
				t.Fatalf("parallelism %+v, peak %d, docker calls %d", p, c.peak.Load(), tc.docker.calls)
			}
			if tc.exact != "" && p.Note != tc.exact {
				t.Fatalf("note %q, want %q", p.Note, tc.exact)
			}
			for _, part := range tc.note {
				if !strings.Contains(p.Note, part) {
					t.Fatalf("note %q lacks %q", p.Note, part)
				}
			}
		})
	}
}

// No Docker call and no concurrency when one check at a time is requested,
// when there is a single check, and when no container can start. When no
// initial check started a sandbox, the note says so instead of saying that
// they ran.
func TestRunChecksProbesOnlyWhenChecksCanOverlap(t *testing.T) {
	forbidDocker(t)
	one := batchFixture(t, 1, nil)
	countingExec(one, "ok\n")
	one.RunChecks(context.Background(), initialKinds)
	if p := one.Execution().Parallelism; p != (model.ExecutionParallelism{Requested: 1, Effective: 1, Note: parallelOneNote}) {
		t.Fatalf("parallelism %+v", p)
	}

	single := batchFixture(t, 4, nil)
	countingExec(single, "ok\n")
	if checks := single.RunChecks(context.Background(), []string{"build"}); len(checks) != 1 || checks[0].ID != "check-1" || checks[0].Status != "PASS" {
		t.Fatalf("checks %+v", checks)
	}
	if p := single.Execution().Parallelism; p.Effective != 1 || p.Note != parallelSerialNote+" Only one initial check was requested." {
		t.Fatalf("parallelism %+v", p)
	}

	noImage := batchFixture(t, 3, func(o *Options) { o.Image = "" })
	calls := countingExec(noImage, "ok\n")
	for _, c := range noImage.RunChecks(context.Background(), initialKinds) {
		if c.Status != "SKIPPED" {
			t.Fatalf("check without an image %+v", c)
		}
	}
	if p := noImage.Execution().Parallelism; *calls != 0 || p.Effective != 1 || p.Note != "No initial check started a sandbox. No sandbox image is configured." {
		t.Fatalf("parallelism %+v, executor calls %d", p, *calls)
	}

	singleNoImage := batchFixture(t, 2, func(o *Options) { o.Image = "" })
	singleNoImage.RunChecks(context.Background(), []string{"test"})
	if p := singleNoImage.Execution().Parallelism; p.Effective != 1 || p.Note != "No initial check started a sandbox. Only one initial check was requested." {
		t.Fatalf("parallelism %+v", p)
	}

	expired, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), ErrOverallDeadline)
	defer cancel()
	late := batchFixture(t, 3, nil)
	calls = countingExec(late, "ok\n")
	for _, c := range late.RunChecks(expired, initialKinds) {
		if c.Status != "SKIPPED" || c.Output != deadlineText {
			t.Fatalf("check after the overall deadline %+v", c)
		}
	}
	if p := late.Execution().Parallelism; *calls != 0 || p.Effective != 1 || p.Note != "No initial check started a sandbox. The time limit was reached before the initial checks started." {
		t.Fatalf("parallelism %+v, executor calls %d", p, *calls)
	}

	// A cancelled review (an interrupt) is not a time limit. Its runs are
	// launched, as they are one at a time, but the Docker client does not
	// start on an ended context: the executor below behaves as runDocker.
	interrupted, stop := context.WithCancel(context.Background())
	stop()
	cancelled := batchFixture(t, 3, nil)
	var starts atomic.Int32
	cancelled.execute = func(ctx context.Context, _ string, _ []string, _ io.Writer) execution {
		if ctx.Err() != nil {
			return execution{ExitCode: -1, Err: ctx.Err(), TimedOut: true}
		}
		starts.Add(1)
		return execution{ExitCode: 0}
	}
	for _, c := range cancelled.RunChecks(interrupted, initialKinds) {
		if c.Status != "TIMEOUT" {
			t.Fatalf("check of a cancelled review %+v", c)
		}
	}
	if p := cancelled.Execution().Parallelism; starts.Load() != 0 || p.Effective != 1 || p.Note != "No initial check started a sandbox. The review was cancelled before the initial checks started." {
		t.Fatalf("parallelism %+v, started %d", p, starts.Load())
	}

	closed := batchFixture(t, 3, nil)
	closed.Close()
	for _, c := range closed.RunChecks(context.Background(), initialKinds) {
		if c.Status != "ERROR" || c.Output != "harness is closed" {
			t.Fatalf("check on a closed harness %+v", c)
		}
	}
	if p := closed.Execution().Parallelism; p.Effective != 1 || p.Note != "No initial check started a sandbox. The sandbox harness was closed." {
		t.Fatalf("parallelism of a closed harness %+v", p)
	}

	// Without kinds, RunChecks records nothing: the summary stays the one the
	// harness started with.
	empty := batchFixture(t, 3, nil)
	if checks := empty.RunChecks(context.Background(), nil); checks == nil || len(checks) != 0 {
		t.Fatalf("no kinds: %#v", checks)
	}
	if p := empty.Execution().Parallelism; p != (model.ExecutionParallelism{Requested: 3, Effective: 1, Note: sequentialNote(3)}) {
		t.Fatalf("parallelism without initial checks %+v", p)
	}
}

// Before RunChecks records a summary, the harness keeps the one it started
// with (newExecState's); RunChecks replaces it, and a later call with a
// smaller group does not lower what an earlier call recorded.
func TestParallelismSummaryFollowsRunChecks(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	h := batchFixture(t, 3, nil)
	if p := h.Execution().Parallelism; p != (model.ExecutionParallelism{Requested: 3, Effective: 1, Note: sequentialNote(3)}) {
		t.Fatalf("parallelism before the initial checks %+v", p)
	}
	h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
		fmt.Fprint(out, "ok\n")
		return execution{ExitCode: 0}
	}
	h.RunChecks(context.Background(), []string{"test", "typecheck"})
	first := h.Execution().Parallelism
	if first.Effective != 2 || first.Note != "Only 2 initial checks were requested. "+parallelSemanticsNote {
		t.Fatalf("parallelism %+v", first)
	}
	h.RunChecks(context.Background(), []string{"build"})
	if p := h.Execution().Parallelism; p != first {
		t.Fatalf("a later single check replaced the summary: %+v", p)
	}
	if checks := h.Checks(); len(checks) != 3 || checks[2].ID != "check-3" || checks[2].Kind != "build" {
		t.Fatalf("ledger %+v", checks)
	}
}

// RunChecks holds h.mu while a group runs: a reader never sees part of a
// group, and no other run can start in between.
func TestRunChecksHoldsTheHarnessDuringAGroup(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	h := batchFixture(t, 3, nil)
	seen := make(chan int, 1)
	var once sync.Once
	h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
		once.Do(func() {
			go func() { seen <- len(h.Checks()) }()
			time.Sleep(50 * time.Millisecond) // give the reader time to try
		})
		fmt.Fprint(out, "ok\n")
		return execution{ExitCode: 0}
	}
	h.RunChecks(context.Background(), initialKinds)
	select {
	case n := <-seen:
		if n != 3 {
			t.Fatalf("a reader saw %d checks while the group ran", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reader never returned")
	}
}

// An expired overall deadline during a group ends the running checks as
// TIMEOUT, and the checks of later groups are skipped with the deadline text.
func TestRunChecksDeadlineDuringAGroup(t *testing.T) {
	useDocker(t, capacityDocker(8, 16<<30))
	h := batchFixture(t, 2, nil)
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(100*time.Millisecond), ErrOverallDeadline)
	defer cancel()
	h.execute = func(ctx context.Context, _ string, _ []string, _ io.Writer) execution {
		<-ctx.Done()
		return execution{ExitCode: -1, Err: ctx.Err(), TimedOut: true}
	}
	checks := h.RunChecks(ctx, initialKinds)
	if checks[0].Status != "TIMEOUT" || checks[1].Status != "TIMEOUT" || checks[2].Status != "SKIPPED" || checks[2].Output != deadlineText {
		t.Fatalf("checks %+v", checks)
	}
}

func TestSandboxCapacity(t *testing.T) {
	for _, tc := range []struct {
		ncpu      int
		mem       int64
		cpus, mb  int
		want      int
		rationale string
	}{
		{8, 16 << 30, 2, 1024, 4, "cpus bound"},
		{8, 3 << 30, 1, 1024, 3, "memory bound"},
		{1, 16 << 30, 2, 1024, 0, "one sandbox does not fit the CPUs"},
		{8, 512 << 20, 1, 1024, 0, "one sandbox does not fit the memory"},
		{8, 16 << 30, 0, 1024, 0, "invalid CPUs"},
	} {
		if got := sandboxCapacity(dockerutil.Info{NCPU: tc.ncpu, MemTotal: tc.mem}, tc.cpus, tc.mb); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.rationale, got, tc.want)
		}
	}
}

// Real containers: with room for two sandboxes, two initial checks run at the
// same time on the host (their docker invocations overlap), the checks are
// recorded in configured order with the statuses the commands produce, every
// check's own time is charged, and no container survives.
func TestDockerRunChecksRunsInitialChecksConcurrently(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	info, err := dockerutil.ServerInfo(context.Background(), dockerutil.DefaultRunner)
	if err != nil {
		t.Fatal(err)
	}
	src, base := t.TempDir(), t.TempDir()
	for _, dir := range []string{src, base} {
		for path, content := range map[string]string{
			"go.mod":           "module example.test/calc\n\ngo 1.23.0\n",
			"calc/calc.go":     "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
			"calc/add_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"2+3\")\n\t}\n}\n",
		} {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	h, err := New(Options{CandidateDir: src, BaseDir: base, ArtifactDir: t.TempDir(), Image: image, Parallel: 3, CPUs: 1, MemoryMB: 512,
		Timeout: 3 * time.Minute, MaxRuntime: 20 * time.Minute, MaxOutputBytes: 64 * 1024,
		Commands: map[string][]string{
			"test":      {"go", "test", "./..."},
			"typecheck": {"sh", "-c", "sleep 3; go vet ./..."},
			"build":     {"sh", "-c", "sleep 3; echo build step failed; exit 3"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	type interval struct {
		name, last string // container name, last argument of the command
		start, end time.Time
	}
	var mu sync.Mutex
	var runs []interval
	execute := h.execute
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		started := time.Now()
		result := execute(ctx, name, args, out)
		mu.Lock()
		runs = append(runs, interval{name, checkName(args), started, time.Now()})
		mu.Unlock()
		return result
	}
	checks := h.RunChecks(context.Background(), initialKinds)
	want := []struct{ kind, status string }{{"test", "PASS"}, {"typecheck", "PASS"}, {"build", "FAIL"}}
	for i, w := range want {
		c := checks[i]
		if c.ID != fmt.Sprintf("check-%d", i+1) || c.Kind != w.kind || c.Status != w.status {
			t.Fatalf("check %d: %+v, want %s %s", i, c, w.kind, w.status)
		}
	}
	if checks[2].ExitCode != 3 || !strings.Contains(checks[2].Output, "build step failed") {
		t.Fatalf("build check %+v", checks[2])
	}
	capacity := min(sandboxCapacity(info, 1, 512), 3)
	p := h.Execution().Parallelism
	if p.Requested != 3 || p.Effective != max(capacity, 1) {
		t.Fatalf("parallelism %+v with a server of %d CPUs and %d bytes", p, info.NCPU, info.MemTotal)
	}
	if len(runs) != 3 {
		t.Fatalf("%d containers, want 3", len(runs))
	}
	var names []string
	byCommand := map[string]interval{}
	for _, r := range runs {
		names = append(names, r.name)
		byCommand[r.last] = r
	}
	if capacity >= 2 {
		// The first group holds test and typecheck: each container started
		// before the other one ended.
		test, typecheck := byCommand["./..."], byCommand["sleep 3; go vet ./..."]
		if !test.start.Before(typecheck.end) || !typecheck.start.Before(test.end) {
			t.Fatalf("test and typecheck did not run at the same time: %+v", runs)
		}
	} else {
		t.Logf("the Docker server has room for one sandbox of 1 CPU and 512 MiB; the checks ran one at a time: %s", p.Note)
	}
	var own time.Duration
	for _, r := range runs {
		own += r.end.Sub(r.start)
	}
	h.mu.Lock()
	spent, reserved := h.spent, h.reserved
	h.mu.Unlock()
	if reserved != 0 || spent < own-time.Second {
		t.Fatalf("spent %v for %v of container time (reserved %v)", spent, own, reserved)
	}
	assertNoContainers(t, names)
}
