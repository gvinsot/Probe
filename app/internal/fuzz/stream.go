package fuzz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// StreamVersion is the version of the normalized stream kept in Check.Results.
const StreamVersion = 1

// Function states of a normalized stream.
const (
	StateComplete    = "complete"    // an end record: every planned input has a record
	StateStopped     = "stopped"     // a stop record ended the function early
	StateInterrupted = "interrupted" // the records end without an end or stop record: the process ended
	StateNotStarted  = "not_started" // the function has no record
)

// Stop reasons of a stop record.
const (
	StopTimeout  = "timeout"  // one evaluation exceeded call_timeout_ms; later functions of the process are poisoned
	StopGoexit   = "goexit"   // the called function ended its goroutine (runtime.Goexit)
	StopAbort    = "abort"    // the evaluation panicked outside the called function
	StopPoisoned = "poisoned" // not evaluated: an earlier evaluation of the process timed out
)

// maxHarnessTests bounds the functions of one harness (fuzz.max_functions is
// at most 32).
const maxHarnessTests = 32

// Record is one observed input of one revision. SHA256 and Length describe the
// full canonical encoding computed in the sandbox (bounded to
// MaxEncodingBytes, Truncated when a bound was reached); comparison uses only
// them. Display is a redacted cut of that encoding for people, and Call is the
// host-rendered input.
type Record struct {
	Index     int    `json:"i"`
	Call      string `json:"c"`
	SHA256    string `json:"h"`
	Length    int    `json:"l"`
	Display   string `json:"o"`
	Panicked  bool   `json:"p"`
	Unstable  bool   `json:"d"` // the two in-process evaluations differed
	Truncated bool   `json:"t"`
}

// FunctionStream is the normalized stream of one fuzz test.
type FunctionStream struct {
	Test    string   `json:"test"`
	Planned int      `json:"planned"`
	State   string   `json:"state"`
	Stop    string   `json:"stop"`    // stop reason when State is stopped
	At      int      `json:"at"`      // index at which evaluation ended (stopped, interrupted), else -1
	AtCall  string   `json:"at_call"` // the input at At, when there is one
	Records []Record `json:"records"`
}

// Stream is the normalized observation stream of one run: the Check.Results
// of a fuzz check. It is canonical JSON in the layout of encodeStream and a
// fixed point of redaction.
type Stream struct {
	Version   int              `json:"v"`
	Scheme    string           `json:"scheme"`
	Display   int              `json:"display"`
	Functions []FunctionStream `json:"functions"`
}

// Function returns the stream of the named test.
func (s Stream) Function(test string) (FunctionStream, bool) {
	for _, f := range s.Functions {
		if f.Test == test {
			return f, true
		}
	}
	return FunctionStream{}, false
}

var (
	hashPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	testNamePattern = regexp.MustCompile(`^TestSwiftProofFuzz_[0-9a-f]{8,32}_[1-9][0-9]?$`)
)

// rawRecord is one line of the in-container stream. Pointer fields tell which
// keys were present; each kind allows exactly its own keys.
type rawRecord struct {
	F *int    `json:"f"`
	K *string `json:"k"`
	N *int    `json:"n"`
	I *int    `json:"i"`
	H *string `json:"h"`
	L *int    `json:"l"`
	O *string `json:"o"`
	P *bool   `json:"p"`
	D *bool   `json:"d"`
	T *bool   `json:"t"`
	X *string `json:"x"`
}

