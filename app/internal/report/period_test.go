package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// A reason recorded verbatim from a harness text that ends with a period is
// printed with one final period in the Markdown, never two.
func TestMarkdownReasonsEndWithOnePeriod(t *testing.T) {
	const reason = "no run started: Sandbox runtime reserved for reviewer experiments."
	var b bytes.Buffer
	writeImpact(&b, &model.Report{Impact: &model.Impact{Status: model.ImpactUnavailable, Reason: "the index failed.", TestsStatus: model.ImpactTestsNotRun, TestsReason: reason, Note: model.ImpactNote}})
	writeImpact(&b, &model.Report{Impact: &model.Impact{Status: model.ImpactLimited, Reason: "the file limit was reached.", IndexedFiles: 3, Note: model.ImpactNote,
		ChangedFunctions: []model.ImpactFunction{
			{Symbol: "pkg.A", Path: "pkg/a.go", Line: 1, EndLine: 2, Reason: "not indexed."},
			{Symbol: "pkg.B", Path: "pkg/b.go", Line: 1, EndLine: 2, Indexed: true, Reason: "the caller limit was reached.", Callers: []model.ImpactCaller{}, Tests: []model.ImpactTest{}},
		}}})
	for _, status := range []string{model.FuzzNotRun, model.FuzzNoCandidates, model.FuzzDisabled} {
		writeFuzz(&b, &model.Report{Fuzz: &model.FuzzReport{Status: status, Reason: reason, Functions: []model.FuzzFunction{}, Skipped: []model.FuzzSkip{}}})
	}
	md := b.String()
	if strings.Contains(md, "..") || strings.Contains(md, ".)") {
		t.Fatalf("a doubled period:\n%s", md)
	}
	for _, want := range []string{
		"Impacted tests: not\\_run: no run started: Sandbox runtime reserved for reviewer experiments.\n",
		"The static Go index is unavailable: the index failed. Callers",
		"limited: the file limit was reached. Callers",
		"Not indexed, so its callers and tests were not searched: not indexed.\n",
		"Search bound reached: the caller limit was reached.\n",
		"Differential fuzzing did not run: no run started: Sandbox runtime reserved for reviewer experiments.\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, md)
		}
	}
}
