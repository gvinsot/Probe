package server

// The MCP tools. Each one resolves the repository and commit an agent names,
// then calls the same API route the dashboard calls, under the token's
// account (see handleMCP). Read tools never change state; write tools need a
// write token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

type toolArgs map[string]any

// toolInputError is a mistake in the agent's arguments, reported as a tool
// error so the agent can correct itself.
type toolInputError struct{ message string }

func (e *toolInputError) Error() string { return e.message }

func inputErrorf(format string, args ...any) error {
	return &toolInputError{message: fmt.Sprintf(format, args...)}
}

func (a toolArgs) str(name string) (string, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", inputErrorf("%s must be a string", name)
	}
	return strings.TrimSpace(s), nil
}

func (a toolArgs) required(name string) (string, error) {
	s, err := a.str(name)
	if err == nil && s == "" {
		err = inputErrorf("%s is required", name)
	}
	return s, err
}

func (a toolArgs) boolean(name string, fallback bool) (bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return fallback, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, inputErrorf("%s must be true or false", name)
	}
	return b, nil
}

func (a toolArgs) integer(name string, fallback, min, max int) (int, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return fallback, nil
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) || int(f) < min || int(f) > max {
		return 0, inputErrorf("%s must be an integer from %d to %d", name, min, max)
	}
	return int(f), nil
}

func (a toolArgs) variant() (string, error) {
	v, err := a.str("variant")
	if err != nil {
		return "", err
	}
	if v == "" {
		return "normal", nil
	}
	if v != "normal" && v != "plan" {
		return "", inputErrorf(`variant must be "normal" or "plan"`)
	}
	return v, nil
}

// mcpTool describes one tool.
type mcpTool struct {
	name, title, description string
	write                    bool // needs a write token
	destructive, idempotent  bool
	properties               map[string]any
	requiredArgs             []string
	run                      func(s *Server, ctx context.Context, p *agentPrincipal, args toolArgs) (any, error)
}

func (t mcpTool) describe() map[string]any {
	schema := map[string]any{"type": "object", "properties": t.properties, "additionalProperties": false}
	if t.properties == nil {
		schema["properties"] = map[string]any{}
	}
	if len(t.requiredArgs) > 0 {
		schema["required"] = t.requiredArgs
	}
	return map[string]any{
		"name": t.name, "title": t.title, "description": t.description, "inputSchema": schema,
		"annotations": map[string]any{
			"title": t.title, "readOnlyHint": !t.write, "destructiveHint": t.destructive,
			"idempotentHint": t.idempotent || !t.write, "openWorldHint": false,
		},
	}
}

func findTool(name string) (mcpTool, bool) {
	for _, t := range mcpTools {
		if t.name == name {
			return t, true
		}
	}
	return mcpTool{}, false
}

// Common argument schemas.
var (
	argRepo    = map[string]any{"type": "string", "description": `Repository: "owner/name" as on the forge, its bare name when unique, or the hub key.`}
	argCommit  = map[string]any{"type": "string", "description": "Commit SHA (a unique prefix of at least 7 characters is enough for an analyzed commit). Defaults to the latest analyzed commit."}
	argVariant = map[string]any{"type": "string", "enum": []string{"normal", "plan"}, "description": `"normal" (the review, default) or "plan" (a pre-change plan).`}
)

// mcpTools is filled by init: the tools call the router, which serves the MCP
// endpoint, which lists the tools.
var mcpTools []mcpTool

func init() { mcpTools = mcpToolList() }

