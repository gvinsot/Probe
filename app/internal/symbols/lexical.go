package symbols

// The lexical part of the index: TypeScript/JavaScript, Python and Rust.
//
// These languages have no type checker in the Go standard library, so their
// sources are scanned instead: a per-language tokenizer drops comments and
// keeps string literals as single tokens, a small parser finds the
// declarations (functions, methods, and tests), and every identifier in call
// position becomes a call site. Calls are then linked by name (lexresolve.go).
// Nothing is executed, imported or type-checked; every link is approximate
// and is reported with the "name" resolution.

import (
	"bytes"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
)

// Languages of the index.
const (
	LangGo         = "go"
	LangTypeScript = "typescript" // TypeScript and JavaScript sources
	LangPython     = "python"
	LangRust       = "rust"
)

// lexSkippedDirs are directories that hold dependencies, generated or built
// files rather than the repository's own source.
var lexSkippedDirs = map[string]bool{
	"node_modules": true, "bower_components": true, "dist": true, "build": true, "coverage": true,
	"target": true, "venv": true, "__pycache__": true, "site-packages": true, "vendor": true,
}

// lexLanguage returns the lexical language of a path, or "" when the path is
// not a TypeScript/JavaScript, Python or Rust source. Declaration files
// (.d.ts) and minified bundles hold no reviewable implementation.
func lexLanguage(p string) string {
	base := strings.ToLower(path.Base(p))
	switch path.Ext(base) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		if strings.Contains(base, ".d.") && strings.HasSuffix(strings.TrimSuffix(base, path.Ext(base)), ".d") || strings.Contains(base, ".min.") {
			return ""
		}
		return LangTypeScript
	case ".py":
		return LangPython
	case ".rs":
		return LangRust
	}
	return ""
}

// lexIndexable reports whether a tree path is a lexical source the index may
// read: a portable path outside dependency, build and hidden directories, and
// not sensitive.
func lexIndexable(p string, sensitive func(string) bool) bool {
	if lexLanguage(p) == "" || gitrepo.SafePath(p) != nil {
		return false
	}
	segments := strings.Split(p, "/")
	for _, s := range segments[:len(segments)-1] {
		if lexSkippedDirs[s] || strings.HasPrefix(s, ".") {
			return false
		}
	}
	return sensitive == nil || !sensitive(p)
}

// lexTestFile reports whether a path is a test file by the conventions of its
// language: *.test.* / *.spec.* and __tests__/ for TypeScript and JavaScript,
// test_*.py, *_test.py, conftest.py and tests/ for Python, and the tests/
// directory of a Rust crate. Rust unit tests inside src/ files are found by
// their #[test] attribute instead.
func lexTestFile(lang, p string) bool {
	lower := strings.ToLower(p)
	base := path.Base(lower)
	slashed := "/" + lower
	switch lang {
	case LangTypeScript:
		return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains(slashed, "/__tests__/")
	case LangPython:
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py" || strings.Contains(slashed, "/tests/") || strings.Contains(slashed, "/test/")
	case LangRust:
		return strings.Contains(slashed, "/tests/")
	}
	return false
}

// testFile reports whether an indexed path is a test file of its language.
func testFile(p string) bool {
	if strings.HasSuffix(p, ".go") {
		return strings.HasSuffix(p, "_test.go")
	}
	return lexTestFile(lexLanguage(p), p)
}

// langOf is the index language of a path.
func langOf(p string) string {
	if strings.HasSuffix(p, ".go") {
		return LangGo
	}
	return lexLanguage(p)
}

// langName is the display name of an index language.
func langName(lang string) string {
	switch lang {
	case LangGo:
		return "Go"
	case LangTypeScript:
		return "TypeScript/JavaScript"
	case LangPython:
		return "Python"
	case LangRust:
		return "Rust"
	}
	return lang
}

// --- tokens ------------------------------------------------------------------

// Token kinds.
const (
	tokIdent    = 'i'
	tokString   = 's' // string, template chunk, regular expression or char literal
	tokNumber   = 'n'
	tokLifetime = 'l' // Rust 'a
	tokPunct    = 'p'
)

// ltok is one token of a lexical source.
type ltok struct {
	text   string
	kind   byte
	line   int32
	col    int32 // one-based byte column
	first  bool  // Python: the first token of a logical line
	indent int32 // Python: the indentation (columns) of that logical line
}

// multiPuncts are the punctuators kept as one token. Everything else is a
// single byte, so ">>" closes two generic argument lists.
var multiPuncts = func() [][]byte {
	var out [][]byte
	for _, p := range []string{"===", "!==", "...", "=>", "::", "->", "?.", "==", "!=", "<=", ">=", "&&", "||", "??", "+=", "-=", "*=", "/=", "**"} {
		out = append(out, []byte(p))
	}
	return out
}()

// lexer is the state of one tokenization.
type lexer struct {
	src        []byte
	lang       string
	i          int
	line       int32
	lineStart  int
	toks       []ltok
	depth      int  // Python: bracket depth, which joins physical lines
	lineHasTok bool // Python: the current logical line has a token
}

