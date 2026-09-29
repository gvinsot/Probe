// Package observe compares the values a generated test recorded on the
// baseline and on the candidate: the observation oracle. It is pure and
// deterministic. The harness uses it while an experiment runs, and
// report.Finalize uses the same functions again to re-derive every outcome
// from the recorded checks, so a stored status counts only when the
// recomputation agrees with it.
//
// An outcome describes recorded values only. DIVERGED means that, for the same
// named test and key, two baseline runs recorded the same value and the
// candidate run recorded a different one. It never says which revision is
// correct. NOT_DIVERGED means that every recorded value was equal; the values
// are the test's own bounded, redacted serializations, so it does not establish
// equivalent behavior, even for the recorded inputs.
package observe

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// Recording conventions and limits.
const (
	// GoKeyPrefix starts every Go attribute key (t.Attr) that records an
	// observation; the key recorded is what follows it.
	GoKeyPrefix = "probe."
	// MetaField is the field of a Vitest task.meta that holds observations.
	MetaField = "probe"
	// MaxKeys bounds the distinct keys one run may record; more is a channel
	// error, which makes the whole outcome UNVERIFIED.
	MaxKeys = 32
	// MaxKeyBytes bounds a key; a longer key cannot be compared.
	MaxKeyBytes = 200
	// MaxValueBytes bounds a value that is compared; a longer one cannot be.
	MaxValueBytes = 1024
	// DisplayBytes is the UTF-8-safe display cut of a value in a row.
	DisplayBytes = 256
	// OversizedPrefix starts the stand-in (Oversized) that replaces a recorded
	// Vitest key or value too long to be compared. It contains whitespace, so
	// a stand-in is never a valid key, and a value containing it is never
	// compared.
	OversizedPrefix = "probe: not kept: "
)

// Oversized returns the fixed stand-in for a recorded key or value that is too
// long to be compared: its length and the sha256 of text, which the caller has
// already redacted. Two stand-ins are equal only when the texts were, so
// Differ still sees a difference between two long values; Compare never
// compares a value that contains the stand-in. When text holds the redaction
// marker, so does the stand-in, because equal redacted texts do not establish
// equal original values.
func Oversized(text string) string {
	s := fmt.Sprintf("%s%d bytes, sha256 %x", OversizedPrefix, len(text), sha256.Sum256([]byte(text)))
	if strings.Contains(text, redact.Marker) {
		s += " " + redact.Marker
	}
	return s
}

// Fixed row reasons. They never contain recorded text.
const (
	reasonKeyInvalid      = "the key is empty, longer than 200 bytes, not UTF-8, or contains whitespace or control characters"
	reasonKeyRedacted     = "the key was altered by redaction"
	reasonDuplicate       = "the key was recorded more than once in one run"
	reasonValueRedacted   = "a value was altered by redaction; equality of the original values is unknown"
	reasonValueTooLong    = "a value exceeds 1024 bytes"
	reasonUnstableValue   = "a value looks like a memory address or a source position, which can differ between builds"
	reasonOneRevision     = "the key was recorded on one revision only; presence differences are not validated as divergences"
	reasonRepeatOnly      = "the key was recorded only by the baseline repeat"
	reasonRepeatDiffers   = "the baseline repeat did not record the same value as the first baseline run"
	reasonAwaitingRepeat  = "the candidate value differs, and no baseline repeat was recorded to confirm the baseline value"
	reasonNoObservation   = "no observation was recorded"
	reasonNeedsRepeat     = "a recorded value differs; a baseline repeat is needed before anything can be concluded"
	reasonNothingDecided  = "no key diverged, and at least one key could not be compared or was unstable across the two baseline runs"
	summaryUnverifiedNote = "inconclusive"
)

// Key names one recorded observation: the named test and the key it recorded.
type Key struct {
	Test, Name string
}

func (k Key) less(o Key) bool {
	if k.Test != o.Test {
		return k.Test < o.Test
	}
	return k.Name < o.Name
}

