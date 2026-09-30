// Package reviewer asks an AI model to review a report.
//
// The model reads the findings and the changed excerpts the deterministic
// comparison already produced, and the few unchanged passages that still use
// a term a change replaced or removed; never the whole document. It answers
// with a plain-language explanation and may point out additional risky
// changes the rules missed. Those are kept apart as AI findings: they never
// remove or downgrade a rule finding. It may also give each rule finding a
// reading: a precise title in the words of the document ("landlord's name
// replaced in paragraph 4") and whether the change still agrees with the rest
// of the document.
//
// When the model states that the modifications may have a legal or financial
// impact, the severity of the document is raised: to high for one of them, to
// critical for both. The model can only raise the severity, never lower it.
package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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
	// Readings qualify the rule findings of the report.
	Readings []Reading
	// Impacts are the consequences the model states the modifications may
	// have (ImpactLegal, ImpactFinancial), declared or written in its answer.
	Impacts []string
	// Severity is the level the impacts raise the document to, empty when
	// they raise nothing.
	Severity string
}

// Reading is what the model says about a rule finding: a title naming what
// changed in the words of the document, and whether the change still agrees
// with the rest of it.
type Reading struct {
	// Finding is the index of the rule finding in the report.
	Finding     int    `json:"finding"`
	Title       string `json:"title"`
	Consistency string `json:"consistency,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Impacts the model can state.
const (
	ImpactLegal     = "legal"
	ImpactFinancial = "financial"
)

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
	return parseAnswer(raw, len(report.Findings)), nil
}

// parseAnswer reads the JSON answer asked by the prompt and the impacts it
// states. A model that ignores the format (small local models) still gives a
// usable explanation: the raw text is then shown as is, without extra
// findings, and still read for impacts.
func parseAnswer(raw string, ruleFindings int) Result {
	res, declared := decodeAnswer(raw, ruleFindings)
	texts := []string{res.Text}
	for _, f := range res.Findings {
		texts = append(texts, f.Title, f.Note)
	}
	for _, r := range res.Readings {
		texts = append(texts, r.Title, r.Note)
	}
	res.Impacts = impacts(declared, texts)
	res.Severity = escalation(res.Impacts)
	return res
}

// consistency keeps the verdicts the interface knows; "unknown" and anything
// else say nothing.
func consistency(s string) string {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case office.Inconsistent, office.Consistent:
		return v
	}
	return ""
}

func decodeAnswer(raw string, ruleFindings int) (Result, []string) {
	raw = strings.TrimSpace(raw)
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return Result{Text: raw}, nil
	}
	var a struct {
		Explanation string   `json:"explanation"`
		Impacts     []string `json:"impacts"`
		Findings    []struct {
			Severity    string `json:"severity"`
			Title       string `json:"title"`
			Location    string `json:"location"`
			Before      string `json:"before"`
			After       string `json:"after"`
			Consistency string `json:"consistency"`
			Note        string `json:"note"`
		} `json:"findings"`
		Readings []struct {
			Finding     int    `json:"finding"`
			Title       string `json:"title"`
			Consistency string `json:"consistency"`
			Note        string `json:"note"`
		} `json:"readings"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &a); err != nil || strings.TrimSpace(a.Explanation) == "" {
		return Result{Text: raw}, nil
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
			Consistency: consistency(f.Consistency), Note: clip(f.Note, 400),
		})
	}
	// The prompt numbers the rule findings from 1; one reading per finding.
	seen := map[int]bool{}
	for _, rd := range a.Readings {
		i := rd.Finding - 1
		title := strings.TrimSpace(rd.Title)
		if i < 0 || i >= ruleFindings || seen[i] || (title == "" && consistency(rd.Consistency) == "") {
			continue
		}
		seen[i] = true
		res.Readings = append(res.Readings, Reading{
			Finding: i, Title: clip(title, 200), Consistency: consistency(rd.Consistency), Note: clip(rd.Note, 400),
		})
	}
	return res, a.Impacts
}

// A sentence states an impact when it names a consequence and its domain,
// like "Cette modification peut avoir une incidence juridique et financière"
// or "this change has legal and financial implications", and is not negated.
var (
	sentenceEnd    = regexp.MustCompile(`[.!?;\n]+`)
	consequenceRe  = regexp.MustCompile(`(?i)incidence|impact|cons[ée]quence|implication|effet|effect|port[ée]e|enjeu|risque|risk|exposure|exposition`)
	legalRe        = regexp.MustCompile(`(?i)juridique|l[ée]gal|contractuel|contractual|r[ée]glementaire|regulatory`)
	financialRe    = regexp.MustCompile(`(?i)financi|fiscal`)
	negationRe     = regexp.MustCompile(`(?i)\b(aucune?|sans|pas|ni|no|not|without|none|neither|nor)\b`)
	notNegationsRe = regexp.MustCompile(`(?i)\bno longer\b|\bnot only\b|\bpas seulement\b`)
)

// impacts merges the impacts the model declared with the ones its text
// states, in a fixed order.
func impacts(declared, texts []string) []string {
	legal, financial := false, false
	for _, d := range declared {
		switch strings.ToLower(strings.TrimSpace(d)) {
		case ImpactLegal:
			legal = true
		case ImpactFinancial:
			financial = true
		}
	}
	for _, t := range texts {
		for _, s := range sentenceEnd.Split(t, -1) {
			if !consequenceRe.MatchString(s) || negationRe.MatchString(notNegationsRe.ReplaceAllString(s, "")) {
				continue
			}
			legal = legal || legalRe.MatchString(s)
			financial = financial || financialRe.MatchString(s)
		}
	}
	var out []string
	if legal {
		out = append(out, ImpactLegal)
	}
	if financial {
		out = append(out, ImpactFinancial)
	}
	return out
}

