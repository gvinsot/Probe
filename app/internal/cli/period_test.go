package cli

import (
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// A reason recorded verbatim from a harness text that ends with a period is
// printed with one final period on stdout, never two.
func TestStdoutReasonsEndWithOnePeriod(t *testing.T) {
	const reason = "the control run mutation-check-1 was not executed: Sandbox runtime reserved for reviewer experiments."
	for name, got := range map[string]string{
		"mutation not run":      mutationLine(&model.Mutation{Status: model.MutationNotRun, Reason: reason}),
		"mutation no mutant":    mutationLine(&model.Mutation{Status: model.MutationNoCandidates, Reason: reason}),
		"fuzz not run":          fuzzLine(&model.FuzzReport{Status: model.FuzzNotRun, Reason: reason}),
		"fuzz disabled":         fuzzLine(&model.FuzzReport{Status: model.FuzzDisabled, Reason: reason}),
		"fuzz no candidates":    fuzzLine(&model.FuzzReport{Status: model.FuzzNoCandidates, Reason: reason}),
		"base tests not run":    baseTestsLine(&model.BaseTests{Status: model.BaseTestsNotRun, Reason: reason}),
		"mutation empty reason": mutationLine(&model.Mutation{Status: model.MutationNotRun, Reason: " "}),
	} {
		if strings.Contains(got, "..") || strings.Contains(got, ".)") || !strings.HasSuffix(got, ".") {
			t.Errorf("%s: %q", name, got)
		}
	}
}
