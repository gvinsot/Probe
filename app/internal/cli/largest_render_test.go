package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/report"
)

// Integration agent I (contract §2 "Agent I"): the largest report a review
// can write with the default policy limits must still be re-rendered by
// `swiftproof report`, whose input limit is 64 MiB, and the re-render must be
// idempotent. The synthetic report fills the structured-results budget
// (harness.ResultsBudget) with fuzz observation streams of 16 fuzz functions,
// holds 200 mutants with one control run per mutant package (400 mutation
// checks), and gives every check a log of the default sandbox.max_output_bytes
// (64 KiB) of go test -json lines. Adversarial logs made of characters that
// JSON escapes as six bytes ("<", ">", "&") can still exceed the limit; CI.md
// documents that bound.

const largestOutputBytes = 65536 // config.Default().Sandbox.MaxOutputBytes

// largestGoLog returns go test -json lines of about size bytes.
func largestGoLog(pkg, test string, size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size-200; i++ {
		fmt.Fprintf(&b, `{"Time":"2026-09-26T10:00:%02d.%09dZ","Action":"output","Package":%q,"Test":%q,"Output":"    case_test.go:%d: value %d: got \"%d\", want \"%d\"\n"}`+"\n", i%60, i, pkg, test, 10+i%90, i, i, i+1)
	}
	fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Test":%q,"Elapsed":0.01}`+"\n", pkg, test)
	return b.String()
}

// largestStream returns an indented JSON document of about size bytes shaped
// like a normalized fuzz observation stream: per record a call, a hash, a
// length, a display value with quotes and flags.
func largestStream(seed, size int) string {
	type record struct {
		Index   int    `json:"index"`
		Call    string `json:"call"`
		SHA256  string `json:"sha256"`
		Len     int    `json:"len"`
		Display string `json:"display"`
		Flags   string `json:"flags"`
	}
	var records []record
	total := 0
	for i := 0; total < size; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", seed, i)))
		r := record{Index: i, Call: fmt.Sprintf("Label(%d, \"k%d\")", i, i%7), SHA256: hex.EncodeToString(sum[:]), Len: 12 + i%40,
			Display: fmt.Sprintf("string(\"item-%d \\\"quoted\\\" %s\")", i, strings.Repeat("x", i%48)), Flags: "---"}
		records = append(records, r)
		total += 190 + len(r.Display)
	}
	data, _ := json.MarshalIndent(map[string]any{"functions": []any{map[string]any{"state": "complete", "records": records}}}, "", "")
	return string(data)
}

