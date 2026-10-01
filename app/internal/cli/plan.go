package cli

// probe plan: a pre-change analysis. The configured provider simulates
// the implementation of an intent at the base commit, through read-only tools,
// and submits a structured plan; Probe evaluates that plan with fixed
// rules (package plan) and writes PLAN.json and PLAN.md. Nothing is executed
// and nothing in the repository is modified. review --plan later checks the
// real diff against the plan's contract.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/plan"
	"github.com/gvinsot/Probe/app/internal/report"
	"github.com/gvinsot/Probe/app/internal/reviewer"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// maxPlanBytes bounds a PLAN.json read by review --plan.
const maxPlanBytes = 4 << 20

// planIndexer builds the static index of a commit; tests may replace it.
var planIndexer = func(ctx context.Context, repo *gitrepo.Repository, commit string) (*symbols.Index, *model.Impact, error) {
	return symbols.IndexCommit(ctx, repo, commit, symbols.Options{Sensitive: harness.IsSensitivePath})
}

func planCommand(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	f := flag.NewFlagSet("plan", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	base := f.String("base", "main", "base branch or revision the plan starts from; its tip supplies the trusted policy")
	policyPath := f.String("config", "", "explicit trusted local configuration (default: policy at the tip of --base)")
	outDir := f.String("out", ".probe", "output directory, relative to repository")
	ci := f.Bool("ci", false, "return 2 when a category is flagged or something is unverified")
	maxIterations := f.Int("max-iterations", 0, "override the provider iteration budget (1..100)")
	intent := f.String("intent", "", "intent of the change to plan")
	intentFile := f.String("intent-file", "", "UTF-8 file containing the intent of the change to plan")
	issues := addIssueFlags(f)
	useReviewer := f.Bool("reviewer", true, "plan needs the configured provider; --reviewer=false is refused")
	reportLanguage := f.String("report-language", "", "natural language the model writes the plan in, such as French (default: English)")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "plan accepts no positional arguments")
	}
	if !*useReviewer {
		return fail(errOut, 3, "plan needs the configured provider: the model writes the plan that Probe evaluates; --reviewer=false cannot be used with plan")
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
	doc, err := parseIntent(*intent)
	if err != nil {
		return fail(errOut, 3, "intent: %v", err)
	}
	if strings.TrimSpace(doc.Text) == "" && !issues.any() {
		return fail(errOut, 3, "plan needs an intent: use --intent-file FILE, --intent TEXT, --jira KEY, --linear KEY, --notion PAGE or --gdoc DOC")
	}
	repo, err := gitrepo.Open(ctx, *repoPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if issues.any() {
		// Before the change exists, only the branch names the issue.
		text, err := issueIntent(ctx, errOut, issues, *intent, func() []string {
			return issueKeySources(ctx, repo, "HEAD", "", "")
		})
		if err != nil {
			return fail(errOut, 3, "%v", err)
		}
		if doc, err = parseIntent(text); err != nil {
			return fail(errOut, 3, "intent: %v", err)
		}
		if strings.TrimSpace(doc.Text) == "" {
			return fail(errOut, 3, "plan needs an intent: no issue was found; use --jira KEY, --linear KEY, --intent-file FILE or --intent TEXT")
		}
	}
	// An empty comparison resolves the base ref and its commit.
	start, err := repo.Analyze(ctx, *base, *base, false)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	commit := start.BaseCommit
	policyCommit := start.BaseRefCommit
	if policyCommit == "" {
		policyCommit = commit
	}
	cfg, policy, err := loadPolicy(ctx, repo, policyCommit, *policyPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if policy.Source == model.PolicyDefault {
		fmt.Fprintf(errOut, "No %s at %s (%s); using built-in %s defaults.\n", config.Filename, start.BaseRef, shortCommit(policy.Commit), cfg.Language)
	}
	if *maxIterations != 0 {
		cfg.Reviewer.MaxIterations = *maxIterations
	}
	if err := cfg.Validate(); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	provider, err := cfg.ResolveReviewer(os.Getenv, os.ReadFile)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if provider.Model == "" {
		return fail(errOut, 3, "plan needs a provider: configure reviewer.model in policy or %s", config.ModelEnv)
	}
	options := reviewer.Options{Endpoint: provider.Endpoint, Model: provider.Model, APIKey: provider.APIKey, Provider: provider.Provider, Temperature: provider.Temperature, MaxIterations: cfg.Reviewer.MaxIterations, Timeout: time.Duration(cfg.Reviewer.TimeoutSeconds) * time.Second, MaxInputBytes: cfg.Reviewer.MaxInputBytes}
	options.AllowInsecureHTTP = provider.AllowInsecureHTTP
	options.Language = *reportLanguage
	if err := reviewer.Validate(options); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if len(provider.Sources) > 0 {
		fmt.Fprintf(errOut, "Reviewer configuration: %s.\n", strings.Join(provider.Sources, ", "))
	}
	output := *outDir
	if !filepath.IsAbs(output) {
		output = filepath.Join(repo.Root, output)
	}
	if output, err = filepath.Abs(output); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if err := validateOutput(output); err != nil {
		return fail(errOut, 3, "%v", err)
	}

	entries, err := repo.Tree(ctx, commit)
	if err != nil {
		return fail(errOut, 4, "base tree: %v", err)
	}
	tree := plan.Tree{}
	var listed []string
	for _, e := range entries {
		if e.Type != "blob" || gitrepo.SafePath(e.Path) != nil {
			continue
		}
		tree[e.Path] = true
		if !harness.IsSensitivePath(e.Path) {
			listed = append(listed, e.Path)
		}
	}
	sort.Strings(listed)
	index, section, err := planIndexer(ctx, repo, commit)
	if err != nil {
		return fail(errOut, 4, "static index: %v", err)
	}
	temp, err := os.MkdirTemp("", "probe-plan-")
	if err != nil {
		return fail(errOut, 4, "%v", err)
	}
	defer os.RemoveAll(temp)
	snapshot := filepath.Join(temp, "base")
	if err := repo.Snapshot(ctx, commit, snapshot); err != nil {
		return fail(errOut, 4, "base snapshot: %v", err)
	}
	// A harness without image or commands executes nothing: only its read
	// tools are offered to the planner.
	hopts := harness.Options{CandidateDir: snapshot, ArtifactDir: filepath.Join(temp, "artifacts")}
	if index != nil {
		hopts.Symbols = index
	}
	h, err := harness.NewContext(ctx, hopts)
	if err != nil {
		return fail(errOut, 4, "harness: %v", err)
	}
	defer h.Close()

	fmt.Fprintln(errOut, "Planning with the configured provider (read-only, nothing is executed)...")
	result, err := reviewer.Plan(ctx, options, reviewer.PlanInput{Intent: doc.Text, IntentCriteria: doc.Criteria, BaseRef: start.BaseRef, BaseCommit: commit, Language: cfg.Language, Files: listed}, planTools{h})
	if err != nil {
		for _, note := range result.Unverified {
			fmt.Fprintln(errOut, note)
		}
		if errors.Is(err, reviewer.ErrNoPlan) {
			return fail(errOut, 4, "%v", err)
		}
		return fail(errOut, 4, "planner: %v", err)
	}
	assessment, contract, notes, err := plan.Evaluate(ctx, plan.Input{
		BaseCommit: commit, Proposal: result.Proposal, SensitiveGlobs: cfg.SensitivePaths, Tree: tree,
		Index: func(context.Context) (*symbols.Index, *model.Impact, error) { return index, section, nil },
	})
	if err != nil {
		return fail(errOut, 4, "evaluate plan: %v", err)
	}
	p := &model.Plan{
		ToolVersion: version, GeneratedAt: time.Now().UTC(), Intent: doc.Text, IntentSHA256: doc.SHA256,
		BaseRef: start.BaseRef, BaseCommit: commit, Policy: policy, Model: provider.Model,
		Proposal: result.Proposal, Assessment: assessment, Contract: contract,
	}
	p.Unverified = append(append(append(p.Unverified, doc.Notes...), result.Unverified...), notes...)
	p.Audit = append(result.Audit, h.Audit()...)
	sort.SliceStable(p.Audit, func(i, j int) bool { return p.Audit[i].Time.Before(p.Audit[j].Time) })
	report.FinalizePlan(p, *ci)
	if err := report.WritePlan(output, p); err != nil {
		return fail(errOut, 4, "write plan: %v", err)
	}
	var flagged []string
	for _, c := range p.Assessment.Categories {
		if c.Flagged {
			flagged = append(flagged, c.Name)
		}
	}
	if len(flagged) == 0 {
		flagged = []string{"none"}
	}
	fmt.Fprintf(out, "Plan: %d files, %d symbols, %d dependencies; %d plan signals.\nFlagged categories: %s (fixed rules applied to the plan; the model did not judge risk).\n", len(p.Proposal.Files), len(p.Proposal.Symbols), len(p.Proposal.Dependencies), len(p.Assessment.Signals), strings.Join(flagged, ", "))
	fmt.Fprintf(out, "Plan: %s\nCheck the implementation with: probe review --plan %s\n", filepath.Join(output, report.PlanJSONName), filepath.Join(output, report.PlanJSONName))
	return p.ExitCode
}

// planTools restricts the harness to the planner's read tools, whatever the
// model asks for.
type planTools struct{ h *harness.Harness }

func (t planTools) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	for _, allowed := range reviewer.PlanReadTools {
		if name == allowed {
			return t.h.Call(ctx, name, args)
		}
	}
	return nil, errors.New("tool is not available")
}

// loadPlanContract reads a PLAN.json for review --plan and returns the drift
// section it seeds, before any Git analysis.
func loadPlanContract(path string) (*model.PlanDrift, model.PlanProposal, error) {
	data, err := readLimited(path, maxPlanBytes)
	if err != nil {
		return nil, model.PlanProposal{}, err
	}
	var p model.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, model.PlanProposal{}, fmt.Errorf("%s is not a plan document: %v", path, err)
	}
	if p.Format != model.PlanFormat || p.Version != model.PlanVersion {
		return nil, model.PlanProposal{}, fmt.Errorf("%s is not a version %d %s document", path, model.PlanVersion, model.PlanFormat)
	}
	if p.Contract.BaseCommit == "" || len(p.Contract.Files) == 0 {
		return nil, model.PlanProposal{}, fmt.Errorf("%s has no contract (base commit and planned files)", path)
	}
	for _, f := range p.Contract.Files {
		if gitrepo.SafePath(f) != nil {
			return nil, model.PlanProposal{}, fmt.Errorf("%s lists an invalid planned path %q", path, f)
		}
	}
	sum := sha256.Sum256(data)
	return &model.PlanDrift{PlanSHA256: hex.EncodeToString(sum[:]), IntentSHA256: p.IntentSHA256, Contract: p.Contract}, p.Proposal, nil
}

