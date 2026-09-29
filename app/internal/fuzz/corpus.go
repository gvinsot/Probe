package fuzz

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/model"
)

// splitmix64 is the SplitMix64 generator (Steele, Lea and Flood, 2014). It is
// defined here rather than taken from math/rand so that the corpus stays
// byte-identical across Go versions; the tests pin its output.
type splitmix64 struct{ state uint64 }

func (s *splitmix64) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// intn returns a value in [0, n). The modulo bias is irrelevant here: the
// generator only has to be deterministic.
func (s *splitmix64) intn(n int) int { return int(s.next() % uint64(n)) }

// Seed is the corpus seed of a target: the first eight bytes (big-endian) of
// SHA-256 over the seed scheme, the package directory, the function name and
// the signature. It depends on the function's identity only, never on commit
// IDs, so the inputs stay the same across updates of a pull request.
func Seed(t Target) uint64 {
	sum := sha256.Sum256([]byte(model.FuzzSeedScheme + "\x00" + t.Dir + "\x00" + t.Name + "\x00" + t.Signature))
	return binary.BigEndian.Uint64(sum[:8])
}

type valueClass int

const (
	classBool valueClass = iota
	classString
	classInt
	classUint
	classFloat
)

// basicInfo returns the class and bit size of a generated basic type. int and
// uint are 64-bit: the sandbox platforms are linux/amd64 and linux/arm64.
func basicInfo(basic string) (valueClass, int) {
	switch basic {
	case "bool":
		return classBool, 1
	case "string":
		return classString, 0
	case "int8":
		return classInt, 8
	case "int16":
		return classInt, 16
	case "int32", "rune":
		return classInt, 32
	case "int", "int64":
		return classInt, 64
	case "uint8", "byte":
		return classUint, 8
	case "uint16":
		return classUint, 16
	case "uint32":
		return classUint, 32
	case "uint", "uint64":
		return classUint, 64
	case "float32":
		return classFloat, 32
	}
	return classFloat, 64
}

// stringEdges are tried first for every string element, simplest first.
var stringEdges = []string{
	"", "a", "A", "0", "1", "-1", " ", "  a  ", "abc", "hello world", "a,b", "a\nb", "\t", "\x00",
	"é", "日本語", "‮", "\xff", "0.5", "true", "null", "../", strings.Repeat("a", 64),
}

// edges returns the edge values of a basic type, simplest first. Integer
// edges include each kind's minimum and maximum; float edges include NaN,
// both infinities and negative zero.
func edges(basic string) []Scalar {
	class, bits := basicInfo(basic)
	var out []Scalar
	switch class {
	case classBool:
		return []Scalar{{B: false}, {B: true}}
	case classString:
		for _, s := range stringEdges {
			out = append(out, Scalar{S: s})
		}
		return out
	case classInt:
		lo, hi := int64(math.MinInt64), int64(math.MaxInt64)
		if bits < 64 {
			lo, hi = -1<<(bits-1), 1<<(bits-1)-1
		}
		values := []int64{0, 1, -1, 2, -2, 7, 10, 100, -100, 255, 256, 1000, 65535, 1<<31 - 1, -1 << 31, lo, hi, lo + 1, hi - 1}
		if basic == "rune" {
			values = append([]int64{0, 'a', 1, -1, 'A', '0', ' ', 'é', '日', 0x10FFFF, 0xD800, 0x110000}, values...)
		}
		seen := map[int64]bool{}
		for _, v := range values {
			if v >= lo && v <= hi && !seen[v] {
				seen[v] = true
				out = append(out, Scalar{I: v})
			}
		}
		return out
	case classUint:
		hi := uint64(math.MaxUint64)
		if bits < 64 {
			hi = 1<<bits - 1
		}
		seen := map[uint64]bool{}
		for _, v := range []uint64{0, 1, 2, 7, 10, 100, 255, 256, 1000, 65535, 1<<31 - 1, 1<<32 - 1, hi, hi - 1} {
			if v <= hi && !seen[v] {
				seen[v] = true
				out = append(out, Scalar{U: v})
			}
		}
		return out
	}
	values := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, 0.1, 1e-9, 1e6, 1e300, -1e300, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1), math.NaN()}
	if bits == 32 {
		values = []float64{0, math.Copysign(0, -1), 1, -1, 0.5, 0.1, 1e-9, 1e6, 1e30, -1e30, math.MaxFloat32, math.SmallestNonzeroFloat32, math.Inf(1), math.Inf(-1), math.NaN()}
	}
	seen := map[uint64]bool{}
	for _, v := range values {
		v = narrowFloat(v, bits)
		key := math.Float64bits(v)
		if math.IsNaN(v) {
			key = math.Float64bits(math.NaN())
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, Scalar{F: v})
		}
	}
	return out
}

