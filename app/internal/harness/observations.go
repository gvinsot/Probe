package harness

// The observation oracle (F1): a generated test may record values instead of
// asserting them. Go tests record them with testing.T.Attr (Go 1.25 or later
// in the sandbox image), which go test -json turns into "attr" events inside
// the recorded check log; Vitest tests record them in task.meta.probe,
// which the Jest-compatible JSON report carries on the {results_out} payload
// channel; pytest tests record them with the record_property fixture under a
// "probe." name, which pytest's JUnit XML report carries on the same channel. When the generated test passes on both revisions, the recorded
// values are compared key by key (package observe). A key whose candidate value
// differs is decided by exactly one extra, live baseline run.
//
// Both channels can be written by code executing in the sandbox: they are kept
// apart from ordinary log text so that unrelated output cannot impersonate an
// observation, and nothing more. The outcome is recomputed by report.Finalize
// from the recorded checks with EvaluateObservations.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/observe"
	"github.com/gvinsot/Probe/app/internal/redact"
)

// The harness holds no observation state: each observation experiment is
// decided within one run_generated_test call.

// Fixed channel-error and outcome reasons. They never contain recorded text.
const (
	reasonAttrUnparsable   = "an observation event could not be parsed (it may have been altered by redaction)"
	reasonLogUnreadable    = "the check log could not be read line by line"
	reasonLineLimit        = "the value exceeded the runner's line limit and was not converted into an observation event"
	reasonResultsBad       = "the runner's JSON report could not be read"
	reasonNoNames          = "no generated test name is known"
	reasonCommandsDiffer   = "the baseline and candidate runs used different commands"
	reasonNotBothPass      = "observations are compared only when the generated test passes on both revisions"
	reasonRepeatCommand    = "the baseline repeat used a different command"
	reasonRepeatNotPass    = "the baseline repeat run did not pass; the stability of baseline values is unknown"
	reasonDeclaredNotFound = "the test declares observations but none was recorded; Go needs a 1.25 or later toolchain for t.Attr, Jest reports carry no per-test metadata, and pytest records properties only with the record_property fixture"
	reasonNotRetained      = "the diverging generated test could not be retained as an artifact, so the difference is not recorded as a divergence"
)

// Normalized Vitest meta values that stand for observations Probe did not
// keep. They replace whatever the runner reported, so no untrusted text
// survives, and they are fixed points of normalizeJestMeta.
const (
	metaNotObject   = "probe: observations were not a key/value object"
	metaTooMany     = "probe: more than 32 observations were recorded"
	metaCollision   = "probe: two keys became equal after redaction"
	metaUnstable    = "probe: the observations would be altered by redaction"
	metaUnencodable = "probe: the observations could not be encoded"
	metaDropped     = "probe: observations were dropped because the report would be altered by redaction"
	metaTooLarge    = "probe: observations were dropped because the report would exceed its size budget"
)

// metaReasons maps each marker to the channel-error reason it stands for.
var metaReasons = map[string]string{
	metaNotObject:   "the runner reported observations that are not a key/value object",
	metaTooMany:     "more than 32 distinct keys were recorded in one run",
	metaCollision:   "two keys became equal after redaction",
	metaUnstable:    "the recorded observations would be altered by redaction",
	metaUnencodable: "the recorded observations could not be encoded",
	metaDropped:     "the recorded observations were dropped because the runner's report would be altered by redaction",
	metaTooLarge:    "the recorded observations were dropped because the runner's report would exceed the payload or results budget",
}

// jestMeta is the part of a Vitest assertion's task.meta that observation
// experiments use. Its decoding never fails, so an unexpected meta shape can
// never make a runner report unreadable: a meta that is not an object keeps
// only a fixed marker.
type jestMeta struct {
	Observations json.RawMessage `json:"probe,omitempty"`
}

// UnmarshalJSON keeps the "probe" member of a meta object. A meta that is
// not an object becomes a marker; it is never a decoding error.
func (m *jestMeta) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		*m = jestMeta{}
		if !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			m.Observations = metaMarker(metaNotObject)
		}
		return nil
	}
	*m = jestMeta{Observations: object[observe.MetaField]}
	return nil
}

func metaMarker(text string) json.RawMessage {
	b, _ := json.Marshal(text)
	return b
}

