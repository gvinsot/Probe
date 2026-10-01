package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/observe"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// realAttrLog is go test -json output captured from golang:1.26-bookworm
// (go1.26.8) for a test that records observations with t.Attr, including a
// subtest, a non-prefixed key, a plain "=== ATTR" line printed by the code
// under test (an output event), a framed line printed by the code under test
// (which test2json does convert), a value too long for test2json's line limit
// (left as output events), an empty value (no "Value" field) and a failing
// test whose key contains whitespace.
const realAttrLog = `{"Time":"2026-09-25T22:15:27.364236596Z","Action":"start","Package":"example.com/m/pkg"}
{"Time":"2026-09-25T22:15:27.366116976Z","Action":"run","Package":"example.com/m/pkg","Test":"TestObserve"}
{"Time":"2026-09-25T22:15:27.366125097Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"=== RUN   TestObserve\n"}
{"Time":"2026-09-25T22:15:27.366140325Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve","Key":"probe.v","Value":"1"}
{"Time":"2026-09-25T22:15:27.366142516Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"=== ATTR  TestObserve probe.v 1\n"}
{"Time":"2026-09-25T22:15:27.366143993Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"=== ATTR  TestObserve probe.fake 9\n"}
{"Time":"2026-09-25T22:15:27.366147984Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve","Key":"probe.forged","Value":"7"}
{"Time":"2026-09-25T22:15:27.366148989Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"=== ATTR  TestObserve probe.forged 7\n"}
{"Time":"2026-09-25T22:15:27.366153813Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"\u0016=== ATTR  TestObserve probe.long xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
{"Time":"2026-09-25T22:15:27.366156065Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
{"Time":"2026-09-25T22:15:27.36616495Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve","Key":"probe.q","Value":"a\"b\\c"}
{"Time":"2026-09-25T22:15:27.366165965Z","Action":"output","Package":"example.com/m/pkg","Test":"TestObserve","Output":"=== ATTR  TestObserve probe.q a\"b\\c\n"}
{"Time":"2026-09-25T22:15:27.366167131Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve","Key":"probe.after","Value":"ok"}
{"Time":"2026-09-25T22:15:27.366169272Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve","Key":"other","Value":"z"}
{"Time":"2026-09-25T22:15:27.366420427Z","Action":"run","Package":"example.com/m/pkg","Test":"TestObserve/sub"}
{"Time":"2026-09-25T22:15:27.366505177Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestObserve/sub","Key":"probe.sub","Value":"s"}
{"Time":"2026-09-25T22:15:27.366610713Z","Action":"pass","Package":"example.com/m/pkg","Test":"TestObserve/sub","Elapsed":0}
{"Time":"2026-09-25T22:15:27.366652988Z","Action":"pass","Package":"example.com/m/pkg","Test":"TestObserve","Elapsed":0}
{"Time":"2026-09-25T22:15:27.366706469Z","Action":"run","Package":"example.com/m/pkg","Test":"TestWS"}
{"Time":"2026-09-25T22:15:27.366742422Z","Action":"output","Package":"example.com/m/pkg","Test":"TestWS","Output":"    testing.go:1606: disallowed whitespace in attribute key \"probe.ws key\"\n"}
{"Time":"2026-09-25T22:15:27.366768876Z","Action":"fail","Package":"example.com/m/pkg","Test":"TestWS","Elapsed":0}
{"Time":"2026-09-25T22:15:27.366929723Z","Action":"run","Package":"example.com/m/pkg","Test":"TestEmpty"}
{"Time":"2026-09-25T22:15:27.366965274Z","Action":"attr","Package":"example.com/m/pkg","Test":"TestEmpty","Key":"probe.e"}
{"Time":"2026-09-25T22:15:27.367074448Z","Action":"pass","Package":"example.com/m/pkg","Test":"TestEmpty","Elapsed":0}
`

func goSetValues(t *testing.T, s observe.Set, test string) map[string]string {
	t.Helper()
	o := observe.Compare(s, s, nil)
	out := map[string]string{}
	for _, row := range o.Observations {
		if row.Test != test {
			continue
		}
		v := row.Base
		if row.Status == model.ObservationIncomparable {
			v = "INCOMPARABLE:" + row.Reason
		}
		out[row.Key] = v
	}
	return out
}

func TestExtractGoObservationsFromRealEvents(t *testing.T) {
	s := extractGoObservations(realAttrLog, []string{"TestObserve"})
	if s.Err() != "" {
		t.Fatalf("channel error %q", s.Err())
	}
	got := goSetValues(t, s, "TestObserve")
	want := map[string]string{
		"v":      "1",
		"forged": "7", // a framed line printed by the code under test is an attr event
		"q":      `a"b\c`,
		"after":  "ok",
		"long":   "INCOMPARABLE:" + reasonLineLimit,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observations %v, want %v (plain \"=== ATTR\" output, non-prefixed keys and subtests must be ignored)", got, want)
	}
	if empty := extractGoObservations(realAttrLog, []string{"TestEmpty"}); !reflect.DeepEqual(goSetValues(t, empty, "TestEmpty"), map[string]string{"e": ""}) {
		t.Fatalf("an attr event without a Value field is an empty value: %v", goSetValues(t, empty, "TestEmpty"))
	}
	for _, names := range [][]string{{"TestWS"}, {"TestOther"}, nil} {
		if s := extractGoObservations(realAttrLog, names); !s.Empty() {
			t.Errorf("names %v recorded %d keys", names, s.Len())
		}
	}
	// An attr line that no longer parses, for example after redaction, fails
	// the whole set; so does an attr event without a key.
	for _, line := range []string{
		`{"Action":"attr","Test":"TestObserve","Key":"probe.x","Value":"[REDACTED]`,
		`{"Action":"attr","Test":"TestObserve","Value":"1"}`,
	} {
		broken := extractGoObservations(realAttrLog+line+"\n", []string{"TestObserve"})
		if broken.Err() == "" {
			t.Errorf("%s did not fail the set", line)
		}
	}
	// Unrelated non-JSON lines are ignored.
	if s := extractGoObservations("FAIL\tpkg\nsomething else\n"+realAttrLog, []string{"TestObserve"}); s.Err() != "" || s.Len() != 5 {
		t.Fatalf("non-JSON lines disturbed extraction: %q, %d keys", s.Err(), s.Len())
	}
}