// reassessPlan re-applies the fixed plan rules to the plan's proposal at its
// base commit, with the sensitive paths of this review's trusted policy, and
// records the result in the drift section. The assessment and the contract
// stored in PLAN.json are never trusted: the contract is re-derived from the
// proposal, so a contract edited after planning cannot widen the scope
// without the widened scope being assessed. A proposal that cannot be
// re-assessed leaves the assessment unavailable, which requires human review.
// It returns an error only when ctx is cancelled.
func reassessPlan(ctx context.Context, repo *gitrepo.Repository, drift *model.PlanDrift, proposal model.PlanProposal, globs []string) error {
	unavailable := func(reason string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		drift.Assessment = model.PlanGateAssessment{Status: model.PlanUnassessed, FlaggedCategories: []string{}, Gaps: []string{}, Reason: reason}
		return nil
	}
	normalized, err := plan.Normalize(proposal)
	if err != nil {
		return unavailable("the plan's proposal is invalid: " + err.Error())
	}
	commit := drift.Contract.BaseCommit
	entries, err := repo.Tree(ctx, commit)
	if err != nil {
		return unavailable("the plan's base commit could not be read: " + err.Error())
	}
	tree := plan.Tree{}
	for _, e := range entries {
		if e.Type == "blob" && gitrepo.SafePath(e.Path) == nil {
			tree[e.Path] = true
		}
	}
	assessment, contract, _, err := plan.Evaluate(ctx, plan.Input{
		BaseCommit: commit, Proposal: normalized, SensitiveGlobs: globs, Tree: tree,
		Index: func(ctx context.Context) (*symbols.Index, *model.Impact, error) {
			return planIndexer(ctx, repo, commit)
		},
	})
	if err != nil {
		return unavailable("the plan could not be evaluated: " + err.Error())
	}
	gate := model.PlanGateAssessment{Status: model.PlanAssessed, FlaggedCategories: []string{}, Gaps: []string{}}
	for _, c := range assessment.Categories {
		if c.Flagged {
			gate.FlaggedCategories = append(gate.FlaggedCategories, c.Name)
		}
	}
	gate.Major = len(gate.FlaggedCategories) > 0
	counts := map[string]int{}
	for _, s := range assessment.Signals {
		counts[s.Kind]++
	}
	if n := counts[model.PlanSignalUnmeasured]; n > 0 {
		gate.Gaps = append(gate.Gaps, fmt.Sprintf("%d planned symbols could not be measured on the static index", n))
	}
	if n := counts[model.PlanSignalInconsistent]; n > 0 {
		gate.Gaps = append(gate.Gaps, fmt.Sprintf("%d statements of the plan do not match its base commit", n))
	}
	if s := assessment.Index.Status; s == model.ImpactLimited || s == model.ImpactUnavailable {
		gate.Gaps = append(gate.Gaps, "the static index of the base commit is "+s)
	}
	for _, sym := range assessment.Symbols {
		if sym.Found && !sym.Complete {
			gate.Gaps = append(gate.Gaps, "the callers or reaching tests of "+sym.Name+" were not fully searched")
		}
	}
	drift.Assessment = gate
	drift.Contract = contract
	return nil
}

// planDriftLine is the stdout line of the plan_drift section.
func planDriftLine(d *model.PlanDrift) string {
	if d == nil {
		return ""
	}
	counts := map[string]int{}
	for _, it := range d.Items {
		counts[it.Severity]++
	}
	line := fmt.Sprintf("Plan conformance: %s; %d high, %d medium, %d low differences from the plan.", d.Status, counts["high"], counts["medium"], counts["low"])
	if d.Decision == model.PlanDecisionNoReview {
		return line + "\nPlan gate: no human review required (low-risk plan, conforming change, checks passed, nothing else requests review)."
	}
	return line + fmt.Sprintf("\nPlan gate: human review required (%d reasons; see the report).", len(d.DecisionReasons))
}
