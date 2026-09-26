package fuzz

import (
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// scriptStreamHarness renders a two-test TS/JS harness: a(number) with 3
// inputs and b(string) with 2.
func scriptStreamHarness(t *testing.T) Harness {
	t.Helper()
	a := scriptTargetFor("a", scalar("number"))
	a.Inputs = 3
	b := scriptTargetFor("b", scalar("string"))
	b.Inputs = 2
	h, err := RenderScript(scriptPlan("web/m.ts", a, b), scriptOptions(FamilyVitest))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Tests[0].Inputs) != 3 || len(h.Tests[1].Inputs) != 2 {
		t.Fatalf("inputs %d %d", len(h.Tests[0].Inputs), len(h.Tests[1].Inputs))
	}
	return h
}

func headOf(h Harness, f int) string { return scriptHeadLine(f, h.Suffix, h.Tests[f-1].Name) }
func doneOf(h Harness, f, n int) string {
	return scriptDoneLine(f, h.Tests[f-1].Name, n)
}

func scriptCompleteLines(h Harness) []string {
	return []string{
		headOf(h, 1), beginLine(1, 3), obsLine(1, 0, "0", false), obsLine(1, 1, "1", false), obsLine(1, 2, `throw(error("TypeError", "x"))`, false), endLine(1, 3), doneOf(h, 1, 3),
		headOf(h, 2), beginLine(2, 2), obsLine(2, 0, `""`, true), obsLine(2, 1, `resolved("A")`, false), endLine(2, 2), doneOf(h, 2, 2),
	}
}

func TestNormalizeScriptValidStream(t *testing.T) {
	h := scriptStreamHarness(t)
	results, err := h.Normalize(rawStream(scriptCompleteLines(h)...))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseResults(results)
	if err != nil {
		t.Fatal(err)
	}
	if s.Runner != harness.RunnerJest || len(s.Functions) != 2 || s.Functions[0].State != StateComplete || s.Functions[1].State != StateComplete {
		t.Fatalf("stream %+v", s)
	}
	if r := s.Functions[1].Records[0]; !r.Unstable || r.Call != h.Tests[1].Inputs[0].Call {
		t.Fatalf("record %+v", r)
	}
	if n, inside := StartedTests(results); n != 2 || inside {
		t.Fatalf("started tests %d, inside %v", n, inside)
	}
	if n, inside := StartedTests("x"); n != 0 || inside {
		t.Fatalf("started tests of an invalid stream %d, inside %v", n, inside)
	}
	// The same raw lines without the framing are a Go stream, which a TS/JS
	// harness rejects.
	var goLines []string
	for _, l := range scriptCompleteLines(h) {
		if !strings.Contains(l, `"k":"head"`) && !strings.Contains(l, `"k":"done"`) {
			goLines = append(goLines, l)
		}
	}
	if _, err := h.Normalize(rawStream(goLines...)); err == nil {
		t.Fatal("a stream without head records was accepted")
	}
	// A Go harness never writes runner-marked streams, and an unknown runner
	// never parses.
	if _, err := ParseResults(strings.Replace(results, `"runner": "jest_json"`, `"runner": "mocha"`, 1)); err == nil {
		t.Fatal("an unknown stream runner parsed")
	}
}

func TestNormalizeScriptEarlyEnds(t *testing.T) {
	h := scriptStreamHarness(t)
	check := func(name string, lines []string, want ...string) {
		t.Helper()
		results, err := h.Normalize(rawStream(lines...))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s, _ := ParseResults(results)
		var got []string
		for _, f := range s.Functions {
			got = append(got, f.State+":"+f.Stop+":"+f.AtCall)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: states %q, want %q", name, got, want)
		}
	}
	a0, a1 := h.Tests[0].Inputs[0].Call, h.Tests[0].Inputs[1].Call
	b0 := h.Tests[1].Inputs[0].Call
	// A timeout stops the function and poisons the next one.
	check("timeout", []string{headOf(h, 1), beginLine(1, 3), obsLine(1, 0, "0", false), stopLine(1, 1, StopTimeout), doneOf(h, 1, 1),
		headOf(h, 2), beginLine(2, 2), stopLine(2, 0, StopPoisoned), doneOf(h, 2, 0)},
		"stopped:timeout:"+a1, "stopped:poisoned:"+b0)
	// The process ends inside a function: interrupted there, not started after.
	check("exit", []string{headOf(h, 1), beginLine(1, 3), obsLine(1, 0, "0", false)}, "interrupted::"+a1, "not_started::")
	check("exit after head", []string{headOf(h, 1)}, "interrupted::"+a0, "not_started::")
	// An end record without its done record: the test did not finish.
	check("no done", []string{headOf(h, 1), beginLine(1, 3), obsLine(1, 0, "0", false), obsLine(1, 1, "1", false), obsLine(1, 2, "2", false), endLine(1, 3)},
		"interrupted::", "not_started::")
	// A stop without its done record keeps no stop reason.
	check("stop without done", []string{headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopAbort)}, "interrupted::"+a0, "not_started::")
	check("abort", []string{headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopAbort), doneOf(h, 1, 0), headOf(h, 2)},
		"stopped:abort:"+a0, "interrupted::"+b0)
}

