package analysis

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/learning"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// The learned feedback reaches the reviewer as a private JSON file.
func TestRunCLIPassesTeamFeedbackToTheReviewer(t *testing.T) {
	binary := fakeCLI(t, `while [ $# -gt 0 ]; do
  if [ "$1" = "--feedback-file" ]; then cat "$2" > feedback.json; fi
  shift
done`)
	r, _ := testRunner(t, binary)
	t.Setenv(config.EndpointEnvName, "https://provider.example/v1")
	t.Setenv(config.ModelEnvName, "test")
	feedback := &learning.Feedback{Topics: []learning.Topic{{Topic: "issue", Useful: 2}}, Comments: []learning.Comment{}}
	for _, tc := range []struct {
		name     string
		feedback *learning.Feedback
		want     bool
	}{{"with feedback", feedback, true}, {"empty", &learning.Feedback{}, false}, {"learning off", nil, false}} {
		work := t.TempDir()
		if _, code, err := r.runCLI(context.Background(), work, config.ModeReadOnly, "base", "head", reviewerInputs{feedback: tc.feedback}); code != 0 || err != nil {
			t.Fatalf("%s: exit %d: %v", tc.name, code, err)
		}
		data, err := os.ReadFile(filepath.Join(work, "feedback.json"))
		if (err == nil) != tc.want {
			t.Fatalf("%s: feedback file passed = %v", tc.name, err == nil)
		}
		if tc.want && !strings.Contains(string(data), `"topic":"issue","useful":2`) {
			t.Fatalf("%s: feedback = %s", tc.name, data)
		}
	}
}

const outcomeReport = `{"version":1,"change":{"files":[%s]},"linter":[%s],"checks":[],"hypotheses":[],"evidence":[],"reproduced_issues":[],"unverified":[],"review_targets":[],"review_surface":{},"coverage":{},"exit_code":0}`

func outcomeRaw(files, signals string) []byte {
	return []byte(strings.Replace(strings.Replace(outcomeReport, "%s", files, 1), "%s", signals, 1))
}

// A run compares the report of its base commit with its own once, and learns
// nothing while learning is off.
func TestLearnOutcomesFromTheNextCommit(t *testing.T) {
	r, st := testRunner(t, "unused")
	user, repoKey := store.Key("github", "1"), store.Key("github", "10")
	if err := st.PutRepo(user, &store.Repo{Key: repoKey, Provider: "github", ID: "10"}); err != nil {
		t.Fatal(err)
	}
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	previous := outcomeRaw(`{"path":"a.go"},{"path":"b.go"}`, `{"id":"1","kind":"no_test_change","path":"a.go","line":1,"severity":"medium","summary":"x","evidence":""},{"id":"2","kind":"no_test_change","path":"b.go","line":1,"severity":"medium","summary":"x","evidence":""}`)
	if err := st.PutRecord(&store.Record{UserKey: user, RepoKey: repoKey, Raw: json.RawMessage(previous), Run: store.Run{Commit: base, Status: store.StatusDone}}); err != nil {
		t.Fatal(err)
	}
	j := Job{UserKey: user, RepoKey: repoKey, Commit: head}
	run := store.Run{Commit: head, BaseCommit: base, Status: store.StatusDone}
	next := outcomeRaw(`{"path":"a.go"}`, ``)
	r.learnOutcomes(j, run, next)
	r.learnOutcomes(j, run, next) // a rerun counts nothing twice
	repo, _ := st.Repo(user, repoKey)
	if got := repo.Outcomes["signal:no_test_change"]; got != (store.OutcomeCounts{Changed: 1, Unchanged: 1}) || len(repo.OutcomeBases) != 1 {
		t.Fatalf("outcomes = %+v, bases %v", repo.Outcomes, repo.OutcomeBases)
	}

	if _, err := st.UpdateRepo(user, repoKey, func(r *store.Repo) error { r.LearningOff, r.Outcomes, r.OutcomeBases = true, nil, nil; return nil }); err != nil {
		t.Fatal(err)
	}
	r.learnOutcomes(j, run, next)
	if repo, _ := st.Repo(user, repoKey); repo.Outcomes != nil {
		t.Fatal("learned while learning was off")
	}
	j.Variant = "plan"
	r.learnOutcomes(j, run, next)
}
