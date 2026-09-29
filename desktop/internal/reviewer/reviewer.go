// Package reviewer asks an AI model to explain a report in plain language.
//
// The model reads the findings and the changed excerpts the deterministic
// comparison already produced, never the whole document, and its answer is
// shown as an explanation next to the findings. It does not add, remove or
// grade findings: the verdict stays the one of the rules.
package reviewer

import (
	"context"
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

// Explain returns the explanation of a report.
func Explain(ctx context.Context, s config.Settings, apiKey, path string, report *office.Report) (string, error) {
	if s.Provider == config.ProviderNone || (apiKey == "" && s.BaseURL == "") {
		return "", ErrNotConfigured
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
	switch s.Provider {
	case config.ProviderAnthropic:
		return explainAnthropic(ctx, req)
	case config.ProviderOpenAI:
		return explainOpenAI(ctx, req)
	}
	return "", ErrNotConfigured
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

Answer in ` + lang + `, in plain text with short paragraphs or "- " bullet lines, no tables, no headings, at most 250 words.`
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
