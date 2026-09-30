package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

func TestResolveSwarm(t *testing.T) {
	flags := func(enabled bool, agents string) swarmFlags { return swarmFlags{enabled: &enabled, agents: &agents} }
	two := 2
	policy := &config.Swarm{Agents: []string{"security"}, MaxParallel: 4, PartitionFiles: &two}

	if s, err := resolveSwarm("review", map[string]bool{}, flags(false, ""), nil, true); err != nil || s != nil {
		t.Fatalf("no swarm by default: %+v %v", s, err)
	}
	s, err := resolveSwarm("review", map[string]bool{}, flags(false, ""), policy, true)
	if err != nil || s == nil || s.Agents[0] != "security" || s.MaxParallel != 4 || s.PartitionFiles != 2 {
		t.Fatalf("policy swarm: %+v %v", s, err)
	}
	if s, err := resolveSwarm("review", map[string]bool{"swarm": true}, flags(false, ""), policy, true); err != nil || s != nil {
		t.Fatalf("--swarm=false: %+v %v", s, err)
	}
	s, err = resolveSwarm("review", map[string]bool{"swarm": true}, flags(true, ""), nil, true)
	if err != nil || s == nil || len(s.Agents) != 0 || s.PartitionFiles != reviewer.DefaultPartitionFiles {
		t.Fatalf("--swarm: %+v %v", s, err)
	}
	s, err = resolveSwarm("review", map[string]bool{"swarm-agents": true}, flags(false, "tests, security"), policy, true)
	if err != nil || strings.Join(s.Agents, ",") != "tests,security" || s.MaxParallel != 4 {
		t.Fatalf("--swarm-agents: %+v %v", s, err)
	}
	for name, c := range map[string]struct {
		mode     string
		explicit map[string]bool
		flags    swarmFlags
		reviewer bool
	}{
		"lint":           {"lint", map[string]bool{"swarm": true}, flags(true, ""), true},
		"unknown agent":  {"review", map[string]bool{"swarm-agents": true}, flags(false, "poet"), true},
		"empty list":     {"review", map[string]bool{"swarm-agents": true}, flags(false, " , "), true},
		"false and list": {"review", map[string]bool{"swarm": true, "swarm-agents": true}, flags(false, "tests"), true},
		"reviewer=false": {"review", map[string]bool{"swarm": true, "reviewer": true}, flags(true, ""), false},
	} {
		if _, err := resolveSwarm(c.mode, c.explicit, c.flags, nil, c.reviewer); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReadOnlyReviewWithSwarm(t *testing.T) {
	forbidExecution(t)
	dir := fixture(t)
	pattern := regexp.MustCompile(`you are the "([a-z0-9-]+)" agent`)
	var mu sync.Mutex
	agents := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		name := "solo"
		if m := pattern.FindStringSubmatch(request.Messages[0].Content); m != nil {
			name = m[1]
		}
		mu.Lock()
		agents[name]++
		turn := agents[name]
		mu.Unlock()
		if name == "security" && turn == 1 {
			w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"h","type":"function","function":{"name":"submit_hypothesis","arguments":"{\"title\":\"Every user is allowed\",\"severity\":\"critical\",\"status\":\"UNVERIFIED\",\"rationale\":\"Allowed returns true\",\"evidence_ids\":[],\"path\":\"auth.go\",\"line\":3}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Done by ` + name + `"}}]}`))
	}))
	defer server.Close()
	t.Setenv(config.EndpointEnv, server.URL)
	t.Setenv(config.ModelEnv, "deployment-model")
	t.Setenv("PROBE_API_KEY", "deployment-key")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	var output bytes.Buffer
	code := Run(context.Background(), []string{"review", "--read-only", "--swarm-agents", "security,tests", "--repo", dir, "--out", "report", "--ci"}, &output, &output, "test")
	if code != 2 {
		t.Fatalf("exit=%d: %s", code, output.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if agents["security"] < 2 || agents["tests"] < 1 || agents["solo"] != 2 {
		t.Fatalf("agent requests %v", agents)
	}
	r := readReviewerReport(t, dir)
	if len(r.ReviewerAgents) != 2 || r.ReviewerAgents[0].Name != "security" || len(r.Hypotheses) != 1 || r.Hypotheses[0].Agents[0] != "security" {
		t.Fatalf("report agents %+v hypotheses %+v", r.ReviewerAgents, r.Hypotheses)
	}
	md, err := os.ReadFile(filepath.Join(dir, "report", "CONFIDENCE_REPORT.md"))
	if err != nil || !strings.Contains(string(md), "## Reviewer Swarm") || !strings.Contains(string(md), "_(agents: security)_") {
		t.Fatalf("markdown %v:\n%s", err, md)
	}
}
