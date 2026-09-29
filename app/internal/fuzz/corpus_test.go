package fuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/redact"
)

func TestSplitMix64GoldenVectors(t *testing.T) {
	// Reference values of SplitMix64 (Vigna's splitmix64.c), computed
	// independently.
	for seed, want := range map[uint64][]uint64{
		1234567: {6457827717110365317, 3203168211198807973, 9817491932198370423, 4593380528125082431, 16408922859458223821},
		0:       {16294208416658607535, 7960286522194355700, 487617019471545679},
	} {
		r := &splitmix64{state: seed}
		for i, w := range want {
			if got := r.next(); got != w {
				t.Fatalf("seed %d output %d = %d, want %d", seed, i, got, w)
			}
		}
	}
}

var discountTarget = Target{Dir: "calc", Name: "Discount", Signature: "func(Cents) Cents", Params: []Param{{Kind: ParamScalar, Basic: "int64", Named: "Cents"}}}

func TestSeedDependsOnIdentityOnly(t *testing.T) {
	// sha256("probe-fuzz/v1\x00calc\x00Discount\x00func(Cents) Cents")[:8], computed independently.
	if got := Seed(discountTarget); got != 14535095871764194211 {
		t.Fatalf("seed %d", got)
	}
	moved := discountTarget
	moved.Path, moved.Line, moved.EndLine, moved.Inputs = "calc/other.go", 99, 120, 3
	if Seed(moved) != Seed(discountTarget) {
		t.Fatal("the seed must not depend on the location or budget")
	}
	for _, change := range []func(*Target){
		func(t *Target) { t.Dir = "pricing" },
		func(t *Target) { t.Name = "Discount2" },
		func(t *Target) { t.Signature = "func(Cents) int64" },
	} {
		other := discountTarget
		change(&other)
		if Seed(other) == Seed(discountTarget) {
			t.Fatalf("seed unchanged for %+v", other)
		}
	}
}

func corpusCalls(in []Input) []string {
	calls := make([]string, len(in))
	for i, x := range in {
		calls[i] = x.Call
	}
	return calls
}

func TestCorpusGolden(t *testing.T) {
	in := Corpus(discountTarget, 64)
	calls := corpusCalls(in)
	head := []string{"Discount(Cents(0))", "Discount(Cents(1))", "Discount(Cents(-1))", "Discount(Cents(2))", "Discount(Cents(-2))",
		"Discount(Cents(7))", "Discount(Cents(10))", "Discount(Cents(100))", "Discount(Cents(-100))", "Discount(Cents(255))",
		"Discount(Cents(256))", "Discount(Cents(1000))"}
	if strings.Join(calls[:len(head)], ",") != strings.Join(head, ",") {
		t.Fatalf("head %v", calls[:len(head)])
	}
	// A change in the generator changes the inputs of every pull request;
	// it must be deliberate (bump the seed scheme).
	sum := sha256.Sum256([]byte(strings.Join(calls, "\n")))
	if got := hex.EncodeToString(sum[:]); got != "4b325f866a90e2aa0bae533b343c2e0901ad93c4bcfc438f9251359fd2524320" || len(calls) != 64 {
		t.Fatalf("corpus digest %s (%d inputs)", got, len(calls))
	}
	join := Target{Dir: "calc", Name: "Join", Signature: "func([]string) string", Params: []Param{{Kind: ParamSlice, Basic: "string"}}}
	calls = corpusCalls(Corpus(join, 64))
	sum = sha256.Sum256([]byte(strings.Join(calls, "\n")))
	if got := hex.EncodeToString(sum[:]); got != "9cb5efbd264fc75cf9650479e2d438c6d58b3d1ed1986cdfa42872e4da12390e" {
		t.Fatalf("Join corpus digest %s", got)
	}
	if calls[0] != "Join([]string(nil))" || calls[1] != "Join([]string{})" || calls[2] != `Join([]string{""})` || calls[3] != `Join([]string{"a", ""})` {
		t.Fatalf("Join head %v", calls[:4])
	}
}

