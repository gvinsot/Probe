package fuzz

// TS/JS enumeration (F2b, Appendix D.13). No TS/JS harness runs in this build:
// changed exported TS/JS functions whose signature is unchanged are listed in
// fuzz.skipped with a fixed reason, so the report says which changed functions
// differential fuzzing did not cover. The enumeration is lexical and
// best-effort: it reads the two snapshots on the host, never executes
// anything, and a construct it does not recognize is simply not listed.
//
// Cost: the files are candidate content, so every bound is linear in their
// size whatever they contain. Tokenizing visits each byte a bounded number of
// times and nests template literals at most maxScriptTemplateDepth deep; the
// parse of one module spends at most scriptStepsPerToken token visits per
// token; at most maxScriptSourceBytes of source are read in total; and the
// caller's context (the fuzz sub-cap and --deadline) is checked between files.
// A file that one of these bounds stops gets one file-level entry (line 0)
// instead of its functions.

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// ReasonScriptNotImplemented is the skip reason of a changed exported TS/JS
// function whose signature is unchanged (Appendix D.13).
const ReasonScriptNotImplemented = "TS/JS differential fuzzing is not implemented in this build"

// Bounds of the TS/JS enumeration.
const (
	// maxScriptTokens bounds the tokens read from one TS/JS file.
	maxScriptTokens = 1 << 20
	// maxScriptTemplateDepth bounds the nesting of template literals and
	// their substitutions.
	maxScriptTemplateDepth = 64
	// scriptStepsPerToken and scriptStepsBase bound the token visits of the
	// parse of one module: scriptStepsPerToken per token plus scriptStepsBase.
	scriptStepsPerToken = 16
	scriptStepsBase     = 1 << 16
	// maxScriptSourceBytes bounds the source read in total, both revisions
	// counted: no further file is read once it is reached.
	maxScriptSourceBytes = 32 << 20
)

// scriptSourceBudget is maxScriptSourceBytes; tests lower it.
var scriptSourceBudget = maxScriptSourceBytes

// File-level skip reasons of the TS/JS enumeration (line 0, no symbol).
const (
	ReasonScriptTooLarge   = "TS/JS functions of this file were not enumerated: the file exceeds 2 MiB on one revision"
	ReasonScriptScanBound  = "TS/JS functions of this file were not enumerated: its lexical scan reached a bound (1,048,576 tokens, template nesting depth 64, or a work bound of 16 token visits per token)"
	ReasonScriptTotalBound = "TS/JS functions of this file were not enumerated: 32 MiB of TS/JS source had already been read"
	ReasonScriptTimeLimit  = "TS/JS functions of this file were not enumerated: the time limit of the fuzz stage (fuzz.max_runtime_seconds or --deadline) was reached"
)

