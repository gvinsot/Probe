package report

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/mutation"
)

// This file belongs to F4 (mutation of added lines). A mutant outcome is not
// evidence: it creates no evidence record and supports no hypothesis. A stored
// KILLED or SURVIVED is kept only when the recorded mutation-ledger checks and
// retained patch artifact reproduce it; everything else becomes INCONCLUSIVE.

var (
	mutantIDPattern       = regexp.MustCompile(`^mutant-[1-9][0-9]*$`)
	mutationCheckIDFormat = regexp.MustCompile(`^mutation-check-[1-9][0-9]*$`)
	sha256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// verifyMutation returns mutant ID -> status for the KILLED and SURVIVED
// mutants whose recorded checks support their status. It is pure: it reads r
// and never mutates it. A mutant is kept only when all of these hold:
//   - its ID is well formed and unique;
//   - check_id and control_check_id differ, each matches mutation-check-N,
//     resolves exactly once in r.Mutation.Checks and collides with no ID in
//     r.Checks; their kinds are mutant and mutation_control;
//   - the mutant's package is the {package} expansion of its path and an
//     argument of the recorded mutant command;
//   - mutation.Classify re-derives the same status from the two recorded checks
//     with harness.GoTestOutcome: the control PASS with exit code 0, untruncated,
//     at least one passing named test and none failing; identical non-empty
//     argv; and for SURVIVED a mutant PASS with exit code 0, untruncated, whose
//     count of passing top-level test names equals tests_run (at least 1),
//     no failed_tests and an r.Artifacts entry of kind mutant_patch with
//     sha256 == patch_sha256; for KILLED a mutant FAIL with exit code 1..124,
//     untruncated, and 1..5 distinct failed_tests names whose recorded outcome
//     is fail.
func verifyMutation(r *model.Report) map[string]string {
	m := r.Mutation
	if m == nil || len(m.Mutants) == 0 {
		return nil
	}
	mainIDs := map[string]bool{}
	for _, c := range r.Checks {
		mainIDs[c.ID] = true
	}
	checks := map[string]model.Check{}
	duplicate := map[string]bool{}
	for _, c := range m.Checks {
		if _, seen := checks[c.ID]; seen {
			duplicate[c.ID] = true
		}
		checks[c.ID] = c
	}
	resolve := func(id string) (model.Check, bool) {
		c, ok := checks[id]
		if !ok || duplicate[id] || mainIDs[id] || !mutationCheckIDFormat.MatchString(id) {
			return model.Check{}, false
		}
		return c, true
	}
	patches := map[string]bool{}
	for _, a := range r.Artifacts {
		if a.Kind == model.ArtifactMutantPatch && sha256Pattern.MatchString(a.SHA256) {
			patches[a.SHA256] = true
		}
	}
	ids := map[string]int{}
	for _, mu := range m.Mutants {
		ids[mu.ID]++
	}
	verified := map[string]string{}
	for _, mu := range m.Mutants {
		if mu.Status != model.MutantKilled && mu.Status != model.MutantSurvived {
			continue
		}
		if !mutantIDPattern.MatchString(mu.ID) || ids[mu.ID] != 1 || mu.CheckID == mu.ControlCheckID {
			continue
		}
		control, ok := resolve(mu.ControlCheckID)
		if !ok {
			continue
		}
		run, ok := resolve(mu.CheckID)
		if !ok || control.Kind != model.CheckMutationControl || run.Kind != model.CheckMutant {
			continue
		}
		if mu.Package == "" || mu.Package != mutation.PackageArg(mu.Path) || !containsArg(run.Command, mu.Package) {
			continue
		}
		v := mutation.Classify(control, run, harness.GoTestOutcome)
		if v.Status != mu.Status {
			continue
		}
		switch mu.Status {
		case model.MutantSurvived:
			if v.TestsRun < 1 || mu.TestsRun != v.TestsRun || len(mu.FailedTests) != 0 || !sha256Pattern.MatchString(mu.PatchSHA256) || !patches[mu.PatchSHA256] {
				continue
			}
		case model.MutantKilled:
			if !failedTestsRecorded(mu.FailedTests, run.Output) {
				continue
			}
		}
		verified[mu.ID] = mu.Status
	}
	return verified
}

// failedTestsRecorded reports 1..5 distinct names whose recorded outcome in
// output is fail.
func failedTestsRecorded(names []string, output string) bool {
	if len(names) == 0 || len(names) > 5 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
		if action, _ := harness.GoTestOutcome(output, name); action != "fail" {
			return false
		}
	}
	return true
}

func containsArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

// unverifiedMutantReason explains a status Finalize withdrew.
const unverifiedMutantReason = "the recorded mutation checks do not support this status"

// finalizeMutation turns every SURVIVED or KILLED mutant whose status is not
// in verified into INCONCLUSIVE (a status outside the known set as well),
// recomputes the counts, and turns a section whose status no longer holds
// into "incomplete": a "ran" section with an INCONCLUSIVE or NOT_RUN mutant or
// dropped candidates, a "no_candidates" section with mutants, or an unknown
// status. It returns true for status not_run or incomplete. It may mutate only
// r.Mutation, normalizes absent lists and the note, and is idempotent.
func finalizeMutation(r *model.Report, verified map[string]string) bool {
	m := r.Mutation
	if m == nil {
		return false
	}
	if m.Command == nil {
		m.Command = []string{}
	}
	if m.Files == nil {
		m.Files = []model.MutationFile{}
	}
	if m.Mutants == nil {
		m.Mutants = []model.Mutant{}
	}
	if m.Checks == nil {
		m.Checks = []model.Check{}
	}
	if m.Note == "" {
		m.Note = model.MutationNote
	}
	for i := range m.Mutants {
		mu := &m.Mutants[i]
		switch mu.Status {
		case model.MutantKilled, model.MutantSurvived:
			if verified[mu.ID] != mu.Status {
				mu.Status, mu.Reason = model.MutantInconclusive, unverifiedMutantReason
			}
		case model.MutantInvalid, model.MutantTimeout, model.MutantInconclusive, model.MutantNotRun:
		default:
			mu.Status, mu.Reason = model.MutantInconclusive, unverifiedMutantReason
		}
	}
	mutation.Recount(m)
	downgrade := false
	switch m.Status {
	case model.MutationNotRun, model.MutationIncomplete:
	case model.MutationNoCandidates:
		downgrade = len(m.Mutants) > 0
	case model.MutationRan:
		downgrade = m.Inconclusive+m.NotRun > 0 || m.Dropped > 0
	default:
		downgrade = true
	}
	if downgrade {
		m.Status, m.Reason = model.MutationIncomplete, mutation.IncompleteReason(*m)
		if m.Reason == "" {
			m.Reason = "the recorded section status does not match its mutants"
		}
	}
	return m.Status == model.MutationNotRun || m.Status == model.MutationIncomplete
}

// maxListedMutants caps the mutants without an outcome and the skipped files
// listed in Markdown; the JSON keeps every entry. Surviving mutants are all
// listed (at most max_mutants, 200).
const maxListedMutants = 20

