package cli

// Native Jira context: --jira KEY (or --jira auto) fetches the issue the
// change implements and makes it part of the intent, so that criteria
// extraction, intent tests, the reviewer and plan compare the implementation
// with the ticket. The issue is untrusted text: it goes through parseIntent
// like any --intent. Jira is contacted only when --jira is given.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/jira"
)

// maxIntentBytes bounds the intent text, whatever its sources.
const maxIntentBytes = 65536

// jiraTruncated ends a Jira intent shortened to fit maxIntentBytes.
const jiraTruncated = "\n\n[Probe truncated the Jira issue to fit the 64 KiB intent limit.]\n"

// jiraFetcher reads one issue; tests replace newJiraFetcher.
type jiraFetcher interface {
	Fetch(ctx context.Context, key string) (jira.Issue, error)
}

var newJiraFetcher = func(cfg jira.Config) jiraFetcher { return &jira.Client{Config: cfg} }

// jiraEnv reads the operator's Jira configuration; tests replace it.
var jiraEnv = func() (jira.Config, error) { return jira.FromEnv(os.Getenv, os.ReadFile) }

// branchEnv lists the CI variables that name the source branch of a change,
// read by --jira auto when the checkout is detached.
var branchEnv = []string{"GITHUB_HEAD_REF", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME", "CI_COMMIT_REF_NAME", "BITBUCKET_BRANCH", "BRANCH_NAME", "GIT_BRANCH"}

func addJiraFlag(f *flag.FlagSet) *string {
	return f.String("jira", "", "Jira issue whose summary, description and acceptance criteria join the intent: a key such as PROJ-123, or \"auto\" to find it in the branch name and commit messages (needs "+jira.URLEnv+")")
}

// checkJiraFlag validates --jira before any repository access or request.
func checkJiraFlag(value string) error {
	if value == "" || value == jira.Auto || jira.ValidKey(value) {
		return nil
	}
	return fmt.Errorf("--jira must be an issue key such as PROJ-123 or %q", jira.Auto)
}

// jiraKeySources returns, in priority order, the texts --jira auto searches:
// the head ref as given, the checked-out branch when the head is HEAD, the CI
// branch variables, then the commit messages (only when base and head are
// commit identifiers).
func jiraKeySources(ctx context.Context, repo *gitrepo.Repository, headRef, base, head string) []string {
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

// jiraIntent resolves --jira and returns the intent text to parse: the issue
// rendered as Markdown followed by the explicit intent. With --jira auto and
// no key in sources, the explicit intent is returned unchanged with a message
// on errOut. Every error exits 3.
func jiraIntent(ctx context.Context, errOut io.Writer, value, explicit string, sources func() []string) (string, error) {
	if value == "" {
		return explicit, nil
	}
	cfg, err := jiraEnv()
	if err != nil {
		return "", fmt.Errorf("jira: %w", err)
	}
	if !cfg.Configured() {
		return "", fmt.Errorf("jira: --jira needs the Jira site in %s", jira.URLEnv)
	}
	key := value
	if value == jira.Auto {
		if key = jira.FindKey(sources()...); key == "" {
			fmt.Fprintln(errOut, "Jira: no issue key found in the branch name or commit messages; continuing without a Jira issue.")
			return explicit, nil
		}
	}
	issue, err := newJiraFetcher(cfg).Fetch(ctx, key)
	if err != nil {
		return "", fmt.Errorf("jira: %w", err)
	}
	text := fitJiraIntent(issue.Intent(), maxIntentBytes-len(explicit)-2)
	summary := strings.Join(strings.Fields(issue.Summary), " ")
	if len(summary) > 80 {
		summary = truncateUTF8(summary, 77) + "..."
	}
	fmt.Fprintf(errOut, "Jira: %s %q joins the intent.\n", issue.Key, summary)
	if strings.TrimSpace(explicit) == "" {
		return text, nil
	}
	return text + "\n\n" + explicit, nil
}

// fitJiraIntent shortens text to at most limit bytes on a rune boundary,
// ending it with jiraTruncated.
func fitJiraIntent(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= len(jiraTruncated) {
		return ""
	}
	return truncateUTF8(text, limit-len(jiraTruncated)) + jiraTruncated
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
