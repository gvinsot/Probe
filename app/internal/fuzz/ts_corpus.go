package fuzz

// Seeded corpus of TS/JS targets (F2c). The scheme is the one of Corpus: the
// seed of the function's identity (Seed, with the module path in place of the
// package directory), SplitMix64, edge values first, one parameter at a time,
// then seeded random values, deduplicated by call text and filtered by
// streamSafe. Only the value tables and the renderings are JavaScript: a
// number is an IEEE 754 double, a string is a sequence of UTF-16 code units
// (kept here as WTF-8, so that a lone surrogate is representable), and an
// array has no nil form.

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// wtf8 encodes one code point, surrogates included, as WTF-8.
func wtf8(cp rune) string {
	if cp >= 0xD800 && cp <= 0xDFFF {
		return string([]byte{0xED, byte(0x80 | (cp>>6)&0x3F), byte(0x80 | cp&0x3F)})
	}
	return string(cp)
}

// decodeWTF8 decodes the first code point of s, a lone surrogate included.
// An invalid byte decodes as U+FFFD of size 1; the corpus never produces one.
func decodeWTF8(s string) (rune, int) {
	if len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2]&0xC0 == 0x80 {
		return 0xD000 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), 3
	}
	return utf8.DecodeRuneInString(s)
}

// scriptNumberEdges are tried first for every number, simplest first.
var scriptNumberEdges = []float64{
	0, 1, -1, 2, -2, 0.5, -0.5, 7, 10, 100, -100, 255, 256, 1000, 65535,
	2147483647, -2147483648, 4294967295, 9007199254740991, -9007199254740991, 9007199254740992,
	0.1, 1e-7, 1e21, 1e300, -1e300, math.MaxFloat64, math.SmallestNonzeroFloat64,
	math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1),
}

// scriptStringEdges are tried first for every string, simplest first.
var scriptStringEdges = []string{
	"", "a", "A", "0", "1", "-1", " ", "  a  ", "abc", "hello world", "a,b", "a\nb", "\t", "\x00",
	"é", "日本語", "\u202e", wtf8(0xD800), "0.5", "true", "null", "undefined", "NaN", "../",
	"__proto__", "constructor", strings.Repeat("a", 64),
}

// scriptEdges returns the edge values of a TS/JS basic type.
func scriptEdges(basic string) []Scalar {
	var out []Scalar
	switch basic {
	case "boolean":
		return []Scalar{{B: false}, {B: true}}
	case "string":
		for _, s := range scriptStringEdges {
			out = append(out, Scalar{S: s})
		}
		return out
	}
	for _, f := range scriptNumberEdges {
		out = append(out, Scalar{F: f})
	}
	return out
}

// scriptEdgeValues returns the edge arguments of one parameter. Arrays try
// empty, one element, two elements in both orders, a repeated element, and
// four elements ascending and descending.
func scriptEdgeValues(p Param) []Value {
	e := scriptEdges(p.Basic)
	if p.Kind == ParamScalar {
		out := make([]Value, len(e))
		for i, s := range e {
			out[i] = Value{Scalar: s}
		}
		return out
	}
	out := []Value{{Elems: []Scalar{}}, {Elems: []Scalar{e[0]}}, {Elems: []Scalar{e[1], e[0]}}, {Elems: []Scalar{e[0], e[0]}}}
	n := min(4, len(e))
	asc := append([]Scalar(nil), e[:n]...)
	desc := make([]Scalar, n)
	for i := range asc {
		desc[n-1-i] = asc[i]
	}
	return append(out, Value{Elems: asc}, Value{Elems: desc})
}

// Pieces of random JavaScript strings besides stringAlphabet, stringMultibyte
// and stringControl: lone surrogates stand for what invalid UTF-8 is in Go.
var scriptStringInvalid = []string{wtf8(0xD800), wtf8(0xDFFF), wtf8(0xD83D)}

func randomScriptString(r *splitmix64) string {
	n := r.intn(33)
	var b strings.Builder
	for i := 0; i < n; i++ {
		var piece string
		switch p := r.intn(100); {
		case p < 80:
			piece = string(stringAlphabet[r.intn(len(stringAlphabet))])
		case p < 90:
			piece = stringMultibyte[r.intn(len(stringMultibyte))]
		case p < 95:
			piece = stringControl[r.intn(len(stringControl))]
		default:
			piece = scriptStringInvalid[r.intn(len(scriptStringInvalid))]
		}
		if b.Len()+len(piece) > maxStringBytes {
			break
		}
		b.WriteString(piece)
	}
	return b.String()
}