// writeMutation renders "## Mutation of Added Lines" when r.Mutation is
// present. Killed mutants are counted, never listed. Every string goes through
// inline(); no percentage and no score is written.
func writeMutation(b *bytes.Buffer, r *model.Report) {
	m := r.Mutation
	line(b, "\n## Mutation of Added Lines\n")
	switch m.Status {
	case model.MutationNotRun:
		fmt.Fprintf(b, "Mutation analysis did not run: %s\n\n", inline(orNone(m.Reason)))
	case model.MutationNoCandidates:
		fmt.Fprintf(b, "No mutant was run: %s\n\n", inline(orNone(m.Reason)))
	case model.MutationRan:
		line(b, "Status: ran. Every selected mutant reached an outcome and max\\_mutants dropped none; this does not make the analysis exhaustive.\n")
	default:
		fmt.Fprintf(b, "Status: %s. %s\n\n", inline(m.Status), inline(orNone(m.Reason)))
	}
	if len(m.Command) > 0 {
		fmt.Fprintf(b, "Command: %s, run once per package with {package} expanded (max\\_mutants %d, timeout\\_seconds %d, max\\_runtime\\_seconds %d).\n\n", inline(strings.Join(m.Command, " ")), m.Limits.MaxMutants, m.Limits.TimeoutSeconds, m.Limits.MaxRuntimeSeconds)
	}
	if m.Status != model.MutationNotRun || len(m.Mutants) > 0 {
		coverageNote := ""
		if m.CoverageSkipped > 0 {
			coverageNote = fmt.Sprintf("; %d more were skipped on added lines the coverage run did not execute", m.CoverageSkipped)
		}
		fmt.Fprintf(b, "Candidate mutants on added lines: %d generated, %d selected, %d not run because of max\\_mutants%s.\n\n", m.Generated, len(m.Mutants), m.Dropped, coverageNote)
		if len(m.Mutants) > 0 {
			fmt.Fprintf(b, "Outcomes of the selected mutants: %d killed (counted, not listed), %d survived, %d did not build or pass go vet, %d timed out, %d inconclusive, %d not run.\n\n", m.Killed, m.Survived, m.Invalid, m.TimedOut, m.Inconclusive, m.NotRun)
		}
	}
	if m.Survived > 0 {
		line(b, "Surviving mutants (the package's tests all passed with the change):\n")
		for _, mu := range m.Mutants {
			if mu.Status != model.MutantSurvived {
				continue
			}
			fmt.Fprintf(b, "- **%s** %s at %s: replaced %s with %s; control check %s, mutant check %s; %d named tests passed in package %s; patch sha256 %s\n",
				inline(mu.ID), inline(mu.Operator), mutantLocation(mu), inline(orEmpty(mu.Original)), inline(orEmpty(mu.Mutated)), inline(mu.ControlCheckID), inline(mu.CheckID), mu.TestsRun, inline(mu.Package), inline(mu.PatchSHA256))
		}
		line(b, "")
	}
	listed, open := 0, 0
	for _, mu := range m.Mutants {
		if mu.Status != model.MutantInconclusive && mu.Status != model.MutantNotRun {
			continue
		}
		open++
		if listed == maxListedMutants {
			continue
		}
		if listed == 0 {
			line(b, "Mutants without an outcome:\n")
		}
		listed++
		fmt.Fprintf(b, "- **%s** %s, %s at %s: %s\n", inline(mu.ID), inline(mu.Status), inline(mu.Operator), mutantLocation(mu), inline(orNone(mu.Reason)))
	}
	if open > listed {
		fmt.Fprintf(b, "- … %d more in confidence-report.json\n", open-listed)
	}
	if listed > 0 {
		line(b, "")
	}
	listed, skipped := 0, 0
	for _, f := range m.Files {
		if f.Status != model.MutationFileSkipped {
			continue
		}
		skipped++
		if listed == maxListedMutants {
			continue
		}
		if listed == 0 {
			line(b, "Changed Go files that were not mutated:\n")
		}
		listed++
		fmt.Fprintf(b, "- %s: %s\n", inline(f.Path), inline(orNone(f.Reason)))
	}
	if skipped > listed {
		fmt.Fprintf(b, "- … %d more in confidence-report.json\n", skipped-listed)
	}
	if listed > 0 {
		line(b, "")
	}
	line(b, inline(orEmpty(m.Note)))
}

// mutantLocation renders path:line and the enclosing function, escaped.
func mutantLocation(mu model.Mutant) string {
	s := fmt.Sprintf("%s:%d", inline(mu.Path), mu.Line)
	if mu.EndLine > mu.Line {
		s += fmt.Sprintf("–%d", mu.EndLine)
	}
	if mu.Symbol != "" {
		s += " in " + inline(mu.Symbol)
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no reason was recorded"
	}
	return s
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