// narrowFloat rounds v to the precision of a float of the given size.
func narrowFloat(v float64, bits int) float64 {
	if bits == 32 {
		return float64(float32(v))
	}
	return v
}

// edgeValues returns the edge arguments of one parameter. Slices and
// variadics try nil, empty, one element, two elements in both orders, a
// repeated element, and four elements ascending and descending; arrays try
// the zero value and the first edges in both orders.
func edgeValues(p Param) []Value {
	e := edges(p.Basic)
	var out []Value
	switch p.Kind {
	case ParamSlice, ParamVariadic:
		out = append(out, Value{Nil: true}, Value{Elems: []Scalar{}}, Value{Elems: []Scalar{e[0]}})
		if len(e) > 1 {
			out = append(out, Value{Elems: []Scalar{e[1], e[0]}})
		}
		out = append(out, Value{Elems: []Scalar{e[0], e[0]}})
		n := min(4, len(e))
		asc := append([]Scalar(nil), e[:n]...)
		desc := make([]Scalar, n)
		for i := range asc {
			desc[n-1-i] = asc[i]
		}
		return append(out, Value{Elems: asc}, Value{Elems: desc})
	case ParamArray:
		zero := make([]Scalar, p.Len)
		out = append(out, Value{Elems: zero})
		if p.Len == 0 {
			return out
		}
		asc := make([]Scalar, p.Len)
		desc := make([]Scalar, p.Len)
		for i := range asc {
			asc[i] = e[i%len(e)]
			desc[p.Len-1-i] = asc[i]
		}
		return append(out, Value{Elems: asc}, Value{Elems: desc})
	}
	for _, s := range e {
		out = append(out, Value{Scalar: s})
	}
	return out
}

// Biased alphabet of random strings: mostly ASCII, with occasional multibyte
// characters, control characters and invalid UTF-8 sequences.
const stringAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 -_.,;:/=+*#@!?()[]{}<>'\"\\"

var (
	stringMultibyte = []string{"é", "ß", "日", "本", "€", "😀", "‮", "́"}
	stringControl   = []string{"\n", "\t", "\x00", "\r"}
	stringInvalid   = []string{"\xff", "\xc3", "\xed\xa0\x80"}
)

// maxStringBytes bounds a generated string value.
const maxStringBytes = 64

func randomString(r *splitmix64) string {
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
			piece = stringInvalid[r.intn(len(stringInvalid))]
		}
		if b.Len()+len(piece) > maxStringBytes {
			break
		}
		b.WriteString(piece)
	}
	return b.String()
}

