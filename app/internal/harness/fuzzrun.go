package harness

// Differential fuzzing runs (F2). The fuzz package renders one observation
// harness per Go package or TS/JS module; this file stages it on both
// revisions, runs it through the unchanged sandbox with the reviewed
// generated_test template and the payload channel, and records the checks and
// the evidence. The harness never imports the fuzz package: cli orchestrates,
// and the stream is validated by the Normalize callback the caller passes.
//
// A TS/JS harness (runner jest_json, F2c, Appendix D.13) runs through a
// verifiable Vitest or Jest template with the harness as its {file} target.
// Its only payload is the observation stream: the JSON report that the
// template writes to {results_out} is not captured, and the framework's log
// is never read for a verdict.
//
// Status rules (§1.17, Appendix D.6):
//   - A baseline-side run (fuzz_base, fuzz_base_confirm) becomes ERROR when
//     its complete log shows that the harness did not build or start on the
//     baseline, or that a passing run did not run every harness test, or when
//     a passing run returned no readable observation stream. These are
//     failures of the harness on trusted code, decided from the recorded
//     check, never from text that candidate code wrote. A log that was cut
//     (sandbox.max_output_bytes) never decides ERROR: the run keeps PASS or
//     FAIL, and the comparison makes its functions inconclusive. For a TS/JS
//     harness the log is not read at all: a run that returned no payload did
//     not load or start the harness, and a passing run whose stream ended
//     between two tests (after a done record, before the next head record)
//     did not run every test (scriptBaselineStartFailure). A passing run
//     whose stream ended inside a test (a head record without its done
//     record) keeps PASS: the baseline process ended while it evaluated an
//     input (Jest lets a process.exit(0) end the run with exit code 0), as a
//     failing run that started the harness keeps FAIL.
//   - A candidate-side run (fuzz_candidate, fuzz_candidate_confirm) keeps the
//     status run.go gave it: a compile, setup or run failure stays FAIL, and
//     only an infrastructure cause (Docker, exit code 125 or above, a lost
//     artifact) is ERROR. Candidate code can therefore never force exit 4.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/model"
)

// FuzzObservationsPath is the fixed in-container path of the observation
// stream a fuzz harness appends to. It returns on the framed payload channel
// (captureScript); policy argv never names it.
const FuzzObservationsPath = "/tmp/probe-observations.jsonl"

// Audit names of the fuzz stage (reserved "stage:" prefix, §1.2).
const (
	auditRunFuzz        = model.AuditStagePrefix + "run_fuzz"
	auditRunFuzzConfirm = model.AuditStagePrefix + "run_fuzz_confirm"
)

// Bounds and fixed texts of the fuzz runs.
const (
	// fuzzRejectedLimit bounds the redacted copy of a rejected payload.
	fuzzRejectedLimit = 4096
	// Baseline-side ERROR causes, appended to the recorded log.
	fuzzBaseNotStarted  = "the fuzz harness did not build or start on the baseline: no harness test recorded a run event"
	fuzzBaseTestsMissed = "the fuzz harness did not run every harness test on the baseline although the run passed"
	fuzzBaseNoStream    = "the baseline run passed but returned no observation stream"
	fuzzBaseBadStream   = "the baseline run passed but its observation stream was rejected"
	// fuzzStreamLost is the ERROR cause of a stream that could not be
	// retained as an artifact: a host-side failure on either side.
	fuzzStreamLost = "the observation stream could not be retained as an artifact"
	// Baseline-side ERROR causes of a TS/JS harness, decided from its stream
	// (the framework's log is never read).
	fuzzScriptNotStarted  = "the TS/JS fuzz harness did not load or start on the baseline: the run wrote no observation stream"
	fuzzScriptTestsMissed = "the TS/JS fuzz harness did not run every harness test on the baseline although the run passed: the stream ended between two tests"
	// fuzzCausePrefix introduces the ERROR cause line appended to the
	// recorded output of a fuzz check (after its log artifact was retained).
	fuzzCausePrefix = "\nprobe: "
)

// errFuzzSubCap is the cancellation cause of a fuzz run's context when the
// stage sub-cap (fuzz.max_runtime_seconds) ends before the overall deadline.
var errFuzzSubCap = errors.New("fuzz.max_runtime_seconds reached")

