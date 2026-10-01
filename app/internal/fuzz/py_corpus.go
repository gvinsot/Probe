package fuzz

// Seeded corpus of Python targets. The scheme is the one of Corpus: the seed
// of the function's identity (Seed, with the module path in place of the
// package directory), SplitMix64, edge values first, one parameter at a time,
// then seeded random values, deduplicated by call text and filtered by
// streamSafe. Only the value tables and the renderings are Python: an int has
// arbitrary precision (kept as its decimal text in Scalar.S), a float is an
// IEEE 754 double, a str is a sequence of code points in which a lone
// surrogate is allowed (kept as WTF-8), and a list has no nil form.

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// pythonIntEdges are tried first for every int, simplest first.
var pythonIntEdges = []string{
	"0", "1", "-1", "2", "-2", "7", "10", "100", "-100", "255", "256", "1000", "65535",
	"2147483647", "-2147483648", "4294967295", "9007199254740993", "9223372036854775807", "-9223372036854775808",
	"18446744073709551616", "100000000000000000000", "-100000000000000000000",
}

// pythonFloatEdges are tried first for every float, simplest first.
var pythonFloatEdges = []float64{
	0, 1, -1, 0.5, -0.5, 2, 0.1, 1e-7, 1e16, 1e21, 1e300, -1e300, math.MaxFloat64, math.SmallestNonzeroFloat64,
	math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1),
}

// pythonStringEdges are tried first for every str, simplest first.
var pythonStringEdges = []string{
	"", "a", "A", "0", "1", "-1", " ", "  a  ", "abc", "hello world", "a,b", "a\nb", "\t", "\x00",
	"é", "日本語", "‮", wtf8(0xD800), "0.5", "True", "None", "nan", "../",
	"__class__", "__init__", strings.Repeat("a", 64),
}

// pythonEdges returns the edge values of a Python basic type.
func pythonEdges(basic string) []Scalar {
	var out []Scalar
	switch basic {
	case "bool":
		return []Scalar{{B: false}, {B: true}}
	case "str":
		for _, s := range pythonStringEdges {
			out = append(out, Scalar{S: s})
		}
	case "int":
		for _, s := range pythonIntEdges {
			out = append(out, Scalar{S: s})
		}
	default:
		for _, f := range pythonFloatEdges {
			out = append(out, Scalar{F: f})
		}
	}
	return out
}

// pythonEdgeValues returns the edge arguments of one parameter. Lists try
// empty, one element, two elements in both orders, a repeated element, and
// four elements ascending and descending.
func pythonEdgeValues(p Param) []Value {
	e := pythonEdges(p.Basic)
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

// randomPythonInt draws a small, 32-bit, 64-bit or 128-bit int.
func randomPythonInt(r *splitmix64) string {
	switch r.intn(4) {
	case 0:
		return strconv.Itoa(r.intn(33) - 16)
	case 1:
		return strconv.FormatInt(int64(int32(uint32(r.next()))), 10)
	case 2:
		return strconv.FormatInt(int64(r.next()), 10)
	}
	n := new(big.Int).SetUint64(r.next())
	n.Lsh(n, 64)
	n.Or(n, new(big.Int).SetUint64(r.next()))
	if r.next()&1 == 1 {
		n.Neg(n)
	}
	return n.String()
}

func randomPythonScalar(r *splitmix64, basic string) Scalar {
	switch basic {
	case "bool":
		return Scalar{B: r.next()&1 == 1}
	case "str":
		return Scalar{S: randomScriptString(r)}
	case "int":
		return Scalar{S: randomPythonInt(r)}
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

// randomPythonValue draws one argument: each scalar or element is an edge
// value or a random value with equal probability; lists have 0 to 8
// elements.
func randomPythonValue(r *splitmix64, p Param, e []Scalar) Value {
	if p.Kind == ParamScalar {
		return Value{Scalar: randomPythonScalar(r, p.Basic)}
	}
	elems := make([]Scalar, r.intn(9))
	for i := range elems {
		if r.next()&1 == 0 {
			elems[i] = e[r.intn(len(e))]
		} else {
			elems[i] = randomPythonScalar(r, p.Basic)
		}
	}
	return Value{Elems: elems}
}

var pythonScheme = corpusScheme{edgeValues: pythonEdgeValues, edges: pythonEdges, random: randomPythonValue, call: pythonCallText}

// pythonCorpus is Corpus for a Python target.
func pythonCorpus(t Target, n int) []Input {
	return corpusWith(pythonScheme, t, n)
}

// pythonCallText renders the display form of a call, which is also its
// observation key, for example discount(1000, 33.0), total([1.0, 2.0],
// rate=0.5) or add(1, *[2, 3]). Every literal is a Python literal of the
// value.
func pythonCallText(t Target, args []Value) string {
	return t.Name + "(" + pythonArgs(t, args) + ")"
}

// pythonArgs renders the argument list of a call: positional values, the
// elements of *args spread from a list, then keyword arguments.
func pythonArgs(t Target, args []Value) string {
	var positional, keywords []string
	for j, p := range t.Params {
		var text string
		switch p.Kind {
		case ParamScalar:
			text = pythonLiteral(p.Basic, args[j].Scalar)
		case ParamVariadic:
			text = "*" + pythonList(p, args[j])
		default:
			text = pythonList(p, args[j])
		}
		if p.Keyword != "" {
			keywords = append(keywords, p.Keyword+"="+text)
			continue
		}
		positional = append(positional, text)
	}
	return strings.Join(append(positional, keywords...), ", ")
}

func pythonList(p Param, v Value) string {
	elems := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		elems[i] = pythonLiteral(p.Basic, e)
	}
	return "[" + strings.Join(elems, ", ") + "]"
}

// pythonLiteral renders a scalar as a Python expression that evaluates to
// exactly the value.
func pythonLiteral(basic string, s Scalar) string {
	switch basic {
	case "bool":
		if s.B {
			return "True"
		}
		return "False"
	case "str":
		return pyString(s.S)
	case "int":
		return s.S
	}
	return pyFloat(s.F)
}

// pyFloat renders a float as a literal that evaluates to exactly f: the
// shortest digits that round-trip, with ".0" when they would read as an int,
// float("nan") and float("inf") for the values that have no literal, and
// -0.0 for negative zero.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return `float("nan")`
	case math.IsInf(f, 1):
		return `float("inf")`
	case math.IsInf(f, -1):
		return `-float("inf")`
	case f == 0 && math.Signbit(f):
		return "-0.0"
	}
	text := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(text, ".e") {
		text += ".0"
	}
	return text
}

// pyString renders a WTF-8 string as a double-quoted Python string literal:
// printable characters as they are, quote and backslash escaped, \n, \r and
// \t, and every other code point (controls, format characters, line
// separators, lone surrogates) as \xXX, \uXXXX or \UXXXXXXXX, so the literal
// is one line of valid UTF-8 that evaluates to exactly those code points.
func pyString(s string) string {
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
		case r < 0x100:
			const digits = "0123456789abcdef"
			b.WriteString(`\x` + string([]byte{digits[r>>4], digits[r&15]}))
		case r > 0xFFFF:
			b.WriteString(`\U` + strings.Repeat("0", 8-len(strconv.FormatInt(int64(r), 16))) + strconv.FormatInt(int64(r), 16))
		default:
			b.WriteString(jsUnit(r))
		}
	}
	b.WriteByte('"')
	return b.String()
}
