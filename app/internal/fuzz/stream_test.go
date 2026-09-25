package fuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Raw stream lines exactly as the harness writes them.
func beginLine(f, n int) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"begin","n":` + strconv.Itoa(n) + `}`
}

func endLine(f, n int) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"end","n":` + strconv.Itoa(n) + `}`
}

func stopLine(f, i int, x string) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"stop","i":` + strconv.Itoa(i) + `,"x":"` + x + `"}`
}

func sha(enc string) string {
	sum := sha256.Sum256([]byte(enc))
	return hex.EncodeToString(sum[:])
}

// obsLine records enc in full (it must fit the display bound).
func obsLine(f, i int, enc string, unstable bool) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"obs","i":` + strconv.Itoa(i) + `,"h":"` + sha(enc) + `","l":` + strconv.Itoa(len(enc)) +
		`,"o":` + jsonQuote(enc) + `,"p":false,"d":` + strconv.FormatBool(unstable) + `,"t":false}`
}

func rawStream(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// streamHarness renders a two-test harness: A(int) with 3 inputs and B(string)
// with 2.
func streamHarness(t *testing.T) Harness {
	t.Helper()
	h, err := Render(obsPlan(target("A", 1, 3, scalar("int")), target("B", 1, 2, scalar("string"))), renderOptions("abcdef12"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Tests[0].Inputs) != 3 || len(h.Tests[1].Inputs) != 2 {
		t.Fatalf("inputs %d %d", len(h.Tests[0].Inputs), len(h.Tests[1].Inputs))
	}
	return h
}

func completeLines() []string {
	return []string{
		beginLine(1, 3), obsLine(1, 0, "int(0)", false), obsLine(1, 1, "int(1)", false), obsLine(1, 2, `string("password=hunter2")`, false), endLine(1, 3),
		beginLine(2, 2), obsLine(2, 0, `string("")`, true), obsLine(2, 1, "panic(error(\"x\\ny\"))", false), endLine(2, 2),
	}
}

func TestNormalizeValidStream(t *testing.T) {
	h := streamHarness(t)
	results, err := h.Normalize(rawStream(completeLines()...))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseResults(results)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != StreamVersion || s.Display != h.Display || len(s.Functions) != 2 {
		t.Fatalf("stream %+v", s)
	}
	a := s.Functions[0]
	if a.Test != h.Tests[0].Name || a.State != StateComplete || a.At != -1 || a.Planned != 3 || len(a.Records) != 3 {
		t.Fatalf("A = %+v", a)
	}
	for i, r := range a.Records {
		if r.Call != h.Tests[0].Inputs[i].Call || r.Index != i {
			t.Fatalf("record %d = %+v", i, r)
		}
	}
	secret := a.Records[2]
	if secret.Display != `string("[REDACTED]")` || secret.SHA256 != sha(`string("password=hunter2")`) || secret.Length != len(`string("password=hunter2")`) {
		t.Fatalf("display must be redacted, hash and length kept: %+v", secret)
	}
	if strings.Contains(results, "hunter2") {
		t.Fatal("the secret reached the results")
	}
	b := s.Functions[1]
	if !b.Records[0].Unstable || b.Records[1].Display != "panic(error(\"x\\ny\"))" {
		t.Fatalf("B = %+v", b)
	}
	// The normalized form is canonical: parsing and normalizing again agree.
	again, _ := h.Normalize(rawStream(completeLines()...))
	if again != results {
		t.Fatal("normalization is not deterministic")
	}
}

func TestNormalizeRejectsMalformedStreams(t *testing.T) {
	h := streamHarness(t)
	lines := completeLines()
	with := func(i int, line string) []byte {
		copied := append([]string(nil), lines...)
		copied[i] = line
		return rawStream(copied...)
	}
	long := strings.Repeat("x", h.Display+1)
	cases := map[string][]byte{
		"empty":                   nil,
		"no final newline":        []byte(strings.Join(lines, "\n")),
		"invalid UTF-8":           with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha("a")+`","l":1,"o":"`+"\xff"+`","p":false,"d":false,"t":false}`),
		"unknown field":           with(0, `{"f":1,"k":"begin","n":3,"z":1}`),
		"extra key of a kind":     with(0, `{"f":1,"k":"begin","n":3,"i":0}`),
		"missing key":             with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha("int(0)")+`","l":6,"o":"int(0)","p":false,"d":false}`),
		"unknown kind":            with(0, `{"f":1,"k":"start","n":3}`),
		"not canonical spacing":   with(0, `{"f": 1,"k":"begin","n":3}`),
		"duplicate key":           with(0, `{"f":1,"f":1,"k":"begin","n":3}`),
		"other escape":            with(7, strings.Replace(lines[7], `\"x`, `"x`, 1)),
		"trailing data":           with(0, beginLine(1, 3)+` {}`),
		"out of order index":      with(1, obsLine(1, 1, "int(0)", false)),
		"duplicate index":         with(2, obsLine(1, 0, "int(1)", false)),
		"function out of range":   with(5, beginLine(3, 2)),
		"function 0":              with(0, beginLine(0, 3)),
		"missing begin":           rawStream(lines[1:]...),
		"planned count mismatch":  with(0, beginLine(1, 4)),
		"end count mismatch":      with(4, endLine(1, 2)),
		"uppercase hash":          with(1, strings.Replace(obsLine(1, 0, "int(0)", false), sha("int(0)"), strings.ToUpper(sha("int(0)")), 1)),
		"short hash":              with(1, strings.Replace(obsLine(1, 0, "int(0)", false), sha("int(0)"), sha("int(0)")[:32], 1)),
		"oversized display":       with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha(long)+`","l":`+strconv.Itoa(len(long))+`,"o":"`+long+`","p":false,"d":false,"t":false}`),
		"display longer than len": with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha("int(0)")+`","l":3,"o":"int(0)","p":false,"d":false,"t":false}`),
		"cut display that fits":   with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha("int(0)")+`","l":6,"o":"int(","p":false,"d":false,"t":false}`),
		"negative length":         with(1, `{"f":1,"k":"obs","i":0,"h":"`+sha("int(0)")+`","l":-1,"o":"","p":false,"d":false,"t":false}`),
		"record after end":        rawStream(append(append([]string(nil), lines[:5]...), obsLine(1, 3, "int(3)", false))...),
		"record after stop":       rawStream(beginLine(1, 3), stopLine(1, 0, "goexit"), obsLine(1, 0, "int(0)", false)),
		"begin while open":        rawStream(beginLine(1, 3), obsLine(1, 0, "int(0)", false), beginLine(2, 2)),
		"skipped function":        rawStream(beginLine(2, 2)),
		"unknown stop reason":     rawStream(beginLine(1, 3), stopLine(1, 0, "crash")),
		"stop index out of order": rawStream(beginLine(1, 3), stopLine(1, 1, "goexit")),
		"poisoned without timeout": rawStream(beginLine(1, 3), obsLine(1, 0, "int(0)", false), obsLine(1, 1, "int(1)", false), obsLine(1, 2, "int(2)", false), endLine(1, 3),
			beginLine(2, 2), stopLine(2, 0, "poisoned")),
		"observation after timeout": rawStream(beginLine(1, 3), stopLine(1, 0, "timeout"), beginLine(2, 2), obsLine(2, 0, `string("")`, false)),
		"goexit after timeout":      rawStream(beginLine(1, 3), stopLine(1, 0, "timeout"), beginLine(2, 2), stopLine(2, 0, "goexit")),
		"too many lines":            rawStream(append(append([]string(nil), lines...), lines...)...),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if results, err := h.Normalize(payload); err == nil {
				t.Fatalf("accepted: %s", results)
			}
		})
	}
}

