// Package issuetext holds what the issue-tracker integrations (Jira, Linear)
// share: reading their credential the way the deployment mounts secrets, and
// shaping issue text so that intent parsing reads it as intended.
package issuetext

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// SecretsDir is where the deployment mounts Docker secrets.
const SecretsDir = "/run/secrets"

const maxSecretBytes = 8192

// Secret returns the credential named by the environment variable name: the
// variable itself, then the file named by name_FILE, then
// /run/secrets/<name>. It also returns where the value came from. An absent
// conventional file is not an error (the credential is then ""), but a named
// or mounted file that cannot be used always is.
func Secret(name string, getenv func(string) string, readFile func(string) ([]byte, error)) (string, string, error) {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", "", fmt.Errorf("%s must be a single line", name)
		}
		return v, name, nil
	}
	file, explicit := strings.TrimSpace(getenv(name+"_FILE")), true
	if file == "" {
		file, explicit = SecretsDir+"/"+name, false
	}
	value, err := readSecret(file, readFile)
	switch {
	case err == nil:
		return value, file, nil
	case explicit || !errors.Is(err, fs.ErrNotExist):
		return "", "", fmt.Errorf("secret %s: %w", file, err)
	}
	return "", "", nil
}

func readSecret(file string, readFile func(string) ([]byte, error)) (string, error) {
	if readFile == nil {
		return "", fs.ErrNotExist
	}
	data, err := readFile(file)
	if err != nil {
		return "", err
	}
	if len(data) > maxSecretBytes {
		return "", errors.New("secret exceeds 8 KiB")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("secret is empty")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("secret must be a single line")
	}
	return value, nil
}

// OneLine collapses whitespace, so that a title stays on one line.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})([ \t].*|)$`)

// NestHeadings moves every ATX heading of s two levels down (at most level
// 6) and, with rename, turns "acceptance criteria" / "acceptance criterion"
// in a heading into "criteria" / "criterion". Fenced code is left alone.
func NestHeadings(s string, rename bool) string {
	lines := strings.Split(s, "\n")
	fenced := ""
	for n, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if fenced != "" {
			if strings.HasPrefix(trimmed, fenced) {
				fenced = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = trimmed[:3]
			continue
		}
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level := len(m[1]) + 2
		if level > 6 {
			level = 6
		}
		title := m[2]
		if rename {
			title = acceptanceWord.ReplaceAllString(title, "$1")
		}
		lines[n] = strings.Repeat("#", level) + title
	}
	return strings.Join(lines, "\n")
}

var acceptanceWord = regexp.MustCompile(`(?i)acceptance\s+(criteri)`)

var (
	proseHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t#]*$`)
	proseBullet  = regexp.MustCompile(`^([ \t]*)(?:[-*+])[ \t]+(?:\[[ xX]\][ \t]+)?(.*)$`)
	proseNumber  = regexp.MustCompile(`^([ \t]*)([0-9]{1,9})[.)][ \t]+(.*)$`)
	criteriaWord = regexp.MustCompile(`(?i)acceptance criteri(a|on)`)
)

// ProseLists keeps Markdown list items only where intent parsing should take
// them as acceptance criteria: under a heading that names acceptance
// criteria. Every other list item becomes prose ("• item", "(1) item"), so
// that reference material such as an architecture page adds context without
// adding criteria. Sections follow the criteria grammar: a matching heading
// of level L is closed by the next heading of level L or higher. Fenced code
// is left alone.
func ProseLists(s string) string {
	lines := strings.Split(s, "\n")
	fenced := ""
	inScope, scopeLevel := false, 0
	for n, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if fenced != "" {
			if strings.HasPrefix(trimmed, fenced) {
				fenced = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = trimmed[:3]
			continue
		}
		if m := proseHeading.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			switch {
			case criteriaWord.MatchString(m[2]):
				if !inScope || level <= scopeLevel {
					scopeLevel = level
				}
				inScope = true
			case inScope && level <= scopeLevel:
				inScope = false
			}
			continue
		}
		if inScope {
			continue
		}
		if m := proseBullet.FindStringSubmatch(line); m != nil {
			lines[n] = m[1] + "• " + m[2]
		} else if m := proseNumber.FindStringSubmatch(line); m != nil {
			lines[n] = m[1] + "(" + m[2] + ") " + m[3]
		}
	}
	return strings.Join(lines, "\n")
}
