package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// replayReport has three generated experiments:
//   - evidence-1 NOT_REPRODUCED on a replayed baseline (two agreeing live runs);
//   - evidence-2 NOT_REPRODUCED on a live baseline;
//   - evidence-3 REPRODUCED on the live confirmation of a replayed baseline.
func replayReport() *model.Report {
	command := []string{"go", "test", "-json", "-count=1", "-run", "^TestRegression$", "."}
	check := func(id, kind, status string, exit int, output string, cache *model.CheckCache) model.Check {
		return model.Check{ID: id, Kind: kind, Status: status, ExitCode: exit, Command: command, Output: output, Cache: cache}
	}
	evidence := func(id, status, base, candidate string) model.Evidence {
		return model.Evidence{ID: id, Kind: model.EvidenceDifferentialTest, Status: status, BaseCheckID: base, CheckID: candidate, Runner: "go_test_json", TestNames: []string{"TestRegression"}}
	}
	return &model.Report{
		Change: model.Change{Files: []model.ChangedFile{{Path: "value.go", Status: "M"}}},
		Checks: []model.Check{
			check("check-1", model.CheckGeneratedBase, "PASS", 0, goPass, cacheRecord(model.CacheHit, 2)),
			check("check-2", model.CheckGeneratedCandidate, "PASS", 0, goPass, nil),
			check("check-3", model.CheckGeneratedBase, "PASS", 0, goPass, nil),
			check("check-4", model.CheckGeneratedCandidate, "PASS", 0, goPass, nil),
			check("check-5", model.CheckGeneratedBase, "PASS", 0, goPass, cacheRecord(model.CacheHit, 3)),
			check("check-6", model.CheckGeneratedCandidate, "FAIL", 1, goFail, nil),
			check("check-7", model.CheckGeneratedBase, "PASS", 0, goPass, cacheRecord(model.CacheStored, 4)),
		},
		Evidence: []model.Evidence{
			evidence("evidence-3", model.StatusReproduced, "check-7", "check-6"),
			evidence("evidence-1", model.StatusNotReproduced, "check-1", "check-2"),
			evidence("evidence-2", model.StatusNotReproduced, "check-3", "check-4"),
		},
		Hypotheses: []model.Hypothesis{
			{ID: "h1", Title: "not reproduced (replayed)", Severity: "high", Status: model.StatusNotReproduced, EvidenceIDs: []string{"evidence-1"}},
			{ID: "h2", Title: "reproduced", Severity: "high", Status: model.StatusReproduced, EvidenceIDs: []string{"evidence-3"}},
		},
		Execution: &model.Execution{
			Cache:       model.ExecutionCache{Status: model.CacheEnabled, Scope: model.CacheScopeBaseline, ImageID: "sha256:" + strings.Repeat("a", 64), Hits: 2, Stored: 1, Note: model.ExecutionCacheNote},
			Parallelism: model.ExecutionParallelism{Requested: 1, Effective: 1, Note: "Initial checks run one at a time."},
			Budget:      model.ExecutionBudget{MaxRuntimeMS: 600000, SpentMS: 1234},
		},
	}
}

func TestFinalizeListsReplayBackedNegativesOnly(t *testing.T) {
	r := replayReport()
	Finalize(r, true)
	if got := r.Execution.ReplayBacked; !reflect.DeepEqual(got, []string{"evidence-1"}) {
		t.Fatalf("replay_backed %v", got)
	}
	if r.Hypotheses[0].Status != model.StatusNotReproduced || r.Hypotheses[1].Status != model.StatusReproduced || r.ExitCode != 1 {
		t.Fatalf("statuses %s %s exit %d", r.Hypotheses[0].Status, r.Hypotheses[1].Status, r.ExitCode)
	}
	// A replay-backed negative does not by itself request review (R3, §6 Q1).
	r = replayReport()
	r.Hypotheses = r.Hypotheses[:1]
	r.Checks = r.Checks[:4]
	r.Evidence = r.Evidence[1:]
	Finalize(r, true)
	if r.ExitCode != 0 || !reflect.DeepEqual(r.Execution.ReplayBacked, []string{"evidence-1"}) {
		t.Fatalf("exit %d replay_backed %v", r.ExitCode, r.Execution.ReplayBacked)
	}
	// One agreeing live run is not enough: nothing is verified, nothing listed,
	// and the hypothesis falls back to UNVERIFIED.
	r = replayReport()
	r.Checks[0].Cache.LiveRuns = 1
	Finalize(r, true)
	if len(r.Execution.ReplayBacked) != 0 || r.Hypotheses[0].Status != model.StatusUnverified {
		t.Fatalf("one live run: replay_backed %v status %s", r.Execution.ReplayBacked, r.Hypotheses[0].Status)
	}
}

