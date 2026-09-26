package fuzz

// TS/JS enumeration (F2b, Appendix D.13). No TS/JS harness runs in this build:
// changed exported TS/JS functions whose signature is unchanged are listed in
// fuzz.skipped with a fixed reason, so the report says which changed functions
// differential fuzzing did not cover. The enumeration is lexical and
// best-effort: it reads the two snapshots on the host, never executes
// anything, and a construct it does not recognize is simply not listed.

import (
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

// maxScriptTokens bounds the tokens read from one TS/JS file.
const maxScriptTokens = 1 << 20

var scriptExtensions = map[string]bool{".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true}

// SelectScripts lists the changed exported TS/JS functions whose body changed
// and whose signature text (type parameters, parameters and return type,
// compared token by token) is identical on both revisions, each with
// ReasonScriptNotImplemented. Only modified or renamed non-test source files
// are read, at most 2 MiB each, from the two snapshots; declaration files,
// test files, hidden directories, node_modules, vendor, testdata and paths the
// sandbox excludes are ignored. Functions are recognized lexically: function
// declarations (async, generator, default) and const, let or var bindings of
// function expressions and arrow functions at the top level of the module,
// exported directly or through an export list without "from". The result is
// sorted by path and line.
func SelectScripts(baseDir, candidateDir string, change model.Change) []model.FuzzSkip {
	var out []model.FuzzSkip
	seen := map[string]bool{}
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
		baseSrc, err := readSnapshotFile(baseDir, old)
		if err != nil {
			continue
		}
		candSrc, err := readSnapshotFile(candidateDir, f.Path)
		if err != nil {
			continue
		}
		base := scriptFunctions(baseSrc)
		for _, fn := range sortedScriptFunctions(scriptFunctions(candSrc)) {
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
// components and files above the source bound.
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
		if i == len(parts)-1 && (!info.Mode().IsRegular() || info.Size() > maxSourceBytes) {
			return nil, errors.New("not a readable source file")
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
// apart.
func scriptTokens(src string) []scriptToken {
	var toks []scriptToken
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
	for i := 0; i < len(src) && len(toks) < maxScriptTokens; {
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
			i = skipTemplate(src, i)
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
	return toks
}

func isScriptIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isScriptIdentPart(c byte) bool { return isScriptIdentStart(c) || c >= '0' && c <= '9' }

// skipTemplate returns the index after the template literal starting at i,
// following ${...} substitutions with their own strings, comments and nested
// template literals.
func skipTemplate(src string, i int) int {
	i++ // opening backtick
	for i < len(src) {
		switch {
		case src[i] == '\\':
			i += 2
		case src[i] == '`':
			return i + 1
		case src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			i = skipSubstitution(src, i+2)
		default:
			i++
		}
	}
	return len(src)
}

// skipSubstitution returns the index after the "}" that closes a template
// substitution whose body starts at i.
func skipSubstitution(src string, i int) int {
	depth := 1
	for i < len(src) {
		switch c := src[i]; {
		case c == '{':
			depth++
			i++
		case c == '}':
			depth--
			i++
			if depth == 0 {
				return i
			}
		case c == '`':
			i = skipTemplate(src, i)
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

// scriptFunctions returns the exported functions of a module by exported
// name. A name recognized more than once keeps its last declaration.
func scriptFunctions(src []byte) map[string]scriptFunction {
	toks := scriptTokens(string(src))
	at := func(i int) string {
		if i < 0 || i >= len(toks) {
			return ""
		}
		return toks[i].text
	}
	exported := map[string]scriptFunction{}
	locals := map[string]scriptFunction{}
	var lists [][2]string // local name, exported name
	depth := 0
	for i := 0; i < len(toks); {
		switch at(i) {
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
		if at(j) == "export" {
			export = true
			j++
			if at(j) == "default" {
				byDefault = true
				j++
			}
		}
		if export && !byDefault && at(j) == "{" {
			names, next := exportList(toks, j)
			if at(next) != "from" {
				lists = append(lists, names...)
			}
			i = next
			continue
		}
		if byDefault && isScriptName(at(j)) && at(j) != "function" && at(j) != "async" && at(j) != "class" && (at(j+1) == ";" || j+1 >= len(toks) || toks[j+1].line > toks[j].line) {
			lists = append(lists, [2]string{at(j), "default"})
			i = j + 1
			continue
		}
		fn, next, ok := parseScriptFunction(toks, j, byDefault)
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
	return exported
}

// exportList parses "{ a, b as c }" starting at the opening brace and returns
// the (local, exported) pairs and the index after the closing brace.
func exportList(toks []scriptToken, i int) ([][2]string, int) {
	var out [][2]string
	i++
	for i < len(toks) && toks[i].text != "}" {
		name := toks[i].text
		if !isScriptName(name) {
			i++
			continue
		}
		as := name
		if i+2 < len(toks) && toks[i+1].text == "as" && isScriptName(toks[i+2].text) {
			as = toks[i+2].text
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

// parseScriptFunction recognizes a function at toks[i]: a function
// declaration or expression, a const, let or var binding of one or of an
// arrow function, or, when anonymous is true (a default export), a bare arrow
// function. It returns the function, the index after it, and false when toks[i]
// starts no function with a body.
func parseScriptFunction(toks []scriptToken, i int, anonymous bool) (scriptFunction, int, bool) {
	at := func(k int) string {
		if k < 0 || k >= len(toks) {
			return ""
		}
		return toks[k].text
	}
	// async becomes part of the signature: it changes what the function
	// returns.
	asyncPrefix := func(fn scriptFunction, next int, ok bool) (scriptFunction, int, bool) {
		fn.signature = "async " + fn.signature
		return fn, next, ok
	}
	var fn scriptFunction
	switch at(i) {
	case "const", "let", "var":
		if !isScriptName(at(i + 1)) {
			return fn, 0, false
		}
		name := at(i + 1)
		k := i + 2
		if at(k) == ":" {
			k = scanUntil(toks, k+1, "=")
		}
		if at(k) != "=" {
			return fn, 0, false
		}
		k++
		async := at(k) == "async" && (at(k+1) == "function" || at(k+1) == "(" || at(k+1) == "<" || isScriptName(at(k+1)) && at(k+2) == "=>")
		if async {
			k++
		}
		var next int
		var ok bool
		if at(k) == "function" {
			fn, next, ok = parseDeclaration(toks, k)
		} else {
			fn, next, ok = parseArrow(toks, k, "")
		}
		fn.name = name
		if async {
			return asyncPrefix(fn, next, ok)
		}
		return fn, next, ok
	case "async":
		if at(i+1) == "function" {
			return asyncPrefix(parseDeclaration(toks, i+1))
		}
		if anonymous {
			return asyncPrefix(parseArrow(toks, i+1, ""))
		}
	case "function":
		return parseDeclaration(toks, i)
	case "(", "<":
		if anonymous {
			return parseArrow(toks, i, "")
		}
	}
	return fn, 0, false
}

// parseDeclaration parses "function [*] [name] [<...>] (...) [: type] {...}"
// starting at the function keyword.
func parseDeclaration(toks []scriptToken, i int) (scriptFunction, int, bool) {
	var fn scriptFunction
	k := i + 1
	generator := k < len(toks) && toks[k].text == "*"
	if generator {
		k++
	}
	if k < len(toks) && isScriptName(toks[k].text) {
		fn.name = toks[k].text
		k++
	}
	sigStart := k
	if k < len(toks) && toks[k].text == "<" {
		k = matchAngle(toks, k)
	}
	if k >= len(toks) || toks[k].text != "(" {
		return fn, 0, false
	}
	k = matchClose(toks, k)
	if k < len(toks) && toks[k].text == ":" {
		k = returnTypeEnd(toks, k+1)
	}
	if k >= len(toks) || toks[k].text != "{" {
		return fn, 0, false // an overload or a declaration without a body
	}
	fn.signature = joinTokens(toks[sigStart:k])
	if generator {
		fn.signature = "* " + fn.signature
	}
	end := matchClose(toks, k)
	fn.body = joinTokens(toks[k:end])
	return fn, end, true
}

// parseArrow parses "[<...>] (params) [: type] => body" or "name => body"
// starting at toks[i]. An expression body ends at a semicolon or comma at its
// own nesting level, or before a later line that starts a new top-level
// statement.
func parseArrow(toks []scriptToken, i int, name string) (scriptFunction, int, bool) {
	fn := scriptFunction{name: name}
	k := i
	sigStart := k
	switch {
	case k < len(toks) && (toks[k].text == "<" || toks[k].text == "("):
		if toks[k].text == "<" {
			k = matchAngle(toks, k)
		}
		if k >= len(toks) || toks[k].text != "(" {
			return fn, 0, false
		}
		k = matchClose(toks, k)
		if k < len(toks) && toks[k].text == ":" {
			k = scanUntil(toks, k+1, "=>")
		}
	case k < len(toks) && isScriptName(toks[k].text):
		k++
	default:
		return fn, 0, false
	}
	if k >= len(toks) || toks[k].text != "=>" {
		return fn, 0, false
	}
	fn.signature = joinTokens(toks[sigStart:k])
	k++
	if k < len(toks) && toks[k].text == "{" {
		end := matchClose(toks, k)
		fn.body = joinTokens(toks[k:end])
		return fn, end, true
	}
	start, depth := k, 0
	for ; k < len(toks); k++ {
		t := toks[k].text
		if depth == 0 && (t == ";" || t == ",") {
			break
		}
		if depth == 0 && k > start && toks[k].line > toks[k-1].line && statementStart[t] {
			break
		}
		switch t {
		case "{", "(", "[":
			depth++
		case "}", ")", "]":
			if depth == 0 {
				fn.body = joinTokens(toks[start:k])
				return fn, k, k > start
			}
			depth--
		}
	}
	fn.body = joinTokens(toks[start:k])
	return fn, k, k > start
}

// statementStart lists the tokens that start a new top-level statement after
// an expression-bodied arrow function without a semicolon.
var statementStart = map[string]bool{"export": true, "import": true, "const": true, "let": true, "var": true, "function": true, "class": true, "async": true, "interface": true, "type": true, "enum": true, "declare": true}

// matchClose returns the index after the bracket that closes toks[i].
func matchClose(toks []scriptToken, i int) int {
	depth := 0
	for k := i; k < len(toks); k++ {
		switch toks[k].text {
		case "{", "(", "[":
			depth++
		case "}", ")", "]":
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return len(toks)
}

// matchAngle returns the index after the ">" that closes the type parameter
// list opening at toks[i].
func matchAngle(toks []scriptToken, i int) int {
	depth := 0
	for k := i; k < len(toks); k++ {
		switch toks[k].text {
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
	return len(toks)
}

// returnTypeEnd returns the index of the "{" that opens the body after a
// return type annotation starting at toks[i]. An object type literal right
// after the colon is part of the type.
func returnTypeEnd(toks []scriptToken, i int) int {
	k := i
	if k < len(toks) && toks[k].text == "{" {
		k = matchClose(toks, k)
	}
	depth := 0
	for ; k < len(toks); k++ {
		switch toks[k].text {
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
	return len(toks)
}

// scanUntil returns the index of the first token equal to stop at the
// nesting level of toks[i], or len(toks).
func scanUntil(toks []scriptToken, i int, stop string) int {
	depth := 0
	for k := i; k < len(toks); k++ {
		t := toks[k].text
		if depth <= 0 && t == stop {
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
	return len(toks)
}

func joinTokens(toks []scriptToken) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}
