package fuzz

import (
	"fmt"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

// scriptPackage stands for the package of a TS/JS side in consistent: every
// run of one TS/JS harness executes one module's harness file.
const scriptPackage = "(TS/JS module)"

// endedInside reports whether a stream ended inside a function: one of its
// functions has a head record without its done record, so the process ended
// while that function was evaluated.
func (s *Stream) endedInside() bool {
	for _, f := range s.Functions {
		if f.State == StateInterrupted {
			return true
		}
	}
	return false
}

// scriptView is view for a TS/JS harness (runner jest_json). The named
// execution of the test is validated from the stream's own framing, never
// from the framework's log (Appendix D.13): the check is a live (or, for the
// first baseline run, an agreed replayed) PASS run with exit code 0 of the
// expected kind; its payload was one complete frame that normalized into a
// TS/JS stream (so it was not truncated); the stream plans the same number of
// inputs; and the test wrote its head and done records, which leaves it
// complete or stopped. A truncated log does not matter. A failing run
// supports no test: its stream still names the input being evaluated when
// the process ended. So does a passing run whose stream ended inside a test
// (under Jest, a process.exit(0) in the code under test ends the run with
// exit code 0): its later tests did not start.
func scriptView(c *model.Check, stream *Stream, name, kind, test string, planned int, allowAgreedReplay bool) side {
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
	case c.Results == "" && c.Status == "FAIL":
		s.problem = "failed without recording an observation stream (for example, the module or the harness could not be loaded; see the check log)"
		return s
	case c.Results == "":
		s.problem = "recorded no observation stream"
		return s
	case stream == nil:
		s.problem = "observation stream was rejected"
		return s
	case stream.Runner != harness.RunnerJest:
		s.problem = "observation stream is not the stream of a TS/JS harness"
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
	ended := fn.State == StateInterrupted || fn.State == StateNotStarted
	switch {
	case c.Status != "PASS" || c.ExitCode != 0:
		s.problem = fmt.Sprintf("ended with exit code %d; a TS/JS run supports a comparison only when it passes with exit code 0", c.ExitCode)
		s.ended = ended
	case ended:
		s.problem = "observation stream has no done record of the fuzz test although the run passed"
		s.ended = stream.endedInside()
	default:
		s.usable, s.pkg = true, scriptPackage
	}
	return s
}
