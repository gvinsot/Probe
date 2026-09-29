// Package acceptance extracts acceptance criteria from a supplied intent text
// (criteria grammar v1) and finds, statically, the changed declarations that an
// intent test names. It is pure: it performs no I/O and never executes code.
//
// Extraction reads Markdown list items only. It is not an understanding of the
// intent: prose outside list items is not read, and an intent without list
// items yields no criteria, which does not mean it states no requirement.
package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Grammar names the criteria grammar recorded in the documentation.
const Grammar = "markdown-list-items/v1"

// Limits of the extraction.
const (
	IDPrefix          = "AC-"
	MaxCriteria       = 100
	MaxCriterionBytes = 1024
)

// PRCommentNote is the fixed note recorded when Probe PR-comment output
// was removed from the intent text.
const PRCommentNote = "Probe PR-comment output was removed from the intent text."

// ErrEncoding is returned for an intent that is not UTF-8 text or contains NUL.
var ErrEncoding = errors.New("intent must be UTF-8 text without NUL bytes")

// Document is the result of parsing one intent text.
type Document struct {
	// SHA256 is the hex SHA-256 of the exact text parsed; "" for an empty text.
	SHA256 string
	// Criteria are the extracted criteria in document order; never nil.
	Criteria []model.IntentCriterion
	// Overlong counts list items skipped because they exceed MaxCriterionBytes;
	// Omitted counts list items skipped after MaxCriteria.
	Overlong, Omitted int
}

// Notes returns the fixed Unverified sentences about skipped list items.
func (d Document) Notes() []string {
	var notes []string
	if d.Overlong > 0 {
		notes = append(notes, fmt.Sprintf("%d intent list %s longer than %d bytes %s not taken as acceptance criteria.", d.Overlong, plural(d.Overlong, "item", "items"), MaxCriterionBytes, plural(d.Overlong, "was", "were")))
	}
	if d.Omitted > 0 {
		notes = append(notes, fmt.Sprintf("Only the first %d acceptance criteria were extracted from the intent; %d further list %s %s ignored.", MaxCriteria, d.Omitted, plural(d.Omitted, "item", "items"), plural(d.Omitted, "was", "were")))
	}
	return notes
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// CheckEncoding returns ErrEncoding unless text is valid UTF-8 without NUL.
func CheckEncoding(text string) error {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return ErrEncoding
	}
	return nil
}

// maxStripPasses bounds the removal passes of StripPRComments.
const maxStripPasses = 8

// StripPRComments removes every block from model.PRCommentBegin to
// model.PRCommentEnd, markers included; a begin marker without an end marker
// removes everything after it, and a stray end marker is removed too. It
// reports whether anything was removed.
//
// Removing a block can join the text around it into a new marker (for example
// "<!-- probe:pr-" + block + "comment:begin v1 -->"), so the removal is
// repeated until neither marker occurs. Every pass removes at least one
// marker, and after maxStripPasses passes the text is cut at the first marker
// that remains, as for an unterminated block. The result therefore never
// contains a marker, and StripPRComments of its result removes nothing.
func StripPRComments(text string) (string, bool) {
	removed := false
	for pass := 0; containsPRMarker(text); pass++ {
		removed = true
		if pass == maxStripPasses {
			// No marker starts before the first one, so the prefix holds none.
			return text[:firstPRMarker(text)], true
		}
		text = stripPRCommentsOnce(text)
	}
	return text, removed
}

func containsPRMarker(text string) bool {
	return strings.Contains(text, model.PRCommentBegin) || strings.Contains(text, model.PRCommentEnd)
}

// firstPRMarker is the index of the first begin or end marker of text, which
// contains at least one.
func firstPRMarker(text string) int {
	i, j := strings.Index(text, model.PRCommentBegin), strings.Index(text, model.PRCommentEnd)
	if i < 0 || j >= 0 && j < i {
		return j
	}
	return i
}

// stripPRCommentsOnce is one removal pass: every begin..end block, the rest of
// the text after an unterminated begin marker, then every stray end marker.
func stripPRCommentsOnce(text string) string {
	var b strings.Builder
	rest := text
	for {
		i := strings.Index(rest, model.PRCommentBegin)
		if i < 0 {
			break
		}
		b.WriteString(rest[:i])
		after := rest[i+len(model.PRCommentBegin):]
		j := strings.Index(after, model.PRCommentEnd)
		if j < 0 {
			rest = ""
			break
		}
		rest = after[j+len(model.PRCommentEnd):]
	}
	b.WriteString(rest)
	return strings.ReplaceAll(b.String(), model.PRCommentEnd, "")
}

var (
	listItemPattern      = regexp.MustCompile(`^[ \t]*(?:[-*+]|[0-9]{1,9}[.)])[ \t]+(.*)$`)
	headingPattern       = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t#]*$`)
	fencePattern         = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	thematicBreakPattern = regexp.MustCompile(`^ {0,3}([-*_])(?:[ \t]*[-*_]){2,}[ \t]*$`)
	checkboxPattern      = regexp.MustCompile(`^\[[ xX]\](?:[ \t]+|$)`)
	spacePattern         = regexp.MustCompile(`\s+`)
)