// Normalize validates the raw observation stream a run of this harness
// returned and converts it to its normalized form. Validation is strict and
// all-or-nothing: every line must be the byte-exact canonical form of one
// record the harness writes, functions must appear in harness order, each
// beginning with its planned count, observation indices must be consecutive
// from 0, and nothing may follow an end or stop record of a function. After a
// timeout every later function may only be poisoned. Any violation rejects the
// whole stream; nothing is partially trusted.
//
// Display values are redacted and cut to the harness's display bound (see
// displayValue); hashes were computed in the sandbox over the full encodings,
// so redacting a display never changes a comparison. The result must be a
// fixed point of redaction, or it is rejected too.
func (h Harness) Normalize(payload []byte) (string, error) {
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
		limit += len(t.Inputs) + 3
	}
	lines := strings.Split(string(payload[:len(payload)-1]), "\n")
	if len(lines) > limit {
		return "", fmt.Errorf("the observation stream has more than %d lines", limit)
	}
	functions := make([]FunctionStream, len(h.Tests))
	for i, t := range h.Tests {
		functions[i] = FunctionStream{Test: t.Name, Planned: len(t.Inputs), State: StateNotStarted, At: -1, Records: []Record{}}
	}
	current := 0 // 1-based index of the function whose records are being read
	open := false
	timedOut := false
	for number, line := range lines {
		rec, kind, err := parseRawLine(line)
		if err != nil {
			return "", fmt.Errorf("line %d: %v", number+1, err)
		}
		f := *rec.F
		fail := func(format string, args ...any) (string, error) {
			return "", fmt.Errorf("line %d: "+format, append([]any{number + 1}, args...)...)
		}
		if kind == "begin" {
			if open || f != current+1 || f > len(h.Tests) {
				return fail("function %d begins out of order", f)
			}
			if *rec.N != len(h.Tests[f-1].Inputs) {
				return fail("function %d plans %d inputs, not %d", f, *rec.N, len(h.Tests[f-1].Inputs))
			}
			current, open = f, true
			functions[f-1].State = ""
			continue
		}
		if !open || f != current {
			return fail("record of function %d outside its begin and end", f)
		}
		fn := &functions[f-1]
		test := h.Tests[f-1]
		switch kind {
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
			case StopTimeout, StopGoexit, StopAbort:
				if timedOut {
					return fail("stop after a timeout that is not a poisoned stop")
				}
			default:
				return fail("unknown stop reason")
			}
			if x == StopTimeout {
				timedOut = true
			}
			fn.State, fn.Stop, fn.At, fn.AtCall = StateStopped, x, i, test.Inputs[i].Call
			open = false
		case "end":
			if timedOut || *rec.N != fn.Planned || len(fn.Records) != fn.Planned {
				return fail("end record before every planned input was observed")
			}
			fn.State = StateComplete
			open = false
		}
	}
	if open {
		fn := &functions[current-1]
		fn.State, fn.At = StateInterrupted, len(fn.Records)
		if fn.At < fn.Planned {
			fn.AtCall = h.Tests[current-1].Inputs[fn.At].Call
		}
	}
	out, err := encodeStream(Stream{Version: StreamVersion, Scheme: model.FuzzSeedScheme, Display: h.Display, Functions: functions})
	if err != nil {
		return "", err
	}
	// ParseResults also requires the fixed point of redaction.
	results := string(out)
	if _, err := ParseResults(results); err != nil {
		return "", err
	}
	return results, nil
}

// encodeStream is the canonical layout of a normalized stream: JSON with every
// object member and array element on its own line and no indentation
// (json.MarshalIndent with empty prefix and indent).
//
// The layout keeps the redaction rules from matching across fields. Every
// variable-length class of the rules that could run over JSON punctuation
// stops at whitespace (the URL-credential rule) or at a quote (the others),
// and every string member but the last of its object is followed by a comma
// and a line break. A match can therefore only lie inside one JSON string, and
// every string the host stores (calls, displays) is checked to be a fixed
// point of redaction in its JSON-escaped form (streamSafe). With compact JSON,
// a display ending in "http://host" followed by a later record holding an "@"
// would form one match and reject a stream whose every value is harmless.
func encodeStream(s Stream) ([]byte, error) {
	return json.MarshalIndent(s, "", "")
}

// streamSafe reports whether redaction leaves s unchanged both as it is and in
// the JSON-escaped form it takes inside a normalized stream. Escaping can make
// a match: `[]string{"Password:"}` is a fixed point, while its escaped form
// `[]string{\"Password:\"}` is not, because the escaping backslash completes
// the secret-assignment rule.
func streamSafe(s string) bool {
	if !redact.IsFixedPoint(s) {
		return false
	}
	quoted, err := json.Marshal(s)
	return err == nil && redact.IsFixedPoint(string(quoted))
}