// goLog renders the events of one run of a generated Go test that recorded
// the given key/value pairs, ending with action ("pass" or "fail").
func goLog(name, action string, pairs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/m/pkg","Test":%q}`+"\n", name)
	for i := 0; i+1 < len(pairs); i += 2 {
		event := map[string]string{"Action": "attr", "Package": "example.com/m/pkg", "Test": name, "Key": observe.GoKeyPrefix + pairs[i]}
		if pairs[i+1] != "" {
			event["Value"] = pairs[i+1]
		}
		line, _ := json.Marshal(event)
		b.Write(line)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, `{"Action":%q,"Package":"example.com/m/pkg","Test":%q}`+"\n", action, name)
	return b.String()
}

func TestForgedFramingDuplicateNeverYieldsNotDiverged(t *testing.T) {
	names := []string{"TestProbeObserve"}
	base := extractGoObservations(goLog(names[0], "pass", "Discount(5,33)", "4", "Discount(0,50)", "0"), names)
	forged := extractGoObservations(goLog(names[0], "pass", "Discount(5,33)", "4", "Discount(5,33)", "4", "Discount(0,50)", "0"), names)
	o := observe.Compare(base, forged, nil)
	if o.Status != model.StatusUnverified || o.NeedsRepeat {
		t.Fatalf("a forged duplicate gave %s", o.Status)
	}
	if row := o.Observations[1]; row.Key != "Discount(5,33)" || row.Status != model.ObservationIncomparable {
		t.Fatalf("row %+v", row)
	}
}

func TestNormalizeJestMeta(t *testing.T) {
	meta := func(raw string) *jestMeta { return &jestMeta{Observations: json.RawMessage(raw)} }
	marker := func(text string) string { b, _ := json.Marshal(text); return string(b) }
	var many []string
	for i := 0; i <= observe.MaxKeys; i++ {
		many = append(many, fmt.Sprintf(`"k%02d":1`, i))
	}
	longValue := `"` + strings.Repeat("<", observe.MaxValueBytes) + `"` // 1026 bytes as canonical text
	longKey := strings.Repeat("k", observe.MaxKeyBytes+1)
	standIn := func(text string) string { return marker(observe.Oversized(text)) }
	for _, tc := range []struct {
		name string
		in   *jestMeta
		want string // "" means nil
	}{
		{"nil", nil, ""},
		{"no observations", &jestMeta{}, ""},
		{"null observations", meta("null"), ""},
		{"strings kept", meta(`{"b":"2","a":"1"}`), `{"a":"1","b":"2"}`},
		{"canonical values keep their type", meta(`{"n":5,"f":0.10,"big":12345678901234567890,"o":{"b":2,"a":[1,"x"]},"nul":null,"t":true,"s":"<x>","ns":"5"}`), `{"big":12345678901234567890,"f":0.10,"n":5,"ns":"5","nul":null,"o":{"a":[1,"x"],"b":2},"s":"<x>","t":true}`},
		{"redacted values", meta(`{"k":"password=abc"}`), `{"k":"[REDACTED]"}`},
		{"non-string value altered by redaction", meta(`{"k":{"password":"abc"}}`), `{"k":"[REDACTED]"}`},
		{"value too long to compare", meta(`{"k":` + longValue + `}`), `{"k":` + standIn(longValue) + `}`},
		{"value that fits", meta(`{"k":"` + strings.Repeat("<", observe.MaxValueBytes-2) + `"}`), `{"k":"` + strings.Repeat("<", observe.MaxValueBytes-2) + `"}`},
		{"key too long to compare", meta(`{"` + longKey + `":1}`), `{` + standIn(longKey) + `:1}`},
		{"credential-like key", meta(`{"password":"abc"}`), marker(metaUnstable)},
		{"keys equal after redaction", meta(`{"sk-aaaaaaaaaaaa":"1","sk-bbbbbbbbbbbb":"2"}`), marker(metaCollision)},
		{"too many keys", meta("{" + strings.Join(many, ",") + "}"), marker(metaTooMany)},
		{"string", meta(`"forged"`), marker(metaNotObject)},
		{"known marker", meta(marker(metaDropped)), marker(metaDropped)},
		{"size marker", meta(marker(metaTooLarge)), marker(metaTooLarge)},
		{"array", meta(`[1,2]`), marker(metaNotObject)},
		{"number", meta(`5`), marker(metaNotObject)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeJestMeta(tc.in)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %s, want nil", got.Observations)
				}
				return
			}
			if got == nil || string(got.Observations) != tc.want {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
			if !redact.IsFixedPoint(string(got.Observations)) {
				t.Fatalf("%s is not a Redact fixed point", got.Observations)
			}
			if again := normalizeJestMeta(got); again == nil || string(again.Observations) != string(got.Observations) {
				t.Fatalf("not idempotent: %v then %v", got, again)
			}
		})
	}
}

// jestReportWithMeta renders a Jest-compatible report for one file with one
// top-level assertion carrying the raw meta JSON (omitted when "").
func jestReportWithMeta(file, title, status, meta string) string {
	assertion := fmt.Sprintf(`{"ancestorTitles":[],"fullName":%q,"status":%q,"title":%q,"duration":1.5,"failureMessages":[]`, title, status, title)
	if meta != "" {
		assertion += `,"meta":` + meta
	}
	assertion += "}"
	return fmt.Sprintf(`{"numTotalTests":1,"testResults":[{"assertionResults":[%s],"status":"passed","message":"","name":"/workspace/%s"}]}`, assertion, file)
}

