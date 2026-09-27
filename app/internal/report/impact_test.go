package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// impactReport is a finalized-shape report with an indexed impact section:
// one changed function with a caller and two reaching tests.
func impactReport() *model.Report {
	return &model.Report{
		Change: model.Change{Files: []model.ChangedFile{{Path: "price/price.go", Status: "M", Hunks: []model.Hunk{{NewStart: 6, NewLines: 1, Lines: []model.DiffLine{{Kind: "add", NewLine: 6, Content: "x"}}}}}}},
		Impact: &model.Impact{
			Status: model.ImpactIndexed, IndexedFiles: 6,
			ChangedFunctions: []model.ImpactFunction{{
				Path: "price/price.go", Line: 4, EndLine: 10, Symbol: "example.test/shop/price.Total", Change: model.ChangeBodyChanged, Indexed: true,
				Callers:      []model.ImpactCaller{{Path: "api/handler.go", Line: 7, Symbol: "example.test/shop/api.Checkout", Depth: 1, Resolution: model.ResolutionStatic}},
				CallersTotal: 1,
				Tests: []model.ImpactTest{
					{Name: "TestTotal", Path: "price/price_test.go", Line: 5, Package: "example.test/shop/price", Depth: 1, Resolution: model.ResolutionStatic},
					{Name: "TestCheckout", Path: "api/handler_test.go", Line: 5, Package: "example.test/shop/api", Depth: 2, Resolution: model.ResolutionStatic},
				},
				TestsTotal: 2,
			}},
			Note: model.ImpactNote,
		},
	}
}

func TestImpactSectionRenders(t *testing.T) {
	r := impactReport()
	Finalize(r, false)
	md := string(Markdown(r))
	body := section(t, md, "## Impact Analysis")
	for _, want := range []string{
		"Static Go index of 6 files (approximate).",
		"- **example.test/shop/price.Total** (body changed) price/price.go:4–10",
		"  Callers in unchanged code found by the index: 1.",
		"  - caller example.test/shop/api.Checkout at api/handler.go:7 (static)",
		"  Existing tests reaching it within 3 references: 2.",
		"  - test TestTotal at price/price\\_test.go:5 (depth 1, static)",
		"  - test TestCheckout at api/handler\\_test.go:5 (depth 2, static)",
		"A listed caller is a place to review, not a defect",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("section lacks %q:\n%s", want, body)
		}
	}
	// The section sits between Changed-line Execution and Recorded Evidence.
	if i, j, k := strings.Index(md, "\n## Changed-line Execution\n"), strings.Index(md, "\n## Impact Analysis\n"), strings.Index(md, "\n## Recorded Evidence\n"); !(i < j && j < k) {
		t.Fatalf("section order %d %d %d", i, j, k)
	}
	if stray := strayHeadings(md); len(stray) > 0 {
		t.Fatalf("unexpected headings %q", stray)
	}
}

func TestImpactSectionStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.Impact)
		want   []string
	}{
		{"not applicable", func(i *model.Impact) {
			i.Status, i.Reason, i.ChangedFunctions = model.ImpactNotApplicable, "no indexable Go file changed (files under testdata or vendor, in directories whose name starts with _ or ., and sensitive paths are not indexed)", []model.ImpactFunction{}
		}, []string{"No indexable Go, TypeScript/JavaScript, Python or Rust file changed (Go files under testdata or vendor or in directories whose name starts with _, dependency and build directories such as node_modules, target or venv, directories whose name starts with ., and sensitive paths are not indexed), so no static index was built."}},
		{"search bound", func(i *model.Impact) {
			i.Status, i.Reason = model.ImpactLimited, "the caller search stopped at its limit of 10 visits; callers of 1 changed functions may be missing"
			i.ChangedFunctions[0].Reason = "the caller search stopped at its time or visit limit; callers of this function may be missing"
		}, []string{"  Search bound reached: the caller search stopped at its time or visit limit; callers of this function may be missing.", "  Callers in unchanged code found by the index: 1."}},
		{"unavailable", func(i *model.Impact) {
			i.Status, i.Reason = model.ImpactUnavailable, "no go.mod file in the committed tree"
			i.ChangedFunctions[0].Indexed, i.ChangedFunctions[0].Reason = false, "the static Go index is unavailable; callers and tests were not searched"
		}, []string{"The static Go index is unavailable: no go.mod file in the committed tree. Callers and tests of changed functions were not searched.", "  Not indexed, so its callers and tests were not searched: the static Go index is unavailable; callers and tests were not searched."}},
		{"limited", func(i *model.Impact) { i.Status, i.Reason = model.ImpactLimited, "1 Go files could not be parsed" }, []string{"Static Go index of 6 files (approximate), limited: 1 Go files could not be parsed. Callers and tests of changed functions may be missing."}},
		{"zero callers", func(i *model.Impact) {
			i.ChangedFunctions[0].Callers, i.ChangedFunctions[0].CallersTotal = []model.ImpactCaller{}, 0
		}, []string{"Callers in unchanged code found by the index: 0 (not proof that none exist)."}},
		{"no changed function", func(i *model.Impact) { i.ChangedFunctions = []model.ImpactFunction{} }, []string{"The changed non-test Go files contain no changed function or method."}},
		{"many callers", func(i *model.Impact) {
			fn := &i.ChangedFunctions[0]
			for n := 0; n < 9; n++ {
				fn.Callers = append(fn.Callers, fn.Callers[0])
			}
			fn.CallersTotal = 42
		}, []string{"found by the index: 42.", "  - … 5 more in confidence-report.json"}},
		{"many functions", func(i *model.Impact) {
			for n := 0; n < 24; n++ {
				i.ChangedFunctions = append(i.ChangedFunctions, i.ChangedFunctions[0])
			}
		}, []string{"- … 5 more changed functions in confidence-report.json"}},
		{"lexical languages", func(i *model.Impact) {
			i.Languages = []string{"go", "typescript", "python"}
			i.ChangedFunctions[0].Callers[0].Resolution = model.ResolutionName
		}, []string{"Static index of 6 Go, TypeScript/JavaScript and Python files (approximate; Go is type-checked, the other languages are scanned lexically and linked by name).", "(name)", "TypeScript/JavaScript, Python and Rust sources are scanned lexically, without type checking"}},
		{"lexical no changed function", func(i *model.Impact) {
			i.Languages = []string{"rust"}
			i.ChangedFunctions = []model.ImpactFunction{}
		}, []string{"Static index of 6 Rust files", "The changed non-test source files contain no changed function or method."}},
		{"impacted tests status", func(i *model.Impact) {
			i.TestsStatus, i.TestsReason = model.ImpactTestsNotRun, "not implemented in this build"
		}, []string{"Impacted tests: not\\_run: not implemented in this build."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := impactReport()
			tc.mutate(r.Impact)
			Finalize(r, false)
			body := section(t, string(Markdown(r)), "## Impact Analysis")
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("section lacks %q:\n%s", want, body)
				}
			}
			lower := strings.ToLower(body)
			for _, word := range []string{"no callers", "all callers", "unused", "complete", "safe", "covers", "tested", "verified", "regress", "%"} {
				if strings.Contains(lower, word) {
					t.Errorf("section uses %q:\n%s", word, body)
				}
			}
		})
	}
}

// Repository strings cannot forge headings, links or emphasis.
func TestImpactSectionEscapesHostileText(t *testing.T) {
	r := impactReport()
	fn := &r.Impact.ChangedFunctions[0]
	fn.Symbol = "x\n## Forged\n[link](https://evil.invalid) `code` *bold*"
	fn.Callers[0].Symbol = "y\r\n# Also forged"
	fn.Tests[0].Name = "TestX\n## Forged test"
	r.Impact.Reason = "r\n## Forged reason"
	Finalize(r, false)
	md := string(Markdown(r))
	if stray := strayHeadings(md); len(stray) > 0 {
		t.Fatalf("forged headings %q", stray)
	}
	body := section(t, md, "## Impact Analysis")
	// Only the renderer's own bold markers may be unescaped.
	trimmed := strings.ReplaceAll(strings.ReplaceAll(body, "- **", "- "), "** (", " (")
	for _, c := range []byte{'[', ']', '`', '*'} {
		if unescaped(trimmed, c) {
			t.Errorf("unescaped %q in:\n%s", c, body)
		}
	}
}