// fuzzState counts the RunObserved calls of one harness, so that each run's
// artifacts get distinct names, and remembers the runner of every fuzz check
// it recorded, so that AddFuzzEvidence accepts only records whose runner is
// the one their checks ran with.
type fuzzState struct {
	runs    int
	runners map[string]string // check ID -> RunnerGo or RunnerJest
}

// ObservedRun asks RunObserved to run one rendered fuzz harness on both
// revisions.
type ObservedRun struct {
	// Path is the repository-relative path of the harness file, a _test.go file
	// that exists in neither snapshot. Content is staged there, byte-identical,
	// on both revisions and removed after the runs.
	Path, Content string
	// TestNames are the harness's test functions, in execution order; the
	// parsed declarations of Content must be exactly these.
	TestNames []string
	// Confirm selects the confirmation kinds fuzz_base_confirm and
	// fuzz_candidate_confirm instead of fuzz_base and fuzz_candidate.
	// Confirmation runs are always live.
	Confirm bool
	// SaveSource retains Content as the fuzz_harness artifact before any run.
	SaveSource bool
	// Deadline is the end of the stage's sub-cap: no run starts after it, and
	// a run in progress is stopped at it (TIMEOUT).
	Deadline time.Time
	// Normalize validates a raw observation stream and returns the normalized
	// stream that becomes Check.Results. It must return a fixed point of
	// redaction; any error rejects the whole stream.
	Normalize func(payload []byte) (string, error)
	// Runner is RunnerGo (also when empty) for a Go harness and RunnerJest
	// for a TS/JS harness: Path is then a new <name>.test.ts or .test.js file,
	// Content declares exactly TestNames as top-level test() calls, and the
	// template is a verifiable Vitest or Jest template.
	Runner string
	// Started is required for a TS/JS harness: it returns how many harness
	// tests a normalized stream shows as started (their head record), and
	// whether the stream ended inside a started test (its head record
	// without its done record). These decide the baseline-side ERROR rules
	// instead of the log.
	Started func(results string) (started int, inside bool)
}

// ObservedSide is one recorded run of an ObservedRun.
type ObservedSide struct {
	Check model.Check
	// OverflowSHA256 is set when the normalized stream exceeded what remained
	// of the results budget for the run's side: the stream was kept only as
	// the fuzz_observations artifact with this SHA-256 and Check.Results is
	// empty.
	OverflowSHA256 string
}

// fuzzTemplate reports whether a generated_test template can run a fuzz
// harness: a verifiable single-package go test template whose target is the
// standalone {package} placeholder. A {file} target would compile the harness
// file without the package it observes.
func fuzzTemplate(command []string) bool {
	if !verifiableGoTemplate(command) {
		return false
	}
	for _, arg := range command[2:] {
		if arg == "{package}" {
			return true
		}
	}
	return false
}