// thematicBreak reports a line of three or more identical -, * or _
// characters, optionally separated by spaces or tabs.
func thematicBreak(line string) bool {
	m := thematicBreakPattern.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	c := m[1]
	for _, r := range strings.TrimSpace(line) {
		if r != ' ' && r != '\t' && string(r) != c {
			return false
		}
	}
	return true
}

// heading returns the level and normalized title of an ATX heading.
func heading(line string) (int, string, bool) {
	m := headingPattern.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	title := strings.ToLower(strings.TrimSpace(spacePattern.ReplaceAllString(m[2], " ")))
	return len(m[1]), title, true
}

func scopeHeading(title string) bool {
	return strings.Contains(title, "acceptance criteria") || strings.Contains(title, "acceptance criterion")
}

// fence tracks fenced code blocks: a fence closes on the same character with
// at least the opener's length.
type fence struct {
	char  byte
	count int
}

func (f *fence) update(line string) bool {
	m := fencePattern.FindStringSubmatch(line)
	if f.count == 0 {
		if m == nil {
			return false
		}
		f.char, f.count = m[1][0], len(m[1])
		return true
	}
	if m != nil && m[1][0] == f.char && len(m[1]) >= f.count && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), string(f.char))) == "" {
		f.count = 0
	}
	return true
}

// Parse extracts the acceptance criteria of text with grammar v1:
//
//   - criteria are Markdown list items (-, *, +, 1., 1)); a task checkbox
//     [ ] / [x] is stripped;
//   - when any ATX heading contains "acceptance criteria" (or "acceptance
//     criterion", case-insensitive), only items inside such sections count: a
//     matching heading of level L opens a section and the next heading of
//     level L or higher closes it; otherwise every list item counts;
//   - indented continuation lines join their item; fenced code blocks and
//     thematic breaks are ignored;
//   - IDs are positional, AC-1 to AC-100, in document order; items longer than
//     MaxCriterionBytes and items after MaxCriteria are counted, not numbered.
//
// The text must be UTF-8 without NUL. SHA256 covers the exact bytes of text; a
// leading byte-order mark is ignored for parsing only. Lines are 1-based.
func Parse(text string) (Document, error) {
	doc := Document{Criteria: []model.IntentCriterion{}}
	if err := CheckEncoding(text); err != nil {
		return doc, err
	}
	if text == "" {
		return doc, nil
	}
	sum := sha256.Sum256([]byte(text))
	doc.SHA256 = hex.EncodeToString(sum[:])
	lines := strings.Split(strings.TrimPrefix(text, "\uFEFF"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	scoped := false
	var f fence
	for _, line := range lines {
		if f.update(line) {
			continue
		}
		if _, title, ok := heading(line); ok && scopeHeading(title) {
			scoped = true
			break
		}
	}
	f = fence{}
	inScope, scopeLevel := false, 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if f.update(line) {
			continue
		}
		if level, title, ok := heading(line); ok {
			switch {
			case scopeHeading(title):
				// A matching subheading inside an open section keeps the
				// outer section's level, so the outer section closes it.
				if !inScope || level <= scopeLevel {
					scopeLevel = level
				}
				inScope = true
			case inScope && level <= scopeLevel:
				inScope = false
			}
			continue
		}
		if thematicBreak(line) {
			continue
		}
		m := listItemPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start := i + 1
		item := checkboxPattern.ReplaceAllString(m[1], "")
		for i+1 < len(lines) && continuation(lines[i+1]) {
			i++
			item += " " + strings.TrimSpace(lines[i])
		}
		item = strings.TrimSpace(item)
		if item == "" || scoped && !inScope {
			continue
		}
		if len(item) > MaxCriterionBytes {
			doc.Overlong++
			continue
		}
		if len(doc.Criteria) == MaxCriteria {
			doc.Omitted++
			continue
		}
		doc.Criteria = append(doc.Criteria, model.IntentCriterion{ID: IDPrefix + strconv.Itoa(len(doc.Criteria)+1), Text: item, Line: start})
	}
	return doc, nil
}

// continuation reports an indented, non-blank line that continues the list
// item above it: it starts with at least two spaces or a tab and is not itself
// a list item, a heading, a fence or a thematic break.
func continuation(line string) bool {
	if strings.TrimSpace(line) == "" || !(strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
		return false
	}
	if listItemPattern.MatchString(line) || thematicBreak(line) || fencePattern.MatchString(line) {
		return false
	}
	_, _, isHeading := heading(line)
	return !isHeading
}

// ValidID reports whether id matches ^AC-[1-9][0-9]{0,2}$.
func ValidID(id string) bool { return model.ValidCriterionID(id) }

// Find returns the criterion with this ID when the ID is valid and occurs
// exactly once in criteria.
func Find(criteria []model.IntentCriterion, id string) (model.IntentCriterion, bool) {
	if !ValidID(id) {
		return model.IntentCriterion{}, false
	}
	var found model.IntentCriterion
	count := 0
	for _, c := range criteria {
		if c.ID == id {
			found = c
			count++
		}
	}
	return found, count == 1
}
