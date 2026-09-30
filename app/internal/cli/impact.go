package cli

// Impact analysis (F6a owns this file; runImpactedTests and
// recordImpactedTestsSkipped belong to F6b). With --impact (the default) lint
// and review build a static index of the candidate's Go packages (and,
// lexically, of its TypeScript/JavaScript, Python and Rust sources) on the host,
// from committed Git objects, without executing repository code. It adds low
// impacted_caller signals and at most one medium analysis_limited signal, the
// impact section, and the index behind the reviewer's symbol tools.

import (
	"context"
	"fmt"
	"io"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
	"github.com/gvinsot/Probe/app/internal/report"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// impactResult is what the static impact analysis hands to the rest of the run.
type impactResult struct {
	report  *model.Impact       // nil with --impact=false
	signals []model.Signal      // impacted_caller / analysis_limited signals
	lookup  harness.SymbolIndex // nil when no index was built
	// index is the same index, for the repository graph; nil when none.
	index *symbols.Index
}

// analyzeImpact builds the static symbol index of the change. It returns an
// error only when ctx is cancelled (exit 4); any other failure is recorded in
// the section as limited or unavailable. Sensitive paths, which snapshots
// exclude, are neither read nor indexed.
func analyzeImpact(ctx context.Context, repo *gitrepo.Repository, change model.Change, enabled bool) (impactResult, error) {
	if err := ctx.Err(); err != nil {
		return impactResult{}, err
	}
	if !enabled {
		return impactResult{}, nil
	}
	res, err := symbols.Analyze(ctx, repo, change, symbols.Options{Sensitive: harness.IsSensitivePath})
	if err != nil {
		return impactResult{}, err
	}
	out := impactResult{report: res.Report(), signals: res.Signals()}
	// A nil *symbols.Index must not become a non-nil interface value.
	if index := res.Index(); index != nil {
		out.lookup, out.index = index, index
	}
	return out, nil
}

// runImpactedTests runs the unchanged tests that statically reach changed code
// on baseline and candidate (--impacted-tests, F6b). It selects them from the
// impact section, which res.report also points to, and records the outcome
// there (see runImpactedStage in impacted.go). It returns true when a run of
// the stage was recorded as ERROR, an operational failure.
func runImpactedTests(ctx context.Context, h *harness.Harness, r *model.Report, res impactResult, errOut io.Writer) (operational bool) {
	return runImpactedStage(ctx, h, r, errOut)
}

// recordImpactedTestsSkipped records why a requested --impacted-tests stage did
// not run (§1.7, F6b): not_run with the reason and the stage's Unverified
// entry, or no_candidates without changed files. It is a no-op in lint, when
// not requested, without an impact section, or when tests_status is already
// set. --checks=false and --impact=false cannot occur: the flags reject them
// with exit 3.
func recordImpactedTestsSkipped(sc stageContext, requested bool, r *model.Report) {
	if sc.mode != "review" || !requested || r.Impact == nil || r.Impact.TestsStatus != "" {
		return
	}
	if sc.reason == reasonNoChangedFiles {
		r.Impact.TestsStatus, r.Impact.TestsReason = model.ImpactTestsNoCandidates, "no changed files"
		return
	}
	r.Impact.TestsStatus, r.Impact.TestsReason = model.ImpactTestsNotRun, skippedReason(sc)
	r.Unverified = append(r.Unverified, impactedUnverifiedPrefix+r.Impact.TestsReason)
}

// impactLine is the stdout line of the impact section: counts for an indexed
// or limited index, the reason for an unavailable one, and nothing when no
// indexable Go file changed. With --impacted-tests it adds the finalized test
// statuses.
func impactLine(i *model.Impact) string {
	if i == nil {
		return ""
	}
	line := ""
	switch i.Status {
	case model.ImpactIndexed, model.ImpactLimited:
		callers, tests := 0, 0
		for _, f := range i.ChangedFunctions {
			callers += f.CallersTotal
			tests += f.TestsTotal
		}
		line = fmt.Sprintf("Impact analysis (static Go index, approximate): %d changed Go functions; %d caller sites in unchanged code and %d reaching tests, counted per function.", len(i.ChangedFunctions), callers, tests)
		if len(i.Languages) > 0 {
			line = fmt.Sprintf("Impact analysis (static index of %s sources, approximate): %d changed functions; %d caller sites in unchanged code and %d reaching tests, counted per function.", report.ImpactLanguages(i.Languages), len(i.ChangedFunctions), callers, tests)
		}
		if i.Status == model.ImpactLimited {
			line += " The index is limited; see the report."
		}
	case model.ImpactUnavailable:
		line = "Impact analysis: the static Go index is unavailable (" + redact.TruncateUTF8(i.Reason, 200) + ")."
		if len(i.Languages) > 0 {
			line = "Impact analysis: the static index is unavailable (" + redact.TruncateUTF8(i.Reason, 200) + ")."
		}
	}
	if i.TestsStatus != "" {
		counts := map[string]int{}
		seen := map[[2]string]bool{}
		for _, f := range i.ChangedFunctions {
			for _, t := range f.Tests {
				k := [2]string{t.Path, t.Name}
				if t.Status == "" || seen[k] {
					continue
				}
				seen[k] = true
				counts[t.Status]++
			}
		}
		tests := fmt.Sprintf("Impacted tests: %s", i.TestsStatus)
		if len(seen) > 0 {
			tests += fmt.Sprintf("; %d FAILS_ON_CANDIDATE, %d PASSES_ON_CANDIDATE, %d UNVERIFIED", counts[model.StatusFailsOnCandidate], counts[model.StatusPassesOnCandidate], counts[model.StatusUnverified])
		}
		if line != "" {
			line += " "
		}
		line += tests + "."
	}
	return line
}
