package cli

// Native issue-tracker context: --jira KEY and --linear KEY (or "auto")
// fetch the issue a change implements and make it part of the intent, so
// that criteria extraction, intent tests, the reviewer and plan compare the
// implementation with the ticket. The issue is untrusted text: it goes
// through parseIntent like any --intent. A tracker is contacted only when its
// flag is given.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/gdocs"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/jira"
	"github.com/gvinsot/Probe/app/internal/linear"
	"github.com/gvinsot/Probe/app/internal/notion"
)

// maxIntentBytes bounds the intent text, whatever its sources.
const maxIntentBytes = 65536

// issueTruncated ends an issue shortened to fit maxIntentBytes.
func issueTruncated(tracker string) string {
	noun := "issue"
	switch tracker {
	case "Notion":
		noun = "page"
	case "Google Docs":
		tracker, noun = "Google", "document"
	}
	return "\n\n[Probe truncated the " + tracker + " " + noun + " to fit the 64 KiB intent limit.]\n"
}

// jiraFetcher and linearFetcher read one issue; tests may replace the
// constructors and the environment readers.
type jiraFetcher interface {
	Fetch(ctx context.Context, key string) (jira.Issue, error)
}

type linearFetcher interface {
	Fetch(ctx context.Context, key string) (linear.Issue, error)
}

type notionFetcher interface {
	Fetch(ctx context.Context, id string) (notion.Page, error)
}

type gdocsFetcher interface {
	Fetch(ctx context.Context, id string) (gdocs.Document, error)
}

var (
	newJiraFetcher   = func(cfg jira.Config) jiraFetcher { return &jira.Client{Config: cfg} }
	jiraEnv          = func() (jira.Config, error) { return jira.FromEnv(os.Getenv, os.ReadFile) }
	newLinearFetcher = func(cfg linear.Config) linearFetcher { return &linear.Client{Config: cfg} }
	linearEnv        = func() (linear.Config, error) { return linear.FromEnv(os.Getenv, os.ReadFile) }
	newNotionFetcher = func(cfg notion.Config) notionFetcher { return &notion.Client{Config: cfg} }
	notionEnv        = func() (notion.Config, error) { return notion.FromEnv(os.Getenv, os.ReadFile) }
	newGdocsFetcher  = func(cfg gdocs.Config) gdocsFetcher { return &gdocs.Client{Config: cfg} }
	gdocsEnv         = func() (gdocs.Config, error) { return gdocs.FromEnv(os.Getenv, os.ReadFile) }
)