func mcpToolList() []mcpTool {
	return []mcpTool{
		{
			name: "list_repositories", title: "List repositories",
			description: "Lists the repositories of the account with their policy and monitoring state and the latest analysis. Use it to find the repository name other tools take.",
			properties: map[string]any{
				"query":          map[string]any{"type": "string", "description": "Case-insensitive filter on the repository name."},
				"monitored_only": map[string]any{"type": "boolean", "description": "Only repositories whose pushes are analyzed."},
			},
			run: toolListRepositories,
		},
		{
			name: "get_repository", title: "Get repository",
			description: "Returns one repository: default branch, policy, monitoring, latest analysis, and its review context (team coding rules, whether learning from feedback is on, feedback count).",
			properties:  map[string]any{"repo": argRepo}, requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				return s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key, nil, nil)
			},
		},
		{
			name: "list_analyses", title: "List analyses",
			description: "Lists the stored analyses of a repository, newest first: commit, branch, status, trigger, variant and verdict summary.",
			properties: map[string]any{
				"repo":  argRepo,
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": store.MaxHistory, "description": "At most this many analyses (default 20)."},
			},
			requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				limit, err := a.integer("limit", 20, 1, store.MaxHistory)
				if err != nil {
					return nil, err
				}
				out, err := s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/runs", url.Values{"limit": {fmt.Sprint(limit)}}, nil)
				if err != nil {
					return nil, err
				}
				return map[string]any{"repository": repo.FullName, "runs": out["runs"]}, nil
			},
		},
		{
			name: "list_active_analyses", title: "List active analyses",
			description: "Lists queued and running analyses of the account and those that finished in the last 48 hours.",
			properties:  map[string]any{"repo": map[string]any{"type": "string", "description": "Only this repository."}},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				var only *store.Repo
				if name, _ := a.str("repo"); name != "" {
					repo, err := s.toolRepo(p, a)
					if err != nil {
						return nil, err
					}
					only = repo
				}
				items := s.runner.Activity(p.session.UserKey)
				out := make([]map[string]any, 0, len(items))
				for _, item := range items {
					if only != nil && item.RepoKey != only.Key {
						continue
					}
					out = append(out, map[string]any{"repo_key": item.RepoKey, "commit": item.Commit, "variant": item.Variant, "status": item.Status, "trigger": item.Trigger, "mode": item.Mode, "error": item.Error, "queued_at": item.QueuedAt, "started_at": item.StartedAt, "finished_at": item.FinishedAt})
				}
				return map[string]any{"analyses": out}, nil
			},
		},
		{
			name: "get_findings", title: "Get findings",
			description: "Returns the findings of an analysis: the CLI's verdict and summary, the alerts (severity, file, line, explanation, evidence; reproduced issues and unverified model suspicions are distinguished), what the AI reviewer set aside, unverified areas and checks. Diffs are omitted; use get_report for them. For a plan, returns the plan.",
			properties: map[string]any{
				"repo": argRepo, "commit": argCommit, "variant": argVariant,
				"min_severity": map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}, "description": "Only alerts at or above this severity (default low: all)."},
			},
			requiredArgs: []string{"repo"},
			run:          toolGetFindings,
		},
		{
			name: "get_report", title: "Get report",
			description: `Returns a stored report in full. format "view" (default) is the hub's rendering with every changed file's diff; "raw" is the confidence report JSON the CLI wrote. Large; prefer get_findings.`,
			properties: map[string]any{
				"repo": argRepo, "commit": argCommit, "variant": argVariant,
				"format": map[string]any{"type": "string", "enum": []string{"view", "raw"}},
			},
			requiredArgs: []string{"repo"},
			run:          toolGetReport,
		},
		{
			name: "wait_for_analysis", title: "Wait for analysis",
			description: "Waits until the latest analysis of a commit is no longer queued or running (at most timeout_seconds), then returns its status and, when it finished, its findings.",
			properties: map[string]any{
				"repo": argRepo, "commit": argCommit, "variant": argVariant,
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "Default 120."},
			},
			requiredArgs: []string{"repo"},
			run:          toolWaitForAnalysis,
		},
		{
			name: "list_feedback", title: "List feedback",
			description:  "Lists the team's votes and comments on the findings of one analyzed commit.",
			properties:   map[string]any{"repo": argRepo, "commit": argCommit},
			requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, commit, err := s.toolRecord(p, a)
				if err != nil {
					return nil, err
				}
				return s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/reports/"+commit+"/feedback", nil, nil)
			},
		},
		{
			name: "get_learning", title: "Get learned context",
			description: "Returns what the reviewer learned from the team's feedback on a repository and whether learning is on.",
			properties:  map[string]any{"repo": argRepo}, requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				return s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/learning", nil, nil)
			},
		},

		// Write tools.
		{
			name: "trigger_review", title: "Trigger review", write: true, idempotent: true,
			description: "Queues an analysis of a commit, of the tip of a branch, or (with neither) of the tip of the default branch, in the deployment's mode. With variant \"plan\", queues a pre-change plan of the commit for the given intent. Returns at once; call wait_for_analysis next. An analysis already queued for the same commit is not duplicated.",
			properties: map[string]any{
				"repo": argRepo, "variant": argVariant,
				"commit": map[string]any{"type": "string", "description": "Full or abbreviated (7+) commit SHA."},
				"branch": map[string]any{"type": "string", "description": "Branch whose tip to analyze, when no commit is given."},
				"intent": map[string]any{"type": "string", "description": "Plan only: what the change should do (at most 64 KiB)."},
			},
			requiredArgs: []string{"repo"},
			run:          toolTriggerReview,
		},
		{
			name: "rerun_analysis", title: "Rerun analysis", write: true, idempotent: true,
			description:  "Queues a finished analysis again with its stored branch, base and intent, for example after the policy, the coding rules or the CLI changed.",
			properties:   map[string]any{"repo": argRepo, "commit": argCommit, "variant": argVariant},
			requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				return attemptTool(s, ctx, p, a, "/rerun")
			},
		},
		{
			name: "cancel_analysis", title: "Cancel analysis", write: true, idempotent: true,
			description:  "Withdraws an analysis that is still queued. A running analysis cannot be cancelled.",
			properties:   map[string]any{"repo": argRepo, "commit": map[string]any{"type": "string", "description": "Commit SHA of the queued analysis."}, "variant": argVariant},
			requiredArgs: []string{"repo", "commit"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				return attemptTool(s, ctx, p, a, "/cancel")
			},
		},
		{
			name: "update_coding_rules", title: "Update coding rules", write: true, idempotent: true,
			description:  "Replaces the team coding rules of a repository (Markdown, at most 32 KiB; empty removes them). The AI reviewer checks every later review against them. Read the current rules with get_repository first and send the complete new text. Earlier analyses are not rerun.",
			properties:   map[string]any{"repo": argRepo, "rules": map[string]any{"type": "string"}},
			requiredArgs: []string{"repo", "rules"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				rules, ok := a["rules"].(string)
				if !ok {
					return nil, inputErrorf("rules must be a string")
				}
				return s.callAPI(ctx, http.MethodPut, "/api/repos/"+repo.Key+"/rules", nil, map[string]any{"rules": rules})
			},
		},
		{
			name: "set_learning", title: "Set learning", write: true, idempotent: true,
			description:  "Turns learning from team feedback on or off for a repository. Off keeps the feedback but neither collects nor uses it.",
			properties:   map[string]any{"repo": argRepo, "enabled": map[string]any{"type": "boolean"}},
			requiredArgs: []string{"repo", "enabled"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				if _, ok := a["enabled"].(bool); !ok {
					return nil, inputErrorf("enabled must be true or false")
				}
				enabled, _ := a.boolean("enabled", true)
				return s.callAPI(ctx, http.MethodPut, "/api/repos/"+repo.Key+"/learning", nil, map[string]any{"enabled": enabled})
			},
		},
		{
			name: "reset_feedback", title: "Forget feedback", write: true, destructive: true, idempotent: true,
			description: "Forgets every vote and comment of a repository, and so everything the reviewer learned from them. Cannot be undone.",
			properties:  map[string]any{"repo": argRepo}, requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				return s.callAPI(ctx, http.MethodDelete, "/api/repos/"+repo.Key+"/feedback", nil, nil)
			},
		},
		{
			name: "add_feedback", title: "Add feedback", write: true,
			description: "Votes on a finding (\"up\": useful, \"down\": not useful) and/or comments on it, or replies to a comment. The reviewer adapts later reviews to this feedback when learning is on. The entry is signed with the account and the token name.",
			properties: map[string]any{
				"repo": argRepo, "commit": argCommit,
				"alert_id": map[string]any{"type": "string", "description": "The id of the finding, from get_findings."},
				"vote":     map[string]any{"type": "string", "enum": []string{"up", "down"}},
				"comment":  map[string]any{"type": "string"},
				"reply_to": map[string]any{"type": "string", "description": "Id of the comment this replies to."},
			},
			requiredArgs: []string{"repo", "alert_id"},
			run:          toolAddFeedback,
		},
		{
			name: "sync_repositories", title: "Refresh repositories", write: true, idempotent: true,
			description: "Refreshes the repository list from the forge in the background (new repositories, policy presence).",
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				return s.callAPI(ctx, http.MethodPost, "/api/repos/sync", nil, map[string]any{})
			},
		},
		{
			name: "create_policy", title: "Create policy", write: true,
			description: "Generates the .probe.json review policy of a repository that has none. With preview (default true) it only returns the policy; with preview false it commits it on the default branch.",
			properties: map[string]any{
				"repo":     argRepo,
				"language": map[string]any{"type": "string", "enum": []string{"go", "typescript", "javascript", "python", "rust"}, "description": "Default: detected from the default branch."},
				"preview":  map[string]any{"type": "boolean"},
			},
			requiredArgs: []string{"repo"},
			run: func(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
				repo, err := s.toolRepo(p, a)
				if err != nil {
					return nil, err
				}
				language, err := a.str("language")
				if err != nil {
					return nil, err
				}
				preview, err := a.boolean("preview", true)
				if err != nil {
					return nil, err
				}
				return s.callAPI(ctx, http.MethodPost, "/api/repos/"+repo.Key+"/policy", nil, map[string]any{"language": language, "preview": preview})
			},
		},
	}
}

