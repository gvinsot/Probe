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
	s := r.PRSummary
	if s == nil {
		return
	}
	line(b, "## Pull Request Summary\n")
	line(b, "Model output, not evidence: written by the reviewer model after the review, from its report. It changes no status, severity or exit code.\n")
	writePRSummaryBody(b, s, "###")
	writeIntentGroups(b, r)
}

// writeIntentGroups lists the signals and hypotheses each intent of the
// summary cites, with their recorded title and location. An ID the report
// does not hold is skipped, and so is an intent left without any.
func writeIntentGroups(b *bytes.Buffer, r *model.Report) {
	signals := make(map[string]model.Signal, len(r.Signals))
	for _, s := range r.Signals {
		signals[s.ID] = s
	}
	hypotheses := make(map[string]model.Hypothesis, len(r.Hypotheses))
	for _, h := range r.Hypotheses {
		hypotheses[h.ID] = h
	}
	heading := false
	for _, intent := range r.PRSummary.Intents {
		var items []string
		for _, id := range intent.SignalIDs {
			if s, ok := signals[id]; ok {
				items = append(items, fmt.Sprintf("%s — %s", inline(s.Summary), signalLocation(s)))
			}
		}
		for _, id := range intent.HypothesisIDs {
			if h, ok := hypotheses[id]; ok {
				where := inline(h.Path)
				if h.Line > 0 {
					where += fmt.Sprintf(":%d", h.Line)
				}
				items = append(items, fmt.Sprintf("**%s / %s** %s — %s", inline(h.Status), inline(h.Severity), inline(h.Title), where))
			}
		}
		if len(items) == 0 {
			continue
		}
		if !heading {
			line(b, "### Findings by intent\n")
			heading = true
		}
		fmt.Fprintf(b, "- **%s**\n", inline(intent.Intent))
		for _, item := range items {
			line(b, "  - "+item)
		}
	}
	if heading {
		line(b, "")
	}
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
// prefix of its subsections.
func writePRSummaryBody(b *bytes.Buffer, s *model.PRSummary, level string) {
	fmt.Fprintf(b, "**%s**\n\n", inline(s.Title))
	for _, paragraph := range strings.Split(s.Overview, "\n") {
		if paragraph = strings.TrimSpace(paragraph); paragraph != "" {
			line(b, inline(paragraph)+"\n")
		}
	}
	if len(s.Changes) > 0 {
		line(b, level+" Changes\n")
		for _, c := range s.Changes {
			files := ""
			if len(c.Files) > 0 {
				quoted := make([]string, len(c.Files))
				for i, f := range c.Files {
					quoted[i] = inline(f)
				}
				files = " (" + strings.Join(quoted, ", ") + ")"
			}
			fmt.Fprintf(b, "- **%s**: %s%s\n", inline(c.Area), inline(c.Summary), files)
		}
		line(b, "")
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		line(b, level+" "+title+"\n")
		for _, item := range items {
			line(b, "- "+inline(item))
		}
		line(b, "")
	}
	list("Behavior changes", s.BehaviorChanges)
	list("Risks", s.Risks)
	list("Where to look first", s.ReviewFocus)
	if strings.TrimSpace(s.Testing) != "" {
		line(b, level+" Testing\n")
		line(b, inline(s.Testing)+"\n")
	}
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
	body := *s
	body.Overview = ""
	var rest bytes.Buffer
	writePRSummaryBody(&rest, &body, "##")
	// The body repeats the title in bold; the file has it as its heading.
	b.Write(bytes.TrimPrefix(rest.Bytes(), []byte(fmt.Sprintf("**%s**\n\n", inline(s.Title)))))
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
