package symbols

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// ScriptTest is one test() or it() call of a TypeScript or JavaScript test
// file, as the lexical index reads it.
type ScriptTest struct {
	// Name is the test's describe titles and title joined by " > ", each
	// normalized as the index normalizes a string literal.
	Name          string
	Line, EndLine int
	// Digest covers the tokens of the whole call, from the test identifier to
	// its closing parenthesis, so comment and layout edits do not change it.
	Digest string
}

// ScriptTestFile is the lexical reading of one TypeScript or JavaScript test
// file.
type ScriptTestFile struct {
	// Tests are the test calls in source order. A name can appear more than
	// once (for example the same title in two describe blocks with one
	// title); callers decide what an ambiguous name means.
	Tests []ScriptTest
	// Shared covers every token outside the test calls: imports, helpers,
	// describe titles, hooks (beforeEach, afterAll, ...) and module-level
	// code. Any edit there can change how an unchanged test runs.
	Shared string
}

// ReadScriptTests reads the test calls of a TypeScript or JavaScript test
// file statically; it never executes repository code. It returns false when p
// is not a TypeScript or JavaScript test file by the index's conventions.
func ReadScriptTests(p string, src []byte) (ScriptTestFile, bool) {
	if lexLanguage(p) != LangTypeScript || !lexTestFile(LangTypeScript, p) {
		return ScriptTestFile{}, false
	}
	f := parseLexical(p, src)
	inside := make([]bool, len(f.toks))
	var out ScriptTestFile
	for _, d := range f.decls {
		if !d.test || d.start < 0 || d.end >= len(f.toks) || d.start > d.end {
			continue
		}
		h := sha256.New()
		for i := d.start; i <= d.end; i++ {
			digestToken(h, f.toks[i])
			inside[i] = true
		}
		// The statement's semicolon belongs to the call: adding or removing
		// a test must not read as an edit of the shared code.
		if next := d.end + 1; next < len(f.toks) && f.toks[next].kind == tokPunct && f.toks[next].text == ";" {
			inside[next] = true
		}
		out.Tests = append(out.Tests, ScriptTest{Name: d.name, Line: int(f.toks[d.start].line), EndLine: int(f.toks[d.end].line), Digest: hex.EncodeToString(h.Sum(nil))})
	}
	h := sha256.New()
	for i, t := range f.toks {
		if !inside[i] {
			digestToken(h, t)
		}
	}
	out.Shared = hex.EncodeToString(h.Sum(nil))
	return out, true
}

// digestToken feeds one token, kind and length-prefixed text, to h, so that
// no token text can forge a token boundary.
func digestToken(h hash.Hash, t ltok) {
	var head [5]byte
	head[0] = t.kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(t.text)))
	h.Write(head[:])
	h.Write([]byte(t.text))
}

// IsScriptTestPath reports whether p is a TypeScript or JavaScript test file
// the index reads: *.test.*, *.spec.* or __tests__/, outside dependency,
// build and hidden directories.
func IsScriptTestPath(p string) bool {
	return lexIndexable(p, nil) && lexLanguage(p) == LangTypeScript && lexTestFile(LangTypeScript, p)
}

// ScriptToken is one token of a TypeScript or JavaScript source. Comments are
// not tokens. Kind is 'i' (identifier or keyword), 's' (string, template
// chunk or regular expression), 'n' (number) or 'p' (punctuation).
type ScriptToken struct {
	Text      string
	Kind      byte
	Line, Col int // Col is the 1-based byte column
	// Match is the index of the matching bracket of ( [ { ) ] }, or -1.
	Match int
}

// ScriptFunction is one function or method body of a TypeScript or
// JavaScript source, as token indexes of its first and last body tokens.
type ScriptFunction struct {
	Name      string // "f", or "C.m" for a method
	Body, End int
}

// ScriptSource is the lexical reading of one TypeScript or JavaScript source
// file: its tokens and the bodies of the functions and methods the index
// records (test calls excluded).
type ScriptSource struct {
	Tokens    []ScriptToken
	Functions []ScriptFunction
}

// ReadScriptSource tokenizes and reads a TypeScript or JavaScript source
// statically, the way the static index does. It returns false when p is not
// a TypeScript or JavaScript source.
func ReadScriptSource(p string, src []byte) (ScriptSource, bool) {
	if lexLanguage(p) != LangTypeScript {
		return ScriptSource{}, false
	}
	f := parseLexical(p, src)
	m := matchBrackets(f.toks)
	out := ScriptSource{Tokens: make([]ScriptToken, len(f.toks))}
	for i, t := range f.toks {
		out.Tokens[i] = ScriptToken{Text: t.text, Kind: t.kind, Line: int(t.line), Col: int(t.col), Match: m[i]}
	}
	for _, d := range f.decls {
		if d.test || d.bodyStart < 0 || d.end >= len(f.toks) || d.bodyStart > d.end {
			continue
		}
		out.Functions = append(out.Functions, ScriptFunction{Name: d.name, Body: d.bodyStart, End: d.end})
	}
	return out, true
}