var scriptExtensions = map[string]bool{".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true}

// SelectScripts lists the changed exported TS/JS functions whose body changed
// and whose signature text (type parameters, parameters and return type,
// compared token by token) is identical on both revisions, each with
// ReasonScriptNotImplemented. Only modified or renamed non-test source files
// are read, at most 2 MiB each and 32 MiB in total, from the two snapshots;
// declaration files, test files, hidden directories, node_modules, vendor,
// testdata and paths the sandbox excludes are ignored. Functions are
// recognized lexically: function declarations (async, generator, default) and
// const, let or var bindings of function expressions and arrow functions at
// the top level of the module, exported directly or through an export list
// without "from". A file that is too large, whose scan reaches a bound, or
// that comes after the total bound or after ctx is done gets one file-level
// entry with the reason instead. The result is sorted by path and line.
func SelectScripts(ctx context.Context, baseDir, candidateDir string, change model.Change) []model.FuzzSkip {
	var out []model.FuzzSkip
	seen := map[string]bool{}
	read := 0
	for _, f := range change.Files {
		if !scriptSourceCandidate(f) || seen[f.Path] || harness.IsSensitivePath(f.Path) {
			continue
		}
		seen[f.Path] = true
		old := f.Path
		if f.Status == "R" && f.OldPath != "" {
			old = f.OldPath
			if !scriptSourcePath(old) || harness.IsSensitivePath(old) {
				continue
			}
		}
		fileSkip := func(reason string) {
			out = append(out, model.FuzzSkip{Path: f.Path, Reason: reason})
		}
		switch {
		case ctx.Err() != nil:
			fileSkip(ReasonScriptTimeLimit)
			continue
		case read >= scriptSourceBudget:
			fileSkip(ReasonScriptTotalBound)
			continue
		}
		baseSrc, err := readSnapshotFile(baseDir, old)
		if err != nil {
			if errors.Is(err, errSourceTooLarge) {
				fileSkip(ReasonScriptTooLarge)
			}
			continue
		}
		candSrc, err := readSnapshotFile(candidateDir, f.Path)
		if err != nil {
			if errors.Is(err, errSourceTooLarge) {
				fileSkip(ReasonScriptTooLarge)
			}
			continue
		}
		read += len(baseSrc) + len(candSrc)
		base, baseCut := scriptFunctions(baseSrc)
		candidate, candCut := scriptFunctions(candSrc)
		if baseCut || candCut {
			fileSkip(ReasonScriptScanBound)
			continue
		}
		for _, fn := range sortedScriptFunctions(candidate) {
			b, ok := base[fn.name]
			if !ok || b.signature != fn.signature || b.body == fn.body {
				continue
			}
			out = append(out, model.FuzzSkip{Path: f.Path, Line: fn.line, Symbol: shortText(fn.name, 128), Reason: ReasonScriptNotImplemented})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// scriptSourceCandidate reports whether f is a modified or renamed TS/JS
// source file that SelectScripts reads.
func scriptSourceCandidate(f model.ChangedFile) bool {
	if f.Binary || f.Status != "M" && f.Status != "R" {
		return false
	}
	return scriptSourcePath(f.Path)
}

func scriptSourcePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p {
		return false
	}
	base := path.Base(p)
	ext := path.Ext(base)
	if !scriptExtensions[ext] {
		return false
	}
	stem := strings.TrimSuffix(base, ext)
	if strings.HasSuffix(stem, ".d") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "" || part == ".." || strings.HasPrefix(part, "."):
			return false
		case part == "node_modules" || part == "vendor" || part == "testdata" || part == "__tests__" || part == "__mocks__":
			return false
		}
	}
	return true
}

// readSnapshotFile reads a regular file of a snapshot, refusing symlinked
// components and files above the source bound (errSourceTooLarge).
func readSnapshotFile(root, rel string) ([]byte, error) {
	cursor := root
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cursor = filepath.Join(cursor, part)
		info, err := os.Lstat(cursor)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlinked path")
		}
		if i == len(parts)-1 {
			switch {
			case !info.Mode().IsRegular():
				return nil, errors.New("not a regular file")
			case info.Size() > maxSourceBytes:
				return nil, errSourceTooLarge
			}
		}
	}
	return readLimited(cursor, maxSourceBytes)
}

// scriptFunction is one recognized TS/JS function of a module.
type scriptFunction struct {
	name      string // exported name ("default" for a default export)
	line      int    // line of the declaration's first token
	signature string // tokens of type parameters, parameters and return type
	body      string // tokens of the body
}

func sortedScriptFunctions(m map[string]scriptFunction) []scriptFunction {
	out := make([]scriptFunction, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].name < out[j].name
	})
	return out
}

// scriptToken is one lexical token of a TS/JS module. Comments are dropped;
// strings, template literals and regular expression literals are single
// tokens.
type scriptToken struct {
	text string
	line int
}