// tokenize splits a source into tokens, dropping comments. Unterminated
// constructs end at the end of the line (strings) or of the file (comments).
func tokenize(lang string, src []byte) []ltok {
	l := &lexer{src: src, lang: lang, line: 1}
	var templates []int // TypeScript: brace depths at which a template resumes
	braces := 0
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == '\n':
			l.newline()
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			l.i++
		case c == '\\' && lang == LangPython && l.continuation():
		case c == '#' && lang == LangPython:
			l.skipLine()
		case c == '/' && l.peek(1) == '/' && lang != LangPython:
			l.skipLine()
		case c == '/' && l.peek(1) == '*' && lang != LangPython:
			l.blockComment()
		case c == '/' && lang == LangTypeScript && l.regexAllowed():
			l.regex()
		case c == '`' && lang == LangTypeScript:
			if l.template() {
				templates = append(templates, braces)
			}
		case c == '}' && lang == LangTypeScript && len(templates) > 0 && templates[len(templates)-1] == braces:
			// The end of a ${...} substitution: the template text resumes.
			templates = templates[:len(templates)-1]
			if l.template() {
				templates = append(templates, braces)
			}
		case c == '"' || c == '\'' && lang != LangRust:
			l.quoted(l.i, 0)
		case c == '\'' && lang == LangRust:
			l.charOrLifetime()
		case isIdentStart(c) || c >= utf8.RuneSelf:
			l.identifier()
		case c >= '0' && c <= '9':
			l.number()
		default:
			l.punct()
			if lang == LangTypeScript {
				switch c {
				case '{':
					braces++
				case '}':
					braces--
				}
			}
		}
	}
	return l.toks
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c >= utf8.RuneSelf
}

func (l *lexer) peek(n int) byte {
	if l.i+n < len(l.src) {
		return l.src[l.i+n]
	}
	return 0
}

func (l *lexer) newline() {
	l.i++
	l.line++
	l.lineStart = l.i
	if l.depth == 0 {
		l.lineHasTok = false
	}
}

// continuation skips a Python backslash-newline, which joins two physical
// lines into one logical line.
func (l *lexer) continuation() bool {
	j := l.i + 1
	if j < len(l.src) && l.src[j] == '\r' {
		j++
	}
	if j >= len(l.src) || l.src[j] != '\n' {
		l.i++
		return true
	}
	l.i = j + 1
	l.line++
	l.lineStart = l.i
	return true
}

func (l *lexer) skipLine() {
	for l.i < len(l.src) && l.src[l.i] != '\n' {
		l.i++
	}
}

// blockComment skips /* ... */; Rust block comments nest.
func (l *lexer) blockComment() {
	nesting := 0
	for l.i < len(l.src) {
		switch {
		case l.src[l.i] == '/' && l.peek(1) == '*':
			nesting++
			l.i += 2
		case l.src[l.i] == '*' && l.peek(1) == '/':
			nesting--
			l.i += 2
			if nesting == 0 || l.lang != LangRust {
				return
			}
		case l.src[l.i] == '\n':
			l.i++
			l.line++
			l.lineStart = l.i
		default:
			l.i++
		}
	}
}

// emit appends the token src[start:l.i], which began on line at column col.
func (l *lexer) emit(kind byte, start int, line int32, col int32) {
	t := ltok{text: string(l.src[start:l.i]), kind: kind, line: line, col: col}
	if l.lang == LangPython {
		if !l.lineHasTok {
			t.first, t.indent = true, col-1
			l.lineHasTok = true
		}
		if kind == tokPunct {
			switch t.text {
			case "(", "[", "{":
				l.depth++
			case ")", "]", "}":
				if l.depth > 0 {
					l.depth--
				}
			}
		}
	}
	l.toks = append(l.toks, t)
}

func (l *lexer) position(start int) (int32, int32) {
	return l.line, int32(start-l.lineStart) + 1
}

// quoted scans a string literal whose opening quote is at l.i, emitted from
// start (which includes any prefix). A Python triple quote spans lines; so
// does every Rust string. hashes is the number of '#' of a Rust raw string
// (-1 for a raw string without hashes), where backslashes do not escape.
func (l *lexer) quoted(start int, hashes int) {
	line, col := l.position(start)
	q := l.src[l.i]
	triple := l.lang == LangPython && l.peek(1) == q && l.peek(2) == q
	multiline := triple || l.lang == LangRust
	raw := hashes != 0
	if triple {
		l.i += 3
	} else {
		l.i++
	}
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch {
		case c == '\\' && !raw:
			if l.peek(1) == '\n' {
				l.line++
				l.lineStart = l.i + 2
			}
			l.i += 2
			continue
		case c == '\n':
			if !multiline {
				l.emit(tokString, start, line, col)
				return
			}
			l.i++
			l.line++
			l.lineStart = l.i
			continue
		case c == q && triple:
			if l.peek(1) == q && l.peek(2) == q {
				l.i += 3
				l.emit(tokString, start, line, col)
				return
			}
		case c == q:
			n := 0
			for n < hashes && l.peek(1+n) == '#' {
				n++
			}
			if n == hashes || hashes < 0 {
				l.i += 1 + n
				l.emit(tokString, start, line, col)
				return
			}
		}
		l.i++
	}
	if l.i > len(l.src) {
		l.i = len(l.src)
	}
	l.emit(tokString, start, line, col)
}

