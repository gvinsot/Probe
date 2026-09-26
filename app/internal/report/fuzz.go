package report

// The differential fuzzing report side (F2). Every differential_fuzz record is
// derived again here, from its recorded checks and their normalized
// observation streams, with the comparison the fuzz stage used
// (fuzz.Recorded.Evaluate); a stored status counts only when the derivation
// agrees with it and with the fuzz function that cites the record. A fuzz
// record never supports REPRODUCED, NOT_REPRODUCED or DISMISSED (see
// accepted), and nothing here sets an exit code.

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// Fixed texts of the fuzz section.
const (
	fuzzHeading = "\n## Differential Fuzzing\n"
	// fuzzRevalidationText replaces the reason of a function whose stored
	// diverged or not_diverged outcome the recorded checks do not support.
	fuzzRevalidationText = "the recorded outcome could not be derived again from the recorded checks and observation streams"
	// fuzzNoReasonText is the reason of an inconclusive function recorded
	// without one.
	fuzzNoReasonText = "no recorded run supports another outcome"
	// maxFuzzSkipsShown caps the skipped functions listed in Markdown; the JSON
	// keeps up to fuzz.MaxSkipped and counts every one.
	maxFuzzSkipsShown = 20
	// maxFuzzTargetText bounds a quoted input or reason in a review target.
	maxFuzzTargetText = 200
)

// fuzzDerived is one fuzz function that cites exactly one differential_fuzz
// record, with that record, its checks and the outcome derived again from
// them.
type fuzzDerived struct {
	fn                       model.FuzzFunction
	e                        model.Evidence
	base, candidate          model.Check
	baseConfirm, candConfirm *model.Check
	ev                       fuzz.Evaluation
}

// deriveFuzz evaluates every fuzz function whose evidence ID is cited by no
// other function and resolves to one differential_fuzz record whose checks
// resolve: the record names the function's test, runner go_test_json and a
// harness path that report sanitizing leaves unchanged; its check_id and
// base_check_id are the function's first pair (kinds fuzz_candidate and
// fuzz_base); the confirmation pair is complete or absent (kinds
// fuzz_candidate_confirm and fuzz_base_confirm); and the baseline command
// runs the harness's package and test. The streams of one set of checks are
// parsed once. The result is keyed by evidence ID. It is pure: it never
// modifies the report or the ledger.
func deriveFuzz(r *model.Report, l *ledger) map[string]fuzzDerived {
	out := map[string]fuzzDerived{}
	if r.Fuzz == nil {
		return out
	}
	cites := map[string]int{}
	for _, fn := range r.Fuzz.Functions {
		if fn.EvidenceID != "" {
			cites[fn.EvidenceID]++
		}
	}
	parsed := map[[4]string]fuzz.Recorded{}
	for _, fn := range r.Fuzz.Functions {
		if fn.EvidenceID == "" || cites[fn.EvidenceID] != 1 || fn.Checks == nil {
			continue
		}
		e, ok := l.item(fn.EvidenceID)
		if !ok || e.Kind != model.EvidenceDifferentialFuzz || e.Runner != harness.RunnerGo || len(e.TestNames) != 1 || !verifiableNames(e.TestNames) ||
			e.Path == "" || !verifiableText(e.Path) || fn.TestName != e.TestNames[0] || fn.Checks.Base != e.BaseCheckID || fn.Checks.Candidate != e.CheckID {
			continue
		}
		d := fuzzDerived{fn: fn, e: e}
		var baseOK, candOK bool
		d.base, baseOK = l.check(e.BaseCheckID)
		d.candidate, candOK = l.check(e.CheckID)
		if !baseOK || !candOK || d.base.Kind != model.CheckFuzzBase || d.candidate.Kind != model.CheckFuzzCandidate || !fuzzCommandTargets(d.base.Command, e.Path, fn.TestName) {
			continue
		}
		key := [4]string{e.BaseCheckID, e.CheckID}
		switch {
		case fn.Checks.BaseConfirm == "" && fn.Checks.CandidateConfirm == "":
		case fn.Checks.BaseConfirm == "" || fn.Checks.CandidateConfirm == "":
			continue
		default:
			bc, bok := l.check(fn.Checks.BaseConfirm)
			cc, cok := l.check(fn.Checks.CandidateConfirm)
			if !bok || !cok || bc.Kind != model.CheckFuzzBaseConfirm || cc.Kind != model.CheckFuzzCandidateConfirm {
				continue
			}
			d.baseConfirm, d.candConfirm = &bc, &cc
			key[2], key[3] = bc.ID, cc.ID
		}
		rec, seen := parsed[key]
		if !seen {
			rec = fuzz.ParseChecks(fuzz.Checks{Base: &d.base, Candidate: &d.candidate, BaseConfirm: d.baseConfirm, CandidateConfirm: d.candConfirm})
			parsed[key] = rec
		}
		d.ev = rec.Evaluate(fn.TestName, fn.Inputs)
		out[fn.EvidenceID] = d
	}
	return out
}

