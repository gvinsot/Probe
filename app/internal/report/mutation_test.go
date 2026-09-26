package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// goTestLog renders a go test -json log of one package.
func goTestLog(pkg string, tests ...[2]string) string {
	var b strings.Builder
	result := "pass"
	for _, tc := range tests {
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", pkg, tc[0])
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`+"\n", tc[1], pkg, tc[0])
		if tc[1] == "fail" {
			result = "fail"
		}
	}
	fmt.Fprintf(&b, `{"Action":%q,"Package":%q}`+"\n", result, pkg)
	return b.String()
}

var mutationCommand = []string{"go", "test", "-json", "-count=1", "-failfast", "./price"}

const patchSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// mutationReport is a finalized-shape report with one surviving and one killed
// mutant whose recorded checks support them.
func mutationReport() *model.Report {
	const pkg = "example.test/shop/price"
	pass := [2]string{"TestClamp", "pass"}
	control := model.Check{ID: "mutation-check-1", Kind: model.CheckMutationControl, Status: "PASS", Command: append([]string(nil), mutationCommand...), Output: goTestLog(pkg, pass, [2]string{"TestDiscount", "pass"})}
	survivor := model.Check{ID: "mutation-check-2", Kind: model.CheckMutant, Status: "PASS", Command: append([]string(nil), mutationCommand...), Output: goTestLog(pkg, pass, [2]string{"TestDiscount", "pass"})}
	killer := model.Check{ID: "mutation-check-3", Kind: model.CheckMutant, Status: "FAIL", ExitCode: 1, Command: append([]string(nil), mutationCommand...), Output: goTestLog(pkg, pass, [2]string{"TestDiscount", "fail"})}
	var lines []model.DiffLine
	for n := 5; n <= 13; n++ {
		lines = append(lines, model.DiffLine{Kind: "add", NewLine: n, Content: "x"})
	}
	return &model.Report{
		Version: 1,
		Change:  model.Change{Files: []model.ChangedFile{{Path: "price/price.go", Status: "M", Additions: 9, Hunks: []model.Hunk{{NewStart: 5, NewLines: 9, Lines: lines}}}}, Additions: 9},
		Checks:  []model.Check{{ID: "check-1", Kind: "test", Status: "PASS", Command: []string{"go", "test", "./..."}, Output: "ok"}},
		Signals: []model.Signal{{ID: "sig-1", Kind: model.SignalSurvivingMutant, Path: "price/price.go", Line: 6, Side: "new", Symbol: "Discount", Severity: "medium", Summary: "A single-change mutant of this added line did not make any test of its package fail", Evidence: "Mutant mutant-1 (boundary) ..."}},
		Mutation: &model.Mutation{
			Status: model.MutationRan, Command: []string{"go", "test", "-json", "-count=1", "-failfast", "{package}"},
			Limits: model.MutationLimits{MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 400},
			Files:  []model.MutationFile{{Path: "price/price.go", Status: model.MutationFileEligible, AddedLines: 9, MutatedLines: 1}, {Path: "price/fast_windows.go", Status: model.MutationFileSkipped, Reason: "the file name has a GOOS or GOARCH suffix", AddedLines: 3}},
			Mutants: []model.Mutant{
				{ID: "mutant-1", Path: "price/price.go", Line: 6, Column: 11, Symbol: "Discount", Package: "./price", Operator: "boundary", Original: "<", Mutated: "<=", Status: model.MutantSurvived, CheckID: "mutation-check-2", ControlCheckID: "mutation-check-1", PatchSHA256: patchSHA, TestsRun: 2},
				{ID: "mutant-2", Path: "price/price.go", Line: 6, Column: 5, Symbol: "Discount", Package: "./price", Operator: "negate_condition", Original: "total < 0", Mutated: "!(total < 0)", Status: model.MutantKilled, CheckID: "mutation-check-3", ControlCheckID: "mutation-check-1", FailedTests: []string{"TestDiscount"}},
			},
			Generated: 2, Survived: 1, Killed: 1,
			Checks: []model.Check{control, survivor, killer},
			Note:   model.MutationNote,
		},
		Artifacts: []model.Artifact{
			{Path: "artifacts/run-mutation-check-1.log", Kind: model.ArtifactMutationCheckOutput, SHA256: strings.Repeat("1", 64)},
			{Path: "artifacts/run-mutant-1.patch", Kind: model.ArtifactMutantPatch, SHA256: patchSHA},
		},
	}
}

func TestVerifiedMutantsSurviveFinalize(t *testing.T) {
	r := mutationReport()
	before, _ := json.Marshal(r)
	verified := verifyMutation(r)
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("verifyMutation mutated the report")
	}
	if verified["mutant-1"] != model.MutantSurvived || verified["mutant-2"] != model.MutantKilled || len(verified) != 2 {
		t.Fatalf("verified %v", verified)
	}
	Finalize(r, true)
	m := r.Mutation
	if m.Status != model.MutationRan || m.Survived != 1 || m.Killed != 1 || m.Inconclusive != 0 {
		t.Fatalf("section %+v", m)
	}
	// Expected FAIL checks of killed mutants live in the mutation ledger and
	// never request human review; a surviving mutant is a medium signal only.
	if r.ExitCode != 0 || len(r.Unverified) != 0 {
		t.Fatalf("exit %d unverified %q", r.ExitCode, r.Unverified)
	}
	if len(r.Evidence) != 0 || len(r.Hypotheses) != 0 || len(r.ReproducedIssues) != 0 {
		t.Fatal("mutation created evidence or hypotheses")
	}
	target := false
	for _, rt := range r.ReviewTargets {
		if rt.Path == "price/price.go" && rt.StartLine <= 6 && rt.EndLine >= 6 && rt.Severity == "medium" && hasString(rt.Reasons, r.Signals[0].Summary) {
			target = true
		}
	}
	if !target {
		t.Fatalf("the survivor signal did not reach review targets: %+v", r.ReviewTargets)
	}
}

// Every tampering of a recorded mutation turns the affected mutant into
// INCONCLUSIVE and the section into incomplete, which requests human review.
func TestTamperedMutantsBecomeInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutant string
		edit   func(r *model.Report)
	}{
		{"mutation check ID collides with the main ledger", "mutant-1", func(r *model.Report) {
			r.Checks = append(r.Checks, model.Check{ID: "mutation-check-2", Kind: "test", Status: "PASS"})
		}},
		{"patch hash mismatch", "mutant-1", func(r *model.Report) { r.Artifacts[1].SHA256 = strings.Repeat("f", 64) }},
		{"patch artifact of another kind", "mutant-1", func(r *model.Report) { r.Artifacts[1].Kind = "check_output" }},
		{"no patch hash", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[0].PatchSHA256 = "" }},
		{"truncated mutant log", "mutant-1", func(r *model.Report) { r.Mutation.Checks[1].Truncated = true }},
		{"truncated control log", "mutant-1", func(r *model.Report) { r.Mutation.Checks[0].Truncated = true }},
		{"forged tests_run", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[0].TestsRun = 3 }},
		{"survivor with failed tests", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[0].FailedTests = []string{"TestDiscount"} }},
		{"survivor check failed", "mutant-1", func(r *model.Report) {
			r.Mutation.Checks[1].Status, r.Mutation.Checks[1].ExitCode = "FAIL", 1
		}},
		{"duplicate mutation check ID", "mutant-1", func(r *model.Report) {
			r.Mutation.Checks = append(r.Mutation.Checks, r.Mutation.Checks[1])
		}},
		{"commands differ", "mutant-1", func(r *model.Report) { r.Mutation.Checks[1].Command = []string{"go", "test", "-json", "./price"} }},
		{"control kind swapped", "mutant-1", func(r *model.Report) { r.Mutation.Checks[0].Kind = model.CheckMutant }},
		{"mutant kind swapped", "mutant-1", func(r *model.Report) { r.Mutation.Checks[1].Kind = model.CheckMutationControl }},
		{"check is its own control", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[0].CheckID = "mutation-check-1" }},
		{"check ID of the main ledger form", "mutant-1", func(r *model.Report) {
			r.Mutation.Checks[1].ID, r.Mutation.Mutants[0].CheckID = "check-2", "check-2"
		}},
		{"missing check", "mutant-1", func(r *model.Report) { r.Mutation.Checks = r.Mutation.Checks[:1] }},
		{"control failed", "mutant-1", func(r *model.Report) {
			r.Mutation.Checks[0].Status, r.Mutation.Checks[0].ExitCode = "FAIL", 1
		}},
		{"package differs from path", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[0].Package = "./other" }},
		{"malformed mutant ID", "mutant-01", func(r *model.Report) { r.Mutation.Mutants[0].ID = "mutant-01" }},
		{"duplicate mutant ID", "mutant-1", func(r *model.Report) { r.Mutation.Mutants[1].ID = "mutant-1" }},
		{"killed name that did not fail", "mutant-2", func(r *model.Report) { r.Mutation.Mutants[1].FailedTests = []string{"TestClamp"} }},
		{"killed without names", "mutant-2", func(r *model.Report) { r.Mutation.Mutants[1].FailedTests = nil }},
		{"killed check passed", "mutant-2", func(r *model.Report) {
			r.Mutation.Checks[2].Status, r.Mutation.Checks[2].ExitCode = "PASS", 0
		}},
		{"killed check truncated", "mutant-2", func(r *model.Report) { r.Mutation.Checks[2].Truncated = true }},
		{"killed with an infrastructure exit code", "mutant-2", func(r *model.Report) { r.Mutation.Checks[2].ExitCode = 125 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mutationReport()
			tc.edit(r)
			Finalize(r, true)
			found := false
			for _, mu := range r.Mutation.Mutants {
				if mu.ID == tc.mutant && (mu.Status == model.MutantSurvived || mu.Status == model.MutantKilled) && strings.Contains(tc.name, "duplicate mutant") == false {
					t.Fatalf("tampered mutant kept %s", mu.Status)
				}
				if mu.ID == tc.mutant && mu.Status == model.MutantInconclusive && mu.Reason == unverifiedMutantReason {
					found = true
				}
			}
			if !found {
				t.Fatalf("no INCONCLUSIVE %s: %+v", tc.mutant, r.Mutation.Mutants)
			}
			if r.Mutation.Status != model.MutationIncomplete || r.ExitCode != 2 {
				t.Fatalf("status %s exit %d", r.Mutation.Status, r.ExitCode)
			}
			if r.Mutation.Survived+r.Mutation.Killed+r.Mutation.Inconclusive != 2 {
				t.Fatalf("counts %+v", r.Mutation)
			}
		})
	}
}

// Surviving mutants never produce exit 1, with or without --ci.
func TestSurvivorsNeverExit1(t *testing.T) {
	for _, ci := range []bool{false, true} {
		r := mutationReport()
		r.Signals[0].Severity = "medium"
		Finalize(r, ci)
		if r.ExitCode != 0 {
			t.Fatalf("ci=%v: exit %d", ci, r.ExitCode)
		}
	}
}

func TestFinalizeIdempotentWithMutation(t *testing.T) {
	r := mutationReport()
	r.Mutation.Mutants[0].Original = "password: swordfish <"
	Finalize(r, true)
	first, _ := json.Marshal(r)
	Finalize(r, true)
	second, _ := json.Marshal(r)
	if string(first) != string(second) {
		t.Fatal("Finalize is not idempotent")
	}
	dir := t.TempDir()
	data, md := writeAndRead(t, dir, r)
	var decoded model.Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	Finalize(&decoded, true)
	data2, md2 := writeAndRead(t, t.TempDir(), &decoded)
	if string(data) != string(data2) || string(md) != string(md2) {
		t.Fatal("Write -> decode -> Finalize -> Write is not byte-identical")
	}
	if decoded.Mutation.Survived != 1 || decoded.Mutation.Killed != 1 {
		t.Fatalf("round trip changed the verdicts: %+v", decoded.Mutation)
	}
}

// forbiddenMutationWords are words and marks the section never uses about
// its results, outside the explicit negation of a score.
var forbiddenMutationWords = regexp.MustCompile(`(?i)\b(tested|verified|covered|safe|correct|score|complete|regression|bug|approved)\b|%`)

func TestMarkdownMutationSection(t *testing.T) {
	r := mutationReport()
	r.Mutation.Mutants[0].Symbol = "Discount_*evil*`x`"
	r.Mutation.Mutants[0].Mutated = "<= [link](http://x) <img src=x>"
	r.Mutation.Mutants[0].Original = "<"
	Finalize(r, true)
	if r.Mutation.Survived != 1 {
		t.Fatalf("setup: %+v", r.Mutation.Mutants[0])
	}
	md := string(Markdown(r))
	body := section(t, md, "## Mutation of Added Lines")
	if !strings.Contains(body, "**mutant-1** boundary at price/price.go:6 in Discount\\_\\*evil\\*\\`x\\`") {
		t.Fatalf("survivor line missing or unescaped:\n%s", body)
	}
	if strings.Contains(body, "mutant-2") || strings.Contains(body, "negate\\_condition") {
		t.Fatalf("a killed mutant was listed:\n%s", body)
	}
	if !strings.Contains(body, "1 killed (counted, not listed), 1 survived") || !strings.Contains(body, "2 generated, 2 selected, 0 not run because of max\\_mutants") {
		t.Fatalf("counts missing:\n%s", body)
	}
	if strings.Contains(body, "<img") || strings.Contains(body, "](http") {
		t.Fatalf("raw HTML or a link reached the Markdown:\n%s", body)
	}
	if stray := strayHeadings(md); len(stray) > 0 {
		t.Fatalf("stray headings %q", stray)
	}
	clean := strings.ReplaceAll(body, "There is no mutation score.", "")
	if m := forbiddenMutationWords.FindString(clean); m != "" {
		t.Fatalf("forbidden wording %q in:\n%s", m, body)
	}
	if !strings.Contains(body, "fast\\_windows.go") {
		t.Fatalf("skipped file not listed:\n%s", body)
	}
	if i, j := strings.Index(md, "## Changed-line Execution"), strings.Index(md, "## Mutation of Added Lines"); i < 0 || j < i {
		t.Fatal("the mutation section is not after Changed-line Execution")
	}
}

func TestMarkdownMutationStatuses(t *testing.T) {
	for status, want := range map[string]string{
		model.MutationNotRun:       "Mutation analysis did not run: initial checks disabled",
		model.MutationNoCandidates: "No mutant was run: no candidate mutant",
		model.MutationIncomplete:   "Status: incomplete. 1 of 1 selected mutants have no outcome",
	} {
		r := &model.Report{Mutation: &model.Mutation{Status: status}}
		switch status {
		case model.MutationNotRun:
			r.Mutation.Reason = "initial checks disabled (--checks=false)"
		case model.MutationNoCandidates:
			r.Mutation.Reason = "no candidate mutant on added lines of changed non-test Go files"
		case model.MutationIncomplete:
			r.Mutation.Mutants = []model.Mutant{{ID: "mutant-1", Path: "a.go", Line: 3, Package: ".", Operator: "boundary", Original: "<", Mutated: "<=", Status: model.MutantNotRun, Reason: "budget"}}
			r.Mutation.Reason = "1 of 1 selected mutants have no outcome (1 not run, 0 inconclusive)"
		}
		Finalize(r, false)
		body := section(t, string(Markdown(r)), "## Mutation of Added Lines")
		if !strings.Contains(body, want) {
			t.Errorf("%s:\n%s", status, body)
		}
		if strings.Contains(body, "Status: ran") {
			t.Errorf("%s rendered as ran", status)
		}
		if status == model.MutationIncomplete && !strings.Contains(body, "**mutant-1** NOT\\_RUN, boundary at a.go:3: budget") {
			t.Errorf("mutant without an outcome not listed:\n%s", body)
		}
	}
}

// Finalize repairs a section whose status its mutants do not support, and
// normalizes absent lists and the note.
func TestMutationStatusNormalization(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    model.Mutation
		want string
	}{
		{"unknown status", model.Mutation{Status: "complete"}, model.MutationIncomplete},
		{"no candidates with mutants", model.Mutation{Status: model.MutationNoCandidates, Mutants: []model.Mutant{{ID: "mutant-1", Status: model.MutantInvalid}}}, model.MutationIncomplete},
		{"ran with dropped candidates", model.Mutation{Status: model.MutationRan, Dropped: 3, Generated: 4, Mutants: []model.Mutant{{ID: "mutant-1", Status: model.MutantTimeout}}}, model.MutationIncomplete},
		{"ran with timeout and invalid", model.Mutation{Status: model.MutationRan, Mutants: []model.Mutant{{ID: "mutant-1", Status: model.MutantTimeout}, {ID: "mutant-2", Status: model.MutantInvalid}}}, model.MutationRan},
		{"not run", model.Mutation{Status: model.MutationNotRun}, model.MutationNotRun},
	} {
		r := &model.Report{Mutation: &tc.m}
		Finalize(r, true)
		m := r.Mutation
		if m.Status != tc.want || m.Note != model.MutationNote || m.Files == nil || m.Mutants == nil || m.Checks == nil || m.Command == nil {
			t.Errorf("%s: %+v", tc.name, m)
		}
		if m.Status == model.MutationIncomplete && m.Reason == "" {
			t.Errorf("%s: incomplete without a reason", tc.name)
		}
		wantExit := 0
		if tc.want == model.MutationIncomplete || tc.want == model.MutationNotRun {
			wantExit = 2
		}
		if r.ExitCode != wantExit {
			t.Errorf("%s: exit %d, want %d", tc.name, r.ExitCode, wantExit)
		}
	}
}

// The persisted report validates its mutation files: the JSON written for a
// verified report keeps both mutants and their checks.
func TestMutationJSONKeepsLedger(t *testing.T) {
	r := mutationReport()
	Finalize(r, true)
	dir := t.TempDir()
	if err := Write(dir, r, []string{FormatJSON}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Checks   []model.Check   `json:"checks"`
		Mutation *model.Mutation `json:"mutation"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Mutation == nil || len(saved.Mutation.Checks) != 3 || len(saved.Checks) != 1 || saved.Mutation.Mutants[0].Column != 11 || saved.Mutation.Mutants[0].Symbol != "Discount" {
		t.Fatalf("saved %+v", saved)
	}
}