// charOrLifetime scans a Rust char literal ('a', '\n', '\u{1F600}') or a
// lifetime ('a, 'static).
func (l *lexer) charOrLifetime() {
	start := l.i
	line, col := l.position(start)
	if l.peek(1) == '\\' {
		l.i += 2
		for n := 0; l.i < len(l.src) && l.src[l.i] != '\'' && l.src[l.i] != '\n' && n < 12; n++ {
			l.i++
		}
		if l.i < len(l.src) && l.src[l.i] == '\'' {
			l.i++
		}
		l.emit(tokString, start, line, col)
		return
	}
	_, size := utf8.DecodeRune(l.src[l.i+1:])
	if size > 0 && l.i+1+size < len(l.src) && l.src[l.i+1+size] == '\'' {
		l.i += 2 + size
		l.emit(tokString, start, line, col)
		return
	}
	l.i++
	for l.i < len(l.src) && isIdentByte(l.src[l.i]) {
		l.i++
	}
	l.emit(tokLifetime, start, line, col)
}

// identifier scans an identifier, or a prefixed string literal (Python r"",
// b"", f""; Rust b"", r#""#, c"").
func (l *lexer) identifier() {
	start := l.i
	for l.i < len(l.src) && isIdentByte(l.src[l.i]) {
		if l.src[l.i] == '$' && l.lang != LangTypeScript {
			break
		}
		l.i++
	}
	if l.i == start {
		// A lone byte that starts no token.
		l.i++
		return
	}
	word := strings.ToLower(string(l.src[start:l.i]))
	if l.i < len(l.src) {
		next := l.src[l.i]
		switch l.lang {
		case LangPython:
			if (next == '"' || next == '\'') && pythonPrefix(word) {
				l.quoted(start, 0)
				return
			}
		case LangRust:
			switch {
			case next == '"' && (word == "b" || word == "c"):
				l.quoted(start, 0)
				return
			case (next == '"' || next == '#') && (word == "r" || word == "br" || word == "cr"):
				hashes := 0
				j := l.i
				for j < len(l.src) && l.src[j] == '#' {
					hashes++
					j++
				}
				if j < len(l.src) && l.src[j] == '"' {
					l.i = j
					if hashes == 0 {
						hashes = -1
					}
					l.quoted(start, hashes)
					return
				}
			case next == '\'' && word == "b":
				l.charOrLifetime()
				// The prefix and the literal form one token.
				last := &l.toks[len(l.toks)-1]
				last.text = "b" + last.text
				last.col--
				return
			}
		}
	}
	line, col := l.position(start)
	l.emit(tokIdent, start, line, col)
}

func pythonPrefix(word string) bool {
	switch word {
	case "r", "u", "b", "f", "rb", "br", "fr", "rf":
		return true
	}
	return false
}

func (l *lexer) number() {
	start := l.i
	for l.i < len(l.src) && (isIdentByte(l.src[l.i]) || l.src[l.i] == '.' && l.peek(1) >= '0' && l.peek(1) <= '9') {
		l.i++
	}
	line, col := l.position(start)
	l.emit(tokNumber, start, line, col)
}

func (l *lexer) punct() {
	start := l.i
	line, col := l.position(start)
	for _, p := range multiPuncts {
		if bytes.HasPrefix(l.src[l.i:], p) {
			l.i += len(p)
			l.emit(tokPunct, start, line, col)
			return
		}
	}
	l.i++
	l.emit(tokPunct, start, line, col)
}

// regexAllowed reports whether a '/' starts a regular expression literal
// rather than a division, from the previous token.
func (l *lexer) regexAllowed() bool {
	if len(l.toks) == 0 {
		return true
	}
	prev := l.toks[len(l.toks)-1]
	switch prev.kind {
	case tokPunct:
		return prev.text != ")" && prev.text != "]" && prev.text != "}"
	case tokIdent:
		switch prev.text {
		case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "throw", "case", "do", "else", "yield", "await":
			return true
		}
	}
	return false
}

// regex scans a regular expression literal; one that does not end on its
// line is taken back as a division operator.
func (l *lexer) regex() {
	start := l.i
	line, col := l.position(start)
	j := l.i + 1
	inClass := false
	for j < len(l.src) {
		c := l.src[j]
		switch {
		case c == '\\':
			j += 2
			continue
		case c == '\n':
			l.punct()
			return
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			j++
			for j < len(l.src) && isIdentByte(l.src[j]) {
				j++
			}
			l.i = j
			l.emit(tokString, start, line, col)
			return
		}
		j++
	}
	l.punct()
}