// branchEnv lists the CI variables that name the source branch of a change,
// read by "auto" when the checkout is detached.
var branchEnv = []string{"GITHUB_HEAD_REF", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME", "CI_COMMIT_REF_NAME", "BITBUCKET_BRANCH", "BRANCH_NAME", "GIT_BRANCH"}

// issueFlags are the tracker flags of review, lint and plan.
type issueFlags struct{ jira, linear, notion, gdoc *string }

func (f issueFlags) any() bool {
	return *f.jira != "" || *f.linear != "" || *f.notion != "" || *f.gdoc != ""
}

func addIssueFlags(f *flag.FlagSet) issueFlags {
	return issueFlags{
		jira:   f.String("jira", "", "Jira issue whose summary, description and acceptance criteria join the intent: a key such as PROJ-123, or \"auto\" to find it in the branch name and commit messages (needs "+jira.URLEnv+")"),
		notion: f.String("notion", "", "Notion pages (URLs or IDs, comma-separated, at most 5) whose content joins the intent as product, architecture or requirements context; only list items under their \"Acceptance criteria\" headings become criteria (needs "+notion.TokenEnv+")"),
		gdoc:   f.String("gdoc", "", "Google Docs (URLs or IDs, comma-separated, at most 5), such as design or requirement documents, whose content joins the intent as context; only list items under their \"Acceptance criteria\" headings become criteria (needs "+gdocs.AccessTokenEnv+", "+gdocs.CredentialsEnv+" or "+gdocs.APIKeyEnv+")"),
		linear: f.String("linear", "", "Linear issue whose title and description join the intent: an identifier such as ENG-123, or \"auto\" to find it in the branch name and commit messages (needs "+linear.APIKeyEnv+")"),
	}
}

// check validates the flags before any repository access or request.
func (f issueFlags) check() error {
	if v := *f.jira; v != "" && v != jira.Auto && !jira.ValidKey(v) {
		return fmt.Errorf("--jira must be an issue key such as PROJ-123 or %q", jira.Auto)
	}
	if v := *f.linear; v != "" && v != linear.Auto && !linear.ValidKey(v) {
		return fmt.Errorf("--linear must be an issue identifier such as ENG-123 or %q", linear.Auto)
	}
	if *f.notion != "" {
		if _, err := notion.ParsePages(*f.notion); err != nil {
			return fmt.Errorf("--notion: %w", err)
		}
	}
	if *f.gdoc != "" {
		if _, err := gdocs.ParseDocuments(*f.gdoc); err != nil {
			return fmt.Errorf("--gdoc: %w", err)
		}
	}
	return nil
}

// issueKeySources returns, in priority order, the texts "auto" searches: the
// head ref as given, the checked-out branch when the head is HEAD, the CI
// branch variables, then the commit messages (only when base and head are
// commit identifiers).
func issueKeySources(ctx context.Context, repo *gitrepo.Repository, headRef, base, head string) []string {
	var texts []string
	if headRef != "" && headRef != "HEAD" {
		texts = append(texts, headRef)
	}
	if headRef == "" || headRef == "HEAD" {
		if branch := repo.CurrentBranch(ctx); branch != "" {
			texts = append(texts, branch)
		}
	}
	for _, name := range branchEnv {
		if v := os.Getenv(name); v != "" {
			texts = append(texts, v)
		}
	}
	if base != "" && head != "" && base != head {
		if messages, err := repo.CommitMessages(ctx, base, head); err == nil {
			texts = append(texts, messages...)
		}
	}
	return texts
}

// trackerIssue is a fetched issue, rendered.
type trackerIssue struct {
	tracker, key, title, intent string
}

// issueIntent resolves the tracker flags and returns the intent text to
// parse: the Jira issue, then the Linear issue, then the Notion pages, then
// the Google Docs, then the explicit intent.
// With "auto" and no issue found, that tracker adds nothing and a message
// goes to errOut. Every error exits 3.
func issueIntent(ctx context.Context, errOut io.Writer, flags issueFlags, explicit string, sources func() []string) (string, error) {
	var cached []string
	texts := func() []string {
		if cached == nil {
			cached = append([]string{}, sources()...)
		}
		return cached
	}
	var issues []trackerIssue
	if *flags.jira != "" {
		issue, err := jiraIssue(ctx, errOut, *flags.jira, texts)
		if err != nil {
			return "", fmt.Errorf("jira: %w", err)
		}
		if issue != nil {
			issues = append(issues, *issue)
		}
	}
	if *flags.linear != "" {
		issue, err := linearIssue(ctx, errOut, *flags.linear, texts)
		if err != nil {
			return "", fmt.Errorf("linear: %w", err)
		}
		if issue != nil {
			issues = append(issues, *issue)
		}
	}
	if *flags.notion != "" {
		pages, err := notionPages(ctx, *flags.notion)
		if err != nil {
			return "", fmt.Errorf("notion: %w", err)
		}
		issues = append(issues, pages...)
	}
	if *flags.gdoc != "" {
		docs, err := googleDocs(ctx, *flags.gdoc)
		if err != nil {
			return "", fmt.Errorf("gdoc: %w", err)
		}
		issues = append(issues, docs...)
	}
	budget := maxIntentBytes - len(explicit)
	var parts []string
	for _, issue := range issues {
		text := fitIssueIntent(issue.intent, budget-2, issue.tracker)
		if text == "" {
			fmt.Fprintf(errOut, "%s: %s does not fit in the 64 KiB intent limit; left out.\n", issue.tracker, issue.key)
			continue
		}
		budget -= len(text) + 2
		parts = append(parts, text)
		title := strings.Join(strings.Fields(issue.title), " ")
		if len(title) > 80 {
			title = truncateUTF8(title, 77) + "..."
		}
		fmt.Fprintf(errOut, "%s: %s %q joins the intent.\n", issue.tracker, issue.key, title)
	}
	if strings.TrimSpace(explicit) != "" || len(parts) == 0 {
		parts = append(parts, explicit)
	}
	return strings.Join(parts, "\n\n"), nil
}

func jiraIssue(ctx context.Context, errOut io.Writer, value string, sources func() []string) (*trackerIssue, error) {
	cfg, err := jiraEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, fmt.Errorf("--jira needs the Jira site in %s", jira.URLEnv)
	}
	key := value
	if value == jira.Auto {
		if key = jira.FindKey(sources()...); key == "" {
			fmt.Fprintln(errOut, "Jira: no issue key found in the branch name or commit messages; continuing without a Jira issue.")
			return nil, nil
		}
	}
	issue, err := newJiraFetcher(cfg).Fetch(ctx, key)
	if err != nil {
		return nil, err
	}
	return &trackerIssue{tracker: "Jira", key: issue.Key, title: issue.Summary, intent: issue.Intent()}, nil
}

