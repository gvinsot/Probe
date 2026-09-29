package reviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
)

func sampleReport() *office.Report {
	return &office.Report{
		Kind: office.Excel, Severity: office.High, ChangeCount: 1,
		Findings: []office.Finding{{Severity: office.High, Rule: "excel.formula-hardcoded", Title: "A formula was replaced by its value", Location: "Budget!C1", Before: "=A1*2", After: "200"}},
		Changes:  []office.Change{{Kind: "modified", Location: "Budget!C1", Before: "=A1*2", After: "200"}},
	}
}

func TestPromptCarriesFindingsNotPaths(t *testing.T) {
	p := userPrompt(`C:\Users\x\OneDrive\Budget 2026.xlsx`, sampleReport())
	for _, want := range []string{"Budget 2026.xlsx", "[high] A formula was replaced by its value at Budget!C1", "before: =A1*2"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, `C:\Users`) {
		t.Error("prompt leaks the local path")
	}
}

func TestOpenAICompatibleEndpoint(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"content":"  Check C1.  "}}]}`))
	}))
	defer srv.Close()
	s := config.Settings{Provider: config.ProviderOpenAI, Model: "local-model", BaseURL: srv.URL + "/v1", Language: "fr"}
	res, err := Explain(context.Background(), s, "k", "b.xlsx", sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Check C1." || len(res.Findings) != 0 || got["model"] != "local-model" {
		t.Fatalf("result %+v, request %v", res, got)
	}
}

func TestNotConfigured(t *testing.T) {
	if _, err := Explain(context.Background(), config.Defaults(), "", "a.docx", sampleReport()); err != ErrNotConfigured {
		t.Fatalf("err = %v", err)
	}
}

func TestParseAnswerRaisesBoundedAIFindings(t *testing.T) {
	raw := "```json\n" + `{"explanation":"Total looks off.","findings":[` +
		`{"severity":"HIGH","title":"Total no longer matches the lines","location":"Budget!C9","before":"120","after":"90"},` +
		`{"severity":"weird","title":"Odd label"},{"severity":"low","title":""}]}` + "\n```"
	res := parseAnswer(raw)
	if res.Text != "Total looks off." || len(res.Findings) != 2 {
		t.Fatalf("result %+v", res)
	}
	if f := res.Findings[0]; f.Severity != office.High || f.Rule != RuleAI || f.Location != "Budget!C9" {
		t.Errorf("finding %+v", f)
	}
	if res.Findings[1].Severity != office.Low {
		t.Errorf("unknown severity kept: %+v", res.Findings[1])
	}
}

func TestParseAnswerFallsBackToPlainText(t *testing.T) {
	for _, raw := range []string{"Just check C1.", `{"findings":[{"title":"x"}]}`, "{broken"} {
		res := parseAnswer(raw)
		if res.Text != raw || len(res.Findings) != 0 {
			t.Errorf("%q -> %+v", raw, res)
		}
	}
}