func TestCorpusOrderAndPrefix(t *testing.T) {
	percent := Target{Dir: "calc", Name: "Percent", Signature: "func(int, int) int", Params: []Param{{Kind: ParamScalar, Basic: "int"}, {Kind: ParamScalar, Basic: "int"}}}
	full := Corpus(percent, 64)
	calls := corpusCalls(full)
	if calls[0] != "Percent(0, 0)" || calls[1] != "Percent(1, 0)" || calls[2] != "Percent(-1, 0)" {
		t.Fatalf("one-factor order %v", calls[:3])
	}
	edgesCount := len(edges("int"))
	if calls[edgesCount] != "Percent(0, 1)" {
		t.Fatalf("second parameter order %v", calls[edgesCount-1:edgesCount+2])
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if seen[c] {
			t.Fatalf("duplicate input %s", c)
		}
		seen[c] = true
	}
	for _, n := range []int{1, 7, 20, 63} {
		if got := corpusCalls(Corpus(percent, n)); strings.Join(got, "\n") != strings.Join(calls[:n], "\n") {
			t.Fatalf("Corpus(%d) is not a prefix of Corpus(64)", n)
		}
	}
	if Corpus(percent, 0) != nil {
		t.Fatal("n=0 must give no input")
	}
	zero := Target{Dir: "calc", Name: "Now", Signature: "func() int"}
	if in := Corpus(zero, 64); len(in) != 1 || in[0].Call != "Now()" || len(in[0].Args) != 0 {
		t.Fatalf("zero-parameter corpus %+v", in)
	}
	boolean := Target{Dir: "p", Name: "B", Signature: "func(bool) int", Params: []Param{{Kind: ParamScalar, Basic: "bool"}}}
	if in := Corpus(boolean, 64); len(in) != 2 {
		t.Fatalf("bool corpus has %d inputs", len(in))
	}
	many := Target{Dir: "p", Name: "S", Signature: "func(string) int", Params: []Param{{Kind: ParamScalar, Basic: "string"}}}
	if in := Corpus(many, 256); len(in) != 256 {
		t.Fatalf("string corpus has %d inputs", len(in))
	}
}

func TestEdgesCoverKindBounds(t *testing.T) {
	for basic, bounds := range map[string][2]int64{
		"int8": {math.MinInt8, math.MaxInt8}, "int16": {math.MinInt16, math.MaxInt16}, "int32": {math.MinInt32, math.MaxInt32},
		"rune": {math.MinInt32, math.MaxInt32}, "int64": {math.MinInt64, math.MaxInt64}, "int": {math.MinInt64, math.MaxInt64},
	} {
		values := map[int64]bool{}
		for _, s := range edges(basic) {
			if s.I < bounds[0] || s.I > bounds[1] {
				t.Fatalf("%s edge %d out of range", basic, s.I)
			}
			values[s.I] = true
		}
		for _, v := range []int64{0, bounds[0], bounds[1], bounds[0] + 1, bounds[1] - 1} {
			if !values[v] {
				t.Fatalf("%s edges lack %d", basic, v)
			}
		}
		if edges(basic)[0].I != 0 {
			t.Fatalf("%s first edge is not 0", basic)
		}
	}
	for basic, max := range map[string]uint64{"uint8": math.MaxUint8, "byte": math.MaxUint8, "uint16": math.MaxUint16, "uint32": math.MaxUint32, "uint64": math.MaxUint64, "uint": math.MaxUint64} {
		values := map[uint64]bool{}
		for _, s := range edges(basic) {
			if s.U > max {
				t.Fatalf("%s edge %d out of range", basic, s.U)
			}
			values[s.U] = true
		}
		if !values[0] || !values[max] || !values[max-1] {
			t.Fatalf("%s edges lack a bound", basic)
		}
	}
	for _, basic := range []string{"float32", "float64"} {
		var nan, posInf, negInf, negZero bool
		for _, s := range edges(basic) {
			switch {
			case math.IsNaN(s.F):
				nan = true
			case math.IsInf(s.F, 1):
				posInf = true
			case math.IsInf(s.F, -1):
				negInf = true
			case s.F == 0 && math.Signbit(s.F):
				negZero = true
			}
			if basic == "float32" && !math.IsNaN(s.F) && float64(float32(s.F)) != s.F {
				t.Fatalf("float32 edge %v is not representable", s.F)
			}
		}
		if !nan || !posInf || !negInf || !negZero {
			t.Fatalf("%s edges lack a special value", basic)
		}
	}
	runes := map[int64]bool{}
	for _, s := range edges("rune") {
		runes[s.I] = true
	}
	for _, r := range []int64{'a', 0x10FFFF, 0xD800, -1} {
		if !runes[r] {
			t.Fatalf("rune edges lack %d", r)
		}
	}
}

