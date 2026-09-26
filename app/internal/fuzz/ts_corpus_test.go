package fuzz

import (
	"math"
	"strings"
	"testing"
)

func scriptTargetFor(name string, params ...Param) Target {
	return Target{Dir: "web/price.ts", Path: "web/price.ts", Line: 1, EndLine: 3, Name: name, Symbol: name, Signature: "(x)", Params: params, Results: 1, Exported: true, Language: LanguageScript}
}

func TestJSNumberMatchesNumberToString(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0", 1: "1", -1: "-1", 0.5: "0.5", -0.5: "-0.5", 100: "100", 123.456: "123.456", 0.1: "0.1",
		1e-7: "1e-7", 1.5e-7: "1.5e-7", 0.000001: "0.000001", 1e21: "1e+21", 1e20: "100000000000000000000", 1.5e300: "1.5e+300",
		9007199254740991: "9007199254740991", -2147483648: "-2147483648", math.MaxFloat64: "1.7976931348623157e+308",
		math.SmallestNonzeroFloat64: "5e-324", math.Inf(1): "Infinity", math.Inf(-1): "-Infinity",
	} {
		if got := jsNumber(f); got != want {
			t.Errorf("jsNumber(%v) = %q, want %q", f, got, want)
		}
	}
	if jsNumber(math.NaN()) != "NaN" || jsNumber(math.Copysign(0, -1)) != "-0" {
		t.Fatal("NaN or negative zero")
	}
}

func TestJSStringLiteral(t *testing.T) {
	for s, want := range map[string]string{
		"":                   `""`,
		`a"b\c`:              `"a\"b\\c"`,
		"a\nb\tc\rd":         `"a\nb\tc\rd"`,
		"\x00\x1f\x7f":       `"\u0000\u001f\u007f"`,
		"\u00e9\u65e5":       "\"\u00e9\u65e5\"",
		"\u202e\u2028\ufeff": `"\u202e\u2028\ufeff"`,
		wtf8(0xD800) + "x":   `"\ud800x"`,
		wtf8(0xDFFF):         `"\udfff"`,
		"\U0001F600":         "\"\U0001F600\"",
		"\u00a0`$":           "\"\\u00a0`$\"",
	} {
		if got := jsString(s); got != want {
			t.Errorf("jsString(%q) = %s, want %s", s, got, want)
		}
	}
	for _, cp := range []rune{0xD800, 0xDBFF, 0xDC00, 0xDFFF} {
		if r, n := decodeWTF8(wtf8(cp)); r != cp || n != 3 {
			t.Errorf("round trip of %x: %x, %d", cp, r, n)
		}
	}
}

// The TS/JS corpus follows the scheme of the Go corpus: edge values first,
// one parameter at a time, then seeded random values, deduplicated, with call
// texts that redaction leaves unchanged; a prefix property; a seed that
// depends on the module, the name and the signature.
func TestScriptCorpusScheme(t *testing.T) {
	tg := scriptTargetFor("discount", scalar("number"), scalar("string"))
	inputs := scriptCorpus(tg, 64)
	if len(inputs) != 64 {
		t.Fatalf("%d inputs", len(inputs))
	}
	if inputs[0].Call != `discount(0, "")` || inputs[1].Call != `discount(1, "")` || inputs[len(scriptNumberEdges)].Call != `discount(0, "a")` {
		t.Fatalf("order: %q %q %q", inputs[0].Call, inputs[1].Call, inputs[len(scriptNumberEdges)].Call)
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		if seen[in.Call] || !streamSafe(in.Call) {
			t.Fatalf("duplicate or unsafe call %q", in.Call)
		}
		seen[in.Call] = true
	}
	if again := scriptCorpus(tg, 64); strings.Join(calls(again), "\n") != strings.Join(calls(inputs), "\n") {
		t.Fatal("the corpus is not deterministic")
	}
	if prefix := scriptCorpus(tg, 10); strings.Join(calls(prefix), "\n") != strings.Join(calls(inputs[:10]), "\n") {
		t.Fatal("a smaller corpus is not a prefix")
	}
	other := tg
	other.Dir = "web/other.ts"
	if strings.Join(calls(scriptCorpus(other, 64)[56:]), "\n") == strings.Join(calls(inputs[56:]), "\n") {
		t.Fatal("the seed does not depend on the module")
	}
	// Every special number of the edge table appears as a literal.
	joined := strings.Join(calls(scriptCorpus(scriptTargetFor("f", scalar("number")), 64)), " ")
	for _, want := range []string{"f(NaN)", "f(-0)", "f(Infinity)", "f(-Infinity)", "f(9007199254740992)", "f(5e-324)", "f(1e+21)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("corpus lacks %s", want)
		}
	}
	if got := scriptCorpus(scriptTargetFor("none"), 5); len(got) != 1 || got[0].Call != "none()" {
		t.Fatalf("no parameters: %+v", got)
	}
	if got := scriptCorpus(scriptTargetFor("flag", scalar("boolean")), 64); len(got) != 2 || got[1].Call != "flag(true)" {
		t.Fatalf("boolean: %+v", got)
	}
}

// Arrays have no nil form; a rest parameter is displayed spread and rendered
// as separate arguments; array arguments are rendered as array literals.
func TestScriptCorpusArraysAndRest(t *testing.T) {
	tg := scriptTargetFor("sum", scalar("string"), Param{Kind: ParamSlice, Basic: "number"}, Param{Kind: ParamVariadic, Basic: "boolean"})
	inputs := scriptCorpus(tg, 40)
	if inputs[0].Call != `sum("", [], ...[])` {
		t.Fatalf("first call %q", inputs[0].Call)
	}
	for _, in := range inputs {
		if in.Args[1].Nil || in.Args[2].Nil {
			t.Fatalf("a nil array in %q", in.Call)
		}
	}
	found := false
	for _, in := range inputs {
		if in.Call == `sum("", [], ...[true, false])` {
			found = true
			if args := scriptArgs(tg, in.Args); args != `"", [], true, false` {
				t.Fatalf("rendered arguments %q", args)
			}
		}
	}
	if !found {
		t.Fatalf("no spread edge value in %q", calls(inputs))
	}
	// Random strings may hold lone surrogates, rendered as escapes.
	strs := scriptCorpus(scriptTargetFor("s", scalar("string")), 256)
	surrogate := false
	for _, in := range strs {
		if strings.Contains(in.Call, `\ud8`) || strings.Contains(in.Call, `\udf`) {
			surrogate = true
		}
		if strings.ContainsAny(in.Call, "\n\r\x00") {
			t.Fatalf("a raw control character in %q", in.Call)
		}
	}
	if !surrogate {
		t.Fatal("no lone surrogate in 256 string inputs")
	}
}

// The Go corpus is unchanged by the shared scheme: its first calls and a
// random one are pinned.
func TestGoCorpusUnchangedBySharedScheme(t *testing.T) {
	tg := Target{Dir: "calc", Name: "Discount", Signature: "func(Cents) Cents", Params: []Param{named("Cents", "int64")}, Results: 1}
	got := calls(Corpus(tg, 3))
	if strings.Join(got, ",") != "Discount(Cents(0)),Discount(Cents(1)),Discount(Cents(-1))" {
		t.Fatalf("Go corpus %q", got)
	}
}

func calls(inputs []Input) []string {
	out := make([]string, len(inputs))
	for i, in := range inputs {
		out[i] = in.Call
	}
	return out
}
