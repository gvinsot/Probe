package store

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxErrorBytes = 1024

var authDiagnostic = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:basic|bearer)\s+[^\s"']+`)
var secretDiagnostic = regexp.MustCompile(`(?i)((?:access_token|refresh_token|token|password|secret)\s*[=:]\s*)[^\s&"']+`)
var forgeDiagnostic = regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`)

// Conservatively hide long opaque arguments too, including unknown token formats.
var opaqueDiagnostic = regexp.MustCompile(`(^|[\s"'=])([A-Za-z0-9_+/-]{32,}={0,2})`)
var diagnosticURL = regexp.MustCompile(`https?://[^\s<>"']+`)

// SafeError protects legacy records too. Known secrets, credential-shaped
// values and URL credentials are removed before truncation, so a long value cannot evade redaction at the boundary.
func SafeError(message string, secrets ...string) string {
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
	message = forgeDiagnostic.ReplaceAllString(message, "[redacted]")
	message = opaqueDiagnostic.ReplaceAllString(message, "${1}[redacted]")
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
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
