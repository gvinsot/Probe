package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
	reports "github.com/gvinsot/Probe/app/internal/report"
)

// safeHarness is a fakeHarness that tolerates concurrent agents.
type safeHarness struct {
	mu    sync.Mutex
	calls []string
}

func (f *safeHarness) Call(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return json.RawMessage(`{"content":"observed"}`), nil
}

var agentPattern = regexp.MustCompile(`you are the "([a-z0-9-]+)" agent`)

// swarmProvider answers each agent from its system prompt: script maps an
// agent to the tool calls of its first turn; the second turn ends it.
type swarmProvider struct {
	mu        sync.Mutex
	turns     map[string]int
	tools     map[string][]string
	prompts   map[string]string
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	script    map[string][]toolCall
	fail      map[string]bool
}

func (p *swarmProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		old := p.maxFlight.Load()
		if now <= old || p.maxFlight.CompareAndSwap(old, now) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond) // let agents overlap
	var body struct {
		Messages []message         `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m := agentPattern.FindStringSubmatch(body.Messages[0].Content)
	name := "solo"
	if m != nil {
		name = m[1]
	}
	p.mu.Lock()
	p.turns[name]++
	turn := p.turns[name]
	var tools []string
	for _, t := range body.Tools {
		var d struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(t, &d)
		tools = append(tools, d.Function.Name)
	}
	p.tools[name] = tools
	p.prompts[name] = body.Messages[0].Content + "\n" + body.Messages[1].Content
	p.mu.Unlock()
	if p.fail[name] {
		http.Error(w, "provider unavailable", http.StatusInternalServerError)
		return
	}
	if turn == 1 && len(p.script[name]) > 0 {
		complete(w, p.script[name]...)
		return
	}
	m2 := message{Role: "assistant", Content: "Summary from " + name}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m2, "finish_reason": "stop"}}})
}

func newSwarmProvider(t *testing.T) (*swarmProvider, string) {
	p := &swarmProvider{turns: map[string]int{}, tools: map[string][]string{}, prompts: map[string]string{}, script: map[string][]toolCall{}, fail: map[string]bool{}}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return p, server.URL + "/v1"
}

func hypothesis(title, status, path string, line int) string {
	return fmt.Sprintf(`{"title":%q,"severity":"high","status":%q,"rationale":"because","evidence_ids":[],"path":%q,"line":%d}`, title, status, path, line)
}

func swarmReport() *model.Report {
	return &model.Report{
		Change:  model.Change{Files: []model.ChangedFile{{Path: "auth.go", Status: "M", Additions: 3}, {Path: "auth_test.go", Status: "M", Additions: 1}}},
		Signals: []model.Signal{{ID: "signal-1", Summary: "Sensitive path changed", Path: "auth.go", Severity: "high"}},
	}
}

func TestSwarmRunsSpecializedAgentsAndMergesDeterministically(t *testing.T) {
	p, endpoint := newSwarmProvider(t)
	p.script[AgentCorrectness] = []toolCall{
		call("c1", "read_file", `{"path":"auth.go"}`),
		call("c2", "submit_hypothesis", hypothesis("Admin check always passes", "UNVERIFIED", "auth.go", 3)),
	}
	p.script[AgentSecurity] = []toolCall{
		call("s1", "submit_hypothesis", strings.Replace(hypothesis("admin check  always passes", "REPRODUCED", "auth.go", 3), `"evidence_ids":[]`, `"evidence_ids":["invented"]`, 1)),
		call("s2", "submit_hypothesis", hypothesis("Token logged in clear", "UNVERIFIED", "auth.go", 9)),
	}
	p.script[AgentTests] = []toolCall{call("t1", "submit_hypothesis", hypothesis("No test covers non-admins", "UNVERIFIED", "auth_test.go", 1))}
	p.fail[AgentReliability] = true

	r := swarmReport()
	h := &safeHarness{}
	o := Options{Endpoint: endpoint, Model: "test", Swarm: &Swarm{MaxParallel: 2}, MaxIterations: 4}
	if err := Run(context.Background(), o, r, h); err != nil {
		t.Fatal(err)
	}
	// Default agents without acceptance criteria: no intent agent.
	var names []string
	for _, a := range r.ReviewerAgents {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "correctness,security,tests,compatibility,reliability" {
		t.Fatalf("agents %v", names)
	}
	if got := p.maxFlight.Load(); got > 2 || got < 2 {
		t.Fatalf("max concurrent provider requests %d, want 2", got)
	}
	// Only the first agent assesses signals.
	for name, tools := range p.tools {
		has := strings.Contains(strings.Join(tools, ","), AssessTool)
		if has != (name == AgentCorrectness) {
			t.Errorf("%s tools %v", name, tools)
		}
	}
	if !strings.Contains(p.prompts[AgentSecurity], "Your focus: vulnerabilities and trust boundaries") || !strings.Contains(p.prompts[AgentSecurity], "Investigate this change as the security agent.") {
		t.Fatalf("security prompt:\n%s", p.prompts[AgentSecurity])
	}

	// Duplicate finding kept once, strongest status, both agents credited.
	if len(r.Hypotheses) != 3 {
		t.Fatalf("hypotheses %+v", r.Hypotheses)
	}
	first := r.Hypotheses[0]
	if first.ID != "hypothesis-1" || first.Status != "REPRODUCED" || strings.Join(first.Agents, ",") != "correctness,security" || first.Title != "admin check  always passes" {
		t.Fatalf("merged finding %+v", first)
	}
	if r.Hypotheses[1].ID != "hypothesis-2" || r.Hypotheses[2].Agents[0] != AgentTests {
		t.Fatalf("order %+v", r.Hypotheses)
	}
	var reliability model.ReviewerAgent
	for _, a := range r.ReviewerAgents {
		if a.Name == AgentReliability {
			reliability = a
		}
	}
	if reliability.Status != model.AgentIncomplete || reliability.Note == "" {
		t.Fatalf("failed agent %+v", reliability)
	}
	if !strings.Contains(strings.Join(r.Unverified, "\n"), "Reviewer agent reliability incomplete") {
		t.Fatalf("unverified %v", r.Unverified)
	}
	if !strings.Contains(r.ReviewerSummary, "[correctness] Summary from correctness") || !strings.Contains(r.ReviewerSummary, "[tests] Summary from tests") {
		t.Fatalf("summary %q", r.ReviewerSummary)
	}
	tagged := 0
	for _, e := range r.Audit {
		if e.Agent == "" {
			t.Fatalf("untagged reviewer event %+v", e)
		}
		tagged++
	}
	if tagged == 0 || len(h.calls) != 1 {
		t.Fatalf("audit %d, harness calls %v", tagged, h.calls)
	}

	// Evidence validation is unchanged: the unsupported REPRODUCED claim is
	// downgraded, whichever agent made it.
	reports.Finalize(r, true)
	if r.Hypotheses[0].Status == "REPRODUCED" || len(r.ReproducedIssues) != 0 {
		t.Fatal("a swarm agent's claim became proof")
	}
	data, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(data), `"reviewer_agents"`) || !strings.Contains(string(data), `"agents":["correctness","security"]`) {
		t.Fatalf("json %s %v", data, err)
	}
}

func TestSwarmAllAgentsFailing(t *testing.T) {
	p, endpoint := newSwarmProvider(t)
	for _, n := range []string{AgentSecurity, AgentTests} {
		p.fail[n] = true
	}
	r := swarmReport()
	err := Run(context.Background(), Options{Endpoint: endpoint, Model: "test", Swarm: &Swarm{Agents: []string{AgentSecurity, AgentTests}}, MaxIterations: 2}, r, &safeHarness{})
	if err == nil || !strings.Contains(err.Error(), "security") || !strings.Contains(err.Error(), "tests") {
		t.Fatalf("err %v", err)
	}
	if len(r.ReviewerAgents) != 2 || r.ReviewerAgents[0].Status != model.AgentIncomplete {
		t.Fatalf("agents %+v", r.ReviewerAgents)
	}
}

func TestSwarmIntentAgentNeedsCriteria(t *testing.T) {
	_, endpoint := newSwarmProvider(t)
	r := swarmReport()
	if err := Run(context.Background(), Options{Endpoint: endpoint, Model: "test", Swarm: &Swarm{Agents: []string{AgentIntent, AgentSecurity, AgentTests}}, MaxIterations: 2}, r, &safeHarness{}); err != nil {
		t.Fatal(err)
	}
	if len(r.ReviewerAgents) != 2 || r.ReviewerAgents[0].Name != AgentSecurity || !strings.Contains(strings.Join(r.Unverified, "\n"), "intent agent did not run") {
		t.Fatalf("agents %+v unverified %v", r.ReviewerAgents, r.Unverified)
	}
	roles, _ := planSwarm(&Swarm{}, &model.Report{IntentCriteria: []model.IntentCriterion{{ID: "AC-1", Text: "x"}}})
	if roles[len(roles)-1].name != AgentIntent {
		t.Fatalf("default roles with criteria %+v", roles)
	}
}

func TestSwarmPartitionsLargeChanges(t *testing.T) {
	var files []model.ChangedFile
	for i := 0; i < 40; i++ {
		files = append(files, model.ChangedFile{Path: fmt.Sprintf("pkg%d/file%02d.go", i%5, i), Additions: i})
	}
	r := &model.Report{Change: model.Change{Files: files}}
	roles, _ := planSwarm(&Swarm{Agents: []string{AgentCorrectness, AgentSecurity}, PartitionFiles: 15}, r)
	var covered []string
	parts := 0
	for _, role := range roles {
		if strings.HasPrefix(role.name, "correctness-") {
			parts++
			if role.parts != 3 || len(role.paths) == 0 {
				t.Fatalf("role %+v", role)
			}
			covered = append(covered, role.paths...)
		}
	}
	if parts != 3 || len(roles) != 4 || roles[3].name != AgentSecurity || !roles[0].assess || roles[1].assess {
		t.Fatalf("roles %+v", roles)
	}
	sort.Strings(covered)
	if len(covered) != 40 || covered[0] != "pkg0/file00.go" {
		t.Fatalf("files covered %d", len(covered))
	}
	for i := 1; i < len(covered); i++ {
		if covered[i] == covered[i-1] {
			t.Fatalf("file %s owned twice", covered[i])
		}
	}
	if got := partition(files, 0); got != nil {
		t.Fatal("partition_files 0 must not split")
	}
	if got := partition(files[:10], 15); got != nil {
		t.Fatal("small change split")
	}
	if got := partition(files, 2); len(got) != MaxPartitions {
		t.Fatalf("partitions %d", len(got))
	}
}

func TestSwarmPartitionedAgentReceivesOnlyItsHunks(t *testing.T) {
	p, endpoint := newSwarmProvider(t)
	var files []model.ChangedFile
	for i := 0; i < 4; i++ {
		files = append(files, model.ChangedFile{Path: fmt.Sprintf("f%d.go", i), Additions: 1, Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "add", NewLine: 1, Content: fmt.Sprintf("marker-%d", i)}}}}})
	}
	r := &model.Report{Change: model.Change{Files: files}}
	o := Options{Endpoint: endpoint, Model: "test", Swarm: &Swarm{Agents: []string{AgentCorrectness}, PartitionFiles: 2}, MaxIterations: 2}
	if err := Run(context.Background(), o, r, &safeHarness{}); err != nil {
		t.Fatal(err)
	}
	one := p.prompts["correctness-1"]
	if !strings.Contains(one, "marker-0") || strings.Contains(one, "marker-3") || !strings.Contains(one, "You own part 1 of 2") {
		t.Fatalf("partition input:\n%s", one)
	}
}

func TestValidateSwarm(t *testing.T) {
	for _, s := range []*Swarm{
		{Agents: []string{"poet"}},
		{Agents: []string{AgentTests, AgentTests}},
		{MaxParallel: 9},
		{PartitionFiles: -1},
	} {
		if ValidateSwarm(s) == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	if ValidateSwarm(nil) != nil || ValidateSwarm(&Swarm{Agents: SpecializationNames()}) != nil {
		t.Fatal("valid swarm refused")
	}
}
