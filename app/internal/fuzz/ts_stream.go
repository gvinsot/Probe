package fuzz

// The observation stream of a TS/JS harness (F2c, Appendix D.13). It is the
// stream of a Go harness with one addition per function: a head record, the
// first record of the function, carries the per-run suffix and the test name,
// and a done record, the last one, repeats the test name with the number of
// observations written. The test framework's log is never read: the named
// execution of a test is established by its head and done records in the
// payload of a run that exited 0, and the normalized stream records which
// functions have both (their state is complete or stopped).

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

// scriptHeadLine and scriptDoneLine are the only accepted forms of the head
// and done records of function f.
func scriptHeadLine(f int, suffix, test string) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"head","s":` + jsonQuote(suffix) + `,"w":` + jsonQuote(test) + `}`
}

func scriptDoneLine(f int, test string, observed int) string {
	return `{"f":` + strconv.Itoa(f) + `,"k":"done","w":` + jsonQuote(test) + `,"n":` + strconv.Itoa(observed) + `}`
}

// normalizeScript is Normalize for a TS/JS harness. Validation is strict and
// all-or-nothing, as for Go: every function appears in harness order and
// opens with its head record, then its begin record; its records follow the
// Go rules; after its end or stop record comes its done record, and only then
// the head record of the next function. A function whose records end before
// its done record is interrupted, even after an end or stop record: without
// the done record, nothing shows that its test finished. The normalized
// stream is marked with the runner jest_json.
func (h Harness) normalizeScript(payload []byte) (string, error) {
	if len(h.Tests) == 0 || len(h.Tests) > maxHarnessTests {
		return "", errors.New("the harness has no fuzz test")
	}
	if len(payload) == 0 {
		return "", errors.New("the observation stream is empty")
	}
	if !utf8.Valid(payload) {
		return "", errors.New("the observation stream is not valid UTF-8")
	}
	if payload[len(payload)-1] != '\n' {
		return "", errors.New("the observation stream does not end with a complete line")
	}
	limit := 0
	for _, t := range h.Tests {
		limit += len(t.Inputs) + 5
	}
	lines := strings.Split(string(payload[:len(payload)-1]), "\n")
	if len(lines) > limit {
		return "", fmt.Errorf("the observation stream has more than %d lines", limit)
	}
	functions := make([]FunctionStream, len(h.Tests))
	for i, t := range h.Tests {
		functions[i] = FunctionStream{Test: t.Name, Planned: len(t.Inputs), State: StateNotStarted, At: -1, Records: []Record{}}
	}
	// phase of the current function: after its head, while its records are
	// open, after its end or stop (closed), or between functions ("").
	const (
		between = ""
		headed  = "head"
		open    = "open"
		closed  = "closed"
	)
	phase, current := between, 0
	closing := FunctionStream{} // state, stop and position set by the end or stop record
	timedOut := false
	for number, line := range lines {
		fail := func(format string, args ...any) (string, error) {
			return "", fmt.Errorf("line %d: "+format, append([]any{number + 1}, args...)...)
		}
		switch phase {
		case between:
			next := current + 1
			if next > len(h.Tests) || line != scriptHeadLine(next, h.Suffix, h.Tests[next-1].Name) {
				return fail("expected the head record of function %d", next)
			}
			current, phase = next, headed
			functions[current-1].State = ""
			continue
		case closed:
			fn := &functions[current-1]
			if line != scriptDoneLine(current, fn.Test, len(fn.Records)) {
				return fail("expected the done record of function %d", current)
			}
			fn.State, fn.Stop, fn.At, fn.AtCall = closing.State, closing.Stop, closing.At, closing.AtCall
			phase = between
			continue
		}
		rec, kind, err := parseRawLine(line)
		if err != nil {
			return fail("%v", err)
		}
		if *rec.F != current {
			return fail("record of function %d outside its head and done records", *rec.F)
		}
		fn := &functions[current-1]
		test := h.Tests[current-1]
		if phase == headed {
			if kind != "begin" {
				return fail("function %d does not begin after its head record", current)
			}
			if *rec.N != len(test.Inputs) {
				return fail("function %d plans %d inputs, not %d", current, *rec.N, len(test.Inputs))
			}
			phase = open
			continue
		}
		switch kind {
		case "begin":
			return fail("function %d begins twice", current)
		case "obs":
			i := *rec.I
			if timedOut {
				return fail("observation after a timeout")
			}
			if i != len(fn.Records) || i >= fn.Planned {
				return fail("observation index %d out of order", i)
			}
			o, l := *rec.O, *rec.L
			if !hashPattern.MatchString(*rec.H) || l < 0 || l > MaxEncodingBytes {
				return fail("malformed observation")
			}
			if len(o) > h.Display || len(o) > l || l <= h.Display && len(o) != l {
				return fail("display value does not match the recorded length")
			}
			fn.Records = append(fn.Records, Record{
				Index: i, Call: test.Inputs[i].Call, SHA256: *rec.H, Length: l,
				Display: displayValue(o, h.Display), Panicked: *rec.P, Unstable: *rec.D, Truncated: *rec.T,
			})
		case "stop":
			i, x := *rec.I, *rec.X
			if i != len(fn.Records) || i >= fn.Planned {
				return fail("stop index %d out of order", i)
			}
			switch x {
			case StopPoisoned:
				if !timedOut || i != 0 {
					return fail("poisoned function without an earlier timeout")
				}
			case StopTimeout, StopAbort:
				if timedOut {
					return fail("stop after a timeout that is not a poisoned stop")
				}
			default:
				return fail("unknown stop reason")
			}
			if x == StopTimeout {
				timedOut = true
			}
			closing = FunctionStream{State: StateStopped, Stop: x, At: i, AtCall: test.Inputs[i].Call}
			phase = closed
		case "end":
			if timedOut || *rec.N != fn.Planned || len(fn.Records) != fn.Planned {
				return fail("end record before every planned input was observed")
			}
			closing = FunctionStream{State: StateComplete, At: -1}
			phase = closed
		}
	}
	if phase != between {
		fn := &functions[current-1]
		fn.State, fn.Stop, fn.At, fn.AtCall = StateInterrupted, "", len(fn.Records), ""
		if fn.At < fn.Planned {
			fn.AtCall = h.Tests[current-1].Inputs[fn.At].Call
		}
	}
	out, err := encodeStream(Stream{Version: StreamVersion, Scheme: model.FuzzSeedScheme, Display: h.Display, Runner: harness.RunnerJest, Functions: functions})
	if err != nil {
		return "", err
	}
	results := string(out)
	if _, err := ParseResults(results); err != nil {
		return "", err
	}
	return results, nil
}

// StartedTests returns how many fuzz tests of a normalized TS/JS stream wrote
// their head record, and whether the stream ended inside one of them (the
// last started test is interrupted: no done record), or 0 and false when
// results do not parse. The harness uses it to tell a baseline run that did
// not start the harness (none started), a passing baseline run that did not
// run every test (fewer than all, the stream ended between two tests), and a
// baseline run whose process ended while it evaluated an input (inside).
func StartedTests(results string) (started int, inside bool) {
	s, err := ParseResults(results)
	if err != nil {
		return 0, false
	}
	for _, f := range s.Functions {
		if f.State != StateNotStarted {
			started++
			inside = f.State == StateInterrupted
		}
	}
	return started, inside
}
