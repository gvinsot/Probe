package model

// TeamFeedback is what a team's past reactions to Probe findings say about its
// review preferences. It is produced outside the review (Probe Hub builds it
// from votes, comments and outcomes of earlier findings) and given to the
// reviewer with --feedback-file. It is guidance for the model, never
// evidence: it changes no status, severity or exit code by itself.
type TeamFeedback struct {
	Topics   []FeedbackTopic   `json:"topics"`
	Comments []FeedbackComment `json:"comments"`
}

// FeedbackTopic aggregates the reactions to one kind of finding. Topic is
// "signal:<linter kind>" (for example "signal:no_test_change"), "issue" for
// reviewer hypotheses, or "check" for failed checks.
type FeedbackTopic struct {
	Topic string `json:"topic"`
	// Votes of reviewers: the finding was useful, or not.
	Useful    int `json:"useful"`
	NotUseful int `json:"not_useful"`
	// Outcomes: the next analyzed commit changed the file the finding was
	// about, or left it unchanged. A heuristic, not proof of a fix.
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
}

// FeedbackComment is one human comment on a finding, or a reply to one.
type FeedbackComment struct {
	Topic   string `json:"topic"`
	Path    string `json:"path,omitempty"`
	Title   string `json:"title,omitempty"`
	Vote    string `json:"vote,omitempty"` // "up", "down" or ""
	Comment string `json:"comment"`
	ReplyTo string `json:"reply_to,omitempty"` // the comment this one answers
}

// Feedback votes.
const (
	FeedbackUp   = "up"
	FeedbackDown = "down"
)
