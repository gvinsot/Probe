// Package cli composes immutable analysis, isolated experiments and evidence reports.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/fsutil"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/graph"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/knowledge"
	"github.com/gvinsot/Probe/app/internal/linter"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/report"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

const usage = `Probe — evidence for focused review of AI-assisted changes

Usage:
  probe init [--repo PATH] [--language go|typescript|javascript|python|rust]
  probe lint [--base main] [--head HEAD] [--ci]
  probe review [--base main] [--reviewer=false] [--ci]
  probe review --swarm [--swarm-agents correctness,security,...] [--base main]
  probe review --read-only [--base main] [--ci]
  probe review [flags] BASE..HEAD
  probe plan --intent-file FILE [--base main] [--ci]
  probe plan --jira PROJ-123 | --linear ENG-123 [--notion PAGE] [--gdoc DOC] [--base main] [--ci]
  probe review --plan .probe/PLAN.json [flags]
  probe knowledge build [--base main] [--focus TEXT]
  probe knowledge apply [--from .probe/knowledge-updates.json]
  probe knowledge check [--knowledge PROBE_KNOWLEDGE.md]
  probe context check [--context-dir DIR] [--context-repo NAME=PATH] [--clusters FILE]
  probe graph build [--commit HEAD] [--out FILE]
  probe graph query search|neighbors|path ARGS [--commit HEAD]
  probe report [--input .probe/confidence-report.json] [--out DIR] [--format LIST] [--report-url URL]
  probe login [--hub URL] [--status]
  probe logout
  probe version

Analysis compares the merge base by default; BASE..HEAD compares exact commits.
Policy is read from .probe.json at the tip of the base branch (main).
Only committed files are reviewed. Output defaults to .probe/.
Review runs configured checks in Docker. Lint never executes repository code.
Review automatically uses the LLM when reviewer.model is configured in trusted policy.
PROBE_REVIEWER_ENDPOINT and PROBE_REVIEWER_MODEL override that policy, and
the API key comes from the api_key_env variable or its /run/secrets/<NAME> Docker secret.
PROBE_REVIEWER_PROVIDER routes the request to named providers (OpenRouter's
"provider" field: a comma-separated list, or a JSON object sent as is), and
PROBE_REVIEWER_TEMPERATURE (0 to 2) sets the sampling temperature.
The reviewer sends bounded, redacted source context to its configured API.
Use --reviewer=false to disable it. Lint never calls a provider.
Without a configured endpoint or API key, the provider is the LLM of the Probe
Hub account this machine logged into: probe login (or PROBE_HUB_TOKEN, a token
created in the dashboard, for CI) connects it, within the account's daily
quota; probe logout revokes it.
Review --read-only inspects the diff with the deployment's LLM, without Docker
or code execution. Its suspicions stay unverified; execution flags are refused.
Plan asks the provider for an implementation plan (read-only, nothing runs) and
evaluates it with fixed rules; review or lint --plan check the diff against it.
--jira KEY and --linear KEY (or auto: the key in the branch name or commit
messages) add that Jira or Linear issue to the intent of review, lint and plan;
configure PROBE_JIRA_URL and PROBE_JIRA_EMAIL/PROBE_JIRA_TOKEN, or
PROBE_LINEAR_API_KEY. --notion PAGE[,PAGE] adds Notion pages as context
(PROBE_NOTION_TOKEN) and --gdoc DOC[,DOC] Google Docs such as design documents
(PROBE_GOOGLE_ACCESS_TOKEN, PROBE_GOOGLE_CREDENTIALS or PROBE_GOOGLE_API_KEY);
only their "Acceptance criteria" items become criteria.
The codebase knowledge base (PROBE_KNOWLEDGE.md, editable Markdown) is read at
the tip of the base branch and given to the reviewer, which proposes updates in
.probe/knowledge-updates.json; knowledge build proposes entries from a
read-only exploration, apply merges proposals for you to review and commit.
The repository graph (components, packages, files, functions, types and
external dependencies with their calls, imports and dependencies) is built
from the committed files of the head commit by lint and review (--graph,
default on), cached by commit in --graph-cache (default: the user cache
directory), recorded with its structural delta against the base, and queried
by the reviewer; probe graph builds and queries it outside a review.
Policy "context" names other repositories, directly or through repository
clusters, that the reviewer reads to check cross-repository contracts; point
review at their checkouts with --context-dir or --context-repo NAME=PATH.

Evidence stages (review only unless noted; policy keys fuzz, mutation and prepare
are opt-in and need a v0.4 binary):
  --base-tests             run baseline versions of changed Go tests on candidate code
  --fuzz=false             skip the differential fuzzing that policy "fuzz" configures
  --impact=false           skip the static impact index (lint and review)
  --impacted-tests         run unchanged Go tests that statically reach changed code
  --cache-dir DIR          opt-in baseline execution cache outside repository and output
  --parallel N             run the initial checks N at a time (1..4)
  --allow-prepare-network  permit network for policy "prepare" only if it enables it too
  --deadline D             overall time limit from 1m to 24h (30s are kept for the report)
  --format LIST            report formats: markdown,json,sarif,pr-comment,pr-summary (lint, review and report)
  --report-url URL         link to the full report cited by the pr-comment format
Use 'probe <command> --help' for options.
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "probe %s\nAGPL-3.0 with an attribution term, see NOTICE: https://github.com/gvinsot/Probe\n", version)
		return 0
	case "init":
		return initialize(args[1:], stdout, stderr)
	case "lint", "review":
		return analyze(ctx, args[0], args[1:], stdout, stderr, version)
	case "report":
		return render(args[1:], stdout, stderr)
	case "plan":
		return planCommand(ctx, args[1:], stdout, stderr, version)
	case "knowledge":
		return knowledgeCommand(ctx, args[1:], stdout, stderr, version)
	case "context":
		return contextCommand(ctx, args[1:], stdout, stderr)
	case "graph":
		return graphCommand(ctx, args[1:], stdout, stderr, version)
	case "login":
		return loginCommand(ctx, args[1:], stdout, stderr)
	case "logout":
		return logoutCommand(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
		return 3
	}
}

func initialize(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("init", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoDir := f.String("repo", ".", "repository directory")
	language := f.String("language", "", "project language (auto-detected when omitted)")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "init accepts no positional arguments")
	}
	if *language == "" {
		*language = detect(func(name string) bool { _, err := os.Stat(filepath.Join(*repoDir, name)); return err == nil })
	}
	if *language != "go" && *language != "typescript" && *language != "javascript" && *language != "python" && *language != "rust" && *language != "unknown" {
		return fail(errOut, 3, "unsupported language %q", *language)
	}
	c := config.Default(*language)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	path := filepath.Join(*repoDir, config.Filename)
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(errOut, 3, "create configuration: %v", err)
	}
	_, writeErr := fh.Write(append(data, '\n'))
	closeErr := fh.Close()
	if writeErr != nil {
		return fail(errOut, 3, "%v", writeErr)
	}
	if closeErr != nil {
		return fail(errOut, 3, "%v", closeErr)
	}
	fmt.Fprintf(out, "Created %s (%s). Review commands and sandbox image, then commit this policy to your base branch.\nUse --config %s to explicitly try the local policy before committing it.\n", path, c.Language, path)
	return 0
}

func analyze(ctx context.Context, mode string, args []string, out, errOut io.Writer, version string) int {
	// --deadline counts from here unless the caller recorded an earlier start.
	if _, ok := ctx.Value(startKey{}).(time.Time); !ok {
		ctx = withStart(ctx, time.Now())
	}
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	base := f.String("base", "main", "base branch or revision; its tip supplies the trusted policy")
	head := f.String("head", "HEAD", "candidate Git revision")
	exact := f.Bool("exact", false, "compare exact base instead of merge base")
	policyPath := f.String("config", "", "explicit trusted local configuration (default: policy at the tip of --base)")
	outDir := f.String("out", ".probe", "report directory, relative to repository")
	format := f.String("format", "markdown,json", "comma-separated output formats: markdown,json,sarif,pr-comment,pr-summary")
	ci := f.Bool("ci", false, "return 2 when human review is required")
	checks := f.Bool("checks", mode == "review", "run configured checks in the Docker sandbox")
	readOnly := f.Bool("read-only", false, "review: inspect changes with the LLM using read-only tools, without Docker or code execution")
	useReviewer := f.Bool("reviewer", false, "use LLM investigation (default: enabled for review when a model is configured in policy or the environment); --reviewer=false disables provider calls")
	aiImpactsCriticality := f.Bool("ai-impacts-criticality", true, "lower by one level the severity of a linter signal the reviewer read as no_risk from a recorded source observation, and set a low one aside; --ai-impacts-criticality=false keeps linter severities")
	maxIterations := f.Int("max-iterations", 0, "override LLM iteration budget (1..100)")
	swarmOptions := addSwarmFlags(f)
	prSummary := f.Bool("pr-summary", true, "review: after the review, have the reviewer model write a natural-language pull request summary (pr_summary, PR_SUMMARY.md); one extra provider call; --pr-summary=false skips it")
	intent := f.String("intent", "", "PR intent or acceptance criteria")
	intentFile := f.String("intent-file", "", "UTF-8 file containing PR intent")
	issues := addIssueFlags(f)
	rules := f.String("rules", "", "review: team coding rules the reviewer checks the changed code against")
	rulesFile := f.String("rules-file", "", "review: UTF-8 file containing team coding rules")
	reportLanguage := f.String("report-language", "", "review: natural language the reviewer writes its findings and summary in, such as French (default: English)")
	contextOptions := addContextFlags(f)
	knowledgePath := f.String("knowledge", knowledge.DefaultPath, "review: codebase knowledge base, read at the tip of --base and given to the reviewer, which proposes updates; \"none\" disables it")
	feedbackFile := f.String("feedback-file", "", "review: JSON file of team feedback on earlier findings (Probe Hub writes it), used to adapt the reviewer to the team")
	allowNetwork := f.Bool("allow-network", false, "permit sandbox network only if trusted policy also enables it")
	noNetwork := f.Bool("no-network", false, "force sandbox networking off (use --reviewer=false to also disable the reviewer API)")
	reportURL := f.String("report-url", "", "https link to the full report, cited by the pr-comment format")
	baseTests := f.Bool("base-tests", false, "review: run the baseline versions of changed Go tests on candidate code")
	fuzzFlag := f.Bool("fuzz", true, "review: run the differential fuzzing that trusted policy configures; --fuzz=false records it as disabled")
	impactFlag := f.Bool("impact", true, "build the static impact index of changed functions: Go type-checked, TS/JS, Python and Rust lexical (lint and review)")
	graphFlag := f.Bool("graph", true, "build the repository graph of the head commit (components, packages, files, functions, types, dependencies), record it and give it to the reviewer (lint and review)")
	graphCache := f.String("graph-cache", "", "repository graph cache directory, outside the repository and the output directory (default: the user cache directory; \"off\" disables it)")
	impactedTests := f.Bool("impacted-tests", false, "review: run unchanged Go tests that statically reach changed code on baseline and candidate")
	cacheDir := f.String("cache-dir", "", "review: opt-in baseline execution cache directory, outside the repository and the output directory")
	parallel := f.Int("parallel", 1, "review: number of initial checks run at a time (1..4)")
	allowPrepareNetwork := f.Bool("allow-prepare-network", false, "review: permit network for the trusted prepare container only, if policy prepare.network also enables it")
	deadline := f.Duration("deadline", 0, "review: overall time limit from 1m to 24h; 30s of it are kept for cleanup and the report")
	planFile := f.String("plan", "", "PLAN.json written by probe plan: check the diff against its contract (scope drift)")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	formats, err := parseFormats(*format)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	opts, err := reportOptions(formats, *reportURL)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	explicit := visitedFlags(f)
	if err := validateReadOnlyFlags(mode, explicit, *readOnly, *checks, *useReviewer, *allowNetwork, *allowPrepareNetwork); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if *readOnly {
		*checks, *useReviewer = false, true
	}
	if err := validateExecutionFlags(mode, explicit, execFlags{checks: *checks, baseTests: *baseTests, impact: *impactFlag, impactedTests: *impactedTests, parallel: *parallel, cacheDir: *cacheDir, deadline: *deadline}); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	mergeBase := !*exact
	if f.NArg() > 1 {
		return fail(errOut, 3, "expected at most one BASE..HEAD or BASE...HEAD range")
	}
	if f.NArg() == 1 {
		sep := ".."
		mergeBase = false
		if strings.Contains(f.Arg(0), "...") {
			sep = "..."
			mergeBase = true
		}
		parts := strings.Split(f.Arg(0), sep)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fail(errOut, 3, "range must be BASE..HEAD or BASE...HEAD")
		}
		*base, *head = parts[0], parts[1]
	}
	if *intentFile != "" {
		if *intent != "" {
			return fail(errOut, 3, "use either --intent or --intent-file")
		}
		b, err := readLimited(*intentFile, 65536)
		if err != nil {
			return fail(errOut, 3, "intent: %v", err)
		}
		*intent = string(b)
	}
	if len(*intent) > maxIntentBytes {
		return fail(errOut, 3, "intent exceeds 64 KiB")
	}
	if err := issues.check(); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if err := reviewer.ValidateLanguage(*reportLanguage); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	codingRules, err := loadCodingRules(*rules, *rulesFile)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if *knowledgePath != noKnowledge {
		if err := knowledge.ValidPath(*knowledgePath); err != nil {
			return fail(errOut, 3, "%v", err)
		}
	}
	feedback, feedbackSHA256, err := loadTeamFeedback(*feedbackFile)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	doc, err := parseIntent(*intent)
	if err != nil {
		return fail(errOut, 3, "intent: %v", err)
	}
	var drift *model.PlanDrift
	var proposal model.PlanProposal
	if *planFile != "" {
		if drift, proposal, err = loadPlanContract(*planFile); err != nil {
			return fail(errOut, 3, "plan: %v", err)
		}
	}
	repo, err := gitrepo.Open(ctx, *repoPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	change, err := repo.Analyze(ctx, *base, *head, mergeBase)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if issues.any() {
		text, err := issueIntent(ctx, errOut, issues, *intent, func() []string {
			return issueKeySources(ctx, repo, change.HeadRef, change.BaseCommit, change.HeadCommit)
		})
		if err != nil {
			return fail(errOut, 3, "%v", err)
		}
		if doc, err = parseIntent(text); err != nil {
			return fail(errOut, 3, "intent: %v", err)
		}
	}
	// The diff starts at the merge base, but the policy comes from the tip of
	// the base ref: the branch the change targets decides its current rules,
	// and a branch forked before the policy existed still gets it.
	cfg, policy, err := loadPolicy(ctx, repo, change.BaseRefCommit, *policyPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if policy.Source == model.PolicyDefault {
		fmt.Fprintf(errOut, "No %s at %s (%s); using built-in %s defaults.\n", config.Filename, change.BaseRef, shortCommit(policy.Commit), cfg.Language)
	}
	if *readOnly {
		// Public repositories cannot select the provider, credential name or
		// investigation budgets. Read-only review uses deployment settings.
		cfg.Reviewer = config.Default(cfg.Language).Reviewer
	}
	if *maxIterations != 0 {
		cfg.Reviewer.MaxIterations = *maxIterations
	}
	if err := cfg.Validate(); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	// Only trusted baseline policy (or explicit --config) can enable provider
	// traffic. A supplied boolean flag, including false, overrides auto-selection.
	reviewerExplicit := explicit["reviewer"]
	// Endpoint, model and credential come from the deployment: the same image
	// and the same trusted policy are pointed at the operator's provider
	// without a policy change. A misconfigured secret fails the run here
	// rather than downgrading it to an unauthenticated request.
	provider, err := cfg.ResolveReviewer(os.Getenv, os.ReadFile)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if !reviewerExplicit && !*readOnly {
		*useReviewer = mode == "review" && provider.Model != ""
	}
	if mode == "lint" && (*checks || *useReviewer) {
		return fail(errOut, 3, "lint does not execute checks or a reviewer; use review")
	}
	if *useReviewer && provider.Model == "" {
		if *readOnly {
			return fail(errOut, 3, "%s must be configured before using --read-only", config.ModelEnv)
		}
		return fail(errOut, 3, "reviewer.model must be configured in policy or %s before using --reviewer", config.ModelEnv)
	}
	reviewerOptions := reviewer.Options{Endpoint: provider.Endpoint, Model: provider.Model, APIKey: provider.APIKey, Provider: provider.Provider, Temperature: provider.Temperature, MaxIterations: cfg.Reviewer.MaxIterations, Timeout: time.Duration(cfg.Reviewer.TimeoutSeconds) * time.Second, MaxInputBytes: cfg.Reviewer.MaxInputBytes}
	reviewerOptions.ReadOnly = *readOnly
	reviewerOptions.Language = *reportLanguage
	reviewerOptions.AllowInsecureHTTP = provider.AllowInsecureHTTP
	if reviewerOptions.Swarm, err = resolveSwarm(mode, explicit, swarmOptions, cfg.Reviewer.Swarm, *useReviewer); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if explicit["pr-summary"] && *prSummary && (mode == "lint" || !*useReviewer) {
		return fail(errOut, 3, "--pr-summary is written by the reviewer model; it needs review with the reviewer enabled")
	}
	if *useReviewer {
		if err := reviewer.Validate(reviewerOptions); err != nil {
			return fail(errOut, 3, "%v", err)
		}
		if len(provider.Sources) > 0 {
			fmt.Fprintf(errOut, "Reviewer configuration: %s.\n", strings.Join(provider.Sources, ", "))
		}
	}
	output := *outDir
	if !filepath.IsAbs(output) {
		output = filepath.Join(repo.Root, output)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if err := validateOutput(output); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	// The cache directory is validated here, before dependency preparation and
	// before any container starts; nil when --cache-dir is unset (no Docker call).
	cache, err := openExecutionCache(repo.Root, output, *cacheDir, version, errOut)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	var graphStore *graph.Store
	if *graphFlag {
		if graphStore, err = openGraphStore(*graphCache, explicit["graph-cache"], repo.Root, output, version, errOut); err != nil {
			return fail(errOut, 3, "%v", err)
		}
	}
	// Prepare, harness runs and the reviewer use work; static analysis,
	// snapshots, cleanup and report writing use the parent context.
	work, stopWork := workContext(ctx, *deadline)
	defer stopWork()
	signals, err := linter.Analyze(ctx, repo, change, cfg.SensitivePaths)
	if err != nil {
		return fail(errOut, 4, "linter: %v", err)
	}
	signals = linter.Merge(signals, prepareSignals(cfg.Prepare, change))
	impact, err := analyzeImpact(ctx, repo, change, *impactFlag)
	if err != nil {
		return fail(errOut, 4, "impact analysis: %v", err)
	}
	signals = linter.Merge(signals, impact.signals)
	graphView, graphSection, err := buildGraph(ctx, repo, change, impact, graphStore, *graphFlag)
	if err != nil {
		return fail(errOut, 4, "repository graph: %v", err)
	}
	if graphView != nil {
		reviewerOptions.Graph = graphView
	}
	if line := graphSummary(graphSection); line != "" {
		fmt.Fprintln(errOut, line)
	}
	r := model.Report{Version: 1, ToolVersion: version, GeneratedAt: time.Now().UTC(), Intent: doc.Text, IntentSHA256: doc.SHA256, IntentCriteria: doc.Criteria, Change: change, Policy: policy, Signals: signals, Impact: impact.report, Graph: graphSection, Coverage: coverage.NotConfigured()}
	if *readOnly {
		r.AnalysisMode = "review-read-only"
	}
	r.AIImpactsCriticality = *aiImpactsCriticality
	if codingRules != "" {
		// Rules only reach a reviewer; without one they are not recorded, so
		// that the report never suggests that they were checked.
		if *useReviewer {
			r.CodingRules = codingRules
			r.CodingRulesSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(codingRules)))
		} else {
			fmt.Fprintln(errOut, "Coding rules not applied: no reviewer runs in this analysis.")
		}
	}
	// Cross-repository context comes from the trusted policy, like the rest.
	if *useReviewer {
		temp, err := os.MkdirTemp("", "probe-context-")
		if err != nil {
			return fail(errOut, 4, "%v", err)
		}
		defer os.RemoveAll(temp)
		set, records, err := openContext(ctx, cfg, repo, contextOptions, temp)
		if err != nil {
			return fail(errOut, 3, "context: %v", err)
		}
		if len(records) > 0 {
			r.ContextRepos = records
			fmt.Fprintln(errOut, contextSummary(records))
			for _, c := range records {
				if c.Status != model.ContextAvailable {
					fmt.Fprintf(errOut, "  %s unavailable: %s\n", c.Name, c.Reason)
				}
			}
			if len(set.Repos()) > 0 {
				reviewerOptions.Context = set
			}
		}
	}
	// The knowledge base comes from the tip of the base ref, like the policy:
	// a change never supplies the knowledge its own review receives.
	var knowledgeBase *knowledge.Base
	if *useReviewer && *knowledgePath != noKnowledge {
		k, kb, err := loadReviewKnowledge(ctx, repo, change.BaseRefCommit, *knowledgePath, change)
		if err != nil {
			fmt.Fprintf(errOut, "Knowledge base not used: %v\n", err)
		} else {
			r.Knowledge, knowledgeBase = k, kb
		}
	}
	if feedback != nil {
		if *useReviewer {
			r.TeamFeedback, r.TeamFeedbackSHA256 = feedback, feedbackSHA256
		} else {
			fmt.Fprintln(errOut, "Team feedback not applied: no reviewer runs in this analysis.")
		}
	}
	r.Unverified = append(r.Unverified, doc.Notes...)
	if drift != nil {
		// The critical globs of this review's trusted policy; Finalize
		// computes the items and status.
		drift.CriticalGlobs = append([]string{}, cfg.SensitivePaths...)
		if err := reassessPlan(ctx, repo, drift, proposal, cfg.SensitivePaths); err != nil {
			return fail(errOut, 4, "plan assessment: %v", err)
		}
		r.PlanDrift = drift
	}
	operationalFailure := false
	needExecution := mode == "review" && !*readOnly && len(change.Files) > 0 && (*checks || *useReviewer)
	sc := stageContext{mode: mode, checks: *checks, reason: noExecutionReason(mode, change, *checks, *useReviewer)}
	if *readOnly {
		sc.reason = "read-only review does not execute repository code"
	}
	image := cfg.Sandbox.Image
	var prep preparation
	if mode == "review" && cfg.Prepare != nil {
		if needExecution {
			executionStarted()
			prep = runPrepare(work, repo, cfg, change, filepath.Join(output, "artifacts"), cfg.Prepare.Network && *allowPrepareNetwork && !*noNetwork, version, errOut)
			if prep.ok {
				image = prep.image
			} else {
				// No fallback to the unprepared image: nothing executes, and the
				// run is an operational failure.
				needExecution, operationalFailure, sc.reason = false, true, reasonPrepareFailed
			}
		} else {
			prep = preparation{record: prepareNotRun(cfg, change, sc.reason)}
		}
		r.Prepare = prep.record
		r.Unverified = append(r.Unverified, prep.unverified...)
	}
	if needExecution {
		sc.executed = true
		temp, err := os.MkdirTemp("", "probe-")
		if err != nil {
			return fail(errOut, 4, "%v", err)
		}
		defer os.RemoveAll(temp)
		candidateDir, baseDir := filepath.Join(temp, "candidate"), filepath.Join(temp, "base")
		if err := repo.Snapshot(ctx, change.HeadCommit, candidateDir); err != nil {
			return fail(errOut, 4, "candidate snapshot: %v", err)
		}
		if err := repo.Snapshot(ctx, change.BaseCommit, baseDir); err != nil {
			return fail(errOut, 4, "base snapshot: %v", err)
		}
		diffJSON, _ := json.Marshal(change)
		executionStarted()
		h, err := harness.NewContext(work, harness.Options{
			CandidateDir: candidateDir, BaseDir: baseDir, ArtifactDir: filepath.Join(output, "artifacts"),
			Commands: cfg.Commands, Image: image, Network: cfg.Sandbox.Network && *allowNetwork && !*noNetwork,
			Timeout: time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second, MaxRuntime: time.Duration(cfg.Sandbox.MaxRuntimeSeconds) * time.Second,
			MaxGeneratedTests: cfg.Reviewer.MaxGeneratedTests, MaxOutputBytes: cfg.Sandbox.MaxOutputBytes,
			MemoryMB: cfg.Sandbox.MemoryMB, CPUs: cfg.Sandbox.CPUs, Diff: string(diffJSON),
			IntentCriteria: r.IntentCriteria, Symbols: impact.lookup, Cache: cache, Parallel: *parallel,
			ReviewerReserve: reviewerReserve(cfg, *useReviewer),
		})
		if err != nil {
			return fail(errOut, 4, "harness: %v", err)
		}
		defer h.Close()
		// Stage order: initial checks, coverage, baseline versions of changed
		// tests, impacted tests, fuzzing, mutation, then the reviewer. The
		// cheapest and strongest deterministic evidence comes first.
		if *checks {
			if kinds := initialChecks(cfg.Commands); len(kinds) > 0 {
				fmt.Fprintf(errOut, "Running %s in isolated Docker sandboxes...\n", strings.Join(kinds, ", "))
				for _, c := range h.RunChecks(work, kinds) {
					if c.Status == "ERROR" {
						operationalFailure = true
					}
				}
			}
			// Coverage runs after the initial checks and in addition to the
			// configured test command, so the shared runtime budget starves the
			// measurement rather than the checks a repository already relies on.
			covered := coverage.Result{}
			if _, configured := cfg.Commands[coverage.CommandKey]; configured {
				fmt.Fprintf(errOut, "Running %s in isolated Docker sandbox...\n", coverage.CommandKey)
				check, profile, sha, reason := h.RunCoverage(work)
				if check.Status == "ERROR" {
					operationalFailure = true
				}
				expected := coverage.ExpectedFormat(cfg.Commands[coverage.CommandKey], cfg.Language)
				result := coverage.NotMeasuredAs(expected, reason)
				if reason == "" {
					result = measure(profile, candidateDir, check, sha, change)
				}
				r.Coverage = result.Report()
				// Coverage may add signals and add sentences; it never deletes a
				// signal, lowers a severity or supports a dismissal.
				r.Signals = linter.Merge(coverage.Requalify(r.Signals, result), result.Signals())
				if result.Status() != coverage.StatusMeasured {
					r.Unverified = append(r.Unverified, "Changed-line execution was not measured: "+r.Coverage.Reason)
				}
				if reason == "" && check.Status == "PASS" {
					covered = result
				}
			}
			if *baseTests && runBaseTests(work, repo, change, h, &r, errOut) {
				operationalFailure = true
			}
			if *impactedTests && runImpactedTests(work, h, &r, impact, errOut) {
				operationalFailure = true
			}
			runFuzz(work, h, cfg, change, baseDir, candidateDir, &r, *fuzzFlag, errOut)
			if cfg.Mutation != nil && runMutation(work, h, cfg, change, covered, &r, errOut) {
				operationalFailure = true
			}
		}
		r.Checks, r.Evidence, r.Audit = h.Checks(), h.Evidence(), h.Audit()
		auditBefore := len(r.Audit)
		if *useReviewer {
			if reviewerOptions.Swarm != nil {
				fmt.Fprintln(errOut, "Investigating with a swarm of specialized reviewer agents through the configured reviewer API...")
			} else {
				fmt.Fprintln(errOut, "Investigating with the configured reviewer API...")
			}
			err := reviewer.Run(work, reviewerOptions, &r, h)
			if err != nil {
				r.Unverified = append(r.Unverified, "Reviewer incomplete: "+err.Error())
			}
		}
		r.Checks, r.Evidence, r.Artifacts = h.Checks(), h.Evidence(), h.Artifacts()
		r.Audit = append(r.Audit, h.Audit()[auditBefore:]...)
		sort.SliceStable(r.Audit, func(i, j int) bool { return r.Audit[i].Time.Before(r.Audit[j].Time) })
		r.Execution = executionSummary(h, work)
		// A model-written test that did not build, load or run its named
		// tests is its own failure: its evidence stays unverified and review
		// is requested (exit 2). Every other ERROR check is a failure of the
		// run.
		for _, c := range r.Checks {
			if c.Operational() {
				operationalFailure = true
			}
		}
	} else if *readOnly && len(change.Files) > 0 {
		if err := runReadOnlyReview(work, repo, &r, impact.lookup, reviewerOptions, output, errOut); err != nil {
			// The reviewer is the whole of a read-only review: without it
			// nothing beyond lint was examined, so the run is incomplete
			// rather than a review outcome. Budget limits are not errors.
			r.Unverified = append(r.Unverified, "Read-only reviewer incomplete: "+err.Error())
			fmt.Fprintln(errOut, "Read-only reviewer incomplete: "+err.Error())
			operationalFailure = true
		}
	} else if mode == "review" && len(change.Files) > 0 && !(*checks || *useReviewer) {
		r.Unverified = append(r.Unverified, "Automated execution was explicitly disabled; only static change analysis was performed.")
	}
	if deadlineReached(ctx, work) {
		r.Unverified = append(r.Unverified, deadlineNote)
	}
	// A requested or configured stage that did not run still records its
	// section, with the reason (present means requested).
	recordFuzzSkipped(cfg, sc, *fuzzFlag, &r)
	recordBaseTestsSkipped(sc, *baseTests, &r)
	recordImpactedTestsSkipped(sc, *impactedTests, &r)
	recordMutationSkipped(cfg, sc, &r)
	r.Artifacts = append(prep.artifacts, r.Artifacts...)
	r.Audit = append(prep.audit, r.Audit...)
	for i := range r.Artifacts {
		if relative, err := filepath.Rel(output, r.Artifacts[i].Path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			r.Artifacts[i].Path = filepath.ToSlash(relative)
		}
	}
	report.Finalize(&r, *ci)
	if operationalFailure {
		r.ExitCode = 4
	}
	if *prSummary && *useReviewer && reviewer.SummaryAvailable(&r) {
		writePRSummary(work, repo, reviewerOptions, &r, errOut)
	}
	if err := report.Write(output, &r, formats, opts...); err != nil {
		return fail(errOut, 4, "write report: %v", err)
	}
	if k := r.Knowledge; k != nil && len(k.Updates) > 0 {
		p := model.KnowledgeProposal{ToolVersion: version, Source: "review", Path: k.Path, BaseCommit: k.Commit, HeadCommit: change.HeadCommit, Updates: k.Updates}
		if err := writeKnowledgeProposal(output, p, knowledgeBase, knowledgeStamp("review", change.HeadCommit)); err != nil {
			return fail(errOut, 4, "write knowledge proposal: %v", err)
		}
		fmt.Fprintf(out, "Knowledge: %d updates proposed for %s (model output); apply with probe knowledge apply, then review and commit.\n", len(k.Updates), k.Path)
	}
	fmt.Fprintf(out, "%d files, +%d/-%d lines; %d risk signals; %d reproduced issues.\nFocused review: %d / %d changed lines (a prioritization aid, not a correctness guarantee).\n", len(change.Files), change.Additions, change.Deletions, len(r.Signals), len(r.ReproducedIssues), r.ReviewSurface.FocusedLines, r.ReviewSurface.ChangedLines)
	if r.Coverage.Status == coverage.StatusMeasured {
		fmt.Fprintf(out, "Changed-line execution: %d executed, %d not executed, %d outside any instrumented block, %d not measured, of %d added %s lines.\n", r.Coverage.ExecutedLines, r.Coverage.NotExecutedLines, r.Coverage.NoBlockLines, r.Coverage.NotMeasuredLines, r.Coverage.AddedLines, coverage.Languages(r.Coverage))
	} else {
		fmt.Fprintln(out, "Changed-line execution: not measured.")
	}
	for _, line := range stdoutLines(&r) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "Reports: %s\n", output)
	return r.ExitCode
}

// measure resolves the recorded profile against the diff. Every failure short of
// a complete, mapped measurement returns "not measured": there is no path from
// missing data to a not-executed claim.
func measure(profile []byte, candidateDir string, check model.Check, sha string, change model.Change) coverage.Result {
	parsed, err := coverage.Parse(profile)
	if err != nil {
		return coverage.NotMeasured(err.Error())
	}
	run := coverage.Run{CheckID: check.ID, Status: check.Status, Command: check.Command, SHA256: sha}
	// Only a Go profile keys its files by module path; an LCOV report names
	// them relative to the repository root.
	if parsed.Format == coverage.FormatGo {
		if run.Module, run.Workspace, err = coverage.Modules(candidateDir); err != nil {
			return coverage.NotMeasured(err.Error())
		}
	}
	return coverage.Analyze(parsed, run, change)
}

// loadPolicy reads the trusted policy from commit, the resolved base ref, unless
// the caller explicitly selects a local file. It never reads the candidate.
func loadPolicy(ctx context.Context, repo *gitrepo.Repository, commit, explicit string) (config.Config, model.Policy, error) {
	if explicit != "" {
		policy := model.Policy{Source: model.PolicyExplicit, Path: explicit}
		b, err := readLimited(explicit, 1<<20)
		if err != nil {
			return config.Config{}, policy, err
		}
		cfg, err := config.Decode(b)
		return cfg, policy, err
	}
	policy := model.Policy{Source: model.PolicyBaseRef, Commit: commit, Path: config.Filename}
	b, err := repo.ReadFile(ctx, commit, config.Filename)
	if err == nil {
		cfg, err := config.Decode(b)
		return cfg, policy, err
	}
	if !errors.Is(err, gitrepo.ErrNotFound) {
		return config.Config{}, policy, fmt.Errorf("load base policy: %w", err)
	}
	language := detect(func(name string) bool { _, err := repo.ReadFile(ctx, commit, name); return err == nil })
	return config.Default(language), model.Policy{Source: model.PolicyDefault, Commit: commit}, nil
}

func shortCommit(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func detect(exists func(string) bool) string {
	for _, item := range []struct{ file, language string }{{"go.mod", "go"}, {"go.work", "go"}, {"Cargo.toml", "rust"}, {"tsconfig.json", "typescript"}, {"package.json", "javascript"}, {"pyproject.toml", "python"}, {"setup.py", "python"}, {"requirements.txt", "python"}} {
		if exists(item.file) {
			return item.language
		}
	}
	return "unknown"
}

func render(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("report", flag.ContinueOnError)
	f.SetOutput(errOut)
	input := f.String("input", ".probe/confidence-report.json", "saved JSON report")
	dir := f.String("out", ".probe", "output directory")
	format := f.String("format", "markdown,json", "comma-separated output formats: markdown,json,sarif,pr-comment,pr-summary")
	reportURL := f.String("report-url", "", "https link to the full report, cited by the pr-comment format")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "report accepts no positional arguments")
	}
	formats, err := parseFormats(*format)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	opts, err := reportOptions(formats, *reportURL)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	b, err := readLimited(*input, 64<<20)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	var r model.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if r.Version != 1 {
		return fail(errOut, 3, "unsupported report version %d", r.Version)
	}
	if r.ExitCode < 0 || r.ExitCode > 4 {
		return fail(errOut, 3, "invalid report exit code %d", r.ExitCode)
	}
	previousExit := r.ExitCode
	report.Finalize(&r, previousExit == 2)
	if previousExit == 4 {
		r.ExitCode = 4
	}
	if err := validateOutput(*dir); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if err := report.Write(*dir, &r, formats, opts...); err != nil {
		return fail(errOut, 4, "%v", err)
	}
	fmt.Fprintf(out, "Reports: %s\n", *dir)
	return 0
}

func parseFormats(s string) ([]string, error) {
	var formats []string
	seen := map[string]bool{}
	for _, value := range strings.Split(s, ",") {
		value = strings.TrimSpace(value)
		if !report.ValidFormat(value) {
			return nil, fmt.Errorf("unknown report format %q", value)
		}
		if !seen[value] {
			formats = append(formats, value)
			seen[value] = true
		}
	}
	return formats, nil
}

// loadCodingRules returns the team coding rules of --rules or --rules-file,
// trimmed, or "" when neither is set.
func loadCodingRules(text, path string) (string, error) {
	if path != "" {
		if text != "" {
			return "", errors.New("use either --rules or --rules-file")
		}
		b, err := readLimited(path, reviewer.MaxCodingRulesBytes)
		if err != nil {
			return "", fmt.Errorf("rules: %w", err)
		}
		text = string(b)
	}
	text = strings.TrimSpace(text)
	switch {
	case len(text) > reviewer.MaxCodingRulesBytes:
		return "", fmt.Errorf("coding rules exceed %d bytes", reviewer.MaxCodingRulesBytes)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return "", errors.New("coding rules must be UTF-8 text")
	}
	return text, nil
}

// loadTeamFeedback reads --feedback-file: one JSON object, strictly decoded
// and bounded. It returns nil when the flag is unset.
func loadTeamFeedback(path string) (*model.TeamFeedback, string, error) {
	if path == "" {
		return nil, "", nil
	}
	data, err := readLimited(path, reviewer.MaxFeedbackBytes)
	if err != nil {
		return nil, "", fmt.Errorf("feedback: %w", err)
	}
	var f model.TeamFeedback
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return nil, "", fmt.Errorf("feedback: %w", err)
	}
	if d.More() {
		return nil, "", errors.New("feedback must contain exactly one JSON object")
	}
	if len(f.Topics) > reviewer.MaxFeedbackTopics || len(f.Comments) > reviewer.MaxFeedbackComments {
		return nil, "", fmt.Errorf("feedback holds at most %d topics and %d comments", reviewer.MaxFeedbackTopics, reviewer.MaxFeedbackComments)
	}
	for _, t := range f.Topics {
		if strings.TrimSpace(t.Topic) == "" || t.Useful < 0 || t.NotUseful < 0 || t.Changed < 0 || t.Unchanged < 0 {
			return nil, "", errors.New("feedback topics need a name and non-negative counts")
		}
	}
	for _, c := range f.Comments {
		if strings.TrimSpace(c.Topic) == "" || strings.TrimSpace(c.Comment) == "" {
			return nil, "", errors.New("feedback comments need a topic and a comment")
		}
		if c.Vote != "" && c.Vote != model.FeedbackUp && c.Vote != model.FeedbackDown {
			return nil, "", errors.New(`feedback votes are "up" or "down"`)
		}
	}
	if len(f.Topics) == 0 && len(f.Comments) == 0 {
		return nil, "", nil
	}
	if f.Topics == nil {
		f.Topics = []model.FeedbackTopic{}
	}
	if f.Comments == nil {
		f.Comments = []model.FeedbackComment{}
	}
	return &f, fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return b, nil
}
func validateOutput(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for p := absolute; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) && !fsutil.IsSystemAlias(p) {
			return fmt.Errorf("output path must contain only directories, not links: %s", p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func fail(w io.Writer, code int, format string, args ...any) int {
	fmt.Fprintf(w, "probe: "+format+"\n", args...)
	return code
}
func flagCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 3
}