// StartedTests tells the shapes of a stream that does not show every test:
// it ended inside a test (the process ended while it evaluated an input) or
// between two tests (after a done record), and how many tests started.
func TestStartedTestsShapes(t *testing.T) {
	h := scriptStreamHarness(t)
	for name, tc := range map[string]struct {
		lines   []string
		started int
		inside  bool
	}{
		"complete":            {scriptCompleteLines(h), 2, false},
		"inside the first":    {[]string{headOf(h, 1), beginLine(1, 3), obsLine(1, 0, "0", false)}, 1, true},
		"after the first":     {scriptCompleteLines(h)[:7], 1, false},
		"inside the second":   {scriptCompleteLines(h)[:10], 2, true},
		"after a stop":        {[]string{headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopAbort), doneOf(h, 1, 0)}, 1, false},
		"head of the second":  {scriptCompleteLines(h)[:8], 2, true},
		"end without done":    {scriptCompleteLines(h)[:6], 1, true},
		"head of the first":   {[]string{headOf(h, 1)}, 1, true},
		"stop without done":   {[]string{headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopAbort)}, 1, true},
		"second after a stop": {[]string{headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopAbort), doneOf(h, 1, 0), headOf(h, 2), beginLine(2, 2)}, 2, true},
	} {
		results, err := h.Normalize(rawStream(tc.lines...))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n, inside := StartedTests(results); n != tc.started || inside != tc.inside {
			t.Errorf("%s: started %d inside %v, want %d %v", name, n, inside, tc.started, tc.inside)
		}
	}
}

// A goexit stop never parses in a TS/JS stream, also when a stored stream is
// parsed again on re-render; a Go stream keeps it.
func TestParseResultsRefusesGoexitInScriptStreams(t *testing.T) {
	stopped := FunctionStream{Test: testName, Planned: 3, State: StateStopped, Stop: StopGoexit, At: 1, AtCall: "F(1)", Records: encodings("0")}
	if _, err := ParseResults(results(t, stopped)); err != nil {
		t.Fatalf("Go stream with a goexit stop: %v", err)
	}
	s := Stream{Version: StreamVersion, Scheme: model.FuzzSeedScheme, Display: MaxDisplayBytes, Runner: harness.RunnerJest, Functions: []FunctionStream{stopped}}
	out, err := encodeStream(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseResults(string(out)); err == nil || !strings.Contains(err.Error(), "goexit") {
		t.Fatalf("TS/JS stream with a goexit stop: %v", err)
	}
	stopped.Stop = StopAbort
	s.Functions = []FunctionStream{stopped}
	if out, err = encodeStream(s); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseResults(string(out)); err != nil {
		t.Fatalf("TS/JS stream with an abort stop: %v", err)
	}
}

func TestNormalizeScriptRejectsMalformedStreams(t *testing.T) {
	h := scriptStreamHarness(t)
	good := scriptCompleteLines(h)
	replace := func(i int, line string) []string {
		out := append([]string(nil), good...)
		out[i] = line
		return out
	}
	other := h
	other.Suffix = "0123456789abcdef"
	for name, lines := range map[string][]string{
		"no head":               good[1:],
		"head of another run":   replace(0, scriptHeadLine(1, other.Suffix, h.Tests[0].Name)),
		"head of another test":  replace(0, scriptHeadLine(1, h.Suffix, h.Tests[1].Name)),
		"second function first": append([]string{headOf(h, 2)}, good[8:]...),
		"head with spaces":      replace(0, strings.Replace(headOf(h, 1), `,"s"`, `, "s"`, 1)),
		"begin twice":           replace(2, beginLine(1, 3)),
		"wrong plan":            replace(1, beginLine(1, 4)),
		"obs before begin":      replace(1, obsLine(1, 0, "0", false)),
		"wrong done count":      replace(6, doneOf(h, 1, 2)),
		"done of another test":  replace(6, scriptDoneLine(1, h.Tests[1].Name, 3)),
		"missing done":          append(append([]string(nil), good[:6]...), good[7:]...),
		"record after done":     append(append([]string(nil), good[:7]...), obsLine(1, 0, "0", false)),
		"goexit stop":           {headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopGoexit), doneOf(h, 1, 0)},
		"poisoned first":        {headOf(h, 1), beginLine(1, 3), stopLine(1, 0, StopPoisoned), doneOf(h, 1, 0)},
		"extra function":        append(append([]string(nil), good...), scriptHeadLine(3, h.Suffix, "TestSwiftProofFuzz_abcdef0123456789_3")),
		"too many lines":        append(append([]string(nil), good...), make([]string, 20)...),
	} {
		if _, err := h.Normalize(rawStream(lines...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := h.Normalize([]byte(strings.Join(good, "\n"))); err == nil {
		t.Error("a stream without a final newline was accepted")
	}
}
