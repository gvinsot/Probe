package report

import (
	"encoding/json"
	"reflect"
	"testing"
)

func cacheBustReport() *Report {
	r := &Report{Version: 1, ExitCode: 2}
	for _, filename := range []string{"devops/docker-compose.swarm.yml", "docker-compose.yml", "runner-service/Dockerfile"} {
		old, next := "        OPENCODE_CLI_CACHE_BUST: ${OPENCODE_CLI_CACHE_BUST:-1}", "        OPENCODE_CLI_CACHE_BUST: ${OPENCODE_CLI_CACHE_BUST:-2026-09-28}"
		if filename == "runner-service/Dockerfile" {
			old, next = "ARG OPENCODE_CLI_CACHE_BUST=1", "ARG OPENCODE_CLI_CACHE_BUST=2026-09-28"
			r.Signals = append(r.Signals, Signal{ID: "infra", Kind: "infrastructure_change", Path: filename, Severity: "high", Summary: "Deployment configuration changed", Evidence: "Inspect permissions and deployment effects"})
		}
		r.Change.Files = append(r.Change.Files, ChangedFile{Path: filename, Status: "M", Additions: 1, Deletions: 1,
			Hunks: []Hunk{{Lines: []DiffLine{{Kind: "delete", Content: old}, {Kind: "add", Content: next}}}}})
		r.Signals = append(r.Signals, Signal{ID: filename, Kind: "sensitive_path", Path: filename, Line: 63, Scope: ScopeFile,
			Severity: "high", Summary: "Sensitive file changed", Evidence: "Configured pattern " + filename})
	}
	return r
}

func TestGroupCacheBustChangesPreservesAllSignals(t *testing.T) {
	r := cacheBustReport()
	before, _ := json.Marshal(r)
	originals := r.allAlerts()
	v := r.BuildView()
	if len(v.Alerts) != 1 || len(v.Alerts[0].Members) != 4 {
		t.Fatalf("want one group of four signals, got %+v", v.Alerts)
	}
	g := v.Alerts[0]
	if g.Title != "Configuration value changed: OPENCODE_CLI_CACHE_BUST" || g.Path != "" || g.Line != 0 {
		t.Fatalf("incorrect group location/title: %+v", g)
	}
	if !reflect.DeepEqual(g.Members, originals) {
		t.Fatalf("group lost original signals: %+v", g.Members)
	}
	if v.Summary.Counts.Total != 1 || v.Summary.Counts.High != 1 || v.Summary.Verdict != VerdictReview || len(v.Files) != 3 {
		t.Fatalf("view counts/verdict/files: %+v", v)
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("grouping modified the raw report")
	}
	if !reflect.DeepEqual(r.Alerts(), v.Alerts) {
		t.Fatal("group IDs/order are not stable")
	}
}

func TestFileGroupsDoNotConflateOtherConcerns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Report)
	}{
		{"different replacement", func(r *Report) {
			r.Change.Files[0].Hunks[0].Lines[1].Content = "OPENCODE_CLI_CACHE_BUST: ${OPENCODE_CLI_CACHE_BUST:-2}"
		}},
		{"different original", func(r *Report) {
			r.Change.Files[0].Hunks[0].Lines[0].Content = "OPENCODE_CLI_CACHE_BUST: ${OPENCODE_CLI_CACHE_BUST:-0}"
		}},
		{"additional edit", func(r *Report) {
			r.Change.Files[0].Additions++
			r.Change.Files[0].Hunks[0].Lines = append(r.Change.Files[0].Hunks[0].Lines, DiffLine{Kind: "add", Content: "OTHER: 2"})
		}},
		{"incomplete diff", func(r *Report) { r.Change.Files[0].Additions++ }},
		{"binary", func(r *Report) { r.Change.Files[0].Binary = true }},
		{"rename", func(r *Report) { r.Change.Files[0].OldPath = "old.yml" }},
		{"different severity", func(r *Report) { r.Signals[0].Severity = "critical" }},
		{"other concern", func(r *Report) { r.Signals[0].Kind = "no_test_change" }},
		{"line signal", func(r *Report) { r.Signals[0].Scope = ""; r.Signals[0].Kind = "auth_change" }},
		{"different AI judgment", func(r *Report) {
			r.SignalAssessments = []SignalAssessment{{SignalID: r.Signals[0].ID, Title: "Risk", Judgment: JudgmentRisk}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cacheBustReport()
			tc.change(r)
			if got := r.Alerts(); len(got) != 2 {
				t.Fatalf("want independent alert and group, got %+v", got)
			}
		})
	}
}

func TestFileGroupsKeepDismissedSignalsSeparate(t *testing.T) {
	r := cacheBustReport()
	r.SignalAssessments = []SignalAssessment{{SignalID: r.Signals[0].ID, Title: "Harmless", Judgment: JudgmentNoRisk, SetAside: true}}
	if a, d := r.Alerts(), r.Dismissed(); len(a) != 1 || len(a[0].Members) != 3 || len(d) != 1 || len(d[0].Members) != 0 {
		t.Fatalf("active = %+v, dismissed = %+v", a, d)
	}
}

func TestOverlappingFileSignalsWithoutDiff(t *testing.T) {
	r := cacheBustReport()
	r.Change.Files = nil
	alerts := r.Alerts()
	if len(alerts) != 3 || len(alerts[2].Members) != 2 || alerts[2].Path != "runner-service/Dockerfile" {
		t.Fatalf("want same-file overlap grouped even without a diff: %+v", alerts)
	}
}

func TestFileChangeKeyUsesWholeEdit(t *testing.T) {
	a := ChangedFile{Path: "a.txt", Status: "M", Additions: 1, Deletions: 1,
		Hunks: []Hunk{{Lines: []DiffLine{{Kind: "delete", Content: "old"}, {Kind: "add", Content: "new"}}}}}
	b := a
	b.Path = "b.txt"
	b.Hunks = []Hunk{{NewStart: 100, Lines: []DiffLine{{Kind: "context", Content: "different context"}, {Kind: "delete", Content: "old"}, {Kind: "add", Content: "new"}}}}
	ka, _ := fileChangeKey(a)
	kb, _ := fileChangeKey(b)
	if ka == "" || ka != kb {
		t.Fatal("identical edits should match despite context and location")
	}
	b.Hunks[0].Lines[2].Content = " new"
	kb, _ = fileChangeKey(b)
	if ka == kb {
		t.Fatal("whitespace in arbitrary source must not be normalized")
	}
}

func TestConfigAssignmentRejectsAmbiguousValues(t *testing.T) {
	for _, tc := range []struct{ path, line string }{
		{"docker-compose.yml", "KEY: ${OTHER:-1}"},
		{"docker-compose.yml", "KEY: ${KEY-1}"},
		{"docker-compose.yml", "KEY: ${KEY:-$(command)}"},
		{"docker-compose.yml", "KEY: ${KEY:-1} # comment"},
		{"Dockerfile", "ARG KEY=1 OTHER=2"},
		{"Dockerfile", "ARG KEY=\"1\""},
		{"source.go", "ARG KEY=1"},
	} {
		if key, _ := configAssignment(tc.path, tc.line); key != "" {
			t.Errorf("unexpected normalization: %+v", tc)
		}
	}
}
