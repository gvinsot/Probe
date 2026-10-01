package symbols

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"path"
	"sort"
	"strings"
)

// PythonTestNameSeparator joins the classes and the function of a pytest
// test name, as pytest's node IDs do: "TestCart::test_total".
const PythonTestNameSeparator = "::"

// IsPythonTestPath reports whether p is a Python test module by pytest's
// default python_files patterns, test_*.py and *_test.py, and indexable
// (outside hidden directories, vendored and generated trees). conftest.py
// and helpers under tests/ hold no tests of their own.
func IsPythonTestPath(p string) bool {
	if !lexIndexable(p, nil) || lexLanguage(p) != LangPython {
		return false
	}
	base := strings.ToLower(path.Base(p))
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
}

// PythonTestName is the pytest name of an indexed Python test declaration:
// "test_total" stays, "TestCart.test_total" becomes "TestCart::test_total".
// Python identifiers contain neither "." nor ":", so the mapping is exact.
func PythonTestName(indexName string) string {
	return strings.ReplaceAll(indexName, ".", PythonTestNameSeparator)
}

// ReadPythonTests reads the test functions of a Python test module
// statically, as pytest would collect them by default: functions named
// test* at module level and in test classes (Test* classes and
// unittest.TestCase subclasses), named as pytest names them
// (PythonTestName). It never executes repository code, and returns false
// when p is not a Python test module (IsPythonTestPath).
//
// A test's digest covers its decorator lines and its tokens, each logical
// line with its indentation relative to the def, so a comment or layout edit
// leaves it unchanged while moving a statement into or out of a block does
// not. Shared covers every other token with its indentation: imports,
// fixtures, helpers, class headers and class-level code, so any edit there
// selects every test of the module.
func ReadPythonTests(p string, src []byte) (ScriptTestFile, bool) {
	if !IsPythonTestPath(p) {
		return ScriptTestFile{}, false
	}
	f := parseLexical(p, src)
	inside := make([]bool, len(f.toks))
	var out ScriptTestFile
	for _, d := range f.decls {
		if !d.test || d.start < 0 || d.end >= len(f.toks) || d.start > d.end {
			continue
		}
		start := pythonDecorators(f.toks, d.start)
		base := f.toks[d.start].indent
		h := sha256.New()
		for i := start; i <= d.end; i++ {
			digestPythonToken(h, f.toks[i], base)
			inside[i] = true
		}
		out.Tests = append(out.Tests, ScriptTest{Name: PythonTestName(d.name), Line: int(f.toks[start].line), EndLine: int(f.toks[d.end].line), Digest: hex.EncodeToString(h.Sum(nil))})
	}
	h := sha256.New()
	for i, t := range f.toks {
		if !inside[i] {
			digestPythonToken(h, t, 0)
		}
	}
	out.Shared = hex.EncodeToString(h.Sum(nil))
	return out, true
}

// pythonDecorators returns the first token of the decorator lines directly
// above the def (or async def) whose first token is at start: logical lines
// at the same indentation that begin with "@".
func pythonDecorators(toks []ltok, start int) int {
	indent := toks[start].indent
	first := start
	for {
		prev := first - 1
		for prev >= 0 && !toks[prev].first {
			prev--
		}
		if prev < 0 || toks[prev].text != "@" || toks[prev].indent != indent {
			return first
		}
		first = prev
	}
}

// digestPythonToken is digestToken with the indentation of a logical line's
// first token, relative to base, since indentation is Python syntax.
func digestPythonToken(h hash.Hash, t ltok, base int32) {
	if t.first {
		var head [5]byte
		head[0] = '\n'
		binary.BigEndian.PutUint32(head[1:], uint32(t.indent-base))
		h.Write(head[:])
	}
	digestToken(h, t)
}