func TestLegacyReportHasNoImpactSection(t *testing.T) {
	r := &model.Report{}
	Finalize(r, false)
	md := string(Markdown(r))
	if strings.Contains(md, "## Impact Analysis") {
		t.Fatal("a report without impact rendered the section")
	}
	data, _ := json.Marshal(Sanitize(r))
	if strings.Contains(string(data), `"impact"`) {
		t.Fatal("a nil impact serialized")
	}
}

func impactEvidence(id, name, path, status string) model.Evidence {
	return model.Evidence{ID: id, Kind: model.EvidenceImpactedTestDifferential, Description: "d", Path: path, CheckID: "check-2", BaseCheckID: "check-1", Status: status, Runner: "go_test_json", TestNames: []string{name}}
}

// Impacted-test statuses come only from verified evidence of that test.
func TestFinalizeImpactDerivesTestStatuses(t *testing.T) {
	r := impactReport()
	r.Impact.TestsStatus = model.ImpactTestsRan
	tests := r.Impact.ChangedFunctions[0].Tests
	tests[0].EvidenceID = "evidence-1"
	tests[1].EvidenceID = "evidence-2"
	r.Evidence = []model.Evidence{
		impactEvidence("evidence-1", "TestTotal", "price/price_test.go", model.StatusFailsOnCandidate),
		impactEvidence("evidence-2", "TestCheckout", "api/handler_test.go", model.StatusPassesOnCandidate),
	}
	l := newLedger(r)
	l.admit("evidence-1", model.StatusFailsOnCandidate)
	l.admit("evidence-2", model.StatusPassesOnCandidate)
	if !finalizeImpact(r, l) {
		t.Fatal("a test failing on candidate code does not request review")
	}
	if tests[0].Status != model.StatusFailsOnCandidate || tests[1].Status != model.StatusPassesOnCandidate {
		t.Fatalf("%+v", tests)
	}
	targets := impactTargets(r)
	if len(targets) != 1 || targets[0].path != "price/price_test.go" || targets[0].start != 5 || targets[0].severity != "high" || strings.Contains(strings.ToLower(targets[0].reason), "regress") {
		t.Fatalf("targets %+v", targets)
	}
	// Idempotent.
	if !finalizeImpact(r, l) || tests[0].Status != model.StatusFailsOnCandidate {
		t.Fatal("not idempotent")
	}

	for _, tc := range []struct {
		name   string
		mutate func(r *model.Report, l *ledger)
		want   string
	}{
		{"not verified", func(r *model.Report, l *ledger) { delete(l.verified, "evidence-1") }, model.StatusUnverified},
		{"evidence of another test", func(r *model.Report, l *ledger) { r.Impact.ChangedFunctions[0].Tests[0].EvidenceID = "evidence-2" }, model.StatusUnverified},
		{"evidence of another kind", func(r *model.Report, l *ledger) {
			r.Evidence[0].Kind = model.EvidenceBaseTestDifferential
			*l = *newLedger(r)
			l.admit("evidence-1", model.StatusFailsOnCandidate)
		}, model.StatusUnverified},
		{"missing evidence", func(r *model.Report, l *ledger) { r.Impact.ChangedFunctions[0].Tests[0].EvidenceID = "evidence-9" }, model.StatusUnverified},
		{"forged status without evidence", func(r *model.Report, l *ledger) {
			r.Impact.ChangedFunctions[0].Tests[0].EvidenceID = ""
			r.Impact.ChangedFunctions[0].Tests[0].Status = model.StatusFailsOnCandidate
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := impactReport()
			r.Impact.TestsStatus = model.ImpactTestsRan
			r.Impact.ChangedFunctions[0].Tests[0].EvidenceID = "evidence-1"
			r.Evidence = []model.Evidence{
				impactEvidence("evidence-1", "TestTotal", "price/price_test.go", model.StatusFailsOnCandidate),
				impactEvidence("evidence-2", "TestCheckout", "api/handler_test.go", model.StatusPassesOnCandidate),
			}
			l := newLedger(r)
			l.admit("evidence-1", model.StatusFailsOnCandidate)
			l.admit("evidence-2", model.StatusPassesOnCandidate)
			tc.mutate(r, l)
			needs := finalizeImpact(r, l)
			got := r.Impact.ChangedFunctions[0].Tests[0].Status
			if got != tc.want || needs != (tc.want == model.StatusUnverified) || len(impactTargets(r)) != 0 {
				t.Fatalf("status %q needs %v targets %+v", got, needs, impactTargets(r))
			}
		})
	}
}