// displayValue redacts a display value and cuts it to limit bytes of valid
// UTF-8. A cut of a redaction fixed point is still one. A display whose
// JSON-escaped form redaction would still alter is replaced by redact.Marker
// as a whole: displays are for people only, and comparisons use the hashes.
func displayValue(o string, limit int) string {
	o = redact.Redact(o)
	if len(o) > limit {
		o = o[:limit]
		for len(o) > 0 && !utf8.ValidString(o) {
			o = o[:len(o)-1]
		}
	}
	// o is a fixed point already (Redact returns one, and cutting keeps it
	// one); only its escaped form remains to be checked.
	if quoted, err := json.Marshal(o); err != nil || !redact.IsFixedPoint(string(quoted)) {
		return redact.Marker
	}
	return o
}

// Whole reports whether Display is the whole recorded encoding: the encoding
// reached no bound in the sandbox, the display was not cut, and it holds no
// redaction marker (a display that redaction changed always holds one). When
// it is false, two records with different hashes may show identical displays.
func (r Record) Whole() bool {
	return !r.Truncated && r.Length == len(r.Display) && !strings.Contains(r.Display, redact.Marker)
}

// parseRawLine decodes one in-container record and requires the line to be
// the byte-exact canonical form of that record.
func parseRawLine(line string) (rawRecord, string, error) {
	var rec rawRecord
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, "", errors.New("not a stream record")
	}
	if _, err := dec.Token(); err != io.EOF {
		return rec, "", errors.New("trailing data after the record")
	}
	if rec.F == nil || rec.K == nil || *rec.F < 1 {
		return rec, "", errors.New("record without a function index or kind")
	}
	kind := *rec.K
	has := func(present ...bool) bool {
		for _, p := range present {
			if !p {
				return false
			}
		}
		return true
	}
	var canonical string
	head := `{"f":` + strconv.Itoa(*rec.F) + `,"k":`
	switch kind {
	case "begin", "end":
		if !has(rec.N != nil) || rec.I != nil || rec.H != nil || rec.L != nil || rec.O != nil || rec.P != nil || rec.D != nil || rec.T != nil || rec.X != nil {
			return rec, "", errors.New("malformed " + kind + " record")
		}
		canonical = head + `"` + kind + `","n":` + strconv.Itoa(*rec.N) + `}`
	case "obs":
		if !has(rec.I != nil, rec.H != nil, rec.L != nil, rec.O != nil, rec.P != nil, rec.D != nil, rec.T != nil) || rec.N != nil || rec.X != nil {
			return rec, "", errors.New("malformed observation record")
		}
		canonical = head + `"obs","i":` + strconv.Itoa(*rec.I) + `,"h":"` + *rec.H + `","l":` + strconv.Itoa(*rec.L) +
			`,"o":` + jsonQuote(*rec.O) + `,"p":` + strconv.FormatBool(*rec.P) + `,"d":` + strconv.FormatBool(*rec.D) +
			`,"t":` + strconv.FormatBool(*rec.T) + `}`
	case "stop":
		if !has(rec.I != nil, rec.X != nil) || rec.N != nil || rec.H != nil || rec.L != nil || rec.O != nil || rec.P != nil || rec.D != nil || rec.T != nil {
			return rec, "", errors.New("malformed stop record")
		}
		canonical = head + `"stop","i":` + strconv.Itoa(*rec.I) + `,"x":` + jsonQuote(*rec.X) + `}`
	default:
		return rec, "", errors.New("unknown record kind")
	}
	if canonical != line {
		return rec, "", errors.New("record is not in canonical form")
	}
	return rec, kind, nil
}