// canonicalJSON encodes a decoded JSON value (decoded with UseNumber)
// canonically: object keys sorted, numbers as written, no HTML escaping and no
// insignificant whitespace. A recorded Vitest value is compared as this text,
// so its JSON type stays part of the value: the number 4 is 4 and the string
// "4" is "4", quoted.
func canonicalJSON(v any) (string, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// keptMetaValue returns the canonical text normalizeJestMeta keeps for one
// recorded value. A string is redacted. A value whose canonical text is still
// not a Redact fixed point (for example an object holding "password":"x")
// becomes the redaction marker as a string, and a text longer than
// observe.MaxValueBytes becomes its stand-in (observe.Oversized) as a string,
// so no kept value can be compared beyond those bounds or inflate the report.
// The result is a fixed point of keptMetaValue.
func keptMetaValue(v any) (string, error) {
	if s, isString := v.(string); isString {
		v = redact.Redact(s)
	}
	text, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	if !redact.IsFixedPoint(text) {
		text = `"` + redact.Marker + `"`
	}
	if len(text) > observe.MaxValueBytes {
		return canonicalJSON(observe.Oversized(text))
	}
	return text, nil
}

// normalizeJestMeta keeps only a canonical, redacted form of the observations:
// a JSON object of at most observe.MaxKeys keys whose keys are redacted (a key
// longer than observe.MaxKeyBytes becomes its stand-in, observe.Oversized),
// whose values are kept by keptMetaValue with their JSON type, and whose
// encoding is a Redact fixed point. Anything else becomes one fixed marker
// string, so no untrusted text of a rejected meta is kept. It returns nil when
// there is nothing to keep, and it is idempotent.
func normalizeJestMeta(m *jestMeta) *jestMeta {
	if m == nil {
		return nil
	}
	if trimmed := bytes.TrimSpace(m.Observations); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	marker := func(text string) *jestMeta { return &jestMeta{Observations: metaMarker(text)} }
	var text string
	if json.Unmarshal(m.Observations, &text) == nil {
		if _, known := metaReasons[text]; known {
			return marker(text)
		}
		return marker(metaNotObject)
	}
	decoder := json.NewDecoder(bytes.NewReader(m.Observations))
	decoder.UseNumber()
	var raw any
	if decoder.Decode(&raw) != nil || decoder.Decode(new(any)) != io.EOF {
		return marker(metaNotObject)
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return marker(metaNotObject)
	}
	if len(object) > observe.MaxKeys {
		return marker(metaTooMany)
	}
	kept := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		text, err := keptMetaValue(value)
		if err != nil {
			return marker(metaUnencodable)
		}
		key = redact.Redact(key)
		if len(key) > observe.MaxKeyBytes {
			key = observe.Oversized(key)
		}
		if _, taken := kept[key]; taken {
			return marker(metaCollision)
		}
		kept[key] = json.RawMessage(text)
	}
	encoded, err := canonicalJSON(kept)
	if err != nil {
		return marker(metaUnencodable)
	}
	if !redact.IsFixedPoint(encoded) {
		return marker(metaUnstable)
	}
	return &jestMeta{Observations: json.RawMessage(encoded)}
}

// dropJestMeta replaces every kept meta with the fixed marker text. It is used
// when the normalized report as a whole is not a Redact fixed point, or would
// exceed the payload or results budget, so that observations can never make
// an otherwise readable report unreadable. It reports whether anything
// changed.
func dropJestMeta(report *jestReport, text string) bool {
	changed := false
	for i := range report.TestResults {
		for j := range report.TestResults[i].AssertionResults {
			a := &report.TestResults[i].AssertionResults[j]
			if a.Meta != nil {
				a.Meta = &jestMeta{Observations: metaMarker(text)}
				changed = true
			}
		}
	}
	return changed
}

// ObservationSet extracts what one recorded check observed for the named
// tests: Go attr events from the check log, or Vitest meta from the check's
// structured results. It returns false for a runner without an observation
// channel. It does not validate the execution; EvaluateObservations does.
func ObservationSet(runner string, c model.Check, path string, names []string) (observe.Set, bool) {
	switch runner {
	case RunnerGo:
		return extractGoObservations(c.Output, names), true
	case RunnerJest:
		return extractJestObservations(c.Results, path, names), true
	case RunnerPytest:
		return extractPytestObservations(c.Results, path, names), true
	}
	return observe.Set{}, false
}

// goEvent is the subset of a go test -json event that carries observations.
type goEvent struct {
	Action string  `json:"Action"`
	Test   string  `json:"Test"`
	Key    *string `json:"Key"`
	Value  string  `json:"Value"` // test2json omits an empty value
	Output string  `json:"Output"`
}