// An unexpected meta shape never makes a report unreadable or changes the
// validated execution; it only makes the observations a channel error.
func TestJestMetaNeverBreaksValidation(t *testing.T) {
	const path, title = "src/cart.test.ts", "observe total"
	for _, tc := range []struct {
		meta    string
		values  map[string]string
		failure bool
	}{
		{"", nil, false},
		{`{}`, nil, false},
		{`null`, nil, false},
		{`5`, nil, true},
		{`"x"`, nil, true},
		{`[{"probe":{}}]`, nil, true},
		{`{"other":1}`, nil, false},
		{`{"probe":[1]}`, nil, true},
		{`{"probe":"forged"}`, nil, true},
		{`{"probe":{"total([10],0.5)":5,"obj":{"b":2,"a":1},"nan":null,"label":"<b>5</b>"}}`, map[string]string{"total([10],0.5)": "5", "obj": `{"a":1,"b":2}`, "nan": "null", "label": `"<b>5</b>"`}, false},
	} {
		results, err := normalizeJestReport([]byte(jestReportWithMeta(path, title, "passed", tc.meta)))
		if err != nil || !redact.IsFixedPoint(results) {
			t.Fatalf("meta %s: %v (fixed point %v)", tc.meta, err, redact.IsFixedPoint(results))
		}
		c := ValidateJestExecution(model.Check{Status: "PASS", Results: results}, path, []string{title})
		if c.Status != "PASS" {
			t.Fatalf("meta %s changed the validated execution: %s", tc.meta, c.Status)
		}
		s := extractJestObservations(results, path, []string{title})
		if (s.Err() != "") != tc.failure {
			t.Fatalf("meta %s: channel error %q", tc.meta, s.Err())
		}
		if !tc.failure {
			got := map[string]string{}
			for _, row := range observe.Compare(s, s, nil).Observations {
				got[row.Key] = row.Base
			}
			if len(tc.values) == 0 && len(got) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, tc.values) {
				t.Fatalf("meta %s: values %v, want %v", tc.meta, got, tc.values)
			}
		}
		again, err := normalizeJestReport([]byte(results))
		if err != nil || again != results {
			t.Fatalf("normalization is not idempotent for meta %s:\n%s\n%s", tc.meta, results, again)
		}
	}
	// Only the named top-level test of the generated file is read.
	results, _ := normalizeJestReport([]byte(jestReportWithMeta(path, title, "passed", `{"probe":{"k":"1"}}`)))
	for _, s := range []observe.Set{
		extractJestObservations(results, path, []string{"another title"}),
		extractJestObservations(results, "src/other.test.ts", []string{title}),
		extractJestObservations("", path, []string{title}),
	} {
		if s.Len() != 0 {
			t.Fatal("observations of another test or file were read")
		}
	}
	// A tampered report that repeats a key records it twice.
	tampered := strings.Replace(results, `{"k":"1"}`, `{"k":"1","k":"1"}`, 1)
	if o := observe.Compare(extractJestObservations(tampered, path, []string{title}), extractJestObservations(results, path, []string{title}), nil); o.Status != model.StatusUnverified || o.Observations[0].Status != model.ObservationIncomparable {
		t.Fatalf("a repeated key in a tampered report: %+v", o)
	}
}

// Observations never turn a readable report into one that redaction would
// alter: the metas are replaced by a fixed marker instead.
func TestJestMetaDroppedWhenTheReportWouldBeAltered(t *testing.T) {
	const path = "src/cart.test.ts"
	// Neither string matches a redaction rule alone, but the encoded report
	// does: "://a" in the title, then ':' and '@' further on.
	raw := jestReportWithMeta(path, "x://a", "passed", `{"probe":{"k":"v@w"}}`)
	results, err := normalizeJestReport([]byte(raw))
	if err != nil || !redact.IsFixedPoint(results) {
		t.Fatalf("report not readable: %v", err)
	}
	if !strings.Contains(results, metaDropped) {
		t.Fatalf("meta not dropped: %s", results)
	}
	if c := ValidateJestExecution(model.Check{Status: "PASS", Results: results}, path, []string{"x://a"}); c.Status != "PASS" {
		t.Fatalf("validation %s", c.Status)
	}
	if s := extractJestObservations(results, path, []string{"x://a"}); s.Err() == "" {
		t.Fatal("dropped observations were read")
	}
}

const observationSource = `package pkg

import (
	"fmt"
	"testing"
)

func TestProbeObserve(t *testing.T) {
	t.Attr("probe.Value()", fmt.Sprintf("%#v", Value()))
	t.Attr("probe.Twice()", fmt.Sprintf("%#v", 2*Value()))
}
`

var observationNames = []string{"TestProbeObserve"}

// sideOf names the snapshot a docker invocation mounts.
func sideOf(h *Harness, args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "type=bind,src="+h.base+",") {
			return "base"
		}
		if strings.HasPrefix(arg, "type=bind,src="+h.candidate+",") {
			return "candidate"
		}
	}
	return "unknown"
}

type goRun struct {
	action string   // pass or fail; "" means exit 0 with pass
	exit   int      // exit code
	pairs  []string // recorded key/value pairs
}

// observeGo runs one generated Go experiment whose baseline and candidate runs
// (in order) emit the given logs. It returns the tool result and the argv of
// every execution.
func observeGo(t *testing.T, h *Harness, source string, runs []goRun) (map[string]any, [][]string) {
	t.Helper()
	var argvs [][]string
	h.execute = func(_ context.Context, _ string, args []string, out io.Writer) execution {
		n := len(argvs)
		argvs = append(argvs, append([]string(nil), args...))
		if _, err := os.Stat(filepath.Join(h.base, "pkg", "obs_test.go")); err != nil {
			t.Errorf("run %d: the generated test is not staged in the baseline", n+1)
		}
		if n >= len(runs) {
			t.Fatalf("unexpected run %d (%s)", n+1, sideOf(h, args))
		}
		r := runs[n]
		action := r.action
		if action == "" {
			action = "pass"
		}
		fmt.Fprint(out, goLog(observationNames[0], action, r.pairs...))
		return execution{ExitCode: r.exit}
	}
	call(t, h, "create_test", map[string]any{"path": "pkg/obs_test.go", "content": source, "description": "Value is stable"})
	var result map[string]any
	if err := json.Unmarshal(call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"}), &result); err != nil {
		t.Fatal(err)
	}
	return result, argvs
}

