// Package notion reads Notion pages and renders them as context for the
// intent of a change: product notes, architecture decisions or requirements
// that review, lint and plan should compare the implementation with.
//
// A page is reference material, not a ticket, so its list items are not
// acceptance criteria unless the page puts them under an "Acceptance
// criteria" heading (issuetext.ProseLists). The page is untrusted text: the
// caller feeds the rendered intent through the usual intent parsing. This
// package only fetches and renders; it never executes anything.
//
// Configuration comes from the operator's environment, never from the
// repository: PROBE_NOTION_TOKEN is the secret of a Notion integration the
// pages are shared with (also read from PROBE_NOTION_TOKEN_FILE or
// /run/secrets/PROBE_NOTION_TOKEN). PROBE_NOTION_URL replaces the API root (a
// proxy or a test server).
package notion

import (
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
	TokenEnv             = "PROBE_NOTION_TOKEN"
	URLEnv               = "PROBE_NOTION_URL"
	AllowInsecureHTTPEnv = "PROBE_NOTION_ALLOW_INSECURE_HTTP"
)

// DefaultURL is the Notion API root.
const DefaultURL = "https://api.notion.com"

// APIVersion is the Notion-Version header sent with every request.
const APIVersion = "2022-06-28"

// Limits of one page.
const (
	MaxPages         = 5 // pages per run
	maxDepth         = 6 // nesting of child blocks read
	maxBlocks        = 2000
	maxRequests      = 60 // API calls per page
	maxResponseBytes = 4 << 20
	defaultTimeout   = 30 * time.Second
)

var (
	hexID  = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)
	pageID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ParsePage returns the canonical (dashed, lower-case) ID of a page given as
// an ID, with or without dashes, or as a Notion URL
// (https://www.notion.so/acme/Checkout-rules-0123...cdef?pvs=4). A URL must
// name a Notion host; the ID is the last one it contains.
func ParsePage(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	candidate := ref
	if strings.Contains(ref, "://") {
		u, err := url.Parse(ref)
		if err != nil || u.Scheme != "https" || !notionHost(u.Hostname()) {
			return "", fmt.Errorf("%q is not a Notion page URL", ref)
		}
		ids := hexID.FindAllString(u.Path, -1)
		if len(ids) == 0 {
			return "", fmt.Errorf("%q names no Notion page ID", ref)
		}
		candidate = ids[len(ids)-1]
	}
	if len(candidate) > 36 || !hexID.MatchString(candidate) || hexID.FindString(candidate) != candidate {
		return "", fmt.Errorf("%q is not a Notion page ID or URL", ref)
	}
	h := strings.ToLower(strings.ReplaceAll(candidate, "-", ""))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func notionHost(host string) bool {
	host = strings.ToLower(host)
	return host == "notion.so" || strings.HasSuffix(host, ".notion.so") || host == "notion.site" || strings.HasSuffix(host, ".notion.site")
}

// ParsePages splits a comma-separated list, parses every page and removes
// duplicates. It accepts at most MaxPages pages.
func ParsePages(list string) ([]string, error) {
	var pages []string
	seen := map[string]bool{}
	for _, ref := range strings.Split(list, ",") {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		id, err := ParsePage(ref)
		if err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			pages = append(pages, id)
		}
	}
	if len(pages) == 0 {
		return nil, errors.New("name at least one Notion page")
	}
	if len(pages) > MaxPages {
		return nil, fmt.Errorf("at most %d Notion pages", MaxPages)
	}
	return pages, nil
}

// Config is the operator's Notion configuration.
type Config struct {
	URL               string
	Token             string
	TokenSource       string
	AllowInsecureHTTP bool
}

// Configured reports whether a token is available.
func (c Config) Configured() bool { return c.Token != "" }

// FromEnv reads the configuration; a missing token is not an error
// (Configured is false), an unusable value is.
func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	c := Config{URL: strings.TrimRight(strings.TrimSpace(getenv(URLEnv)), "/")}
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
	token, source, err := issuetext.Secret(TokenEnv, getenv, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("Notion token: %w", err)
	}
	c.Token, c.TokenSource = token, source
	return c, nil
}

// Page is one rendered Notion page.
type Page struct {
	ID         string
	URL        string
	Title      string
	LastEdited string
	Markdown   string // the page content, headings as written
	Truncated  bool   // a block, depth or request limit was reached
}