// regexKeywords may precede a regular expression literal.
var regexKeywords = map[string]bool{"return": true, "typeof": true, "instanceof": true, "in": true, "of": true, "new": true, "delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true, "yield": true, "await": true}

// scriptTokens splits a TS/JS module into tokens. It is lexical and never
// fails: an unterminated string ends at the line end, and an unterminated
// comment or template literal at the end of the input. Only "=>", "..." and
// "?." are multi-character punctuation, which keeps nested generic brackets
// apart. cut reports that a bound stopped it: maxScriptTokens tokens, or
// template literals nested deeper than maxScriptTemplateDepth.
func scriptTokens(src string) (toks []scriptToken, cut bool) {
	line := 1
	regexAllowed := func() bool {
		if len(toks) == 0 {
			return true
		}
		prev := toks[len(toks)-1].text
		switch c := prev[0]; {
		case isScriptIdentStart(c) || c >= '0' && c <= '9':
			return regexKeywords[prev]
		case c == '"' || c == '\'' || c == '`':
			return false
		case prev == ")" || prev == "]" || prev == "}":
			return false
		case c == '/' && len(prev) > 1:
			return false // a regular expression literal
		}
		return true
	}
	for i := 0; i < len(src); {
		if len(toks) == maxScriptTokens {
			return toks, true
		}
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = len(src) - i - 2
			}
			line += strings.Count(src[i:i+2+end], "\n")
			i += end + 4
		case c == '"' || c == '\'':
			start := i
			i++
			for i < len(src) && src[i] != c && src[i] != '\n' {
				if src[i] == '\\' && i+1 < len(src) && src[i+1] != '\n' {
					i++
				}
				i++
			}
			if i < len(src) && src[i] == c {
				i++
			}
			toks = append(toks, scriptToken{src[start:i], line})
		case c == '`':
			start, startLine := i, line
			i = skipTemplate(src, i, 1, &cut)
			if cut {
				return toks, true
			}
			line += strings.Count(src[start:i], "\n")
			toks = append(toks, scriptToken{src[start:i], startLine})
		case c == '/' && regexAllowed():
			start := i
			i++
			inClass := false
			for i < len(src) && src[i] != '\n' {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				if src[i] == '[' {
					inClass = true
				} else if src[i] == ']' {
					inClass = false
				} else if src[i] == '/' && !inClass {
					i++
					break
				}
				i++
			}
			for i < len(src) && isScriptIdentPart(src[i]) {
				i++
			}
			toks = append(toks, scriptToken{src[start:i], line})
		case isScriptIdentStart(c):
			start := i
			for i < len(src) && isScriptIdentPart(src[i]) {
				i++
			}
			toks = append(toks, scriptToken{src[start:i], line})
		case c >= '0' && c <= '9':
			start := i
			for i < len(src) && (isScriptIdentPart(src[i]) || src[i] == '.') {
				i++
			}
			toks = append(toks, scriptToken{src[start:i], line})
		default:
			n := 1
			for _, op := range []string{"=>", "...", "?."} {
				if strings.HasPrefix(src[i:], op) {
					n = len(op)
					break
				}
			}
			toks = append(toks, scriptToken{src[i : i+n], line})
			i += n
		}
	}
	return toks, false
}

func isScriptIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isScriptIdentPart(c byte) bool { return isScriptIdentStart(c) || c >= '0' && c <= '9' }

// skipTemplate returns the index after the template literal starting at i,
// following ${...} substitutions with their own strings, comments and nested
// template literals. depth counts the template literals and substitutions
// open around i; beyond maxScriptTemplateDepth it sets *cut and returns
// len(src), so nesting never costs more than bounded stack. Every byte is
// visited once.
func skipTemplate(src string, i, depth int, cut *bool) int {
	if depth > maxScriptTemplateDepth {
		*cut = true
		return len(src)
	}
	i++ // opening backtick
	for i < len(src) {
		switch {
		case src[i] == '\\':
			i += 2
		case src[i] == '`':
			return i + 1
		case src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			i = skipSubstitution(src, i+2, depth+1, cut)
		default:
			i++
		}
	}
	return len(src)
}

