package reviewer

// The reviewer swarm: several specialized agents investigate one change in
// parallel. Each agent is the ordinary bounded reviewer (same tools, same
// budgets, same untrusted-data rules) with one focus: correctness, security,
// tests, compatibility, reliability or the intent. When the change is large,
// the correctness agent is split into agents that each own part of the
// changed files. The first agent also assesses the linter signals and
// proposes knowledge, so that this work is done once.
//
// Agents share the harness, so their experiments are serialized by it and
// draw on the same generated-test budget, and every evidence ID stays unique.
// Merging is deterministic and uses no model: hypotheses are gathered in
// agent order, a finding several agents submitted is kept once (the one with
// the strongest status) and credits all of them, and IDs are renumbered.
// Package report then validates every hypothesis against the evidence, as for
// a single reviewer: a swarm adds investigation breadth, never authority.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// Swarm configures a reviewer swarm.
type Swarm struct {
	// Agents names the specializations to run, in order; empty runs the
	// default set (every specialization; intent only with acceptance
	// criteria).
	Agents []string
	// MaxParallel bounds the agents investigating at the same time (1..8).
	MaxParallel int
	// PartitionFiles splits the correctness agent into one agent per group of
	// about this many changed files, at most MaxPartitions; 0 never splits.
	PartitionFiles int
}

// Swarm bounds and defaults.
const (
	DefaultSwarmParallel  = 3
	MaxSwarmParallel      = 8
	DefaultPartitionFiles = 15
	MaxPartitions         = 4
	MaxSwarmAgents        = 12
	maxSwarmSummaryBytes  = 12000
	maxAgentPathsRecorded = 2000
)

// Specialization names.
const (
	AgentCorrectness   = "correctness"
	AgentSecurity      = "security"
	AgentTests         = "tests"
	AgentCompatibility = "compatibility"
	AgentReliability   = "reliability"
	AgentIntent        = "intent"
)

// specialization is one focus an agent can take.
type specialization struct {
	name, focus, prompt string
}

var specializations = []specialization{
	{AgentCorrectness, "logic errors, edge cases and regressions of existing behavior",
		"Look for logic errors in the changed code: wrong conditions and operators, off-by-one errors, broken invariants, unhandled edge cases (empty, nil or zero values, boundaries, large inputs, Unicode), wrong state transitions, and regressions of behavior that existed before the change."},
	{AgentSecurity, "vulnerabilities and trust boundaries",
		"Look for security defects the change introduces or exposes: injection (SQL, shell, path, template, header), authentication or authorization bypasses, missing validation of untrusted input, secrets or personal data exposed in code, logs or errors, unsafe deserialization, server-side request forgery, weak or misused cryptography, and permissive defaults."},
	{AgentTests, "whether the tests exercise the changed behavior",
		"Examine whether the tests exercise the changed behavior: changed code that no test reaches, assertions that were weakened, deleted or made unconditional, tests that cannot fail, mocks that hide the changed code, and missing negative cases. Where the riskiest changed behavior is untested and your tools allow it, write a differential test for it."},
	{AgentCompatibility, "contracts with callers, other components and other repositories",
		"Examine the contracts the change touches: exported functions, types and signatures, the callers of changed code in unchanged files, serialized formats, configuration keys, database schemas and migrations, public APIs, and the contracts of other components or repositories. Use the impact, graph and context tools to find the code that depends on what changed."},
	{AgentReliability, "error handling, concurrency, resources and performance",
		"Look for reliability defects: errors that are ignored, swallowed or wrapped wrongly, missing cleanup, data races, deadlocks and leaked goroutines, threads or promises, resource leaks (files, connections, locks), missing or wrong timeouts and retries, and performance regressions (repeated queries, quadratic loops, unbounded memory or output)."},
	{AgentIntent, "whether the change does what its intent asks",
		"Compare the change with its intent and acceptance criteria: criteria that are not implemented, implemented differently, or only partly, and behavior the change adds that the intent does not ask for. Where your tools offer intent tests, use them to check a criterion on the candidate."},
}

