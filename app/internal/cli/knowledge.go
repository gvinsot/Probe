package cli

// The codebase knowledge base. A review reads it at the tip of the base ref,
// like the policy, gives the reviewer the entries relevant to the change, and
// writes the updates the reviewer proposes to the output directory. probe
// knowledge build proposes entries from a read-only exploration of the base
// commit, apply merges proposed updates into the working tree for a person to
// review and commit, and check validates manual edits.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/gvinsot/Probe/app/internal/knowledge"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

// knowledgeBudget bounds the knowledge text a review gives the reviewer.
const knowledgeBudget = 24 * 1024

// noKnowledge disables the knowledge base.
const noKnowledge = "none"

// loadReviewKnowledge reads the knowledge base at commit and selects the
// entries relevant to the change. A missing file is an empty base.
func loadReviewKnowledge(ctx context.Context, repo *gitrepo.Repository, commit, path string, change model.Change) (*model.Knowledge, *knowledge.Base, error) {
	base, data, err := knowledge.Read(func(p string) ([]byte, error) { return repo.ReadFile(ctx, commit, p) }, path)
	if err != nil {
		return nil, nil, err
	}
	k := &model.Knowledge{Path: path, Commit: commit, EntriesTotal: len(base.Entries), Updates: []model.KnowledgeUpdate{}}
	if data != nil {
		sum := sha256.Sum256(data)
		k.SHA256 = hex.EncodeToString(sum[:])
	}
	var changed []string
	for _, f := range change.Files {
		changed = append(changed, f.Path)
		if f.OldPath != "" {
			changed = append(changed, f.OldPath)
		}
	}
	k.Entries = base.Relevant(changed, knowledgeBudget)
	return k, base, nil
}

// writeKnowledgeProposal writes knowledge-updates.json and a KNOWLEDGE.md
// preview of the base merged with the updates.
func writeKnowledgeProposal(output string, p model.KnowledgeProposal, base *knowledge.Base, stamp string) error {
	p.Format, p.Version = model.KnowledgeUpdatesFormat, 1
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	if err := knowledge.WriteFile(filepath.Join(output, knowledge.ProposalName), append(data, '\n')); err != nil {
		return err
	}
	preview := &knowledge.Base{Preamble: base.Preamble, Entries: append([]model.KnowledgeEntry{}, base.Entries...)}
	preview.Merge(p.Updates, stamp)
	return knowledge.WriteFile(filepath.Join(output, knowledge.PreviewName), preview.Render())
}

func knowledgeStamp(source, commit string) string {
	return time.Now().UTC().Format("2006-01-02") + " (" + source + " of " + shortCommit(commit) + ")"
}

func knowledgeCommand(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	if len(args) == 0 {
		return fail(errOut, 3, "usage: probe knowledge build|apply|check [flags]")
	}
	switch args[0] {
	case "build":
		return knowledgeBuild(ctx, args[1:], out, errOut, version)
	case "apply":
		return knowledgeApply(args[1:], out, errOut)
	case "check":
		return knowledgeCheck(args[1:], out, errOut)
	}
	return fail(errOut, 3, "unknown knowledge command %q: use build, apply or check", args[0])
}

// workingPath resolves the knowledge base in the working tree of repoPath.
func workingPath(ctx context.Context, repoPath, path string) (*gitrepo.Repository, string, error) {
	if err := knowledge.ValidPath(path); err != nil {
		return nil, "", err
	}
	repo, err := gitrepo.Open(ctx, repoPath)
	if err != nil {
		return nil, "", err
	}
	return repo, filepath.Join(repo.Root, filepath.FromSlash(path)), nil
}