// template scans TypeScript template text from the backtick or the closing
// brace of a substitution at l.i, and reports whether it stopped at a ${
// (the template resumes at the matching brace).
func (l *lexer) template() bool {
	start := l.i
	line, col := l.position(start)
	l.i++
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch {
		case c == '\\':
			l.i += 2
			continue
		case c == '\n':
			l.i++
			l.line++
			l.lineStart = l.i
			continue
		case c == '`':
			l.i++
			l.emit(tokString, start, line, col)
			return false
		case c == '$' && l.peek(1) == '{':
			l.i += 2
			l.emit(tokString, start, line, col)
			return true
		}
		l.i++
	}
	l.i = len(l.src)
	l.emit(tokString, start, line, col)
	return false
}

// --- parsing -----------------------------------------------------------------

// lexDecl is one declaration found in a lexical source. Token indexes are
// inclusive.
type lexDecl struct {
	name      string // "f", "C.m", or a test's title
	kind      string // KindFunc or KindMethod
	test      bool   // a test function or test block
	testCode  bool   // test code: a test, or a helper inside a Rust #[cfg(test)] module
	start     int    // first token of the signature
	bodyStart int    // first token of the body
	end       int    // last token of the body
}

// lexCall is one identifier in call position.
type lexCall struct {
	name   string
	qual   string // the identifier before "." or "::", if any
	member bool   // x.name(...)
	path   bool   // Q::name(...) (Rust)
	ctor   bool   // new Name(...) (TypeScript)
	tok    int
}

// lexFile is one parsed lexical source.
type lexFile struct {
	path, lang string
	toks       []ltok
	decls      []lexDecl
	calls      []lexCall
}

// parseLexical tokenizes and parses one source.
func parseLexical(p string, src []byte) *lexFile {
	lang := lexLanguage(p)
	f := &lexFile{path: p, lang: lang, toks: tokenize(lang, src)}
	m := matchBrackets(f.toks)
	names := map[int]bool{} // tokens naming a declaration, never calls
	skip := map[int]bool{}  // tokens that are not code (type-only bodies)
	switch lang {
	case LangTypeScript:
		parseTypeScript(f, m, names, skip)
	case LangPython:
		parsePython(f, m, names)
	case LangRust:
		parseRust(f, m, names)
	}
	f.calls = findCalls(f, m, names, skip)
	return f
}

// matchBrackets maps every opening bracket to its closing bracket and back;
// unmatched brackets map to -1.
func matchBrackets(t []ltok) []int {
	m := make([]int, len(t))
	var stack []int
	for i := range t {
		m[i] = -1
		if t[i].kind != tokPunct {
			continue
		}
		switch t[i].text {
		case "(", "[", "{":
			stack = append(stack, i)
		case ")", "]", "}":
			open := map[string]string{")": "(", "]": "[", "}": "{"}[t[i].text]
			// A stray closer is ignored; a missing closer is skipped over.
			for k := len(stack) - 1; k >= 0 && k >= len(stack)-3; k-- {
				if t[stack[k]].text == open {
					m[stack[k]], m[i] = i, stack[k]
					stack = stack[:k]
					break
				}
			}
		}
	}
	return m
}

func is(t []ltok, i int, text string) bool {
	return i >= 0 && i < len(t) && t[i].text == text && t[i].kind != tokString
}

func ident(t []ltok, i int) bool { return i >= 0 && i < len(t) && t[i].kind == tokIdent }

// skipAngles returns the token after the generic argument list that opens at
// i ("<"), or i when it does not close within a bounded span.
func skipAngles(t []ltok, m []int, i int) int {
	depth := 0
	for j := i; j < len(t) && j < i+400; j++ {
		switch {
		case is(t, j, "<"):
			depth++
		case is(t, j, ">"):
			depth--
			if depth == 0 {
				return j + 1
			}
		case is(t, j, "=>"), is(t, j, "->"):
		case is(t, j, "(") || is(t, j, "[") || is(t, j, "{"):
			if m[j] < 0 {
				return i
			}
			j = m[j]
		case is(t, j, ";"):
			return i
		}
	}
	return i
}

// --- TypeScript / JavaScript -------------------------------------------------

var tsModifiers = map[string]bool{"public": true, "private": true, "protected": true, "static": true, "async": true, "readonly": true, "abstract": true, "override": true, "declare": true, "get": true, "set": true, "accessor": true, "export": true, "default": true}

// tsStatementStart are the words that start a new statement on a new line,
// ending an arrow function's expression body.
var tsStatementStart = map[string]bool{"const": true, "let": true, "var": true, "function": true, "class": true, "export": true, "import": true, "return": true, "if": true, "for": true, "while": true, "switch": true, "try": true, "throw": true, "interface": true, "type": true, "enum": true}

