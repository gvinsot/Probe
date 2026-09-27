package analysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/hub/internal/config"
	"github.com/gvinsot/SwiftProof/hub/internal/report"
	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

func TestPlanUsesParentAndPreservesCLIDecision(t *testing.T) {
	t.Setenv(config.EndpointEnvName, "https://provider.example/v1")
	t.Setenv(config.ModelEnvName, "test-model")
	binary := fakeCLI(t, `
printf '%s\n' "$@" > args.txt
mkdir -p .swiftproof
printf '%s' '{"format":"swiftproof-plan","version":1,"base_commit":"parent-sha","tool_version":"test","exit_code":2}' > .swiftproof/PLAN.json
exit 2
`)
	r, _ := testRunner(t, binary)
	work := t.TempDir()
	run := &store.Run{}
	rec, err := r.analyzePlan(context.Background(), work, "parent-sha", Job{Commit: strings.Repeat("a", 40), Intent: "add a graph\nwith branches"}, run, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode != "plan" || run.Summary.Verdict != report.VerdictReview || len(rec.Raw) == 0 {
		t.Fatalf("run = %+v", run)
	}
	args, err := os.ReadFile(filepath.Join(work, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--base\nparent-sha\n") || strings.Contains(string(args), "--head") || !strings.Contains(string(args), "--intent\nadd a graph\nwith branches\n") {
		t.Fatalf("args: %s", args)
	}
}

func TestPlanRequiresDeploymentProvider(t *testing.T) {
	t.Setenv(config.EndpointEnvName, "")
	r, _ := testRunner(t, "/must-not-run")
	_, err := r.analyzePlan(context.Background(), t.TempDir(), "base", Job{}, &store.Run{}, "repo")
	if err == nil || !strings.Contains(err.Error(), "deployment-configured") {
		t.Fatalf("err: %v", err)
	}
}

func TestQueueSeparatesPlanAndNormalAndDeduplicatesEach(t *testing.T) {
	r, _ := testRunner(t, "/bin/true")
	job := Job{UserKey: "user", RepoKey: "repo", Commit: strings.Repeat("a", 40)}
	for _, variant := range []string{"", "normal", "plan", "plan"} {
		job.Variant, job.Intent = variant, "intent"
		if err := r.Enqueue(job); err != nil {
			t.Fatal(err)
		}
	}
	if r.Pending() != 2 {
		t.Fatalf("queue = %d", r.Pending())
	}
	job.Variant = "plan"
	job.Intent = ""
	if r.Enqueue(job) == nil {
		t.Fatal("accepted an empty plan intent")
	}
}

func TestPlanNeverReplacesRepositoryVerdict(t *testing.T) {
	r, st := testRunner(t, "/bin/true")
	normal := &store.Run{Commit: strings.Repeat("a", 40), Summary: report.Summary{Verdict: report.VerdictBlocked}}
	if err := st.PutRepo("user", &store.Repo{Key: "repo", Latest: normal}); err != nil {
		t.Fatal(err)
	}
	r.publishRun(Job{UserKey: "user", RepoKey: "repo", Variant: "plan"}, store.Run{Summary: report.Summary{Verdict: report.VerdictClear}})
	repo, err := st.Repo("user", "repo")
	if err != nil || repo.Latest.Summary.Verdict != report.VerdictBlocked {
		t.Fatalf("repo=%+v err=%v", repo, err)
	}
}

func TestFailedAttemptWithoutArtifactIsCachedPerVariant(t *testing.T) {
	r, st := testRunner(t, "/must-not-run")
	if err := st.PutRepo("user", &store.Repo{Key: "repo"}); err != nil {
		t.Fatal(err)
	}
	// The missing account fails before any network/CLI access.
	for _, variant := range []string{"normal", "plan"} {
		job := Job{UserKey: "user", RepoKey: "repo", Commit: strings.Repeat("a", 40), Variant: variant}
		r.process(context.Background(), job)
		rec, err := st.RecordVariant("user", "repo", job.Commit, variant)
		if err != nil || rec.Status != store.StatusFailed || rec.Error == "" || len(rec.Raw) != 0 {
			t.Fatalf("record=%+v err=%v", rec, err)
		}
	}
	runs, err := st.History("user", "repo", 0)
	if err != nil || len(runs) != 2 {
		t.Fatalf("history=%+v err=%v", runs, err)
	}
}