func evidenceOfKind(h *Harness, kind string) []model.Evidence {
	var out []model.Evidence
	for _, e := range h.Evidence() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestObservationDivergenceRunsOneLiveRepeat(t *testing.T) {
	h := fixture(t)
	result, argvs := observeGo(t, h, observationSource, []goRun{
		{pairs: []string{"Value()", "42", "Twice()", "84"}},
		{pairs: []string{"Value()", "41", "Twice()", "84"}},
		{pairs: []string{"Value()", "42", "Twice()", "84"}},
	})
	checks := h.Checks()
	if len(checks) != 3 || checks[0].Kind != model.CheckGeneratedBase || checks[1].Kind != model.CheckGeneratedCandidate || checks[2].Kind != model.CheckGeneratedBaseRepeat {
		t.Fatalf("checks %+v", checks)
	}
	for _, c := range checks {
		if c.Status != "PASS" || c.Replayed() {
			t.Fatalf("check %+v", c)
		}
	}
	if sideOf(h, argvs[0]) != "base" || sideOf(h, argvs[1]) != "candidate" || sideOf(h, argvs[2]) != "base" {
		t.Fatal("the repeat did not run on the baseline")
	}
	if !reflect.DeepEqual(argvs[0][len(argvs[0])-7:], argvs[2][len(argvs[2])-7:]) || !reflect.DeepEqual(checks[0].Command, checks[2].Command) {
		t.Fatal("the repeat did not run the baseline command")
	}
	evidence := h.Evidence()
	if len(evidence) != 2 || evidence[0].Kind != model.EvidenceDifferentialTest || evidence[0].Status != model.StatusNotReproduced {
		t.Fatalf("evidence %+v", evidence)
	}
	o := evidence[1]
	if o.Kind != model.EvidenceDifferentialObservation || o.Status != model.StatusDiverged || o.CheckID != "check-2" || o.BaseCheckID != "check-1" || o.RepeatCheckID != "check-3" || o.Runner != RunnerGo || !reflect.DeepEqual(o.TestNames, observationNames) || o.Path != "pkg/obs_test.go" {
		t.Fatalf("observation evidence %+v", o)
	}
	if !strings.Contains(o.Description, "observations: 1 diverged, 1 equal, 0 unstable, 0 incomparable") {
		t.Fatalf("description %q", o.Description)
	}
	observation := result["observation"].(map[string]any)
	rows := observation["observations"].([]any)
	if len(rows) != 2 || observation["repeat_check"].(map[string]any)["kind"] != model.CheckGeneratedBaseRepeat {
		t.Fatalf("tool result %v", observation)
	}
	diverged := rows[1].(map[string]any)
	if diverged["key"] != "Value()" || diverged["status"] != model.ObservationDiverged || diverged["base"] != "42" || diverged["candidate"] != "41" {
		t.Fatalf("row %v", diverged)
	}
	if _, ok := artifactByKind(h, model.ArtifactGeneratedTest); !ok {
		t.Fatal("the diverging test was not retained")
	}
	if _, err := h.Call(context.Background(), "delete_generated_test", json.RawMessage(`{"test_id":"generated-test-1"}`)); err == nil || !strings.Contains(err.Error(), "recorded a divergence") {
		t.Fatalf("a diverging test was deleted: %v", err)
	}
	// Re-deriving from the recorded checks gives the stored outcome.
	re, known := EvaluateObservations(RunnerGo, checks[0], checks[1], &checks[2], o.Path, o.TestNames)
	if !known || re.Status != model.StatusDiverged {
		t.Fatalf("re-derived %+v", re)
	}
	for _, dir := range []string{h.base, h.candidate} {
		if _, err := os.Stat(filepath.Join(dir, "pkg", "obs_test.go")); !os.IsNotExist(err) {
			t.Fatal("the generated test was not unstaged")
		}
	}
}

func TestObservationOutcomesWithoutDivergence(t *testing.T) {
	plain := `package pkg
import "testing"
func TestProbeObserve(t *testing.T) { if Value() != 42 { t.Fatal("changed") } }
`
	for _, tc := range []struct {
		name         string
		source       string
		runs         []goRun
		differential string
		observation  string // "" means no observation record
		checks       int
		describes    string
	}{
		{"all equal", observationSource, []goRun{{pairs: []string{"Value()", "42", "Twice()", "84"}}, {pairs: []string{"Value()", "42", "Twice()", "84"}}}, model.StatusNotReproduced, model.StatusNotDiverged, 2, "0 diverged, 2 equal"},
		{"unstable baseline", observationSource, []goRun{{pairs: []string{"Value()", "42"}}, {pairs: []string{"Value()", "41"}}, {pairs: []string{"Value()", "43"}}}, model.StatusNotReproduced, model.StatusUnverified, 3, "1 unstable"},
		{"no observations", plain, []goRun{{}, {}}, model.StatusNotReproduced, "", 2, ""},
		{"declared but not recorded", observationSource, []goRun{{}, {}}, model.StatusNotReproduced, model.StatusUnverified, 2, reasonDeclaredNotFound},
		{"candidate fails", observationSource, []goRun{{pairs: []string{"Value()", "42"}}, {action: "fail", exit: 1, pairs: []string{"Value()", "41"}}}, model.StatusReproduced, model.StatusUnverified, 2, reasonNotBothPass},
		{"forged duplicate on the candidate", observationSource, []goRun{{pairs: []string{"Value()", "42"}}, {pairs: []string{"Value()", "42", "Value()", "42"}}}, model.StatusNotReproduced, model.StatusUnverified, 2, "1 incomparable"},
		{"one-sided key", observationSource, []goRun{{pairs: []string{"Value()", "42"}}, {pairs: []string{"Value()", "42", "Twice()", "84"}}}, model.StatusNotReproduced, model.StatusUnverified, 2, "1 equal, 0 unstable, 1 incomparable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fixture(t)
			result, _ := observeGo(t, h, tc.source, tc.runs)
			if n := len(h.Checks()); n != tc.checks {
				t.Fatalf("%d checks, want %d", n, tc.checks)
			}
			if d := evidenceOfKind(h, model.EvidenceDifferentialTest); len(d) != 1 || d[0].Status != tc.differential {
				t.Fatalf("differential evidence %+v", d)
			}
			obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
			if tc.observation == "" {
				if len(obs) != 0 || result["observation"] != nil || len(h.Evidence()) != 1 {
					t.Fatalf("an ordinary experiment gained an observation record: %+v", obs)
				}
				return
			}
			if len(obs) != 1 || obs[0].Status != tc.observation || !strings.Contains(obs[0].Description, tc.describes) {
				t.Fatalf("observation evidence %+v", obs)
			}
			if tc.observation == model.StatusUnverified && result["observation"].(map[string]any)["reason"] == "" {
				t.Fatal("an UNVERIFIED observation gave the model no reason")
			}
			if (obs[0].RepeatCheckID != "") != (tc.checks == 3) {
				t.Fatalf("repeat check %q with %d checks", obs[0].RepeatCheckID, tc.checks)
			}
			if tc.differential != model.StatusReproduced {
				if _, ok := artifactByKind(h, model.ArtifactGeneratedTest); ok {
					t.Fatal("a non-diverging test was retained")
				}
				if _, err := h.Call(context.Background(), "delete_generated_test", json.RawMessage(`{"test_id":"generated-test-1"}`)); err != nil {
					t.Fatalf("delete refused: %v", err)
				}
			}
		})
	}
}