// extractGoObservations reads the "attr" events of exactly the named top-level
// tests whose key starts with observe.GoKeyPrefix. Subtests, other tests and
// other keys are ignored, and so is ordinary output: only a line framed by the
// test runner becomes an attr event. A framed line too long for test2json to
// convert stays an output event; its key is recorded as unreadable. An attr
// line that no longer parses (for example after redaction) is a channel error.
func extractGoObservations(output string, names []string) observe.Set {
	var s observe.Set
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxFileBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event goEvent
		if err := json.Unmarshal(line, &event); err != nil {
			if bytes.Contains(line, []byte(`"Action":"attr"`)) {
				s.Fail(reasonAttrUnparsable)
			}
			continue
		}
		if !wanted[event.Test] {
			continue
		}
		switch event.Action {
		case "attr":
			if event.Key == nil {
				s.Fail(reasonAttrUnparsable)
				continue
			}
			if strings.HasPrefix(*event.Key, observe.GoKeyPrefix) {
				s.Record(event.Test, strings.TrimPrefix(*event.Key, observe.GoKeyPrefix), event.Value)
			}
		case "output":
			prefix := "\x16=== ATTR  " + event.Test + " " + observe.GoKeyPrefix
			if rest, ok := strings.CutPrefix(event.Output, prefix); ok {
				name, _, _ := strings.Cut(rest, " ")
				s.Invalidate(event.Test, strings.TrimRight(name, "\n"), reasonLineLimit)
			}
		}
	}
	if scanner.Err() != nil {
		s.Fail(reasonLogUnreadable)
	}
	return s
}

// extractJestObservations reads the normalized meta of exactly the named
// top-level tests in the report entry for the generated file.
//
// A check without a report (a run that ended in ERROR, TIMEOUT or SKIPPED, or
// whose report could not be recorded) and a report without exactly one entry
// for the file recorded nothing: the set is empty, not a channel error, so an
// ordinary experiment whose run did not produce a report gains no observation
// record. Whether the named executions passed is decided by
// EvaluateObservations, never here. Only results that are present but do not
// parse (which a harness-recorded check never holds) are a channel error.
func extractJestObservations(results, path string, names []string) observe.Set {
	var s observe.Set
	if results == "" {
		return s
	}
	var report jestReport
	if json.Unmarshal([]byte(results), &report) != nil {
		s.Fail(reasonResultsBad)
		return s
	}
	want := "/workspace/" + filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	var file *jestFile
	for i := range report.TestResults {
		if report.TestResults[i].Name == want {
			if file != nil {
				return observe.Set{}
			}
			file = &report.TestResults[i]
		}
	}
	if file == nil {
		return s
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	for _, a := range file.AssertionResults {
		if len(a.AncestorTitles) != 0 || !wanted[a.Title] || a.Meta == nil || len(bytes.TrimSpace(a.Meta.Observations)) == 0 {
			continue
		}
		recordMeta(&s, a.Title, a.Meta.Observations)
	}
	return s
}

// recordMeta records the entries of one normalized meta object in order, so
// that a key present twice in a (tampered) report is seen twice. Each value is
// recorded as its canonical JSON text (canonicalJSON), so its type is part of
// what is compared. A marker or anything but an object is a channel error.
func recordMeta(s *observe.Set, test string, raw json.RawMessage) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		reason, known := metaReasons[text]
		if !known {
			reason = metaReasons[metaNotObject]
		}
		s.Fail(reason)
		return
	}
	notObject := metaReasons[metaNotObject]
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		s.Fail(notObject)
		return
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, isKey := keyToken.(string)
		if err != nil || !isKey {
			s.Fail(notObject)
			return
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			s.Fail(notObject)
			return
		}
		text, err := canonicalJSON(value)
		if err != nil {
			s.Fail(notObject)
			return
		}
		s.Record(test, key, text)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		s.Fail(notObject)
	}
}

// EvaluateObservations re-derives the observation outcome of one generated
// test from its recorded checks. The harness uses it while the experiment runs
// and report.Finalize uses it to verify the stored status. It returns false for
// a runner without an observation channel.
//
// The base and candidate checks must use the same command and pass validated,
// named executions with exit code 0; a repeat, when given, must use the same
// command and pass too. Otherwise the outcome is UNVERIFIED. Check kinds and
// the live-baseline rule are the verifier's concern.
func EvaluateObservations(runner string, base, candidate model.Check, repeat *model.Check, path string, names []string) (observe.Outcome, bool) {
	if runner != RunnerGo && runner != RunnerJest && runner != RunnerPytest {
		return observe.Outcome{}, false
	}
	if len(names) == 0 {
		return observe.Unverified(reasonNoNames), true
	}
	if !sameCommand(base.Command, candidate.Command) {
		return observe.Unverified(reasonCommandsDiffer), true
	}
	b, _ := ValidateExecution(runner, base, path, names)
	c, _ := ValidateExecution(runner, candidate, path, names)
	if b.Status != "PASS" || b.ExitCode != 0 || c.Status != "PASS" || c.ExitCode != 0 {
		return observe.Unverified(reasonNotBothPass), true
	}
	baseSet, _ := ObservationSet(runner, base, path, names)
	candidateSet, _ := ObservationSet(runner, candidate, path, names)
	if repeat == nil {
		return observe.Compare(baseSet, candidateSet, nil), true
	}
	if !sameCommand(repeat.Command, base.Command) {
		return observe.Unverified(reasonRepeatCommand), true
	}
	if r, _ := ValidateExecution(runner, *repeat, path, names); r.Status != "PASS" || r.ExitCode != 0 {
		return observe.Unverified(reasonRepeatNotPass), true
	}
	repeatSet, _ := ObservationSet(runner, *repeat, path, names)
	return observe.Compare(baseSet, candidateSet, &repeatSet), true
}

