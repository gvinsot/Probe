package linter

// Deterministic security checks on added lines, without a model: hard-coded
// credentials and private keys, other hard-coded values (credentials or
// secrets in URLs, e-mail and IP addresses), and flagrant misconfigurations
// (TLS verification disabled, debug mode, world-writable permissions,
// disabled protections). They are regular expressions: review prompts, not
// verified vulnerabilities, and a clean scan is not proof that no secret or
// misconfiguration was added.
//
// A matched secret value is never copied into a signal: the evidence names
// the pattern and masks the value, since reports and provider context must
// not carry it (report redaction is best effort on top of this).

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Security signal kinds.
const (
	KindHardcodedSecret   = "hardcoded_secret"
	KindPrivateKey        = "private_key"
	KindCredentialInURL   = "credential_in_url"
	KindHardcodedEmail    = "hardcoded_email"
	KindHardcodedIP       = "hardcoded_ip"
	KindTLSDisabled       = "tls_verification_disabled"
	KindDebugEnabled      = "debug_enabled"
	KindExcessivePerms    = "excessive_permissions"
	KindProtectionOff     = "protection_disabled"
	maxSecurityPerKind    = 3 // signals of one kind per file; the evidence counts the rest
	securityEvidenceIntro = "Security pattern (text heuristic, not a verified vulnerability): "
)

type securityPattern struct {
	kind, severity, summary, label string
	re                             *regexp.Regexp
	// secret masks the matched text: the evidence names the pattern only.
	secret bool
	// group selects the submatch checked for placeholders (0: whole match).
	group int
	// skipLowNoise skips the pattern in test and documentation files, where
	// sample values are expected.
	skipLowNoise bool
}

