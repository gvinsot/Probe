package fuzz

import (
	"strconv"
	"testing"
)

// BenchmarkSelectAndRenderCalc measures host-side selection and rendering of
// the calc fixture (five functions, 320 inputs).
func BenchmarkSelectAndRenderCalc(b *testing.B) {
	base, candidate := b.TempDir()+"/base", b.TempDir()+"/candidate"
	gomod := "module example.test/fuzzdemo\n\ngo 1.21\n"
	writeTree(b, base, map[string]string{"go.mod": gomod, "calc/calc.go": calcBase})
	writeTree(b, candidate, map[string]string{"go.mod": gomod, "calc/calc.go": calcCandidate})
	for i := 0; i < b.N; i++ {
		plan, err := Select(base, candidate, modified("calc/calc.go"), nil, defaultLimits())
		if err != nil || plan.Targets() != 5 {
			b.Fatal(err)
		}
		if _, err := Render(plan.Packages[0], renderOptions("abcdef12")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNormalizeFullPackage measures validating and normalizing the
// largest stream one package run plans (1024 inputs over 16 functions).
func BenchmarkNormalizeFullPackage(b *testing.B) {
	var targets []Target
	for i := 0; i < 16; i++ {
		targets = append(targets, target("F"+strconv.Itoa(i), 1, 64, scalar("int"), scalar("string")))
	}
	h, err := Render(obsPlan(targets...), renderOptions("abcdef12"))
	if err != nil {
		b.Fatal(err)
	}
	var lines []string
	for n, test := range h.Tests {
		lines = append(lines, beginLine(n+1, len(test.Inputs)))
		for i := range test.Inputs {
			lines = append(lines, obsLine(n+1, i, "int("+strconv.Itoa(i*7919)+"), error(nil)", false))
		}
		lines = append(lines, endLine(n+1, len(test.Inputs)))
	}
	payload := rawStream(lines...)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Normalize(payload); err != nil {
			b.Fatal(err)
		}
	}
}