func parseTypeScript(f *lexFile, m []int, names, skip map[int]bool) {
	t := f.toks
	test := lexTestFile(f.lang, f.path)
	parent := make([]int, len(t)) // innermost enclosing '{'
	var stack []int
	for i := range t {
		for len(stack) > 0 && m[stack[len(stack)-1]] >= 0 && i > m[stack[len(stack)-1]] {
			stack = stack[:len(stack)-1]
		}
		parent[i] = -1
		if len(stack) > 0 {
			parent[i] = stack[len(stack)-1]
		}
		if is(t, i, "{") && m[i] > i {
			stack = append(stack, i)
		}
	}
	classes := map[int]string{} // '{' of a class body -> class name
	fnEnd := -1                 // tokens up to fnEnd are inside a recorded function body
	type describe struct {
		end   int
		title string
	}
	var describes []describe
	record := func(name, kind string, nameTok, start, body, end int) {
		names[nameTok] = true
		f.decls = append(f.decls, lexDecl{name: name, kind: kind, start: start, bodyStart: body, end: end})
		if end > fnEnd {
			fnEnd = end
		}
	}
	// arrow records "name = [async] (params) [: T] => body" whose value starts at k.
	arrow := func(name, kind string, nameTok, start, k int) bool {
		if is(t, k, "async") {
			k++
		}
		if is(t, k, "function") {
			k++
			if is(t, k, "*") {
				k++
			}
			if ident(t, k) {
				names[k] = true
				k++
			}
			if is(t, k, "<") {
				k = skipAngles(t, m, k)
			}
			if !is(t, k, "(") || m[k] < 0 {
				return false
			}
			body := tsBody(t, m, m[k]+1)
			if body < 0 {
				return false
			}
			record(name, kind, nameTok, start, body, m[body])
			return true
		}
		if is(t, k, "<") {
			k = skipAngles(t, m, k)
		}
		switch {
		case is(t, k, "(") && m[k] > 0:
			k = m[k] + 1
		case ident(t, k):
			k++
		default:
			return false
		}
		if is(t, k, ":") {
			for k < len(t) && !is(t, k, "=>") && !is(t, k, ";") && !is(t, k, "=") {
				if (is(t, k, "(") || is(t, k, "[") || is(t, k, "{")) && m[k] > k {
					k = m[k]
				}
				k++
			}
		}
		if !is(t, k, "=>") {
			return false
		}
		k++
		if is(t, k, "{") && m[k] > k {
			record(name, kind, nameTok, start, k, m[k])
			return true
		}
		record(name, kind, nameTok, start, k, tsExpressionEnd(t, m, k))
		return true
	}
	for i := 0; i < len(t); i++ {
		for len(describes) > 0 && i > describes[len(describes)-1].end {
			describes = describes[:len(describes)-1]
		}
		if t[i].kind != tokIdent {
			continue
		}
		inFn := i <= fnEnd
		// A member directly in a class body.
		if cls, ok := classes[parent[i]]; ok && !inFn && tsMemberStart(t, parent[i], i) {
			j := i
			for tsModifiers[t[j].text] && (ident(t, j+1) || is(t, j+1, "*") || is(t, j+1, "#") || t[j+1].kind == tokString) {
				j++
			}
			if is(t, j, "*") || is(t, j, "#") {
				j++
			}
			if !ident(t, j) && (j >= len(t) || t[j].kind != tokString) {
				continue
			}
			name := strings.Trim(t[j].text, "'\"")
			k := j + 1
			if is(t, k, "?") || is(t, k, "!") {
				k++
			}
			if is(t, k, "<") {
				k = skipAngles(t, m, k)
			}
			switch {
			case is(t, k, "(") && m[k] > k:
				names[j] = true
				if body := tsBody(t, m, m[k]+1); body >= 0 {
					record(cls+"."+name, KindMethod, j, i, body, m[body])
				}
				i = max(i, m[k])
			case is(t, k, "="):
				arrow(cls+"."+name, KindMethod, j, i, k+1)
			}
			continue
		}
		switch t[i].text {
		case "function":
			if inFn {
				continue
			}
			j := i + 1
			if is(t, j, "*") {
				j++
			}
			if !ident(t, j) {
				continue
			}
			names[j] = true
			k := j + 1
			if is(t, k, "<") {
				k = skipAngles(t, m, k)
			}
			if !is(t, k, "(") || m[k] < 0 {
				continue
			}
			if body := tsBody(t, m, m[k]+1); body >= 0 {
				record(t[j].text, KindFunc, j, i, body, m[body])
			}
		case "class":
			if !ident(t, i+1) {
				continue
			}
			names[i+1] = true
			for k := i + 2; k < len(t) && k < i+400; k++ {
				if is(t, k, "<") {
					k = skipAngles(t, m, k) - 1
					continue
				}
				if is(t, k, "(") && m[k] > k {
					k = m[k]
					continue
				}
				if is(t, k, "{") {
					if m[k] > k && !inFn {
						classes[k] = t[i+1].text
					}
					break
				}
				if is(t, k, ";") {
					break
				}
			}
		case "interface", "enum":
			// Type-only bodies: their "name(...)" members are not calls.
			for k := i + 1; k < len(t) && k < i+400; k++ {
				if is(t, k, "{") {
					if m[k] > k {
						for s := k; s <= m[k]; s++ {
							skip[s] = true
						}
					}
					break
				}
			}
		case "const", "let", "var":
			if inFn || !ident(t, i+1) {
				continue
			}
			j := i + 1
			k := j + 1
			if is(t, k, ":") {
				for k < len(t) && !is(t, k, "=") && !is(t, k, ";") {
					if (is(t, k, "(") || is(t, k, "[") || is(t, k, "{")) && m[k] > k {
						k = m[k]
					}
					k++
				}
			}
			if is(t, k, "=") {
				if arrow(t[j].text, KindFunc, j, i, k+1) {
					continue
				}
			}
		case "describe", "it", "test":
			if !test || is(t, i-1, ".") || !is(t, i+1, "(") || m[i+1] < 0 || i+2 >= len(t) || t[i+2].kind != tokString {
				continue
			}
			title := tsTitle(t[i+2].text)
			if t[i].text == "describe" {
				describes = append(describes, describe{end: m[i+1], title: title})
				continue
			}
			var parts []string
			for _, d := range describes {
				parts = append(parts, d.title)
			}
			parts = append(parts, title)
			f.decls = append(f.decls, lexDecl{name: strings.Join(parts, " > "), kind: KindFunc, test: true, testCode: true, start: i, bodyStart: i + 2, end: m[i+1]})
		}
	}
}

