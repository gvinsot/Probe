package mutation

// Mutation sites of TypeScript and JavaScript sources. There is no parser in
// the standard library for these languages, so sites are found on the tokens
// of the static index's lexical reader, inside the function and method bodies
// it records, with conservative rules: an operator is mutated only when it is
// written as a binary operator (white space on both sides), and no rule needs
// type information. A mutant that does not load is classified INVALID from the
// runner's report, never KILLED.

import (
	"bytes"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/gvinsot/Probe/app/internal/symbols"
)

// Script file skip reasons.
const (
	skipScriptRead      = "the file could not be read as TypeScript or JavaScript source"
	skipScriptGenerated = "the file is marked as generated code"
	skipScriptMinified  = "the file has a line longer than 1000 bytes (minified or generated code)"
)

// scriptSwaps are the operator replacements of TS/JS sites.
var scriptSwaps = map[string]struct{ op, replacement string }{
	"<":   {OpBoundary, "<="},
	"<=":  {OpBoundary, "<"},
	">":   {OpBoundary, ">="},
	">=":  {OpBoundary, ">"},
	"===": {OpNegateComparison, "!=="},
	"!==": {OpNegateComparison, "==="},
	"==":  {OpNegateComparison, "!="},
	"!=":  {OpNegateComparison, "=="},
	"&&":  {OpSwapLogical, "||"},
	"||":  {OpSwapLogical, "&&"},
	"+":   {OpSwapArithmetic, "-"},
	"-":   {OpSwapArithmetic, "+"},
	"*":   {OpSwapArithmetic, "/"},
	"/":   {OpSwapArithmetic, "*"},
}

// scriptComparisons are the operators whose integer operand
// increment_constant changes.
var scriptComparisons = map[string]bool{"<": true, "<=": true, ">": true, ">=": true, "===": true, "!==": true, "==": true, "!=": true}

// ScriptFileSites is FileSites for a TypeScript or JavaScript source: the
// sites of the function and method bodies whose whole replaced span lies on
// added lines, sorted by line, operator rank and column.
func ScriptFileSites(p string, src []byte, added map[int]bool) (sites []Site, capped bool, skip string) {
	read, ok := symbols.ReadScriptSource(p, src)
	if !ok {
		return nil, false, skipScriptRead
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
	offset := func(t symbols.ScriptToken) int {
		if t.Line < 1 || t.Line > len(starts) {
			return -1
		}
		return starts[t.Line-1] + t.Col - 1
	}
	toks := read.Tokens
	jsx := strings.HasSuffix(p, "x") // .tsx and .jsx: < and > may be tags
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
	// spaced reports whether token i is written with white space on both
	// sides, the form of a binary operator.
	spaced := func(i int) bool {
		start := offset(toks[i])
		end := start + len(toks[i].Text)
		return start > 0 && start <= len(src) && end < len(src) && isSpace(src[start-1]) && isSpace(src[end])
	}
	for _, fn := range read.Functions {
		for i := fn.Body; i <= fn.End && i < len(toks); i++ {
			t := toks[i]
			switch {
			case t.Kind == 'i' && (t.Text == "if" || t.Text == "while") && i+1 < len(toks) && toks[i+1].Text == "(" && toks[i+1].Match > i+1:
				open, close := i+1, toks[i+1].Match
				start, end := offset(toks[open]), offset(toks[close])
				if start >= 0 && end > start {
					add(open, close, OpNegateCondition, "(!("+string(src[start+1:end])+"))", fn.Name)
				}
			case t.Kind == 'i' && (t.Text == "true" || t.Text == "false") && i > 0 && (toks[i-1].Text == "return" || toks[i-1].Text == "=>"):
				add(i, i, OpFlipBoolean, map[string]string{"true": "false", "false": "true"}[t.Text], fn.Name)
			case t.Kind == 'p':
				swap, ok := scriptSwaps[t.Text]
				if !ok || !spaced(i) || jsx && (t.Text == "<" || t.Text == ">") {
					continue
				}
				add(i, i, swap.op, swap.replacement, fn.Name)
				if !scriptComparisons[t.Text] {
					continue
				}
				for _, j := range []int{i - 1, i + 1} {
					if j < 0 || j >= len(toks) || toks[j].Kind != 'n' {
						continue
					}
					if n, err := strconv.ParseInt(toks[j].Text, 10, 64); err == nil && n >= 0 && n < 1<<53 {
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

// lineStarts returns the byte offset of the start of every line of src.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// scriptJoinedTokens are the TS/JS tokens a replacement could form with a
// neighbouring character; such sites are refused at Apply time.
var scriptJoinedTokens = map[string]bool{
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true, "^=": true,
	"<<": true, ">>": true, "&&": true, "||": true, "??": true, "++": true, "--": true, "**": true,
	"==": true, "!=": true, "<=": true, ">=": true, "=>": true, "?.": true, "//": true, "/*": true, "*/": true,
}

// applyScript is Apply for a TS/JS site: the recorded span must still hold
// Original, the replacement must not join a neighbouring character into
// another token, and the line count must not change. Nothing parses the
// result: a mutant that does not load is classified from the runner's report.
func (s Site) applyScript(src []byte) ([]byte, error) {
	if s.Start < 0 || s.End > len(src) || s.Start >= s.End || string(src[s.Start:s.End]) != s.Original {
		return nil, errors.New("the source no longer holds the original text at the recorded position")
	}
	r := s.Replacement
	if r == "" || s.Start > 0 && scriptJoinedTokens[string([]byte{src[s.Start-1], r[0]})] || s.End < len(src) && scriptJoinedTokens[string([]byte{r[len(r)-1], src[s.End]})] {
		return nil, errors.New("the replacement would merge with a neighbouring character into another token")
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

// scriptPath reports whether a mutation site or mutant path is a TypeScript
// or JavaScript source rather than a Go file.
func scriptPath(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}