// RunObserved stages run.Content at run.Path in both snapshots, runs the
// generated_test template for its package with -json -count=1 -run
// ^(names)$ on the baseline and then on the candidate, each with the payload
// channel on FuzzObservationsPath, and records both checks. It returns an
// error, and records no check, when a precondition fails: the harness is
// closed or has no baseline, the template cannot run a fuzz harness, the path
// is not a new _test.go file on both revisions, the content does not declare
// exactly run.TestNames, a test name already exists in the package directory,
// or the source artifact cannot be retained.
//
// Each run's timeout is at most what remains before run.Deadline, and its
// budget limit is the pre-reviewer ceiling (§1.7.1). The stream is normalized
// with run.Normalize, retained as a hashed fuzz_observations artifact, and
// stored in Check.Results when it fits the results budget of the run's side;
// otherwise only the artifact keeps it (ObservedSide.OverflowSHA256). A
// payload that is not one complete frame, or that Normalize rejects, is kept
// as a bounded, redacted fuzz_payload_rejected artifact. The runs never use
// the reviewer's generated-test budget and are not reviewer tests. One
// stage:run_fuzz (or stage:run_fuzz_confirm) audit event records the pair.
func (h *Harness) RunObserved(ctx context.Context, run ObservedRun) (base, candidate ObservedSide, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	started := time.Now()
	command, err := h.fuzzPrecheck(run)
	if err != nil {
		return ObservedSide{}, ObservedSide{}, err
	}
	runner := RunnerGo
	if run.Runner == RunnerJest {
		runner = RunnerJest
	}
	h.fuzz.runs++
	if run.SaveSource {
		if err := h.saveArtifact(fmt.Sprintf("fuzz-harness-%d-%s", h.fuzz.runs, path.Base(run.Path)), model.ArtifactFuzzHarness, []byte(run.Content)); err != nil {
			return ObservedSide{}, ObservedSide{}, errors.New("the fuzz harness source could not be retained as an artifact")
		}
	}
	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	for _, root := range []string{h.base, h.candidate} {
		cleanup, err := stageEphemeral(root, run.Path, run.Content)
		if err != nil {
			return ObservedSide{}, ObservedSide{}, errors.New("the fuzz harness could not be staged in the snapshots")
		}
		cleanups = append(cleanups, cleanup)
	}
	// The sub-cap is the cause of this context's own deadline, so that a
	// SKIPPED run can be told apart from the overall --deadline.
	runCtx, cancel := context.WithDeadlineCause(ctx, run.Deadline, errFuzzSubCap)
	defer cancel()
	baseKind, candidateKind, tool := model.CheckFuzzBase, model.CheckFuzzCandidate, auditRunFuzz
	if run.Confirm {
		baseKind, candidateKind, tool = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm, auditRunFuzzConfirm
	}
	ceiling := h.preReviewerCeiling()
	record := func(kind, dir string, baseline bool) (ObservedSide, string) {
		remaining := time.Until(run.Deadline)
		if remaining <= 0 {
			// The sub-cap is over: wait for the context to say so, so that the
			// run is recorded as SKIPPED instead of starting with no limit.
			<-runCtx.Done()
		}
		o := runOptions{capture: FuzzObservationsPath, timeout: remaining, ceiling: ceiling, live: run.Confirm}
		c, payload, truncated := h.runWithOptions(runCtx, kind, dir, command, o)
		return h.fuzzSide(c, payload, truncated, run, baseline)
	}
	base, baseCause := record(baseKind, h.base, true)
	candidate, candidateCause := record(candidateKind, h.candidate, false)
	if h.fuzz.runners == nil {
		h.fuzz.runners = map[string]string{}
	}
	for _, c := range []model.Check{base.Check, candidate.Check} {
		if c.ID != "" {
			h.fuzz.runners[c.ID] = runner
		}
	}
	fields := map[string]any{"path": run.Path, "tests": len(run.TestNames), "checks": []string{base.Check.ID, candidate.Check.ID}, "baseline": base.Check.Status, "runner": runner}
	// The ERROR causes are also kept here: the retained check-N.log artifact
	// holds the sandbox log as it was recorded, before the cause line.
	if baseCause != "" {
		fields["baseline_error"] = baseCause
	}
	if candidateCause != "" {
		fields["candidate_error"] = candidateCause
	}
	arguments, _ := json.Marshal(fields)
	h.audit = append(h.audit, model.AuditEvent{Time: started.UTC(), Tool: tool, Arguments: truncateUTF8(Redact(string(arguments)), 4096), Status: candidate.Check.Status, DurationMS: time.Since(started).Milliseconds()})
	return base, candidate, nil
}

