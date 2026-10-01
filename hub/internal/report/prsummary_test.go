package report

import "testing"

func TestViewCarriesThePRSummary(t *testing.T) {
	r, err := Decode([]byte(`{"version":1,"exit_code":2,"change":{"files":[]},"pr_summary":{"title":"Round discounts up","overview":"Discounts round up.","changes":[{"area":"Pricing","summary":"Rounding changed.","refs":[{"path":"calc.go","start_line":3,"end_line":5}]}],"behavior_changes":[],"risks":[{"text":"Unverified: off by one cent","signal_ids":[],"hypothesis_ids":["h1"],"refs":[{"path":"calc.go","start_line":2,"side":"old"}]}],"review_focus":[],"testing":[],"model":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	v := r.BuildView()
	if v.PRSummary == nil || v.PRSummary.Title != "Round discounts up" || v.PRSummary.Changes[0].Refs[0] != (CodeRef{Path: "calc.go", StartLine: 3, EndLine: 5}) || v.Summary.ExitCode != 2 {
		t.Fatalf("view %+v", v.PRSummary)
	}
	if risk := v.PRSummary.Risks[0]; risk.HypothesisIDs[0] != "h1" || risk.Refs[0].Side != "old" {
		t.Fatalf("view %+v", v.PRSummary)
	}
	plain, _ := Decode([]byte(`{"version":1,"change":{"files":[]}}`))
	if plain.BuildView().PRSummary != nil {
		t.Fatal("a summary appeared from nowhere")
	}
}

func TestAlertsCarryTheirArea(t *testing.T) {
	r, err := Decode([]byte(`{"version":1,"exit_code":2,"change":{"files":[]},
"linter":[
 {"id":"s1","kind":"branch_growth","path":"sort.ts","line":1,"scope":"file","severity":"low","summary":"More branching constructs appear in the diff"},
 {"id":"s2","kind":"public_api","path":"sort.ts","line":12,"side":"new","severity":"low","summary":"Possible public declaration added"},
 {"id":"s3","kind":"suppression","path":"sort.test.ts","line":7,"side":"new","severity":"medium","summary":"Type or safety checking suppression added"},
 {"id":"s4","kind":"todo","path":"other.ts","line":3,"side":"new","severity":"low","summary":"TODO added"}],
"hypotheses":[{"id":"h1","title":"Sort is unstable","severity":"medium","status":"UNVERIFIED","path":"sort.ts","line":20}],
"pr_summary":{"title":"t","overview":"o","behavior_changes":[],"risks":[],"review_focus":[],"testing":[],"model":"m",
 "changes":[{"area":"Add agent sorting","summary":"s","refs":[],"signal_ids":["s1","s2"],"hypothesis_ids":["h1"]},{"area":"Test agent sorting","summary":"s","refs":[],"signal_ids":["s3","s1"],"hypothesis_ids":[]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, a := range r.BuildView().Alerts {
		got[a.ID] = a.Area
	}
	want := map[string]int{"signal:s1": 1, "signal:s2": 1, "issue:h1": 1, "signal:s3": 2, "signal:s4": 0}
	for id, area := range want {
		if got[id] != area {
			t.Errorf("%s area = %d, want %d (all: %v)", id, got[id], area, got)
		}
	}
}

func TestViewSaysWhyThereIsNoPRSummary(t *testing.T) {
	r, err := Decode([]byte(`{"version":1,"change":{"files":[]},"audit":[{"time":"2026-10-01T00:00:00Z","tool":"pr_summary_completion","arguments":"iteration=1","status":"OK","duration_ms":1},{"time":"2026-10-01T00:00:00Z","tool":"pr_summary","arguments":"the change is too large for the reviewer input budget","status":"ERROR","duration_ms":0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.BuildView().PRSummaryError; got != "the change is too large for the reviewer input budget" {
		t.Fatalf("pr_summary_error %q", got)
	}
	plain, _ := Decode([]byte(`{"version":1,"change":{"files":[]},"audit":[]}`))
	if plain.BuildView().PRSummaryError != "" {
		t.Fatal("an error appeared from nowhere")
	}
}
