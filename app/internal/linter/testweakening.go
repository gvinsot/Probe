package linter

// Lexical test-edit signals (F3). They read the diff hunks of one changed test
// file and flag edits that may make a test weaker: removed assertions or test
// cases, added skip or focus markers, and exact expectations replaced by
// looser ones. They are text heuristics and review prompts, never evidence:
// nothing here parses or runs the tests.

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Signal kinds and per-file caps.
const (
	weakeningPrefix   = "Text heuristic, not evidence that a test got weaker: "
	maxSkipSignals    = 10
	maxFocusSignals   = 5
	maxRelaxedSignals = 10
	weakeningGo       = "go"
	weakeningJS       = "js"
	weakeningPython   = "python"
)

// weakeningPatterns are the lexical patterns of one language.
type weakeningPatterns struct {
	assertion, declaration, skip, focus, strict, loose []*regexp.Regexp
	// comparison names the statement whose operators count as expectations:
	// "if" for Go, "assert" for Python, "" for none.
	comparison string
	null       string // the literal an equality with which is not an exact expectation
}

func patterns(exprs ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(exprs))
	for i, e := range exprs {
		out[i] = regexp.MustCompile(e)
	}
	return out
}

var weakeningLanguages = map[string]weakeningPatterns{
	weakeningGo: {
		assertion:   patterns(`\bt\.(Error|Errorf|Fatal|Fatalf|Fail|FailNow)\s*\(`, `\b(assert|require)\.[A-Z]\w*\s*\(`),
		declaration: patterns(`^\s*func\s+Test\w*\s*\(`),
		skip:        patterns(`\bt\.(Skip|Skipf|SkipNow)\s*\(`),
		strict:      patterns(`\b(assert|require)\.(Equal|EqualValues|Exactly|JSONEq|YAMLEq|ErrorIs|EqualError)\s*\(`, `\bt\.(Error|Errorf|Fatal|Fatalf|Fail|FailNow)\s*\(`),
		loose:       patterns(`\b(assert|require)\.(NotNil|True|False|NotEmpty|Contains|NotZero|Error|NoError|Greater\w*|Less\w*|Len|Positive|Negative|Subset)\s*\(`, `\bt\.(Log|Logf)\s*\(`),
		comparison:  "if",
		null:        "nil",
	},
	weakeningJS: {
		assertion:   patterns(`\bexpect\s*\(`, `(^|[^\w.$])assert(\.\w+)?\s*\(`),
		declaration: patterns(`^\s*(it|test)(\.(only|skip|todo|concurrent|failing))?\s*\(`),
		skip:        patterns(`(^|[^\w.$])(it|test|describe|context)\.(skip|todo)\s*\(`, `(^|[^\w.$])x(it|test|describe|context)\s*\(`, `(^|[^\w.$])test\.fixme\s*\(`),
		focus:       patterns(`(^|[^\w.$])(it|test|describe|context)\.only\s*\(`, `(^|[^\w.$])f(it|describe)\s*\(`),
		strict:      patterns(`\.(toBe|toEqual|toStrictEqual)\s*\(`, `\.toThrow\s*\(\s*[^)\s]`, `(^|[^\w.$])assert\.(equal|strictEqual|deepEqual|deepStrictEqual|throws)\s*\(`),
		loose:       patterns(`\.(toBeDefined|toBeTruthy|toBeFalsy|toBeGreaterThan\w*|toBeLessThan\w*|toContain|toMatch|toHaveLength|toHaveBeenCalled)\s*\(`, `\.not\.(toBeNull|toBeUndefined)\s*\(`, `\bexpect\.(any|anything)\s*\(`, `\.toThrow\s*\(\s*\)`, `(^|[^\w.$])assert\.(ok|notEqual)\s*\(`, `(^|[^\w.$])assert\s*\(`),
	},
	weakeningPython: {
		assertion:   patterns(`\bself\.assert\w*\s*\(`, `^\s*assert\b`, `\bpytest\.raises\s*\(`),
		declaration: patterns(`^\s*(async\s+)?def\s+test\w*\s*\(`),
		skip:        patterns(`@pytest\.mark\.(skip|skipif|xfail)\b`, `@unittest\.(skip|skipIf|skipUnless|expectedFailure)\b`, `\bpytest\.skip\s*\(`, `\bself\.skipTest\s*\(`),
		strict:      patterns(`\bself\.(assertEqual|assertEquals|assertIs|assertRaises|assertDictEqual|assertListEqual|assertSequenceEqual)\s*\(`),
		loose:       patterns(`\bself\.(assertTrue|assertFalse|assertIsNotNone|assertIsNone|assertIn|assertNotIn|assertGreater\w*|assertLess\w*|assertAlmostEqual)\s*\(`, `^\s*assert\b.*\bis\s+not\s+None\b`),
		comparison:  "assert",
		null:        "None",
	},
}

