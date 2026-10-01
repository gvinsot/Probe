package symbols

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"path"
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
