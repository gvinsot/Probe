package fuzz

import (
	"fmt"
	"sort"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// MaxDivergenceRows bounds the divergent inputs listed for one function
// (divergence observations, Appendix A).
const MaxDivergenceRows = 32

// Checks are the recorded checks of one package run in their roles. The
// confirmation pair is nil when it did not run.
type Checks struct {
	Base, Candidate, BaseConfirm, CandidateConfirm *model.Check
}

// Evaluation is the outcome of one fuzz test, derived only from recorded
// checks. Inputs = Compared + Unstable + Unconfirmed + NotRecorded, and
// Diverged <= Compared.
type Evaluation struct {
	Outcome        string // model.FuzzDiverged, model.FuzzNotDiverged or model.FuzzInconclusive
	Reason         string // why the outcome is inconclusive
	Inputs         int
	Compared       int
	Diverged       int
	Unstable       int
	Unconfirmed    int
	NotRecorded    int
	Counterexample *model.FuzzCounterexample
	// CounterexampleCut reports that a counterexample display is not the whole
	// recorded encoding (cut, redacted, or bounded in the sandbox), so the two
	// values shown may look identical although their encodings differ.
	CounterexampleCut bool
	// Differs reports that the first pair recorded different values for at
	// least one input that neither revision flagged as unstable, so a
	// confirmation pair is needed.
	Differs   bool
	divergent []divergentInput
}

type divergentInput struct {
	index           int
	base, candidate Record
}

// EvidenceStatus maps the outcome to the status of its differential_fuzz
// evidence: DIVERGED, NOT_DIVERGED or UNVERIFIED.
func (e Evaluation) EvidenceStatus() string {
	switch e.Outcome {
	case model.FuzzDiverged:
		return model.StatusDiverged
	case model.FuzzNotDiverged:
		return model.StatusNotDiverged
	}
	return model.StatusUnverified
}

// Observations returns the divergent inputs as observation rows of test,
// counterexample first, then by call length and index, at most
// MaxDivergenceRows. Base and Candidate are the redacted display cuts;
// Truncated is set when either display is not the whole recorded encoding
// (Record.Whole), so identical-looking values are never shown as complete.
func (e Evaluation) Observations(test string) []model.Observation {
	rows := []model.Observation{}
	for _, d := range e.divergent {
		if len(rows) == MaxDivergenceRows {
			break
		}
		rows = append(rows, model.Observation{
			Test: test, Key: d.base.Call, Status: model.ObservationDiverged,
			Base: d.base.Display, Candidate: d.candidate.Display,
			BaseRecorded: true, CandidateRecorded: true,
			Truncated: !d.base.Whole() || !d.candidate.Whole(),
		})
	}
	return rows
}

// side is what one recorded run says about one fuzz test.
type side struct {
	name    string          // "baseline" or "candidate" (first pair and confirmation alike)
	present bool            // a check was given
	usable  bool            // the named execution is validated: records may be compared
	problem string          // why the side is not usable
	stream  *FunctionStream // the function's stream, even when not usable (for reason texts)
	ended   bool            // the test did not pass and its stream shows where the process ended
	pkg     string
}

func (s side) record(i int) *Record {
	if !s.usable || s.stream == nil || i >= len(s.stream.Records) {
		return nil
	}
	return &s.stream.Records[i]
}

// Recorded holds the checks of one package run with their streams parsed
// once, so that every function of the package is evaluated without parsing
// the streams again.
type Recorded struct {
	checks  Checks
	streams [4]*Stream // nil when the check is absent or its stream does not parse
}

// ParseChecks parses the streams of the given checks once.
func ParseChecks(c Checks) Recorded {
	r := Recorded{checks: c}
	for i, check := range []*model.Check{c.Base, c.Candidate, c.BaseConfirm, c.CandidateConfirm} {
		if check == nil || check.Results == "" {
			continue
		}
		if s, err := ParseResults(check.Results); err == nil {
			r.streams[i] = &s
		}
	}
	return r
}

// view validates one check for one fuzz test. A side is usable only when the
// check is a live (or, for the first baseline run, a cache entry that two
// live runs agreed on) PASS or FAIL run of the expected kind with an
// untruncated log that records exactly one run and one pass of the test in
// one package, and its normalized stream parses and plans the same number of
// inputs. A FAIL check can still be usable for the tests that passed before
// its process ended.
func view(c *model.Check, stream *Stream, name, kind, test string, planned int, allowAgreedReplay bool) side {
	s := side{name: name, present: c != nil}
	switch {
	case c == nil:
		s.problem = "did not run"
		return s
	case c.Kind != kind:
		s.problem = "is not a " + kind + " check"
		return s
	case c.Replayed() && !allowAgreedReplay:
		s.problem = "was replayed from the execution cache; only live runs are accepted for this check"
		return s
	case c.Replayed() && c.Cache.LiveRuns < 2:
		s.problem = "was replayed from the execution cache without two agreeing live runs"
		return s
	case c.Status != "PASS" && c.Status != "FAIL":
		s.problem = "ended with status " + c.Status
		return s
	case c.Status == "PASS" && c.ExitCode != 0 || c.Status == "FAIL" && (c.ExitCode < 1 || c.ExitCode > 124):
		s.problem = fmt.Sprintf("ended with exit code %d", c.ExitCode)
		return s
	case c.Truncated:
		s.problem = "log was truncated"
		return s
	case c.Results == "":
		s.problem = "recorded no observation stream"
		return s
	}
	if stream == nil {
		s.problem = "observation stream was rejected"
		return s
	}
	fn, ok := stream.Function(test)
	if !ok {
		s.problem = "observation stream has no records of the fuzz test"
		return s
	}
	if fn.Planned != planned {
		s.problem = "observation stream planned a different number of inputs"
		return s
	}
	s.stream = &fn
	action, pkg := harness.GoTestOutcome(c.Output, test)
	switch {
	case action != "pass" || pkg == "":
		s.problem = "log does not record exactly one run and one pass of the fuzz test"
		s.ended = fn.State == StateInterrupted || fn.State == StateNotStarted
	case fn.State == StateInterrupted || fn.State == StateNotStarted:
		s.problem = "observation stream ended although the fuzz test passed"
	default:
		s.usable, s.pkg = true, pkg
	}
	return s
}

// reason explains why a side cannot support a comparison, pointing at the
// input being evaluated when the process ended or the function stopped.
func (s side) reason() string {
	if s.ended {
		switch s.stream.State {
		case StateInterrupted:
			if s.stream.AtCall != "" {
				return fmt.Sprintf("the %s process ended while evaluating input %d: %s", s.name, s.stream.At, shortText(s.stream.AtCall, 160))
			}
			return fmt.Sprintf("the %s process ended before the function's end record", s.name)
		case StateNotStarted:
			return fmt.Sprintf("the %s process ended before this function was evaluated", s.name)
		}
	}
	return "the " + s.name + " run " + s.problem
}

// stopReason explains a usable side whose function stopped early.
func (s side) stopReason() string {
	f := s.stream
	switch f.Stop {
	case StopPoisoned:
		return fmt.Sprintf("the %s skipped this function because an earlier call in the same process exceeded call_timeout_ms", s.name)
	case StopTimeout:
		return fmt.Sprintf("the %s stopped (timeout) while evaluating input %d: %s", s.name, f.At, shortText(f.AtCall, 160))
	case StopGoexit:
		return fmt.Sprintf("the %s stopped (goexit) while evaluating input %d: %s", s.name, f.At, shortText(f.AtCall, 160))
	}
	return fmt.Sprintf("the %s stopped (abort) while evaluating input %d: %s", s.name, f.At, shortText(f.AtCall, 160))
}

// Evaluate derives the outcome of one fuzz test from recorded checks. It is
// the single comparison rule: the orchestrator records its result, and a
// verifier re-derives it from a saved report.
//
// For each planned input: a missing record on either side of the first pair
// is not recorded; an in-process instability flag on either side is unstable;
// equal hashes and lengths are compared; a difference needs the confirmation
// pair, where each revision must reproduce its own first hash without an
// instability flag (else unstable), and then the input is compared and
// diverged; without confirmation records it is unconfirmed.
//
// Two kinds of unstable input separate the revisions: an input flagged
// unstable on the candidate only (the baseline evaluated it the same way
// twice), and a first-pair difference that the confirmation pair did not
// reproduce. They are counted as unstable, never as diverged, but they keep
// the function from being not_diverged.
//
// Outcome: diverged when any input diverged; else inconclusive when a
// difference was unconfirmed; else not_diverged when both first-pair streams
// are complete, at least one input was compared and no unstable input
// separates the revisions; else inconclusive with a precise reason. The
// counterexample is the divergent input with the shortest call, then the
// lowest index, among the divergent inputs whose two displays differ; when
// every divergent input shows identical displays (the difference lies in a
// cut or redacted part), it is the shortest call overall and
// CounterexampleCut is set.
func Evaluate(test string, planned int, c Checks) Evaluation {
	return ParseChecks(c).Evaluate(test, planned)
}

// Evaluate is Evaluate over checks whose streams are already parsed.
func (r Recorded) Evaluate(test string, planned int) Evaluation {
	c := r.checks
	ev := Evaluation{Outcome: model.FuzzInconclusive, Inputs: planned, NotRecorded: planned}
	if planned < 1 {
		ev.Inputs, ev.NotRecorded = 0, 0
		ev.Reason = "no input was planned"
		return ev
	}
	b1 := view(c.Base, r.streams[0], "baseline", model.CheckFuzzBase, test, planned, true)
	c1 := view(c.Candidate, r.streams[1], "candidate", model.CheckFuzzCandidate, test, planned, false)
	confirm := c.BaseConfirm != nil || c.CandidateConfirm != nil
	var b2, c2 side
	if confirm {
		b2 = view(c.BaseConfirm, r.streams[2], "baseline confirmation", model.CheckFuzzBaseConfirm, test, planned, false)
		c2 = view(c.CandidateConfirm, r.streams[3], "candidate confirmation", model.CheckFuzzCandidateConfirm, test, planned, false)
	}
	if reason := consistent(c, []side{b1, c1, b2, c2}); reason != "" {
		ev.Reason = reason
		return ev
	}
	ev.NotRecorded = 0
	separating := 0 // unstable inputs that separate the revisions
	for i := 0; i < planned; i++ {
		rb, rc := b1.record(i), c1.record(i)
		switch {
		case rb == nil || rc == nil:
			ev.NotRecorded++
		case rb.Unstable || rc.Unstable:
			ev.Unstable++
			if !rb.Unstable {
				separating++
			}
		case rb.SHA256 == rc.SHA256 && rb.Length == rc.Length:
			ev.Compared++
		default:
			ev.Differs = true
			rb2, rc2 := b2.record(i), c2.record(i)
			switch {
			case rb2 == nil || rc2 == nil:
				ev.Unconfirmed++
			case rb2.Unstable || rc2.Unstable || rb2.SHA256 != rb.SHA256 || rb2.Length != rb.Length || rc2.SHA256 != rc.SHA256 || rc2.Length != rc.Length:
				ev.Unstable++
				separating++
			default:
				ev.Compared++
				ev.Diverged++
				ev.divergent = append(ev.divergent, divergentInput{index: i, base: *rb, candidate: *rc})
			}
		}
	}
	switch {
	case ev.Diverged > 0:
		sort.SliceStable(ev.divergent, func(i, j int) bool {
			a, b := ev.divergent[i], ev.divergent[j]
			if len(a.base.Call) != len(b.base.Call) {
				return len(a.base.Call) < len(b.base.Call)
			}
			return a.index < b.index
		})
		// Prefer a counterexample whose displays show the difference.
		for k, d := range ev.divergent {
			if d.base.Display != d.candidate.Display {
				copy(ev.divergent[1:k+1], ev.divergent[:k])
				ev.divergent[0] = d
				break
			}
		}
		d := ev.divergent[0]
		ev.Outcome = model.FuzzDiverged
		ev.Counterexample = &model.FuzzCounterexample{Index: d.index, Input: d.base.Call, Base: d.base.Display, Candidate: d.candidate.Display}
		ev.CounterexampleCut = !d.base.Whole() || !d.candidate.Whole()
	case ev.Unconfirmed > 0:
		ev.Reason = "a difference seen in one run per revision could not be confirmed"
		if !confirm {
			ev.Reason += ": the confirmation runs did not take place"
		} else if !b2.usable {
			ev.Reason += ": " + b2.reason()
		} else if !c2.usable {
			ev.Reason += ": " + c2.reason()
		}
	case b1.usable && c1.usable && b1.stream.State == StateComplete && c1.stream.State == StateComplete && ev.Compared > 0 && separating == 0:
		ev.Outcome = model.FuzzNotDiverged
	case !b1.usable:
		ev.Reason = b1.reason()
	case !c1.usable:
		ev.Reason = c1.reason()
	case b1.stream.State != StateComplete:
		ev.Reason = b1.stopReason()
	case c1.stream.State != StateComplete:
		ev.Reason = c1.stopReason()
	case ev.Unstable == planned:
		ev.Reason = fmt.Sprintf("all %d inputs gave different observations on repeated evaluation", planned)
	case separating > 0:
		ev.Reason = fmt.Sprintf("%d of %d inputs were unstable on the candidate only, or differed between the revisions without repeating in the confirmation runs", separating, planned)
	default:
		ev.Reason = "no input was compared"
	}
	return ev
}

// consistent checks that the given checks describe one experiment: one
// identical recorded command, one package for the fuzz test, and the same
// inputs at every index. It returns a reason when they do not.
func consistent(c Checks, sides []side) string {
	var command []string
	for _, check := range []*model.Check{c.Base, c.Candidate, c.BaseConfirm, c.CandidateConfirm} {
		if check == nil {
			continue
		}
		if len(check.Command) == 0 {
			return "a run recorded no command"
		}
		if command == nil {
			command = check.Command
		} else if !equalStrings(command, check.Command) {
			return "the runs did not use one identical recorded command"
		}
	}
	pkg := ""
	calls := map[int]string{}
	for _, s := range sides {
		if !s.usable {
			continue
		}
		if pkg == "" {
			pkg = s.pkg
		} else if s.pkg != pkg {
			return "the runs recorded the fuzz test in different packages"
		}
		for _, r := range s.stream.Records {
			if call, ok := calls[r.Index]; ok && call != r.Call {
				return "the recorded streams do not describe the same inputs"
			}
			calls[r.Index] = r.Call
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