// fuzzCommandTargets reports whether a recorded fuzz command runs the package
// directory of the harness file p ("./dir", or "." for the root) and selects
// the test by name.
func fuzzCommandTargets(command []string, p, test string) bool {
	dir := path.Dir(p)
	pkg := "."
	if dir != "." {
		pkg = "./" + dir
	}
	target, selected := false, false
	for i, arg := range command {
		if arg == pkg {
			target = true
		}
		if arg == "-run" && i+1 < len(command) && strings.Contains(command[i+1], test) {
			selected = true
		}
	}
	return target && selected
}

// fuzzFunctionAgrees reports whether a stored fuzz function records exactly
// the derived evaluation: outcome, reason, counts and counterexample.
func fuzzFunctionAgrees(fn model.FuzzFunction, ev fuzz.Evaluation) bool {
	if fn.Outcome != ev.Outcome || fn.Reason != ev.Reason || fn.Inputs != ev.Inputs || fn.Compared != ev.Compared || fn.Diverged != ev.Diverged ||
		fn.Unstable != ev.Unstable || fn.Unconfirmed != ev.Unconfirmed || fn.NotRecorded != ev.NotRecorded {
		return false
	}
	if fn.Counterexample == nil || ev.Counterexample == nil {
		return fn.Counterexample == nil && ev.Counterexample == nil
	}
	return *fn.Counterexample == *ev.Counterexample
}

// verifyFuzz derives again the status of every differential_fuzz record that
// exactly one fuzz function cites. It returns an ID only when the derived
// status is DIVERGED or NOT_DIVERGED, equals the stored status, and the citing
// function records exactly the derived outcome, counts and counterexample.
// Live-baseline rule (§1.11): DIVERGED needs a confirmation pair that was
// executed, never replayed; NOT_DIVERGED may rest on a replayed first
// baseline run only when two agreeing live runs recorded it; a candidate-side
// check is never a replay.
func verifyFuzz(r *model.Report, l *ledger) map[string]string {
	out := map[string]string{}
	for id, d := range deriveFuzz(r, l) {
		status := d.ev.EvidenceStatus()
		if status != d.e.Status || !fuzzFunctionAgrees(d.fn, d.ev) || d.candidate.Replayed() {
			continue
		}
		switch status {
		case model.StatusDiverged:
			if d.baseConfirm == nil || !positiveBaseline(*d.baseConfirm) || d.candConfirm.Replayed() {
				continue
			}
		case model.StatusNotDiverged:
			if !negativeBaseline(d.base) {
				continue
			}
		default:
			continue
		}
		out[id] = status
	}
	return out
}

// fuzzDivergences returns one candidate divergence per diverged fuzz function
// whose evidence is verified DIVERGED: the function's path, line and symbol
// (anchor changed_function), the harness path and test name, the four checks
// [base, candidate, base_confirm, candidate_confirm], and the divergent inputs
// as rows (at most 32, the counterexample first, then by call length and
// index), derived again from the recorded streams.
func fuzzDivergences(r *model.Report, l *ledger) []model.Divergence {
	if r.Fuzz == nil {
		return nil
	}
	derived := deriveFuzz(r, l)
	var out []model.Divergence
	for _, fn := range r.Fuzz.Functions {
		if fn.Outcome != model.FuzzDiverged || l.verified[fn.EvidenceID] != model.StatusDiverged {
			continue
		}
		d, ok := derived[fn.EvidenceID]
		if !ok || d.baseConfirm == nil {
			continue
		}
		out = append(out, model.Divergence{
			EvidenceID:    fn.EvidenceID,
			Kind:          model.EvidenceDifferentialFuzz,
			Path:          fn.Path,
			Line:          fn.Line,
			Symbol:        fn.Symbol,
			AnchorSource:  anchorChangedFunction,
			TestPath:      d.e.Path,
			TestNames:     []string{fn.TestName},
			CheckIDs:      []string{d.base.ID, d.candidate.ID, d.baseConfirm.ID, d.candConfirm.ID},
			HypothesisIDs: []string{},
			Observations:  d.ev.Observations(fn.TestName),
			Note:          model.DivergenceNote,
		})
	}
	return out
}