// When the runtime budget is exhausted, the repeat is recorded as SKIPPED and
// nothing is concluded.
func TestObservationRepeatSkippedWhenTheBudgetIsExhausted(t *testing.T) {
	h := fixture(t)
	h.opts.MaxRuntime = time.Hour
	runs := 0
	h.execute = func(_ context.Context, _ string, _ []string, out io.Writer) execution {
		runs++
		value := "42"
		if runs == 2 {
			value = "41"
			h.spent = h.opts.MaxRuntime // the candidate run used the rest of the budget
		}
		fmt.Fprint(out, goLog(observationNames[0], "pass", "Value()", value))
		return execution{ExitCode: 0}
	}
	call(t, h, "create_test", map[string]any{"path": "pkg/obs_test.go", "content": observationSource})
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	checks := h.Checks()
	if runs != 2 || len(checks) != 3 || checks[2].Kind != model.CheckGeneratedBaseRepeat || checks[2].Status != "SKIPPED" {
		t.Fatalf("runs %d, checks %+v", runs, checks)
	}
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 1 || obs[0].Status != model.StatusUnverified || obs[0].RepeatCheckID != "check-3" || !strings.Contains(obs[0].Description, reasonRepeatNotPass) {
		t.Fatalf("observation %+v", obs)
	}
}

// A divergence whose test cannot be retained is not recorded as one.
func TestObservationRetentionFailureDowngrades(t *testing.T) {
	h := fixture(t)
	blocker := filepath.Join(h.opts.ArtifactDir, h.runID+"-generated-test-1-obs_test.go")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	observeGo(t, h, observationSource, []goRun{
		{pairs: []string{"Value()", "42"}},
		{pairs: []string{"Value()", "41"}},
		{pairs: []string{"Value()", "42"}},
	})
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 1 || obs[0].Status != model.StatusUnverified || !strings.Contains(obs[0].Description, reasonNotRetained) {
		t.Fatalf("observation %+v", obs)
	}
	if _, err := h.Call(context.Background(), "delete_generated_test", json.RawMessage(`{"test_id":"generated-test-1"}`)); err != nil {
		t.Fatalf("an unretained test is not evidence and may be deleted: %v", err)
	}
}

// A replayed baseline may back NOT_DIVERGED; a divergence is decided by the
// live repeat, which is never served from the execution cache.
func TestObservationWithReplayedBaseline(t *testing.T) {
	h := fixture(t)
	m := useMemoryCache(h)
	candidateValue := "42"
	h.execute = func(_ context.Context, _ string, args []string, out io.Writer) execution {
		value := "42"
		if sideOf(h, args) == "candidate" {
			value = candidateValue
		}
		fmt.Fprint(out, goLog(observationNames[0], "pass", "Value()", value))
		return execution{ExitCode: 0}
	}
	call(t, h, "create_test", map[string]any{"path": "pkg/obs_test.go", "content": observationSource})
	for i := 0; i < 3; i++ {
		call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	}
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 3 || obs[2].Status != model.StatusNotDiverged {
		t.Fatalf("observations %+v", obs)
	}
	base := findCheck(t, h, obs[2].BaseCheckID)
	if !base.Replayed() || base.Cache.LiveRuns < 2 {
		t.Fatalf("the third baseline was not replayed: %+v", base)
	}
	candidateValue = "41"
	gets := m.gets
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	obs = evidenceOfKind(h, model.EvidenceDifferentialObservation)
	last := obs[len(obs)-1]
	repeat := findCheck(t, h, last.RepeatCheckID)
	if last.Status != model.StatusDiverged || !findCheck(t, h, last.BaseCheckID).Replayed() || repeat.Replayed() || repeat.Cache != nil || repeat.Kind != model.CheckGeneratedBaseRepeat {
		t.Fatalf("divergence %+v on repeat %+v", last, repeat)
	}
	if m.gets != gets+1 {
		t.Fatalf("the cache was consulted %d times for one experiment; the repeat must never be", m.gets-gets)
	}
}

func findCheck(t *testing.T, h *Harness, id string) model.Check {
	t.Helper()
	for _, c := range h.Checks() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return model.Check{}
}

