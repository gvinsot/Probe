package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const fuzzPath = "pkg/swiftproof_fuzz_abcdef12_test.go"

// fuzzSource is a stand-in for a rendered fuzz harness: the harness side only
// needs a Go test file that declares the expected test names.
const fuzzSource = `package pkg

import fuzzT "testing"

func TestSwiftProofFuzz_abcdef12_1(t *fuzzT.T) {}

func TestSwiftProofFuzz_abcdef12_2(t *fuzzT.T) {}
`

var fuzzNames = []string{"TestSwiftProofFuzz_abcdef12_1", "TestSwiftProofFuzz_abcdef12_2"}

// testNormalize accepts payloads that start with "stream:".
func testNormalize(p []byte) (string, error) {
	if !strings.HasPrefix(string(p), "stream:") {
		return "", errors.New("not a stream")
	}
	return "normalized " + string(p), nil
}

// fuzzGoLog renders go test -json events: a run and an action event for each name.
func fuzzGoLog(action string, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/m/pkg","Test":%q}`+"\n", n)
		if action != "" {
			fmt.Fprintf(&b, `{"Action":%q,"Package":"example.com/m/pkg","Test":%q}`+"\n", action, n)
		}
	}
	return b.String()
}

func fuzzRun(confirm, source bool) ObservedRun {
	return ObservedRun{Path: fuzzPath, Content: fuzzSource, TestNames: fuzzNames, Confirm: confirm, SaveSource: source, Deadline: time.Now().Add(time.Minute), Normalize: testNormalize}
}

// fuzzSide is what the fake sandbox does for one side of a fuzz run.
type fuzzSideRun struct {
	exit    int
	log     string
	payload string // written as is on the payload channel
}

// fakeFuzz installs a capture executor that answers the baseline run and the
// candidate run with the given behavior and returns the recorded argv.
func fakeFuzz(t *testing.T, h *Harness, base, candidate fuzzSideRun) *[][]string {
	t.Helper()
	var argvs [][]string
	h.executeCapture = func(_ context.Context, _ string, args []string, log, payload io.Writer) execution {
		argvs = append(argvs, append([]string(nil), args...))
		run := base
		if sideOf(h, args) == "candidate" {
			run = candidate
		}
		fmt.Fprint(log, run.log)
		fmt.Fprint(payload, run.payload)
		return execution{ExitCode: run.exit}
	}
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a fuzz run went without the payload channel")
		return execution{}
	}
	return &argvs
}

