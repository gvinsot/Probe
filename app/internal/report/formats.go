package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// Report formats. ValidFormat is the single whitelist that the cli flags and
// Write use. The sarif and pr-comment formats are evidence-only exports
// (findings.go): they never change the exit code, a status or the canonical
// Markdown and JSON reports.
const (
	FormatMarkdown  = "markdown"
	FormatJSON      = "json"
	FormatSARIF     = "sarif"
	FormatPRComment = "pr-comment"
)

// File names written inside the output directory.
const (
	markdownFile  = "CONFIDENCE_REPORT.md"
	jsonFile      = "confidence-report.json"
	sarifFile     = "confidence-report.sarif"
	prCommentFile = "PR_COMMENT.md"
)

// maxReportURLBytes bounds --report-url.
const maxReportURLBytes = 512

// writeOptions carries per-write rendering options.
type writeOptions struct {
	reportURL string
}

// Option configures one Write call.
type Option func(*writeOptions)

// WithReportURL records the link to the full report that a PR-comment render
// cites. The cli validates it (ValidateReportURL) before anything runs, and the
// pr-comment renderer validates it again, so an invalid URL writes nothing.
// Probe never fetches it.
func WithReportURL(url string) Option {
	return func(o *writeOptions) { o.reportURL = url }
}

// ValidFormat reports whether format names a report format this build renders.
func ValidFormat(format string) bool {
	switch format {
	case FormatMarkdown, FormatJSON, FormatSARIF, FormatPRComment:
		return true
	}
	return false
}

// ValidateReportURL accepts only an https URL with a host, without userinfo,
// of at most 512 bytes, whose characters are all in
// [A-Za-z0-9._~:/?#!$&*+,;=%-]. The character set cannot close a Markdown link
// or start raw HTML: parentheses, brackets, quotes, backslashes, backticks,
// spaces, angle brackets and "@" are all refused. The URL is the one string of
// the exports that the report's sanitizing never sees, so a URL that looks as
// if it carries a credential (reportURLCredential) is refused rather than
// written into the comment.
func ValidateReportURL(s string) error {
	if s == "" {
		return errors.New("the report URL is empty")
	}
	if len(s) > maxReportURLBytes {
		return fmt.Errorf("the report URL exceeds %d bytes", maxReportURLBytes)
	}
	for i := 0; i < len(s); i++ {
		if !reportURLByte(s[i]) {
			return fmt.Errorf("the report URL contains a character outside [A-Za-z0-9._~:/?#!$&*+,;=%%-] at byte %d", i)
		}
	}
	if !strings.HasPrefix(s, "https://") {
		return errors.New("the report URL must start with https://")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("the report URL does not parse: %v", err)
	}
	if u.Scheme != "https" || u.Opaque != "" {
		return errors.New("the report URL must be an https URL")
	}
	if u.User != nil {
		return errors.New("the report URL must not contain user information")
	}
	if u.Hostname() == "" {
		return errors.New("the report URL must name a host")
	}
	if what := reportURLCredential(s); what != "" {
		return fmt.Errorf("the report URL appears to contain a credential (%s); pass a URL without secrets", what)
	}
	return nil
}

// urlTokenShapes are the credential shapes of the redaction rules
// (internal/redact), anchored where a token can start: at the beginning of the
// text or after a character that cannot be part of the token. The redaction
// rules have no such anchor, which suits free text but refuses ordinary report
// URLs: "flask-sqlalchemy", "task-scheduler" and "disk-usage-report" contain
// "sk-" followed by eight token characters. A refused URL stops the whole run
// with exit 3, so the URL check uses these anchored shapes instead of the
// redaction itself.
var urlTokenShapes = []struct {
	what string
	re   *regexp.Regexp
}{
	{`a value shaped like an "sk-" key`, regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])sk-[A-Za-z0-9_-]{8,}`)},
	{"a value shaped like a GitHub token", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{8,}`)},
	{"a value shaped like an AWS access key ID", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])AKIA[A-Z0-9]{16}`)},
	{"a value shaped like a JSON Web Token", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"a bearer token", regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])bearer(?:\s|\+)+[A-Za-z0-9._~+/=-]`)},
	{"a private key", regexp.MustCompile(`(?i)-----BEGIN[A-Z +]*PRIVATE(?:\s|\+)+KEY`)},
}

