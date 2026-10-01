package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

// writePRSummary asks the reviewer model for the pull request summary of the
// finalized report. The verdict is already derived: a failure is reported on
// errOut and in the audit, never as a review outcome.
func writePRSummary(ctx context.Context, repo *gitrepo.Repository, options reviewer.Options, r *model.Report, errOut io.Writer) {
	fmt.Fprintln(errOut, "Writing the pull request summary with the configured reviewer API...")
	var commits []string
	if messages, err := repo.CommitMessages(ctx, r.Change.BaseCommit, r.Change.HeadCommit); err == nil {
		commits = messages
	}
	// The summary is one completion: the swarm does not apply.
	options.Swarm = nil
	summary, events, err := reviewer.Summarize(ctx, options, r, reviewer.SummaryInput{CommitMessages: commits})
	r.Audit = append(r.Audit, events...)
	if err != nil {
		fmt.Fprintln(errOut, "Pull request summary not written: "+err.Error())
		// Recorded in the report too, so that a reader of the report, or of
		// the hub, sees why the summary is missing.
		r.Audit = append(r.Audit, model.AuditEvent{Time: time.Now().UTC(), Tool: "pr_summary", Arguments: redact.TruncateUTF8(err.Error(), 300), Status: "ERROR"})
		return
	}
	r.PRSummary = summary
}