// finalizeFuzz sets every function outcome from verified evidence and never
// from the stored outcome alone: diverged and not_diverged stay only when the
// function's evidence was verified with DIVERGED or NOT_DIVERGED; every other
// function becomes inconclusive, with a reason and without a counterexample.
// It also restores the fixed note and seed scheme. It mutates only r.Fuzz, is
// idempotent, and returns true (a human must look) for status not_run and for
// any function that is not not_diverged.
func finalizeFuzz(r *model.Report, l *ledger) bool {
	f := r.Fuzz
	if f == nil {
		return false
	}
	f.SeedScheme, f.Note = model.FuzzSeedScheme, model.FuzzNote
	if f.Functions == nil {
		f.Functions = []model.FuzzFunction{}
	}
	if f.Skipped == nil {
		f.Skipped = []model.FuzzSkip{}
	}
	if f.SkippedTotal < len(f.Skipped) {
		f.SkippedTotal = len(f.Skipped)
	}
	needsHuman := f.Status == model.FuzzNotRun
	for i := range f.Functions {
		fn := &f.Functions[i]
		want := ""
		switch fn.Outcome {
		case model.FuzzDiverged:
			want = model.StatusDiverged
		case model.FuzzNotDiverged:
			want = model.StatusNotDiverged
		}
		if want == "" || fn.EvidenceID == "" || l.verified[fn.EvidenceID] != want {
			if want != "" {
				fn.Reason = fuzzRevalidationText
			}
			fn.Outcome, fn.Counterexample = model.FuzzInconclusive, nil
			if strings.TrimSpace(fn.Reason) == "" {
				fn.Reason = fuzzNoReasonText
			}
		}
		if fn.Outcome != model.FuzzNotDiverged {
			needsHuman = true
		}
	}
	return needsHuman
}

// fuzzTargets returns the review targets of the fuzz section, read after
// finalizeFuzz: the candidate lines of each diverged function (high) and of
// each inconclusive function (medium).
func fuzzTargets(r *model.Report) []extraTarget {
	if r.Fuzz == nil {
		return nil
	}
	var out []extraTarget
	for _, fn := range r.Fuzz.Functions {
		switch fn.Outcome {
		case model.FuzzDiverged:
			reason := "Differential fuzzing: the baseline and the candidate recorded different values"
			if c := fn.Counterexample; c != nil {
				reason += " for " + redact.TruncateUTF8(c.Input, maxFuzzTargetText)
			}
			out = append(out, extraTarget{path: fn.Path, side: "new", start: fn.Line, end: fn.EndLine, severity: "high", reason: reason + "; a human decides which behavior is intended"})
		case model.FuzzInconclusive:
			out = append(out, extraTarget{path: fn.Path, side: "new", start: fn.Line, end: fn.EndLine, severity: "medium",
				reason: "Differential fuzzing was inconclusive: " + redact.TruncateUTF8(fn.Reason, maxFuzzTargetText)})
		}
	}
	return out
}

