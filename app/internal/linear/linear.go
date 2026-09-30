// Package linear reads one Linear issue and renders it as the intent of a
// change, like package jira does for Jira: review, lint and plan then compare
// an implementation with the issue it claims to implement.
//
// The issue is untrusted text, exactly like a pull request description: the
// caller feeds the rendered intent through the usual intent parsing. This
// package only fetches and renders; it never executes anything.
//
// Configuration comes from the operator's environment, never from the
// repository: PROBE_LINEAR_API_KEY (a personal API key, lin_api_..., sent as
// is, or an OAuth access token, sent as a bearer token), also read from
// PROBE_LINEAR_API_KEY_FILE or /run/secrets/PROBE_LINEAR_API_KEY.
// PROBE_LINEAR_TEAMS optionally restricts the team keys --linear auto
// accepts, and PROBE_LINEAR_URL replaces the GraphQL endpoint (a proxy or a
// test server).
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/issuetext"
)

// Environment variables read by FromEnv.
const (
	APIKeyEnv            = "PROBE_LINEAR_API_KEY"
	URLEnv               = "PROBE_LINEAR_URL"
	TeamsEnv             = "PROBE_LINEAR_TEAMS"
	AllowInsecureHTTPEnv = "PROBE_LINEAR_ALLOW_INSECURE_HTTP"
)

// DefaultURL is Linear's GraphQL API.
const DefaultURL = "https://api.linear.app/graphql"

// Auto is the --linear value that detects the issue from the branch name and
// the commit messages of the change.
const Auto = "auto"

const (
	maxResponseBytes = 4 << 20
	defaultTimeout   = 30 * time.Second
	// MaxCandidates bounds the issue keys --linear auto tries.
	MaxCandidates = 5
)