func TestNormalizeRecordsEarlyEnds(t *testing.T) {
	h := streamHarness(t)
	// The process ended during the second input of A.
	results, err := h.Normalize(rawStream(beginLine(1, 3), obsLine(1, 0, "int(0)", false)))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := ParseResults(results)
	a, b := s.Functions[0], s.Functions[1]
	if a.State != StateInterrupted || a.At != 1 || a.AtCall != h.Tests[0].Inputs[1].Call || len(a.Records) != 1 {
		t.Fatalf("A = %+v", a)
	}
	if b.State != StateNotStarted || b.At != -1 || len(b.Records) != 0 {
		t.Fatalf("B = %+v", b)
	}
	// A timeout poisons the rest of the process.
	results, err = h.Normalize(rawStream(beginLine(1, 3), obsLine(1, 0, "int(0)", false), stopLine(1, 1, "timeout"), beginLine(2, 2), stopLine(2, 0, "poisoned")))
	if err != nil {
		t.Fatal(err)
	}
	s, _ = ParseResults(results)
	a, b = s.Functions[0], s.Functions[1]
	if a.State != StateStopped || a.Stop != StopTimeout || a.At != 1 || a.AtCall != h.Tests[0].Inputs[1].Call {
		t.Fatalf("A = %+v", a)
	}
	if b.State != StateStopped || b.Stop != StopPoisoned || b.At != 0 || b.AtCall != h.Tests[1].Inputs[0].Call {
		t.Fatalf("B = %+v", b)
	}
	// All records written but no end record: the process ended before it.
	results, err = h.Normalize(rawStream(beginLine(1, 3), obsLine(1, 0, "a", false), obsLine(1, 1, "b", false), obsLine(1, 2, "c", false)))
	if err != nil {
		t.Fatal(err)
	}
	s, _ = ParseResults(results)
	if a := s.Functions[0]; a.State != StateInterrupted || a.At != 3 || a.AtCall != "" {
		t.Fatalf("A = %+v", a)
	}
}

