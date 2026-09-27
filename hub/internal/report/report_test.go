package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// sample is a minimal but schema-shaped report exercising every alert source.
const sample = `{
  "version": 1,
  "tool_version": "v0.3.1",
  "generated_at": "2026-09-25T10:00:00Z",
  "change": {
    "base_ref": "main", "head_ref": "HEAD",
    "base_commit": "aaaa", "head_commit": "bbbb",
    "additions": 3, "deletions": 1,
    "files": [{
      "path": "pay/refund.go", "status": "M", "binary": false, "additions": 3, "deletions": 1,
      "hunks": [{"old_start": 10, "old_lines": 2, "new_start": 10, "new_lines": 4, "lines": [
        {"kind": "context", "old_line": 10, "new_line": 10, "content": "func Refund() {"},
        {"kind": "delete", "old_line": 11, "content": "  check(amount)"},
        {"kind": "add", "new_line": 11, "content": "  // check removed"},
        {"kind": "add", "new_line": 12, "content": "  pay(amount)"}
      ]}]
    }]
  },
  "linter": [
    {"id": "s1", "kind": "removed_validation", "path": "pay/refund.go", "line": 11, "end_line": 12,
     "side": "new", "severity": "high", "summary": "validation removed", "evidence": "the guard disappeared"},
    {"id": "s2", "kind": "style", "path": "pay/refund.go", "line": 12, "severity": "low",
     "summary": "comment only", "evidence": "noise"}
  ],
  "checks": [
    {"id": "c1", "kind": "test", "status": "PASS", "exit_code": 0, "duration_ms": 10, "output": "ok", "truncated": false},
    {"id": "c2", "kind": "typecheck", "status": "FAIL", "exit_code": 1, "duration_ms": 12, "output": "vet failed", "truncated": false}
  ],
  "hypotheses": [
    {"id": "h1", "title": "refund accepts a negative amount", "severity": "critical", "status": "REPRODUCED",
     "rationale": "differential test", "evidence_ids": ["e1"], "path": "pay/refund.go", "line": 11},
    {"id": "h2", "title": "maybe unbounded loop", "severity": "medium", "status": "UNVERIFIED",
     "rationale": "not investigated", "evidence_ids": []}
  ],
  "evidence": [
    {"id": "e1", "kind": "differential_test", "description": "fails on candidate", "status": "REPRODUCED",
     "check_id": "c3", "base_check_id": "c4", "runner": "go_test_json", "test_names": ["TestRefundNegative"]}
  ],
  "reproduced_issues": [
    {"id": "h1", "title": "refund accepts a negative amount", "severity": "critical", "status": "REPRODUCED",
     "rationale": "differential test", "evidence_ids": ["e1"]}
  ],
  "unverified": ["the coverage command was not configured"],
  "review_targets": [
    {"path": "pay/refund.go", "start_line": 11, "end_line": 12, "side": "new", "severity": "high",
     "reasons": ["removed_validation"], "signal_ids": ["s1"]}
  ],
  "review_surface": {"changed_lines": 4, "focused_lines": 2, "note": "prioritization, not coverage"},
  "coverage": {"status": "not_configured", "added_lines": 0, "executed_lines": 0, "not_executed_lines": 0,
   "no_block_lines": 0, "not_measured_lines": 0, "removed_lines": 0, "files": [], "note": "nothing measured"},
  "exit_code": 1
}`

func decodeSample(t *testing.T) *Report {
	t.Helper()
	r, err := Decode([]byte(sample))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return r
}

func TestDecodeRejectsUnknownVersion(t *testing.T) {
	if _, err := Decode([]byte(`{"version": 2}`)); err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
}

func TestRankAndNormalize(t *testing.T) {
	cases := map[string]struct {
		rank int
		name string
	}{
		"critical": {3, "critical"},
		"HIGH":     {2, "high"},
		" medium ": {1, "medium"},
		"low":      {0, "low"},
		"":         {0, "low"},
		"nonsense": {0, "low"},
	}
	for input, want := range cases {
		if got := Rank(input); got != want.rank {
			t.Errorf("Rank(%q) = %d, want %d", input, got, want.rank)
		}
		if got := Normalize(input); got != want.name {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want.name)
		}
	}
}