var securityPatterns = []securityPattern{
	// Private keys.
	{kind: KindPrivateKey, severity: "critical", summary: "Private key added", label: "PEM private key block", re: regexp.MustCompile(`-----BEGIN (?:RSA |DSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----`), secret: true},
	{kind: KindPrivateKey, severity: "critical", summary: "Private key added", label: "PuTTY private key", re: regexp.MustCompile(`PuTTY-User-Key-File-\d+:`), secret: true},

	// Provider-specific credential shapes.
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "AWS access key ID", re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "AWS secret access key", re: regexp.MustCompile(`(?i)aws_?secret_?access_?key["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})\b`), secret: true, group: 1},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "GitHub token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "GitLab token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Slack token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Slack webhook URL", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Za-z0-9]+/B[A-Za-z0-9]+/[A-Za-z0-9]+`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Stripe live key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "LLM provider API key", re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{32,}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "npm token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Twilio or SendGrid key", re: regexp.MustCompile(`\b(?:SK[0-9a-f]{32}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43})\b`), secret: true},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "Azure storage account key", re: regexp.MustCompile(`(?i)AccountKey=([A-Za-z0-9+/=]{40,})`), secret: true, group: 1},
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "JSON Web Token", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), secret: true},
	// A secret-named key assigned a quoted literal of at least 8 characters.
	{kind: KindHardcodedSecret, severity: "high", summary: "Hard-coded credential added", label: "secret-named value assigned a literal", re: regexp.MustCompile(`(?i)\b[\w.-]*(?:password|passwd|pwd|secret|api[_-]?key|apikey|access[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|private[_-]?key|client[_-]?secret|credentials?)[\w.-]*["']?\s*(?::=|=>|[:=])\s*["'` + "`" + `]([^"'` + "`" + `\s]{8,})["'` + "`" + `]`), secret: true, group: 1},

	// Credentials and secrets in URLs.
	{kind: KindCredentialInURL, severity: "high", summary: "Credentials embedded in a URL", label: "user:password in a URL", re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@"'<>]+:([^\s/@"'<>]+)@[^\s/"'<>]+`), secret: true, group: 1},
	{kind: KindCredentialInURL, severity: "high", summary: "Secret passed in a URL query", label: "secret-named query parameter", re: regexp.MustCompile(`(?i)[?&](?:api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|client[_-]?secret|password|passwd|sig|signature)=([^&\s"'#<>]{8,})`), secret: true, group: 1},

	// Other hard-coded values.
	{kind: KindHardcodedEmail, severity: "low", summary: "Hard-coded e-mail address added", label: "e-mail address", re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), skipLowNoise: true},
	{kind: KindHardcodedIP, severity: "low", summary: "Hard-coded IP address added", label: "IPv4 address in a string or URL", re: regexp.MustCompile(`(?:["'` + "`" + `]|://|@)((?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3})(?::\d+)?\b`), group: 1, skipLowNoise: true},

	// TLS verification disabled or weakened.
	{kind: KindTLSDisabled, severity: "high", summary: "TLS certificate verification disabled", label: "TLS verification disabled", re: regexp.MustCompile(`(?i)(InsecureSkipVerify\s*:\s*true|rejectUnauthorized["']?\s*:\s*false|NODE_TLS_REJECT_UNAUTHORIZED["']?\s*[:=]\s*["']?0|\bverify\s*=\s*False\b|ssl_?verify(?:_?peer)?["']?\s*[:=]\s*(?:false|0|no)\b|CURLOPT_SSL_VERIFY(?:PEER|HOST)\s*,\s*(?:false|0)|ssl\._create_unverified_context|check_hostname\s*=\s*False|CERT_NONE\b|danger_accept_invalid_certs\s*\(\s*true|\bcurl\b[^\n]*\s(?:-k|--insecure)\b|\bwget\b[^\n]*--no-check-certificate|sslmode\s*=\s*disable|tls\.Version(?:SSL30|TLS10|TLS11)\b|git\s+config[^\n]*http\.sslVerify\s+false)`)},

	// Debug mode.
	{kind: KindDebugEnabled, severity: "medium", summary: "Debug mode enabled", label: "debug mode enabled", re: regexp.MustCompile(`(?i)(\bDEBUG\s*=\s*(?:True|1|["']?true["']?)\b|\bdebug["']?\s*:\s*true\b|\.run\([^)]*debug\s*=\s*True|FLASK_DEBUG\s*=\s*["']?1|APP_DEBUG\s*=\s*["']?true|gin\.SetMode\(\s*gin\.DebugMode|app\.debug\s*=\s*True|DJANGO_DEBUG\s*=\s*["']?(?:True|1))`)},

	// Excessive Linux permissions and privileges.
	{kind: KindExcessivePerms, severity: "high", summary: "Excessive file permissions or privileges", label: "world-writable mode or root privileges", re: regexp.MustCompile(`(?i)(\bchmod\s+(?:-R\s+)?(?:0?777|0?666|a\+w|o\+w|ugo\+w)\b|\b(?:Chmod|WriteFile|Mkdir|MkdirAll|OpenFile|chmod|makedirs|mkdir|writeFileSync|chmodSync)\s*\([^)]*\b0o?(?:777|666)\b|\bmode\s*[:=]\s*["']?0?(?:777|666)\b|\bprivileged\s*:\s*true\b|--privileged\b|allowPrivilegeEscalation\s*:\s*true|runAsUser\s*:\s*0\b|runAsNonRoot\s*:\s*false|^\s*USER\s+root\b|\bsudo\s+chmod\s+777|NOPASSWD\s*:\s*ALL|hostPID\s*:\s*true|hostNetwork\s*:\s*true|cap_add\s*:\s*\[?\s*["']?(?:ALL|SYS_ADMIN)|--cap-add[= ](?:ALL|SYS_ADMIN)|/var/run/docker\.sock)`)},

	// Protections disabled.
	{kind: KindProtectionOff, severity: "high", summary: "Security protection disabled", label: "security protection disabled", re: regexp.MustCompile(`(?i)(@csrf_exempt|\bcsrf(?:_?protection|_?enabled)?["']?\s*[:=]\s*(?:false|0|["']?off)|csrf\(\)\.disable\(\)|WTF_CSRF_ENABLED\s*=\s*False|Access-Control-Allow-Origin["']?\s*[:,]\s*["']\*["']|AllowAllOrigins\s*:\s*true|origin\s*:\s*["']\*["']|allow_origins\s*=\s*\[\s*["']\*["']|SECURE_SSL_REDIRECT\s*=\s*False|SESSION_COOKIE_SECURE\s*=\s*False|\bHttpOnly\s*[:=]\s*false|\bsecure\s*:\s*false\b|autoescape\s*=\s*False|\{\{[^}]*\|\s*safe\s*\}\}|algorithms\s*=\s*\[\s*["']none["']|alg["']?\s*:\s*["']none["']|verify_signature["']?\s*:\s*False|setenforce\s+0|SELINUX\s*=\s*disabled|\bufw\s+disable|seccomp[:=]unconfined|apparmor[:=]unconfined|permissions\s*:\s*write-all|cidr_blocks\s*=\s*\[\s*["']0\.0\.0\.0/0["']|publicly_accessible\s*=\s*true|acl\s*=\s*["']public-read(?:-write)?["']|X-Frame-Options["']?\s*[:,]\s*["']?ALLOWALL|helmet\(\s*\{\s*contentSecurityPolicy\s*:\s*false)`)},
}