// skipSubstitution returns the index after the "}" that closes a template
// substitution whose body starts at i. depth is as for skipTemplate.
func skipSubstitution(src string, i, depth int, cut *bool) int {
	if depth > maxScriptTemplateDepth {
		*cut = true
		return len(src)
	}
	braces := 1
	for i < len(src) {
		switch c := src[i]; {
		case c == '{':
			braces++
			i++
		case c == '}':
			braces--
			i++
			if braces == 0 {
				return i
			}
		case c == '`':
			i = skipTemplate(src, i, depth+1, cut)
		case c == '"' || c == '\'':
			i++
			for i < len(src) && src[i] != c && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return len(src)
			}
			i += end + 4
		default:
			i++
		}
	}
	return len(src)
}

// scriptParser holds the tokens of one module and the token visits its parse
// may still spend. Every scan of the parse spends one visit per token it
// reads, so the parse is linear in the module's size whatever it contains: a
// failed attempt that scans far ahead is paid from the same budget.
type scriptParser struct {
	toks  []scriptToken
	steps int
}

func newScriptParser(toks []scriptToken) *scriptParser {
	return &scriptParser{toks: toks, steps: scriptStepsPerToken*len(toks) + scriptStepsBase}
}

// at returns the text of token k, or "" outside the module.
func (p *scriptParser) at(k int) string {
	if k < 0 || k >= len(p.toks) {
		return ""
	}
	return p.toks[k].text
}

// spend uses one token visit and reports whether one was left.
func (p *scriptParser) spend() bool {
	if p.steps <= 0 {
		return false
	}
	p.steps--
	return true
}