const observationTSSource = `import { test } from "vitest";
import { total } from "./cart";

test("observe total", ({ task }) => {
  (task.meta as any).probe = { "total([10],0.5)": total([10], 0.5) };
});
`

func TestVitestObservationDivergence(t *testing.T) {
	h := tsFixture(t)
	const path, title = "src/obs.test.ts", "observe total"
	values := []string{"5", "4", "5"}
	run := 0
	h.executeCapture = func(_ context.Context, _ string, args []string, _, payload io.Writer) execution {
		if !contains(args, "--outputFile="+ResultsPath) || !contains(args, path) {
			t.Errorf("unexpected argv %q", args)
		}
		meta := `{"probe":{"total([10],0.5)":` + values[run] + `,"shape":{"b":2,"a":1}}}`
		run++
		fmt.Fprint(payload, coverageFrame(jestReportWithMeta(path, title, "passed", meta)))
		return execution{ExitCode: 0}
	}
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a verifiable experiment ran without the results channel")
		return execution{}
	}
	call(t, h, "create_test", map[string]any{"path": path, "content": observationTSSource})
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	checks := h.Checks()
	if run != 3 || len(checks) != 3 || checks[2].Kind != model.CheckGeneratedBaseRepeat {
		t.Fatalf("runs %d, checks %+v", run, checks)
	}
	for _, c := range checks {
		if c.Status != "PASS" || !redact.IsFixedPoint(c.Results) || !strings.Contains(c.Results, `"shape":{"a":1,"b":2}`) {
			t.Fatalf("check %s: %s %s", c.ID, c.Status, c.Results)
		}
	}
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 1 || obs[0].Status != model.StatusDiverged || obs[0].Runner != RunnerJest || obs[0].RepeatCheckID != checks[2].ID {
		t.Fatalf("observation %+v", obs)
	}
	re, _ := EvaluateObservations(RunnerJest, checks[0], checks[1], &checks[2], path, []string{title})
	if d := re.Diverged(); len(d) != 1 || d[0].Key != "total([10],0.5)" || d[0].Base != "5" || d[0].Candidate != "4" || d[0].Test != title {
		t.Fatalf("rows %+v", re.Observations)
	}
}

// Jest reports carry no per-test metadata: a declared observation experiment
// run through a Jest template records an UNVERIFIED observation that says so.
func TestJestTemplateRecordsNoObservation(t *testing.T) {
	h := tsFixture(t)
	h.opts.Commands["generated_test"] = []string{"npx", "--no", "jest", "{file}", "--json", "--outputFile={results_out}"}
	const path, title = "src/obs.test.ts", "observe total"
	h.executeCapture = func(_ context.Context, _ string, _ []string, _, payload io.Writer) execution {
		fmt.Fprint(payload, coverageFrame(jestReportWithMeta(path, title, "passed", "")))
		return execution{ExitCode: 0}
	}
	call(t, h, "create_test", map[string]any{"path": path, "content": observationTSSource})
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 1 || obs[0].Status != model.StatusUnverified || !strings.Contains(obs[0].Description, reasonDeclaredNotFound) || len(h.Checks()) != 2 {
		t.Fatalf("observation %+v", obs)
	}
}

func TestEvaluateObservationsPreconditions(t *testing.T) {
	names := observationNames
	command := []string{"go", "test", "./pkg"}
	check := func(id, kind, log string, exit int, cmd []string) model.Check {
		status := "PASS"
		if exit != 0 {
			status = "FAIL"
		}
		return model.Check{ID: id, Kind: kind, Status: status, ExitCode: exit, Command: cmd, Output: log}
	}
	base := check("check-1", model.CheckGeneratedBase, goLog(names[0], "pass", "k", "1"), 0, command)
	candidate := check("check-2", model.CheckGeneratedCandidate, goLog(names[0], "pass", "k", "2"), 0, command)
	repeat := check("check-3", model.CheckGeneratedBaseRepeat, goLog(names[0], "pass", "k", "1"), 0, command)
	if o, known := EvaluateObservations(RunnerGo, base, candidate, &repeat, "pkg/obs_test.go", names); !known || o.Status != model.StatusDiverged {
		t.Fatalf("valid experiment: %+v", o)
	}
	if _, known := EvaluateObservations("pytest", base, candidate, &repeat, "pkg/obs_test.go", names); known {
		t.Fatal("a runner without an observation channel is known")
	}
	otherCommand := check("check-3", model.CheckGeneratedBaseRepeat, repeat.Output, 0, []string{"go", "test", "./other"})
	failingRepeat := check("check-3", model.CheckGeneratedBaseRepeat, goLog(names[0], "fail", "k", "1"), 1, command)
	truncated := repeat
	truncated.Truncated = true
	failingBase := check("check-1", model.CheckGeneratedBase, goLog(names[0], "fail", "k", "1"), 1, command)
	unnamed := check("check-2", model.CheckGeneratedCandidate, goLog("TestOther", "pass", "k", "2"), 0, command)
	for name, tc := range map[string]struct {
		base, candidate model.Check
		repeat          *model.Check
		names           []string
	}{
		"no names":               {base, candidate, &repeat, nil},
		"commands differ":        {base, check("check-2", model.CheckGeneratedCandidate, candidate.Output, 0, []string{"go", "test", "./other"}), &repeat, names},
		"baseline fails":         {failingBase, candidate, &repeat, names},
		"named test missing":     {base, unnamed, &repeat, names},
		"repeat command":         {base, candidate, &otherCommand, names},
		"repeat fails":           {base, candidate, &failingRepeat, names},
		"repeat truncated":       {base, candidate, &truncated, names},
		"pending without repeat": {base, candidate, nil, names},
	} {
		o, known := EvaluateObservations(RunnerGo, tc.base, tc.candidate, tc.repeat, "pkg/obs_test.go", tc.names)
		if !known || o.Status != model.StatusUnverified || o.Reason == "" {
			t.Errorf("%s: %+v", name, o)
		}
	}
}

