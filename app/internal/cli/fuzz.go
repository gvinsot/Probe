package cli

// Differential fuzzing (F2). A configured fuzz policy selects changed Go and
// TS/JS functions whose signature is unchanged, runs identical seeded inputs
// through them on both revisions in the unchanged sandbox, and records the
// fuzz section, the fuzz checks and one differential_fuzz evidence record per
// function that ran. Nothing here sets an exit code: a divergence or an
// inconclusive function requests review through Finalize (exit 2 with --ci,
// never 1); a baseline-side harness failure is an ERROR check (exit 4).

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// fuzzTemplateReason is the not_run reason when changed Go functions are
// eligible but the generated_test template cannot run a fuzz harness.
const fuzzTemplateReason = "the generated_test command cannot run a fuzz harness; configure it as a single-package go test template with the {package} target, such as [\"go\", \"test\", \"{package}\"]"

// runFuzz runs differential fuzzing when the policy has fuzz and --fuzz is not
// false. Selection only parses the two snapshots on the host. A change with no
// function to run records no_candidates without running a container. The
// template decides the language (fuzz.SelectAll): eligible TS/JS functions run
// only with a verifiable Vitest or Jest generated_test template, and with any
// other template they are listed in fuzz.skipped with the reason; eligible Go
// functions run only with a Go fuzz template. Eligible Go functions that the
// template cannot run, when no TS/JS function runs either, or a selection
// failure, record not_run with an Unverified line. When a Vitest or Jest
// template runs TS/JS functions, the eligible Go functions are listed in
// fuzz.skipped and get one Unverified line. Every planned function ends
// diverged, not_diverged or inconclusive, and each inconclusive function and
// the budget cuts get an Unverified line.
func runFuzz(ctx context.Context, h *harness.Harness, cfg config.Config, change model.Change, baseDir, candidateDir string, r *model.Report, enabled bool, errOut io.Writer) {
	if cfg.Fuzz == nil || !enabled {
		return
	}
	limits := fuzz.NewLimits(cfg.Fuzz.Effective(cfg.Sandbox))
	template := cfg.Commands["generated_test"]
	family := fuzz.ScriptFamily(template)
	// The TS/JS enumeration reads candidate content on the host: besides its
	// own size and work bounds, the fuzz sub-cap and --deadline stop it.
	scriptCtx, cancel := context.WithTimeout(ctx, limits.MaxRuntime)
	plan, err := fuzz.SelectAll(scriptCtx, baseDir, candidateDir, change, r.Signals, limits, family)
	cancel()
	if err != nil {
		fuzzNotRun(cfg, r, nil, "the changed functions could not be selected: "+err.Error())
		return
	}
	// With a Vitest or Jest template, the Go functions are already skipped
	// (plan.GoTemplateSkipped); with any other template no TS/JS function is
	// planned. Either way, eligible Go functions that the template cannot run
	// make the stage not_run unless TS/JS functions run.
	if plan.GoTargets() > 0 && !fuzz.CommandSupported(template) || plan.Targets() == 0 && plan.GoTemplateSkipped > 0 {
		fuzzNotRun(cfg, r, plan.Skipped, fuzzTemplateReason)
		return
	}
	if plan.Targets() > 0 {
		fmt.Fprintf(errOut, "Running differential fuzzing of %s in isolated Docker sandboxes...\n", fuzzPlanText(plan))
	}
	rep := fuzz.Run(ctx, fuzzRunner{h: h}, plan, fuzz.Options{
		Limits:           limits,
		ObservationsPath: harness.FuzzObservationsPath,
		PayloadLimit:     harness.PayloadLimit(cfg.Sandbox.MaxOutputBytes),
		ScriptFamily:     family,
	})
	r.Fuzz = &rep
	r.Unverified = append(r.Unverified, fuzz.Unverified(rep, plan.BudgetSkipped, plan.GoTemplateSkipped)...)
}

// fuzzNotRun records a configured stage that could not run, with the skipped
// functions selection found, and its Unverified line.
func fuzzNotRun(cfg config.Config, r *model.Report, skipped []model.FuzzSkip, reason string) {
	section := fuzzSection(cfg, model.FuzzNotRun, reason)
	section.Skipped, section.SkippedTotal = boundedSkips(skipped)
	r.Fuzz = section
	r.Unverified = append(r.Unverified, "Differential fuzzing did not run: "+reason)
}

