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
	text, err := Explain(context.Background(), s, "k", "b.xlsx", sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Check C1." || got["model"] != "local-model" {
		t.Fatalf("text %q, request %v", text, got)
	}
}

func TestNotConfigured(t *testing.T) {
	if _, err := Explain(context.Background(), config.Defaults(), "", "a.docx", sampleReport()); err != ErrNotConfigured {
		t.Fatalf("err = %v", err)
	}
}