// callAPI serves one request on the hub's own router, under the agent
// identity carried by ctx, and decodes the JSON reply. An error status
// becomes an error with the handler's message.
func (s *Server) callAPI(ctx context.Context, method, path string, query url.Values, body any) (map[string]any, error) {
	raw, err := s.callAPIRaw(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, errors.New("the hub returned an unreadable reply")
		}
	}
	return out, nil
}

func (s *Server) callAPIRaw(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if agentOf(ctx) == nil {
		return nil, errors.New("no agent identity")
	}
	var reader io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &apiRecorder{header: http.Header{}, status: http.StatusOK}
	s.routes().ServeHTTP(rec, req)
	if rec.status >= 400 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rec.body.Bytes(), &failure) == nil && failure.Error != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, fmt.Errorf("request failed with status %d", rec.status)
	}
	return rec.body.Bytes(), nil
}

// apiRecorder captures an internal response.
type apiRecorder struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func (r *apiRecorder) Header() http.Header { return r.header }
func (r *apiRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
}
func (r *apiRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.body.Write(p)
}

// toolRepo resolves the "repo" argument within the token's account.
func (s *Server) toolRepo(p *agentPrincipal, a toolArgs) (*store.Repo, error) {
	name, err := a.required("repo")
	if err != nil {
		return nil, err
	}
	repos, err := s.store.Repos(p.session.UserKey)
	if err != nil {
		return nil, errors.New("could not list the repositories")
	}
	var byName []*store.Repo
	for _, repo := range repos {
		if repo.Key == name || strings.EqualFold(repo.FullName, name) {
			return repo, nil
		}
		if i := strings.LastIndex(repo.FullName, "/"); i >= 0 && strings.EqualFold(repo.FullName[i+1:], name) {
			byName = append(byName, repo)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return nil, inputErrorf("unknown repository %q; call list_repositories (or sync_repositories if it is new)", name)
	default:
		names := make([]string, len(byName))
		for i, r := range byName {
			names[i] = r.FullName
		}
		sort.Strings(names)
		return nil, inputErrorf("%q is ambiguous: %s", name, strings.Join(names, ", "))
	}
}

// toolCommit resolves an abbreviated commit against the analyses of repo; an
// empty commit is the latest analyzed one. A commit no analysis matches is
// returned as given.
func (s *Server) toolCommit(p *agentPrincipal, repo *store.Repo, commit string) (string, error) {
	if commit == "" {
		if repo.Latest == nil || repo.Latest.Commit == "" {
			return "", inputErrorf("%s has no analysis yet; call trigger_review", repo.FullName)
		}
		return repo.Latest.Commit, nil
	}
	commit = strings.ToLower(commit)
	if !store.ValidKey(commit) {
		return "", inputErrorf("invalid commit %q", commit)
	}
	if len(commit) == 40 || len(commit) == 64 {
		return commit, nil
	}
	if len(commit) < 7 {
		return "", inputErrorf("a commit prefix needs at least 7 characters")
	}
	runs, err := s.store.History(p.session.UserKey, repo.Key, store.MaxHistory)
	if err != nil {
		return commit, nil
	}
	match := ""
	for _, run := range runs {
		if strings.HasPrefix(run.Commit, commit) && run.Commit != match {
			if match != "" {
				return "", inputErrorf("commit prefix %q is ambiguous", commit)
			}
			match = run.Commit
		}
	}
	if match == "" {
		return commit, nil
	}
	return match, nil
}

func (s *Server) toolRecord(p *agentPrincipal, a toolArgs) (*store.Repo, string, error) {
	repo, err := s.toolRepo(p, a)
	if err != nil {
		return nil, "", err
	}
	commit, err := a.str("commit")
	if err != nil {
		return nil, "", err
	}
	commit, err = s.toolCommit(p, repo, commit)
	return repo, commit, err
}

func toolListRepositories(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	query, err := a.str("query")
	if err != nil {
		return nil, err
	}
	monitored, err := a.boolean("monitored_only", false)
	if err != nil {
		return nil, err
	}
	repos, err := s.store.Repos(p.session.UserKey)
	if err != nil {
		return nil, errors.New("could not list the repositories")
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	out := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		if monitored && !repo.Monitored {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(repo.FullName), strings.ToLower(query)) {
			continue
		}
		public := repo.Public()
		item := map[string]any{
			"name": public.FullName, "key": public.Key, "default_branch": public.DefaultBranch,
			"has_policy": public.HasPolicy, "monitored": public.Monitored, "private": public.Private,
			"coding_rules": public.CodingRules != "", "learning": public.Learning,
		}
		if public.Latest != nil {
			item["latest"] = public.Latest
		}
		out = append(out, item)
	}
	return map[string]any{"repositories": out, "count": len(out)}, nil
}

var severityRank = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

func toolGetFindings(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	repo, commit, err := s.toolRecord(p, a)
	if err != nil {
		return nil, err
	}
	variant, err := a.variant()
	if err != nil {
		return nil, err
	}
	minimum, err := a.str("min_severity")
	if err != nil {
		return nil, err
	}
	if _, ok := severityRank[minimum]; minimum != "" && !ok {
		return nil, inputErrorf("min_severity must be low, medium, high or critical")
	}
	return s.findings(ctx, repo, commit, variant, minimum)
}

// findings is the compact form of a stored report.
func (s *Server) findings(ctx context.Context, repo *store.Repo, commit, variant, minimum string) (map[string]any, error) {
	out, err := s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/reports/"+commit, url.Values{"variant": {variant}}, nil)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"repository": repo.FullName, "commit": commit, "variant": variant, "run": out["run"]}
	if plan, ok := out["plan"]; ok {
		result["plan"] = plan
		return result, nil
	}
	view, _ := out["view"].(map[string]any)
	for _, key := range []string{"summary", "change", "unverified", "checks", "coverage", "review_surface", "intent", "reviewer_summary", "pr_summary", "diff_truncated", "tool_version", "generated_at"} {
		if v, ok := view[key]; ok {
			result[key] = v
		}
	}
	if change, ok := result["change"].(map[string]any); ok {
		delete(change, "files")
	}
	alerts, _ := view["alerts"].([]any)
	kept := make([]any, 0, len(alerts))
	for _, alert := range alerts {
		m, _ := alert.(map[string]any)
		severity, _ := m["severity"].(string)
		if minimum == "" || severityRank[severity] >= severityRank[minimum] {
			kept = append(kept, alert)
		}
	}
	result["alerts"] = kept
	if len(kept) != len(alerts) {
		result["alerts_below_min_severity"] = len(alerts) - len(kept)
	}
	result["set_aside_by_reviewer"] = view["dismissed"]
	result["report_path"] = "/api/repos/" + repo.Key + "/reports/" + commit + "/raw"
	return result, nil
}

func toolGetReport(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	repo, commit, err := s.toolRecord(p, a)
	if err != nil {
		return nil, err
	}
	variant, err := a.variant()
	if err != nil {
		return nil, err
	}
	format, err := a.str("format")
	if err != nil {
		return nil, err
	}
	query := url.Values{"variant": {variant}}
	switch format {
	case "", "view":
		return s.callAPI(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/reports/"+commit, query, nil)
	case "raw":
		raw, err := s.callAPIRaw(ctx, http.MethodGet, "/api/repos/"+repo.Key+"/reports/"+commit+"/raw", query, nil)
		if err != nil {
			return nil, err
		}
		var report any
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, errors.New("the stored report is not JSON")
		}
		return map[string]any{"repository": repo.FullName, "commit": commit, "variant": variant, "report": report}, nil
	default:
		return nil, inputErrorf(`format must be "view" or "raw"`)
	}
}

// waitPoll is how often wait_for_analysis looks at the queue; tests shorten it.
var waitPoll = 2 * time.Second

func toolWaitForAnalysis(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	repo, commit, err := s.toolRecord(p, a)
	if err != nil {
		return nil, err
	}
	variant, err := a.variant()
	if err != nil {
		return nil, err
	}
	timeout, err := a.integer("timeout_seconds", 120, 1, 600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		status, failure := "", ""
		var latest time.Time
		for _, item := range s.runner.Activity(p.session.UserKey) {
			if item.RepoKey == repo.Key && item.Variant == variant && strings.HasPrefix(item.Commit, commit) && !item.QueuedAt.Before(latest) {
				status, failure, latest = item.Status, item.Error, item.QueuedAt
			}
		}
		if status == "" {
			// Not in the recent activity: a stored result decides.
			if rec, err := s.store.RecordVariant(p.session.UserKey, repo.Key, commit, variant); err == nil {
				status, failure = rec.Status, store.SafeError(rec.Error)
			} else {
				return map[string]any{"repository": repo.FullName, "commit": commit, "variant": variant, "status": "unknown", "message": "no analysis of this commit is queued, running or stored; call trigger_review"}, nil
			}
		}
		if status != store.StatusQueued && status != store.StatusRunning {
			result := map[string]any{"repository": repo.FullName, "commit": commit, "variant": variant, "status": status}
			if failure != "" {
				result["error"] = failure
			}
			if status == store.StatusDone {
				if f, err := s.findings(ctx, repo, commit, variant, ""); err == nil {
					result["findings"] = f
				}
			}
			return result, nil
		}
		if !time.Now().Before(deadline) {
			return map[string]any{"repository": repo.FullName, "commit": commit, "variant": variant, "status": status, "message": "still " + status + "; call wait_for_analysis again"}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

func toolTriggerReview(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	repo, err := s.toolRepo(p, a)
	if err != nil {
		return nil, err
	}
	variant, err := a.variant()
	if err != nil {
		return nil, err
	}
	commit, err := a.str("commit")
	if err != nil {
		return nil, err
	}
	branch, err := a.str("branch")
	if err != nil {
		return nil, err
	}
	intent, err := a.str("intent")
	if err != nil {
		return nil, err
	}
	if commit != "" && branch != "" {
		return nil, inputErrorf("give either commit or branch")
	}
	if intent != "" && variant != "plan" {
		return nil, inputErrorf(`intent applies to variant "plan" only`)
	}
	body := map[string]any{"variant": variant, "intent": intent}
	if commit != "" {
		commit = strings.ToLower(commit)
		if !store.ValidKey(commit) {
			return nil, inputErrorf("invalid commit %q", commit)
		}
		body["commit"] = commit
	}
	if branch != "" {
		user, err := s.store.User(p.session.UserKey)
		if err != nil {
			return nil, errors.New("could not load the account")
		}
		provider, err := s.accounts.Provider(user.Provider)
		if err != nil {
			return nil, err
		}
		token, err := s.accounts.Token(ctx, user)
		if err != nil {
			return nil, fmt.Errorf("sign in to the hub again: %w", err)
		}
		head, err := provider.HeadCommit(ctx, token, forgeRepo(repo), strings.TrimPrefix(branch, "refs/heads/"))
		if err != nil {
			return nil, fmt.Errorf("resolve branch %s: %w", branch, err)
		}
		body["commit"], body["ref"] = head.SHA, "refs/heads/"+strings.TrimPrefix(branch, "refs/heads/")
	}
	if variant == "plan" && body["commit"] == nil {
		return nil, inputErrorf("a plan needs a commit or a branch")
	}
	out, err := s.callAPI(ctx, http.MethodPost, "/api/repos/"+repo.Key+"/analyze", nil, body)
	if err != nil {
		return nil, err
	}
	out["repository"] = repo.FullName
	out["next"] = "call wait_for_analysis with this repository and commit"
	return out, nil
}

func attemptTool(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs, action string) (any, error) {
	repo, commit, err := s.toolRecord(p, a)
	if err != nil {
		return nil, err
	}
	variant, err := a.variant()
	if err != nil {
		return nil, err
	}
	out, err := s.callAPI(ctx, http.MethodPost, "/api/repos/"+repo.Key+action, nil, map[string]any{"commit": commit, "variant": variant})
	if err != nil {
		return nil, err
	}
	out["repository"] = repo.FullName
	return out, nil
}

func toolAddFeedback(s *Server, ctx context.Context, p *agentPrincipal, a toolArgs) (any, error) {
	repo, commit, err := s.toolRecord(p, a)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	for _, name := range []string{"alert_id", "vote", "comment", "reply_to"} {
		v, err := a.str(name)
		if err != nil {
			return nil, err
		}
		body[name] = v
	}
	return s.callAPI(ctx, http.MethodPost, "/api/repos/"+repo.Key+"/reports/"+commit+"/feedback", nil, body)
}
