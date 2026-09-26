package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
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
// SwiftProof never fetches it.
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
// the exports that the report's sanitizing never sees, so a URL that the
// credential redaction would change (a token in its query, for example) is
// refused rather than written into the comment.
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
	if !redact.IsFixedPoint(s) {
		return errors.New("the report URL appears to contain a credential, which SwiftProof's redaction would mask; pass a URL without secrets")
	}
	return nil
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