// SpecializationNames lists the valid agent names, in default order.
func SpecializationNames() []string {
	names := make([]string, len(specializations))
	for i, s := range specializations {
		names[i] = s.name
	}
	return names
}

func findSpecialization(name string) (specialization, bool) {
	for _, s := range specializations {
		if s.name == name {
			return s, true
		}
	}
	return specialization{}, false
}

// ValidateSwarm checks a swarm configuration without network access.
func ValidateSwarm(s *Swarm) error {
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, name := range s.Agents {
		if _, ok := findSpecialization(name); !ok {
			return fmt.Errorf("unknown reviewer swarm agent %q (known: %s)", name, strings.Join(SpecializationNames(), ", "))
		}
		if seen[name] {
			return fmt.Errorf("reviewer swarm agent %q is listed twice", name)
		}
		seen[name] = true
	}
	if s.MaxParallel < 0 || s.MaxParallel > MaxSwarmParallel {
		return fmt.Errorf("reviewer swarm max_parallel must be between 1 and %d", MaxSwarmParallel)
	}
	if s.PartitionFiles < 0 || s.PartitionFiles > 1000 {
		return errors.New("reviewer swarm partition_files must be between 0 and 1000")
	}
	return nil
}

// agentRole narrows one investigation.
type agentRole struct {
	name, focus, instructions string
	paths                     []string // owned changed files; empty: all
	part, parts               int      // 1-based partition, of parts; 0 when not split
	assess, knowledge         bool
	swarm                     []string // the names of every agent, for the prompt
}

// soloRole is the single reviewer.
var soloRole = agentRole{assess: true, knowledge: true}

func (a agentRole) prompt() string {
	if a.name == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nReview swarm: you are the %q agent of a review swarm in which specialized agents investigate this change in parallel (%s). Your focus: %s. %s", a.name, strings.Join(a.swarm, ", "), a.focus, a.instructions)
	if len(a.paths) > 0 {
		fmt.Fprintf(&b, " You own part %d of %d of the changed files: the input includes the hunks of your files only (%s); other agents with your focus own the rest. Read other files when you need them.", a.part, a.parts, strings.Join(a.paths, ", "))
	}
	b.WriteString(" Other agents cover the other focuses: submit a finding outside your focus only when it is severe. Everything else in these instructions applies unchanged, including evidence requirements.")
	if !a.assess {
		b.WriteString(" Another agent assesses the linter signals; you do not.")
	}
	b.WriteString(" End with a brief summary of what you found within your focus.")
	return b.String()
}

func (a agentRole) request() string {
	if a.name == "" {
		return "Investigate this change."
	}
	return fmt.Sprintf("Investigate this change as the %s agent.", a.name)
}

// planSwarm returns the agents to run for a report.
func planSwarm(s *Swarm, r *model.Report) ([]agentRole, []string) {
	names := s.Agents
	var notes []string
	if len(names) == 0 {
		names = SpecializationNames()
		if len(r.IntentCriteria) == 0 {
			names = names[:len(names)-1] // intent needs acceptance criteria
		}
	} else if len(r.IntentCriteria) == 0 {
		for i, n := range names {
			if n == AgentIntent {
				names = append(append([]string{}, names[:i]...), names[i+1:]...)
				notes = append(notes, "Reviewer swarm: the intent agent did not run because the intent yielded no acceptance criteria.")
				break
			}
		}
	}
	var roles []agentRole
	for _, name := range names {
		spec, _ := findSpecialization(name)
		role := agentRole{name: spec.name, focus: spec.focus, instructions: spec.prompt}
		groups := partition(r.Change.Files, s.PartitionFiles)
		if name == AgentCorrectness && len(groups) > 1 {
			for i, g := range groups {
				part := role
				part.name = fmt.Sprintf("%s-%d", name, i+1)
				part.paths, part.part, part.parts = g, i+1, len(groups)
				roles = append(roles, part)
			}
			continue
		}
		roles = append(roles, role)
	}
	if len(roles) > MaxSwarmAgents {
		roles = roles[:MaxSwarmAgents]
	}
	all := make([]string, len(roles))
	for i := range roles {
		all[i] = roles[i].name
	}
	for i := range roles {
		roles[i].swarm = all
	}
	if len(roles) > 0 {
		roles[0].assess, roles[0].knowledge = true, true
	}
	return roles, notes
}