// fuzzPrecheck validates an ObservedRun and returns the command both runs
// execute. Caller holds h.mu.
func (h *Harness) fuzzPrecheck(run ObservedRun) ([]string, error) {
	switch {
	case h.closed:
		return nil, errors.New("harness is closed")
	case h.base == "":
		return nil, errors.New("differential fuzzing needs a baseline snapshot")
	case run.Normalize == nil:
		return nil, errors.New("no observation stream validator was given")
	case run.Deadline.IsZero():
		return nil, errors.New("no stage deadline was given")
	case run.Runner == RunnerJest:
		return h.scriptFuzzPrecheck(run)
	case run.Runner != "" && run.Runner != RunnerGo:
		return nil, errors.New("unknown fuzz harness runner")
	case !fuzzTemplate(h.opts.Commands["generated_test"]):
		return nil, errors.New("the generated_test command cannot run a fuzz harness; configure it as a single-package go test {package} template")
	case !strings.HasSuffix(run.Path, "_test.go") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(run.Path))) != run.Path:
		return nil, errors.New("the fuzz harness path must be a clean _test.go path")
	case len(run.Content) == 0 || len(run.Content) > maxFileBytes || strings.ContainsRune(run.Content, 0):
		return nil, errors.New("the fuzz harness must contain 1 byte to 1 MiB of text")
	}
	names, err := generatedGoTests(run.Path, run.Content)
	if err != nil {
		return nil, err
	}
	if !equalStrings(names, run.TestNames) {
		return nil, errors.New("the fuzz harness does not declare exactly the expected tests")
	}
	for _, root := range []string{h.base, h.candidate} {
		p, err := safePath(root, run.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return nil, errors.New("the fuzz harness path already exists in a snapshot")
		}
		if err := rejectGoTestCollisions(root, run.Path, names); err != nil {
			return nil, err
		}
	}
	command := h.testCommand(run.Path)
	if len(command) == 0 {
		return nil, errors.New("the generated_test command has no {package} target")
	}
	return selectGoTests(command, names), nil
}

// fuzzSide post-processes one recorded fuzz run: it decodes the framed
// payload, normalizes and retains the stream, applies the results budget and
// the baseline-side ERROR rules, and writes the check back to the ledger. It
// returns the side and the ERROR cause it recorded, or "". Caller holds h.mu.
func (h *Harness) fuzzSide(c model.Check, payload []byte, truncated bool, run ObservedRun, baseline bool) (ObservedSide, string) {
	side := ObservedSide{}
	if c.Status != "PASS" && c.Status != "FAIL" {
		side.Check = c
		return side, ""
	}
	cause := ""
	raw, frameErr := coverage.DecodeFrame(payload, truncated)
	switch {
	case frameErr == nil:
		results, err := run.Normalize(raw)
		switch {
		case err != nil:
			h.rejectFuzzPayload(c.ID, raw)
			if baseline && c.Status == "PASS" {
				cause = fuzzBaseBadStream + ": " + truncateUTF8(Redact(err.Error()), 300)
			}
		case h.saveArtifact(c.ID+"-fuzz-observations.json", model.ArtifactFuzzObservations, []byte(results)) != nil:
			cause = fuzzStreamLost
		case len(results) > h.resultsRemainingFor(c.Kind):
			side.OverflowSHA256 = h.artifacts[len(h.artifacts)-1].SHA256
		default:
			c.Results = results
		}
	case len(payload) > 0:
		h.rejectFuzzPayload(c.ID, payload)
		if baseline && c.Status == "PASS" {
			cause = fuzzBaseBadStream + ": the payload was not one complete frame"
		}
	case baseline && c.Status == "PASS":
		cause = fuzzBaseNoStream
	}
	switch {
	case !baseline || cause != "":
	case run.Runner == RunnerJest:
		cause = scriptBaselineStartFailure(c, payload, side.OverflowSHA256 != "", run)
	case h.completeLog(c):
		cause = fuzzBaselineStartFailure(c, run.TestNames)
	}
	if cause != "" {
		c.Status, c.Results = "ERROR", ""
		c.Output = h.withCause(c.Output, cause)
	}
	h.replaceCheck(c)
	side.Check = c
	return side, cause
}

// completeLog reports whether the recorded log of c can be trusted to be the
// whole log: it was not cut by the output bound, and it is not so close to
// that bound that redaction may have cut its end. Only a complete log may
// decide a baseline-side ERROR from missing run events: baseline code that
// writes a lot of output cuts the log, and the candidate chooses which
// baseline functions run, so a cut log is never a harness failure.
func (h *Harness) completeLog(c model.Check) bool {
	return !c.Truncated && len(c.Output) <= h.opts.MaxOutputBytes-utf8.UTFMax
}

// withCause appends the ERROR cause line to a recorded output. The output is
// cut first, so that the cause always fits within the output bound, and the
// composed text is redacted as a whole (Appendix D.16).
func (h *Harness) withCause(output, cause string) string {
	suffix := fuzzCausePrefix + cause
	room := h.opts.MaxOutputBytes - len(suffix)
	if room < 0 {
		room = 0
	}
	return truncateUTF8(Redact(truncateUTF8(output, room)+suffix), h.opts.MaxOutputBytes)
}