func largestReport() model.Report {
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	head, base := strings.Repeat("d", 40), strings.Repeat("c", 40)
	r := model.Report{Version: 1, ToolVersion: "I-largest", GeneratedAt: at, IntentCriteria: []model.IntentCriterion{},
		Change: model.Change{BaseRef: "main", HeadRef: "HEAD", BaseCommit: base, HeadCommit: head, BaseRefCommit: base}}
	next := 0
	check := func(kind, status string, exit int, pkg, test string) model.Check {
		next++
		return model.Check{ID: fmt.Sprintf("check-%d", next), Kind: kind, Status: status, ExitCode: exit,
			Command: []string{"go", "test", "-json", "-count=1", "./" + pkg}, DurationMS: 1000, Output: largestGoLog("example.com/m/"+pkg, test, largestOutputBytes), Truncated: true}
	}
	// Initial checks and coverage.
	for _, kind := range []string{model.CheckTest, model.CheckTypecheck, model.CheckBuild, model.CheckCoverage} {
		r.Checks = append(r.Checks, check(kind, "PASS", 0, "p", "TestInitial"))
	}
	// Reviewer experiments (max_generated_tests 10: base, candidate, repeat).
	for i := 0; i < 10; i++ {
		for _, kind := range []string{model.CheckGeneratedBase, model.CheckGeneratedCandidate, model.CheckGeneratedBaseRepeat} {
			r.Checks = append(r.Checks, check(kind, "PASS", 0, "p", fmt.Sprintf("TestGenerated%d", i)))
		}
	}
	// Changed baseline tests (10 units) and impacted tests (4 packages).
	for i := 0; i < 10; i++ {
		r.Checks = append(r.Checks, check(model.CheckBaseTestBase, "PASS", 0, fmt.Sprintf("u%d", i), "TestBase"), check(model.CheckBaseTestHybrid, "FAIL", 1, fmt.Sprintf("u%d", i), "TestBase"))
	}
	for i := 0; i < 4; i++ {
		r.Checks = append(r.Checks, check(model.CheckImpactedTestBase, "PASS", 0, fmt.Sprintf("i%d", i), "TestImpacted"), check(model.CheckImpactedTestCandidate, "PASS", 0, fmt.Sprintf("i%d", i), "TestImpacted"))
	}
	// Differential fuzzing: 16 functions in 4 packages, four runs per package,
	// the structured-results budget spread over the 16 fuzz checks.
	fuzzReport := &model.FuzzReport{Status: model.FuzzRan, SeedScheme: model.FuzzSeedScheme, Note: model.FuzzNote, Skipped: []model.FuzzSkip{},
		Limits: model.FuzzLimits{MaxFunctions: 16, MaxPackages: 4, MaxInputs: 64, CallTimeoutMS: 1000, MaxRuntimeSeconds: 240}}
	perCheck := harness.ResultsBudget / 16
	for p := 0; p < 4; p++ {
		pkg := fmt.Sprintf("f%d", p)
		ids := make([]string, 4)
		for k, kind := range []string{model.CheckFuzzBase, model.CheckFuzzCandidate, model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm} {
			c := check(kind, "PASS", 0, pkg, "TestSwiftProofFuzz_0123456789abcdef_1")
			c.Results = largestStream(p*4+k, perCheck-4096)
			ids[k] = c.ID
			r.Checks = append(r.Checks, c)
		}
		for f := 0; f < 4; f++ {
			n := len(r.Evidence) + 1
			name := fmt.Sprintf("TestSwiftProofFuzz_0123456789abcdef_%d", f+1)
			r.Evidence = append(r.Evidence, model.Evidence{ID: fmt.Sprintf("evidence-%d", n), Kind: model.EvidenceDifferentialFuzz, Description: "d",
				Path: pkg + "/swiftproof_fuzz_0123456789abcdef_test.go", CheckID: ids[1], BaseCheckID: ids[0], Status: model.StatusNotDiverged, Runner: "go_test_json", TestNames: []string{name}})
			fuzzReport.Functions = append(fuzzReport.Functions, model.FuzzFunction{Path: pkg + "/f.go", Line: 3 + f, EndLine: 5 + f, Symbol: fmt.Sprintf("F%d", f), Signature: "func(int) int",
				TestName: name, Outcome: model.FuzzNotDiverged, EvidenceID: fmt.Sprintf("evidence-%d", n), Inputs: 64, Compared: 64,
				Checks: &model.FuzzChecks{Base: ids[0], Candidate: ids[1], BaseConfirm: ids[2], CandidateConfirm: ids[3]}})
		}
	}
	r.Fuzz = fuzzReport
	// Mutation: 200 mutants, each in its own package with its own control.
	mutation := &model.Mutation{Status: model.MutationRan, Command: []string{"go", "test", "-json", "-count=1", "{package}"}, Note: model.MutationNote,
		Limits: model.MutationLimits{MaxMutants: 200, TimeoutSeconds: 60, MaxRuntimeSeconds: 600}, Files: []model.MutationFile{}}
	for i := 0; i < 200; i++ {
		pkg := fmt.Sprintf("m%03d", i)
		control := model.Check{ID: fmt.Sprintf("mutation-check-%d", 2*i+1), Kind: model.CheckMutationControl, Status: "PASS", Command: []string{"go", "test", "-json", "-count=1", "./" + pkg},
			DurationMS: 900, Output: largestGoLog("example.com/m/"+pkg, "TestMutant", largestOutputBytes)}
		mutant := control
		mutant.ID, mutant.Kind = fmt.Sprintf("mutation-check-%d", 2*i+2), model.CheckMutant
		mutant.Command = append([]string(nil), control.Command...)
		mutation.Checks = append(mutation.Checks, control, mutant)
		mutation.Mutants = append(mutation.Mutants, model.Mutant{ID: fmt.Sprintf("mutant-%d", i+1), Path: pkg + "/m.go", Line: 4, Package: "./" + pkg, Operator: "boundary",
			Original: "<", Mutated: "<=", Status: model.MutantSurvived, CheckID: mutant.ID, ControlCheckID: control.ID, PatchSHA256: strings.Repeat("a", 64), TestsRun: 1})
	}
	mutation.Generated, mutation.Survived = 200, 200
	r.Mutation = mutation
	return r
}

func TestLargestReportReRendersWithinTheInputLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and renders a report of tens of MiB")
	}
	if raceEnabled {
		// A pure data-size test: the race detector adds no coverage and
		// turns 16 s into about five minutes.
		t.Skip("skipped under the race detector")
	}
	dir := t.TempDir()
	r := largestReport()
	report.Finalize(&r, true)
	first := filepath.Join(dir, "review")
	if err := report.Write(first, &r, []string{report.FormatMarkdown, report.FormatJSON}); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(first, "confidence-report.json")
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("review report: %d bytes (%.1f MiB) for %d checks, %d mutation checks and %d bytes of structured results",
		info.Size(), float64(info.Size())/(1<<20), len(r.Checks), len(r.Mutation.Checks), resultsBytes(r))
	if info.Size() > 64<<20 {
		t.Fatalf("the report is %d bytes, over the 64 MiB input limit of swiftproof report", info.Size())
	}
	if resultsBytes(r) < harness.ResultsBudget*9/10 {
		t.Fatalf("the fixture fills only %d bytes of the %d-byte results budget", resultsBytes(r), harness.ResultsBudget)
	}
	render := func(from, to string) (json, md []byte) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{"report", "--input", from, "--out", to}, &stdout, &stderr, "test"); code != 0 {
			t.Fatalf("report exit %d: %s", code, stderr.String())
		}
		json, err := os.ReadFile(filepath.Join(to, "confidence-report.json"))
		if err != nil {
			t.Fatal(err)
		}
		md, err = os.ReadFile(filepath.Join(to, "CONFIDENCE_REPORT.md"))
		if err != nil {
			t.Fatal(err)
		}
		return json, md
	}
	json1, md1 := render(input, filepath.Join(dir, "again"))
	json2, md2 := render(filepath.Join(dir, "again", "confidence-report.json"), filepath.Join(dir, "third"))
	if !bytes.Equal(json1, json2) || !bytes.Equal(md1, md2) {
		t.Fatal("re-rendering the re-rendered report changed it")
	}
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, json1) {
		t.Fatal("re-rendering the review report changed its JSON")
	}
}

func resultsBytes(r model.Report) int {
	n := 0
	for _, c := range r.Checks {
		n += len(c.Results)
	}
	return n
}
