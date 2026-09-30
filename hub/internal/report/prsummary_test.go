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
