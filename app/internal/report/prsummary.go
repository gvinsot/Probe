package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// FormatPRSummary writes the AI-written pull request summary as Markdown, ready
// to paste as a pull request description. It is also written with the
// markdown format whenever a summary exists.
const (
	FormatPRSummary = "pr-summary"
	prSummaryFile   = "PR_SUMMARY.md"
)

// PRSummaryMarker ends a PR_SUMMARY.md render.
const PRSummaryMarker = "<!-- probe:pr-summary v1 -->"

// writePRSummarySection renders the summary inside CONFIDENCE_REPORT.md. It
// writes nothing without a summary, so that other reports keep their exact
// rendering.
func writePRSummarySection(b *bytes.Buffer, r *model.Report) {
	if r.PRSummary == nil {
		return
	}
	line(b, "## Pull Request Summary\n")
	line(b, "Model output, not evidence: written by the reviewer model after the review, from its report. It changes no status, severity or exit code.\n")
	writePRSummaryBody(b, r, "###", true)
}

// areaFindings lists the signals and hypotheses a change area cites, with
// their recorded title, status and location. An ID the report does not hold
// is skipped.
func areaFindings(r *model.Report, c model.PRSummaryChange) []string {
	var items []string
	for _, id := range c.HypothesisIDs {
		for _, h := range r.Hypotheses {
			if h.ID == id {
				where := inline(h.Path)
				if h.Line > 0 {
					where += fmt.Sprintf(":%d", h.Line)
				}
				items = append(items, fmt.Sprintf("**%s / %s** %s — %s", inline(h.Status), inline(h.Severity), inline(h.Title), where))
				break
			}
		}
	}
	for _, id := range c.SignalIDs {
		for _, sig := range r.Signals {
			if sig.ID == id {
				items = append(items, fmt.Sprintf("%s — %s", inline(sig.Summary), signalLocation(sig)))
				break
			}
		}
	}
	return items
}

func signalLocation(s model.Signal) string {
	if s.Scope == model.SignalScopeFile {
		return inline(s.Path) + " (whole file)"
	}
	where := fmt.Sprintf("%s:%d", inline(s.Path), s.Line)
	if s.Side == "old" {
		where += " (old)"
	}
	return where
}

// writePRSummaryBody renders the fields of a summary; level is the heading
// prefix of its subsections. Each statement ends with the code it cites, as
// path:lines, and a risk with the findings it cites. The Testing section also
// states, from the report, which checks Probe executed.
func writePRSummaryBody(b *bytes.Buffer, r *model.Report, level string, overview bool) {
	s := r.PRSummary
	if overview {
		fmt.Fprintf(b, "**%s**\n\n", inline(s.Title))
		for _, paragraph := range strings.Split(s.Overview, "\n") {
			if paragraph = strings.TrimSpace(paragraph); paragraph != "" {
				line(b, inline(paragraph)+"\n")
			}
		}
	}
	if len(s.Changes) > 0 {
		line(b, level+" Changes\n")
		for _, c := range s.Changes {
			fmt.Fprintf(b, "- **%s**: %s%s\n", inline(c.Area), inline(c.Summary), citing(refTexts(c.Refs)))
			for _, item := range areaFindings(r, c) {
				line(b, "  - "+item)
			}
		}
		line(b, "")
	}
	points := func(title string, items []model.PRSummaryPoint) {
		if len(items) == 0 {
			return
		}
		line(b, level+" "+title+"\n")
		for _, p := range items {
			line(b, "- "+inline(p.Text)+citing(refTexts(p.Refs)))
		}
		line(b, "")
	}
	points("Behavior changes", s.BehaviorChanges)
	if len(s.Risks) > 0 {
		line(b, level+" Risks\n")
		for _, risk := range s.Risks {
			line(b, "- **"+inline(risk.Severity)+"** "+inline(risk.Text)+citing(append(refTexts(risk.Refs), findingTexts(r, risk)...)))
		}
		line(b, "")
	}
	if len(s.ReviewFocus) > 0 {
		line(b, level+" Where to look first"+"\n")
		for _, f := range s.ReviewFocus {
			line(b, "- **"+inline(f.Severity)+"** "+inline(f.Text)+citing(refTexts(f.Refs)))
		}
		line(b, "")
	}
	points("Testing", s.Testing)
	if len(s.Testing) == 0 {
		line(b, level+" Testing\n")
	}
	line(b, checksSentence(r.Checks)+"\n")
	if note := citationsNote(s); note != "" {
		line(b, note+"\n")
	}
}

// citing joins what a statement cites, after a dash.
func citing(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return " — " + strings.Join(items, ", ")
}

