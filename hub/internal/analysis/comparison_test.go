package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/hub/internal/accounts"
	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/forge"
	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// The embedded interface catches accidental extra forge calls in this fixture.
type gitOnlyProvider struct{ forge.Provider }

func (gitOnlyProvider) GitAuthHeader(forge.Token) string { return "" }

func TestAnalysisVariantsFetchGitAndKeepBothResults(t *testing.T) {
	t.Setenv(config.EndpointEnvName, "https://provider.example/v1")
	t.Setenv(config.ModelEnvName, "test-model")
	g, commits := repoWithCommits(t, 2)
	// A committed report must never stand in for output from the trusted CLI.
	if err := os.MkdirAll(filepath.Join(g.dir, ".probe"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, reportPath), []byte(fakeReport), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), "add", ".probe"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), "commit", "-m", "Implement requested behavior"); err != nil {
		t.Fatal(err)
	}
	head, err := g.run(context.Background(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + g.dir, "GIT_HTTP_EXPORT_ALL=1"}})
	defer remote.Close()
	logPath := filepath.Join(t.TempDir(), "calls")
	binary := fakeCLI(t, `
printf '%s\n' "$*" >> '`+logPath+`'
if [ "$1" = plan ]; then
 test ! -e .probe/confidence-report.json || exit 4
 mkdir -p .probe
 printf '%s' '{"format":"probe-plan","version":1,"base_commit":"`+commits[1]+`","exit_code":2,"proposal":{"summary":"generated"}}' > .probe/PLAN.json
 exit 2
fi
case "$*" in
 *--plan*) test -s .probe/PLAN.json || exit 4;;
esac
mkdir -p .probe
cat > .probe/confidence-report.json <<'JSON'
`+fakeReport+`
JSON
exit 2
`)
	runner, st := testRunner(t, binary)
	keys, err := secrets.New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.Seal("access")
	if err != nil {
		t.Fatal(err)
	}
	user := &store.User{Key: "user", Provider: "github", Token: token}
	if err := st.PutUser(user); err != nil {
		t.Fatal(err)
	}
	repo := &store.Repo{Key: "repo", Provider: "github", DefaultBranch: "main", CloneURL: remote.URL + "/.git"}
	if err := st.PutRepo(user.Key, repo); err != nil {
		t.Fatal(err)
	}
	runner.accounts = accounts.New(st, keys, map[string]forge.Provider{"github": gitOnlyProvider{}})
	graph, err := runner.Graph(context.Background(), user.Key, repo.Key)
	if err != nil || len(graph.Commits) != 3 {
		t.Fatalf("remote graph: %+v %v", graph, err)
	}
	job := Job{UserKey: user.Key, RepoKey: repo.Key, Commit: head, Trigger: TriggerManual, Variant: "normal"}
	runner.process(context.Background(), job)
	job.Variant = "plan"
	job.Intent = "Implement requested behavior"
	runner.process(context.Background(), job)
	for _, variant := range []string{"normal", "plan"} {
		rec, err := st.RecordVariant(user.Key, repo.Key, head, variant)
		if err != nil || rec.Status != store.StatusDone || rec.BaseCommit != commits[1] {
			t.Fatalf("%s result: %+v %v", variant, rec, err)
		}
		if strings.Contains(string(rec.Raw), "probe-plan") != (variant == "plan") {
			t.Fatalf("wrong plan in %s", variant)
		}
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "lint ") || !strings.HasPrefix(lines[1], "plan ") {
		t.Fatalf("CLI sequence: %s", calls)
	}
	if !strings.Contains(lines[1], "--base "+commits[1]) || !strings.Contains(lines[1], "--intent Implement requested behavior") {
		t.Fatalf("incorrect plan base/intent: %s", calls)
	}
	// Missing provider or failed planner is cached as a failure, never as clear.
	runner.cfg.Binary = fakeCLI(t, "echo 'plan needs a provider'\nexit 3\n")
	runner.process(context.Background(), job)
	failed, err := st.RecordVariant(user.Key, repo.Key, head, "plan")
	if err != nil || failed.Status != store.StatusFailed || !strings.Contains(failed.Error, "plan needs a provider") || len(failed.Raw) != 0 {
		t.Fatalf("planner failure: %+v %v", failed, err)
	}
	normal, err := st.RecordVariant(user.Key, repo.Key, head, "normal")
	if err != nil || !json.Valid(normal.Raw) {
		t.Fatalf("normal cache overwritten: %+v %v", normal, err)
	}
	latest, err := st.Repo(user.Key, repo.Key)
	if err != nil || latest.Latest.Variant != "normal" {
		t.Fatalf("experimental plan changed public latest: %+v %v", latest, err)
	}
}
