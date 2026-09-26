package cli

// Impacted tests (--impacted-tests, F6b). Selection is static: the unchanged
// existing Go tests that the impact index lists as reaching a changed
// function. Execution and evidence belong to the harness; report.Finalize
// re-derives every status. The stage adds evidence and review requests only:
// it never produces exit 1.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/symbols"
)

// impactedUnverifiedPrefix starts the Unverified entry of a stage that did not
// run; impactedUnverifiedLine is the entry of a stage that ran and left a
// selected test without a result.
const (
	impactedUnverifiedPrefix = "Impacted tests did not run: "
	impactedUnverifiedLine   = "Some impacted tests got no FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE result; each one's reason is in impact.changed_functions[].tests[].reason."
	// impactedChangedFileReason is the reason of a reaching test declared in a
	// test file the change modified: the stage runs unchanged files only.
	impactedChangedFileReason = "not run: its test file was modified by the change (--base-tests runs the baseline versions of changed tests)"
)

// impactedPlan is the static selection of the stage.
type impactedPlan struct {
	// tests are the reaching tests of unchanged test files, one per path and
	// name, smallest depth first, then by path and name.
	tests []model.ImpactTest
	// changedFile counts the reaching tests (path and name) declared in test
	// files the change modified; they are not run.
	changedFile int
	// unlisted counts the reaching tests the index found beyond the ones it
	// lists per changed function; they cannot be selected.
	unlisted int
	// status and reason are the section's when tests is empty.
	status, reason string
}