func TestAlertsRankReproducedIssuesFirst(t *testing.T) {
	alerts := decodeSample(t).Alerts()
	// 2 hypotheses, 1 failed check, 2 signals and 1 target, minus the target
	// and the low signal whose lines the critical issue and the high signal
	// already report.
	if len(alerts) != 4 {
		t.Fatalf("got %d alerts, want 4", len(alerts))
	}
	if alerts[0].ID != "issue:h1" {
		t.Errorf("first alert is %q, want the reproduced critical issue", alerts[0].ID)
	}
	if alerts[0].Severity != SeverityCritical || alerts[0].Status != "REPRODUCED" {
		t.Errorf("first alert = %+v", alerts[0])
	}
	if len(alerts[0].Evidence) != 1 || alerts[0].Evidence[0].ID != "e1" {
		t.Errorf("the reproduced issue must carry its evidence, got %+v", alerts[0].Evidence)
	}
	// The passing check must never appear; only the failing one does.
	for _, a := range alerts {
		if a.ID == "check:c1" {
			t.Error("a passing check must not become an alert")
		}
	}
	// Severity is the primary order.
	for i := 1; i < len(alerts); i++ {
		if Rank(alerts[i-1].Severity) < Rank(alerts[i].Severity) {
			t.Fatalf("alerts are not ranked by severity: %q then %q", alerts[i-1].ID, alerts[i].ID)
		}
	}
}

func TestSummarizeCounts(t *testing.T) {
	s := decodeSample(t).Summarize()
	if s.Verdict != VerdictBlocked || s.ExitCode != 1 {
		t.Errorf("verdict = %q (exit %d), want blocked", s.Verdict, s.ExitCode)
	}
	if s.Counts.Critical != 1 {
		t.Errorf("critical = %d, want 1", s.Counts.Critical)
	}
	// One high signal and one failed check; the high review target and the
	// low signal cover lines already reported and are deduplicated.
	if s.Counts.High != 2 {
		t.Errorf("high = %d, want 2", s.Counts.High)
	}
	if s.Counts.Medium != 1 || s.Counts.Low != 0 {
		t.Errorf("medium = %d, low = %d, want 1 and 0", s.Counts.Medium, s.Counts.Low)
	}
	if s.Counts.Total != 4 {
		t.Errorf("total = %d, want 4", s.Counts.Total)
	}
	if s.ChecksPassed != 1 || s.ChecksFailed != 1 {
		t.Errorf("checks = %d passed / %d failed, want 1 and 1", s.ChecksPassed, s.ChecksFailed)
	}
	if s.Reproduced != 1 || s.Unverified != 1 {
		t.Errorf("reproduced = %d, unverified = %d, want 1 and 1", s.Reproduced, s.Unverified)
	}
	if s.FocusedLines != 2 || s.ChangedLines != 4 {
		t.Errorf("surface = %d/%d, want 2/4", s.FocusedLines, s.ChangedLines)
	}
}

func TestCountsAtLeastMatchesTheSeveritySlider(t *testing.T) {
	c := decodeSample(t).Summarize().Counts
	if got := c.AtLeast(SeverityCritical); got != 1 {
		t.Errorf("AtLeast(critical) = %d, want 1", got)
	}
	if got := c.AtLeast(SeverityHigh); got != 3 {
		t.Errorf("AtLeast(high) = %d, want 3", got)
	}
	if got := c.AtLeast(SeverityMedium); got != 4 {
		t.Errorf("AtLeast(medium) = %d, want 4", got)
	}
	if got := c.AtLeast(SeverityLow); got != c.Total {
		t.Errorf("AtLeast(low) = %d, want every alert (%d)", got, c.Total)
	}
}