func refTexts(refs []model.CodeRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, refText(ref))
	}
	return out
}

// refText renders a code reference as path, path:line or path:start-end,
// with a check mark when its quote was found there.
func refText(ref model.CodeRef) string {
	text := inline(ref.Path)
	if ref.StartLine > 0 {
		text += fmt.Sprintf(":%d", ref.StartLine)
		if ref.EndLine > ref.StartLine {
			text += fmt.Sprintf("-%d", ref.EndLine)
		}
		if ref.Side == "old" {
			text += " (old)"
		}
	}
	if ref.Quote != "" {
		text += " ✓"
	}
	return text
}

// citationsNote explains the check marks, and says how many citations were
// dropped; empty when the summary has neither.
func citationsNote(s *model.PRSummary) string {
	quoted := false
	check := func(refs []model.CodeRef) {
		for _, r := range refs {
			quoted = quoted || r.Quote != ""
		}
	}
	for _, c := range s.Changes {
		check(c.Refs)
	}
	for _, list := range [][]model.PRSummaryPoint{s.BehaviorChanges, s.Testing} {
		for _, p := range list {
			check(p.Refs)
		}
	}
	for _, f := range s.ReviewFocus {
		check(f.Refs)
	}
	for _, r := range s.Risks {
		check(r.Refs)
	}
	var parts []string
	if quoted {
		parts = append(parts, "✓ marks a citation whose quoted code Probe found at those lines of the diff; what the summary says about it remains model output.")
	}
	switch n := s.RejectedCitations; {
	case n == 1:
		parts = append(parts, "1 citation quoted code that is not in the diff and was dropped.")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d citations quoted code that is not in the diff and were dropped.", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return "_" + strings.Join(parts, " ") + "_"
}

// findingTexts renders the signals and hypotheses a risk cites with their
// recorded title, location and status; an ID the report does not hold is skipped.
func findingTexts(r *model.Report, risk model.PRSummaryRisk) []string {
	var out []string
	for _, id := range risk.HypothesisIDs {
		for _, h := range r.Hypotheses {
			if h.ID == id {
				where := inline(h.Path)
				if h.Line > 0 {
					where += fmt.Sprintf(":%d", h.Line)
				}
				out = append(out, fmt.Sprintf("%s — %s (%s finding)", inline(h.Title), where, inline(strings.ToLower(h.Status))))
				break
			}
		}
	}
	for _, id := range risk.SignalIDs {
		for _, sig := range r.Signals {
			if sig.ID == id {
				out = append(out, inline(sig.Summary)+" — "+signalLocation(sig)+" (signal)")
				break
			}
		}
	}
	return out
}

// checksSentence states, from the report, what Probe executed.
func checksSentence(checks []model.Check) string {
	if len(checks) == 0 {
		return "_Probe executed no check for this review._"
	}
	counts := map[string]int{}
	var order []string
	for _, c := range checks {
		if counts[c.Status] == 0 {
			order = append(order, c.Status)
		}
		counts[c.Status]++
	}
	parts := make([]string, len(order))
	for i, status := range order {
		parts[i] = fmt.Sprintf("%d %s", counts[status], inline(status))
	}
	noun := "checks"
	if len(checks) == 1 {
		noun = "check"
	}
	return fmt.Sprintf("_Probe executed %d %s for this review: %s._", len(checks), noun, strings.Join(parts, ", "))
}

// renderPRSummary renders PR_SUMMARY.md from a sanitized, finalized report.
func renderPRSummary(r *model.Report) []byte {
	var b bytes.Buffer
	s := r.PRSummary
	if s == nil {
		line(&b, "# Pull request summary\n")
		line(&b, "No AI summary was generated for this run: the reviewer did not run, the change is empty, or the provider did not return a valid summary.\n")
		line(&b, PRSummaryMarker)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "# %s\n\n", inline(s.Title))
	for _, paragraph := range strings.Split(s.Overview, "\n") {
		if paragraph = strings.TrimSpace(paragraph); paragraph != "" {
			line(&b, inline(paragraph)+"\n")
		}
	}
	writePRSummaryBody(&b, r, "##", false)
	fmt.Fprintf(&b, "---\n\n_Written by %s from the Probe review of %d changed files (%s). Model output, not evidence; the review verdict is in the confidence report._\n\n", inline(s.Model), len(r.Change.Files), verdictWords(r.ExitCode))
	line(&b, PRSummaryMarker)
	return b.Bytes()
}

func verdictWords(code int) string {
	switch code {
	case 1:
		return "a high or critical issue was reproduced"
	case 2:
		return "human review required"
	case 4:
		return "the review could not complete"
	default:
		return "no blocking issue was reproduced"
	}
}