func artifactsOfKind(h *Harness, kind string) []model.Artifact {
	var out []model.Artifact
	for _, a := range h.Artifacts() {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func TestRunObservedStagesRunsAndRecords(t *testing.T) {
	h := fixture(t)
	var argvs [][]string
	h.executeCapture = func(ctx context.Context, _ string, args []string, log, payload io.Writer) execution {
		argvs = append(argvs, append([]string(nil), args...))
		for _, root := range []string{h.base, h.candidate} {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(fuzzPath)))
			if err != nil || string(b) != fuzzSource {
				t.Errorf("the harness is not staged byte-identical in %s during the run: %v", root, err)
			}
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a fuzz run has no deadline")
		}
		fmt.Fprint(log, fuzzGoLog("pass", fuzzNames...))
		fmt.Fprint(payload, coverageFrame("stream:"+sideOf(h, args)))
		return execution{ExitCode: 0}
	}
	h.execute = func(context.Context, string, []string, io.Writer) execution {
		t.Fatal("a fuzz run went without the payload channel")
		return execution{}
	}
	base, candidate, err := h.RunObserved(context.Background(), fuzzRun(false, true))
	if err != nil {
		t.Fatal(err)
	}
	if base.Check.Kind != model.CheckFuzzBase || base.Check.Status != "PASS" || base.Check.Results != "normalized stream:base" || base.OverflowSHA256 != "" {
		t.Fatalf("baseline %+v", base)
	}
	if candidate.Check.Kind != model.CheckFuzzCandidate || candidate.Check.Status != "PASS" || candidate.Check.Results != "normalized stream:candidate" {
		t.Fatalf("candidate %+v", candidate)
	}
	// The recorded checks are the returned ones.
	if checks := h.Checks(); len(checks) != 2 || !reflect.DeepEqual(checks[0], base.Check) || !reflect.DeepEqual(checks[1], candidate.Check) {
		t.Fatalf("ledger %+v", checks)
	}
	// The executed argv is the plain sandbox argv with only the capture script
	// changed, and the reviewed template plus -json -count=1 -run.
	command := []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestSwiftProofFuzz_abcdef12_1|TestSwiftProofFuzz_abcdef12_2)$"}
	if len(argvs) != 2 || sideOf(h, argvs[0]) != "base" || sideOf(h, argvs[1]) != "candidate" {
		t.Fatalf("runs %d", len(argvs))
	}
	for i, args := range argvs {
		dir := h.base
		if i == 1 {
			dir = h.candidate
		}
		name := args[indexOfArg(args, "--name")+1]
		plain := h.dockerArgs(name, dir, command)
		if len(args) != len(plain) {
			t.Fatalf("fuzz argv has %d arguments against %d for a plain run", len(args), len(plain))
		}
		for j := range args {
			if args[j] != plain[j] && !(plain[j] == wrapperScript && args[j] == captureScript(FuzzObservationsPath)) {
				t.Fatalf("fuzz argv differs at %d: %q against %q", j, args[j], plain[j])
			}
		}
		if !reflect.DeepEqual(args[len(args)-len(command):], command) {
			t.Fatalf("command tail %q", args[len(args)-len(command):])
		}
	}
	// The file is gone from both snapshots, and pkg itself stays.
	for _, root := range []string{h.base, h.candidate} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(fuzzPath))); !os.IsNotExist(err) {
			t.Fatalf("the harness file remains in %s", root)
		}
		if _, err := os.Stat(filepath.Join(root, "pkg", "main.go")); err != nil {
			t.Fatal(err)
		}
	}
	// The source and both streams are retained with their hashes.
	sources := artifactsOfKind(h, model.ArtifactFuzzHarness)
	streams := artifactsOfKind(h, model.ArtifactFuzzObservations)
	if len(sources) != 1 || len(streams) != 2 {
		t.Fatalf("artifacts %+v", h.Artifacts())
	}
	for _, a := range append(sources, streams...) {
		b, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			t.Fatalf("%s does not match its hash", a.Path)
		}
		if a.Kind == model.ArtifactFuzzHarness && string(b) != fuzzSource {
			t.Fatal("the source artifact is not the executed harness")
		}
	}
	// One audit event, under the reserved stage prefix; the reviewer budget
	// and the reviewer's tests are untouched.
	audit := h.Audit()
	if len(audit) != 1 || audit[0].Tool != "stage:run_fuzz" || audit[0].Status != "PASS" || !strings.Contains(audit[0].Arguments, base.Check.ID) {
		t.Fatalf("audit %+v", audit)
	}
	if h.generated != 0 || len(h.tests) != 0 {
		t.Fatal("a fuzz run used the reviewer's generated-test budget")
	}
}