// planImpactedTests selects the tests to run from the impact section.
func planImpactedTests(im *model.Impact) impactedPlan {
	switch im.Status {
	case model.ImpactNotApplicable:
		return impactedPlan{status: model.ImpactTestsNoCandidates, reason: "no indexable Go file changed, so no reaching test was searched"}
	case model.ImpactUnavailable:
		return impactedPlan{status: model.ImpactTestsNotRun, reason: "the static Go index is unavailable, so no reaching test was searched"}
	case model.ImpactIndexed, model.ImpactLimited:
	default:
		return impactedPlan{status: model.ImpactTestsNotRun, reason: "the impact section holds no usable index"}
	}
	type key struct{ path, name string }
	var plan impactedPlan
	chosen := map[key]int{}
	changed := map[key]bool{}
	for _, f := range im.ChangedFunctions {
		if !f.Indexed {
			continue
		}
		if f.TestsTotal > len(f.Tests) {
			plan.unlisted += f.TestsTotal - len(f.Tests)
		}
		for _, t := range f.Tests {
			k := key{t.Path, t.Name}
			if t.FileChanged {
				changed[k] = true
				continue
			}
			if i, ok := chosen[k]; ok {
				if t.Depth < plan.tests[i].Depth {
					plan.tests[i] = impactedSelection(t)
				}
				continue
			}
			chosen[k] = len(plan.tests)
			plan.tests = append(plan.tests, impactedSelection(t))
		}
	}
	plan.changedFile = len(changed)
	sort.SliceStable(plan.tests, func(i, j int) bool {
		a, b := plan.tests[i], plan.tests[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
	if len(plan.tests) > 0 {
		return plan
	}
	plan.status = model.ImpactTestsNoCandidates
	switch {
	case plan.changedFile > 0:
		plan.reason = "every reaching test the static index lists is declared in a test file the change modified (--base-tests runs the baseline versions of changed tests)"
	default:
		plan.reason = fmt.Sprintf("the static index lists no existing Go test that reaches a changed function within %d references, which is not proof that none exists", symbols.MaxDepth)
	}
	if im.Status == model.ImpactLimited {
		plan.reason += "; the index is limited, so reaching tests may be missing"
	}
	return plan
}

// impactedSelection is the copy of a listed reaching test that the harness
// receives: its identity and static link, without any stored result.
func impactedSelection(t model.ImpactTest) model.ImpactTest {
	return model.ImpactTest{Name: t.Name, Path: t.Path, Line: t.Line, Package: t.Package, Depth: t.Depth, Resolution: t.Resolution}
}

// runImpactedStage selects the impacted tests, runs them in the harness and
// records the outcome in r.Impact: tests_status, tests_reason, and each listed
// test's evidence ID, status and reason. It returns true when a run of the
// stage was recorded as ERROR. A cancelled or expired context is recorded as
// not_run or in each test's reason, never as an operational failure.
func runImpactedStage(ctx context.Context, h *harness.Harness, r *model.Report, errOut io.Writer) bool {
	im := r.Impact
	if im == nil {
		return false
	}
	plan := planImpactedTests(im)
	if len(plan.tests) == 0 {
		im.TestsStatus, im.TestsReason = plan.status, plan.reason
		applyImpactedResults(im, nil)
		if plan.status == model.ImpactTestsNotRun {
			r.Unverified = append(r.Unverified, impactedUnverifiedPrefix+plan.reason)
		}
		return false
	}
	fmt.Fprintf(errOut, "Running %d unchanged Go tests that statically reach changed code on baseline and candidate in isolated Docker sandboxes...\n", len(plan.tests))
	out := h.RunImpactedTests(ctx, plan.tests)
	recordImpactedOutcome(r, plan, out)
	return out.Errors > 0
}

// recordImpactedOutcome records what the harness returned: each listed
// test's result, tests_status and tests_reason, and an Unverified entry when
// the stage did not run or left a selected test without a result.
func recordImpactedOutcome(r *model.Report, plan impactedPlan, out harness.ImpactedTests) {
	im := r.Impact
	applyImpactedResults(im, out.Tests)
	im.TestsStatus = out.Status
	if out.Status != model.ImpactTestsRan {
		im.TestsReason = out.Reason
		if im.TestsReason == "" {
			im.TestsReason = "no reason was recorded"
		}
		r.Unverified = append(r.Unverified, impactedUnverifiedPrefix+im.TestsReason)
		return
	}
	im.TestsReason = impactedRanReason(plan, out)
	// The Unverified entry carries no count: report.Finalize may still
	// downgrade a result, and the section holds the finalized statuses. Tests
	// the limits left out are stated in tests_reason instead.
	withoutResult := -out.Capped
	for _, t := range out.Tests {
		if t.Status != model.StatusFailsOnCandidate && t.Status != model.StatusPassesOnCandidate {
			withoutResult++
		}
	}
	if withoutResult > 0 {
		r.Unverified = append(r.Unverified, impactedUnverifiedLine)
	}
}

// impactedRanReason states what a stage that ran left out, or "".
func impactedRanReason(plan impactedPlan, out harness.ImpactedTests) string {
	var parts []string
	if out.Capped > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d selected tests were not run: the stage runs at most %d tests from at most %d packages (test files, with a {file} template) per review", out.Capped, len(out.Tests), harness.ImpactedMaxTests, harness.ImpactedMaxUnits))
	}
	if plan.changedFile > 0 {
		parts = append(parts, fmt.Sprintf("%d reaching tests declared in test files the change modified were not run (--base-tests runs the baseline versions of changed tests)", plan.changedFile))
	}
	if plan.unlisted > 0 {
		parts = append(parts, fmt.Sprintf("%d reaching tests beyond the %d listed per changed function were not considered", plan.unlisted, symbols.MaxTestsListed))
	}
	return strings.Join(parts, "; ")
}

// applyImpactedResults copies each result to every listed entry of the same
// test (path and name) and gives the reaching tests of modified test files
// their reason. Entries without a result keep no status.
func applyImpactedResults(im *model.Impact, results []model.ImpactTest) {
	type key struct{ path, name string }
	byKey := map[key]model.ImpactTest{}
	for _, t := range results {
		byKey[key{t.Path, t.Name}] = t
	}
	for i := range im.ChangedFunctions {
		tests := im.ChangedFunctions[i].Tests
		for j := range tests {
			t := &tests[j]
			if t.FileChanged {
				t.EvidenceID, t.Status, t.Reason = "", "", impactedChangedFileReason
				continue
			}
			if res, ok := byKey[key{t.Path, t.Name}]; ok {
				t.EvidenceID, t.Status, t.Reason = res.EvidenceID, res.Status, res.Reason
			}
		}
	}
}
