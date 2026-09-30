package office

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The consistency check looks past the changed passage: when an edit replaces
// or removes a term (a name, a party, a product, a place…) that the rest of
// the current version still uses, the document no longer agrees with itself.
// Replacing the landlord's name in one clause while the parties clause keeps
// the old one is the typical case.

// Consistency verdicts of a finding.
const (
	Inconsistent = "inconsistent"
	Consistent   = "consistent"
)

// Mention is a term an edit replaced or removed that the unchanged passages
// of the current version still use.
type Mention struct {
	// Term is the replaced or removed text, Replacement what took its place
	// (empty for a removal), Location the edited passage.
	Term        string       `json:"term"`
	Replacement string       `json:"replacement,omitempty"`
	Location    string       `json:"location"`
	Elsewhere   []Occurrence `json:"elsewhere"`
	// Count is the number of other passages using the term, Elsewhere
	// being bounded.
	Count int `json:"count"`
}

// Occurrence is a passage of the current version that uses a term.
type Occurrence struct {
	Location string `json:"location"`
	Excerpt  string `json:"excerpt"`
}

// Limits of the consistency check.
const (
	maxMentions      = 10
	maxOccurrences   = 5
	maxTermsPerEdit  = 4
	occurrenceWindow = 120
)

// passage is a paragraph of the current version and where it is.
type passage struct {
	location, text string
}

// edit is a modified paragraph: its location and both texts.
type edit struct {
	location, before, after string
}

var wordToken = regexp.MustCompile(`[\p{L}\p{N}]+(?:['’.\-][\p{L}\p{N}]+)*`)

// Short words that never make a term on their own.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for with from that this these those will shall must may can not are was were been have has had its his her their our your which who whom whose into onto upon under over than then there here also such each other any all some only
		les des une dans pour avec par sur sous sans que qui quoi dont est sont été être avoir ont aux leur leurs cette ces ses son sa mais donc car ainsi selon entre vers chez lors tout tous toute toutes autre autres même plus moins très peut doit être fait faire`) {
		stopWords[w] = true
	}
}

// consistency returns the mentions of the terms the edits replaced or
// removed that other passages of the current version still use, and flags
// each of them.
func (r *reportBuilder) consistency() []Mention {
	var out []Mention
	for _, e := range r.edits {
		if len(out) == maxMentions {
			break
		}
		for _, t := range replacedTerms(e.before, e.after) {
			if len(out) == maxMentions {
				break
			}
			m := Mention{Term: t.term, Replacement: t.replacement, Location: e.location}
			re := termPattern(t.term)
			for _, p := range r.passages {
				if p.location == e.location && normalize(p.text) == normalize(e.after) {
					continue // the edited passage itself
				}
				loc := re.FindStringIndex(p.text)
				if loc == nil {
					continue
				}
				m.Count++
				if len(m.Elsewhere) < maxOccurrences {
					m.Elsewhere = append(m.Elsewhere, Occurrence{Location: p.location, Excerpt: around(p.text, loc[0], loc[1])})
				}
			}
			if m.Count == 0 {
				continue
			}
			out = append(out, m)
			r.flag(mentionFinding(m))
		}
	}
	return out
}

func mentionFinding(m Mention) Finding {
	places := make([]string, len(m.Elsewhere))
	for i, o := range m.Elsewhere {
		places[i] = o.Location
	}
	where := strings.Join(places, ", ")
	if m.Count > len(m.Elsewhere) {
		where += fmt.Sprintf(" and %d more", m.Count-len(m.Elsewhere))
	}
	title := fmt.Sprintf("%q was replaced by %q here but is still used elsewhere in the document (%s)", m.Term, m.Replacement, where)
	if m.Replacement == "" {
		title = fmt.Sprintf("%q was removed here but is still used elsewhere in the document (%s)", m.Term, where)
	}
	return Finding{
		Severity: Medium, Rule: "text.inconsistent-mention", Title: title, Location: m.Location,
		Before: m.Term, After: m.Replacement,
		Consistency: Inconsistent,
		Note:        fmt.Sprintf("%s still reads: %s", m.Elsewhere[0].Location, m.Elsewhere[0].Excerpt),
	}
}

type replaced struct{ term, replacement string }

// replacedTerms aligns the words of both versions of a passage and returns
// the runs of words the edit removed, each with the run inserted at the same
// place. Figures, dates and common words are left to the other rules, and a
// term the new version still contains was only moved.
func replacedTerms(before, after string) []replaced {
	bt, at := wordToken.FindAllString(before, -1), wordToken.FindAllString(after, -1)
	lower := func(ws []string) []string {
		out := make([]string, len(ws))
		for i, w := range ws {
			out[i] = strings.ToLower(w)
		}
		return out
	}
	removed, inserted := map[int]bool{}, map[int]bool{}
	for _, d := range diffSeq(lower(bt), lower(at)) {
		if d.i >= 0 {
			removed[d.i] = true
		}
		if d.j >= 0 {
			inserted[d.j] = true
		}
	}
	runs := func(ws []string, set map[int]bool) []string {
		var out []string
		for i := 0; i < len(ws); i++ {
			if !set[i] {
				continue
			}
			j := i
			for j < len(ws) && set[j] {
				j++
			}
			out = append(out, strings.Join(ws[i:j], " "))
			i = j
		}
		return out
	}
	gone, added := runs(bt, removed), runs(at, inserted)
	afterPattern := strings.ToLower(normalize(after))
	var out []replaced
	for k, g := range gone {
		if len(out) == maxTermsPerEdit {
			break
		}
		if !meaningfulTerm(g) || termPattern(g).MatchString(afterPattern) {
			continue
		}
		t := replaced{term: g}
		if len(gone) == len(added) {
			t.replacement = added[k]
		}
		out = append(out, t)
	}
	return out
}

// meaningfulTerm keeps the runs that name something: at least one word of
// three letters or more that is not a common word, and not only figures.
func meaningfulTerm(s string) bool {
	if len([]rune(s)) > 80 {
		return false // a rewritten sentence, not a term
	}
	for _, w := range strings.Fields(s) {
		letters := 0
		for _, c := range w {
			if unicode.IsLetter(c) {
				letters++
			}
		}
		if letters >= 3 && !stopWords[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// termPattern matches a term as whole words, whatever the case and spacing.
func termPattern(term string) *regexp.Regexp {
	words := strings.Fields(term)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])` + strings.Join(words, `[^\p{L}\p{N}]+`) + `(?:$|[^\p{L}\p{N}])`)
}

// around returns the text surrounding a match, clipped to a readable window.
func around(text string, start, end int) string {
	rs := []rune(text)
	s, e := len([]rune(text[:start])), len([]rune(text[:end]))
	from, to := max(0, s-occurrenceWindow), min(len(rs), e+occurrenceWindow)
	out := strings.TrimSpace(string(rs[from:to]))
	if from > 0 {
		out = "…" + out
	}
	if to < len(rs) {
		out += "…"
	}
	return out
}
