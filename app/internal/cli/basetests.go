package cli

// Baseline versions of changed Go tests on candidate code (--base-tests, F3).
// Selection is static (internal/suites); execution and evidence belong to the
// harness; report.Finalize re-derives every status. The stage adds evidence and
// review requests only: it never produces exit 1.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/suites"
)

// baseTestsUnverifiedPrefix starts every Unverified entry of a stage that
// did not run; baseTestsUnverifiedLine is the entry of a stage that ran and
// left some test without a result.
const (
	baseTestsUnverifiedPrefix = "Baseline versions of changed tests did not run: "
	baseTestsUnverifiedLine   = "Some baseline versions of changed tests have no FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE result; see Changed Baseline Tests on Candidate Code."
)

// runBaseTests selects the Go test functions of the changed Go test files
// (internal/suites), runs their baseline versions on the baseline tree and on
// the hybrid tree, and records the section. It returns true on an operational
// failure: the private candidate copy for the hybrid tree could not be made or
// its manifest could not be retained. A cancelled or expired context is
// recorded as not_run, never as an operational failure.
func runBaseTests(ctx context.Context, repo *gitrepo.Repository, change model.Change, h *harness.Harness, r *model.Report, errOut io.Writer) (operational bool) {
	read := func(ctx context.Context, commit, path string) ([]byte, error) {
		b, err := repo.ReadFile(ctx, commit, path)
		if errors.Is(err, gitrepo.ErrLimit) {
			return nil, suites.ErrTooLarge
		}
		return b, err
	}
	sel, err := suites.Plan(ctx, read, change)
	if err != nil {
		reason := "the review was cancelled before the changed tests were selected"
		if errors.Is(context.Cause(ctx), harness.ErrOverallDeadline) {
			reason = "the overall deadline was reached before the changed tests were selected"
		}
		r.BaseTests = baseTestsSection(model.BaseTestsNotRun, reason)
		r.Unverified = append(r.Unverified, baseTestsUnverifiedPrefix+reason)
		return false
	}
	r.Unverified = append(r.Unverified, sel.Notes...)
	if len(sel.Tests) == 0 {
		if len(sel.Notes) > 0 {
			// Something could not be analyzed: "nothing selected" would claim more
			// than planning established.
			reason := "no test was selected, and some changed test files could not be analyzed (see the unverified areas)"
			r.BaseTests = baseTestsSection(model.BaseTestsNotRun, reason)
			r.Unverified = append(r.Unverified, baseTestsUnverifiedPrefix+reason)
			return false
		}
		r.BaseTests = baseTestsSection(model.BaseTestsNoCandidates, "")
		return false
	}
	fmt.Fprintf(errOut, "Running the baseline versions of %d changed Go tests on candidate code in isolated Docker sandboxes...\n", len(sel.Tests))
	section, err := h.RunBaseTests(ctx, sel.Tests)
	r.BaseTests = &section
	if err != nil {
		fmt.Fprintf(errOut, "swiftproof: baseline versions of changed tests: %v\n", err)
		r.Unverified = append(r.Unverified, baseTestsUnverifiedPrefix+section.Reason)
		return true
	}
	if section.Status == model.BaseTestsNotRun {
		r.Unverified = append(r.Unverified, baseTestsUnverifiedPrefix+section.Reason)
		return false
	}
	// The line carries no count: report.Finalize may still downgrade a
	// result, and the section holds the finalized statuses.
	if countBaseTests(section.Tests, model.StatusUnverified) > 0 {
		r.Unverified = append(r.Unverified, baseTestsUnverifiedLine)
	}
	return false
}

// countBaseTests counts the tests with this status.
func countBaseTests(tests []model.BaseTest, status string) int {
	n := 0
	for _, t := range tests {
		if t.Status == status {
			n++
		}
	}
	return n
}

// baseTestsSection builds a base-tests section with no items.
func baseTestsSection(status, reason string) *model.BaseTests {
	return &model.BaseTests{Status: status, Reason: reason, Tests: []model.BaseTest{}, Note: model.BaseTestsNote}
}

// recordBaseTestsSkipped records why a requested --base-tests stage did not run
// (§1.7). It is a no-op in lint, when not requested, or when the section is
// already set. --checks=false cannot occur: the flags reject it with exit 3.
func recordBaseTestsSkipped(sc stageContext, requested bool, r *model.Report) {
	if sc.mode != "review" || !requested || r.BaseTests != nil {
		return
	}
	if sc.reason == reasonNoChangedFiles {
		r.BaseTests = baseTestsSection(model.BaseTestsNoCandidates, "no changed files")
		return
	}
	r.BaseTests = baseTestsSection(model.BaseTestsNotRun, skippedReason(sc))
}

// baseTestsLine is the stdout line of the base-tests section, printed whenever
// the section is present. It counts the finalized statuses and never calls a
// failure more than an outcome difference for a human to judge.
func baseTestsLine(b *model.BaseTests) string {
	if b == nil {
		return ""
	}
	reason := strings.TrimSuffix(strings.TrimSpace(b.Reason), ".")
	switch b.Status {
	case model.BaseTestsNoCandidates:
		if reason == "" {
			reason = "only the tests declared in modified, deleted or renamed Go test files are considered"
		}
		return "Changed baseline tests on candidate code: none selected (" + reason + ")."
	case model.BaseTestsNotRun:
		if reason == "" {
			reason = "no reason was recorded"
		}
		return "Changed baseline tests on candidate code: not run (" + reason + ")."
	}
	fails := countBaseTests(b.Tests, model.StatusFailsOnCandidate)
	passes := countBaseTests(b.Tests, model.StatusPassesOnCandidate)
	return fmt.Sprintf("Changed baseline tests on candidate code: %d FAILS_ON_CANDIDATE, %d PASSES_ON_CANDIDATE, %d UNVERIFIED (a failure is an outcome difference for a human to judge, not a reproduced issue; see base_tests).", fails, passes, len(b.Tests)-fails-passes)
}
