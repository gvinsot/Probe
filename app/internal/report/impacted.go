package report

import (
	"path"
	"regexp"
	"strings"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

// This file belongs to F6b: impacted tests (--impacted-tests), the unchanged
// existing Go tests that the static index found reaching a changed function,
// run on the baseline and on the candidate. An impacted_test_differential
// record never supports a hypothesis status (see accepted) and never sets
// exit 1; finalizeImpact (impact.go) sets each test's status from the
// statuses verifyImpactedTests re-derives here.

// impactedTestName is a top-level Go test function name, the only kind of
// name the stage runs.
var impactedTestName = regexp.MustCompile(`^Test[\p{L}\p{N}_]*$`)

// impactedEvidenceStatus is the status the recorded checks of one
// impacted_test_differential record support, or "":
//   - one verifiable top-level Go test name and a verifiable *_test.go path
//     with runner go_test_json, or one TS/JS test name (describe titles and
//     title) and a verifiable TS/JS test file path with runner jest_json;
//   - an impacted_test_base check (base_check_id) and an
//     impacted_test_candidate check (check_id), each recorded exactly once,
//     the candidate one never replayed;
//   - a command that targets the package (or file) of the path, the file
//     itself for jest_json; both commands are identical (the classifier
//     checks it);
//   - harness.ClassifyExistingTest (go_test_json) or
//     harness.ClassifyExistingJestTest (jest_json, from the recorded JSON
//     reports) on those two checks gives the status;
//   - the live-baseline rule: FAILS_ON_CANDIDATE needs a baseline check that
//     was executed, PASSES_ON_CANDIDATE a baseline check that was executed or
//     replayed from two agreeing live runs.
func impactedEvidenceStatus(e model.Evidence, l *ledger) string {
	if len(e.TestNames) != 1 || !verifiableNames(e.TestNames) || !verifiableText(e.Path) {
		return ""
	}
	switch e.Runner {
	case harness.RunnerGo:
		if !impactedTestName.MatchString(e.TestNames[0]) || !strings.HasSuffix(e.Path, "_test.go") {
			return ""
		}
	case harness.RunnerJest:
		if !harness.ValidJSTestName(e.TestNames[0]) || !harness.ScriptTestPath(e.Path) {
			return ""
		}
	default:
		return ""
	}
	base, baseOK := l.check(e.BaseCheckID)
	candidate, candidateOK := l.check(e.CheckID)
	if !baseOK || !candidateOK || base.Kind != model.CheckImpactedTestBase || candidate.Kind != model.CheckImpactedTestCandidate || candidate.Replayed() {
		return ""
	}
	if e.Runner == harness.RunnerJest && !scriptCommandTargets(base.Command, e.Path) || e.Runner == harness.RunnerGo && !impactedCommandTargets(base.Command, e.Path) {
		return ""
	}
	status, _ := harness.ClassifyExistingTest(base, candidate, e.TestNames[0])
	if e.Runner == harness.RunnerJest {
		status, _ = harness.ClassifyExistingJestTest(base, candidate, e.Path, e.TestNames[0])
	}
	switch {
	case status == model.StatusFailsOnCandidate && positiveBaseline(base):
		return model.StatusFailsOnCandidate
	case status == model.StatusPassesOnCandidate && negativeBaseline(base):
		return model.StatusPassesOnCandidate
	}
	return ""
}

// impactedCommandTargets reports whether a recorded command targets the test
// file p: its package directory as the generated_test template substitutes it
// ("./dir", or "." for the root), or the file itself.
func impactedCommandTargets(command []string, p string) bool {
	dir := path.Dir(p)
	pkg := "."
	if dir != "." {
		pkg = "./" + dir
	}
	for _, arg := range command {
		if arg == pkg || arg == p {
			return true
		}
	}
	return false
}