func TestObservationToolDescriptions(t *testing.T) {
	descriptions := map[string]string{}
	for _, d := range ToolDefinitions() {
		f := d["function"].(map[string]any)
		descriptions[f["name"].(string)] = f["description"].(string)
	}
	for tool, fragments := range map[string][]string{
		"create_test":        {`t.Attr("probe.<key>"`, "(task.meta as any).probe", "Jest and pytest cannot record observations", "def test_name(): ...", "at most 32"},
		"run_generated_test": {"differential_observation", "DIVERGED", "NOT_DIVERGED", "not which one is correct"},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(descriptions[tool], fragment) {
				t.Errorf("%s description lacks %q", tool, fragment)
			}
		}
	}
}

// An ordinary JS/TS experiment (no observation in its source or its runs)
// whose checks produced no runner report keeps exactly its single
// differential_test record: a run without a report recorded no observation.
func TestPlainJSExperimentWithoutAReportRecordsNoObservation(t *testing.T) {
	const path, title = "src/plain.test.ts", "applies the discount once"
	plain := "import { test, expect } from \"vitest\";\n\ntest(\"" + title + "\", () => {\n  expect(1).toBe(1);\n});\n"
	passing := jestReportWithMeta(path, title, "passed", "")
	candidateWritesNothing := func(run int) string {
		if run == 1 {
			return ""
		}
		return passing
	}
	for _, tc := range []struct {
		name     string
		template []string
		setup    func(h *Harness)
		report   func(run int) string // "" writes no report
		statuses []string
	}{
		{"vitest candidate writes no report", vitestTemplate, nil, candidateWritesNothing, []string{"PASS", "ERROR"}},
		{"jest candidate writes no report", []string{"npx", "--no", "jest", "{file}", "--json", "--outputFile={results_out}"}, nil, candidateWritesNothing, []string{"PASS", "ERROR"}},
		{"both runs skipped by the budget", vitestTemplate, func(h *Harness) {
			h.opts.MaxRuntime = time.Hour
			h.spent = h.opts.MaxRuntime
		}, func(int) string { return passing }, []string{"SKIPPED", "SKIPPED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tsFixture(t)
			h.opts.Commands["generated_test"] = tc.template
			if tc.setup != nil {
				tc.setup(h)
			}
			run := 0
			h.executeCapture = func(_ context.Context, _ string, _ []string, _, payload io.Writer) execution {
				if report := tc.report(run); report != "" {
					fmt.Fprint(payload, coverageFrame(report))
				}
				run++
				return execution{ExitCode: 0}
			}
			call(t, h, "create_test", map[string]any{"path": path, "content": plain})
			var result map[string]any
			if err := json.Unmarshal(call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"}), &result); err != nil {
				t.Fatal(err)
			}
			checks := h.Checks()
			if len(checks) != len(tc.statuses) {
				t.Fatalf("checks %+v", checks)
			}
			for i, c := range checks {
				if c.Status != tc.statuses[i] {
					t.Fatalf("check %s is %s, want %s", c.ID, c.Status, tc.statuses[i])
				}
			}
			if evidence := h.Evidence(); len(evidence) != 1 || evidence[0].Kind != model.EvidenceDifferentialTest {
				t.Fatalf("an ordinary experiment gained records: %+v", evidence)
			}
			if _, ok := result["observation"]; ok {
				t.Fatalf("the tool result carries an observation: %v", result["observation"])
			}
		})
	}
}

// jestReportWithMetas renders a Jest-compatible report for one file with one
// passing top-level assertion per title, each carrying the raw meta JSON.
func jestReportWithMetas(file string, titles []string, metas []string) string {
	var assertions []string
	for i, title := range titles {
		assertions = append(assertions, fmt.Sprintf(`{"ancestorTitles":[],"title":%q,"status":"passed","meta":%s}`, title, metas[i]))
	}
	return fmt.Sprintf(`{"testResults":[{"assertionResults":[%s],"status":"passed","message":"","name":"/workspace/%s"}]}`, strings.Join(assertions, ","), file)
}

// The number 4 and the string "4" are different recorded values: a Vitest
// value keeps its JSON type.
func TestVitestValueTypesStayDistinct(t *testing.T) {
	h := tsFixture(t)
	const path, title = "src/obs.test.ts", "observe total"
	values := []string{`4`, `"4"`, `4`}
	run := 0
	h.executeCapture = func(_ context.Context, _ string, _ []string, _, payload io.Writer) execution {
		fmt.Fprint(payload, coverageFrame(jestReportWithMeta(path, title, "passed", `{"probe":{"total([10],0.5)":`+values[run]+`}}`)))
		run++
		return execution{ExitCode: 0}
	}
	call(t, h, "create_test", map[string]any{"path": path, "content": observationTSSource})
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if run != 3 || len(obs) != 1 || obs[0].Status != model.StatusDiverged {
		t.Fatalf("runs %d, observation %+v", run, obs)
	}
	checks := h.Checks()
	re, _ := EvaluateObservations(RunnerJest, checks[0], checks[1], &checks[2], path, []string{title})
	if d := re.Diverged(); len(d) != 1 || d[0].Base != "4" || d[0].Candidate != `"4"` {
		t.Fatalf("rows %+v", re.Observations)
	}
}