// Parameter names that carry credentials, compared in lower case without "-",
// "_" and ".": a name that contains one of credentialNameParts or equals one of
// credentialNames. They cover the names of the redaction's assignment rule
// (api_key, access_token, auth_token, client_secret, secret, password, passwd,
// authorization) and the usual signed-URL parameters (sig, X-Amz-Signature,
// X-Amz-Credential, X-Amz-Security-Token, X-Goog-Signature).
var (
	credentialNameParts = []string{"token", "secret", "password", "passwd", "passphrase", "signature", "credential", "apikey", "accesskey", "privatekey"}
	credentialNames     = map[string]bool{"sig": true, "pwd": true, "pass": true, "key": true, "auth": true, "authorization": true, "jwt": true, "bearer": true, "session": true, "sessionid": true}
	credentialNameFold  = strings.NewReplacer("-", "", "_", "", ".", "")
)

// reportURLCredential describes the credential that the report URL s appears
// to carry, or returns "" when it finds none. In s and in its percent-decoded
// form it looks for a token shape of the redaction rules that starts at a
// token boundary (urlTokenShapes) and, after the host, for a parameter named
// like a credential: the name of every "name=value" or "name:value" piece
// between the separators "/", "?", "&", "#" and ";", so query, fragment and
// path parameters all count. It is a best-effort guard against a URL that
// carries a secret, not a guarantee that none remains.
func reportURLCredential(s string) string {
	texts := []string{s}
	if decoded, err := url.PathUnescape(s); err == nil && decoded != s {
		texts = append(texts, decoded)
	}
	for _, text := range texts {
		for _, shape := range urlTokenShapes {
			if shape.re.MatchString(text) {
				return shape.what
			}
		}
		rest := strings.TrimPrefix(text, "https://")
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[i:]
		} else {
			rest = ""
		}
		for _, piece := range strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune("/?&#;", r) }) {
			i := strings.IndexAny(piece, "=:")
			if i <= 0 {
				continue
			}
			if credentialName(piece[:i]) {
				return fmt.Sprintf("a parameter named %q", redact.TruncateUTF8(piece[:i], 64))
			}
		}
	}
	return ""
}

// credentialName reports whether a URL parameter name reads as the name of a
// credential.
func credentialName(name string) bool {
	folded := strings.ToLower(credentialNameFold.Replace(name))
	if credentialNames[folded] {
		return true
	}
	for _, part := range credentialNameParts {
		if strings.Contains(folded, part) {
			return true
		}
	}
	return false
}

func reportURLByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("._~:/?#!$&*+,;=%-", c) >= 0
}

// renderFormat renders one format of an already sanitized, finalized report
// into memory. It returns the file name to write inside the output directory.
func renderFormat(format string, r *model.Report, o writeOptions) (string, []byte, error) {
	switch format {
	case FormatMarkdown:
		return markdownFile, renderMarkdown(r), nil
	case FormatJSON:
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return "", nil, err
		}
		return jsonFile, append(data, '\n'), nil
	case FormatSARIF:
		data, err := renderSARIF(r, collectFindings(r, verifyExports(r)))
		if err != nil {
			return "", nil, err
		}
		return sarifFile, data, nil
	case FormatPRComment:
		if o.reportURL != "" {
			if err := ValidateReportURL(o.reportURL); err != nil {
				return "", nil, fmt.Errorf("--report-url: %v", err)
			}
		}
		return prCommentFile, renderPRComment(r, collectFindings(r, verifyExports(r)), o.reportURL), nil
	}
	return "", nil, fmt.Errorf("unsupported report format %q", format)
}
