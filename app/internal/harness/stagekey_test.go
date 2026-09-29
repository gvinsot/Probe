package harness

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// The F3 and F6b stages key their baseline runs with their fixed 180 s
// sub-cap, never with what remains of it: the remainder depends on how long
// the stage's earlier runs took, so a key holding it would never repeat
// across reviews. These tests use a policy timeout above the sub-cap and an
// executor that takes a few milliseconds per run, the setting in which a
// remainder-keyed run changes its key between reviews.

const stageKeyPolicyTimeout = 300 * time.Second

// stageKeyDelay makes every fake execution take long enough to change a
// millisecond-resolution remainder.
const stageKeyDelay = 3 * time.Millisecond

func stageKeyReplays(h *Harness, from int, kind string) int {
	n := 0
	for _, c := range h.Checks()[from:] {
		if c.Kind == kind && c.Replayed() {
			n++
		}
	}
	return n
}

// §1.11: when the live re-run of a replayed F3 baseline fails, the entry it
// was replayed from is evicted as contradicted, and the next review runs the
// baseline live instead of replaying it again.
func TestBaseTestsContradictedReplayIsEvicted(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	baseAction, hybridAction := "pass", "pass"
	runs := btExec(t, h, func(r btRun, n int) (string, execution) {
		time.Sleep(stageKeyDelay)
		return btAll(baseAction, hybridAction)(r, n)
	})
	selected := btSelected()[:1]
	// Two reviews with a live baseline record two agreeing live runs of one key.
	for i := 0; i < 2; i++ {
		if res, err := h.RunBaseTests(context.Background(), selected); err != nil || res.Tests[0].Status != model.StatusPassesOnCandidate {
			t.Fatalf("review %d: %+v %v", i+1, res, err)
		}
	}
	keys := cache.keys()
	if len(keys) != 1 {
		t.Fatalf("cache keys %v, want one", keys)
	}
	if e, _ := cache.entry(keys[0]); e.LiveRuns != 2 {
		t.Fatalf("entry %+v, want two agreeing live runs", e)
	}
	// Third review: the replayed baseline would support FAILS; its live
	// re-run fails, which contradicts the entry.
	hybridAction, baseAction = "fail", "fail"
	from := len(h.Checks())
	res, _ := h.RunBaseTests(context.Background(), selected)
	if res.Tests[0].Status != model.StatusUnverified || stageKeyReplays(h, from, model.CheckBaseTestBase) != 1 {
		t.Fatalf("third review: %+v, checks %+v", res.Tests[0], h.Checks()[from:])
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 {
		t.Fatalf("cache counters %+v, want one contradicted and evicted entry", c)
	}
	if keys := cache.keys(); len(keys) != 0 {
		t.Fatalf("cache keys %v after the contradiction, want none", keys)
	}
	// Fourth review: nothing may be replayed from the contradicted entry.
	hybridAction, baseAction = "pass", "pass"
	from, before := len(h.Checks()), len(*runs)
	res, _ = h.RunBaseTests(context.Background(), selected)
	e := btEvidence(t, h, res.Tests[0].EvidenceID)
	if res.Tests[0].Status != model.StatusPassesOnCandidate || btCheck(t, h, e.BaseCheckID).Replayed() || stageKeyReplays(h, from, model.CheckBaseTestBase) != 0 || len(*runs)-before != 2 {
		t.Fatalf("fourth review: %+v, evidence %+v, %d executions", res.Tests[0], e, len(*runs)-before)
	}
}

// A live re-run that passes as a whole but no longer passes the test the
// replay recorded as passed also contradicts the entry: the write-through
// extended it, so it is removed here, and the live check keeps no cache
// provenance.
func TestBaseTestsReplayContradictedByTestOutcome(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	baseAction, hybridAction := "pass", "pass"
	btExec(t, h, func(r btRun, n int) (string, execution) {
		time.Sleep(stageKeyDelay)
		return btAll(baseAction, hybridAction)(r, n)
	})
	selected := btSelected()[:1]
	for i := 0; i < 2; i++ {
		h.RunBaseTests(context.Background(), selected)
	}
	hybridAction, baseAction = "fail", "skip"
	res, _ := h.RunBaseTests(context.Background(), selected)
	e := btEvidence(t, h, res.Tests[0].EvidenceID)
	live := btCheck(t, h, e.BaseCheckID)
	if res.Tests[0].Status != model.StatusUnverified || live.Replayed() || live.Status != "PASS" || live.Cache != nil {
		t.Fatalf("contradicted by outcome: %+v, live base %+v", res.Tests[0], live)
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 || len(cache.keys()) != 0 {
		t.Fatalf("cache counters %+v, keys %v", c, cache.keys())
	}
}

// The same rules for F6b impacted tests.
func TestImpactedTestsContradictedReplayIsEvicted(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	baseAction, candidateAction := "pass", "pass"
	runs := itExec(t, h, func(r itRun, n int) (string, execution) {
		time.Sleep(stageKeyDelay)
		return itAll(baseAction, candidateAction)(r, n)
	})
	selected := itSelected()[:1]
	for i := 0; i < 2; i++ {
		if res := h.RunImpactedTests(context.Background(), selected); res.Tests[0].Status != model.StatusPassesOnCandidate {
			t.Fatalf("review %d: %+v", i+1, res)
		}
	}
	if keys := cache.keys(); len(keys) != 1 {
		t.Fatalf("cache keys %v, want one", keys)
	}
	candidateAction, baseAction = "fail", "fail"
	from := len(h.Checks())
	res := h.RunImpactedTests(context.Background(), selected)
	if res.Tests[0].Status != model.StatusUnverified || !strings.Contains(res.Tests[0].Reason, "baseline run did not pass") || stageKeyReplays(h, from, model.CheckImpactedTestBase) != 1 {
		t.Fatalf("third review: %+v, checks %+v", res.Tests[0], h.Checks()[from:])
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 || len(cache.keys()) != 0 {
		t.Fatalf("cache counters %+v, keys %v", c, cache.keys())
	}
	candidateAction, baseAction = "pass", "pass"
	from, before := len(h.Checks()), len(*runs)
	res = h.RunImpactedTests(context.Background(), selected)
	e := itEvidence(t, h, res.Tests[0].EvidenceID)
	if res.Tests[0].Status != model.StatusPassesOnCandidate || itCheck(t, h, e.BaseCheckID).Replayed() || stageKeyReplays(h, from, model.CheckImpactedTestBase) != 0 || len(*runs)-before != 2 {
		t.Fatalf("fourth review: %+v, evidence %+v, %d executions", res.Tests[0], e, len(*runs)-before)
	}
}

func TestImpactedTestsReplayContradictedByTestOutcome(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	baseAction, candidateAction := "pass", "pass"
	itExec(t, h, func(r itRun, n int) (string, execution) {
		time.Sleep(stageKeyDelay)
		return itAll(baseAction, candidateAction)(r, n)
	})
	selected := itSelected()[:1]
	for i := 0; i < 2; i++ {
		h.RunImpactedTests(context.Background(), selected)
	}
	candidateAction, baseAction = "fail", "skip"
	res := h.RunImpactedTests(context.Background(), selected)
	e := itEvidence(t, h, res.Tests[0].EvidenceID)
	live := itCheck(t, h, e.BaseCheckID)
	if res.Tests[0].Status != model.StatusUnverified || live.Replayed() || live.Status != "PASS" || live.Cache != nil {
		t.Fatalf("contradicted by outcome: %+v, live base %+v", res.Tests[0], live)
	}
	if c := h.Execution().Cache; c.Contradicted != 1 || c.Evicted != 1 || len(cache.keys()) != 0 {
		t.Fatalf("cache counters %+v, keys %v", c, cache.keys())
	}
}

// A live re-run that reproduces the replay keeps the entry and extends it.
func TestImpactedTestsConfirmedReplayKeepsEntry(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	candidateAction := "pass"
	itExec(t, h, func(r itRun, n int) (string, execution) {
		time.Sleep(stageKeyDelay)
		return itAll("pass", candidateAction)(r, n)
	})
	selected := itSelected()[:1]
	for i := 0; i < 2; i++ {
		h.RunImpactedTests(context.Background(), selected)
	}
	candidateAction = "fail"
	res := h.RunImpactedTests(context.Background(), selected)
	e := itEvidence(t, h, res.Tests[0].EvidenceID)
	live := itCheck(t, h, e.BaseCheckID)
	keys := cache.keys()
	if res.Tests[0].Status != model.StatusFailsOnCandidate || live.Replayed() || live.Cache == nil || live.Cache.LiveRuns != 3 || len(keys) != 1 {
		t.Fatalf("confirmed replay: %+v, live base %+v, keys %v", res.Tests[0], live, keys)
	}
	if c := h.Execution().Cache; c.Contradicted != 0 || c.Evicted != 0 {
		t.Fatalf("cache counters %+v", c)
	}
}

// Every unit of the stage keeps its key across reviews, not only the first:
// on the third review both units' baselines are replayed.
func TestImpactedTestsLaterUnitsKeepTheirKey(t *testing.T) {
	base, candidate := itTrees()
	h := itHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	review := 0
	itExec(t, h, func(r itRun, n int) (string, execution) {
		// Each review's runs take a different time, so a key holding what
		// remains of the sub-cap would differ from review to review.
		time.Sleep(time.Duration(review) * stageKeyDelay)
		return itAll("pass", "pass")(r, n)
	})
	selected := []model.ImpactTest{itSelected()[0], itSelected()[2]} // pkg, then other
	for review = 1; review <= 3; review++ {
		from := len(h.Checks())
		res := h.RunImpactedTests(context.Background(), selected)
		for _, test := range res.Tests {
			if test.Status != model.StatusPassesOnCandidate {
				t.Fatalf("review %d: %+v", review, res.Tests)
			}
		}
		want := 0
		if review == 3 {
			want = 2
		}
		if got := stageKeyReplays(h, from, model.CheckImpactedTestBase); got != want {
			t.Fatalf("review %d: %d replayed baselines, want %d; keys %v", review, got, want, cache.keys())
		}
	}
	if keys := cache.keys(); len(keys) != 2 {
		t.Fatalf("cache keys %v, want one per unit", keys)
	}
}

// The same for F3: two units, the second is replayed on the third review.
func TestBaseTestsLaterUnitsKeepTheirKey(t *testing.T) {
	base, candidate := btTrees()
	base["other/o_test.go"] = "package other\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"c\")\n\t}\n}\n"
	candidate["other/o_test.go"] = "package other\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n"
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	h.opts.Timeout = stageKeyPolicyTimeout
	cache := useMemoryCache(h)
	review := 0
	btExec(t, h, func(r btRun, n int) (string, execution) {
		time.Sleep(time.Duration(review) * stageKeyDelay)
		return btAll("pass", "pass")(r, n)
	})
	selected := []model.BaseTest{
		btSelected()[0],
		{Name: "TestC", Path: "other/o_test.go", Line: 5, EndLine: 9, CandidatePath: "other/o_test.go", CandidateLine: 5, CandidateEndLine: 5, Change: model.BaseTestModified, Status: model.StatusUnverified},
	}
	for review = 1; review <= 3; review++ {
		from := len(h.Checks())
		res, err := h.RunBaseTests(context.Background(), selected)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range res.Tests {
			if test.Status != model.StatusPassesOnCandidate {
				t.Fatalf("review %d: %+v", review, res.Tests)
			}
		}
		want := 0
		if review == 3 {
			want = 2
		}
		if got := stageKeyReplays(h, from, model.CheckBaseTestBase); got != want {
			t.Fatalf("review %d: %d replayed baselines, want %d; keys %v", review, got, want, cache.keys())
		}
	}
	if keys := cache.keys(); len(keys) != 2 {
		t.Fatalf("cache keys %v, want one per unit", keys)
	}
}

// The remainder of a sub-cap bounds the launch timeout, the budget
// reservation and the duration an entry may have to be served, but it is not
// part of the key; runOptions.timeout still is.
func TestRemainingBoundsLaunchNotKey(t *testing.T) {
	h := fixture(t)
	h.opts.Timeout = stageKeyPolicyTimeout
	m := useMemoryCache(h)
	var remaining, reserved time.Duration
	calls := 0
	h.execute = func(ctx context.Context, _ string, _ []string, out io.Writer) execution {
		calls++
		remaining = deadlineRemaining(t, ctx)
		reserved = h.reserved // runLocked holds h.mu during the execution
		_, _ = io.WriteString(out, "output\n")
		return execution{}
	}
	first := runBase(h, runOptions{timeout: 180 * time.Second, remaining: 2 * time.Second})
	if first.Status != "PASS" || remaining > 2*time.Second || reserved != 2*time.Second || first.Cache == nil {
		t.Fatalf("first run %+v, launch timeout %v, reserved %v", first, remaining, reserved)
	}
	second := runBase(h, runOptions{timeout: 180 * time.Second, remaining: 170 * time.Second})
	if second.Replayed() || remaining > 170*time.Second || remaining < 160*time.Second || second.Cache == nil || second.Cache.Key != first.Cache.Key || second.Cache.LiveRuns != 2 {
		t.Fatalf("second run %+v, launch timeout %v", second.Cache, remaining)
	}
	// An entry recorded at or above the launch timeout is not served.
	m.update(first.Cache.Key, func(e *CacheEntry) { e.DurationMS = 3000 })
	if c := runBase(h, runOptions{timeout: 180 * time.Second, remaining: 3 * time.Second}); c.Replayed() || calls != 3 {
		t.Fatalf("an entry recorded at the launch timeout was served: %+v", c.Cache)
	}
	m.update(first.Cache.Key, func(e *CacheEntry) { e.DurationMS = 3000 })
	if c := runBase(h, runOptions{timeout: 180 * time.Second, remaining: 4 * time.Second}); !c.Replayed() || calls != 3 {
		t.Fatalf("an entry recorded below the launch timeout was not served: %+v", c.Cache)
	}
	if keys := m.keys(); len(keys) != 1 {
		t.Fatalf("keys %v, want one: the remainder must not change the key", keys)
	}
	// The timeout itself stays keyed.
	if c := runBase(h, runOptions{timeout: 170 * time.Second, remaining: 4 * time.Second}); c.Replayed() || len(m.keys()) != 2 {
		t.Fatalf("another keyed timeout reused the entry: %+v, keys %v", c.Cache, m.keys())
	}
}
