package mutation

// Mutation sites of Python sources, found as for TypeScript and JavaScript
// on the tokens of the static index's lexical reader, inside the function
// and method bodies it records, with conservative rules: an operator is
// mutated only when it is written with white space on both sides, a
// condition only after if, elif or while at the start of a statement, and no
// rule needs type information. A mutant that does not import is classified
// INVALID from pytest's report (a collection error), never KILLED.

import (
	"bytes"
	"errors"
	"path"
	"sort"
	"strconv"

	"github.com/gvinsot/Probe/app/internal/pytestcmd"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// Mode is the language a mutation command mutates.
type Mode int

const (
	// ModeGo mutates Go files with a go test command.
	ModeGo Mode = iota
	// ModeScript mutates TypeScript and JavaScript sources with a Vitest or
	// Jest command.
	ModeScript
	// ModePython mutates Python sources with a pytest command.
	ModePython
)

// CommandMode returns the Mode of a mutation command.
func CommandMode(command []string) Mode {
	switch {
	case PythonCommand(command):
		return ModePython
	case ScriptCommand(command):
		return ModeScript
	}
	return ModeGo
}

// PythonCommand reports whether a mutation command runs pytest (its mutants
// are Python sites, its outcomes read from a JUnit XML report).
func PythonCommand(command []string) bool { return pytestcmd.Is(command) }

// Python file skip reasons.
const skipPythonRead = "the file could not be read as Python source"

// pythonSwaps are the operator replacements of Python sites.
var pythonSwaps = map[string]struct{ op, replacement string }{
	"<":   {OpBoundary, "<="},
	"<=":  {OpBoundary, "<"},
	">":   {OpBoundary, ">="},
	">=":  {OpBoundary, ">"},
	"==":  {OpNegateComparison, "!="},
	"!=":  {OpNegateComparison, "=="},
	"and": {OpSwapLogical, "or"},
	"or":  {OpSwapLogical, "and"},
	"+":   {OpSwapArithmetic, "-"},
	"-":   {OpSwapArithmetic, "+"},
	"*":   {OpSwapArithmetic, "/"},
	"/":   {OpSwapArithmetic, "*"},
}

// pythonComparisons are the operators whose integer operand
// increment_constant changes.
var pythonComparisons = map[string]bool{"<": true, "<=": true, ">": true, ">=": true, "==": true, "!=": true}

// PythonFileSites is FileSites for a Python source: the sites of the
// function and method bodies whose whole replaced span lies on added lines,
// sorted by line, operator rank and column.
func PythonFileSites(p string, src []byte, added map[int]bool) (sites []Site, capped bool, skip string) {
	read, ok := symbols.ReadPythonSource(p, src)
	if !ok {
		return nil, false, skipPythonRead
	}
	head := src
	if len(head) > 2048 {
		head = head[:2048]
	}
	if bytes.Contains(head, []byte("@generated")) || bytes.Contains(head, []byte("DO NOT EDIT")) {
		return nil, false, skipScriptGenerated
	}
	for _, line := range bytes.Split(src, []byte("\n")) {
		if len(line) > 1000 {
			return nil, false, skipScriptMinified
		}
	}
	starts := lineStarts(src)
	toks := read.Tokens
	offset := func(t symbols.PythonToken) int {
		if t.Line < 1 || t.Line > len(starts) {
			return -1
		}
		return starts[t.Line-1] + t.Col - 1
	}
	seen := map[[2]int]bool{}
	add := func(first, last int, op, replacement, symbol string) {
		if capped || first < 0 || last >= len(toks) || first > last {
			return
		}
		start, end := offset(toks[first]), offset(toks[last])
		if start < 0 || end < 0 {
			return
		}
		end += len(toks[last].Text)
		if end > len(src) || start >= end {
			return
		}
		endLine := toks[first].Line + bytes.Count(src[start:end], []byte("\n"))
		for l := toks[first].Line; l <= endLine; l++ {
			if !added[l] {
				return
			}
		}
		if seen[[2]int{start, end}] && op != OpNegateCondition {
			return
		}
		seen[[2]int{start, end}] = true
		if len(sites) >= maxSitesPerFile {
			capped = true
			return
		}
		sites = append(sites, Site{Path: p, Line: toks[first].Line, EndLine: endLine, Column: toks[first].Col, Start: start, End: end,
			Operator: op, Original: string(src[start:end]), Replacement: replacement, Symbol: symbol})
	}
	spaced := func(i int) bool {
		start := offset(toks[i])
		end := start + len(toks[i].Text)
		return start > 0 && start <= len(src) && end < len(src) && isSpace(src[start-1]) && isSpace(src[end])
	}
	for _, fn := range read.Functions {
		for i := fn.Body; i <= fn.End && i < len(toks); i++ {
			t := toks[i]
			switch {
			case t.Kind == 'i' && t.First && (t.Text == "if" || t.Text == "elif" || t.Text == "while"):
				if last, ok := pythonCondition(toks, i, offset); ok {
					start, end := offset(toks[i+1]), offset(toks[last])+len(toks[last].Text)
					add(i+1, last, OpNegateCondition, "not ("+string(src[start:end])+")", fn.Name)
				}
			case t.Kind == 'i' && (t.Text == "True" || t.Text == "False") && i > 0 && toks[i-1].Text == "return" && (i+1 >= len(toks) || toks[i+1].First):
				add(i, i, OpFlipBoolean, map[string]string{"True": "False", "False": "True"}[t.Text], fn.Name)
			case t.Kind == 'i' && (t.Text == "and" || t.Text == "or"):
				swap := pythonSwaps[t.Text]
				add(i, i, swap.op, swap.replacement, fn.Name)
			case t.Kind == 'p':
				swap, ok := pythonSwaps[t.Text]
				if !ok || !spaced(i) {
					continue
				}
				add(i, i, swap.op, swap.replacement, fn.Name)
				if !pythonComparisons[t.Text] {
					continue
				}
				for _, j := range []int{i - 1, i + 1} {
					if j < 0 || j >= len(toks) || toks[j].Kind != 'n' || j == i+1 && toks[j].First {
						continue
					}
					if n, err := strconv.ParseInt(toks[j].Text, 10, 64); err == nil && n >= 0 && n < 1<<53 && toks[j].Text == strconv.FormatInt(n, 10) {
						add(j, j, OpIncrementConstant, strconv.FormatInt(n+1, 10), fn.Name)
					}
				}
			}
		}
	}
	sort.SliceStable(sites, func(i, j int) bool {
		a, b := sites[i], sites[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.rank() != b.rank() {
			return a.rank() < b.rank()
		}
		return a.Column < b.Column
	})
	return sites, capped, ""
}

// pythonCondition returns the last token of the condition of the if, elif
// or while statement whose keyword is token k: the tokens up to the ":" that
// ends the statement's header, outside brackets, a walrus ":=" excepted. It
// returns false for an empty condition, a header without that ":" on its
// logical line, and a condition holding a lambda outside brackets.
func pythonCondition(toks []symbols.PythonToken, k int, offset func(symbols.PythonToken) int) (int, bool) {
	for i := k + 1; i < len(toks) && !toks[i].First; i++ {
		t := toks[i]
		switch {
		case t.Kind == 'p' && t.Match > i:
			i = t.Match
		case t.Kind == 'i' && t.Text == "lambda":
			return 0, false
		case t.Kind == 'p' && t.Text == ":":
			if i+1 < len(toks) && toks[i+1].Text == "=" && offset(toks[i+1]) == offset(t)+1 {
				i++ // a walrus
				continue
			}
			if i == k+1 {
				return 0, false
			}
			return i - 1, true
		}
	}
	return 0, false
}

// pythonJoinedTokens are the Python tokens a replacement could form with a
// neighbouring character; such sites are refused at Apply time.
var pythonJoinedTokens = map[string]bool{
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true, "^=": true, "@=": true,
	"<<": true, ">>": true, "**": true, "//": true, "==": true, "!=": true, "<=": true, ">=": true, "->": true, ":=": true,
}

// applyPython is Apply for a Python site: the recorded span must still hold
// Original, the replacement must not join a neighbouring character into
// another token, and the line count must not change. Nothing parses the
// result: a mutant that does not import is classified from pytest's report.
func (s Site) applyPython(src []byte) ([]byte, error) {
	if s.Start < 0 || s.End > len(src) || s.Start >= s.End || string(src[s.Start:s.End]) != s.Original {
		return nil, errors.New("the source no longer holds the original text at the recorded position")
	}
	r := s.Replacement
	if r == "" || s.Start > 0 && pythonJoinedTokens[string([]byte{src[s.Start-1], r[0]})] || s.End < len(src) && pythonJoinedTokens[string([]byte{r[len(r)-1], src[s.End]})] {
		return nil, errors.New("the replacement would merge with a neighbouring character into another token")
	}
	if isIdentByte(r[0]) && s.Start > 0 && isIdentByte(src[s.Start-1]) || isIdentByte(r[len(r)-1]) && s.End < len(src) && isIdentByte(src[s.End]) {
		return nil, errors.New("the replacement would merge with a neighbouring name")
	}
	out := make([]byte, 0, len(src)-len(s.Original)+len(r))
	out = append(out, src[:s.Start]...)
	out = append(out, r...)
	out = append(out, src[s.End:]...)
	if bytes.Count(out, []byte("\n")) != bytes.Count(src, []byte("\n")) {
		return nil, errors.New("the mutant would change the number of lines")
	}
	return out, nil
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// pythonPath reports whether a mutation site or mutant path is a Python
// source.
func pythonPath(p string) bool { return path.Ext(p) == ".py" }