// Through Finalize, a stored FAILS_ON_CANDIDATE that no verifier re-derived is
// UNVERIFIED: exit 2 with --ci, never 1, and no high target.
func TestFinalizeNeverTrustsStoredImpactStatus(t *testing.T) {
	r := impactReport()
	r.Impact.TestsStatus = model.ImpactTestsRan
	r.Impact.ChangedFunctions[0].Tests[0].EvidenceID = "evidence-1"
	r.Impact.ChangedFunctions[0].Tests[0].Status = model.StatusFailsOnCandidate
	r.Evidence = []model.Evidence{impactEvidence("evidence-1", "TestTotal", "price/price_test.go", model.StatusFailsOnCandidate)}
	r.Checks = []model.Check{{ID: "check-1", Kind: model.CheckImpactedTestBase, Status: "PASS"}, {ID: "check-2", Kind: model.CheckImpactedTestCandidate, Status: "PASS"}}
	Finalize(r, true)
	if got := r.Impact.ChangedFunctions[0].Tests[0].Status; got != model.StatusUnverified || r.ExitCode != 2 {
		t.Fatalf("status %q exit %d", got, r.ExitCode)
	}
	for _, target := range r.ReviewTargets {
		if target.Severity == "high" {
			t.Fatalf("unverified status produced a high target: %+v", target)
		}
	}
}

// The static section alone never requests review and never changes the exit
// code; impacted tests that did not run do request it.
func TestImpactExitEffects(t *testing.T) {
	r := impactReport()
	r.Checks = []model.Check{{ID: "check-1", Kind: "test", Status: "PASS"}}
	r.Signals = []model.Signal{{Kind: model.SignalImpactedCaller, Path: "api/handler.go", Line: 7, Side: "new", Severity: "low", Summary: "Unchanged caller of a changed Go function"}, {Kind: model.SignalAnalysisLimited, Symbol: "impact_index", Path: "price/price.go", Line: 1, Side: "new", Severity: "medium", Summary: "Static impact analysis limited"}}
	Finalize(r, true)
	if r.ExitCode != 0 {
		t.Fatalf("static impact changed the exit code to %d", r.ExitCode)
	}
	r.Impact.TestsStatus = model.ImpactTestsNotRun
	Finalize(r, true)
	if r.ExitCode != 2 {
		t.Fatalf("impacted tests that did not run: exit %d", r.ExitCode)
	}
}

func TestFinalizeIdempotentWithImpact(t *testing.T) {
	r := impactReport()
	r.Impact.ChangedFunctions[0].Tests[0].Via = []string{"a.TestTotal", "a.Total"}
	r.Impact.ChangedFunctions[0].Tests[0].FileChanged = true
	Finalize(r, true)
	first, _ := json.Marshal(Sanitize(r))
	var again model.Report
	if err := json.Unmarshal(first, &again); err != nil {
		t.Fatal(err)
	}
	Finalize(&again, true)
	second, _ := json.Marshal(Sanitize(&again))
	if string(first) != string(second) {
		t.Fatalf("not idempotent:\n%s\n%s", first, second)
	}
	if !strings.Contains(string(first), `"via":["a.TestTotal","a.Total"]`) || !strings.Contains(string(first), `"file_changed":true`) || !strings.Contains(string(first), `"indexed":true`) {
		t.Fatalf("F6a fields not serialized: %s", first)
	}
	// A saved report with an empty or edited note gets the fixed note back:
	// the note is never taken from the stored report.
	for _, stored := range []string{"", "Every caller is listed; the change is safe."} {
		again.Impact.Note = stored
		Finalize(&again, true)
		if again.Impact.Note != model.ImpactNote {
			t.Fatalf("note %q not normalized: %q", stored, again.Impact.Note)
		}
		if md := string(Markdown(&again)); strings.Contains(md, "Every caller is listed") || !strings.Contains(md, "an absent caller is not proof that none exists") {
			t.Fatalf("edited note rendered:\n%s", md)
		}
	}
}