// scriptFunctions returns the exported functions of a module by exported
// name. A name recognized more than once keeps its last declaration. cut
// reports that a bound stopped the tokenizer or the parse, so the result may
// miss functions.
func scriptFunctions(src []byte) (map[string]scriptFunction, bool) {
	toks, cut := scriptTokens(string(src))
	p := newScriptParser(toks)
	exported := map[string]scriptFunction{}
	locals := map[string]scriptFunction{}
	var lists [][2]string // local name, exported name
	depth := 0
	for i := 0; i < len(toks); {
		if !p.spend() {
			return exported, true
		}
		switch p.at(i) {
		case "{", "(", "[":
			depth++
			i++
			continue
		case "}", ")", "]":
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if depth != 0 {
			i++
			continue
		}
		start, j := i, i
		export, byDefault := false, false
		if p.at(j) == "export" {
			export = true
			j++
			if p.at(j) == "default" {
				byDefault = true
				j++
			}
		}
		if export && !byDefault && p.at(j) == "{" {
			names, next := p.exportList(j)
			if p.at(next) != "from" {
				lists = append(lists, names...)
			}
			i = next
			continue
		}
		if byDefault && isScriptName(p.at(j)) && p.at(j) != "function" && p.at(j) != "async" && p.at(j) != "class" && (p.at(j+1) == ";" || j+1 >= len(toks) || toks[j+1].line > toks[j].line) {
			lists = append(lists, [2]string{p.at(j), "default"})
			i = j + 1
			continue
		}
		fn, next, ok := p.function(j, byDefault)
		if !ok {
			if export {
				i = j
			} else {
				i++
			}
			continue
		}
		fn.line = toks[start].line
		switch {
		case byDefault:
			fn.name = "default"
			exported[fn.name] = fn
		case export:
			exported[fn.name] = fn
		default:
			locals[fn.name] = fn
		}
		i = next
	}
	for _, pair := range lists {
		if fn, ok := locals[pair[0]]; ok {
			fn.name = pair[1]
			exported[pair[1]] = fn
		}
	}
	return exported, cut || p.steps <= 0
}

// exportList parses "{ a, b as c }" starting at the opening brace and returns
// the (local, exported) pairs and the index after the closing brace.
func (p *scriptParser) exportList(i int) ([][2]string, int) {
	var out [][2]string
	i++
	for i < len(p.toks) && p.toks[i].text != "}" && p.spend() {
		name := p.toks[i].text
		if !isScriptName(name) {
			i++
			continue
		}
		as := name
		if p.at(i+1) == "as" && isScriptName(p.at(i+2)) {
			as = p.at(i + 2)
			i += 2
		}
		if name != "type" || as != name {
			out = append(out, [2]string{name, as})
		}
		i++
	}
	return out, i + 1
}

func isScriptName(s string) bool { return s != "" && isScriptIdentStart(s[0]) }

// function recognizes a function at token i: a function declaration or
// expression, a const, let or var binding of one or of an arrow function, or,
// when anonymous is true (a default export), a bare arrow function. It returns
// the function, the index after it, and false when token i starts no function
// with a body.
func (p *scriptParser) function(i int, anonymous bool) (scriptFunction, int, bool) {
	// async becomes part of the signature: it changes what the function
	// returns.
	asyncPrefix := func(fn scriptFunction, next int, ok bool) (scriptFunction, int, bool) {
		fn.signature = "async " + fn.signature
		return fn, next, ok
	}
	var fn scriptFunction
	switch p.at(i) {
	case "const", "let", "var":
		if !isScriptName(p.at(i + 1)) {
			return fn, 0, false
		}
		name := p.at(i + 1)
		k := i + 2
		if p.at(k) == ":" {
			k = p.scanUntil(k+1, "=")
		}
		if p.at(k) != "=" {
			return fn, 0, false
		}
		k++
		async := p.at(k) == "async" && (p.at(k+1) == "function" || p.at(k+1) == "(" || p.at(k+1) == "<" || isScriptName(p.at(k+1)) && p.at(k+2) == "=>")
		if async {
			k++
		}
		var next int
		var ok bool
		if p.at(k) == "function" {
			fn, next, ok = p.declaration(k)
		} else {
			fn, next, ok = p.arrow(k, "")
		}
		fn.name = name
		if async {
			return asyncPrefix(fn, next, ok)
		}
		return fn, next, ok
	case "async":
		if p.at(i+1) == "function" {
			return asyncPrefix(p.declaration(i + 1))
		}
		if anonymous {
			return asyncPrefix(p.arrow(i+1, ""))
		}
	case "function":
		return p.declaration(i)
	case "(", "<":
		if anonymous {
			return p.arrow(i, "")
		}
	}
	return fn, 0, false
}

// declaration parses "function [*] [name] [<...>] (...) [: type] {...}"
// starting at the function keyword.
func (p *scriptParser) declaration(i int) (scriptFunction, int, bool) {
	var fn scriptFunction
	k := i + 1
	generator := p.at(k) == "*"
	if generator {
		k++
	}
	if isScriptName(p.at(k)) {
		fn.name = p.at(k)
		k++
	}
	sigStart := k
	if p.at(k) == "<" {
		k = p.matchAngle(k)
	}
	if p.at(k) != "(" {
		return fn, 0, false
	}
	k = p.matchClose(k)
	if p.at(k) == ":" {
		k = p.returnTypeEnd(k + 1)
	}
	if p.at(k) != "{" {
		return fn, 0, false // an overload or a declaration without a body
	}
	end := p.matchClose(k)
	if end > len(p.toks) {
		return fn, 0, false
	}
	fn.signature = joinTokens(p.toks[sigStart:k])
	if generator {
		fn.signature = "* " + fn.signature
	}
	fn.body = joinTokens(p.toks[k:end])
	return fn, end, true
}

// arrow parses "[<...>] (params) [: type] => body" or "name => body" starting
// at token i. An expression body ends at a semicolon or comma at its own
// nesting level, or before a later line that starts a new top-level
// statement.
func (p *scriptParser) arrow(i int, name string) (scriptFunction, int, bool) {
	fn := scriptFunction{name: name}
	k := i
	sigStart := k
	switch {
	case p.at(k) == "<" || p.at(k) == "(":
		if p.at(k) == "<" {
			k = p.matchAngle(k)
		}
		if p.at(k) != "(" {
			return fn, 0, false
		}
		k = p.matchClose(k)
		if p.at(k) == ":" {
			k = p.scanUntil(k+1, "=>")
		}
	case isScriptName(p.at(k)):
		k++
	default:
		return fn, 0, false
	}
	if p.at(k) != "=>" {
		return fn, 0, false
	}
	fn.signature = joinTokens(p.toks[sigStart:k])
	k++
	if p.at(k) == "{" {
		end := p.matchClose(k)
		if end > len(p.toks) {
			return fn, 0, false
		}
		fn.body = joinTokens(p.toks[k:end])
		return fn, end, true
	}
	start, depth := k, 0
	for ; k < len(p.toks); k++ {
		if !p.spend() {
			return fn, 0, false
		}
		t := p.toks[k].text
		if depth == 0 && (t == ";" || t == ",") {
			break
		}
		if depth == 0 && k > start && p.toks[k].line > p.toks[k-1].line && statementStart[t] {
			break
		}
		switch t {
		case "{", "(", "[":
			depth++
		case "}", ")", "]":
			if depth == 0 {
				fn.body = joinTokens(p.toks[start:k])
				return fn, k, k > start
			}
			depth--
		}
	}
	fn.body = joinTokens(p.toks[start:k])
	return fn, k, k > start
}

// statementStart lists the tokens that start a new top-level statement after
// an expression-bodied arrow function or a type annotation without a
// semicolon.
var statementStart = map[string]bool{"export": true, "import": true, "const": true, "let": true, "var": true, "function": true, "class": true, "async": true, "interface": true, "type": true, "enum": true, "declare": true}

// matchClose returns the index after the bracket that closes token i, or
// len(toks) when none does. It returns len(toks)+1 when the parse ran out of
// token visits, so that a caller never takes a cut scan for a whole body.
func (p *scriptParser) matchClose(i int) int {
	depth := 0
	for k := i; k < len(p.toks); k++ {
		if !p.spend() {
			return len(p.toks) + 1
		}
		switch p.toks[k].text {
		case "{", "(", "[":
			depth++
		case "}", ")", "]":
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return len(p.toks)
}

// matchAngle returns the index after the ">" that closes the type parameter
// list opening at token i.
func (p *scriptParser) matchAngle(i int) int {
	depth := 0
	for k := i; k < len(p.toks) && p.spend(); k++ {
		switch p.toks[k].text {
		case "<":
			depth++
		case ">":
			depth--
			if depth == 0 {
				return k + 1
			}
		case "{", ";":
			return k
		}
	}
	return len(p.toks)
}

// returnTypeEnd returns the index of the "{" that opens the body after a
// return type annotation starting at token i. An object type literal right
// after the colon is part of the type.
func (p *scriptParser) returnTypeEnd(i int) int {
	k := i
	if p.at(k) == "{" {
		k = p.matchClose(k)
	}
	depth := 0
	for ; k < len(p.toks) && p.spend(); k++ {
		switch p.toks[k].text {
		case "(", "[", "<":
			depth++
		case ")", "]", ">":
			depth--
		case "{":
			if depth <= 0 {
				return k
			}
		case ";":
			return k
		}
	}
	return len(p.toks)
}

// scanUntil returns the index of the first token equal to stop at the
// nesting level of token i, or of the first semicolon there, or of a token
// that starts a new top-level statement on a later line (a type annotation
// without an initializer, in code that omits semicolons), or len(toks).
func (p *scriptParser) scanUntil(i int, stop string) int {
	depth := 0
	for k := i; k < len(p.toks) && p.spend(); k++ {
		t := p.toks[k].text
		if depth <= 0 && t == stop {
			return k
		}
		if depth <= 0 && k > i && p.toks[k].line > p.toks[k-1].line && statementStart[t] {
			return k
		}
		switch t {
		case "{", "(", "[", "<":
			depth++
		case "}", ")", "]", ">":
			depth--
		case ";":
			if depth <= 0 {
				return k
			}
		}
	}
	return len(p.toks)
}

func joinTokens(toks []scriptToken) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}