func TestRunObservedConfirmationKinds(t *testing.T) {
	h := fixture(t)
	fakeFuzz(t, h, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:b")}, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:c")})
	base, candidate, err := h.RunObserved(context.Background(), fuzzRun(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if base.Check.Kind != model.CheckFuzzBaseConfirm || candidate.Check.Kind != model.CheckFuzzCandidateConfirm {
		t.Fatalf("kinds %s %s", base.Check.Kind, candidate.Check.Kind)
	}
	if len(artifactsOfKind(h, model.ArtifactFuzzHarness)) != 0 {
		t.Fatal("a confirmation run retained the source again")
	}
	if audit := h.Audit(); len(audit) != 1 || audit[0].Tool != "stage:run_fuzz_confirm" {
		t.Fatalf("audit %+v", audit)
	}
}

func TestRunObservedPreconditions(t *testing.T) {
	collision := "package pkg\n\nimport \"testing\"\n\nfunc TestSwiftProofFuzz_abcdef12_1(t *testing.T) {}\n"
	for name, tc := range map[string]struct {
		setup func(*Harness)
		edit  func(*ObservedRun)
	}{
		"closed harness": {setup: func(h *Harness) { h.Close() }},
		"no baseline":    {setup: func(h *Harness) { h.base = "" }},
		"multi-package":  {setup: func(h *Harness) { h.opts.Commands["generated_test"] = []string{"go", "test", "./..."} }},
		"file target":    {setup: func(h *Harness) { h.opts.Commands["generated_test"] = []string{"go", "test", "{file}"} }},
		"exec wrapper": {setup: func(h *Harness) {
			h.opts.Commands["generated_test"] = []string{"go", "test", "-exec=sudo", "{package}"}
		}},
		"not go":                {setup: func(h *Harness) { h.opts.Commands["generated_test"] = []string{"npx", "vitest", "run", "{file}"} }},
		"no validator":          {edit: func(r *ObservedRun) { r.Normalize = nil }},
		"no deadline":           {edit: func(r *ObservedRun) { r.Deadline = time.Time{} }},
		"not a test file":       {edit: func(r *ObservedRun) { r.Path = "pkg/swiftproof_fuzz.go" }},
		"unclean path":          {edit: func(r *ObservedRun) { r.Path = "pkg/../pkg/swiftproof_fuzz_abcdef12_test.go" }},
		"escaping path":         {edit: func(r *ObservedRun) { r.Path = "../swiftproof_fuzz_abcdef12_test.go" }},
		"names differ":          {edit: func(r *ObservedRun) { r.TestNames = fuzzNames[:1] }},
		"names reordered":       {edit: func(r *ObservedRun) { r.TestNames = []string{fuzzNames[1], fuzzNames[0]} }},
		"invalid Go":            {edit: func(r *ObservedRun) { r.Content = "package pkg\nfunc {" }},
		"existing on baseline":  {setup: func(h *Harness) { writeFile(h.base, fuzzPath, fuzzSource) }},
		"existing on candidate": {setup: func(h *Harness) { writeFile(h.candidate, fuzzPath, fuzzSource) }},
		"name collision":        {setup: func(h *Harness) { writeFile(h.candidate, "pkg/existing_test.go", collision) }},
	} {
		t.Run(name, func(t *testing.T) {
			h := fixture(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			h.executeCapture = func(context.Context, string, []string, io.Writer, io.Writer) execution {
				t.Fatal("a refused fuzz run executed")
				return execution{}
			}
			run := fuzzRun(false, true)
			if tc.edit != nil {
				tc.edit(&run)
			}
			if _, _, err := h.RunObserved(context.Background(), run); err == nil {
				t.Fatal("accepted")
			}
			if len(h.checks) != 0 || len(h.artifacts) != 0 || len(h.audit) != 0 {
				t.Fatalf("a refused run recorded checks %d, artifacts %d, audit %d", len(h.checks), len(h.artifacts), len(h.audit))
			}
		})
	}
}

func writeFile(root, rel, content string) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(content), 0o644)
}