// fuzzBaselineStartFailure returns why a baseline-side PASS or FAIL run whose
// log is complete shows that the harness did not build or start, or "". A run
// whose log records no run event for any harness test did not start the
// harness (a build failure, a TestMain that exits early, a failing package
// init); a passing run must record a run event for every harness test. A
// failing run that started the harness is left FAIL: the baseline process
// ended while it evaluated inputs. The log is read line by line without a
// line length bound, so a long output line hides no later event.
func fuzzBaselineStartFailure(c model.Check, names []string) string {
	ran := map[string]bool{}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for rest := c.Output; rest != ""; {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		if !strings.Contains(line, `"run"`) {
			continue
		}
		var event struct{ Action, Test string }
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Action == "run" && wanted[event.Test] {
			ran[event.Test] = true
		}
	}
	switch {
	case len(ran) == 0:
		return fuzzBaseNotStarted
	case c.Status == "PASS" && len(ran) != len(wanted):
		return fuzzBaseTestsMissed
	}
	return ""
}

// rejectFuzzPayload retains a bounded, redacted copy of a payload that was
// not accepted, so that the refusal stays auditable. Caller holds h.mu.
func (h *Harness) rejectFuzzPayload(checkID string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	_ = h.saveArtifact(checkID+"-fuzz-payload-rejected.txt", model.ArtifactFuzzPayloadRejected, []byte(truncateUTF8(Redact(string(payload)), fuzzRejectedLimit)))
}

// AddFuzzEvidence records one differential_fuzz evidence record and returns
// it as stored, with its evidence-N ID. It refuses a record of another kind, a
// status other than DIVERGED, NOT_DIVERGED or UNVERIFIED, a runner other than
// go_test_json and jest_json, anything but exactly one test name, an empty
// path, fields other kinds use (repeat check, criterion, referenced symbols),
// check IDs that do not name exactly one recorded fuzz_candidate (CheckID)
// and fuzz_base (BaseCheckID) check, and a runner other than the one both
// checks ran with. Only host code calls it: no reviewer tool reaches it.
// report.Finalize re-derives the status from the recorded checks.
func (h *Harness) AddFuzzEvidence(e model.Evidence) (model.Evidence, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return model.Evidence{}, errors.New("harness is closed")
	case e.Kind != model.EvidenceDifferentialFuzz:
		return model.Evidence{}, fmt.Errorf("fuzz evidence must have kind %s", model.EvidenceDifferentialFuzz)
	case e.Status != model.StatusDiverged && e.Status != model.StatusNotDiverged && e.Status != model.StatusUnverified:
		return model.Evidence{}, errors.New("fuzz evidence status must be DIVERGED, NOT_DIVERGED or UNVERIFIED")
	case e.Runner != RunnerGo && e.Runner != RunnerJest:
		return model.Evidence{}, fmt.Errorf("fuzz evidence runner must be %s or %s", RunnerGo, RunnerJest)
	case len(e.TestNames) != 1 || e.TestNames[0] == "":
		return model.Evidence{}, errors.New("fuzz evidence must name exactly one test")
	case e.Path == "":
		return model.Evidence{}, errors.New("fuzz evidence must name the harness path")
	case e.RepeatCheckID != "" || e.CriterionID != "" || len(e.ReferencedSymbols) > 0:
		return model.Evidence{}, errors.New("fuzz evidence carries no repeat check, criterion or referenced symbols")
	}
	if !h.recordedKind(e.CheckID, model.CheckFuzzCandidate) || !h.recordedKind(e.BaseCheckID, model.CheckFuzzBase) {
		return model.Evidence{}, errors.New("fuzz evidence must cite a recorded fuzz_candidate check and a recorded fuzz_base check")
	}
	if h.fuzz.runners[e.CheckID] != e.Runner || h.fuzz.runners[e.BaseCheckID] != e.Runner {
		return model.Evidence{}, errors.New("fuzz evidence must name the runner its checks ran with")
	}
	e.Description, e.Output = Redact(e.Description), Redact(e.Output)
	return h.appendEvidence(e), nil
}