// knowledgeCheck validates the knowledge base of the working tree.
func knowledgeCheck(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("knowledge check", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	path := f.String("knowledge", knowledge.DefaultPath, "knowledge base, relative to the repository")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	_, file, err := workingPath(context.Background(), *repoPath, *path)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	data, err := readLimited(file, knowledge.MaxFileBytes)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	base, err := knowledge.Parse(data)
	if err != nil {
		return fail(errOut, 1, "%s: %v", *path, err)
	}
	counts := map[string]int{}
	for _, e := range base.Entries {
		counts[e.Kind]++
	}
	var parts []string
	for _, kind := range model.KnowledgeKinds {
		if counts[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
		}
	}
	summary := "no entry"
	if len(parts) > 0 {
		summary = strings.Join(parts, ", ")
	}
	fmt.Fprintf(out, "%s: %d entries (%s).\n", *path, len(base.Entries), summary)
	return 0
}

// knowledgeApply merges proposed updates into the working-tree knowledge
// base. It never commits: the person reviews the diff and commits it.
func knowledgeApply(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("knowledge apply", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	outDir := f.String("out", ".probe", "directory holding knowledge-updates.json, relative to the repository")
	from := f.String("from", "", "explicit knowledge-updates.json (default: in --out)")
	path := f.String("knowledge", "", "knowledge base, relative to the repository (default: the path recorded in the proposal)")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	repo, err := gitrepo.Open(context.Background(), *repoPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	source := *from
	if source == "" {
		dir := *outDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(repo.Root, dir)
		}
		source = filepath.Join(dir, knowledge.ProposalName)
	}
	data, err := readLimited(source, 1<<20)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	var p model.KnowledgeProposal
	if err := json.Unmarshal(data, &p); err != nil || p.Format != model.KnowledgeUpdatesFormat || p.Version != 1 {
		return fail(errOut, 3, "%s is not a version 1 knowledge-updates document", source)
	}
	for _, u := range p.Updates {
		if err := knowledge.ValidUpdate(u); err != nil {
			return fail(errOut, 3, "update %q: %v", u.Title, err)
		}
	}
	target := *path
	if target == "" {
		target = p.Path
	}
	if target == "" {
		target = knowledge.DefaultPath
	}
	if err := knowledge.ValidPath(target); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	file := filepath.Join(repo.Root, filepath.FromSlash(target))
	base, _, err := knowledge.Read(func(string) ([]byte, error) { return readLimited(file, knowledge.MaxFileBytes) }, target)
	if err != nil {
		return fail(errOut, 3, "%v (fix it, or check it with probe knowledge check)", err)
	}
	commit := p.HeadCommit
	if commit == "" {
		commit = p.BaseCommit
	}
	added, replaced, removed := base.Merge(p.Updates, knowledgeStamp(p.Source, commit))
	if len(base.Entries) > knowledge.MaxEntries {
		return fail(errOut, 3, "the merged knowledge base would hold more than %d entries", knowledge.MaxEntries)
	}
	rendered := base.Render()
	if _, err := knowledge.Parse(rendered); err != nil {
		return fail(errOut, 3, "the merged knowledge base is invalid: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if err := knowledge.WriteFile(file, rendered); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	fmt.Fprintf(out, "%s: %d added, %d replaced, %d removed. Review the diff, edit as needed, and commit it: Probe reads the knowledge base from the base branch.\n", target, added, replaced, removed)
	return 0
}

// knowledgeBuild proposes knowledge entries from a read-only exploration of
// the base commit by the configured provider.
func knowledgeBuild(ctx context.Context, args []string, out, errOut io.Writer, version string) int {
	f := flag.NewFlagSet("knowledge build", flag.ContinueOnError)
	f.SetOutput(errOut)
	repoPath := f.String("repo", ".", "repository directory")
	base := f.String("base", "main", "branch or revision to explore; its tip supplies the trusted policy and the knowledge base")
	policyPath := f.String("config", "", "explicit trusted local configuration (default: policy at the tip of --base)")
	outDir := f.String("out", ".probe", "output directory, relative to repository")
	path := f.String("knowledge", knowledge.DefaultPath, "knowledge base, relative to the repository")
	focus := f.String("focus", "", "optional part of the codebase or question to concentrate on, e.g. \"the payment flow\"")
	maxIterations := f.Int("max-iterations", 0, "override the provider iteration budget (1..100)")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "knowledge build accepts no positional arguments")
	}
	if len(*focus) > 2000 {
		return fail(errOut, 3, "--focus exceeds 2000 bytes")
	}
	if err := knowledge.ValidPath(*path); err != nil {
		return fail(errOut, 3, "%v", err)
	}
	repo, err := gitrepo.Open(ctx, *repoPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	start, err := repo.Analyze(ctx, *base, *base, false)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	commit := start.BaseRefCommit
	if commit == "" {
		commit = start.BaseCommit
	}
	cfg, policy, err := loadPolicy(ctx, repo, commit, *policyPath)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if policy.Source == model.PolicyDefault {
		fmt.Fprintf(errOut, "No %s at %s (%s); using built-in %s defaults.\n", config.Filename, start.BaseRef, shortCommit(commit), cfg.Language)
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
		return fail(errOut, 3, "knowledge build needs a provider: configure reviewer.model in policy or %s", config.ModelEnv)
	}
	options := reviewer.Options{Endpoint: provider.Endpoint, Model: provider.Model, APIKey: provider.APIKey, Provider: provider.Provider, Temperature: provider.Temperature, MaxIterations: cfg.Reviewer.MaxIterations, Timeout: time.Duration(cfg.Reviewer.TimeoutSeconds) * time.Second, MaxInputBytes: cfg.Reviewer.MaxInputBytes, AllowInsecureHTTP: provider.AllowInsecureHTTP}
	if err := reviewer.Validate(options); err != nil {
		return fail(errOut, 3, "%v", err)
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
	kb, _, err := knowledge.Read(func(p string) ([]byte, error) { return repo.ReadFile(ctx, commit, p) }, *path)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	entries, err := repo.Tree(ctx, commit)
	if err != nil {
		return fail(errOut, 4, "tree: %v", err)
	}
	var listed []string
	for _, e := range entries {
		if e.Type == "blob" && gitrepo.SafePath(e.Path) == nil && !harness.IsSensitivePath(e.Path) {
			listed = append(listed, e.Path)
		}
	}
	sort.Strings(listed)
	index, _, err := planIndexer(ctx, repo, commit)
	if err != nil {
		return fail(errOut, 4, "static index: %v", err)
	}
	temp, err := os.MkdirTemp("", "probe-knowledge-")
	if err != nil {
		return fail(errOut, 4, "%v", err)
	}
	defer os.RemoveAll(temp)
	snapshot := filepath.Join(temp, "base")
	if err := repo.Snapshot(ctx, commit, snapshot); err != nil {
		return fail(errOut, 4, "snapshot: %v", err)
	}
	hopts := harness.Options{CandidateDir: snapshot, ArtifactDir: filepath.Join(temp, "artifacts")}
	if index != nil {
		hopts.Symbols = index
	}
	h, err := harness.NewContext(ctx, hopts)
	if err != nil {
		return fail(errOut, 4, "harness: %v", err)
	}
	defer h.Close()
	fmt.Fprintln(errOut, "Exploring the codebase with the configured provider (read-only, nothing is executed)...")
	result, err := reviewer.BuildKnowledge(ctx, options, reviewer.KnowledgeInput{BaseRef: start.BaseRef, BaseCommit: commit, Language: cfg.Language, Focus: *focus, Files: listed, Existing: kb.Entries}, planTools{h})
	for _, note := range result.Unverified {
		fmt.Fprintln(errOut, note)
	}
	if err != nil {
		return fail(errOut, 4, "knowledge build: %v", err)
	}
	if len(result.Updates) == 0 {
		fmt.Fprintln(out, "Knowledge build: no update proposed.")
		return 0
	}
	p := model.KnowledgeProposal{ToolVersion: version, Source: "build", Path: *path, BaseCommit: commit, Updates: result.Updates}
	if err := writeKnowledgeProposal(output, p, kb, knowledgeStamp("build", commit)); err != nil {
		return fail(errOut, 4, "write knowledge proposal: %v", err)
	}
	fmt.Fprintf(out, "Knowledge build: %d updates proposed (model output, not evidence).\nPreview: %s\nApply with: probe knowledge apply, then review and commit %s.\n", len(result.Updates), filepath.Join(output, knowledge.PreviewName), *path)
	return 0
}