func randomScriptScalar(r *splitmix64, basic string) Scalar {
	switch basic {
	case "boolean":
		return Scalar{B: r.next()&1 == 1}
	case "string":
		return Scalar{S: randomScriptString(r)}
	}
	switch r.intn(4) {
	case 0:
		return Scalar{F: float64(r.intn(33) - 16)}
	case 1:
		return Scalar{F: float64(int32(uint32(r.next())))}
	case 2:
		f := math.Float64frombits(r.next())
		if math.IsNaN(f) {
			f = math.NaN()
		}
		return Scalar{F: f}
	}
	return Scalar{F: float64(r.next()>>11)/(1<<53)*2000 - 1000}
}

// randomScriptValue draws one argument: each scalar or element is an edge
// value or a random value with equal probability; arrays have 0 to 8
// elements.
func randomScriptValue(r *splitmix64, p Param, e []Scalar) Value {
	if p.Kind == ParamScalar {
		return Value{Scalar: randomScriptScalar(r, p.Basic)}
	}
	elems := make([]Scalar, r.intn(9))
	for i := range elems {
		if r.next()&1 == 0 {
			elems[i] = e[r.intn(len(e))]
		} else {
			elems[i] = randomScriptScalar(r, p.Basic)
		}
	}
	return Value{Elems: elems}
}

var scriptScheme = corpusScheme{edgeValues: scriptEdgeValues, edges: scriptEdges, random: randomScriptValue, call: scriptCallText}

// scriptCorpus is Corpus for a TS/JS target.
func scriptCorpus(t Target, n int) []Input {
	return corpusWith(scriptScheme, t, n)
}

// scriptCallText renders the display form of a call, which is also its
// observation key, for example discount(1000, 33), join(["a", "b"]) or
// sum(1, ...[2, 3]). Every literal is a JavaScript literal of the value.
func scriptCallText(t Target, args []Value) string {
	parts := make([]string, 0, len(args))
	for j, p := range t.Params {
		switch p.Kind {
		case ParamScalar:
			parts = append(parts, scriptLiteral(p.Basic, args[j].Scalar))
		case ParamVariadic:
			parts = append(parts, "..."+scriptList(p, args[j]))
		default:
			parts = append(parts, scriptList(p, args[j]))
		}
	}
	return t.Name + "(" + strings.Join(parts, ", ") + ")"
}

// scriptArgs renders the JavaScript argument list a case returns: one
// expression per parameter, the elements of a rest parameter spread in place.
func scriptArgs(t Target, args []Value) string {
	parts := make([]string, 0, len(args))
	for j, p := range t.Params {
		switch p.Kind {
		case ParamScalar:
			parts = append(parts, scriptLiteral(p.Basic, args[j].Scalar))
		case ParamVariadic:
			for _, e := range args[j].Elems {
				parts = append(parts, scriptLiteral(p.Basic, e))
			}
		default:
			parts = append(parts, scriptList(p, args[j]))
		}
	}
	return strings.Join(parts, ", ")
}

func scriptList(p Param, v Value) string {
	elems := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		elems[i] = scriptLiteral(p.Basic, e)
	}
	return "[" + strings.Join(elems, ", ") + "]"
}

// scriptLiteral renders a scalar as a JavaScript literal.
func scriptLiteral(basic string, s Scalar) string {
	switch basic {
	case "boolean":
		return strconv.FormatBool(s.B)
	case "string":
		return jsString(s.S)
	}
	return jsNumber(s.F)
}

// jsNumber renders f as ECMAScript's Number::toString does, except that
// negative zero is "-0" (a literal that evaluates to it), so that every
// rendered number evaluates to exactly f.
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0 && math.Signbit(f):
		return "-0"
	case f == 0:
		return "0"
	case f < 0:
		return "-" + jsNumber(-f)
	}
	// The shortest digits that round-trip, and n such that f = 0.digits x 10^n.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	n := x + 1
	k := len(digits)
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	exponent := strconv.Itoa(abs(n - 1))
	if k == 1 {
		return digits + "e" + sign + exponent
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + exponent
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// jsString renders a WTF-8 string as a double-quoted JavaScript string
// literal: printable characters as they are, quote and backslash escaped,
// \n, \r and \t, and every other code unit (controls, format characters,
// line separators, lone surrogates) as \uXXXX, so the literal is one line of
// valid UTF-8 that evaluates to exactly those code units.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := decodeWTF8(s[i:])
		i += size
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r >= 0xD800 && r <= 0xDFFF:
			b.WriteString(jsUnit(r))
		case r >= 0x20 && r < 0x7f || r >= 0xA0 && strconv.IsPrint(r):
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			b.WriteString(jsUnit(0xD800 + r>>10))
			b.WriteString(jsUnit(0xDC00 + r&0x3FF))
		default:
			b.WriteString(jsUnit(r))
		}
	}
	b.WriteByte('"')
	return b.String()
}

func jsUnit(u rune) string {
	const digits = "0123456789abcdef"
	return `\u` + string([]byte{digits[u>>12&15], digits[u>>8&15], digits[u>>4&15], digits[u&15]})
}
