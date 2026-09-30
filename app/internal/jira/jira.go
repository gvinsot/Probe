// Package jira reads one Jira issue and renders it as the intent of a change,
// so that review, lint and plan can compare an implementation with the ticket
// it claims to implement without the caller pasting the ticket by hand.
//
// The issue is untrusted text, exactly like a pull request description: the
// caller feeds the rendered intent through the usual intent parsing (UTF-8
// check, PR-comment removal, redaction, criteria extraction). This package
// only fetches and converts; it never executes anything.
//
// Configuration comes from the operator's environment, never from the
// repository: PROBE_JIRA_URL names the site, PROBE_JIRA_EMAIL with
// PROBE_JIRA_TOKEN authenticates to Jira Cloud (basic auth with an API token),
// and PROBE_JIRA_TOKEN alone is sent as a bearer personal access token (Jira
// Server and Data Center). The token follows the cluster's secret convention:
// the variable, then PROBE_JIRA_TOKEN_FILE, then /run/secrets/PROBE_JIRA_TOKEN.
package jira

import (
	"context"
	"encoding/base64"
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
	URLEnv               = "PROBE_JIRA_URL"
	EmailEnv             = "PROBE_JIRA_EMAIL"
	TokenEnv             = "PROBE_JIRA_TOKEN"
	CriteriaFieldEnv     = "PROBE_JIRA_CRITERIA_FIELD"
	AllowInsecureHTTPEnv = "PROBE_JIRA_ALLOW_INSECURE_HTTP"
)

// Auto is the --jira value that detects the issue key from the branch name
// and the commit messages of the change.
const Auto = "auto"

// Limits of one fetch.
const (
	maxResponseBytes = 4 << 20
	defaultTimeout   = 30 * time.Second
)