// partition splits the changed files, sorted by path so that a directory
// stays together, into consecutive groups of about the same number of
// changed lines: one group per perFile files, at most MaxPartitions. It
// returns nil when the change is not split.
func partition(files []model.ChangedFile, perFile int) [][]string {
	if perFile <= 0 || len(files) <= perFile {
		return nil
	}
	groups := (len(files) + perFile - 1) / perFile
	if groups > MaxPartitions {
		groups = MaxPartitions
	}
	sorted := append([]model.ChangedFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	weight := func(f model.ChangedFile) int { return f.Additions + f.Deletions + 1 }
	total := 0
	for _, f := range sorted {
		total += weight(f)
	}
	out := make([][]string, 0, groups)
	var current []string
	acc, target := 0, total/groups
	for i, f := range sorted {
		current = append(current, f.Path)
		acc += weight(f)
		remainingFiles := len(sorted) - i - 1
		remainingGroups := groups - len(out) - 1
		if remainingGroups > 0 && (acc >= target*(len(out)+1) || remainingFiles == remainingGroups) {
			out = append(out, current)
			current = nil
		}
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

// agentResult is what one agent produced.
type agentResult struct {
	role   agentRole
	report *model.Report
	err    error
}

// fork returns the report an agent works on: the shared inputs, with the
// outputs it writes emptied.
func fork(r *model.Report) *model.Report {
	sub := *r
	sub.Hypotheses = nil
	sub.Audit = nil
	sub.Unverified = nil
	sub.ReviewerSummary = ""
	sub.ReviewerAgents = nil
	sub.SignalAssessments = append([]model.SignalAssessment(nil), r.SignalAssessments...)
	if r.Knowledge != nil {
		// The agent that proposes knowledge appends to its own copy while the
		// others read theirs.
		k := *r.Knowledge
		k.Entries = append([]model.KnowledgeEntry(nil), r.Knowledge.Entries...)
		k.Updates = append([]model.KnowledgeUpdate(nil), r.Knowledge.Updates...)
		sub.Knowledge = &k
	}
	return &sub
}

// lockedHarness serializes tool calls; the harness already does, but a
// harness supplied by a caller may not.
type lockedHarness struct {
	mu sync.Mutex
	h  toolHarness
}

func (l *lockedHarness) Call(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.h.Call(ctx, tool, args)
}

func runSwarm(ctx context.Context, o Options, r *model.Report, h toolHarness) error {
	s := *o.Swarm
	if err := ValidateSwarm(&s); err != nil {
		return err
	}
	if _, _, err := normalize(o); err != nil {
		return err
	}
	if s.MaxParallel == 0 {
		s.MaxParallel = DefaultSwarmParallel
	}
	roles, notes := planSwarm(&s, r)
	r.Unverified = append(r.Unverified, notes...)
	if len(roles) == 0 {
		return errors.New("reviewer swarm has no agent to run")
	}
	if len(roles) == 1 {
		// One agent is a single reviewer with a focus.
		roles[0].swarm = nil
	}
	shared := &lockedHarness{h: h}
	results := make([]agentResult, len(roles))
	limit := make(chan struct{}, s.MaxParallel)
	var wg sync.WaitGroup
	for i, role := range roles {
		wg.Add(1)
		go func(i int, role agentRole) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				results[i] = agentResult{role: role, report: fork(r), err: ctx.Err()}
				return
			}
			defer func() { <-limit }()
			sub := fork(r)
			err := run(ctx, o, sub, shared, role)
			results[i] = agentResult{role: role, report: sub, err: err}
		}(i, role)
	}
	wg.Wait()
	return merge(r, results)
}

// statusStrength orders hypothesis statuses by the evidence they carry, to
// keep the strongest of duplicate findings.
var statusStrength = map[string]int{
	model.StatusReproduced:       6,
	model.StatusIntentTestFailed: 5,
	model.StatusDiverged:         4,
	model.StatusNotReproduced:    3,
	model.StatusDismissed:        2,
	model.StatusUnverified:       1,
}

func findingKey(h model.Hypothesis) string {
	return strings.ToLower(h.Path) + "\x00" + fmt.Sprint(h.Line) + "\x00" + strings.Join(strings.Fields(strings.ToLower(h.Title)), " ")
}

// merge folds the agents' work into r, in agent order.
func merge(r *model.Report, results []agentResult) error {
	index := map[string]int{} // finding key -> index in r.Hypotheses
	for i, h := range r.Hypotheses {
		index[findingKey(h)] = i
	}
	var summaries []string
	failed := 0
	var errs []error
	for _, res := range results {
		sub, role := res.report, res.role
		record := model.ReviewerAgent{Name: role.name, Focus: role.focus, Status: model.AgentCompleted, Hypotheses: len(sub.Hypotheses)}
		if len(role.paths) > 0 {
			record.Paths = role.paths
			if len(record.Paths) > maxAgentPathsRecorded {
				record.Paths = record.Paths[:maxAgentPathsRecorded]
			}
		}
		for _, h := range sub.Hypotheses {
			key := findingKey(h)
			if at, dup := index[key]; dup {
				kept := &r.Hypotheses[at]
				if statusStrength[h.Status] > statusStrength[kept.Status] {
					agents, id := kept.Agents, kept.ID
					*kept = h
					kept.ID, kept.Agents = id, agents
				}
				if !contains(kept.Agents, role.name) && role.name != "" {
					kept.Agents = append(kept.Agents, role.name)
				}
				continue
			}
			h.ID = fmt.Sprintf("hypothesis-%d", len(r.Hypotheses)+1)
			if role.name != "" {
				h.Agents = []string{role.name}
			}
			index[key] = len(r.Hypotheses)
			r.Hypotheses = append(r.Hypotheses, h)
		}
		for _, e := range sub.Audit {
			e.Agent = role.name
			r.Audit = append(r.Audit, e)
		}
		for _, note := range sub.Unverified {
			if role.name != "" {
				note = "Reviewer agent " + role.name + ": " + note
			}
			r.Unverified = append(r.Unverified, note)
			if strings.Contains(note, "incomplete") {
				record.Status = model.AgentIncomplete
			}
		}
		if role.assess {
			r.SignalAssessments = sub.SignalAssessments
		}
		if role.knowledge && r.Knowledge != nil && sub.Knowledge != nil {
			r.Knowledge.Updates = sub.Knowledge.Updates
		}
		if res.err != nil {
			failed++
			errs = append(errs, fmt.Errorf("%s: %w", role.name, res.err))
			record.Status, record.Note = model.AgentIncomplete, redact.TruncateUTF8(res.err.Error(), 500)
			r.Unverified = append(r.Unverified, "Reviewer agent "+role.name+" incomplete: "+res.err.Error())
		}
		if summary := strings.TrimSpace(sub.ReviewerSummary); summary != "" {
			if role.name != "" {
				summary = "[" + role.name + "] " + summary
			}
			summaries = append(summaries, summary)
		}
		if role.name != "" {
			r.ReviewerAgents = append(r.ReviewerAgents, record)
		}
	}
	if len(r.ReviewerAgents) < 2 {
		r.ReviewerAgents = nil
	}
	if len(summaries) > 0 {
		r.ReviewerSummary = redact.TruncateUTF8(strings.Join(summaries, "\n\n"), maxSwarmSummaryBytes)
	}
	if failed == len(results) {
		return errors.Join(errs...)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