// tsMemberStart reports whether token i can start a class member: it follows
// the class body's brace, a semicolon, a closing brace, or ends a previous
// line.
func tsMemberStart(t []ltok, brace, i int) bool {
	if i == brace+1 {
		return true
	}
	prev := t[i-1]
	if prev.kind == tokPunct && (prev.text == ";" || prev.text == "}") {
		return true
	}
	if tsModifiers[prev.text] && prev.kind == tokIdent {
		return false
	}
	return prev.line < t[i].line && (prev.kind != tokPunct || prev.text == ")" || prev.text == "]" || prev.text == ">")
}

// tsBody returns the opening brace of a function body that follows the
// parameter list ending before k, skipping a return type annotation, or -1
// when there is no body (an overload or abstract signature).
func tsBody(t []ltok, m []int, k int) int {
	if is(t, k, "{") {
		if m[k] > k {
			return k
		}
		return -1
	}
	if !is(t, k, ":") {
		return -1
	}
	k++
	angles := 0
	for n := 0; k < len(t) && n < 400; n++ {
		switch {
		case is(t, k, "<"):
			angles++
		case is(t, k, ">"):
			angles--
		case is(t, k, "{"):
			if m[k] < 0 {
				return -1
			}
			prev := t[k-1].text
			if angles > 0 || prev == ":" || prev == "|" || prev == "&" || prev == "," || prev == "<" || prev == "=>" || prev == "(" || prev == "[" || prev == "keyof" || prev == "typeof" {
				k = m[k] + 1
				continue
			}
			return k
		case is(t, k, "(") || is(t, k, "["):
			if m[k] < 0 {
				return -1
			}
			k = m[k] + 1
			continue
		case is(t, k, ";") || is(t, k, "}") || is(t, k, "="):
			return -1
		}
		k++
	}
	return -1
}

// tsExpressionEnd returns the last token of an arrow function's expression
// body starting at k.
func tsExpressionEnd(t []ltok, m []int, k int) int {
	end := k
	for j := k; j < len(t); j++ {
		if t[j].kind == tokPunct {
			switch t[j].text {
			case "(", "[", "{":
				if m[j] < 0 {
					return end
				}
				j = m[j]
				end = j
				continue
			case ";", ",", ")", "]", "}":
				return end
			}
		}
		if j > k && t[j].line > t[j-1].line && t[j].kind == tokIdent && tsStatementStart[t[j].text] {
			return end
		}
		end = j
	}
	return end
}

// tsTitle is the text of a test title literal, cut to 200 bytes.
func tsTitle(lit string) string {
	s := strings.Trim(lit, "'\"`${")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		s = "(untitled)"
	}
	return s
}

// --- Python ------------------------------------------------------------------

