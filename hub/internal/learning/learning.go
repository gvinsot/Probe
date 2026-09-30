// Package learning turns a team's reactions to Probe findings into the
// feedback document the CLI gives its reviewer (--feedback-file). Votes,
// comments and replies come from the hub's users; outcomes are measured by
// comparing a report with the files the next analyzed commit changed. The
// hub never re-derives a verdict from them: they only adapt how the
// reviewer model investigates and words later reviews.
package learning

import (
	"sort"
	"strings"

	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// Bounds of the document given to the CLI; they stay within the CLI's own.
const (
	maxTopics      = 100
	maxComments    = 30
	maxQuotedReply = 300
)

// Feedback mirrors the CLI's --feedback-file document.
type Feedback struct {
	Topics   []Topic   `json:"topics"`
	Comments []Comment `json:"comments"`
}

// Topic aggregates the reactions to one kind of finding.
type Topic struct {
	Topic     string `json:"topic"`
	Useful    int    `json:"useful"`
	NotUseful int    `json:"not_useful"`
	Changed   int    `json:"changed"`
	Unchanged int    `json:"unchanged"`
}

// Comment is one human comment on a finding, or a reply to one.
type Comment struct {
	Topic   string `json:"topic"`
	Path    string `json:"path,omitempty"`
	Title   string `json:"title,omitempty"`
	Vote    string `json:"vote,omitempty"`
	Comment string `json:"comment"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// Empty reports whether the document holds nothing to learn from.
func (f *Feedback) Empty() bool { return f == nil || len(f.Topics) == 0 && len(f.Comments) == 0 }

// TopicOf names the kind of finding an alert belongs to, as the CLI's
// reviewer sees it: "signal:<linter kind>", "issue" or "check". A review
// target, which only prioritizes other findings, has none.
func TopicOf(a report.Alert) string {
	switch a.Kind {
	case report.KindIssue:
		return "issue"
	case report.KindCheck:
		return "check"
	case report.KindSignal:
		if len(a.Members) > 0 {
			return TopicOf(a.Members[0])
		}
		if len(a.Reasons) > 0 && a.Reasons[0] != "" {
			return "signal:" + a.Reasons[0]
		}
	}
	return ""
}

// Find returns an alert of a report by ID, active or set aside, grouped or
// member of a group.
func Find(r *report.Report, id string) (report.Alert, bool) {
	for _, a := range findings(r, false) {
		if a.ID == id {
			return a, true
		}
	}
	return report.Alert{}, false
}

// findings lists the alerts of a report that can receive feedback. With
// members, a group is replaced by the alerts it gathers.
func findings(r *report.Report, members bool) []report.Alert {
	var out []report.Alert
	var add func(a report.Alert)
	add = func(a report.Alert) {
		if len(a.Members) > 0 {
			if !members {
				out = append(out, a)
			}
			for _, m := range a.Members {
				add(m)
			}
			return
		}
		if TopicOf(a) != "" {
			out = append(out, a)
		}
	}
	for _, a := range r.Alerts() {
		add(a)
	}
	for _, a := range r.Dismissed() {
		add(a)
	}
	return out
}

// Outcomes compares the findings of the report of a commit with the report
// of the next analyzed commit on top of it: a finding whose file that commit
// changed counts as changed, any other as unchanged. It is a heuristic (a
// change is no proof of a fix, and the next commit may address another part
// of the work), counted once per kind of finding and file.
func Outcomes(previous, next *report.Report) map[string]store.OutcomeCounts {
	changed := map[string]bool{}
	for _, f := range next.Change.Files {
		changed[f.Path] = true
		if f.OldPath != "" {
			changed[f.OldPath] = true
		}
	}
	seen := map[[2]string]bool{}
	out := map[string]store.OutcomeCounts{}
	for _, a := range findings(previous, true) {
		topic := TopicOf(a)
		key := [2]string{topic, a.Path}
		if a.Path == "" || seen[key] {
			continue
		}
		seen[key] = true
		c := out[topic]
		if changed[a.Path] {
			c.Changed++
		} else {
			c.Unchanged++
		}
		out[topic] = c
	}
	return out
}

// Summarize builds the document the CLI receives from what a repository
// kept. A person's latest vote on a finding is the one counted; comments
// and replies are quoted newest first.
func Summarize(repo *store.Repo) *Feedback {
	topics := map[string]*Topic{}
	topic := func(name string) *Topic {
		t, ok := topics[name]
		if !ok {
			t = &Topic{Topic: name}
			topics[name] = t
		}
		return t
	}
	type voter struct{ commit, alert, author string }
	latest := map[voter]store.FeedbackEntry{}
	byID := make(map[string]store.FeedbackEntry, len(repo.Feedback))
	for _, e := range repo.Feedback {
		byID[e.ID] = e
		if e.Vote != "" {
			latest[voter{e.Commit, e.AlertID, e.Author}] = e
		}
	}
	for _, e := range latest {
		switch e.Vote {
		case store.VoteUp:
			topic(e.Topic).Useful++
		case store.VoteDown:
			topic(e.Topic).NotUseful++
		}
	}
	for name, c := range repo.Outcomes {
		t := topic(name)
		t.Changed, t.Unchanged = c.Changed, c.Unchanged
	}
	f := &Feedback{Topics: make([]Topic, 0, len(topics)), Comments: []Comment{}}
	for _, t := range topics {
		f.Topics = append(f.Topics, *t)
	}
	// The most reacted-to kinds first, so that a cut keeps what matters.
	sort.Slice(f.Topics, func(i, j int) bool {
		a, b := f.Topics[i], f.Topics[j]
		if na, nb := a.Useful+a.NotUseful+a.Changed+a.Unchanged, b.Useful+b.NotUseful+b.Changed+b.Unchanged; na != nb {
			return na > nb
		}
		return a.Topic < b.Topic
	})
	if len(f.Topics) > maxTopics {
		f.Topics = f.Topics[:maxTopics]
	}
	for i := len(repo.Feedback) - 1; i >= 0 && len(f.Comments) < maxComments; i-- {
		e := repo.Feedback[i]
		if strings.TrimSpace(e.Comment) == "" {
			continue
		}
		c := Comment{Topic: e.Topic, Path: e.Path, Title: e.Title, Vote: e.Vote, Comment: e.Comment}
		if parent, ok := byID[e.ReplyTo]; ok && e.ReplyTo != "" {
			c.ReplyTo = truncate(parent.Comment, maxQuotedReply)
			if c.ReplyTo == "" {
				c.ReplyTo = "a vote on this finding"
			}
		}
		f.Comments = append(f.Comments, c)
	}
	return f
}

// truncate cuts s at a rune boundary.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