// The ERROR and FAIL rules: only baseline-side harness failures and
// infrastructure causes are ERROR; candidate-side failures stay FAIL.
func TestRunObservedStatusRules(t *testing.T) {
	passLog := fuzzGoLog("pass", fuzzNames...)
	good := fuzzSideRun{log: passLog, payload: coverageFrame("stream:ok")}
	buildFailed := "# example.com/m/pkg\npkg/main.go:3:1: syntax error\nFAIL\texample.com/m/pkg [build failed]\n"
	for name, tc := range map[string]struct {
		base, candidate          fuzzSideRun
		baseStatus, candStatus   string
		baseResults, candResults bool
		baseCause                string
		rejected                 int
	}{
		"both pass":                         {base: good, candidate: good, baseStatus: "PASS", candStatus: "PASS", baseResults: true, candResults: true},
		"baseline passes without a stream":  {base: fuzzSideRun{log: passLog}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true, baseCause: fuzzBaseNoStream},
		"baseline stream rejected":          {base: fuzzSideRun{log: passLog, payload: coverageFrame("garbage")}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true, baseCause: fuzzBaseBadStream, rejected: 1},
		"baseline frame incomplete":         {base: fuzzSideRun{log: passLog, payload: "SWIFTPROOF"}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true, baseCause: fuzzBaseBadStream, rejected: 1},
		"baseline harness does not build":   {base: fuzzSideRun{exit: 1, log: buildFailed}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true, baseCause: fuzzBaseNotStarted},
		"baseline skips a harness test":     {base: fuzzSideRun{log: fuzzGoLog("pass", fuzzNames[0]), payload: coverageFrame("stream:ok")}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true, baseCause: fuzzBaseTestsMissed},
		"baseline process ends mid-harness": {base: fuzzSideRun{exit: 1, log: fuzzGoLog("", fuzzNames[0]), payload: coverageFrame("stream:partial")}, candidate: good, baseStatus: "FAIL", candStatus: "PASS", baseResults: true, candResults: true},
		"candidate does not build":          {base: good, candidate: fuzzSideRun{exit: 1, log: buildFailed}, baseStatus: "PASS", candStatus: "FAIL", baseResults: true},
		"candidate setup text in its log":   {base: good, candidate: fuzzSideRun{exit: 2, log: "[setup failed] permission denied\nfork/exec /x: permission denied\n"}, baseStatus: "PASS", candStatus: "FAIL", baseResults: true},
		"candidate passes without a stream": {base: good, candidate: fuzzSideRun{log: passLog}, baseStatus: "PASS", candStatus: "PASS", baseResults: true},
		"candidate stream rejected":         {base: good, candidate: fuzzSideRun{log: passLog, payload: coverageFrame("forged")}, baseStatus: "PASS", candStatus: "PASS", baseResults: true, rejected: 1},
		"candidate pollutes the channel":    {base: good, candidate: fuzzSideRun{log: passLog, payload: "noise\n" + coverageFrame("stream:ok")}, baseStatus: "PASS", candStatus: "PASS", baseResults: true, rejected: 1},
		"candidate infrastructure failure":  {base: good, candidate: fuzzSideRun{exit: 125, log: "docker: error"}, baseStatus: "PASS", candStatus: "ERROR", baseResults: true},
		"baseline infrastructure failure":   {base: fuzzSideRun{exit: 137, log: passLog}, candidate: good, baseStatus: "ERROR", candStatus: "PASS", candResults: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := fixture(t)
			fakeFuzz(t, h, tc.base, tc.candidate)
			base, candidate, err := h.RunObserved(context.Background(), fuzzRun(false, false))
			if err != nil {
				t.Fatal(err)
			}
			if base.Check.Status != tc.baseStatus || candidate.Check.Status != tc.candStatus {
				t.Fatalf("statuses %s/%s, want %s/%s\n%s", base.Check.Status, candidate.Check.Status, tc.baseStatus, tc.candStatus, base.Check.Output)
			}
			if (base.Check.Results != "") != tc.baseResults || (candidate.Check.Results != "") != tc.candResults {
				t.Fatalf("results %q / %q", base.Check.Results, candidate.Check.Results)
			}
			if tc.baseCause != "" && !strings.Contains(base.Check.Output, "swiftproof: "+tc.baseCause) {
				t.Fatalf("baseline log lacks its cause: %q", base.Check.Output)
			}
			if tc.baseCause == "" && strings.Contains(base.Check.Output, "swiftproof: ") || strings.Contains(candidate.Check.Output, "swiftproof: ") {
				t.Fatalf("a cause was recorded where none applies: %q / %q", base.Check.Output, candidate.Check.Output)
			}
			if got := len(artifactsOfKind(h, model.ArtifactFuzzPayloadRejected)); got != tc.rejected {
				t.Fatalf("%d rejected-payload artifacts, want %d", got, tc.rejected)
			}
			for _, a := range artifactsOfKind(h, model.ArtifactFuzzPayloadRejected) {
				if b, _ := os.ReadFile(a.Path); len(b) > fuzzRejectedLimit {
					t.Fatalf("rejected payload copy of %d bytes", len(b))
				}
			}
			// The ledger holds what was returned.
			for _, c := range []model.Check{base.Check, candidate.Check} {
				if got := findCheck(t, h, c.ID); !reflect.DeepEqual(got, c) {
					t.Fatalf("ledger %+v, returned %+v", got, c)
				}
			}
		})
	}
}

// A stream that does not fit the results budget of its side is kept only as
// the hashed artifact, without changing the check's status.
func TestRunObservedResultsBudgetOverflow(t *testing.T) {
	h := fixture(t)
	fakeFuzz(t, h, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:b")}, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:c")})
	h.resultsBytes = ResultsBudget
	base, candidate, err := h.RunObserved(context.Background(), fuzzRun(false, false))
	if err != nil {
		t.Fatal(err)
	}
	streams := artifactsOfKind(h, model.ArtifactFuzzObservations)
	if len(streams) != 2 {
		t.Fatalf("artifacts %+v", h.Artifacts())
	}
	for i, side := range []ObservedSide{base, candidate} {
		if side.Check.Status != "PASS" || side.Check.Results != "" || side.OverflowSHA256 != streams[i].SHA256 {
			t.Fatalf("side %d: %+v", i, side)
		}
	}
	if h.resultsBytes != ResultsBudget {
		t.Fatalf("results budget moved to %d", h.resultsBytes)
	}
}