func TestParseResultsRejectsEditedStreams(t *testing.T) {
	h := streamHarness(t)
	results, err := h.Normalize(rawStream(completeLines()...))
	if err != nil {
		t.Fatal(err)
	}
	var s Stream
	if err := json.Unmarshal([]byte(results), &s); err != nil {
		t.Fatal(err)
	}
	edit := func(change func(*Stream)) string {
		var c Stream
		_ = json.Unmarshal([]byte(results), &c)
		change(&c)
		out, _ := json.Marshal(c)
		return string(out)
	}
	indented, _ := json.MarshalIndent(s, "", " ")
	cases := map[string]string{
		"empty":            "",
		"indented":         string(indented),
		"trailing data":    results + " ",
		"unknown field":    strings.Replace(results, `"v":1`, `"v":1,"x":2`, 1),
		"secret display":   edit(func(c *Stream) { c.Functions[0].Records[0].Display = "password=abc" }),
		"version":          edit(func(c *Stream) { c.Version = 2 }),
		"scheme":           edit(func(c *Stream) { c.Scheme = "other" }),
		"display bound":    edit(func(c *Stream) { c.Display = 1024 }),
		"test name":        edit(func(c *Stream) { c.Functions[0].Test = "TestOther" }),
		"duplicate test":   edit(func(c *Stream) { c.Functions[1].Test = c.Functions[0].Test }),
		"hash":             edit(func(c *Stream) { c.Functions[0].Records[0].SHA256 = "zz" }),
		"index":            edit(func(c *Stream) { c.Functions[0].Records[1].Index = 5 }),
		"empty call":       edit(func(c *Stream) { c.Functions[0].Records[1].Call = "" }),
		"long display":     edit(func(c *Stream) { c.Functions[0].Records[1].Display = strings.Repeat("y", c.Display+1) }),
		"missing record":   edit(func(c *Stream) { c.Functions[0].Records = c.Functions[0].Records[:2] }),
		"null records":     edit(func(c *Stream) { c.Functions[0].Records = nil }),
		"stop on complete": edit(func(c *Stream) { c.Functions[0].Stop = StopTimeout }),
		"unknown state":    edit(func(c *Stream) { c.Functions[0].State = "done" }),
		"started after the end": edit(func(c *Stream) {
			c.Functions[0].State, c.Functions[0].Records, c.Functions[0].At = StateNotStarted, []Record{}, -1
		}),
		"no functions":      edit(func(c *Stream) { c.Functions = []FunctionStream{} }),
		"planned too large": edit(func(c *Stream) { c.Functions[0].Planned = MaxPackageInputs + 1 }),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseResults(text); err == nil {
				t.Fatalf("accepted %s", text)
			}
		})
	}
	if _, err := ParseResults(edit(func(*Stream) {})); err != nil {
		t.Fatalf("an unedited round trip must parse: %v", err)
	}
}
