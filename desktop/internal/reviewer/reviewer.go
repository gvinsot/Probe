// Package reviewer asks an AI model to review a report.
//
// The model reads the findings and the changed excerpts the deterministic
// comparison already produced, never the whole document. It answers with a
// plain-language explanation and may point out additional risky changes the
// rules missed. Those are kept apart as AI findings: they never remove or
// downgrade a rule finding and never change the verdict of the rules.
package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
)

// Request is what a provider receives.
type Request struct {
	Model   string
	APIKey  string
	BaseURL string
	System  string
	User    string
}

// ErrNotConfigured reports a missing provider or key.
var ErrNotConfigured = errors.New("no AI provider is configured: choose one and save its API key in the settings")

// Result is the review of a report by the model.
type Result struct {
	Text     string
	Findings []office.Finding
}

// maxAIFindings bounds what the model can add to a report.
const maxAIFindings = 10

// RuleAI is the rule identifier of the findings raised by the model.
const RuleAI = "ai.review"

// Explain returns the explanation of a report and the extra findings the
// model raised.
func Explain(ctx context.Context, s config.Settings, apiKey, path string, report *office.Report) (Result, error) {
	if s.Provider == config.ProviderNone || (apiKey == "" && s.BaseURL == "") {
		return Result{}, ErrNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req := Request{
		Model:   s.EffectiveModel(),
		APIKey:  apiKey,
		BaseURL: s.BaseURL,
		System:  systemPrompt(s.Language),
		User:    userPrompt(path, report),
	}
	var raw string
	var err error
	switch s.Provider {
	case config.ProviderAnthropic:
		raw, err = explainAnthropic(ctx, req)
	case config.ProviderOpenAI:
		raw, err = explainOpenAI(ctx, req)
	default:
		return Result{}, ErrNotConfigured
	}
	if err != nil {
		return Result{}, err
	}
	return parseAnswer(raw), nil
}

// parseAnswer reads the JSON answer asked by the prompt. A model that ignores
// the format (small local models) still gives a usable explanation: the raw
// text is then shown as is, without extra findings.
func parseAnswer(raw string) Result {
	raw = strings.TrimSpace(raw)
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return Result{Text: raw}
	}
	var a struct {
		Explanation string `json:"explanation"`
		Findings    []struct {
			Severity string `json:"severity"`
			Title    string `json:"title"`
			Location string `json:"location"`
			Before   string `json:"before"`
			After    string `json:"after"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &a); err != nil || strings.TrimSpace(a.Explanation) == "" {
		return Result{Text: raw}
	}
	res := Result{Text: strings.TrimSpace(a.Explanation)}
	for _, f := range a.Findings {
		title := strings.TrimSpace(f.Title)
		if title == "" || len(res.Findings) == maxAIFindings {
			continue
		}
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if sev != office.High && sev != office.Medium {
			sev = office.Low
		}
		res.Findings = append(res.Findings, office.Finding{
			Severity: sev, Rule: RuleAI, Title: clip(title, 200),
			Location: clip(f.Location, 200), Before: clip(f.Before, 600), After: clip(f.After, 600),
		})
	}
	return res
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func systemPrompt(language string) string {
	lang := "English"
	if language == "fr" {
		lang = "French"
	}
	return `You help a person review the latest modifications of an office document (Word, Excel or PowerPoint) before they accept them.

A deterministic comparison already listed the changes and flagged the risky ones with a severity. You receive those findings and excerpts of the changed content, not the full document.

Write a short explanation for a non-technical reader:
- First, two or three sentences on what changed overall.
- Then the points that deserve attention, most important first, each saying what to check and why it matters (financial impact, legal meaning, broken calculation, hidden content...).
- Stay factual: rely only on the excerpts given. When the excerpts are not enough to conclude, say what the reader should open and verify.
- Do not change the severities and do not declare the document safe; the person decides.

You may also raise additional findings: risky changes visible in the excerpts that the rules did not flag (a figure that no longer matches its context, a meaning reversed by rewording, a suspicious removal...). Only raise a finding you can point to in the excerpts, with its location; do not repeat a finding already listed. Use severity "high", "medium" or "low". Raise none when nothing was missed.

Answer with a single JSON object and nothing else:
{"explanation": "...", "findings": [{"severity": "medium", "title": "...", "location": "...", "before": "...", "after": "..."}]}
The explanation is written in ` + lang + `, in plain text with short paragraphs or "- " bullet lines, no tables, no headings, at most 250 words. The titles of the findings are in ` + lang + ` too.`
}

// maxPromptChanges bounds the excerpts sent to the provider.
const maxPromptChanges = 80

// userPrompt describes the report. Only the file name is sent, not its
// local path, which would reveal the user's account and folder layout.
func userPrompt(path string, r *office.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Document: %s (%s)\n", baseName(path), r.Kind)
	if r.LastModifiedBy != "" {
		fmt.Fprintf(&b, "Last modified by: %s\n", r.LastModifiedBy)
	}
	fmt.Fprintf(&b, "Overall severity: %s, %d change(s) in total\n\nFindings:\n", r.Severity, r.ChangeCount)
	if len(r.Findings) == 0 {
		b.WriteString("(none)\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "- [%s] %s", f.Severity, f.Title)
		if f.Location != "" {
			fmt.Fprintf(&b, " at %s", f.Location)
		}
		b.WriteString("\n")
		if f.Before != "" {
			fmt.Fprintf(&b, "  before: %s\n", f.Before)
		}
		if f.After != "" {
			fmt.Fprintf(&b, "  after: %s\n", f.After)
		}
	}
	b.WriteString("\nChanges:\n")
	for i, c := range r.Changes {
		if i == maxPromptChanges {
			fmt.Fprintf(&b, "(%d more changes not shown)\n", r.ChangeCount-maxPromptChanges)
			break
		}
		fmt.Fprintf(&b, "- %s %s", c.Kind, c.Location)
		if c.Before != "" {
			fmt.Fprintf(&b, " | before: %s", c.Before)
		}
		if c.After != "" {
			fmt.Fprintf(&b, " | after: %s", c.After)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// baseName handles both separators: the report may be explained on another
// system than the one that produced the path.
func baseName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return filepath.Base(path)
}