// No run starts after the stage deadline, and the pre-reviewer ceiling limits
// the budget of a fuzz run.
func TestRunObservedDeadlineAndCeiling(t *testing.T) {
	h := fixture(t)
	h.executeCapture = func(context.Context, string, []string, io.Writer, io.Writer) execution {
		t.Fatal("a run started after the stage deadline")
		return execution{}
	}
	run := fuzzRun(false, false)
	run.Deadline = time.Now().Add(-time.Second)
	base, candidate, err := h.RunObserved(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if base.Check.Status != "SKIPPED" || candidate.Check.Status != "SKIPPED" {
		t.Fatalf("statuses %s %s", base.Check.Status, candidate.Check.Status)
	}
	h2 := fixture(t)
	h2.executeCapture = func(context.Context, string, []string, io.Writer, io.Writer) execution {
		t.Fatal("a run started inside the reviewer reserve")
		return execution{}
	}
	h2.opts.ReviewerReserve = h2.opts.MaxRuntime / 2
	h2.spent = h2.opts.MaxRuntime / 2
	base, _, err = h2.RunObserved(context.Background(), fuzzRun(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if base.Check.Status != "SKIPPED" || base.Check.Output != budgetReservedText {
		t.Fatalf("baseline %s %q", base.Check.Status, base.Check.Output)
	}
	// A run gets at most what remains before the deadline as its timeout.
	h3 := fixture(t)
	deadline := time.Now().Add(3 * time.Second)
	h3.executeCapture = func(ctx context.Context, _ string, _ []string, log, payload io.Writer) execution {
		if d, ok := ctx.Deadline(); !ok || d.After(deadline) {
			t.Errorf("run deadline %v after the stage deadline %v", d, deadline)
		}
		fmt.Fprint(log, fuzzGoLog("pass", fuzzNames...))
		fmt.Fprint(payload, coverageFrame("stream:x"))
		return execution{}
	}
	run = fuzzRun(false, false)
	run.Deadline = deadline
	if _, _, err := h3.RunObserved(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

func TestAddFuzzEvidence(t *testing.T) {
	h := fixture(t)
	fakeFuzz(t, h, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:b")}, fuzzSideRun{log: fuzzGoLog("pass", fuzzNames...), payload: coverageFrame("stream:c")})
	base, candidate, err := h.RunObserved(context.Background(), fuzzRun(false, false))
	if err != nil {
		t.Fatal(err)
	}
	valid := func() model.Evidence {
		return model.Evidence{Kind: model.EvidenceDifferentialFuzz, Description: "d", Path: fuzzPath, CheckID: candidate.Check.ID, BaseCheckID: base.Check.ID,
			Status: model.StatusDiverged, Runner: RunnerGo, TestNames: []string{fuzzNames[0]}, Output: "Input: F(1)"}
	}
	for i, status := range []string{model.StatusDiverged, model.StatusNotDiverged, model.StatusUnverified} {
		e := valid()
		e.Status = status
		stored, err := h.AddFuzzEvidence(e)
		if err != nil || stored.ID != fmt.Sprintf("evidence-%d", i+1) || stored.Status != status {
			t.Fatalf("%s: %+v %v", status, stored, err)
		}
	}
	for name, edit := range map[string]func(*model.Evidence){
		"wrong kind":         func(e *model.Evidence) { e.Kind = model.EvidenceDifferentialObservation },
		"reproduced":         func(e *model.Evidence) { e.Status = model.StatusReproduced },
		"not reproduced":     func(e *model.Evidence) { e.Status = model.StatusNotReproduced },
		"jest runner":        func(e *model.Evidence) { e.Runner = RunnerJest },
		"no test name":       func(e *model.Evidence) { e.TestNames = nil },
		"two test names":     func(e *model.Evidence) { e.TestNames = fuzzNames },
		"no path":            func(e *model.Evidence) { e.Path = "" },
		"unknown check":      func(e *model.Evidence) { e.CheckID = "check-9" },
		"unknown base":       func(e *model.Evidence) { e.BaseCheckID = "" },
		"swapped checks":     func(e *model.Evidence) { e.CheckID, e.BaseCheckID = e.BaseCheckID, e.CheckID },
		"repeat check":       func(e *model.Evidence) { e.RepeatCheckID = e.BaseCheckID },
		"criterion":          func(e *model.Evidence) { e.CriterionID = "AC-1" },
		"referenced symbols": func(e *model.Evidence) { e.ReferencedSymbols = []string{"F"} },
	} {
		e := valid()
		edit(&e)
		if _, err := h.AddFuzzEvidence(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(h.Evidence()) != 3 {
		t.Fatalf("%d evidence records", len(h.Evidence()))
	}
	h.Close()
	if _, err := h.AddFuzzEvidence(valid()); err == nil {
		t.Fatal("a closed harness accepted evidence")
	}
}