// placeholder reports a sample or indirect value rather than a literal secret.
var placeholder = regexp.MustCompile(`(?i)^(?:x+|\*+|\.+|0+|1234\w*|changeme|change[_-]?me|example\w*|sample\w*|dummy\w*|fake\w*|test\w*|placeholder\w*|redacted|your[_-]?\w*|<[^>]*>|\$\{[^}]*\}|\$\(?[A-Za-z_]\w*\)?|%\(?\w*\)?s?|\{\{[^}]*\}\}|none|null|nil|undefined|password|secret|token)$`)

// indirection reports a line that reads a value from somewhere else rather
// than hard-coding it.
var indirection = regexp.MustCompile(`(?i)(getenv|environ|process\.env|os\.env|env::var|secrets\.|vault|\bconfig\.|ssm:|keyvault|\$\{\{)`)

// benignEmail and benignIP exclude documentation, loopback and version-like
// values.
var benignEmail = regexp.MustCompile(`(?i)@(?:example\.(?:com|org|net)|[\w.-]*\.(?:example|invalid|test|local|localhost)|localhost|users\.noreply\.github\.com|noreply\.[\w.-]+)$|^(?:noreply|no-reply|git|user|test|foo|bar|name|someone|you|me)@`)

func benignIP(ip string) bool {
	return strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" || strings.HasPrefix(ip, "255.") || strings.HasPrefix(ip, "192.0.2.") || strings.HasPrefix(ip, "198.51.100.") || strings.HasPrefix(ip, "203.0.113.")
}

// lockfile reports a generated dependency lock, whose hashes and URLs are not
// authored values.
func lockfile(p string) bool {
	base := strings.ToLower(path.Base(p))
	return strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".sum") || base == "package-lock.json" || base == "npm-shrinkwrap.json" || base == "pnpm-lock.yaml" || base == "bun.lockb"
}

// lowNoiseExempt reports files where sample e-mail and IP addresses are
// expected: tests, documentation and licences.
func lowNoiseExempt(p string) bool {
	lower := strings.ToLower(p)
	base := path.Base(lower)
	switch path.Ext(lower) {
	case ".md", ".rst", ".txt", ".adoc", ".html":
		return true
	}
	return isTest(p) || strings.HasPrefix(base, "license") || strings.HasPrefix(base, "notice") || strings.HasPrefix(base, "authors") || strings.HasPrefix(base, "codeowners") || strings.Contains("/"+lower, "/testdata/") || strings.Contains("/"+lower, "/fixtures/")
}