// weakeningLanguage returns the pattern set for a test file, or false for a
// language without lexical rules.
func weakeningLanguage(p string) (weakeningPatterns, bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".go":
		return weakeningLanguages[weakeningGo], true
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		return weakeningLanguages[weakeningJS], true
	case ".py":
		return weakeningLanguages[weakeningPython], true
	}
	return weakeningPatterns{}, false
}

func matchesAny(list []*regexp.Regexp, s string) bool {
	for _, re := range list {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// comparisonStatement matches the start of the statement whose operators
// count as expectations.
var comparisonStatement = map[string]*regexp.Regexp{
	"if":     regexp.MustCompile(`^\s*(\}\s*else\s+)?if\b`),
	"assert": regexp.MustCompile(`^\s*assert\b`),
}

// comparisons reports whether s holds an equality (== or !=) against
// something other than null, and an ordering (<, >, <=, >=). Shifts, arrows
// and channel operators are not orderings.
func comparisons(s, null string) (equality, ordering bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		var prev, next byte
		if i > 0 {
			prev = s[i-1]
		}
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case (c == '=' || c == '!') && next == '=':
			rest := strings.TrimSpace(s[i+2:])
			isNull := strings.HasPrefix(rest, null) && (len(rest) == len(null) || !isWordByte(rest[len(null)]))
			if prev != '=' && prev != '!' && !isNull {
				equality = true
			}
			i++
		case c == '<' || c == '>':
			if next == c || prev == c || next == '-' || prev == '-' || prev == '=' {
				continue
			}
			ordering = true
			if next == '=' {
				i++
			}
		}
	}
	return equality, ordering
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// expectation classifies one line as an exact (strict) or looser expectation.
func (p weakeningPatterns) expectation(s string) (strict, loose bool) {
	strict, loose = matchesAny(p.strict, s), matchesAny(p.loose, s)
	if re := comparisonStatement[p.comparison]; re != nil && re.MatchString(s) {
		equality, ordering := comparisons(s, p.null)
		strict = strict || equality
		loose = loose || ordering
	}
	return strict, loose
}

// weakeningLine is one added or deleted line of a hunk.
type weakeningLine struct {
	kind    string // "add" | "delete"
	line    int
	content string
}

// testWeakeningSignals returns heuristic signals for a changed test file whose
// edits may weaken it (removed assertions or cases, added skips or focus,
// relaxed expectations). They are review prompts, never evidence.
//   - test_assertion_removed (medium, old side): more assertion lines removed
//     than added in the file;
//   - test_case_removed (medium, old side): more test declarations removed than
//     added in the file;
//   - test_skip_added (medium, new side, at most 10) and test_focus_added (high,
//     new side, at most 5): an added marker line that is not a removed line
//     moved or re-indented;
//   - test_expectation_relaxed (medium, new side, at most 10): in one hunk,
//     more exact expectations removed than added and more looser ones added
//     than removed.
func testWeakeningSignals(f model.ChangedFile) []model.Signal {
	p, ok := weakeningLanguage(f.Path)
	if !ok || f.Binary {
		return nil
	}
	var signals []model.Signal
	add := func(kind, severity, summary, side string, line int, evidence string) {
		signals = append(signals, model.Signal{Kind: kind, Path: f.Path, Line: line, Side: side, Severity: severity, Summary: summary, Evidence: weakeningPrefix + evidence})
	}
	var all []weakeningLine
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			switch d.Kind {
			case "add":
				all = append(all, weakeningLine{"add", d.NewLine, d.Content})
			case "delete":
				all = append(all, weakeningLine{"delete", d.OldLine, d.Content})
			}
		}
	}
	// Removed assertions and test cases, counted over the whole file.
	for _, rule := range []struct {
		kind, summary, noun string
		list                []*regexp.Regexp
	}{
		{model.SignalTestAssertionRemoved, "Test assertions removed", "assertion lines", p.assertion},
		{model.SignalTestCaseRemoved, "Test cases removed", "test declarations", p.declaration},
	} {
		added, removed := 0, 0
		var first *weakeningLine
		for i := range all {
			if !matchesAny(rule.list, all[i].content) {
				continue
			}
			if all[i].kind == "add" {
				added++
			} else {
				removed++
				if first == nil {
					first = &all[i]
				}
			}
		}
		if removed > added && first != nil {
			add(rule.kind, "medium", rule.summary, "old", first.line, fmt.Sprintf("%d %s removed and %d added; first removed: %s", removed, rule.noun, added, weakeningQuote(first.content)))
		}
	}
	// Added skip and focus markers, except moved or re-indented ones.
	for _, rule := range []struct {
		kind, severity, summary string
		list                    []*regexp.Regexp
		limit                   int
	}{
		{model.SignalTestSkipAdded, "medium", "Skip marker added to a test file", p.skip, maxSkipSignals},
		{model.SignalTestFocusAdded, "high", "Focus marker added to a test file; other tests may no longer run", p.focus, maxFocusSignals},
	} {
		if len(rule.list) == 0 {
			continue
		}
		removed := map[string]int{}
		for _, l := range all {
			if l.kind == "delete" && matchesAny(rule.list, l.content) {
				removed[strings.TrimSpace(l.content)]++
			}
		}
		var added []weakeningLine
		for _, l := range all {
			if l.kind != "add" || !matchesAny(rule.list, l.content) {
				continue
			}
			if key := strings.TrimSpace(l.content); removed[key] > 0 {
				removed[key]--
				continue
			}
			added = append(added, l)
		}
		for i, l := range added {
			if i == rule.limit {
				break
			}
			evidence := "added " + weakeningQuote(l.content)
			if i == rule.limit-1 && len(added) > rule.limit {
				evidence += fmt.Sprintf(" (and %d more in this file)", len(added)-rule.limit)
			}
			add(rule.kind, rule.severity, rule.summary, "new", l.line, evidence)
		}
	}
	// Exact expectations replaced by looser ones, per hunk.
	relaxed := 0
	for _, h := range f.Hunks {
		strictAdded, strictRemoved, looseAdded, looseRemoved := 0, 0, 0, 0
		var first, firstRemoved *model.DiffLine
		for i := range h.Lines {
			d := &h.Lines[i]
			if d.Kind != "add" && d.Kind != "delete" {
				continue
			}
			strict, loose := p.expectation(d.Content)
			if d.Kind == "add" {
				if strict {
					strictAdded++
				}
				if loose {
					looseAdded++
					if first == nil {
						first = d
					}
				}
			} else {
				if strict {
					strictRemoved++
					if firstRemoved == nil {
						firstRemoved = d
					}
				}
				if loose {
					looseRemoved++
				}
			}
		}
		if strictRemoved > strictAdded && looseAdded > looseRemoved && first != nil && firstRemoved != nil && relaxed < maxRelaxedSignals {
			relaxed++
			add(model.SignalTestExpectationRelaxed, "medium", "Exact test expectation replaced by a looser one", "new", first.NewLine, fmt.Sprintf("removed %s; added %s", weakeningQuote(firstRemoved.Content), weakeningQuote(first.Content)))
		}
	}
	return signals
}

// weakeningQuote renders a diff line for signal evidence like the other
// linter rules do: trimmed and at most 240 bytes, kept valid UTF-8.
func weakeningQuote(s string) string {
	return strings.ToValidUTF8(short(s), "")
}
