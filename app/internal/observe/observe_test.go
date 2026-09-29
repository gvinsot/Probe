package observe

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/model"
)

const test = "TestDiscount"

// set builds a run's observations from key/value pairs, in order.
func set(pairs ...string) Set {
	var s Set
	for i := 0; i+1 < len(pairs); i += 2 {
		s.Record(test, pairs[i], pairs[i+1])
	}
	return s
}

func ptr(s Set) *Set { return &s }

func rowOf(t *testing.T, o Outcome, key string) model.Observation {
	t.Helper()
	for _, row := range o.Observations {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("no row for %q in %+v", key, o.Observations)
	return model.Observation{}
}

func TestCompareTable(t *testing.T) {
	long := strings.Repeat("v", MaxValueBytes+1)
	for _, tc := range []struct {
		name            string
		base, candidate Set
		repeat          *Set
		status          string
		needsRepeat     bool
		rows            map[string]string // key -> row status
	}{
		{"equal", set("a", "1", "b", "2"), set("a", "1", "b", "2"), nil, model.StatusNotDiverged, false, map[string]string{"a": model.ObservationEqual, "b": model.ObservationEqual}},
		{"differs without a repeat", set("a", "1", "b", "4"), set("a", "1", "b", "3"), nil, model.StatusUnverified, true, map[string]string{"a": model.ObservationEqual, "b": model.ObservationIncomparable}},
		{"differs, repeat agrees", set("a", "1", "b", "4"), set("a", "1", "b", "3"), ptr(set("a", "1", "b", "4")), model.StatusDiverged, false, map[string]string{"a": model.ObservationEqual, "b": model.ObservationDiverged}},
		{"differs, repeat disagrees", set("b", "4"), set("b", "3"), ptr(set("b", "5")), model.StatusUnverified, false, map[string]string{"b": model.ObservationUnstable}},
		{"differs, repeat omits the key", set("b", "4"), set("b", "3"), ptr(set("c", "4")), model.StatusUnverified, false, map[string]string{"b": model.ObservationUnstable, "c": model.ObservationUnstable}},
		{"equal, repeat disagrees", set("a", "1"), set("a", "1"), ptr(set("a", "2")), model.StatusUnverified, false, map[string]string{"a": model.ObservationUnstable}},
		{"candidate only", set("a", "1"), set("a", "1", "b", "3"), ptr(set("a", "1")), model.StatusUnverified, false, map[string]string{"a": model.ObservationEqual, "b": model.ObservationIncomparable}},
		{"baseline only", set("a", "1", "b", "3"), set("a", "1"), ptr(set("a", "1", "b", "3")), model.StatusUnverified, false, map[string]string{"a": model.ObservationEqual, "b": model.ObservationIncomparable}},
		{"duplicate on the candidate", set("a", "1"), set("a", "1", "a", "1"), nil, model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"duplicate on the baseline", set("a", "1", "a", "2"), set("a", "3"), ptr(set("a", "1")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"duplicate on the repeat", set("a", "1"), set("a", "3"), ptr(set("a", "1", "a", "1")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"oversized value", set("a", long), set("a", long+"x"), ptr(set("a", long)), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"redacted and equal", set("a", "[REDACTED]"), set("a", "[REDACTED]"), nil, model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"redacted and differing", set("a", "x [REDACTED]"), set("a", "y"), ptr(set("a", "x [REDACTED]")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"not a redaction fixed point", set("a", "password=abc"), set("a", "password=abd"), ptr(set("a", "password=abc")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"address-like difference", set("a", "&{0xc000012345}"), set("a", "&{0xc000054321}"), ptr(set("a", "&{0xc000012345}")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"address-like but equal", set("a", "0x2a860a162248"), set("a", "0x2a860a162248"), nil, model.StatusNotDiverged, false, map[string]string{"a": model.ObservationEqual}},
		{"source position difference", set("a", "error at price.go:12"), set("a", "error at price.go:14"), ptr(set("a", "error at price.go:12")), model.StatusUnverified, false, map[string]string{"a": model.ObservationIncomparable}},
		{"key with whitespace", set("a b", "1"), set("a b", "1"), nil, model.StatusUnverified, false, map[string]string{"a b": model.ObservationIncomparable}},
		{"empty key", set("", "1"), set("", "1"), nil, model.StatusUnverified, false, nil},
		{"control character in key", set("a\x01", "1"), set("a\x01", "1"), nil, model.StatusUnverified, false, map[string]string{"a\x01": model.ObservationIncomparable}},
		{"key altered by redaction", set("token=[REDACTED]", "1"), set("token=[REDACTED]", "1"), nil, model.StatusUnverified, false, map[string]string{"token=[REDACTED]": model.ObservationIncomparable}},
		{"any divergence decides", set("a", "1", "b", "4", "c", "x", "d", "[REDACTED]"), set("a", "2", "b", "3", "c", "x", "d", "[REDACTED]"), ptr(set("a", "9", "b", "4", "c", "x", "d", "[REDACTED]")), model.StatusDiverged, false, map[string]string{"a": model.ObservationUnstable, "b": model.ObservationDiverged, "c": model.ObservationEqual, "d": model.ObservationIncomparable}},
		{"no keys", Set{}, Set{}, nil, model.StatusUnverified, false, map[string]string{}},
		{"empty values are values", set("a", ""), set("a", "0"), ptr(set("a", "")), model.StatusDiverged, false, map[string]string{"a": model.ObservationDiverged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Compare(tc.base, tc.candidate, tc.repeat)
			if o.Status != tc.status || o.NeedsRepeat != tc.needsRepeat {
				t.Fatalf("status %s (needs repeat %v, reason %q), want %s (%v); rows %+v", o.Status, o.NeedsRepeat, o.Reason, tc.status, tc.needsRepeat, o.Observations)
			}
			if o.Status == model.StatusUnverified && o.Reason == "" {
				t.Fatal("an UNVERIFIED outcome has no reason")
			}
			for key, want := range tc.rows {
				if got := rowOf(t, o, key); got.Status != want || (want != model.ObservationEqual && want != model.ObservationDiverged && got.Reason == "") {
					t.Errorf("row %q: %+v, want %s with a reason", key, got, want)
				}
			}
			if tc.rows != nil && len(o.Observations) != len(tc.rows) {
				t.Errorf("rows %+v, want %d", o.Observations, len(tc.rows))
			}
		})
	}
}

// DIVERGED requires the key recorded exactly once on every side: a presence
// difference is never a divergence, and the recorded flags say which side had it.
func TestPresenceIsRecordedButNeverDiverges(t *testing.T) {
	o := Compare(set("a", "1"), set("b", "1"), ptr(set("a", "1")))
	if o.Status != model.StatusUnverified {
		t.Fatalf("status %s", o.Status)
	}
	a, b := rowOf(t, o, "a"), rowOf(t, o, "b")
	if !a.BaseRecorded || a.CandidateRecorded || a.Base != "1" || a.Candidate != "" || a.Status != model.ObservationIncomparable {
		t.Errorf("baseline-only row %+v", a)
	}
	if b.BaseRecorded || !b.CandidateRecorded || b.Candidate != "1" || b.Status != model.ObservationIncomparable {
		t.Errorf("candidate-only row %+v", b)
	}
}

func TestChannelErrorsAndKeyLimit(t *testing.T) {
	var many Set
	for i := 0; i <= MaxKeys; i++ {
		many.Record(test, fmt.Sprintf("k%02d", i), "1")
	}
	if many.Err() == "" || many.Len() != MaxKeys+1 {
		t.Fatalf("the 33rd key did not fail the set: %q, %d keys", many.Err(), many.Len())
	}
	var exactly Set
	for i := 0; i < MaxKeys; i++ {
		exactly.Record(test, fmt.Sprintf("k%02d", i), "1")
	}
	if exactly.Err() != "" {
		t.Fatalf("32 keys failed: %q", exactly.Err())
	}
	if o := Compare(exactly, exactly, nil); o.Status != model.StatusNotDiverged {
		t.Fatalf("32 equal keys: %s", o.Status)
	}
	failed := set("a", "1")
	failed.Fail("fixed reason")
	failed.Fail("second reason")
	if failed.Err() != "fixed reason" || failed.Empty() {
		t.Fatalf("first failure not kept: %q", failed.Err())
	}
	var onlyFailed Set
	onlyFailed.Fail("x")
	if onlyFailed.Empty() {
		t.Fatal("a failed set is not empty")
	}
	for name, o := range map[string]Outcome{
		"too many keys on the candidate": Compare(set("a", "1"), many, nil),
		"failed baseline":                Compare(failed, set("a", "1"), nil),
		"failed repeat":                  Compare(set("a", "1"), set("a", "2"), &failed),
	} {
		if o.Status != model.StatusUnverified || o.Reason == "" || len(o.Observations) != 0 || o.NeedsRepeat {
			t.Errorf("%s: %+v", name, o)
		}
	}
	var invalid Set
	invalid.Invalidate(test, "long", "value exceeded the runner's line limit")
	invalid.Invalidate(test, "long", "second reason")
	if invalid.Empty() || invalid.Len() != 1 {
		t.Fatal("an invalidated key is not recorded")
	}
	o := Compare(invalid, set("long", "x"), nil)
	if row := rowOf(t, o, "long"); row.Status != model.ObservationIncomparable || row.Reason != "value exceeded the runner's line limit" || !row.BaseRecorded || o.Status != model.StatusUnverified {
		t.Fatalf("invalidated key: %+v (%s)", row, o.Status)
	}
}

func TestRowsAreSortedAndDeterministic(t *testing.T) {
	var base, candidate, repeat Set
	keys := []string{"m", "a", "z", "c", "b"}
	for i, k := range keys {
		base.Record("TestB", k, fmt.Sprint(i))
		repeat.Record("TestB", k, fmt.Sprint(i))
		candidate.Record("TestB", keys[len(keys)-1-i], fmt.Sprint(len(keys)-1-i))
		base.Record("TestA", k, "x")
		repeat.Record("TestA", k, "x")
		candidate.Record("TestA", k, "y")
	}
	first := Compare(base, candidate, &repeat)
	second := Compare(base, candidate, &repeat)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Compare is not deterministic")
	}
	var order []string
	for _, row := range first.Observations {
		order = append(order, row.Test+"/"+row.Key)
	}
	want := "TestA/a TestA/b TestA/c TestA/m TestA/z TestB/a TestB/b TestB/c TestB/m TestB/z"
	if strings.Join(order, " ") != want {
		t.Fatalf("order %v", order)
	}
	if first.Status != model.StatusDiverged || len(first.Diverged()) != 5 {
		t.Fatalf("status %s, %d diverged", first.Status, len(first.Diverged()))
	}
}

// Values are compared in full; the rows carry UTF-8-safe display cuts.
func TestDisplayCutsNeverDecideComparison(t *testing.T) {
	prefix := strings.Repeat("é", DisplayBytes) // 2 bytes per rune: the cut falls inside the prefix
	base, candidate := prefix+"A", prefix+"B"
	o := Compare(set("k", base), set("k", candidate), ptr(set("k", base)))
	row := rowOf(t, o, "k")
	if o.Status != model.StatusDiverged || row.Status != model.ObservationDiverged {
		t.Fatalf("values differing after the display cut did not diverge: %+v", row)
	}
	if !row.Truncated || len(row.Base) > DisplayBytes || !utf8.ValidString(row.Base) || row.Base != row.Candidate {
		t.Fatalf("display cut: truncated %v, %d bytes, valid %v", row.Truncated, len(row.Base), utf8.ValidString(row.Base))
	}
	short := Compare(set("k", "4"), set("k", "3"), ptr(set("k", "4")))
	if r := rowOf(t, short, "k"); r.Truncated || r.Base != "4" || r.Candidate != "3" || !r.BaseRecorded || !r.CandidateRecorded {
		t.Fatalf("short row %+v", r)
	}
}

func TestSummaryAndDiverged(t *testing.T) {
	o := Compare(set("a", "1", "b", "4", "c", "[REDACTED]", "d", "1"), set("a", "1", "b", "3", "c", "[REDACTED]", "d", "2"), ptr(set("a", "1", "b", "4", "c", "[REDACTED]", "d", "3")))
	if got := o.Summary(); got != "observations: 1 diverged, 1 equal, 1 unstable, 1 incomparable" {
		t.Fatalf("summary %q", got)
	}
	if d := o.Diverged(); len(d) != 1 || d[0].Key != "b" || d[0].Base != "4" || d[0].Candidate != "3" {
		t.Fatalf("diverged rows %+v", d)
	}
	u := Compare(set("a", "1"), set("a", "2"), nil)
	if got := u.Summary(); !strings.HasPrefix(got, "observations: 0 diverged, 0 equal, 0 unstable, 1 incomparable; inconclusive: ") {
		t.Fatalf("unverified summary %q", got)
	}
	if d := Unverified("x").Diverged(); d == nil || len(d) != 0 {
		t.Fatalf("Diverged of an empty outcome: %#v", d)
	}
	if o := Unverified("x"); o.Observations == nil {
		t.Fatal("Unverified rows are nil")
	}
}

// Differ fails closed: only full values recorded readably on both sides, with
// no redaction marker and no channel error, can show that nothing differs.
func TestDiffer(t *testing.T) {
	unreadable := func(pairs ...string) Set {
		s := set(pairs...)
		s.Invalidate(test, "a", "too long")
		return s
	}
	failed := func(pairs ...string) Set {
		s := set(pairs...)
		s.Fail("fixed reason")
		return s
	}
	for _, tc := range []struct {
		name            string
		base, candidate Set
		want            bool
	}{
		{"equal", set("a", "1", "b", "2"), set("b", "2", "a", "1"), false},
		{"both empty", Set{}, Set{}, false},
		{"value differs", set("a", "1"), set("a", "2"), true},
		{"redacted values differ", set("a", "[REDACTED]"), set("a", "x"), true},
		{"redacted values equal", set("a", "[REDACTED]"), set("a", "[REDACTED]"), true},
		{"partly redacted values equal", set("a", "user [REDACTED]"), set("a", "user [REDACTED]"), true},
		{"redacted key", set("token=[REDACTED]", "1"), set("token=[REDACTED]", "1"), true},
		{"escaped secret, equal and unaltered", set("a", "password=abc"), set("a", "password=abc"), false},
		{"candidate only", set("a", "1"), set("a", "1", "b", "2"), true},
		{"baseline only", set("a", "1", "b", "2"), set("a", "1"), true},
		{"repeated equal values", set("a", "1"), set("a", "1", "a", "1"), false},
		{"forged second value", set("a", "1"), set("a", "1", "a", "2"), true},
		{"unreadable value on one side only", unreadable(), set("b", "1"), true},
		{"unreadable against recorded", unreadable(), set("a", "1"), true},
		{"recorded against unreadable", set("a", "1"), unreadable(), true},
		{"unreadable on both sides", unreadable(), unreadable(), true},
		{"channel error on the candidate", set("a", "1"), failed("a", "1"), true},
		{"channel error on the baseline, nothing recorded", failed(), Set{}, true},
		{"equal stand-ins", set("a", `"`+Oversized("x")+`"`), set("a", `"`+Oversized("x")+`"`), false},
		{"different stand-ins", set("a", `"`+Oversized("x")+`"`), set("a", `"`+Oversized("y")+`"`), true},
		{"stand-ins of redacted values", set("a", Oversized("[REDACTED]")), set("a", Oversized("[REDACTED]")), true},
	} {
		if got := Differ(tc.base, tc.candidate); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A stand-in for a value too long to compare keeps its length and hash: it is
// never compared and never a valid key, but two different values stay
// different.
func TestOversizedStandIn(t *testing.T) {
	long := strings.Repeat("<", MaxValueBytes+1)
	s := Oversized(long)
	if !strings.HasPrefix(s, OversizedPrefix+fmt.Sprintf("%d bytes, sha256 ", len(long))) || len(s) > 200 || strings.Contains(s, "<") {
		t.Fatalf("stand-in %q", s)
	}
	if Oversized(long) != s || Oversized(long+"x") == s {
		t.Fatal("stand-ins are not a function of the text")
	}
	if strings.Contains(s, "[REDACTED]") || !strings.HasSuffix(Oversized("a [REDACTED] b"), " [REDACTED]") {
		t.Fatal("only the stand-in of a redacted text carries the marker")
	}
	o := Compare(set("a", `"`+s+`"`), set("a", `"`+s+`"`), nil)
	if row := rowOf(t, o, "a"); row.Status != model.ObservationIncomparable || row.Reason != reasonValueTooLong || o.Status != model.StatusUnverified {
		t.Fatalf("equal stand-ins were compared: %+v", row)
	}
	if keyProblem(s) != reasonKeyInvalid {
		t.Fatal("a stand-in is a valid key")
	}
}