// securitySignals scans the added lines of one text file.
func securitySignals(f model.ChangedFile) []model.Signal {
	if f.Binary || f.Status == "D" || lockfile(f.Path) {
		return nil
	}
	exempt := lowNoiseExempt(f.Path)
	test, docs := isTest(f.Path), documentation(f.Path)
	type hit struct {
		line  int
		label string
		text  string
	}
	hits := map[securityPattern][]hit{}
	var order []securityPattern
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			if d.Kind != "add" || len(d.Content) > 4096 {
				continue
			}
			for _, p := range securityPatterns {
				if p.skipLowNoise && exempt {
					continue
				}
				m := p.re.FindStringSubmatch(d.Content)
				if m == nil || !securityMatch(p, m, d.Content) {
					continue
				}
				if _, ok := hits[p]; !ok {
					order = append(order, p)
				}
				hits[p] = append(hits[p], hit{line: d.NewLine, label: p.label, text: d.Content})
			}
		}
	}
	var out []model.Signal
	perKind := map[string]int{}
	for _, p := range order {
		list := hits[p]
		for i, h := range list {
			if perKind[p.kind] >= maxSecurityPerKind {
				break
			}
			perKind[p.kind]++
			severity := adjustedSeverity(p, test, docs)
			evidence := securityEvidenceIntro + p.label
			if p.secret {
				evidence += "; the value is not copied into the report"
			} else {
				evidence += ": " + short(h.text)
			}
			if i == 0 && len(list) > 1 {
				evidence += fmt.Sprintf(" (%d added lines of this file match)", len(list))
			}
			out = append(out, model.Signal{Kind: p.kind, Path: f.Path, Line: h.line, Side: "new", Severity: severity, Summary: p.summary, Evidence: evidence})
		}
	}
	return out
}

// securityMatch applies the per-pattern exclusions to a match.
func securityMatch(p securityPattern, m []string, line string) bool {
	value := m[0]
	if p.group > 0 && p.group < len(m) {
		value = m[p.group]
	}
	switch p.kind {
	case KindHardcodedSecret, KindCredentialInURL:
		trimmed := strings.Trim(value, `"'`+"`")
		if placeholder.MatchString(trimmed) || strings.ContainsAny(trimmed, "${}<>") {
			return false
		}
		if p.label == "secret-named value assigned a literal" {
			// A path or URL assigned to a secret-named key locates a secret
			// rather than holding one.
			if indirection.MatchString(line) || !mixedEnough(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "~/") || strings.Contains(trimmed, "://") {
				return false
			}
		}
	case KindHardcodedEmail:
		if benignEmail.MatchString(value) || strings.Contains(line, "://"+value) || strings.HasSuffix(strings.ToLower(value), ".png") || strings.HasSuffix(strings.ToLower(value), ".svg") {
			return false
		}
	case KindHardcodedIP:
		if benignIP(value) {
			return false
		}
	}
	return true
}

// mixedEnough rejects identifiers and words assigned to a secret-named key
// (a config key name, an enum value) by requiring a digit or a symbol other
// than identifier punctuation.
func mixedEnough(v string) bool {
	hasDigit, hasSymbol, hasUpper, hasLower := false, false, false, false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r == '_' || r == '-' || r == '.':
			// Identifier punctuation: snake_case or dotted names are not secrets.
		default:
			hasSymbol = true
		}
	}
	return (hasDigit || hasSymbol) && (hasUpper || hasLower)
}

// documentation reports prose files, where configuration snippets and flags
// are often quoted rather than applied.
func documentation(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".rst", ".adoc":
		return true
	}
	return false
}

// adjustedSeverity lowers the severity where a match is less likely to be
// live: in test files every high or critical match is medium (fixtures carry
// fake keys and test-only settings, but a real key there leaks as well); in
// documentation a misconfiguration pattern is low (it is usually quoted), and
// a credential stays medium since a pasted one leaks all the same.
func adjustedSeverity(p securityPattern, test, docs bool) string {
	credential := p.kind == KindHardcodedSecret || p.kind == KindPrivateKey || p.kind == KindCredentialInURL
	switch {
	case p.severity == "low":
		return "low"
	case docs && !credential:
		return "low"
	case test || docs:
		return "medium"
	}
	return p.severity
}