// Client fetches pages.
type Client struct {
	Config Config
	HTTP   *http.Client // nil uses a client with a 30s timeout
}

// ErrNotFound is returned when the page does not exist or is not shared with
// the integration.
var ErrNotFound = errors.New("page not found or not shared with the Notion integration")

// Fetch reads a page and its content.
func (c *Client) Fetch(ctx context.Context, id string) (Page, error) {
	if !c.Config.Configured() {
		return Page{}, fmt.Errorf("no Notion token configured: set %s", TokenEnv)
	}
	if !pageID.MatchString(id) {
		return Page{}, fmt.Errorf("%q is not a canonical Notion page ID", id)
	}
	f := &fetcher{client: c, ctx: ctx}
	var meta struct {
		Object         string                     `json:"object"`
		URL            string                     `json:"url"`
		LastEditedTime string                     `json:"last_edited_time"`
		InTrash        bool                       `json:"in_trash"`
		Archived       bool                       `json:"archived"`
		Properties     map[string]json.RawMessage `json:"properties"`
	}
	if err := f.get("/v1/pages/"+id, nil, &meta); err != nil {
		return Page{}, err
	}
	page := Page{ID: id, LastEdited: meta.LastEditedTime}
	if u, err := url.Parse(meta.URL); err == nil && u.Scheme == "https" && notionHost(u.Hostname()) {
		page.URL = meta.URL
	}
	for _, raw := range meta.Properties {
		var p struct {
			Type  string     `json:"type"`
			Title []richText `json:"title"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Type == "title" {
			page.Title = plain(p.Title)
		}
	}
	var b strings.Builder
	f.blocks(&b, id, "", 0)
	page.Markdown = collapseBlankLines(b.String())
	page.Truncated = f.truncated
	return page, nil
}

// fetcher carries the per-page budget.
type fetcher struct {
	client    *Client
	ctx       context.Context
	requests  int
	blockSeen int
	truncated bool
}

func (f *fetcher) get(path string, query url.Values, out any) error {
	if f.requests >= maxRequests {
		f.truncated = true
		return errBudget
	}
	f.requests++
	target := f.client.Config.URL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.client.Config.Token)
	req.Header.Set("Notion-Version", APIVersion)
	req.Header.Set("Accept", "application/json")
	client := f.client.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	guarded := *client
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := guarded.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", redactError(err, f.client.Config.Token))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("fetch: response exceeds %d bytes", maxResponseBytes)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("HTTP 401: check %s", TokenEnv)
	case http.StatusForbidden:
		return ErrNotFound
	default:
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, issuetext.OneLine(truncate(e.Message, 200)))
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if !utf8.Valid(body) {
		return errors.New("response is not UTF-8")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

var errBudget = errors.New("request budget exhausted")

func redactError(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED]"))
}

type richText struct {
	PlainText string `json:"plain_text"`
	Href      string `json:"href"`
}

func plain(texts []richText) string {
	var b strings.Builder
	for _, t := range texts {
		b.WriteString(t.PlainText)
	}
	return b.String()
}

type block struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	HasChildren bool   `json:"has_children"`
}

type blockContent struct {
	RichText   []richText   `json:"rich_text"`
	Checked    bool         `json:"checked"`
	Language   string       `json:"language"`
	Title      string       `json:"title"`
	URL        string       `json:"url"`
	Caption    []richText   `json:"caption"`
	Cells      [][]richText `json:"cells"`
	Expression string       `json:"expression"`
}

// blocks writes the children of parent as Markdown; indent prefixes the
// lines of nested list items.
func (f *fetcher) blocks(b *strings.Builder, parent, indent string, depth int) {
	if depth > maxDepth {
		f.truncated = true
		return
	}
	cursor := ""
	number := 0
	for {
		query := url.Values{"page_size": {"100"}}
		if cursor != "" {
			query.Set("start_cursor", cursor)
		}
		var list struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := f.get("/v1/blocks/"+parent+"/children", query, &list); err != nil {
			if !errors.Is(err, errBudget) {
				f.truncated = true
			}
			return
		}
		for _, raw := range list.Results {
			if f.blockSeen >= maxBlocks {
				f.truncated = true
				return
			}
			f.blockSeen++
			var blk block
			if json.Unmarshal(raw, &blk) != nil {
				continue
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			var content blockContent
			_ = json.Unmarshal(fields[blk.Type], &content)
			if blk.Type == "numbered_list_item" {
				number++
			} else {
				number = 0
			}
			f.block(b, blk, content, indent, depth, number)
		}
		if !list.HasMore || list.NextCursor == "" {
			return
		}
		cursor = list.NextCursor
	}
}

func (f *fetcher) block(b *strings.Builder, blk block, c blockContent, indent string, depth, number int) {
	text := plain(c.RichText)
	children := func(childIndent string) {
		if blk.HasChildren {
			f.blocks(b, blk.ID, childIndent, depth+1)
		}
	}
	line := func(s string) { b.WriteString(indent + strings.ReplaceAll(s, "\n", "\n"+indent) + "\n") }
	switch blk.Type {
	case "paragraph":
		line(text)
		b.WriteString("\n")
		children(indent)
	case "heading_1", "heading_2", "heading_3":
		level := int(blk.Type[len(blk.Type)-1] - '0')
		b.WriteString("\n" + strings.Repeat("#", level) + " " + issuetext.OneLine(text) + "\n\n")
		children(indent) // toggleable headings
	case "bulleted_list_item":
		line("- " + issuetext.OneLine(text))
		children(indent + "  ")
	case "numbered_list_item":
		line(strconv.Itoa(number) + ". " + issuetext.OneLine(text))
		children(indent + "   ")
	case "to_do":
		box := "[ ]"
		if c.Checked {
			box = "[x]"
		}
		line("- " + box + " " + issuetext.OneLine(text))
		children(indent + "  ")
	case "toggle", "quote", "callout":
		line(text)
		b.WriteString("\n")
		children(indent)
	case "code":
		b.WriteString(indent + "```" + issuetext.OneLine(c.Language) + "\n")
		line(strings.ReplaceAll(text, "```", "` ` `"))
		b.WriteString(indent + "```\n\n")
	case "equation":
		line(c.Expression)
	case "divider":
		b.WriteString("\n")
	case "child_page", "child_database":
		// Sub-pages are named, not read: name them with --notion to add them.
		line("Sub-page: " + issuetext.OneLine(c.Title))
	case "bookmark", "link_preview", "embed", "link_to_page":
		if c.URL != "" {
			line(c.URL)
		}
	case "table":
		f.blocks(b, blk.ID, indent, depth+1)
		b.WriteString("\n")
	case "table_row":
		cells := make([]string, len(c.Cells))
		for i, cell := range c.Cells {
			cells[i] = strings.ReplaceAll(issuetext.OneLine(plain(cell)), "|", "\\|")
		}
		line("| " + strings.Join(cells, " | ") + " |")
	case "image", "video", "file", "pdf", "audio":
		if caption := issuetext.OneLine(plain(c.Caption)); caption != "" {
			line(caption)
		}
	default: // column_list, column, synced_block, template, ...
		if text != "" {
			line(text)
		}
		children(indent)
	}
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

var blankLines = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(s string) string {
	return blankLines.ReplaceAllString(strings.TrimSpace(s), "\n\n")
}

// Intent renders the page as intent context. Its headings nest under the
// page, and its list items are prose unless they sit under an "Acceptance
// criteria" heading of the page.
func (p Page) Intent() string {
	var b strings.Builder
	// Prose, not a heading: a title such as "Acceptance criteria for
	// checkout" must not open a criteria section.
	fmt.Fprintf(&b, "Notion page: %s\n\n", issuetext.OneLine(p.Title))
	var meta []string
	if p.LastEdited != "" {
		meta = append(meta, "Last edited: "+issuetext.OneLine(p.LastEdited))
	}
	if p.URL != "" {
		meta = append(meta, "Link: "+p.URL)
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	if md := strings.TrimSpace(p.Markdown); md != "" {
		b.WriteString("\n## Page content\n\n" + issuetext.ProseLists(issuetext.NestHeadings(md, false)) + "\n")
	}
	if p.Truncated {
		b.WriteString("\n[Probe read only part of this Notion page: a size or nesting limit was reached.]\n")
	}
	return b.String()
}
