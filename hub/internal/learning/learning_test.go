package learning

import (
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

func reportWith(files []string, signals []report.Signal, hypotheses []report.Hypothesis) *report.Report {
	r := &report.Report{Version: 1, Signals: signals, Hypotheses: hypotheses}
	for _, f := range files {
		r.Change.Files = append(r.Change.Files, report.ChangedFile{Path: f})
	}
	return r
}

func TestTopicOf(t *testing.T) {
	for want, a := range map[string]report.Alert{
		"issue":                 {Kind: report.KindIssue},
		"check":                 {Kind: report.KindCheck},
		"signal:no_test_change": {Kind: report.KindSignal, Reasons: []string{"no_test_change"}},
		"signal:sensitive_path": {Kind: report.KindSignal, Members: []report.Alert{{Kind: report.KindSignal, Reasons: []string{"sensitive_path"}}}},
		"":                      {Kind: report.KindFocus},
	} {
		if got := TopicOf(a); got != want {
			t.Errorf("TopicOf(%+v) = %q, want %q", a, got, want)
		}
	}
}

// A finding whose file the next commit changed counts as changed, any other
// as unchanged, once per kind and file.
func TestOutcomes(t *testing.T) {
	previous := reportWith([]string{"a.go", "b.go"}, []report.Signal{
		{ID: "1", Kind: "no_test_change", Path: "a.go", Line: 1, Severity: "medium", Summary: "x"},
		{ID: "2", Kind: "no_test_change", Path: "a.go", Line: 5, Severity: "medium", Summary: "y"},
		{ID: "3", Kind: "no_test_change", Path: "b.go", Line: 1, Severity: "medium", Summary: "z"},
	}, []report.Hypothesis{{ID: "h1", Title: "Nil deref", Severity: "high", Status: "UNVERIFIED", Path: "b.go", Line: 2}})
	next := reportWith([]string{"b.go"}, nil, nil)
	got := Outcomes(previous, next)
	if got["signal:no_test_change"] != (store.OutcomeCounts{Changed: 1, Unchanged: 1}) || got["issue"] != (store.OutcomeCounts{Changed: 1}) {
		t.Fatalf("outcomes = %+v", got)
	}
}

func TestSummarize(t *testing.T) {
	at := time.Now()
	repo := &store.Repo{
		Feedback: []store.FeedbackEntry{
			{ID: "1", Commit: "c", AlertID: "a", Topic: "issue", Vote: store.VoteUp, Author: "ada", At: at},
			{ID: "2", Commit: "c", AlertID: "a", Topic: "issue", Vote: store.VoteDown, Comment: "Intentional.", Author: "ada", At: at},
			{ID: "3", Commit: "c", AlertID: "a", Topic: "issue", Vote: store.VoteUp, Author: "grace", At: at},
			{ID: "4", Commit: "c", AlertID: "a", Topic: "issue", Comment: "Why?", ReplyTo: "2", Author: "grace", At: at},
		},
		Outcomes: map[string]store.OutcomeCounts{"signal:no_test_change": {Changed: 1, Unchanged: 9}},
	}
	f := Summarize(repo)
	if len(f.Topics) != 2 || f.Topics[0].Topic != "signal:no_test_change" || f.Topics[0].Unchanged != 9 {
		t.Fatalf("topics = %+v", f.Topics)
	}
	if issue := f.Topics[1]; issue.Useful != 1 || issue.NotUseful != 1 {
		t.Fatalf("each person's latest vote counts once: %+v", issue)
	}
	if len(f.Comments) != 2 || f.Comments[0].Comment != "Why?" || f.Comments[0].ReplyTo != "Intentional." || f.Comments[1].Vote != store.VoteDown {
		t.Fatalf("comments = %+v", f.Comments)
	}
	if !(&Feedback{}).Empty() || f.Empty() || !(*Feedback)(nil).Empty() {
		t.Fatal("Empty")
	}
}
