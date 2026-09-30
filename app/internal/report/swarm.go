package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// writeReviewerSwarm renders the agents of a reviewer swarm. It writes
// nothing without a swarm, so that other reports keep their exact rendering.
func writeReviewerSwarm(b *bytes.Buffer, r *model.Report) {
	if len(r.ReviewerAgents) == 0 {
		return
	}
	line(b, "\n## Reviewer Swarm\n")
	line(b, fmt.Sprintf("%d specialized reviewer agents investigated the change in parallel. Each finding below names the agents that submitted it; attribution is not evidence, and every finding is checked against the recorded evidence like any other.\n", len(r.ReviewerAgents)))
	for _, a := range r.ReviewerAgents {
		scope := "whole change"
		if len(a.Paths) > 0 {
			scope = fmt.Sprintf("%d changed files", len(a.Paths))
		}
		note := ""
		if a.Note != "" {
			note = " — " + inline(a.Note)
		}
		fmt.Fprintf(b, "- **%s** (%s; %s): %s, %d hypotheses submitted%s\n", inline(a.Name), inline(a.Focus), scope, inline(a.Status), a.Hypotheses, note)
	}
}

// agentsNote credits the swarm agents of a hypothesis; "" for one reviewer.
func agentsNote(h model.Hypothesis) string {
	if len(h.Agents) == 0 {
		return ""
	}
	names := make([]string, len(h.Agents))
	for i, a := range h.Agents {
		names[i] = inline(a)
	}
	return " _(agents: " + strings.Join(names, ", ") + ")_"
}