func sameCommand(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// declaresObservations reports whether a generated test's source looks like an
// observation experiment. It only decides whether an experiment that recorded
// nothing still gets an UNVERIFIED observation record that says so.
func declaresObservations(path, content string) bool {
	if strings.HasSuffix(path, "_test.go") {
		return strings.Contains(content, ".Attr(") && strings.Contains(content, `"`+observe.GoKeyPrefix)
	}
	if isJSTestPath(path) {
		return strings.Contains(content, "meta") && strings.Contains(content, observe.MetaField)
	}
	if isPyTestPath(path) {
		return strings.Contains(content, "record_property") && strings.Contains(content, observe.GoKeyPrefix)
	}
	return false
}

// observeGenerated compares the values a generated test recorded on the
// baseline and the candidate. It runs while the generated file is still
// staged, after the differential_test evidence was recorded. Caller holds h.mu.
//
// It records nothing, and returns nil, when neither run recorded an
// observation and the source declares none, so ordinary experiments are
// unchanged; a run that produced no report (ERROR, TIMEOUT, SKIPPED) recorded
// none. Otherwise it records one differential_observation evidence
// record. When a key differs it runs exactly one live baseline repeat
// (generated_test_base_repeat, never served from the execution cache and
// charged to the runtime budget). A DIVERGED experiment retains the generated
// test as a hashed artifact, and deleting it is refused.
func (h *Harness) observeGenerated(ctx context.Context, t *generatedTest, runner string, command, names []string, base, candidate model.Check) map[string]any {
	if runner == "" {
		return nil
	}
	baseSet, _ := ObservationSet(runner, base, t.Path, names)
	candidateSet, _ := ObservationSet(runner, candidate, t.Path, names)
	declared := declaresObservations(t.Path, t.Content)
	if baseSet.Empty() && candidateSet.Empty() && !declared {
		return nil
	}
	outcome, _ := EvaluateObservations(runner, base, candidate, nil, t.Path, names)
	var repeat *model.Check
	if outcome.NeedsRepeat {
		rc := h.runBaselineRepeat(ctx, runner, t.Path, names, command)
		repeat = &rc
		outcome, _ = EvaluateObservations(runner, base, candidate, repeat, t.Path, names)
	}
	e := model.Evidence{
		Kind:        model.EvidenceDifferentialObservation,
		Description: t.Description + " (" + outcome.Summary() + ")",
		Path:        t.Path,
		CheckID:     candidate.ID,
		BaseCheckID: base.ID,
		Status:      outcome.Status,
		Runner:      runner,
		TestNames:   append([]string(nil), names...),
	}
	if repeat != nil {
		e.RepeatCheckID = repeat.ID
	}
	if outcome.Status == model.StatusUnverified && outcome.Reason != reasonNotBothPass && baseSet.Empty() && candidateSet.Empty() && declared {
		e.Description += " (" + reasonDeclaredNotFound + ")"
	}
	if outcome.Status == model.StatusDiverged && !t.Reproduced {
		if err := h.saveArtifact(t.ID+"-"+filepath.Base(t.Path), model.ArtifactGeneratedTest, []byte(t.Content)); err != nil {
			e.Status = model.StatusUnverified
			e.Description += " (" + reasonNotRetained + ")"
		} else {
			t.Reproduced = true
		}
	}
	e = h.appendEvidence(e)
	result := map[string]any{"evidence": e, "observations": outcome.Observations}
	if e.Status == model.StatusUnverified && outcome.Reason != "" {
		result["reason"] = outcome.Reason
	}
	if repeat != nil {
		result["repeat_check"] = *repeat
	}
	return result
}

// runBaselineRepeat runs the staged generated test once more on the baseline,
// live, validates the named execution and records the validated check. Caller
// holds h.mu.
func (h *Harness) runBaselineRepeat(ctx context.Context, runner, path string, names, command []string) model.Check {
	var c model.Check
	if capturesResults(runner) {
		c = h.runWithResultsOptions(ctx, model.CheckGeneratedBaseRepeat, h.base, command, runOptions{live: true})
	} else {
		c, _, _ = h.runWithOptions(ctx, model.CheckGeneratedBaseRepeat, h.base, command, runOptions{live: true})
	}
	c, _ = ValidateExecution(runner, c, path, names)
	h.replaceCheck(c)
	return c
}