func TestVerdictMapsExitCodes(t *testing.T) {
	for code, want := range map[int]string{0: VerdictClear, 1: VerdictBlocked, 2: VerdictReview, 3: VerdictFailed, 4: VerdictFailed} {
		if got := Verdict(code); got != want {
			t.Errorf("Verdict(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestBuildViewCarriesTheConcernedModifications(t *testing.T) {
	v := decodeSample(t).BuildView()
	if len(v.Files) != 1 || v.Files[0].Path != "pay/refund.go" {
		t.Fatalf("view files = %+v", v.Files)
	}
	if len(v.Files[0].Hunks) != 1 || len(v.Files[0].Hunks[0].Lines) != 4 {
		t.Fatalf("the diff of the concerned file must be attached, got %+v", v.Files[0].Hunks)
	}
	if v.DiffTruncated {
		t.Error("a four-line diff must not be reported as truncated")
	}
	if len(v.SeverityLevels) != 4 || v.SeverityLevels[3] != SeverityCritical {
		t.Errorf("the UI needs the ordered severity levels, got %v", v.SeverityLevels)
	}
	// The payload must stay serializable for the browser.
	if _, err := json.Marshal(v); err != nil {
		t.Fatalf("marshal view: %v", err)
	}
}

func TestBuildViewTruncatesAnEnormousDiff(t *testing.T) {
	r := decodeSample(t)
	huge := make([]DiffLine, maxDiffLines+1)
	for i := range huge {
		huge[i] = DiffLine{Kind: "add", NewLine: i + 1, Content: "x"}
	}
	r.Change.Files = append(r.Change.Files, ChangedFile{
		Path: "generated/big.go", Status: "A",
		Hunks: []Hunk{{NewStart: 1, NewLines: len(huge), Lines: huge}},
	})
	v := r.BuildView()
	if !v.DiffTruncated {
		t.Fatal("an oversized diff must be reported as truncated")
	}
	for _, f := range v.Files {
		if f.Path == "generated/big.go" && len(f.Hunks) != 0 {
			t.Error("the oversized hunk must be dropped, not sent to the browser")
		}
	}
}

func TestAlertsToleratesAMissingEvidenceReference(t *testing.T) {
	r := decodeSample(t)
	r.Hypotheses[0].EvidenceIDs = []string{"does-not-exist"}
	alerts := r.Alerts()
	if len(alerts[0].Evidence) != 0 {
		t.Errorf("a dangling evidence id must be skipped, got %+v", alerts[0].Evidence)
	}
}

func TestAlertsKeepOnlyTheMostSevereAlertPerLine(t *testing.T) {
	r := decodeSample(t)
	alerts := r.Alerts()
	seen := map[string]string{}
	for _, a := range alerts {
		if a.Path == "" {
			continue
		}
		for line := a.Line; line <= a.EndLine; line++ {
			key := fmt.Sprintf("%s %s:%d", a.Side, a.Path, line)
			if prev, ok := seen[key]; ok {
				t.Errorf("line %s reported by %s and %s", key, prev, a.ID)
			}
			seen[key] = a.ID
		}
	}
	for _, a := range alerts {
		switch a.ID {
		case "focus:0", "signal:s2":
			t.Errorf("%s only covers lines already reported by more severe alerts", a.ID)
		case "signal:s1":
			if a.Line != 12 || a.EndLine != 12 {
				t.Errorf("signal:s1 keeps %d-%d, want only its unreported line 12", a.Line, a.EndLine)
			}
		}
	}
	// The verdict never depends on the deduplicated list.
	if s := r.Summarize(); s.Verdict != VerdictBlocked || s.Reproduced != 1 {
		t.Errorf("summary = %+v", s)
	}
}

func TestDedupeLinesSplitsAPartlyReportedRange(t *testing.T) {
	alerts := dedupeLines([]Alert{
		{ID: "issue:a", Kind: KindIssue, Severity: SeverityHigh, Path: "a.go", Line: 5, EndLine: 5, Side: "new"},
		{ID: "focus:0", Kind: KindFocus, Severity: SeverityMedium, Title: "a.go:1-9", Path: "a.go", Line: 1, EndLine: 9, Side: "new"},
		{ID: "focus:1", Kind: KindFocus, Severity: SeverityLow, Path: "a.go", Line: 5, EndLine: 5, Side: "old"},
		{ID: "check:x", Kind: KindCheck, Severity: SeverityLow},
	})
	var got []string
	for _, a := range alerts {
		got = append(got, fmt.Sprintf("%s %s %d-%d %s", a.ID, a.Side, a.Line, a.EndLine, a.Title))
	}
	want := []string{
		"issue:a new 5-5 ",
		"focus:0#1 new 1-4 a.go:1-4",
		"focus:0#2 new 6-9 a.go:6-9",
		"focus:1 old 5-5 a.go:5-5",
		"check:x  0-0 ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDedupeLinesNeverDropsAReproducedIssue(t *testing.T) {
	alerts := dedupeLines([]Alert{
		{ID: "issue:u", Kind: KindIssue, Severity: SeverityCritical, Status: "UNVERIFIED", Path: "a.go", Line: 3, EndLine: 3},
		{ID: "issue:r", Kind: KindIssue, Severity: SeverityHigh, Status: "REPRODUCED", Path: "a.go", Line: 3, EndLine: 3},
		{ID: "signal:s", Kind: KindSignal, Severity: SeverityLow, Path: "a.go", Line: 3, EndLine: 3},
	})
	if len(alerts) != 2 || alerts[1].ID != "issue:r" {
		t.Errorf("got %+v, want the unverified and the reproduced issue only", alerts)
	}
}

// fileLevel is a report whose file-level signals sit on the first changed line,
// as the CLI places them, next to a signal about that very line. The review
// target is the one an earlier CLI built by merging them all on that line.
const fileLevel = `{
  "version": 1,
  "change": {"files": [{"path": "hub/api.go", "status": "M", "additions": 2, "deletions": 0,
    "hunks": [{"old_start": 1, "old_lines": 1, "new_start": 1, "new_lines": 3, "lines": [
      {"kind": "context", "old_line": 1, "new_line": 1, "content": "package hub"},
      {"kind": "add", "new_line": 2, "content": "if err != nil {"},
      {"kind": "add", "new_line": 3, "content": "}"}
    ]}]}]},
  "linter": [
    {"id": "path", "kind": "sensitive_path", "path": "hub/api.go", "line": 2, "side": "new", "scope": "file",
     "severity": "high", "summary": "Configured sensitive path changed", "evidence": "Path matches configured pattern hub/**"},
    {"id": "tests", "kind": "no_test_change", "path": "hub/api.go", "line": 2, "side": "new",
     "severity": "low", "summary": "No nearby test file changed", "evidence": "written before the CLI recorded a scope"},
    {"id": "err", "kind": "error_handling_change", "path": "hub/api.go", "line": 2, "side": "new",
     "severity": "medium", "summary": "Error handling changed", "evidence": "if err != nil {"}
  ],
  "review_targets": [
    {"path": "hub/api.go", "start_line": 2, "end_line": 2, "side": "new", "severity": "high",
     "reasons": ["Configured sensitive path changed", "No nearby test file changed", "Error handling changed"],
     "signal_ids": ["path", "tests", "err"]}
  ],
  "exit_code": 2
}`

func TestFileLevelSignalsPointAtNoLine(t *testing.T) {
	r, err := Decode([]byte(fileLevel))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Alert{}
	for _, a := range r.Alerts() {
		got[a.ID] = a
	}
	for _, id := range []string{"signal:path", "signal:tests"} {
		a, ok := got[id]
		if !ok {
			t.Fatalf("%s dropped: a file-level signal holds no line another alert could report", id)
		}
		if a.Scope != ScopeFile || a.Line != 0 || a.EndLine != 0 || a.Side != "" || a.Path != "hub/api.go" {
			t.Errorf("%s = %+v, want the whole file and no line", id, a)
		}
	}
	// The line the sensitive path is anchored on keeps its own message.
	if a, ok := got["signal:err"]; !ok || a.Line != 2 || a.EndLine != 2 || a.Scope != "" {
		t.Errorf("signal:err = %+v, want line 2 under its own alert", a)
	}
	// Every reason of the merged target is shown by its own signal.
	if a, ok := got["focus:0"]; ok {
		t.Errorf("focus:0 = %+v repeats reasons that are not about its line", a)
	}
	if s := r.Summarize(); s.Counts.Total != 3 || s.Counts.High != 1 || s.Counts.Medium != 1 || s.Counts.Low != 1 {
		t.Errorf("counts = %+v, want one alert per signal", s.Counts)
	}
}

func TestReviewTargetsKeepOnlyTheirOwnReasons(t *testing.T) {
	r := &Report{
		Signals:    []Signal{{ID: "s", Kind: "auth_change", Path: "a.go", Line: 4, Side: "new", Severity: "high", Summary: "Authentication or authorization logic changed"}},
		Hypotheses: []Hypothesis{{ID: "h", Title: "guest reaches the admin page", Severity: "high", Status: "UNVERIFIED", Path: "a.go", Line: 5}},
		ReviewTargets: []ReviewTarget{
			{Path: "a.go", StartLine: 4, EndLine: 9, Side: "new", Severity: "high",
				Reasons:   []string{"Authentication or authorization logic changed", "guest reaches the admin page", "Changed baseline test TestAdmin fails on candidate code"},
				SignalIDs: []string{"s"}},
			{Path: "a.go", Side: "new", Severity: "medium", Reasons: []string{"Dependency-preparation input changed"}},
		},
	}
	var focus []Alert
	for _, a := range r.Alerts() {
		if a.Kind == KindFocus {
			focus = append(focus, a)
		}
	}
	if len(focus) != 2 {
		t.Fatalf("focus alerts = %+v, want the range and the whole file", focus)
	}
	if f := focus[0]; strings.Join(f.Reasons, "|") != "Changed baseline test TestAdmin fails on candidate code" || f.Line != 6 || f.EndLine != 9 || f.Title != "a.go:6-9" {
		t.Errorf("range = %+v, want its own reason on the lines no other alert reports", f)
	}
	if f := focus[1]; f.Scope != ScopeFile || f.Line != 0 || f.Title != "a.go" {
		t.Errorf("whole-file target = %+v", f)
	}
}