// The list is recomputed from the ledger: a forged entry disappears, and
// Finalize and a persisted round trip are idempotent.
func TestFinalizeExecutionIsIdempotentAndIgnoresForgedLists(t *testing.T) {
	r := replayReport()
	r.Execution.ReplayBacked = []string{"evidence-3", "evidence-9"}
	r.Execution.Cache.Note = "All baseline results were verified."
	r.Execution.Cache.Scope = "everything"
	Finalize(r, true)
	if !reflect.DeepEqual(r.Execution.ReplayBacked, []string{"evidence-1"}) || r.Execution.Cache.Note != model.ExecutionCacheNote || r.Execution.Cache.Scope != model.CacheScopeBaseline {
		t.Fatalf("forged execution fields survived: %+v", r.Execution)
	}
	first, _ := json.Marshal(r)
	Finalize(r, true)
	second, _ := json.Marshal(r)
	if string(first) != string(second) {
		t.Fatal("Finalize is not idempotent with an execution object")
	}
	dir := t.TempDir()
	if err := Write(dir, r, []string{FormatJSON}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reread model.Report
	if err := json.Unmarshal(data, &reread); err != nil {
		t.Fatal(err)
	}
	Finalize(&reread, true)
	if !reflect.DeepEqual(reread.Execution, r.Execution) {
		t.Fatalf("round trip changed the execution object:\n%+v\n%+v", reread.Execution, r.Execution)
	}
}

func TestFinalizeExecutionNormalizesAndStaysAbsent(t *testing.T) {
	r := &model.Report{}
	Finalize(r, false)
	if r.Execution != nil {
		t.Fatal("Finalize created an execution object")
	}
	r = &model.Report{Execution: &model.Execution{Cache: model.ExecutionCache{Status: "ENABLED"}}}
	Finalize(r, false)
	e := r.Execution
	if e.Cache.Status != model.CacheDisabled || e.Cache.Scope != model.CacheScopeBaseline || e.Parallelism.Requested != 1 || e.Parallelism.Effective != 1 || e.Parallelism.Note == "" || e.ReplayBacked == nil {
		t.Fatalf("normalized %+v", e)
	}
}

func TestCacheNotes(t *testing.T) {
	if cacheNote(model.Check{}) != "" {
		t.Fatal("a live check without cache provenance got a note")
	}
	hit := cacheNote(model.Check{Cache: cacheRecord(model.CacheHit, 2)})
	for _, want := range []string{"Replayed from the execution cache", "not executed in this run", "check-1", "run-1", "2026-09-01T12:00:00Z", "1200 ms", "2 agreeing live runs"} {
		if !strings.Contains(hit, want) {
			t.Errorf("hit note %q lacks %q", hit, want)
		}
	}
	stored := cacheNote(model.Check{Cache: cacheRecord(model.CacheStored, 3)})
	if !strings.Contains(stored, "Executed live in this run") || !strings.Contains(stored, "3 agreeing live runs") {
		t.Errorf("stored note %q", stored)
	}
	if cacheNote(model.Check{Cache: &model.CheckCache{Status: "HIT"}}) != "" {
		t.Error("an unknown cache status got a note")
	}
}

// v0.4 wording rules (§5): the cache texts never present a replay as a fresh
// execution, a re-verification or a re-test, and make no reliability or
// speed claim.
func TestExecutionWordingMakesNoForbiddenClaim(t *testing.T) {
	r := replayReport()
	Finalize(r, true)
	var texts []string
	texts = append(texts, model.ExecutionCacheNote, cacheNote(r.Checks[0]), cacheNote(r.Checks[6]))
	markdown := string(Markdown(r))
	start := strings.Index(markdown, "## Automated Checks")
	end := strings.Index(markdown, "## Investigation Summary")
	texts = append(texts, markdown[start:end])
	forbidden := regexp.MustCompile(`(?i)\b(verified|re-verified|tested|re-tested|safe|correct|approved|regression|bug|faster|speed-?up|reliable)\b|%`)
	for _, text := range texts {
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("forbidden wording %q in %q", m, text)
		}
	}
}

func TestMarkdownRendersCacheProvenanceAndSummary(t *testing.T) {
	r := replayReport()
	r.Execution.Budget.DeadlineReached = true
	Finalize(r, true)
	md := string(Markdown(r))
	for _, want := range []string{
		"- **PASS** generated\\_test\\_base (check-1; exit 0; 0 ms)\n  Replayed from the execution cache, not executed in this run: the recorded result of check check-1 of run run-1",
		`  Executed live in this run and recorded in the execution cache \(4 agreeing live runs of this key\).`,
		"Execution cache: enabled for baseline-side runs only (image sha256:" + strings.Repeat("a", 64) + "). 2 replayed, 1 recorded, 0 misses, 0 uncacheable, 0 rejected, 0 write failures, 0 evicted, 0 contradicted.",
		"A replay never supports a reproduced issue, a divergence or a FAILS\\_ON\\_CANDIDATE result",
		"Initial checks: up to 1 at a time (requested 1). Initial checks run one at a time.",
		"Sandbox runtime charged: 1234 of 600000 ms (reviewer reserve 0 ms). The overall --deadline was reached.",
		"Negative conclusions resting on a replayed baseline (recorded by two agreeing live runs; not executed in this run): evidence-1.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	checks := strings.Index(md, "## Automated Checks")
	summary := strings.Index(md, "Execution cache: enabled")
	investigation := strings.Index(md, "## Investigation Summary")
	if !(checks < summary && summary < investigation) {
		t.Fatal("the execution summary is not at the end of Automated Checks")
	}
	// A disabled cache states its (escaped) reason and adds no replay note.
	r = &model.Report{Execution: &model.Execution{Cache: model.ExecutionCache{Status: model.CacheDisabled, Reason: "the sandbox image could not be pinned: <b>*x*</b> [link](http://x)"}}}
	Finalize(r, false)
	md = string(Markdown(r))
	if !strings.Contains(md, "Execution cache: disabled (the sandbox image could not be pinned: &lt;b&gt;\\*x\\*&lt;/b&gt; \\[link\\]\\(http://x\\)).") {
		t.Fatalf("disabled summary not escaped:\n%s", md)
	}
	if strings.Contains(md, "A replayed check") || strings.Contains(md, "Negative conclusions resting") {
		t.Fatal("a disabled cache rendered replay texts")
	}
	// Without an execution object nothing is rendered.
	if md := string(Markdown(&model.Report{})); strings.Contains(md, "Execution cache") || strings.Contains(md, "Sandbox runtime charged") {
		t.Fatal("an absent execution object was rendered")
	}
}