// writeFuzz renders "## Differential Fuzzing": the section status, one line
// per function, the skipped functions and the fixed note. It shows at most one
// value pair per function (the counterexample) and points to Behavior
// Divergences for the others. Every string goes through inline().
func writeFuzz(b *bytes.Buffer, r *model.Report) {
	f := r.Fuzz
	line(b, fuzzHeading)
	diverged, notDiverged, inconclusive := 0, 0, 0
	for _, fn := range f.Functions {
		switch fn.Outcome {
		case model.FuzzDiverged:
			diverged++
		case model.FuzzNotDiverged:
			notDiverged++
		default:
			inconclusive++
		}
	}
	switch f.Status {
	case model.FuzzRan:
		fmt.Fprintf(b, "Seeded inputs (%s) ran through %s on the baseline and the candidate: %d diverged, %d not diverged, %d inconclusive.\n\n", inline(f.SeedScheme), fuzzPlural(len(f.Functions), "changed Go function"), diverged, notDiverged, inconclusive)
	case model.FuzzNoCandidates:
		fmt.Fprintf(b, "No function ran: %s.\n\n", inline(fuzzReasonText(f.Reason)))
	case model.FuzzNotRun:
		fmt.Fprintf(b, "Differential fuzzing did not run: %s.\n\n", inline(fuzzReasonText(f.Reason)))
	case model.FuzzDisabled:
		fmt.Fprintf(b, "Differential fuzzing was disabled for this run (%s).\n\n", inline(fuzzReasonText(f.Reason)))
	default:
		fmt.Fprintf(b, "Status: %s.\n\n", inline(f.Status))
	}
	for _, fn := range f.Functions {
		where := inline(fn.Path)
		if fn.Line > 0 {
			where += fmt.Sprintf(":%d", fn.Line)
		}
		switch fn.Outcome {
		case model.FuzzDiverged:
			fmt.Fprintf(b, "- **diverged** %s (%s): %d of %d compared inputs recorded different values on the baseline and the candidate; each revision repeated its own value in a second run.", inline(fn.Symbol), where, fn.Diverged, fn.Compared)
			if c := fn.Counterexample; c != nil {
				fmt.Fprintf(b, " Smallest divergent input tried: %s; baseline %s; candidate %s.", inline(c.Input), observedValue(c.Base, true), observedValue(c.Candidate, true))
				if c.Base == c.Candidate {
					b.WriteString(" " + inline(fuzz.CounterexampleCutNote))
				}
			}
			fmt.Fprintf(b, " Evidence %s%s; the values are listed under Behavior Divergences.\n", inline(fn.EvidenceID), fuzzCheckList(fn.Checks))
		case model.FuzzNotDiverged:
			fmt.Fprintf(b, "- **not diverged** %s (%s): %d of %d inputs compared; the recorded encodings were equal for each compared input. Evidence %s%s.\n", inline(fn.Symbol), where, fn.Compared, fn.Inputs, inline(fn.EvidenceID), fuzzCheckList(fn.Checks))
		default:
			fmt.Fprintf(b, "- **inconclusive** %s (%s): %s.\n", inline(fn.Symbol), where, inline(strings.TrimSuffix(fuzzReasonText(fn.Reason), ".")))
		}
	}
	if f.SkippedTotal > 0 || len(f.Skipped) > 0 {
		fmt.Fprintf(b, "\nNot fuzzed (%d):\n\n", max(f.SkippedTotal, len(f.Skipped)))
		shown := 0
		for _, s := range f.Skipped {
			if shown == maxFuzzSkipsShown {
				break
			}
			where := inline(s.Path)
			if s.Line > 0 {
				where += fmt.Sprintf(":%d", s.Line)
			}
			if s.Symbol != "" {
				where += " " + inline(s.Symbol)
			}
			fmt.Fprintf(b, "- %s: %s\n", where, inline(s.Reason))
			shown++
		}
		if rest := max(f.SkippedTotal, len(f.Skipped)) - shown; rest > 0 {
			fmt.Fprintf(b, "- … %d more in confidence-report.json\n", rest)
		}
	}
	fmt.Fprintf(b, "\n%s\n", inline(f.Note))
}

// fuzzCheckList renders the check IDs of a function as " (checks a, b, ...)",
// or "" without any.
func fuzzCheckList(c *model.FuzzChecks) string {
	if c == nil {
		return ""
	}
	ids := []string{}
	for _, id := range []string{c.Base, c.Candidate, c.BaseConfirm, c.CandidateConfirm} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return " (checks " + inline(strings.Join(ids, ", ")) + ")"
}

// fuzzPlural writes n and noun, with a plural s unless n is 1.
func fuzzPlural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func fuzzReasonText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no reason was recorded"
	}
	return s
}