// Set is what one run recorded. Every value is kept in order, so a key
// recorded twice (for example by a forged framing line) is detectable. A key
// can also be present without a readable value (Invalidate). A channel error
// (Fail) makes any comparison of the set UNVERIFIED. The zero value is an
// empty set.
type Set struct {
	values  map[Key][]string
	invalid map[Key]string
	order   []Key
	err     string
}

func (s *Set) note(k Key) {
	if s.values == nil {
		s.values = map[Key][]string{}
		s.invalid = map[Key]string{}
	}
	_, hasValue := s.values[k]
	_, isInvalid := s.invalid[k]
	if !hasValue && !isInvalid {
		s.order = append(s.order, k)
		if len(s.order) > MaxKeys {
			s.Fail(fmt.Sprintf("more than %d distinct keys were recorded in one run", MaxKeys))
		}
	}
}

// Record adds one value recorded under name by the named test.
func (s *Set) Record(test, name, value string) {
	k := Key{test, name}
	s.note(k)
	s.values[k] = append(s.values[k], value)
}

// Invalidate marks a key as recorded without a readable value, with a fixed
// reason (for example a value too long for the runner to convert).
func (s *Set) Invalidate(test, name, reason string) {
	k := Key{test, name}
	s.note(k)
	if _, done := s.invalid[k]; !done {
		s.invalid[k] = reason
	}
}

// Fail records a channel error with a fixed reason. The first error is kept.
func (s *Set) Fail(reason string) {
	if s.err == "" {
		s.err = reason
	}
}

// Err returns the channel error, or "".
func (s Set) Err() string { return s.err }

// Empty reports whether the run recorded nothing: no key and no channel error.
func (s Set) Empty() bool { return len(s.order) == 0 && s.err == "" }

// Len returns the number of distinct keys recorded.
func (s Set) Len() int { return len(s.order) }

// present reports whether the key was recorded, with or without a readable value.
func (s Set) present(k Key) bool {
	_, hasValue := s.values[k]
	_, isInvalid := s.invalid[k]
	return hasValue || isInvalid
}

// Outcome is the comparison of one experiment.
type Outcome struct {
	// Status is model.StatusDiverged, model.StatusNotDiverged or
	// model.StatusUnverified.
	Status string
	// Reason explains an UNVERIFIED status with a fixed text.
	Reason string
	// NeedsRepeat is set when a key differs and no baseline repeat was given:
	// only one more live baseline run can decide it.
	NeedsRepeat bool
	// Observations holds one row per key, sorted by (test, key).
	Observations []model.Observation
}

// Unverified is an UNVERIFIED outcome with a fixed reason and no rows.
func Unverified(reason string) Outcome {
	return Outcome{Status: model.StatusUnverified, Reason: reason, Observations: []model.Observation{}}
}

// Diverged returns the DIVERGED rows, in order.
func (o Outcome) Diverged() []model.Observation {
	rows := []model.Observation{}
	for _, row := range o.Observations {
		if row.Status == model.ObservationDiverged {
			rows = append(rows, row)
		}
	}
	return rows
}

// Summary counts the rows by status, for example
// "observations: 1 diverged, 3 equal, 0 unstable, 1 incomparable".
func (o Outcome) Summary() string {
	count := map[string]int{}
	for _, row := range o.Observations {
		count[row.Status]++
	}
	s := fmt.Sprintf("observations: %d diverged, %d equal, %d unstable, %d incomparable", count[model.ObservationDiverged], count[model.ObservationEqual], count[model.ObservationUnstable], count[model.ObservationIncomparable])
	if o.Status == model.StatusUnverified && o.Reason != "" {
		s += "; " + summaryUnverifiedNote + ": " + o.Reason
	}
	return s
}

var (
	addressLike    = regexp.MustCompile(`0x[0-9a-fA-F]{8,}`)
	sourcePosition = regexp.MustCompile(`\.(?:go|ts|tsx|js|mjs|cjs|jsx):\d+`)
)

// keyProblem returns why a key cannot be compared, or "".
func keyProblem(name string) string {
	if name == "" || len(name) > MaxKeyBytes || !utf8.ValidString(name) {
		return reasonKeyInvalid
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return reasonKeyInvalid
		}
	}
	if !redact.IsFixedPoint(name) || strings.Contains(name, redact.Marker) {
		return reasonKeyRedacted
	}
	return ""
}