// TestCorpusRendersValidGo parses every call and every case expression of
// corpora of every parameter shape, and checks that literals decode to the
// generated values.
func TestCorpusRendersValidGo(t *testing.T) {
	var params []Param
	for basic := range basicTypes {
		params = append(params, Param{Kind: ParamScalar, Basic: basic}, Param{Kind: ParamSlice, Basic: basic},
			Param{Kind: ParamArray, Basic: basic, Len: 3}, Param{Kind: ParamVariadic, Basic: basic},
			Param{Kind: ParamScalar, Basic: basic, Named: "T"}, Param{Kind: ParamArray, Basic: basic, Len: 0})
	}
	for _, p := range params {
		target := Target{Dir: "p", Name: "F", Signature: "func(" + p.Type() + ")", Params: []Param{p}}
		for _, in := range Corpus(target, 128) {
			if !redact.IsFixedPoint(in.Call) {
				t.Fatalf("call %q is not a redaction fixed point", in.Call)
			}
			expr, err := parser.ParseExpr(in.Call)
			if err != nil {
				t.Fatalf("call %q does not parse: %v", in.Call, err)
			}
			if _, ok := expr.(*ast.CallExpr); !ok {
				t.Fatalf("call %q is not a call", in.Call)
			}
			code := argCode(p, in.Args[0], "probeFuzzabcdef12_math")
			if _, err := parser.ParseExpr(code); err != nil {
				t.Fatalf("case expression %q does not parse: %v", code, err)
			}
			v := in.Args[0]
			switch p.Kind {
			case ParamSlice, ParamVariadic:
				if len(v.Elems) > 8 || v.Nil && len(v.Elems) != 0 {
					t.Fatalf("slice bound: %+v", v)
				}
			case ParamArray:
				if len(v.Elems) != p.Len {
					t.Fatalf("array length: %+v", v)
				}
			}
			checkLiterals(t, p, v, code)
		}
	}
}

// checkLiterals decodes the scalar literals of a case expression and compares
// them with the generated values.
func checkLiterals(t *testing.T, p Param, v Value, code string) {
	t.Helper()
	class, bits := basicInfo(p.Basic)
	scalars := v.Elems
	if p.Kind == ParamScalar {
		scalars = []Scalar{v.Scalar}
	}
	for _, s := range scalars {
		lit, constant := literal(p.Basic, s, "m")
		if !constant {
			if class != classFloat {
				t.Fatalf("non-constant literal %q for %s", lit, p.Basic)
			}
			continue
		}
		switch class {
		case classString:
			got, err := strconv.Unquote(lit)
			if err != nil || got != s.S || len(s.S) > maxStringBytes {
				t.Fatalf("string literal %q decodes to %q, want %q", lit, got, s.S)
			}
		case classInt:
			var got int64
			var err error
			if strings.HasPrefix(lit, "'") {
				var r string
				r, err = strconv.Unquote(lit)
				got = int64([]rune(r)[0])
			} else {
				got, err = strconv.ParseInt(lit, 10, bits)
			}
			if err != nil || got != s.I {
				t.Fatalf("%s literal %q decodes to %d, want %d (%v)", p.Basic, lit, got, s.I, err)
			}
		case classUint:
			got, err := strconv.ParseUint(lit, 10, bits)
			if err != nil || got != s.U {
				t.Fatalf("%s literal %q decodes to %d, want %d", p.Basic, lit, got, s.U)
			}
		case classFloat:
			got, err := strconv.ParseFloat(lit, bits)
			if err != nil || math.Float64bits(got) != math.Float64bits(s.F) {
				t.Fatalf("%s literal %q decodes to %v, want %v", p.Basic, lit, got, s.F)
			}
		}
		if !strings.Contains(code, lit) {
			t.Fatalf("case expression %q lacks %q", code, lit)
		}
	}
}

