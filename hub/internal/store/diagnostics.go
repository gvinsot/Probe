package store

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxErrorBytes = 1024

var gitDiagnostic = regexp.MustCompile(`(?s)git [a-z-]+:.*`)
var authDiagnostic = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:basic|bearer)\s+[^\s"']+`)
var secretDiagnostic = regexp.MustCompile(`(?i)((?:access_token|refresh_token|token|password|secret)\s*[=:]\s*)[^\s&"']+`)
var diagnosticURL = regexp.MustCompile(`https?://[^\s<>"']+`)

// SafeError protects legacy records too. Git's untrusted diagnostic body is
// never exposed; credential-shaped values and URL credentials are removed
// before truncation, so a long value cannot evade redaction at the boundary.
func SafeError(message string) string {
	message = gitDiagnostic.ReplaceAllString(message, "git command failed (diagnostics omitted)")
	message = authDiagnostic.ReplaceAllString(message, "${1}[redacted]")
	message = diagnosticURL.ReplaceAllStringFunc(message, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[redacted URL]"
		}
		u.User = nil
		u.RawQuery, u.Fragment = "", ""
		return u.String()
	})
	message = secretDiagnostic.ReplaceAllString(message, "${1}[redacted]")
	return bounded(strings.ToValidUTF8(message, ""), MaxErrorBytes)
}

func bounded(value string, size int) string {
	if len(value) <= size {
		return value
	}
	value = value[:size]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return strings.Clone(value)
}