// PythonIdentifiers returns the identifiers a Python source names outside
// string literals and comments, sorted and without duplicates. Keywords are
// included; nothing is resolved.
func PythonIdentifiers(src []byte) []string {
	f := parseLexical("probe.py", src)
	seen := map[string]bool{}
	for _, t := range f.toks {
		if t.kind == tokIdent {
			seen[t.text] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PythonChangedDeclarations returns the top-level declarations of a Python
// module that contain one of the added lines (1-based): functions and
// classes defined at column 0, with their decorator lines, and module-level
// names assigned at column 0 (NAME = ..., NAME: type = ...). A declaration
// spans from its first line to the line before the next top-level statement.
// The result is sorted and without duplicates.
func PythonChangedDeclarations(p string, src []byte, added []int) []string {
	if len(added) == 0 {
		return nil
	}
	f := parseLexical(p, src)
	t := f.toks
	var starts []int // first tokens of top-level logical lines
	for i := range t {
		if t[i].first && t[i].indent == 0 {
			starts = append(starts, i)
		}
	}
	names := map[string]bool{}
	for k := 0; k < len(starts); k++ {
		first := starts[k]
		// Decorator lines belong to the definition that follows them.
		j := k
		for j < len(starts) && t[starts[j]].text == "@" {
			j++
		}
		if j == len(starts) {
			break
		}
		i := starts[j]
		lastTok := len(t) - 1
		if j+1 < len(starts) {
			lastTok = starts[j+1] - 1
		}
		name := ""
		switch {
		case is(t, i, "async") && is(t, i+1, "def") && ident(t, i+2):
			name = t[i+2].text
		case (is(t, i, "def") || is(t, i, "class")) && ident(t, i+1):
			name = t[i+1].text
		case ident(t, i) && i+1 <= lastTok && (is(t, i+1, "=") || is(t, i+1, ":")):
			name = t[i].text
		}
		if name != "" && pythonKeyword[name] {
			name = ""
		}
		if name != "" {
			from, to := int(t[first].line), int(t[lastTok].line)
			for _, line := range added {
				if line >= from && line <= to {
					names[name] = true
					break
				}
			}
		}
		k = j
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// pythonKeyword lists the soft and hard keywords that can start a top-level
// statement followed by ":" or "=", which are never declarations.
var pythonKeyword = map[string]bool{"if": true, "elif": true, "else": true, "for": true, "while": true, "try": true, "except": true, "finally": true, "with": true, "match": true, "case": true, "lambda": true}

// PythonToken is one token of a Python source. Comments are not tokens.
// Kind is 'i' (identifier or keyword), 's' (string), 'n' (number) or 'p'
// (punctuation); First marks the first token of a logical line and Indent is
// that line's indentation.
type PythonToken struct {
	Text      string
	Kind      byte
	Line, Col int // Col is the 1-based byte column
	// Match is the index of the matching bracket of ( [ { ) ] }, or -1.
	Match  int
	First  bool
	Indent int
}

// PythonSource is the lexical reading of one Python source file: its tokens
// and the bodies of the module-level functions and methods the index
// records (test functions excluded); nested functions belong to their
// enclosing function.
type PythonSource struct {
	Tokens    []PythonToken
	Functions []ScriptFunction
}

// ReadPythonSource tokenizes and reads a Python source statically, the way
// the static index does. It returns false when p is not a Python source.
func ReadPythonSource(p string, src []byte) (PythonSource, bool) {
	if lexLanguage(p) != LangPython {
		return PythonSource{}, false
	}
	f := parseLexical(p, src)
	m := matchBrackets(f.toks)
	out := PythonSource{Tokens: make([]PythonToken, len(f.toks))}
	for i, t := range f.toks {
		out.Tokens[i] = PythonToken{Text: t.text, Kind: t.kind, Line: int(t.line), Col: int(t.col), Match: m[i], First: t.first, Indent: int(t.indent)}
	}
	for _, d := range f.decls {
		if d.test || d.bodyStart < 0 || d.end >= len(f.toks) || d.bodyStart > d.end {
			continue
		}
		out.Functions = append(out.Functions, ScriptFunction{Name: d.name, Body: d.bodyStart, End: d.end})
	}
	return out, true
}