func TestCorpusStringsIncludeInvalidUTF8AndSpecials(t *testing.T) {
	target := Target{Dir: "p", Name: "S", Signature: "func(string)", Params: []Param{{Kind: ParamScalar, Basic: "string"}}}
	var invalid, control, multibyte bool
	for _, in := range Corpus(target, 256) {
		s := in.Args[0].Scalar.S
		if len(s) > maxStringBytes {
			t.Fatalf("string of %d bytes", len(s))
		}
		invalid = invalid || !utf8.ValidString(s)
		control = control || strings.ContainsAny(s, "\x00\n\t")
		multibyte = multibyte || strings.ContainsAny(s, "é日😀")
	}
	if !invalid || !control || !multibyte {
		t.Fatalf("invalid %v, control %v, multibyte %v", invalid, control, multibyte)
	}
	if got := callText(target, []Value{{Scalar: Scalar{S: "\xff"}}}); got != `S("\xff")` {
		t.Fatalf("invalid byte rendered as %s", got)
	}
}

func TestLiteralForms(t *testing.T) {
	cases := []struct {
		p    Param
		s    Scalar
		call string // display of F(x)
		code string // case expression with math package m
	}{
		{Param{Kind: ParamScalar, Basic: "float64"}, Scalar{F: math.NaN()}, "F(math.NaN())", "m.NaN()"},
		{Param{Kind: ParamScalar, Basic: "float32"}, Scalar{F: math.Inf(1)}, "F(float32(math.Inf(1)))", "float32(m.Inf(1))"},
		{Param{Kind: ParamScalar, Basic: "float64", Named: "Ratio"}, Scalar{F: math.Copysign(0, -1)}, "F(Ratio(math.Copysign(0, -1)))", "Ratio(m.Copysign(0, -1))"},
		{Param{Kind: ParamScalar, Basic: "float64", Named: "Ratio"}, Scalar{F: 0.5}, "F(Ratio(0.5))", "0.5"},
		{Param{Kind: ParamScalar, Basic: "float32"}, Scalar{F: float64(float32(0.1))}, "F(0.1)", "0.1"},
		{Param{Kind: ParamScalar, Basic: "rune"}, Scalar{I: 'a'}, "F('a')", "'a'"},
		{Param{Kind: ParamScalar, Basic: "rune"}, Scalar{I: 0xD800}, "F(55296)", "55296"},
		{Param{Kind: ParamScalar, Basic: "rune"}, Scalar{I: -1}, "F(-1)", "-1"},
		{Param{Kind: ParamScalar, Basic: "byte"}, Scalar{U: 255}, "F(255)", "255"},
		{Param{Kind: ParamScalar, Basic: "bool", Named: "Flag"}, Scalar{B: true}, "F(Flag(true))", "true"},
		{Param{Kind: ParamScalar, Basic: "string"}, Scalar{S: "a\x00\"é"}, `F("a\x00\"é")`, `"a\x00\"é"`},
	}
	for _, c := range cases {
		target := Target{Name: "F", Params: []Param{c.p}}
		if got := callText(target, []Value{{Scalar: c.s}}); got != c.call {
			t.Errorf("display %s, want %s", got, c.call)
		}
		if got := argCode(c.p, Value{Scalar: c.s}, "m"); got != c.code {
			t.Errorf("code %s, want %s", got, c.code)
		}
	}
	list := Param{Kind: ParamSlice, Basic: "float32", Named: "R"}
	if got := argCode(list, Value{Elems: []Scalar{{F: 1}, {F: math.NaN()}}}, "m"); got != "[]R{1, R(m.NaN())}" {
		t.Errorf("slice code %s", got)
	}
	variadicStrings := Target{Name: "J", Params: []Param{{Kind: ParamScalar, Basic: "int"}, {Kind: ParamVariadic, Basic: "string"}}}
	if got := callText(variadicStrings, []Value{{Scalar: Scalar{I: 1}}, {Nil: true}}); got != "J(1)" {
		t.Errorf("nil variadic %s", got)
	}
	if got := callText(variadicStrings, []Value{{Scalar: Scalar{I: 1}}, {Elems: []Scalar{}}}); got != "J(1, []string{}...)" {
		t.Errorf("empty variadic %s", got)
	}
	arr := Param{Kind: ParamArray, Basic: "int", Len: 2}
	if got := argCode(arr, Value{Elems: []Scalar{{I: 1}, {I: 2}}}, "m"); got != "[2]int{1, 2}" {
		t.Errorf("array code %s", got)
	}
	if _, err := parser.ParseExprFrom(token.NewFileSet(), "", "[]R{1, R(m.NaN())}", 0); err != nil {
		t.Fatal(err)
	}
}