func parsePython(f *lexFile, m []int, names map[int]bool) {
	t := f.toks
	test := lexTestFile(f.lang, f.path)
	type scope struct {
		indent int32
		class  bool // a class body; otherwise a function body
		name   string
		test   bool // a test class
		decl   int  // index in f.decls, or -1
	}
	var stack []scope
	closeTo := func(indent int32, at int) {
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			if d := stack[len(stack)-1].decl; d >= 0 {
				f.decls[d].end = max(f.decls[d].bodyStart, at-1)
			}
			stack = stack[:len(stack)-1]
		}
	}
	inFunction := func() bool {
		for _, s := range stack {
			if !s.class {
				return true
			}
		}
		return false
	}
	for i := range t {
		if !t[i].first {
			continue
		}
		closeTo(t[i].indent, i)
		j := i
		if is(t, j, "async") {
			j++
		}
		switch {
		case is(t, j, "def") && ident(t, j+1):
			names[j+1] = true
			if inFunction() {
				stack = append(stack, scope{indent: t[i].indent, decl: -1})
				continue
			}
			name, kind := t[j+1].text, KindFunc
			var classNames []string
			testClass := false
			for _, s := range stack {
				classNames = append(classNames, s.name)
				testClass = s.test
			}
			if len(classNames) > 0 {
				name, kind = strings.Join(classNames, ".")+"."+name, KindMethod
			}
			k := j + 2
			if !is(t, k, "(") || m[k] < 0 {
				stack = append(stack, scope{indent: t[i].indent, decl: -1})
				continue
			}
			k = m[k] + 1
			for k < len(t) && !is(t, k, ":") && !t[k].first {
				if (is(t, k, "(") || is(t, k, "[") || is(t, k, "{")) && m[k] > k {
					k = m[k]
				}
				k++
			}
			isTest := test && strings.HasPrefix(t[j+1].text, "test") && (len(classNames) == 0 || testClass)
			f.decls = append(f.decls, lexDecl{name: name, kind: kind, test: isTest, testCode: isTest, start: i, bodyStart: min(k+1, len(t)-1), end: len(t) - 1})
			stack = append(stack, scope{indent: t[i].indent, decl: len(f.decls) - 1})
		case is(t, j, "class") && ident(t, j+1):
			names[j+1] = true
			if inFunction() {
				stack = append(stack, scope{indent: t[i].indent, decl: -1})
				continue
			}
			name := t[j+1].text
			testClass := strings.HasPrefix(name, "Test")
			if is(t, j+2, "(") && m[j+2] > j+2 {
				for k := j + 3; k < m[j+2]; k++ {
					if t[k].text == "TestCase" || strings.HasSuffix(t[k].text, "TestCase") {
						testClass = true
					}
				}
			}
			stack = append(stack, scope{indent: t[i].indent, class: true, name: name, test: testClass, decl: -1})
		}
	}
	closeTo(-1, len(t))
}

// --- Rust --------------------------------------------------------------------