// recordedKind reports whether id names exactly one check in the main ledger,
// and that check has the given kind. Caller holds h.mu.
func (h *Harness) recordedKind(id, kind string) bool {
	found := 0
	for _, c := range h.checks {
		if c.ID != id {
			continue
		}
		if c.Kind != kind {
			return false
		}
		found++
	}
	return id != "" && found == 1
}

// VerifiableJSTemplate reports whether a generated_test template can establish
// which generated JavaScript/TypeScript file ran: exactly one standalone
// {file} target, the runner's JSON report written to {results_out}, no
// {package}, and no package-manager script or shell as the command.
func VerifiableJSTemplate(cmd []string) bool { return verifiableJSTemplate(cmd) }

// scriptFuzzPrecheck is fuzzPrecheck for a TS/JS harness (runner jest_json).
// The template must be verifiable, the path a clean new <name>.test.ts or
// .test.js path in both snapshots, and the content must declare exactly
// run.TestNames, in order, as top-level test() or it() calls with static
// titles (the rules of generated JavaScript/TypeScript tests). The command
// is the template with the harness as its {file} target; no test-name filter
// is added, since the file declares only the harness tests. Caller holds
// h.mu.
func (h *Harness) scriptFuzzPrecheck(run ObservedRun) ([]string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(run.Path)))
	switch {
	case run.Started == nil:
		return nil, errors.New("no started-test counter was given for a TS/JS fuzz harness")
	case !verifiableJSTemplate(h.opts.Commands["generated_test"]):
		return nil, errors.New("the generated_test command cannot run a TS/JS fuzz harness; configure a verifiable Vitest or Jest template with {file} and {results_out}")
	case !isJSTestPath(run.Path) || clean != run.Path || strings.HasPrefix(run.Path, "/") || strings.HasPrefix(run.Path, "../"):
		return nil, errors.New("the TS/JS fuzz harness path must be a clean .test.ts or .test.js path")
	case len(run.Content) == 0 || len(run.Content) > maxFileBytes || strings.ContainsRune(run.Content, 0):
		return nil, errors.New("the fuzz harness must contain 1 byte to 1 MiB of text")
	}
	names, err := generatedJSTests(run.Content)
	if err != nil {
		return nil, err
	}
	if !equalStrings(names, run.TestNames) {
		return nil, errors.New("the fuzz harness does not declare exactly the expected tests")
	}
	for _, root := range []string{h.base, h.candidate} {
		p, err := safePath(root, run.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return nil, errors.New("the fuzz harness path already exists in a snapshot")
		}
	}
	command := h.testCommand(run.Path)
	if len(command) == 0 {
		return nil, errors.New("the generated_test command has no {file} target")
	}
	return command, nil
}

// scriptBaselineStartFailure returns why a baseline-side PASS or FAIL run of a
// TS/JS harness shows that the harness did not load or start, or "". It is
// decided from the payload and the normalized stream, never from the
// framework's log: a run that returned no payload at all did not start the
// harness (the module or the harness could not be loaded, or the runner found
// no test to run), and a passing run whose stream shows fewer started tests
// than the harness declares, and ended between two tests, did not run every
// test. A run whose stream ended inside a started test is left PASS or FAIL:
// the baseline process ended while it evaluated an input, which is behavior
// of the baseline code, not a failure of the harness (under Jest, a
// process.exit(0) in the code ends the run with exit code 0 and the
// remaining tests unrun); its functions are inconclusive. A failing run that
// started the harness is left FAIL for the same reason. A rejected stream of
// a failing run, and a stream kept only as an artifact (the results budget),
// decide nothing here.
func scriptBaselineStartFailure(c model.Check, payload []byte, overflow bool, run ObservedRun) string {
	switch {
	case len(payload) == 0:
		return fuzzScriptNotStarted
	case overflow || c.Results == "":
		return ""
	}
	started, inside := run.Started(c.Results)
	switch {
	case started == 0:
		return fuzzScriptNotStarted
	case c.Status == "PASS" && started != len(run.TestNames) && !inside:
		return fuzzScriptTestsMissed
	}
	return ""
}
