package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
)

// contextRepoFixture commits a context repository at dir.
func contextRepoFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	write(t, dir, "api/auth.go", "package api\n\n// Allowed is the contract clients call.\nfunc Allowed(user string) bool\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "contract")
}

func contextPolicyFile(t *testing.T, endpoint string, context *config.Context) string {
	t.Helper()
	cfg := config.Default("go")
	cfg.Reviewer.Endpoint, cfg.Reviewer.Model = endpoint, "test-model"
	cfg.Reviewer.APIKeyEnv = "PROBE_TEST_REVIEWER_KEY"
	t.Setenv(cfg.Reviewer.APIKeyEnv, "test-reviewer-key")
	cfg.Context = context
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	return policy
}

const contextSearchCall = `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"context_search","arguments":"{\"query\":\"func Allowed\"}"}}]}}]}`

// A review reads the context repositories the policy declares, directly or
// through a cluster, and records them; context check shows the resolution.
func TestReviewReadsCrossRepositoryContext(t *testing.T) {
	dir := fixture(t)
	root := t.TempDir()
	contextRepoFixture(t, filepath.Join(root, "company", "auth-api"))
	server, inputs := knowledgeProvider(t, contextSearchCall)
	policy := contextPolicyFile(t, server.URL, &config.Context{
		Repos:              []config.ContextRepo{{Name: "company/auth-api", Role: "auth contract"}},
		Clusters:           []string{"auth"},
		ClusterDefinitions: map[string]config.Cluster{"auth": {Description: "Auth service and clients", Repos: []config.ContextRepo{{Name: "company/auth-api"}, {Name: "company/web-client"}}}},
	})
	var out bytes.Buffer
	args := []string{"review", "--repo", dir, "--config", policy, "--checks=false", "--out", "report", "--context-dir", root, "--knowledge", "none"}
	if code := Run(context.Background(), args, &out, &out, "test"); code != 0 && code != 2 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Cross-repository context: 1 of 2 repositories available.") || !strings.Contains(out.String(), "company/web-client unavailable") {
		t.Fatalf("run log: %s", out.String())
	}
	if !strings.Contains((*inputs)[0], `"context_repos":[{"name":"company/auth-api","role":"auth contract","clusters":["auth"]`) {
		t.Fatalf("reviewer input %s", (*inputs)[0])
	}
	r := readReviewerReport(t, dir)
	if len(r.ContextRepos) != 2 || r.ContextRepos[0].Status != "available" || r.ContextRepos[0].Files != 1 || r.ContextRepos[1].Status != "unavailable" {
		t.Fatalf("recorded context %+v", r.ContextRepos)
	}
	var searched bool
	for _, e := range r.Audit {
		if e.Tool == "context_search" && e.Status == "OK" {
			searched = true
		}
	}
	if !searched {
		t.Fatalf("the context search is audited: %+v", r.Audit)
	}
	markdown, _ := os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if !strings.Contains(string(markdown), "company/auth-api (cluster auth)") || !strings.Contains(string(markdown), "company/web-client (cluster auth): unavailable") {
		t.Fatalf("markdown lacks the context section:\n%s", markdown)
	}

	// --context=false disables it.
	out.Reset()
	Run(context.Background(), append(args, "--context=false"), &out, &out, "test")
	if r := readReviewerReport(t, dir); r.ContextRepos != nil {
		t.Fatal("--context=false still read the context")
	}

	// context check resolves the same way, exit 1 while a repository is missing.
	out.Reset()
	if code := Run(context.Background(), []string{"context", "check", "--repo", dir, "--config", policy, "--context-dir", root}, &out, &out, "test"); code != 1 || !strings.Contains(out.String(), "available    company/auth-api (cluster auth)") || !strings.Contains(out.String(), "unavailable  company/web-client") {
		t.Fatalf("check exit %d: %s", code, out.String())
	}
	webDir := filepath.Join(t.TempDir(), "web")
	contextRepoFixture(t, webDir)
	out.Reset()
	if code := Run(context.Background(), []string{"context", "check", "--repo", dir, "--config", policy, "--context-dir", root, "--context-repo", "company/web-client=" + webDir}, &out, &out, "test"); code != 0 {
		t.Fatalf("check with every repository: %d %s", code, out.String())
	}
}

// A cluster listing the reviewed repository does not make it its own context.
func TestContextLeavesTheRepositoryItselfOut(t *testing.T) {
	dir := fixture(t)
	git(t, dir, "remote", "add", "origin", "git@github.com:company/fixture.git")
	if got := selfName(context.Background(), dir); got != "company/fixture" {
		t.Fatalf("self name %q", got)
	}
	policy := contextPolicyFile(t, "https://unused.invalid", &config.Context{Clusters: []string{"all"}, ClusterDefinitions: map[string]config.Cluster{"all": {Repos: []config.ContextRepo{{Name: "company/fixture"}, {Name: "company/other"}}}}})
	var out bytes.Buffer
	Run(context.Background(), []string{"context", "check", "--repo", dir, "--config", policy}, &out, &out, "test")
	if strings.Contains(out.String(), "company/fixture") || !strings.Contains(out.String(), "company/other") {
		t.Fatalf("check: %s", out.String())
	}
	if code := Run(context.Background(), []string{"context", "check", "--repo", dir, "--config", policy, "--clusters", filepath.Join(t.TempDir(), "absent.json")}, &out, &out, "test"); code != 3 {
		t.Fatalf("missing clusters file: %d", code)
	}
}