// boundedSkips orders skipped functions by path and line and keeps at most
// fuzz.MaxSkipped of them; the total counts every one.
func boundedSkips(skipped []model.FuzzSkip) ([]model.FuzzSkip, int) {
	out := append([]model.FuzzSkip{}, skipped...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	total := len(out)
	if total > fuzz.MaxSkipped {
		out = out[:fuzz.MaxSkipped]
	}
	return out, total
}

// fuzzRunner adapts the harness to fuzz.Runner. The harness does not import
// the fuzz package; models reach neither method.
type fuzzRunner struct{ h *harness.Harness }

func (f fuzzRunner) Observe(ctx context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	run := harness.ObservedRun{
		Path: req.Harness.Path, Content: req.Harness.Content, TestNames: req.Harness.TestNames(),
		Confirm: req.Confirm, SaveSource: req.SaveSource, Deadline: req.Deadline, Normalize: req.Harness.Normalize,
		Runner: req.Harness.EvidenceRunner(),
	}
	if run.Runner == harness.RunnerJest {
		run.Started = fuzz.StartedTests
	}
	base, candidate, err := f.h.RunObserved(ctx, run)
	return fuzz.Side{Check: base.Check, OverflowSHA256: base.OverflowSHA256}, fuzz.Side{Check: candidate.Check, OverflowSHA256: candidate.OverflowSHA256}, err
}

func (f fuzzRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	return f.h.AddFuzzEvidence(e)
}

// fuzzSection builds a fuzz section with no function results.
func fuzzSection(cfg config.Config, status, reason string) *model.FuzzReport {
	e := cfg.Fuzz.Effective(cfg.Sandbox)
	return &model.FuzzReport{
		Status: status, Reason: reason, SeedScheme: model.FuzzSeedScheme,
		Limits:    model.FuzzLimits{MaxFunctions: e.MaxFunctions, MaxPackages: e.MaxPackages, MaxInputs: e.MaxInputs, CallTimeoutMS: e.CallTimeoutMS, MaxRuntimeSeconds: e.MaxRuntimeSeconds},
		Functions: []model.FuzzFunction{}, Skipped: []model.FuzzSkip{}, Note: model.FuzzNote,
	}
}

// recordFuzzSkipped records why a configured fuzz stage did not run (§1.7). It
// is a no-op in lint, without a fuzz policy, or when the section is already set.
// No changed files gives no_candidates; --fuzz=false or --checks=false, an
// explicit operator choice, gives disabled; any other missed execution gives
// not_run with the stage reason and an Unverified line.
func recordFuzzSkipped(cfg config.Config, sc stageContext, enabled bool, r *model.Report) {
	if sc.mode != "review" || cfg.Fuzz == nil || r.Fuzz != nil {
		return
	}
	switch {
	case sc.reason == reasonNoChangedFiles:
		r.Fuzz = fuzzSection(cfg, model.FuzzNoCandidates, "no changed files")
	case !enabled:
		r.Fuzz = fuzzSection(cfg, model.FuzzDisabled, "--fuzz=false")
	case !sc.checks:
		r.Fuzz = fuzzSection(cfg, model.FuzzDisabled, "--checks=false")
	default:
		fuzzNotRun(cfg, r, nil, skippedReason(sc))
	}
}

// fuzzLine is the stdout line of the fuzz section: counts of outcomes only,
// never a percentage, and no value. It counts the planned functions and,
// among them, those with recorded fuzz checks: a function that the sub-cap,
// the deadline or a harness failure kept from running has none, and is never
// counted as run.
func fuzzLine(f *model.FuzzReport) string {
	if f == nil {
		return ""
	}
	switch f.Status {
	case model.FuzzDisabled:
		return "Differential fuzzing: disabled for this run (" + orNoReason(f.Reason) + ")."
	case model.FuzzNotRun:
		return "Differential fuzzing did not run: " + orNoReason(f.Reason) + "."
	case model.FuzzNoCandidates:
		return fmt.Sprintf("Differential fuzzing: no function ran (%s; %d skipped).", orNoReason(f.Reason), f.SkippedTotal)
	}
	diverged, notDiverged, inconclusive, withChecks := 0, 0, 0, 0
	for _, fn := range f.Functions {
		if fn.Checks != nil {
			withChecks++
		}
		switch fn.Outcome {
		case model.FuzzDiverged:
			diverged++
		case model.FuzzNotDiverged:
			notDiverged++
		default:
			inconclusive++
		}
	}
	return fmt.Sprintf("Differential fuzzing: %s planned, %d with recorded fuzz checks; %d diverged, %d not diverged, %d inconclusive; %d skipped.", fuzzCount(len(f.Functions), "changed function"), withChecks, diverged, notDiverged, inconclusive, f.SkippedTotal)
}

// fuzzPlanText describes what a plan runs, for example "7 changed Go
// functions in 2 packages" or "2 changed TS/JS functions in 1 module".
func fuzzPlanText(plan fuzz.Plan) string {
	goFns, goPkgs, scriptFns, modules := 0, 0, 0, 0
	for _, pkg := range plan.Packages {
		if pkg.Script != nil {
			scriptFns += len(pkg.Targets)
			modules++
		} else {
			goFns += len(pkg.Targets)
			goPkgs++
		}
	}
	var parts []string
	if goFns > 0 {
		parts = append(parts, fuzzCount(goFns, "changed Go function")+" in "+fuzzCount(goPkgs, "package"))
	}
	if scriptFns > 0 {
		parts = append(parts, fuzzCount(scriptFns, "changed TS/JS function")+" in "+fuzzCount(modules, "module"))
	}
	return strings.Join(parts, " and ")
}

// fuzzCount writes n and noun, with a plural s unless n is 1.
func fuzzCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