var (
	keyPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[1-9][0-9]{0,9}$`)
	keySearchPattern = regexp.MustCompile(`[A-Za-z0-9_]+-[0-9]+`)
)

// ValidKey reports whether key is a Linear issue identifier such as ENG-123.
// Only such identifiers are sent to the API.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

// FindKeys returns the distinct issue identifiers found in the texts, in
// order, upper-cased, at most MaxCandidates. Matching ignores case because
// Linear's branch names are lower case ("ana/eng-123-fix-login"). A key must
// stand alone, tokens such as UTF-8 or SHA-256 are skipped, and when teams is
// not empty only those team keys are kept.
func FindKeys(teams []string, texts ...string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range keySearchPattern.FindAllStringIndex(text, -1) {
			if m[1] < len(text) && isWordByte(text[m[1]]) {
				continue
			}
			key := strings.ToUpper(text[m[0]:m[1]])
			if !ValidKey(key) || ignoredKey(key) || seen[key] || !teamAllowed(teams, key) {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
			if len(keys) == MaxCandidates {
				return keys
			}
		}
	}
	return keys
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func ignoredKey(key string) bool {
	team, _, _ := strings.Cut(key, "-")
	switch team {
	case "UTF", "SHA", "ISO", "RFC", "CVE", "CWE", "GHSA", "AES", "HTTP", "TLS", "X", "UCS", "V", "PR", "MR", "RELEASE", "HOTFIX", "FEATURE", "FIX", "BUGFIX", "RC", "BETA", "ALPHA":
		return true
	}
	return false
}

func teamAllowed(teams []string, key string) bool {
	if len(teams) == 0 {
		return true
	}
	team, _, _ := strings.Cut(key, "-")
	for _, t := range teams {
		if t == team {
			return true
		}
	}
	return false
}

// Config is the operator's Linear configuration.
type Config struct {
	URL               string
	APIKey            string
	KeySource         string
	Teams             []string
	AllowInsecureHTTP bool
}

// Configured reports whether a credential is available.
func (c Config) Configured() bool { return c.APIKey != "" }

// FromEnv reads the configuration; getenv and readFile are injected so that
// resolution is testable. A missing API key is not an error (Configured is
// false); an unusable value is.
func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	c := Config{URL: strings.TrimSpace(getenv(URLEnv))}
	if c.URL == "" {
		c.URL = DefaultURL
	}
	if v := strings.TrimSpace(getenv(AllowInsecureHTTPEnv)); v != "" {
		allowed, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", AllowInsecureHTTPEnv)
		}
		c.AllowInsecureHTTP = allowed
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return Config{}, fmt.Errorf("%s must be an absolute URL without credentials or fragment", URLEnv)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && c.AllowInsecureHTTP:
	case u.Scheme == "http":
		return Config{}, fmt.Errorf("%s must use https (set %s=true to permit http)", URLEnv, AllowInsecureHTTPEnv)
	default:
		return Config{}, fmt.Errorf("%s must use https", URLEnv)
	}
	for _, team := range strings.Split(getenv(TeamsEnv), ",") {
		team = strings.ToUpper(strings.TrimSpace(team))
		if team == "" {
			continue
		}
		if !ValidKey(team + "-1") {
			return Config{}, fmt.Errorf("%s must list team keys such as ENG,WEB", TeamsEnv)
		}
		c.Teams = append(c.Teams, team)
	}
	key, source, err := issuetext.Secret(APIKeyEnv, getenv, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("Linear API key: %w", err)
	}
	c.APIKey, c.KeySource = key, source
	return c, nil
}

// Issue is the part of a Linear issue that describes the intended change.
type Issue struct {
	Key         string
	URL         string
	Title       string
	Description string // Markdown, as Linear stores it
	Status      string
	Priority    string
	Team        string
	Project     string
	Labels      []string
	Parent      string // "KEY: title" of the parent issue, if any
}

// Client fetches issues.
type Client struct {
	Config Config
	HTTP   *http.Client // nil uses a client with a 30s timeout
}

// ErrNotFound is returned when the issue does not exist or is not visible.
var ErrNotFound = errors.New("issue not found or not visible to the configured API key")

const issueQuery = `query ProbeIssue($id: String!) {
  issue(id: $id) {
    identifier title description url priorityLabel
    state { name }
    team { key name }
    project { name }
    parent { identifier title }
    labels(first: 50) { nodes { name } }
  }
}`

// Fetch reads one issue by its identifier.
func (c *Client) Fetch(ctx context.Context, key string) (Issue, error) {
	if !c.Config.Configured() {
		return Issue{}, fmt.Errorf("no Linear API key configured: set %s", APIKeyEnv)
	}
	if !ValidKey(key) {
		return Issue{}, fmt.Errorf("%q is not a Linear issue identifier such as ENG-123", key)
	}
	payload, err := json.Marshal(map[string]any{"query": issueQuery, "variables": map[string]string{"id": key}})
	if err != nil {
		return Issue{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.URL, bytes.NewReader(payload))
	if err != nil {
		return Issue{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.HasPrefix(c.Config.APIKey, "lin_api_") {
		req.Header.Set("Authorization", c.Config.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Config.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	// A POST with a credential never follows a redirect.
	guarded := *client
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := guarded.Do(req)
	if err != nil {
		return Issue{}, fmt.Errorf("fetch %s: %w", key, redactError(err, c.Config.APIKey))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Issue{}, fmt.Errorf("fetch %s: %w", key, err)
	}
	if len(body) > maxResponseBytes {
		return Issue{}, fmt.Errorf("fetch %s: response exceeds %d bytes", key, maxResponseBytes)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Issue{}, fmt.Errorf("fetch %s: HTTP %d: check %s", key, resp.StatusCode, APIKeyEnv)
	}
	issue, err := decodeIssue(body)
	switch {
	case errors.Is(err, ErrNotFound):
		return Issue{}, fmt.Errorf("%s: %w", key, ErrNotFound)
	case err != nil && resp.StatusCode != http.StatusOK:
		return Issue{}, fmt.Errorf("fetch %s: HTTP %d: %v", key, resp.StatusCode, err)
	case err != nil:
		return Issue{}, fmt.Errorf("fetch %s: %w", key, err)
	}
	if issue.Key == "" {
		issue.Key = key
	}
	return issue, nil
}

func redactError(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED]"))
}

type graphResponse struct {
	Data struct {
		Issue *struct {
			Identifier    string `json:"identifier"`
			Title         string `json:"title"`
			Description   string `json:"description"`
			URL           string `json:"url"`
			PriorityLabel string `json:"priorityLabel"`
			State         *struct {
				Name string `json:"name"`
			} `json:"state"`
			Team *struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"team"`
			Project *struct {
				Name string `json:"name"`
			} `json:"project"`
			Parent *struct {
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
			} `json:"parent"`
			Labels *struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"issue"`
	} `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

func decodeIssue(body []byte) (Issue, error) {
	if !utf8.Valid(body) {
		return Issue{}, errors.New("response is not UTF-8")
	}
	var r graphResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Issue{}, fmt.Errorf("decode issue: %w", err)
	}
	if len(r.Errors) > 0 {
		message := r.Errors[0].Message
		lower := strings.ToLower(message + " " + r.Errors[0].Extensions.Code)
		if strings.Contains(lower, "not found") || strings.Contains(lower, "entity_not_found") {
			return Issue{}, ErrNotFound
		}
		if strings.Contains(lower, "authentication") || strings.Contains(lower, "unauthenticated") {
			return Issue{}, fmt.Errorf("authentication failed: check %s", APIKeyEnv)
		}
		return Issue{}, fmt.Errorf("Linear API error: %s", issuetext.OneLine(truncate(message, 200)))
	}
	i := r.Data.Issue
	if i == nil {
		return Issue{}, ErrNotFound
	}
	if i.Identifier != "" && !ValidKey(i.Identifier) {
		return Issue{}, errors.New("response names an invalid issue identifier")
	}
	issue := Issue{Key: i.Identifier, Title: i.Title, Description: i.Description, Priority: i.PriorityLabel}
	if u, err := url.Parse(i.URL); err == nil && u.Scheme == "https" {
		issue.URL = i.URL
	}
	if i.State != nil {
		issue.Status = i.State.Name
	}
	if i.Team != nil {
		issue.Team = i.Team.Name
	}
	if i.Project != nil {
		issue.Project = i.Project.Name
	}
	if i.Parent != nil && ValidKey(i.Parent.Identifier) {
		issue.Parent = i.Parent.Identifier + ": " + issuetext.OneLine(i.Parent.Title)
	}
	if i.Labels != nil {
		for _, l := range i.Labels.Nodes {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	return issue, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// Intent renders the issue as intent text. The description is Linear's own
// Markdown, its headings nested under "Description"; its list items (or the
// items under its own "Acceptance criteria" heading) become the criteria.
// Metadata is prose, so it never becomes a criterion.
func (i Issue) Intent() string {
	var b strings.Builder
	// The title is prose, not a heading: a title such as "Export the
	// acceptance criteria" must not open a criteria section.
	fmt.Fprintf(&b, "Linear issue %s: %s\n\n", i.Key, issuetext.OneLine(i.Title))
	var meta []string
	for _, field := range [][2]string{{"Team", i.Team}, {"Project", i.Project}, {"Status", i.Status}, {"Priority", i.Priority}, {"Parent", i.Parent}} {
		if v := issuetext.OneLine(field[1]); v != "" {
			meta = append(meta, field[0]+": "+v)
		}
	}
	if len(i.Labels) > 0 {
		meta = append(meta, "Labels: "+issuetext.OneLine(strings.Join(i.Labels, ", ")))
	}
	if i.URL != "" {
		meta = append(meta, "Link: "+i.URL)
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	if d := strings.TrimSpace(strings.ReplaceAll(i.Description, "\r\n", "\n")); d != "" {
		b.WriteString("\n## Description\n\n" + issuetext.NestHeadings(d, false) + "\n")
	}
	return b.String()
}