func randomScalar(r *splitmix64, basic string) Scalar {
	class, bits := basicInfo(basic)
	switch class {
	case classBool:
		return Scalar{B: r.next()&1 == 1}
	case classString:
		return Scalar{S: randomString(r)}
	case classInt:
		switch r.intn(3) {
		case 0:
			return Scalar{I: int64(r.intn(33)) - 16}
		case 1:
			if basic == "rune" {
				v := int64(r.intn(0x110000))
				if v >= 0xD800 && v <= 0xDFFF {
					v = 'x'
				}
				return Scalar{I: v}
			}
		}
		shift := uint(64 - bits)
		return Scalar{I: int64(r.next()) << shift >> shift}
	case classUint:
		if r.next()&1 == 0 {
			return Scalar{U: uint64(r.intn(33))}
		}
		v := r.next()
		if bits < 64 {
			v &= 1<<uint(bits) - 1
		}
		return Scalar{U: v}
	}
	var f float64
	switch r.intn(3) {
	case 0:
		if bits == 32 {
			f = float64(math.Float32frombits(uint32(r.next())))
		} else {
			f = math.Float64frombits(r.next())
		}
		if math.IsNaN(f) {
			f = math.NaN()
		}
	case 1:
		f = float64(r.next()>>11)/(1<<53)*2000 - 1000
	default:
		f = float64(r.next()>>11) / (1 << 53)
	}
	return Scalar{F: narrowFloat(f, bits)}
}

// randomValue draws one argument: each scalar or element is an edge value or
// a random value with equal probability; slices have 0 to 8 elements and are
// nil one time in ten.
func randomValue(r *splitmix64, p Param, e []Scalar) Value {
	element := func() Scalar {
		if r.next()&1 == 0 {
			return e[r.intn(len(e))]
		}
		return randomScalar(r, p.Basic)
	}
	switch p.Kind {
	case ParamSlice, ParamVariadic:
		if r.intn(10) == 0 {
			return Value{Nil: true}
		}
		elems := make([]Scalar, r.intn(9))
		for i := range elems {
			elems[i] = element()
		}
		return Value{Elems: elems}
	case ParamArray:
		elems := make([]Scalar, p.Len)
		for i := range elems {
			elems[i] = element()
		}
		return Value{Elems: elems}
	}
	return Value{Scalar: randomScalar(r, p.Basic)}
}

// Corpus returns up to n distinct seeded inputs of t, in a fixed order: input
// 0 has every parameter at its first edge value; then each parameter runs
// through its other edge values while the others stay at their first; then
// seeded random inputs follow. Inputs are deduplicated by their call text and
// an input whose call text redaction would alter, as it is or JSON-escaped in
// the normalized stream (streamSafe), is dropped, so every call is a stable
// observation key that redaction leaves unchanged. A function without
// parameters gets exactly one input. The corpus is empty when no call
// survives, for example when the function name itself looks like a
// credential; selection skips such a function. Corpus(t, m) is a prefix of
// Corpus(t, n) for m <= n.
func Corpus(t Target, n int) []Input {
	return corpusWith(goScheme, t, n)
}

// corpusScheme holds the value tables and renderings of one language. The
// order of the corpus and the use of the generator are the same for every
// language (corpusWith).
type corpusScheme struct {
	edgeValues func(Param) []Value
	edges      func(basic string) []Scalar
	random     func(r *splitmix64, p Param, e []Scalar) Value
	call       func(t Target, args []Value) string
}

var goScheme = corpusScheme{edgeValues: edgeValues, edges: edges, random: randomValue, call: callText}

// corpusOf returns the corpus of a target of either language.
func corpusOf(t Target, n int) []Input {
	if t.Language == LanguageScript {
		return scriptCorpus(t, n)
	}
	return Corpus(t, n)
}

func corpusWith(scheme corpusScheme, t Target, n int) []Input {
	if n < 1 {
		return nil
	}
	if len(t.Params) == 0 {
		call := scheme.call(t, nil)
		if !streamSafe(call) {
			return nil
		}
		return []Input{{Args: []Value{}, Call: call}}
	}
	sets := make([][]Value, len(t.Params))
	scalars := make([][]Scalar, len(t.Params))
	for j, p := range t.Params {
		sets[j] = scheme.edgeValues(p)
		scalars[j] = scheme.edges(p.Basic)
	}
	var out []Input
	seen := map[string]bool{}
	add := func(args []Value) bool {
		call := scheme.call(t, args)
		if !seen[call] && streamSafe(call) {
			seen[call] = true
			out = append(out, Input{Args: append([]Value(nil), args...), Call: call})
		}
		return len(out) >= n
	}
	first := make([]Value, len(t.Params))
	for j := range t.Params {
		first[j] = sets[j][0]
	}
	if add(first) {
		return out
	}
	for j := range t.Params {
		for _, v := range sets[j][1:] {
			args := append([]Value(nil), first...)
			args[j] = v
			if add(args) {
				return out
			}
		}
	}
	r := &splitmix64{state: Seed(t)}
	for attempt := 0; attempt < 16*n+64; attempt++ {
		args := make([]Value, len(t.Params))
		for j, p := range t.Params {
			if r.next()&1 == 0 {
				args[j] = sets[j][r.intn(len(sets[j]))]
			} else {
				args[j] = scheme.random(r, p, scalars[j])
			}
		}
		if add(args) {
			break
		}
	}
	return out
}