var (
	keyPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}-[1-9][0-9]{0,9}$`)
	keySearchPattern = regexp.MustCompile(`[A-Za-z0-9_]+-[0-9]+`)
	fieldPattern     = regexp.MustCompile(`^customfield_[0-9]{1,12}$`)
)

// ValidKey reports whether key is a Jira issue key such as PROJ-123. Only
// such keys are placed in a request path.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

// FindKey returns the first issue key found in the texts, in order, or "".
// A key must stand alone: "PROJ-12" matches in "feature/PROJ-12-login" and
// "PROJ-12: fix", but not inside "XPROJ-12a". Well-known non-issue tokens such
// as UTF-8 or SHA-256 are skipped.
func FindKey(texts ...string) string {
	for _, text := range texts {
		for _, m := range keySearchPattern.FindAllStringIndex(text, -1) {
			key := text[m[0]:m[1]]
			if m[1] < len(text) && isWordByte(text[m[1]]) {
				continue // "PROJ-12a"
			}
			if ValidKey(key) && !ignoredKey(key) {
				return key
			}
		}
	}
	return ""
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ignoredKey rejects tokens shaped like issue keys that name standards.
func ignoredKey(key string) bool {
	project, _, _ := strings.Cut(key, "-")
	switch project {
	case "UTF", "SHA", "ISO", "RFC", "CVE", "CWE", "GHSA", "AES", "HTTP", "TLS", "X", "UCS":
		return true
	}
	return false
}

// Config is the operator's Jira configuration.
type Config struct {
	BaseURL           string // site root, e.g. https://example.atlassian.net
	Email             string // Jira Cloud account for basic auth; "" selects bearer auth
	Token             string // API token (Cloud) or personal access token (Server/DC)
	CriteriaField     string // optional custom field holding acceptance criteria
	AllowInsecureHTTP bool
	TokenSource       string // where the token came from, for messages
}

// Configured reports whether a Jira site is configured.
func (c Config) Configured() bool { return c.BaseURL != "" }

// FromEnv reads the configuration. getenv and readFile are injected so that
// resolution is testable. It returns an error for a value that cannot be
// used; an unset PROBE_JIRA_URL is not an error (Configured is false).
func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var c Config
	c.BaseURL = strings.TrimRight(strings.TrimSpace(getenv(URLEnv)), "/")
	c.Email = strings.TrimSpace(getenv(EmailEnv))
	c.CriteriaField = strings.TrimSpace(getenv(CriteriaFieldEnv))
	if v := strings.TrimSpace(getenv(AllowInsecureHTTPEnv)); v != "" {
		allowed, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", AllowInsecureHTTPEnv)
		}
		c.AllowInsecureHTTP = allowed
	}
	if c.BaseURL == "" {
		return c, nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("%s must be an absolute URL without credentials, query or fragment", URLEnv)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && c.AllowInsecureHTTP:
	case u.Scheme == "http":
		return Config{}, fmt.Errorf("%s must use https (set %s=true to permit http)", URLEnv, AllowInsecureHTTPEnv)
	default:
		return Config{}, fmt.Errorf("%s must use https", URLEnv)
	}
	if c.CriteriaField != "" && !fieldPattern.MatchString(c.CriteriaField) {
		return Config{}, fmt.Errorf("%s must name a custom field such as customfield_10035", CriteriaFieldEnv)
	}
	if strings.ContainsAny(c.Email, "\r\n\x00:") {
		return Config{}, fmt.Errorf("%s must be a single-line account without ':'", EmailEnv)
	}
	token, source, err := issuetext.Secret(TokenEnv, getenv, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("Jira token: %w", err)
	}
	c.Token, c.TokenSource = token, source
	if c.Email != "" && c.Token == "" {
		return Config{}, fmt.Errorf("%s is set but no %s is available", EmailEnv, TokenEnv)
	}
	return c, nil
}

// Issue is the part of a Jira issue that describes the intended change.
type Issue struct {
	Key         string
	URL         string // browse link
	Summary     string
	Type        string
	Status      string
	Labels      []string
	Description string // Markdown
	Criteria    string // Markdown, from the configured criteria field
	APIVersion  int    // 3 (Cloud, ADF) or 2 (Server/DC, wiki markup)
}

// Client fetches issues.
type Client struct {
	Config Config
	HTTP   *http.Client // nil uses a client with a 30s timeout
}

// ErrNotFound is returned when the issue does not exist or is not visible.
var ErrNotFound = errors.New("issue not found or not visible to the configured account")

// Fetch reads one issue. It tries REST API v3 (Jira Cloud, ADF bodies) and
// falls back to v2 (Jira Server and Data Center, wiki-markup bodies) when v3
// does not exist on the site.
func (c *Client) Fetch(ctx context.Context, key string) (Issue, error) {
	if !c.Config.Configured() {
		return Issue{}, fmt.Errorf("no Jira site configured: set %s", URLEnv)
	}
	if !ValidKey(key) {
		return Issue{}, fmt.Errorf("%q is not a Jira issue key such as PROJ-123", key)
	}
	issue, status, err := c.fetch(ctx, key, 3)
	if err == nil {
		return issue, nil
	}
	if status != http.StatusNotFound {
		return Issue{}, err
	}
	issue, status, err = c.fetch(ctx, key, 2)
	if status == http.StatusNotFound {
		return Issue{}, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	return issue, err
}

func (c *Client) fetch(ctx context.Context, key string, version int) (Issue, int, error) {
	fields := "summary,description,issuetype,status,labels"
	if c.Config.CriteriaField != "" {
		fields += "," + c.Config.CriteriaField
	}
	endpoint := fmt.Sprintf("%s/rest/api/%d/issue/%s?fields=%s", c.Config.BaseURL, version, url.PathEscape(key), url.QueryEscape(fields))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Issue{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case c.Config.Email != "":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Config.Email+":"+c.Config.Token)))
	case c.Config.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Config.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	// Never follow a redirect to another origin with the credential.
	guarded := *client
	origin := req.URL.Scheme + "://" + req.URL.Host
	guarded.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme+"://"+next.URL.Host != origin {
			return fmt.Errorf("refusing redirect from %s to another origin", origin)
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	resp, err := guarded.Do(req)
	if err != nil {
		return Issue{}, 0, fmt.Errorf("fetch %s: %w", key, redactError(err, c.Config.Token))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Issue{}, resp.StatusCode, fmt.Errorf("fetch %s: %w", key, err)
	}
	if len(body) > maxResponseBytes {
		return Issue{}, resp.StatusCode, fmt.Errorf("fetch %s: response exceeds %d bytes", key, maxResponseBytes)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return Issue{}, resp.StatusCode, fmt.Errorf("fetch %s: HTTP %d: check %s and %s", key, resp.StatusCode, EmailEnv, TokenEnv)
	default:
		return Issue{}, resp.StatusCode, fmt.Errorf("fetch %s: HTTP %d", key, resp.StatusCode)
	}
	issue, err := decodeIssue(body, c.Config, version)
	if err != nil {
		return Issue{}, resp.StatusCode, fmt.Errorf("fetch %s: %w", key, err)
	}
	if issue.Key == "" {
		issue.Key = key
	}
	issue.URL = c.Config.BaseURL + "/browse/" + url.PathEscape(issue.Key)
	return issue, resp.StatusCode, nil
}

// redactError keeps the token out of an error message.
func redactError(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
}

type issueResponse struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

type named struct {
	Name string `json:"name"`
}

func decodeIssue(body []byte, cfg Config, version int) (Issue, error) {
	if !utf8.Valid(body) {
		return Issue{}, errors.New("response is not UTF-8")
	}
	var r issueResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Issue{}, fmt.Errorf("decode issue: %w", err)
	}
	if r.Key != "" && !ValidKey(r.Key) {
		return Issue{}, fmt.Errorf("response names an invalid issue key")
	}
	issue := Issue{Key: r.Key, APIVersion: version}
	_ = json.Unmarshal(r.Fields["summary"], &issue.Summary)
	var t, s named
	_ = json.Unmarshal(r.Fields["issuetype"], &t)
	_ = json.Unmarshal(r.Fields["status"], &s)
	issue.Type, issue.Status = t.Name, s.Name
	_ = json.Unmarshal(r.Fields["labels"], &issue.Labels)
	issue.Description = richText(r.Fields["description"])
	if cfg.CriteriaField != "" {
		issue.Criteria = richText(r.Fields[cfg.CriteriaField])
	}
	return issue, nil
}

// richText converts a field that is either wiki markup (a JSON string, API v2
// and plain custom fields) or an Atlassian Document (a JSON object, API v3)
// into Markdown. Anything else yields "".
func richText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(WikiToMarkdown(s))
	}
	var n adfNode
	if json.Unmarshal(raw, &n) == nil && n.Type != "" {
		return strings.TrimSpace(ADFToMarkdown(n))
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		var b strings.Builder
		for _, item := range list {
			if item = strings.TrimSpace(item); item != "" {
				b.WriteString("- " + item + "\n")
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// Intent renders the issue as intent text. A configured acceptance criteria
// field becomes an "Acceptance criteria" section, which makes it the only
// source of criteria; without one, the description's own list items (or its
// own "Acceptance criteria" heading) supply them. Metadata is prose, so it
// never becomes a criterion.
func (i Issue) Intent() string {
	var b strings.Builder
	// The title is prose, not a heading: a summary such as "Export the
	// acceptance criteria" must not open a criteria section.
	fmt.Fprintf(&b, "Jira issue %s: %s\n\n", i.Key, oneLine(i.Summary))
	var meta []string
	if i.Type != "" {
		meta = append(meta, "Type: "+oneLine(i.Type))
	}
	if i.Status != "" {
		meta = append(meta, "Status: "+oneLine(i.Status))
	}
	if len(i.Labels) > 0 {
		meta = append(meta, "Labels: "+oneLine(strings.Join(i.Labels, ", ")))
	}
	meta = append(meta, "Link: "+i.URL)
	b.WriteString(strings.Join(meta, " · ") + "\n")
	if d := strings.TrimSpace(i.Description); d != "" {
		// Description headings nest under "Description"; with a configured
		// criteria field, the description's own criteria headings are renamed
		// so that only the field supplies criteria.
		d = issuetext.NestHeadings(d, i.Criteria != "")
		b.WriteString("\n## Description\n\n" + d + "\n")
	}
	if c := strings.TrimSpace(i.Criteria); c != "" {
		b.WriteString("\n## Acceptance criteria\n\n" + c + "\n")
	}
	return b.String()
}

func oneLine(s string) string { return issuetext.OneLine(s) }