func parseRust(f *lexFile, m []int, names map[int]bool) {
	t := f.toks
	type scope struct {
		end     int
		kind    byte // 'i' impl or trait block, 'm' module, 'f' function body
		name    string
		testMod bool
	}
	var stack []scope
	inTestMod := func() bool {
		for _, s := range stack {
			if s.testMod {
				return true
			}
		}
		return false
	}
	// braceAfter returns the '{' that opens the block of an item header
	// starting at k, or -1 at a ';' first.
	braceAfter := func(k int) int {
		for n := 0; k < len(t) && n < 400; n++ {
			switch {
			case is(t, k, "{"):
				if m[k] > k {
					return k
				}
				return -1
			case is(t, k, ";"):
				return -1
			case (is(t, k, "(") || is(t, k, "[")) && m[k] > k:
				k = m[k]
			}
			k++
		}
		return -1
	}
	for i := 0; i < len(t); i++ {
		for len(stack) > 0 && i > stack[len(stack)-1].end {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 && stack[len(stack)-1].kind == 'f' {
			i = stack[len(stack)-1].end
			continue
		}
		if t[i].kind != tokIdent {
			continue
		}
		switch t[i].text {
		case "impl":
			k := i + 1
			if is(t, k, "<") {
				k = skipAngles(t, m, k)
			}
			brace := braceAfter(k)
			if brace < 0 {
				continue
			}
			segment := k
			angles := 0
			for s := k; s < brace; s++ {
				switch {
				case is(t, s, "<"):
					angles++
				case is(t, s, ">"):
					angles--
				case angles == 0 && is(t, s, "for"):
					segment = s + 1
				}
			}
			name := ""
			angles = 0
			for s := segment; s < brace && !is(t, s, "where"); s++ {
				switch {
				case is(t, s, "<"):
					angles++
				case is(t, s, ">"):
					angles--
				case angles == 0 && ident(t, s) && t[s].text != "dyn" && t[s].text != "mut" && t[s].text != "crate" && t[s].text != "super" && t[s].text != "self":
					name = t[s].text
				}
			}
			stack = append(stack, scope{end: m[brace], kind: 'i', name: name})
			i = brace
		case "trait":
			if !ident(t, i+1) {
				continue
			}
			names[i+1] = true
			if brace := braceAfter(i + 2); brace >= 0 {
				stack = append(stack, scope{end: m[brace], kind: 'i', name: t[i+1].text})
				i = brace
			}
		case "mod":
			if ident(t, i+1) && is(t, i+2, "{") && m[i+2] > i+2 {
				testMod := false
				for _, attr := range rustAttributes(t, m, i) {
					if strings.Join(attr, "") == "cfg(test)" {
						testMod = true
					}
				}
				stack = append(stack, scope{end: m[i+2], kind: 'm', testMod: testMod})
				i = i + 2
			}
		case "fn":
			if !ident(t, i+1) {
				continue
			}
			names[i+1] = true
			k := i + 2
			if is(t, k, "<") {
				k = skipAngles(t, m, k)
			}
			if !is(t, k, "(") || m[k] < 0 {
				continue
			}
			body := braceAfter(m[k] + 1)
			if body < 0 {
				continue
			}
			name, kind := t[i+1].text, KindFunc
			if len(stack) > 0 && stack[len(stack)-1].kind == 'i' && stack[len(stack)-1].name != "" {
				name, kind = stack[len(stack)-1].name+"."+name, KindMethod
			}
			isTest := false
			for _, attr := range rustAttributes(t, m, i) {
				if rustTestAttribute(attr) {
					isTest = true
				}
			}
			f.decls = append(f.decls, lexDecl{name: name, kind: kind, test: isTest, testCode: isTest || inTestMod() || lexTestFile(f.lang, f.path), start: i, bodyStart: body, end: m[body]})
			stack = append(stack, scope{end: m[body], kind: 'f'})
			i = body
		}
	}
}

// rustModifiers may stand between an item's attributes and its keyword.
var rustModifiers = map[string]bool{"pub": true, "async": true, "const": true, "unsafe": true, "extern": true, "default": true, "crate": true, "super": true, "in": true, "self": true}

// rustAttributes returns the token texts inside each outer attribute #[...]
// of the item whose keyword is at i.
func rustAttributes(t []ltok, m []int, i int) [][]string {
	var out [][]string
	k := i - 1
	for k >= 0 {
		switch {
		case ident(t, k) && rustModifiers[t[k].text], t[k].kind == tokString:
			k--
		case is(t, k, ")") && m[k] >= 0 && is(t, m[k]-1, "pub"):
			k = m[k] - 1
		case is(t, k, "]") && m[k] >= 1 && is(t, m[k]-1, "#"):
			var attr []string
			for s := m[k] + 1; s < k; s++ {
				attr = append(attr, t[s].text)
			}
			out = append(out, attr)
			k = m[k] - 2
		default:
			return out
		}
	}
	return out
}

// rustTestAttribute reports a test attribute: #[test], #[tokio::test],
// #[rstest], #[test_case(...)] and similar.
func rustTestAttribute(attr []string) bool {
	last := ""
	for _, s := range attr {
		if s == "(" || s == "=" {
			break
		}
		if s != "::" {
			last = s
		}
	}
	switch last {
	case "test", "rstest", "test_case", "quickcheck", "proptest":
		return true
	}
	return false
}

// --- calls -------------------------------------------------------------------

// lexKeywords are words that precede "(" without being calls.
var lexKeywords = map[string]map[string]bool{
	LangTypeScript: set("if", "for", "while", "switch", "catch", "function", "return", "typeof", "new", "delete", "void", "in", "of", "instanceof", "await", "yield", "super", "import", "with", "do", "else", "case", "throw", "class", "extends", "constructor", "require", "async", "get", "set"),
	LangPython:     set("if", "elif", "while", "for", "return", "not", "and", "or", "in", "is", "lambda", "def", "class", "assert", "del", "except", "raise", "with", "yield", "await", "print", "super", "global", "nonlocal", "from", "import", "as", "else", "match", "case"),
	LangRust:       set("if", "while", "for", "loop", "match", "return", "as", "in", "let", "mut", "ref", "move", "fn", "struct", "enum", "impl", "trait", "where", "use", "mod", "pub", "crate", "super", "self", "Self", "type", "unsafe", "async", "await", "dyn", "else", "break", "continue", "const", "static", "extern", "Some", "Ok", "Err", "Box", "Vec", "String", "Fn", "FnMut", "FnOnce"),
}

func set(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// findCalls lists the identifiers in call position that are not
// declarations, keywords or macro invocations.
func findCalls(f *lexFile, m []int, names, skip map[int]bool) []lexCall {
	t := f.toks
	keywords := lexKeywords[f.lang]
	if f.lang == LangRust {
		// Attributes (#[cfg(test)], #[tokio::test(...)]) hold no calls.
		for i := 0; i+1 < len(t); i++ {
			open := i + 1
			if is(t, open, "!") {
				open++
			}
			if is(t, i, "#") && is(t, open, "[") && m[open] > open {
				for k := open; k <= m[open]; k++ {
					skip[k] = true
				}
			}
		}
	}
	var out []lexCall
	for i := 0; i+1 < len(t); i++ {
		if t[i].kind != tokIdent || names[i] || skip[i] || keywords[t[i].text] {
			continue
		}
		open := i + 1
		if f.lang == LangRust && is(t, open, "::") && is(t, open+1, "<") {
			open = skipAngles(t, m, open+1)
		}
		if !is(t, open, "(") {
			continue
		}
		if f.lang == LangTypeScript && m[open] > open && is(t, m[open]+1, "{") {
			// name(...) { ... }: an object literal method or a signature, not a call.
			continue
		}
		c := lexCall{name: t[i].text, tok: i}
		switch {
		case is(t, i-1, ".") || is(t, i-1, "?."):
			c.member = true
			if ident(t, i-2) {
				c.qual = t[i-2].text
			}
		case is(t, i-1, "::"):
			c.path = true
			if ident(t, i-2) {
				c.qual = t[i-2].text
			}
		case is(t, i-1, "new"):
			c.ctor = true
		case is(t, i-1, "function") || is(t, i-1, "fn") || is(t, i-1, "def"):
			continue
		}
		out = append(out, c)
	}
	return out
}
