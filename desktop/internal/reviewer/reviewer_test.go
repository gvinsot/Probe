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
	res := parseAnswer(raw, 0)
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

func TestParseAnswerRaisesSeverityOnLegalAndFinancialImpact(t *testing.T) {
	for _, c := range []struct {
		raw     string
		impacts string
		sev     string
	}{
		{`{"explanation":"Le taux de pénalité passe de 1 % à 10 %. Cette modification peut avoir une incidence juridique et financière."}`, "legal,financial", office.Critical},
		{`Cette modification peut avoir une incidence juridique et financière.`, "legal,financial", office.Critical},
		{`{"explanation":"The liability cap was removed; this has legal and financial implications."}`, "legal,financial", office.Critical},
		{`{"explanation":"The payment term changed.","impacts":["Financial","legal"]}`, "legal,financial", office.Critical},
		{`{"explanation":"The total changed: check the financial impact.","impacts":[]}`, "financial", office.High},
		{`{"explanation":"A title was reworded.","impacts":["cosmetic"]}`, "", ""},
		{`{"explanation":"Cette modification n'a aucune incidence juridique ni financière."}`, "", ""},
		{`{"explanation":"The wording changed but has no legal or financial impact."}`, "", ""},
		{`{"explanation":"Check the legal meaning and the amounts."}`, "", ""},
		{`{"explanation":"The rate changed.","findings":[{"severity":"medium","title":"Incidence juridique et financière possible sur la clause 4"}]}`, "legal,financial", office.Critical},
	} {
		res := parseAnswer(c.raw, 0)
		if got := strings.Join(res.Impacts, ","); got != c.impacts || res.Severity != c.sev {
			t.Errorf("%s\n  impacts %q severity %q, want %q %q", c.raw, got, res.Severity, c.impacts, c.sev)
		}
	}
}

func TestParseAnswerFallsBackToPlainText(t *testing.T) {
	for _, raw := range []string{"Just check C1.", `{"findings":[{"title":"x"}]}`, "{broken"} {
		res := parseAnswer(raw, 0)
		if res.Text != raw || len(res.Findings) != 0 {
			t.Errorf("%q -> %+v", raw, res)
		}
	}
}

func TestReadingsQualifyRuleFindings(t *testing.T) {
	raw := `{"explanation": "Le bailleur a changé.", "impacts": [],
		"readings": [
			{"finding": 1, "title": "Nom du bailleur remplacé au paragraphe 3 (Dupont → Martin)", "consistency": "Inconsistent", "note": "Le paragraphe 1 désigne toujours M. Dupont comme bailleur."},
			{"finding": 1, "title": "doublon"},
			{"finding": 7, "title": "hors limites"},
			{"finding": 0, "title": "hors limites"}],
		"findings": [{"severity": "medium", "title": "Clé remise par une autre personne", "location": "Paragraphe 3", "consistency": "maybe", "note": "À vérifier."}]}`
	res := parseAnswer(raw, 2)
	if len(res.Readings) != 1 {
		t.Fatalf("readings = %+v", res.Readings)
	}
	r := res.Readings[0]
	if r.Finding != 0 || r.Consistency != office.Inconsistent || !strings.Contains(r.Title, "bailleur") || r.Note == "" {
		t.Fatalf("reading = %+v", r)
	}
	if len(res.Findings) != 1 || res.Findings[0].Consistency != "" || res.Findings[0].Note != "À vérifier." {
		t.Fatalf("AI finding = %+v", res.Findings)
	}
}

func TestPromptCarriesOtherPassages(t *testing.T) {
	r := sampleReport()
	r.Mentions = []office.Mention{{
		Term: "Jean Dupont", Replacement: "Paul Martin", Location: "Paragraph 3", Count: 1,
		Elsewhere: []office.Occurrence{{Location: "Paragraph 1", Excerpt: "Entre M. Jean Dupont, ci-après « le Bailleur »"}},
	}}
	p := userPrompt("bail.docx", r)
	for _, want := range []string{"F1 [high]", `"Jean Dupont" replaced by "Paul Martin" at Paragraph 3`, "Paragraph 1: Entre M. Jean Dupont, ci-après « le Bailleur »"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q:\n%s", want, p)
		}
	}
	for _, lang := range []string{"en", "fr"} {
		if sp := systemPrompt(lang); !strings.Contains(sp, `"readings"`) || !strings.Contains(sp, "consistency") {
			t.Errorf("%s system prompt does not ask for readings", lang)
		}
	}
	if !strings.Contains(systemPrompt("fr"), "Nom du bailleur") {
		t.Error("French prompt lacks its example")
	}
}