// valueProblem returns why a recorded value cannot be compared, or "". A value
// altered by redaction proves nothing when it equals another; a longer value,
// or the stand-in that replaced one (Oversized), is outside the compared bound.
func valueProblem(v string) string {
	if strings.Contains(v, redact.Marker) || !redact.IsFixedPoint(v) {
		return reasonValueRedacted
	}
	if len(v) > MaxValueBytes || strings.Contains(v, OversizedPrefix) {
		return reasonValueTooLong
	}
	return ""
}

// buildDependent reports whether a value looks like a memory address or a
// source position. Such values can differ between two binaries built from
// different sources without any behavior difference.
func buildDependent(v string) bool {
	return addressLike.MatchString(v) || sourcePosition.MatchString(v)
}

// display returns the UTF-8-safe display cut of a recorded value and whether
// it was cut. The value is redacted first: a value that is not a Redact fixed
// point (it can only be INCOMPARABLE) is never displayed as recorded. A
// comparable value is a fixed point, and so is every prefix of it.
func display(v string) (string, bool) {
	v = redact.Redact(v)
	if len(v) <= DisplayBytes {
		return v, false
	}
	return redact.TruncateUTF8(v, DisplayBytes), true
}

// Compare compares what the baseline and the candidate recorded, and, when
// repeat is not nil, what a second, live baseline run recorded. It never looks
// at display cuts: values are compared in full.
//
// Per key, in order:
//   - INCOMPARABLE when the key is invalid or was not readable on any side,
//     when it was recorded more than once on any side, when any recorded value
//     contains the redaction marker, is not a Redact fixed point, exceeds
//     MaxValueBytes or holds the stand-in of a longer value (Oversized), or
//     when it was recorded on one revision only;
//   - EQUAL when the baseline and the candidate recorded the same value and the
//     repeat, if any, recorded it too;
//   - UNSTABLE when the repeat did not record the first baseline value;
//   - DIVERGED only when the baseline, the repeat and the candidate each
//     recorded the key exactly once, the baseline and the repeat agree, and
//     the candidate differs, unless a differing value looks like an address or
//     a source position (INCOMPARABLE);
//   - a differing key without a repeat is INCOMPARABLE and sets NeedsRepeat.
//
// The outcome is DIVERGED when any key is DIVERGED, NOT_DIVERGED when at least
// one key was compared and every key is EQUAL, and UNVERIFIED otherwise
// (including any channel error and a pending repeat).
func Compare(base, candidate Set, repeat *Set) Outcome {
	switch {
	case base.err != "":
		return Unverified("the baseline observations could not be read: " + base.err)
	case candidate.err != "":
		return Unverified("the candidate observations could not be read: " + candidate.err)
	case repeat != nil && repeat.err != "":
		return Unverified("the baseline repeat observations could not be read: " + repeat.err)
	}
	seen := map[Key]bool{}
	var keys []Key
	sides := []Set{base, candidate}
	if repeat != nil {
		sides = append(sides, *repeat)
	}
	for _, s := range sides {
		for _, k := range s.order {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return Unverified(reasonNoObservation)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	out := Outcome{Observations: make([]model.Observation, 0, len(keys))}
	pending, diverged, equal := false, false, 0
	for _, k := range keys {
		row := compareKey(k, base, candidate, repeat)
		switch row.Status {
		case model.ObservationDiverged:
			diverged = true
		case model.ObservationEqual:
			equal++
		}
		if row.Reason == reasonAwaitingRepeat {
			pending = true
		}
		out.Observations = append(out.Observations, row)
	}
	switch {
	case pending:
		out.Status, out.Reason, out.NeedsRepeat = model.StatusUnverified, reasonNeedsRepeat, true
	case diverged:
		out.Status = model.StatusDiverged
	case equal == len(out.Observations):
		out.Status = model.StatusNotDiverged
	default:
		out.Status, out.Reason = model.StatusUnverified, reasonNothingDecided
	}
	return out
}

// compareKey builds the row of one key (see Compare).
func compareKey(k Key, base, candidate Set, repeat *Set) model.Observation {
	row := model.Observation{Test: k.Test, Key: k.Name, BaseRecorded: base.present(k), CandidateRecorded: candidate.present(k)}
	bvals, cvals := base.values[k], candidate.values[k]
	var cutB, cutC bool
	if len(bvals) > 0 {
		row.Base, cutB = display(bvals[0])
	}
	if len(cvals) > 0 {
		row.Candidate, cutC = display(cvals[0])
	}
	row.Truncated = cutB || cutC
	incomparable := func(reason string) model.Observation {
		row.Status, row.Reason = model.ObservationIncomparable, reason
		return row
	}
	sides := []Set{base, candidate}
	if repeat != nil {
		sides = append(sides, *repeat)
	}
	if reason := keyProblem(k.Name); reason != "" {
		return incomparable(reason)
	}
	for _, s := range sides {
		if reason, ok := s.invalid[k]; ok {
			return incomparable(reason)
		}
	}
	for _, s := range sides {
		if len(s.values[k]) > 1 {
			return incomparable(reasonDuplicate)
		}
	}
	for _, s := range sides {
		for _, v := range s.values[k] {
			if reason := valueProblem(v); reason != "" {
				return incomparable(reason)
			}
		}
	}
	switch {
	case len(bvals) == 0 && len(cvals) == 0:
		row.Status, row.Reason = model.ObservationUnstable, reasonRepeatOnly
		return row
	case len(bvals) == 0 || len(cvals) == 0:
		return incomparable(reasonOneRevision)
	}
	bv, cv := bvals[0], cvals[0]
	var rvals []string
	if repeat != nil {
		rvals = repeat.values[k]
	}
	repeatAgrees := len(rvals) == 1 && rvals[0] == bv
	if bv == cv {
		if repeat != nil && !repeatAgrees {
			row.Status, row.Reason = model.ObservationUnstable, reasonRepeatDiffers
			return row
		}
		row.Status = model.ObservationEqual
		return row
	}
	if buildDependent(bv) || buildDependent(cv) {
		return incomparable(reasonUnstableValue)
	}
	if repeat == nil {
		return incomparable(reasonAwaitingRepeat)
	}
	if !repeatAgrees {
		row.Status, row.Reason = model.ObservationUnstable, reasonRepeatDiffers
		return row
	}
	row.Status = model.ObservationDiverged
	return row
}

// Differ reports whether the baseline and the candidate may have recorded
// different things. It is used only to withdraw a both-pass NOT_REPRODUCED
// conclusion, so that an observed difference is never hidden behind it, and it
// fails closed: whatever does not establish equal full values counts as a
// possible difference. That is:
//   - a channel error on either side (the observations could not be read);
//   - a key present on one side only;
//   - a key that either side recorded without a readable value (for example a
//     Go value too long for the test runner to convert), since equality is
//     then unknown;
//   - a key name or a value that holds the redaction marker, since equal
//     redacted texts do not establish equal original values;
//   - a key recorded on both sides with at least one pair of different full
//     values.
//
// Other validity rules (duplicates, bounds, key syntax, build-dependent
// values) are ignored: they decide what may support a divergence, never what
// may hide one. A value that is not a Redact fixed point but holds no marker
// was not altered by redaction (it was decoded from an escaped form), so it is
// compared exactly like any other value.
func Differ(base, candidate Set) bool {
	if base.err != "" || candidate.err != "" {
		return true
	}
	for _, pair := range [][2]Set{{base, candidate}, {candidate, base}} {
		a, b := pair[0], pair[1]
		for _, k := range a.order {
			if !b.present(k) || strings.Contains(k.Name, redact.Marker) {
				return true
			}
			if _, unreadable := a.invalid[k]; unreadable {
				return true
			}
			for _, av := range a.values[k] {
				if strings.Contains(av, redact.Marker) {
					return true
				}
				for _, bv := range b.values[k] {
					if av != bv {
						return true
					}
				}
			}
		}
	}
	return false
}