// callText renders the display form of a call, e.g. Discount(Cents(1000)) or
// Join([]string{"a", "b"}). A nil variadic argument is omitted; any other is
// spread explicitly, so nil and empty stay distinct.
func callText(t Target, args []Value) string {
	parts := make([]string, 0, len(args))
	for j, p := range t.Params {
		v := args[j]
		switch p.Kind {
		case ParamScalar:
			parts = append(parts, scalarExpr(p, v.Scalar, "math", true))
		case ParamVariadic:
			if !v.Nil {
				parts = append(parts, listExpr(p, v, "math")+"...")
			}
		default:
			parts = append(parts, listExpr(p, v, "math"))
		}
	}
	return t.Name + "(" + strings.Join(parts, ", ") + ")"
}

// argCode renders the Go expression a case closure returns for one argument,
// with mathPkg naming the harness's math import.
func argCode(p Param, v Value, mathPkg string) string {
	if p.Kind == ParamScalar {
		return scalarExpr(p, v.Scalar, mathPkg, false)
	}
	return listExpr(p, v, mathPkg)
}

func listExpr(p Param, v Value, mathPkg string) string {
	if v.Nil {
		return p.Type() + "(nil)"
	}
	elems := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		elems[i] = scalarExpr(p, e, mathPkg, false)
	}
	return p.Type() + "{" + strings.Join(elems, ", ") + "}"
}

// scalarExpr renders s as an expression assignable to the element type. A
// constant stays untyped (wrapped in the named type for display when
// wrapNamed is set); a special float (NaN, an infinity, negative zero) is a
// math call converted to the element type unless that type is float64.
func scalarExpr(p Param, s Scalar, mathPkg string, wrapNamed bool) string {
	lit, constant := literal(p.Basic, s, mathPkg)
	elem := p.Elem()
	switch {
	case !constant && elem != "float64":
		return elem + "(" + lit + ")"
	case wrapNamed && p.Named != "":
		return p.Named + "(" + lit + ")"
	}
	return lit
}

// literal renders a scalar as a Go literal, or as a math call for the float
// values that have no literal (constant is then false).
func literal(basic string, s Scalar, mathPkg string) (text string, constant bool) {
	class, bits := basicInfo(basic)
	switch class {
	case classBool:
		return strconv.FormatBool(s.B), true
	case classString:
		return strconv.Quote(s.S), true
	case classInt:
		if basic == "rune" && utf8.ValidRune(rune(s.I)) && int64(rune(s.I)) == s.I {
			return strconv.QuoteRune(rune(s.I)), true
		}
		return strconv.FormatInt(s.I, 10), true
	case classUint:
		return strconv.FormatUint(s.U, 10), true
	}
	switch {
	case math.IsNaN(s.F):
		return mathPkg + ".NaN()", false
	case math.IsInf(s.F, 1):
		return mathPkg + ".Inf(1)", false
	case math.IsInf(s.F, -1):
		return mathPkg + ".Inf(-1)", false
	case s.F == 0 && math.Signbit(s.F):
		return mathPkg + ".Copysign(0, -1)", false
	}
	return strconv.FormatFloat(s.F, 'g', -1, bits), true
}