// linearIssue fetches --linear. With "auto", Linear's lower-case branch
// names make false candidates likely ("release-2"), so the candidates are
// tried in order and one Linear does not know is skipped.
func linearIssue(ctx context.Context, errOut io.Writer, value string, sources func() []string) (*trackerIssue, error) {
	cfg, err := linearEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, fmt.Errorf("--linear needs an API key in %s", linear.APIKeyEnv)
	}
	keys := []string{value}
	if value == linear.Auto {
		keys = linear.FindKeys(cfg.Teams, sources()...)
	}
	fetcher := newLinearFetcher(cfg)
	for _, key := range keys {
		issue, err := fetcher.Fetch(ctx, key)
		if value == linear.Auto && errors.Is(err, linear.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &trackerIssue{tracker: "Linear", key: issue.Key, title: issue.Title, intent: issue.Intent()}, nil
	}
	fmt.Fprintln(errOut, "Linear: no issue found from the branch name or commit messages; continuing without a Linear issue.")
	return nil, nil
}

// notionPages fetches the --notion pages, in the order given.
func notionPages(ctx context.Context, list string) ([]trackerIssue, error) {
	ids, err := notion.ParsePages(list)
	if err != nil {
		return nil, err
	}
	cfg, err := notionEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, fmt.Errorf("--notion needs an integration token in %s", notion.TokenEnv)
	}
	fetcher := newNotionFetcher(cfg)
	var pages []trackerIssue
	for _, id := range ids {
		page, err := fetcher.Fetch(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("page %s: %w", id, err)
		}
		pages = append(pages, trackerIssue{tracker: "Notion", key: "page", title: page.Title, intent: page.Intent()})
	}
	return pages, nil
}

// googleDocs fetches the --gdoc documents, in the order given.
func googleDocs(ctx context.Context, list string) ([]trackerIssue, error) {
	ids, err := gdocs.ParseDocuments(list)
	if err != nil {
		return nil, err
	}
	cfg, err := gdocsEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, fmt.Errorf("--gdoc needs a Google credential: %s, %s or %s", gdocs.AccessTokenEnv, gdocs.CredentialsEnv, gdocs.APIKeyEnv)
	}
	fetcher := newGdocsFetcher(cfg)
	var docs []trackerIssue
	for _, id := range ids {
		doc, err := fetcher.Fetch(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("document %s: %w", id, err)
		}
		docs = append(docs, trackerIssue{tracker: "Google Docs", key: "document", title: doc.Title, intent: doc.Intent()})
	}
	return docs, nil
}

// fitIssueIntent shortens text to at most limit bytes on a rune boundary,
// ending it with the truncation note of tracker; "" when even that does not
// fit.
func fitIssueIntent(text string, limit int, tracker string) string {
	if len(text) <= limit {
		return text
	}
	note := issueTruncated(tracker)
	if limit <= len(note) {
		return ""
	}
	return truncateUTF8(text, limit-len(note)) + note
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
