package report

import "testing"

func TestViewCarriesThePRSummary(t *testing.T) {
	r, err := Decode([]byte(`{"version":1,"exit_code":2,"change":{"files":[]},"pr_summary":{"title":"Round discounts up","overview":"Discounts round up.","changes":[{"area":"Pricing","summary":"Rounding changed.","files":["calc.go"]}],"behavior_changes":[],"risks":["Unverified: off by one cent"],"review_focus":[],"model":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	v := r.BuildView()
	if v.PRSummary == nil || v.PRSummary.Title != "Round discounts up" || v.PRSummary.Changes[0].Files[0] != "calc.go" || v.Summary.ExitCode != 2 {
		t.Fatalf("view %+v", v.PRSummary)
	}
	plain, _ := Decode([]byte(`{"version":1,"change":{"files":[]}}`))
	if plain.BuildView().PRSummary != nil {
		t.Fatal("a summary appeared from nowhere")
	}
}

func TestAlertsCarryTheirIntent(t *testing.T) {
	r, err := Decode([]byte(`{"version":1,"exit_code":2,"change":{"files":[]},
"linter":[
 {"id":"s1","kind":"branch_growth","path":"sort.ts","line":1,"scope":"file","severity":"low","summary":"More branching constructs appear in the diff"},
 {"id":"s2","kind":"public_api","path":"sort.ts","line":12,"side":"new","severity":"low","summary":"Possible public declaration added"},
 {"id":"s3","kind":"suppression","path":"sort.test.ts","line":7,"side":"new","severity":"medium","summary":"Type or safety checking suppression added"},
 {"id":"s4","kind":"todo","path":"other.ts","line":3,"side":"new","severity":"low","summary":"TODO added"}],
"hypotheses":[{"id":"h1","title":"Sort is unstable","severity":"medium","status":"UNVERIFIED","path":"sort.ts","line":20}],
"pr_summary":{"title":"t","overview":"o","changes":[],"behavior_changes":[],"risks":[],"review_focus":[],"model":"m",
 "intents":[{"intent":"Add agent sorting","signal_ids":["s1","s2"],"hypothesis_ids":["h1"]},{"intent":"Test agent sorting","signal_ids":["s3","s1"],"hypothesis_ids":[]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range r.BuildView().Alerts {
		got[a.ID] = a.IntentGroup
	}
	want := map[string]string{"signal:s1": "Add agent sorting", "signal:s2": "Add agent sorting", "issue:h1": "Add agent sorting", "signal:s3": "Test agent sorting", "signal:s4": ""}
	for id, intent := range want {
		if got[id] != intent {
			t.Errorf("%s intent = %q, want %q (all: %v)", id, got[id], intent, got)
		}
	}
}