// jsonQuote is the host copy of the harness's JSON string encoder: quote,
// backslash and control bytes are escaped, every other byte is copied.
func jsonQuote(s string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ParseResults parses the normalized stream of a fuzz check (Check.Results)
// and checks every invariant Normalize establishes: canonical JSON in the
// layout of encodeStream, a fixed point of redaction, consecutive indices,
// well-formed hashes and consistent function states. A stream whose structure
// was edited by hand therefore fails here instead of being partially trusted;
// an edit that keeps every invariant (a changed display, for example) is not
// detected.
func ParseResults(results string) (Stream, error) {
	if results == "" {
		return Stream{}, errors.New("no observation stream")
	}
	if !redact.IsFixedPoint(results) {
		return Stream{}, errors.New("the observation stream is not a fixed point of redaction")
	}
	var s Stream
	dec := json.NewDecoder(strings.NewReader(results))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Stream{}, errors.New("the observation stream is not valid")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Stream{}, errors.New("trailing data after the observation stream")
	}
	canonical, err := encodeStream(s)
	if err != nil || !bytes.Equal(canonical, []byte(results)) {
		return Stream{}, errors.New("the observation stream is not in canonical form")
	}
	if err := s.validate(); err != nil {
		return Stream{}, err
	}
	return s, nil
}

func (s Stream) validate() error {
	if s.Version != StreamVersion || s.Scheme != model.FuzzSeedScheme {
		return errors.New("unknown observation stream version")
	}
	if s.Display < minDisplayBytes || s.Display > MaxDisplayBytes {
		return errors.New("invalid display bound")
	}
	if len(s.Functions) == 0 || len(s.Functions) > maxHarnessTests {
		return errors.New("invalid number of functions")
	}
	seen := map[string]bool{}
	total := 0
	ended := false    // a function was interrupted or not started: nothing later may have begun
	timedOut := false // a function stopped on a timeout: later ones may only be poisoned
	for _, f := range s.Functions {
		if !testNamePattern.MatchString(f.Test) || seen[f.Test] {
			return fmt.Errorf("invalid or duplicate test name %q", f.Test)
		}
		seen[f.Test] = true
		if f.Planned < 1 || f.Planned > MaxPackageInputs || f.Records == nil || len(f.Records) > f.Planned {
			return fmt.Errorf("%s: invalid planned count or records", f.Test)
		}
		total += f.Planned
		for i, r := range f.Records {
			if r.Index != i || r.Call == "" || !hashPattern.MatchString(r.SHA256) || r.Length < 0 || r.Length > MaxEncodingBytes ||
				len(r.Display) > s.Display || !utf8.ValidString(r.Display) || !utf8.ValidString(r.Call) {
				return fmt.Errorf("%s: invalid record %d", f.Test, i)
			}
		}
		n := len(f.Records)
		switch f.State {
		case StateComplete:
			if n != f.Planned || f.Stop != "" || f.At != -1 || f.AtCall != "" || timedOut {
				return fmt.Errorf("%s: inconsistent complete function", f.Test)
			}
		case StateStopped:
			if f.At != n || f.At >= f.Planned || f.AtCall == "" {
				return fmt.Errorf("%s: inconsistent stopped function", f.Test)
			}
			switch f.Stop {
			case StopPoisoned:
				if !timedOut || n != 0 {
					return fmt.Errorf("%s: poisoned function without an earlier timeout", f.Test)
				}
			case StopTimeout, StopGoexit, StopAbort:
				if timedOut {
					return fmt.Errorf("%s: stop after a timeout", f.Test)
				}
			default:
				return fmt.Errorf("%s: unknown stop reason", f.Test)
			}
			if f.Stop == StopTimeout {
				timedOut = true
			}
		case StateInterrupted:
			if f.Stop != "" || f.At != n || (f.At < f.Planned) != (f.AtCall != "") || timedOut && n != 0 {
				return fmt.Errorf("%s: inconsistent interrupted function", f.Test)
			}
		case StateNotStarted:
			if n != 0 || f.Stop != "" || f.At != -1 || f.AtCall != "" {
				return fmt.Errorf("%s: inconsistent function that did not start", f.Test)
			}
		default:
			return fmt.Errorf("%s: unknown function state", f.Test)
		}
		if ended && f.State != StateNotStarted {
			return fmt.Errorf("%s: records after the process ended", f.Test)
		}
		if f.State == StateInterrupted || f.State == StateNotStarted {
			ended = true
		}
	}
	if total > MaxPackageInputs {
		return errors.New("the observation stream plans too many inputs")
	}
	return nil
}