// escalation is the severity the impacts raise the document to: a change
// that may have both legal and financial consequences is critical.
func escalation(impacts []string) string {
	switch len(impacts) {
	case 0:
		return ""
	case 1:
		return office.High
	}
	return office.Critical
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// promptLanguages gives, per interface language, the language the model
// writes in and an example of a qualified title, a vague one, and a
// consistency note, in that language.
var promptLanguages = map[string]struct{ name, good, bad, note string }{
	"en": {"English", `"Landlord's name replaced in paragraph 4 (Dupont → Martin)"`, `"Person substitution in paragraph 4"`,
		`"Paragraph 1 still names Mr Dupont as the landlord: this is no longer consistent with the rest of the document."`},
	"fr": {"French", `"Nom du bailleur remplacé au paragraphe 4 (Dupont → Martin)"`, `"Substitution de personne dans le paragraphe 4"`,
		`"Le paragraphe 1 désigne toujours M. Dupont comme bailleur : ce n'est plus cohérent avec le reste du document."`},
	"es": {"Spanish", `"Nombre del arrendador sustituido en el párrafo 4 (Dupont → Martin)"`, `"Sustitución de persona en el párrafo 4"`,
		`"El párrafo 1 sigue designando al Sr. Dupont como arrendador: ya no es coherente con el resto del documento."`},
	"de": {"German", `"Name des Vermieters in Absatz 4 ersetzt (Dupont → Martin)"`, `"Personenersetzung in Absatz 4"`,
		`"Absatz 1 nennt weiterhin Herrn Dupont als Vermieter: Das passt nicht mehr zum restlichen Dokument."`},
	"pt": {"Brazilian Portuguese", `"Nome do locador substituído no parágrafo 4 (Dupont → Martin)"`, `"Substituição de pessoa no parágrafo 4"`,
		`"O parágrafo 1 ainda designa o Sr. Dupont como locador: isso não é mais coerente com o restante do documento."`},
	"it": {"Italian", `"Nome del locatore sostituito al paragrafo 4 (Dupont → Martin)"`, `"Sostituzione di persona al paragrafo 4"`,
		`"Il paragrafo 1 indica ancora il sig. Dupont come locatore: non è più coerente con il resto del documento."`},
}

func systemPrompt(language string) string {
	l, ok := promptLanguages[language]
	if !ok {
		l = promptLanguages["en"]
	}
	lang, good, bad, note := l.name, l.good, l.bad, l.note
	return `You help a person review the latest modifications of an office document (Word, Excel or PowerPoint) before they accept them.

A deterministic comparison already listed the changes and flagged the risky ones with a severity. You receive those findings (numbered F1, F2…), excerpts of the changed content and, under "Other passages", the unchanged passages that still use a term a change replaced or removed. You do not receive the full document.

Write a short explanation for a non-technical reader:
- First, two or three sentences on what changed overall.
- Then the points that deserve attention, most important first, each saying what to check and why it matters (financial impact, legal meaning, broken calculation, hidden content...).
- Stay factual: rely only on the excerpts given. When the excerpts are not enough to conclude, say what the reader should open and verify.
- Do not change the severities and do not declare the document safe; the person decides.

Qualify every title precisely, in the words of the document: say which role, party, clause, amount or date changed and where, as a reader who knows the document would. Use the passages to identify roles (a name defined as "the Landlord", "le Preneur", "the Supplier"…). Write ` + good + `, not ` + bad + `.

For each rule finding, you may give a reading in "readings": its number, a qualified title, and its consistency with the rest of the document. Consistency is "inconsistent" when the passages show the rest of the document still uses the previous value or contradicts the new one, "consistent" when they show it was updated everywhere it appears, "unknown" when the excerpts do not tell. Add a one-sentence note saying why, like ` + note + `

You may also raise additional findings: risky changes visible in the excerpts that the rules did not flag (a figure that no longer matches its context, a meaning reversed by rewording, a suspicious removal...). Only raise a finding you can point to in the excerpts, with its location; do not repeat a finding already listed, qualify it with a reading instead. Use severity "high", "medium" or "low", and give its consistency and note as for a reading. Raise none when nothing was missed.

List in "impacts" the consequences the modifications may have: "legal" (meaning of a contract or commitment, obligations, liability, compliance) and "financial" (amounts, prices, payments, totals, budget). List one only when the excerpts support it and say it in the explanation; leave the list empty otherwise.

Answer with a single JSON object and nothing else:
{"explanation": "...", "impacts": ["legal", "financial"], "readings": [{"finding": 1, "title": "...", "consistency": "inconsistent", "note": "..."}], "findings": [{"severity": "medium", "title": "...", "location": "...", "before": "...", "after": "...", "consistency": "unknown", "note": "..."}]}
The explanation is written in ` + lang + `, in plain text with short paragraphs or "- " bullet lines, no tables, no headings, at most 250 words. The titles and notes are in ` + lang + ` too.`
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
	for i, f := range r.Findings {
		fmt.Fprintf(&b, "- F%d [%s] %s", i+1, f.Severity, f.Title)
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
	if len(r.Mentions) > 0 {
		b.WriteString("\nOther passages of the current version that still use a replaced or removed term:\n")
		for _, m := range r.Mentions {
			if m.Replacement != "" {
				fmt.Fprintf(&b, "- %q replaced by %q at %s, still used in %d other passage(s):\n", m.Term, m.Replacement, m.Location, m.Count)
			} else {
				fmt.Fprintf(&b, "- %q removed at %s, still used in %d other passage(s):\n", m.Term, m.Location, m.Count)
			}
			for _, o := range m.Elsewhere {
				fmt.Fprintf(&b, "  %s: %s\n", o.Location, o.Excerpt)
			}
		}
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