// Recorded Vitest values never turn a readable PASS or FAIL generated-test
// check into ERROR: a value too long to compare is kept as a bounded stand-in,
// and when the kept metas still make the normalized report exceed the payload
// limit or the results budget, they are replaced by a fixed marker instead.
func TestLargeVitestMetaNeverMakesACheckUnreadable(t *testing.T) {
	const path, title = "src/obs.test.ts", "observe total"
	// 174 KiB of '<' in one value: HTML escaping would make it about 1 MiB,
	// twice the 512 KiB payload limit of the fixture, while the raw report fits.
	huge := `{"probe":{"total([10],0.5)":"` + strings.Repeat("<", 174*1024) + `"}}`
	small := `{"probe":{"total([10],0.5)":"5"}}`
	// Three tests with 32 values of 1000 '<' each: every value fits the
	// compared bound, and the escaped report (about 580 KB) exceeds the limit.
	titles := []string{"observe a", "observe b", "observe c"}
	var sources, bigMetas, smallMetas []string
	for _, name := range titles {
		sources = append(sources, "test(\""+name+"\", ({ task }) => {\n  (task.meta as any).probe = values();\n});\n")
		var pairs []string
		for k := 0; k < observe.MaxKeys; k++ {
			pairs = append(pairs, fmt.Sprintf(`"k%02d":"%s"`, k, strings.Repeat("<", 1000)))
		}
		bigMetas = append(bigMetas, `{"probe":{`+strings.Join(pairs, ",")+`}}`)
		smallMetas = append(smallMetas, `{"probe":{"k00":"1"}}`)
	}
	manySource := "import { test } from \"vitest\";\nimport { values } from \"./values\";\n\n" + strings.Join(sources, "\n")
	one := []string{title}
	for _, tc := range []struct {
		name             string
		source           string
		names            []string
		base, candidate  string
		candidateExit    int
		differential     string
		standIn, dropped bool
		prefill          int // candidate-side results already recorded
	}{
		{"one huge value", observationTSSource, one, jestReportWithMeta(path, title, "passed", small), jestReportWithMeta(path, title, "passed", huge), 0, model.StatusNotReproduced, true, false, 0},
		{"one huge value on a failing candidate", observationTSSource, one, jestReportWithMeta(path, title, "passed", small), jestReportWithMeta(path, title, "failed", huge), 1, model.StatusReproduced, true, false, 0},
		{"many values over the payload limit", manySource, titles, jestReportWithMetas(path, titles, smallMetas), jestReportWithMetas(path, titles, bigMetas), 0, model.StatusNotReproduced, false, true, 0},
		{"values over the candidate results share", observationTSSource, one, jestReportWithMeta(path, title, "passed", small), jestReportWithMeta(path, title, "passed", `{"probe":{"k":"`+strings.Repeat("<", 1000)+`"}}`), 0, model.StatusNotReproduced, false, true, ResultsBudget/2 - 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tsFixture(t)
			baseID, candidateID := "check-1", "check-2"
			if tc.prefill > 0 {
				h.mu.Lock()
				filler := strings.Repeat("f", tc.prefill)
				h.checks = []model.Check{{ID: "check-1", Kind: model.CheckGeneratedCandidate, Status: "PASS", Results: filler}}
				h.resultsBytes = len(filler)
				h.mu.Unlock()
				baseID, candidateID = "check-2", "check-3"
			}
			if raw := len(tc.candidate); raw > PayloadLimit(h.opts.MaxOutputBytes) {
				t.Fatalf("the raw candidate report (%d bytes) does not fit the payload channel", raw)
			}
			reports := []string{tc.base, tc.candidate}
			run := 0
			h.executeCapture = func(_ context.Context, _ string, _ []string, _, payload io.Writer) execution {
				fmt.Fprint(payload, coverageFrame(reports[run]))
				exit := 0
				if run == 1 {
					exit = tc.candidateExit
				}
				run++
				return execution{ExitCode: exit}
			}
			call(t, h, "create_test", map[string]any{"path": path, "content": tc.source})
			call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
			candidate := findCheck(t, h, candidateID)
			if candidate.Kind != model.CheckGeneratedCandidate || candidate.Status == "ERROR" || candidate.Results == "" || len(candidate.Results) > PayloadLimit(h.opts.MaxOutputBytes) {
				t.Fatalf("candidate check %s (%s): %s (%d bytes of results)\n%s", candidate.ID, candidate.Kind, candidate.Status, len(candidate.Results), candidate.Output)
			}
			if !redact.IsFixedPoint(candidate.Results) {
				t.Fatal("the recorded results are not a Redact fixed point")
			}
			if tc.standIn != strings.Contains(candidate.Results, observe.OversizedPrefix) || tc.dropped != strings.Contains(candidate.Results, metaTooLarge) {
				t.Fatalf("stand-in %v, dropped %v in %.300s", strings.Contains(candidate.Results, observe.OversizedPrefix), strings.Contains(candidate.Results, metaTooLarge), candidate.Results)
			}
			if d := evidenceOfKind(h, model.EvidenceDifferentialTest); len(d) != 1 || d[0].Status != tc.differential {
				t.Fatalf("differential evidence %+v", d)
			}
			if o := evidenceOfKind(h, model.EvidenceDifferentialObservation); len(o) != 1 || o[0].Status != model.StatusUnverified {
				t.Fatalf("observation evidence %+v", o)
			}
			// What the candidate recorded may still differ, so a both-pass test
			// cannot hide it.
			bs, _ := ObservationSet(RunnerJest, findCheck(t, h, baseID), path, tc.names)
			cs, _ := ObservationSet(RunnerJest, candidate, path, tc.names)
			if !observe.Differ(bs, cs) {
				t.Fatal("a replaced or oversized candidate value hides the difference")
			}
		})
	}
}

// A declared observation experiment whose runs produced no report is
// UNVERIFIED because the runs did not both pass, not because nothing was
// recorded.
func TestDeclaredVitestExperimentWithoutAReport(t *testing.T) {
	h := tsFixture(t)
	const path = "src/obs.test.ts"
	h.executeCapture = func(context.Context, string, []string, io.Writer, io.Writer) execution {
		return execution{ExitCode: 0}
	}
	call(t, h, "create_test", map[string]any{"path": path, "content": observationTSSource})
	call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"})
	obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
	if len(obs) != 1 || obs[0].Status != model.StatusUnverified || !strings.Contains(obs[0].Description, reasonNotBothPass) || strings.Contains(obs[0].Description, reasonDeclaredNotFound) {
		t.Fatalf("observation %+v", obs)
	}
}
