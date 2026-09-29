package cli

import (
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Every requested v0.4 stage that record*Skipped marks not_run adds exactly
// one Unverified line naming the reason, and a stage with no changed files
// adds none: the Unverified Areas section must list every stage that did not
// run.
func TestRecordSkippedAddsOneUnverifiedLine(t *testing.T) {
	cfg := stageTestPolicy()
	prepareFailed := stageContext{mode: "review", checks: true, reason: reasonPrepareFailed}
	noFiles := stageContext{mode: "review", checks: true, reason: reasonNoChangedFiles}
	for _, tc := range []struct {
		name   string
		record func(stageContext, *model.Report)
		prefix string
	}{
		{"fuzz", func(sc stageContext, r *model.Report) { recordFuzzSkipped(cfg, sc, true, r) }, "Differential fuzzing did not run: "},
		{"mutation", func(sc stageContext, r *model.Report) { recordMutationSkipped(cfg, sc, r) }, "Mutation analysis did not run: "},
		{"base tests", func(sc stageContext, r *model.Report) { recordBaseTestsSkipped(sc, true, r) }, baseTestsUnverifiedPrefix},
		{"impacted tests", func(sc stageContext, r *model.Report) { recordImpactedTestsSkipped(sc, true, r) }, impactedUnverifiedPrefix},
	} {
		r := &model.Report{Impact: &model.Impact{Status: model.ImpactUnavailable}}
		tc.record(prepareFailed, r)
		if len(r.Unverified) != 1 || r.Unverified[0] != tc.prefix+reasonPrepareFailed {
			t.Errorf("%s not run: unverified %q, want one line %q", tc.name, r.Unverified, tc.prefix+reasonPrepareFailed)
		}
		r = &model.Report{Impact: &model.Impact{Status: model.ImpactUnavailable}}
		tc.record(noFiles, r)
		if len(r.Unverified) != 0 {
			t.Errorf("%s with no changed files: unverified %q, want none", tc.name, r.Unverified)
		}
	}
	// The base-tests line keeps the reason's wording.
	r := &model.Report{}
	recordBaseTestsSkipped(prepareFailed, true, r)
	if !strings.HasPrefix(r.Unverified[0], "Baseline versions of changed tests did not run: ") {
		t.Fatalf("base-tests line %q", r.Unverified[0])
	}
}
