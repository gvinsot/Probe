package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

const btPkg = "example.test/m/pkg"

// btRun is one execution the fake executor received.
type btRun struct {
	side  string // base | hybrid | candidate
	dir   string
	name  string
	args  []string
	names []string          // the -run names
	files map[string]string // the mounted tree, for hybrid runs
}

func btWriteTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func btReadTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func btHarness(t *testing.T, base, candidate map[string]string, template []string) *Harness {
	t.Helper()
	b, c := t.TempDir(), t.TempDir()
	btWriteTree(t, b, base)
	btWriteTree(t, c, candidate)
	h, err := New(Options{CandidateDir: c, BaseDir: b, ArtifactDir: t.TempDir(), Image: "test-image:local", MaxOutputBytes: 64 * 1024,
		Commands: map[string][]string{"test": {"go", "test", "./..."}, "generated_test": template}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// btMount returns the host directory mounted at /source.
func btMount(args []string) string {
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			for _, part := range strings.Split(args[i+1], ",") {
				if strings.HasPrefix(part, "src=") {
					return strings.TrimPrefix(part, "src=")
				}
			}
		}
	}
	return ""
}

// btNames returns the names of the -run expression ^(A|B)$.
func btNames(args []string) []string {
	for i, a := range args {
		if a == "-run" && i+1 < len(args) {
			expr := strings.TrimSuffix(strings.TrimPrefix(args[i+1], "^("), ")$")
			return strings.Split(expr, "|")
		}
	}
	return nil
}

// btEvents renders go test -json events: name, action pairs.
func btEvents(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n", btPkg, pairs[i])
		fmt.Fprintf(&b, "{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", pairs[i+1], btPkg, pairs[i])
	}
	return b.String()
}

// btExec installs a fake executor; respond returns the log and the result of
// the n-th run.
func btExec(t *testing.T, h *Harness, respond func(r btRun, n int) (string, execution)) *[]btRun {
	t.Helper()
	var runs []btRun
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		r := btRun{name: name, args: append([]string(nil), args...), dir: btMount(args), names: btNames(args)}
		switch {
		case r.dir == h.base:
			r.side = "base"
		case r.dir == h.candidate:
			r.side = "candidate"
		case strings.HasPrefix(filepath.Base(r.dir), "hybrid-"):
			r.side = "hybrid"
			r.files = btReadTree(t, r.dir)
		}
		output, result := respond(r, len(runs))
		runs = append(runs, r)
		_, _ = io.WriteString(out, output)
		return result
	}
	return &runs
}

// btAll answers every run with the same outcome for every requested name,
// failing the run when any name fails.
func btAll(baseAction, hybridAction string) func(btRun, int) (string, execution) {
	return func(r btRun, _ int) (string, execution) {
		action := baseAction
		if r.side == "hybrid" {
			action = hybridAction
		}
		var pairs []string
		for _, n := range r.names {
			pairs = append(pairs, n, action)
		}
		exit := 0
		if action == "fail" {
			exit = 1
		}
		return btEvents(pairs...), execution{ExitCode: exit}
	}
}

func btTreeDigest(t *testing.T, root string) string {
	t.Helper()
	files := btReadTree(t, root)
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(sum, "%q %q\n", k, files[k])
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func btEvidence(t *testing.T, h *Harness, id string) model.Evidence {
	t.Helper()
	for _, e := range h.Evidence() {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("evidence %s not recorded", id)
	return model.Evidence{}
}

func btCheck(t *testing.T, h *Harness, id string) model.Check {
	t.Helper()
	for _, c := range h.Checks() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %s not recorded", id)
	return model.Check{}
}

const (
	btBaseTest = "package pkg\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(\"V\")\n\t}\n}\n\nfunc TestB(t *testing.T) {}\n"
	btCandTest = "package pkg\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif V() < 1 {\n\t\tt.Fatal(\"V\")\n\t}\n}\n"
)

func btTrees() (base, candidate map[string]string) {
	base = map[string]string{
		"go.mod":             "module example.test/m\n\ngo 1.23\n",
		"pkg/v.go":           "package pkg\n\nfunc V() int { return 1 }\n",
		"pkg/a_test.go":      btBaseTest,
		"pkg/testdata/g.txt": "base",
		"other/o.go":         "package other\n",
		"other/o_test.go":    "package other\n",
	}
	candidate = map[string]string{
		"go.mod":                 "module example.test/m\n\ngo 1.23\n",
		"pkg/v.go":               "package pkg\n\nfunc V() int { return 2 }\n",
		"pkg/a_test.go":          btCandTest,
		"pkg/b_test.go":          "package pkg\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"pkg/testdata/g.txt":     "candidate",
		"pkg/testdata/extra.txt": "only in the candidate",
		"other/o.go":             "package other\n\n// changed\n",
		"other/o_test.go":        "package other\n",
	}
	return base, candidate
}

func btSelected() []model.BaseTest {
	return []model.BaseTest{
		{Name: "TestA", Path: "pkg/a_test.go", Line: 5, EndLine: 9, CandidatePath: "pkg/a_test.go", CandidateLine: 5, CandidateEndLine: 9, Change: model.BaseTestModified, Status: model.StatusUnverified},
		{Name: "TestB", Path: "pkg/a_test.go", Line: 11, EndLine: 11, Change: model.BaseTestRemoved, Status: model.StatusUnverified},
	}
}

func TestBaseTestsHybridTreeCommandsAndEvidence(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	baseBefore, candidateBefore := btTreeDigest(t, h.base), btTreeDigest(t, h.candidate)
	// TestA fails on the hybrid tree; TestB passes there, but inside a failed
	// run, so it gets a run pair of its own.
	runs := btExec(t, h, func(r btRun, n int) (string, execution) {
		if r.side == "hybrid" && len(r.names) == 2 {
			return btEvents("TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		return btAll("pass", "pass")(r, n)
	})
	res, err := h.RunBaseTests(context.Background(), btSelected())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.BaseTestsRan || res.Reason != "" || res.Note != model.BaseTestsNote || len(res.Tests) != 2 {
		t.Fatalf("section %+v", res)
	}
	if len(*runs) != 4 {
		t.Fatalf("%d runs, want base+hybrid for both tests, then base+hybrid for TestB", len(*runs))
	}
	sides := []string{}
	for _, r := range *runs {
		sides = append(sides, r.side+":"+strings.Join(r.names, "|"))
	}
	if got := strings.Join(sides, ","); got != "base:TestA|TestB,hybrid:TestA|TestB,base:TestB,hybrid:TestB" {
		t.Fatalf("runs %s", got)
	}
	// Both runs of a pair use one identical command, and every run keeps the
	// sandbox profile of dockerArgs.
	wantCommand := []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestA|TestB)$"}
	for i, r := range *runs {
		command := wantCommand
		if i >= 2 {
			command = []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(TestB)$"}
		}
		if want := h.dockerArgs(r.name, r.dir, command); strings.Join(r.args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("run %d args\n%q\nwant\n%q", i, r.args, want)
		}
	}
	// The hybrid tree: baseline test files and testdata, candidate code.
	for _, r := range *runs {
		if r.side != "hybrid" {
			continue
		}
		want := map[string]string{
			"pkg/a_test.go":      btBaseTest,
			"pkg/testdata/g.txt": "base",
			"pkg/v.go":           candidate["pkg/v.go"],
			"other/o.go":         candidate["other/o.go"],
		}
		for p, content := range want {
			if r.files[p] != content {
				t.Fatalf("hybrid %s = %q, want %q", p, r.files[p], content)
			}
		}
		for _, p := range []string{"pkg/b_test.go", "pkg/testdata/extra.txt"} {
			if _, ok := r.files[p]; ok {
				t.Fatalf("hybrid tree still holds the candidate-only %s", p)
			}
		}
	}
	// Verdicts, evidence and provenance.
	checks := h.Checks()
	if len(checks) != 4 || checks[0].Kind != model.CheckBaseTestBase || checks[1].Kind != model.CheckBaseTestHybrid || checks[2].Kind != model.CheckBaseTestBase || checks[3].Kind != model.CheckBaseTestHybrid {
		t.Fatalf("checks %+v", checks)
	}
	if checks[1].Status != "FAIL" || checks[3].Status != "PASS" {
		t.Fatalf("hybrid statuses %s %s", checks[1].Status, checks[3].Status)
	}
	a, b := res.Tests[0], res.Tests[1]
	if a.Status != model.StatusFailsOnCandidate || a.Reason != "" || b.Status != model.StatusPassesOnCandidate || b.Reason != "" {
		t.Fatalf("tests %+v %+v", a, b)
	}
	ea, eb := btEvidence(t, h, a.EvidenceID), btEvidence(t, h, b.EvidenceID)
	if ea.ID != "evidence-1" || eb.ID != "evidence-2" {
		t.Fatalf("evidence IDs %s %s", ea.ID, eb.ID)
	}
	for _, c := range []struct {
		e              model.Evidence
		name, base, hy string
		status         string
	}{{ea, "TestA", checks[0].ID, checks[1].ID, model.StatusFailsOnCandidate}, {eb, "TestB", checks[2].ID, checks[3].ID, model.StatusPassesOnCandidate}} {
		if c.e.Kind != model.EvidenceBaseTestDifferential || c.e.Runner != RunnerGo || strings.Join(c.e.TestNames, ",") != c.name || c.e.Path != "pkg/a_test.go" || c.e.BaseCheckID != c.base || c.e.CheckID != c.hy || c.e.Status != c.status {
			t.Fatalf("evidence %+v", c.e)
		}
		if status, _ := ClassifyExistingTest(btCheck(t, h, c.e.BaseCheckID), btCheck(t, h, c.e.CheckID), c.name); status != c.status {
			t.Fatalf("recorded checks of %s classify as %s", c.name, status)
		}
		for _, word := range []string{"regression", "bug", "tested", "verified", "correct", "safe"} {
			if strings.Contains(strings.ToLower(c.e.Description), word) {
				t.Fatalf("description uses %q: %s", word, c.e.Description)
			}
		}
	}
	// The snapshots are untouched, the hybrid tree is gone, and its manifest
	// is retained with a matching hash.
	if btTreeDigest(t, h.base) != baseBefore || btTreeDigest(t, h.candidate) != candidateBefore {
		t.Fatal("a snapshot changed")
	}
	entries, err := os.ReadDir(h.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hybrid-") {
			t.Fatalf("hybrid tree %s survived", e.Name())
		}
	}
	artifact, ok := artifactByKind(h, model.ArtifactBaseTestHybridManifest)
	if !ok {
		t.Fatal("no hybrid manifest artifact")
	}
	data, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != artifact.SHA256 {
		t.Fatal("manifest hash mismatch")
	}
	var manifest baseTestManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, e := range manifest.Entries {
		actions[e.Action+" "+e.Path] = e.SHA256
	}
	for _, key := range []string{"removed_from_candidate pkg/a_test.go", "removed_from_candidate pkg/b_test.go", "removed_from_candidate pkg/testdata/g.txt", "removed_from_candidate pkg/testdata/extra.txt", "restored_from_baseline pkg/a_test.go", "restored_from_baseline pkg/testdata/g.txt"} {
		if len(actions[key]) != 64 {
			t.Errorf("manifest lacks %q: %+v", key, manifest.Entries)
		}
	}
	if strings.Join(manifest.Dirs, ",") != "pkg" || manifest.Truncated {
		t.Fatalf("manifest %+v", manifest)
	}
	// One audit event per unit, under the reserved stage prefix.
	var audited []model.AuditEvent
	for _, e := range h.Audit() {
		if e.Tool == auditRunBaseTests {
			audited = append(audited, e)
		}
	}
	if len(audited) != 1 || !strings.Contains(audited[0].Arguments, `"dir":"pkg"`) || !strings.Contains(audited[0].Arguments, checks[3].ID) {
		t.Fatalf("audit %+v", audited)
	}
}

func TestBaseTestsOutcomes(t *testing.T) {
	type response struct {
		output string
		result execution
	}
	pass := response{btEvents("TestA", "pass"), execution{}}
	fail := response{btEvents("TestA", "fail"), execution{ExitCode: 1}}
	cases := []struct {
		name         string
		base, hybrid response
		status       string
		reason       string // substring
		runs         int
		hybridStatus string
	}{
		{"fails_on_candidate", pass, fail, model.StatusFailsOnCandidate, "", 2, "FAIL"},
		{"passes_on_candidate", pass, pass, model.StatusPassesOnCandidate, "", 2, "PASS"},
		{"baseline_failed", fail, pass, model.StatusUnverified, "failed on the baseline tree", 1, ""},
		// No selected test passed on the baseline: no hybrid run starts.
		{"baseline_skipped_test", response{btEvents("TestA", "skip"), execution{}}, pass, model.StatusUnverified, "was skipped on the baseline tree (check-1), so it was not run on candidate code", 1, ""},
		{"baseline_test_absent", response{"", execution{}}, pass, model.StatusUnverified, "does not record exactly one run and one result of this test", 1, ""},
		{"baseline_build_failure", response{"# " + btPkg + "\nundefined: V\nFAIL\t" + btPkg + " [build failed]\n", execution{ExitCode: 1}}, pass, model.StatusUnverified, "did not build", 1, ""},
		{"compile_failure_on_candidate_is_fail_not_error", pass, response{"# " + btPkg + "\npkg/a_test.go:6:5: undefined: V\nFAIL\t" + btPkg + " [build failed]\n", execution{ExitCode: 1}}, model.StatusUnverified, reasonCandidateBuild, 2, "FAIL"},
		{"setup_failure_text_on_candidate", pass, response{"fork/exec /tmp/x: permission denied\n", execution{ExitCode: 1}}, model.StatusUnverified, reasonCandidateOutcome, 2, "FAIL"},
		{"candidate_timeout", pass, response{"", execution{ExitCode: -1, TimedOut: true, Err: context.DeadlineExceeded}}, model.StatusUnverified, reasonCandidateTimeout, 2, "TIMEOUT"},
		{"candidate_infrastructure_error", pass, response{btEvents("TestA", "pass"), execution{ExitCode: 125}}, model.StatusUnverified, reasonCandidateIncomplete, 2, "ERROR"},
		{"candidate_skip", pass, response{btEvents("TestA", "skip"), execution{}}, model.StatusUnverified, reasonCandidateOutcome, 2, "PASS"},
		{"candidate_truncated", pass, response{btEvents("TestA", "pass") + strings.Repeat("x", 70*1024), execution{}}, model.StatusUnverified, reasonCandidateTrunc, 2, "PASS"},
		{"forged_pass_after_real_fail", pass, response{btEvents("TestA", "fail") + "{\"Action\":\"pass\",\"Package\":\"" + btPkg + "\",\"Test\":\"TestA\"}\n", execution{}}, model.StatusUnverified, reasonCandidateOutcome, 2, "PASS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, candidate := btTrees()
			h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
			runs := btExec(t, h, func(r btRun, _ int) (string, execution) {
				if r.side == "hybrid" {
					return tc.hybrid.output, tc.hybrid.result
				}
				return tc.base.output, tc.base.result
			})
			res, err := h.RunBaseTests(context.Background(), btSelected()[:1])
			if err != nil {
				t.Fatal(err)
			}
			got := res.Tests[0]
			if got.Status != tc.status || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("test %+v, want %s (%q)", got, tc.status, tc.reason)
			}
			if len(*runs) != tc.runs {
				t.Fatalf("%d runs, want %d", len(*runs), tc.runs)
			}
			checks := h.Checks()
			if tc.hybridStatus == "" {
				// No candidate-side run: no evidence record either.
				if got.EvidenceID != "" || len(h.Evidence()) != 0 || len(checks) != 1 {
					t.Fatalf("a test without a hybrid run has evidence %q / checks %+v", got.EvidenceID, checks)
				}
				return
			}
			if checks[1].Status != tc.hybridStatus {
				t.Fatalf("hybrid check %s, want %s", checks[1].Status, tc.hybridStatus)
			}
			e := btEvidence(t, h, got.EvidenceID)
			if e.Status != got.Status || e.CheckID != checks[1].ID || e.BaseCheckID != checks[0].ID {
				t.Fatalf("evidence %+v", e)
			}
		})
	}
}

// A test that passed inside a failed hybrid run and gets no result from a pair
// of its own keeps the first pair's evidence, with a reason saying so.
func TestBaseTestsRetryWithoutResult(t *testing.T) {
	failedInsideFirst := func(r btRun, n int) (string, execution) {
		if r.side == "hybrid" && len(r.names) == 2 {
			return btEvents("TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		if r.side == "base" && len(r.names) == 1 {
			return btEvents("TestB", "fail"), execution{ExitCode: 1}
		}
		return btAll("pass", "pass")(r, n)
	}
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, failedInsideFirst)
	res, err := h.RunBaseTests(context.Background(), btSelected())
	if err != nil {
		t.Fatal(err)
	}
	b := res.Tests[1]
	if len(*runs) != 3 || res.Tests[0].Status != model.StatusFailsOnCandidate || b.Status != model.StatusUnverified || b.EvidenceID == "" ||
		!strings.HasPrefix(b.Reason, baseTestPassedInsideFailure+"; its own run pair gave no result (the test failed on the baseline tree (check-3)") {
		t.Fatalf("%d runs; TestB %+v", len(*runs), b)
	}
	if e := btEvidence(t, h, b.EvidenceID); e.CheckID != "check-2" || e.Status != model.StatusUnverified {
		t.Fatalf("TestB evidence %+v", e)
	}
	// The review ends while the first pair runs: no retry starts.
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs = btExec(t, h, func(r btRun, n int) (string, execution) {
		if r.side == "hybrid" {
			cancel()
		}
		return failedInsideFirst(r, n)
	})
	res, _ = h.RunBaseTests(ctx, btSelected())
	if b := res.Tests[1]; len(*runs) != 2 || b.Status != model.StatusUnverified || b.Reason != baseTestPassedInsideFailure+"; its own run pair gave no result (the review was cancelled)" {
		t.Fatalf("%d runs; TestB %+v", len(*runs), b)
	}
}

// A baseline run that fails as a whole still runs the tests it records as
// passed, on their own.
func TestBaseTestsBaselineFailureNarrowsToPassedTests(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, func(r btRun, n int) (string, execution) {
		if r.side == "base" && len(r.names) == 2 {
			return btEvents("TestA", "fail", "TestB", "pass"), execution{ExitCode: 1}
		}
		return btAll("pass", "fail")(r, n)
	})
	res, err := h.RunBaseTests(context.Background(), btSelected())
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 3 || (*runs)[1].side != "base" || strings.Join((*runs)[1].names, "|") != "TestB" || (*runs)[2].side != "hybrid" {
		t.Fatalf("runs %+v", *runs)
	}
	a, b := res.Tests[0], res.Tests[1]
	if a.Status != model.StatusUnverified || a.EvidenceID != "" || !strings.Contains(a.Reason, "failed on the baseline tree (check-1)") {
		t.Fatalf("TestA %+v", a)
	}
	if b.Status != model.StatusFailsOnCandidate {
		t.Fatalf("TestB %+v", b)
	}
	if e := btEvidence(t, h, b.EvidenceID); e.BaseCheckID != "check-2" || e.CheckID != "check-3" {
		t.Fatalf("TestB evidence %+v", e)
	}
}

func TestBaseTestsUnverifiableTemplateRunsNothing(t *testing.T) {
	for _, template := range [][]string{{"go", "test", "./..."}, {"npm", "test"}, {"go", "test", "-exec=x", "{package}"}, nil} {
		base, candidate := btTrees()
		h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
		if template == nil {
			delete(h.opts.Commands, "generated_test")
		} else {
			h.opts.Commands["generated_test"] = template
		}
		runs := btExec(t, h, btAll("pass", "pass"))
		res, err := h.RunBaseTests(context.Background(), btSelected())
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != model.BaseTestsNotRun || !strings.Contains(res.Reason, "generated_test") || len(*runs) != 0 || len(h.Checks()) != 0 {
			t.Fatalf("%v: section %+v, %d runs", template, res, len(*runs))
		}
		for _, bt := range res.Tests {
			if bt.Status != model.StatusUnverified || !strings.HasPrefix(bt.Reason, "not run: ") || bt.EvidenceID != "" {
				t.Fatalf("%v: test %+v", template, bt)
			}
		}
		if tools := auditTools(h); len(tools) != 1 || tools[0] != auditRunBaseTests {
			t.Fatalf("audit %v", tools)
		}
	}
	// A closed harness runs nothing either.
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	h.Close()
	if res, _ := h.RunBaseTests(context.Background(), btSelected()); res.Status != model.BaseTestsNotRun || len(*runs) != 0 {
		t.Fatalf("closed harness: %+v", res)
	}
	// Nothing selected is no_candidates, without any audit event.
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	if res, _ := h.RunBaseTests(context.Background(), nil); res.Status != model.BaseTestsNoCandidates || res.Tests == nil || len(h.Audit()) != 0 {
		t.Fatalf("empty selection: %+v", res)
	}
}

func TestBaseTestsBudget(t *testing.T) {
	base, candidate := btTrees()
	// The whole budget is spent: runs are recorded as SKIPPED and nothing executes.
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	h.spent = h.opts.MaxRuntime
	res, err := h.RunBaseTests(context.Background(), btSelected())
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 0 || res.Status != model.BaseTestsNotRun || res.Reason != "no run started: "+budgetExhaustedText {
		t.Fatalf("section %+v after %d runs", res, len(*runs))
	}
	for _, bt := range res.Tests {
		if bt.Status != model.StatusUnverified || !strings.Contains(bt.Reason, "Sandbox runtime budget exhausted") {
			t.Fatalf("test %+v", bt)
		}
	}
	if c := h.Checks(); len(c) != 1 || c[0].Status != "SKIPPED" {
		t.Fatalf("checks %+v", c)
	}
	// Half of the budget is reserved for the reviewer and the other half is
	// spent: the stage's ceiling stops it with the reserve text.
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = btExec(t, h, btAll("pass", "pass"))
	h.opts.ReviewerReserve = h.opts.MaxRuntime / 2
	h.spent = h.opts.MaxRuntime / 2
	res, _ = h.RunBaseTests(context.Background(), btSelected())
	if len(*runs) != 0 || res.Reason != "no run started: "+budgetReservedText {
		t.Fatalf("reserve: section %+v after %d runs", res, len(*runs))
	}
	// A reserve that leaves nothing stops the stage before any run.
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = btExec(t, h, btAll("pass", "pass"))
	h.opts.ReviewerReserve = h.opts.MaxRuntime
	res, _ = h.RunBaseTests(context.Background(), btSelected())
	if len(*runs) != 0 || len(h.Checks()) != 0 || res.Status != model.BaseTestsNotRun || !strings.Contains(res.Tests[0].Reason, budgetReservedText) {
		t.Fatalf("full reserve: section %+v after %d runs", res, len(*runs))
	}
}

// The sub-cap: each run's timeout is at most what remains of 180 s, and no run
// starts once it is used up.
func TestBaseTestsSubCap(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	var remaining time.Duration
	h.execute = func(ctx context.Context, _ string, _ []string, out io.Writer) execution {
		remaining = deadlineRemaining(t, ctx)
		return execution{}
	}
	run := &baseTestRun{h: h, ctx: context.Background(), spent: baseTestSubCap - 2*time.Second}
	if reason := run.stopReason(); reason != "" {
		t.Fatalf("stopped early: %s", reason)
	}
	h.mu.Lock()
	run.exec(model.CheckBaseTestBase, h.base, []string{"go", "test", "./pkg"}, false)
	h.mu.Unlock()
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("run timeout %s, want at most the 2 s left of the sub-cap", remaining)
	}
	run.spent = baseTestSubCap
	if reason := run.stopReason(); !strings.Contains(reason, "180 s time limit") {
		t.Fatalf("stop reason %q", reason)
	}
}

func TestBaseTestsDeadlineStopsBeforeRunning(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), ErrOverallDeadline)
	defer cancel()
	res, err := h.RunBaseTests(ctx, btSelected())
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 0 || res.Status != model.BaseTestsNotRun || !strings.Contains(res.Tests[0].Reason, deadlineText) {
		t.Fatalf("section %+v after %d runs", res, len(*runs))
	}
}

func TestBaseTestsPrechecksRunNothing(t *testing.T) {
	base, candidate := btTrees()
	base["gone/g.go"] = "package gone\n"
	base["gone/g_test.go"] = "package gone\n"
	base["credentials/c_test.go"] = "package credentials\n"
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	selected := []model.BaseTest{
		{Name: "TestGone", Path: "gone/g_test.go", Line: 1, EndLine: 1, Change: model.BaseTestFileDeleted},
		{Name: "TestSecret", Path: "credentials/c_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
		{Name: "TestAbsent", Path: "pkg/absent_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
		{Name: "Helper", Path: "pkg/a_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
		{Name: "TestX", Path: "pkg/v.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
	}
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 0 || res.Status != model.BaseTestsNotRun || res.Reason != baseTestNothingRan {
		t.Fatalf("section %+v after %d runs", res, len(*runs))
	}
	want := []string{"package was removed or moved", "excluded from the sandbox snapshots", "not in the baseline snapshot", "not a Go test function", "not a Go test function"}
	for i, bt := range res.Tests {
		if bt.Status != model.StatusUnverified || !strings.Contains(bt.Reason, want[i]) {
			t.Errorf("%s: %+v, want reason %q", bt.Name, bt, want[i])
		}
	}
}

func TestBaseTestsUnitsChunkAndLimit(t *testing.T) {
	base, candidate := map[string]string{"go.mod": "module example.test/m\n"}, map[string]string{"go.mod": "module example.test/m\n"}
	var selected []model.BaseTest
	for i := 1; i <= baseTestMaxUnits+1; i++ {
		dir := fmt.Sprintf("p%02d", i)
		for _, tree := range []map[string]string{base, candidate} {
			tree[dir+"/c.go"] = "package p\n"
			tree[dir+"/c_test.go"] = "package p\n"
		}
		selected = append(selected, model.BaseTest{Name: "TestOne", Path: dir + "/c_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified})
	}
	// One package with 120 tests needs three runs of at most 50 names.
	for _, tree := range []map[string]string{base, candidate} {
		tree["big/c.go"] = "package big\n"
		tree["big/c_test.go"] = "package big\n"
	}
	for i := 0; i < 120; i++ {
		selected = append(selected, model.BaseTest{Name: fmt.Sprintf("Test%03d", i), Path: "big/c_test.go", Line: i + 1, EndLine: i + 1, Change: model.BaseTestRemoved})
	}
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []string
	for _, r := range *runs {
		if r.side == "base" {
			sizes = append(sizes, fmt.Sprint(len(r.names)))
		}
	}
	// Units sort by directory: big (3 chunks), then p01..p07; p08..p11 exceed
	// the limit of 10 units.
	if got := strings.Join(sizes, ","); got != "50,50,20,1,1,1,1,1,1,1" {
		t.Fatalf("unit sizes %s", got)
	}
	limited := 0
	for _, bt := range res.Tests {
		switch {
		case bt.Status == model.StatusPassesOnCandidate:
		case strings.Contains(bt.Reason, "limit of 10 runs"):
			limited++
		default:
			t.Fatalf("test %+v", bt)
		}
	}
	if limited != 4 {
		t.Fatalf("%d tests beyond the unit limit, want 4", limited)
	}
}

func TestBaseTestsFileTemplateGroupsByFile(t *testing.T) {
	base, candidate := btTrees()
	base["pkg/c_test.go"] = "package pkg\n"
	h := btHarness(t, base, candidate, []string{"go", "test", "-v", "{file}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	selected := append(btSelected(), model.BaseTest{Name: "TestC", Path: "pkg/c_test.go", Line: 1, EndLine: 1, Change: model.BaseTestFileDeleted})
	if _, err := h.RunBaseTests(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, r := range *runs {
		if r.side == "base" {
			targets = append(targets, strings.Join(r.names, "|"))
			if !contains(r.args, "pkg/a_test.go") && !contains(r.args, "pkg/c_test.go") {
				t.Fatalf("file template did not target a file: %q", r.args)
			}
		}
	}
	if strings.Join(targets, ",") != "TestA|TestB,TestC" {
		t.Fatalf("units %v", targets)
	}
}

// A candidate tree whose layout blocks a baseline test path (a directory named
// like a baseline test file, a file where a test-only package directory or a
// parent directory was) is reverted like any other: the blocking entries are
// removed and recorded, and the runs happen. Candidate content never makes the
// stage operational.
func TestBaseTestsCandidateLayoutConflicts(t *testing.T) {
	base, candidate := btTrees()
	delete(candidate, "pkg/a_test.go")
	candidate["pkg/a_test.go/x"] = "a directory where the baseline test file must be restored"
	base["itest/x_test.go"] = "package itest\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"
	candidate["itest"] = "a file where the test-only package directory was"
	base["deep/er/y_test.go"] = "package er\n\nimport \"testing\"\n\nfunc TestY(t *testing.T) {}\n"
	candidate["deep"] = "a file where a parent directory was"
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "fail"))
	selected := append(btSelected(),
		model.BaseTest{Name: "TestX", Path: "itest/x_test.go", Line: 5, EndLine: 5, Change: model.BaseTestFileDeleted},
		model.BaseTest{Name: "TestY", Path: "deep/er/y_test.go", Line: 5, EndLine: 5, Change: model.BaseTestFileDeleted},
	)
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatalf("a candidate layout made the stage operational: %v", err)
	}
	if res.Status != model.BaseTestsRan || len(*runs) != 6 {
		t.Fatalf("section %+v after %d runs", res, len(*runs))
	}
	for _, bt := range res.Tests {
		if bt.Status != model.StatusFailsOnCandidate {
			t.Fatalf("test %+v", bt)
		}
	}
	for _, r := range *runs {
		if r.side != "hybrid" {
			continue
		}
		for p, want := range map[string]string{"pkg/a_test.go": btBaseTest, "itest/x_test.go": base["itest/x_test.go"], "deep/er/y_test.go": base["deep/er/y_test.go"]} {
			if r.files[p] != want {
				t.Fatalf("hybrid %s = %q", p, r.files[p])
			}
		}
		for _, p := range []string{"pkg/a_test.go/x", "itest", "deep"} {
			if _, ok := r.files[p]; ok {
				t.Fatalf("hybrid tree still holds the candidate entry %s", p)
			}
		}
	}
	artifact, ok := artifactByKind(h, model.ArtifactBaseTestHybridManifest)
	if !ok {
		t.Fatal("no hybrid manifest artifact")
	}
	data, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest baseTestManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range manifest.Entries {
		actions[e.Action+" "+e.Path] = len(e.SHA256) == 64
	}
	for _, key := range []string{"removed_from_candidate pkg/a_test.go/x", "removed_from_candidate itest", "removed_from_candidate deep", "restored_from_baseline itest/x_test.go", "restored_from_baseline deep/er/y_test.go", "restored_from_baseline pkg/a_test.go"} {
		if !actions[key] {
			t.Errorf("manifest lacks %q: %+v", key, manifest.Entries)
		}
	}
	if len(manifest.FailedDirs) != 0 {
		t.Fatalf("failed dirs %v", manifest.FailedDirs)
	}
}

// btOversizedBaselineTestdata makes the revert of pkg fail: a baseline
// testdata file beyond the copy limit (sparse where the file system allows).
func btOversizedBaselineTestdata(t *testing.T, h *Harness) {
	t.Helper()
	f, err := os.Create(filepath.Join(h.base, "pkg", "testdata", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(baseTestFileLimit + 1); err != nil {
		t.Fatal(err)
	}
}

// A package directory whose revert fails leaves only its own tests
// UNVERIFIED; the other units run, and the audit record names no host path.
func TestBaseTestsRevertFailureAffectsOnlyItsUnit(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	btOversizedBaselineTestdata(t, h)
	runs := btExec(t, h, btAll("pass", "pass"))
	selected := append(btSelected(), model.BaseTest{Name: "TestO", Path: "other/o_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified})
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatalf("a directory revert failure is not operational: %v", err)
	}
	if res.Status != model.BaseTestsRan || len(*runs) != 2 || (*runs)[0].dir != h.base || strings.Join((*runs)[0].names, "|") != "TestO" {
		t.Fatalf("section %+v after runs %+v", res, *runs)
	}
	for _, bt := range res.Tests[:2] {
		if bt.Status != model.StatusUnverified || bt.Reason != baseTestDirFailed || bt.EvidenceID != "" {
			t.Fatalf("test %+v", bt)
		}
	}
	if o := res.Tests[2]; o.Status != model.StatusPassesOnCandidate {
		t.Fatalf("TestO %+v", o)
	}
	var failed []model.AuditEvent
	for _, e := range h.Audit() {
		if e.Tool == auditRunBaseTests && e.Status == "ERROR" {
			failed = append(failed, e)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Arguments, `"dir":"pkg"`) || !strings.Contains(failed[0].Arguments, "snapshot exceeds safe copy limits") {
		t.Fatalf("audit %+v", failed)
	}
	for _, host := range []string{h.root, filepath.ToSlash(h.root), strings.ReplaceAll(h.root, `\`, `\\`)} {
		if strings.Contains(failed[0].Arguments, host) {
			t.Fatalf("the audit record names the host path %s: %s", host, failed[0].Arguments)
		}
	}
	// A path error names the placeholders, never the host directories.
	hybrid := filepath.Join(h.root, "hybrid-1")
	pathErr := &fs.PathError{Op: "open", Path: filepath.Join(hybrid, "pkg", "a_test.go"), Err: fs.ErrExist}
	for _, e := range []error{pathErr, fmt.Errorf("walk %s: %w", filepath.ToSlash(filepath.Join(h.base, "pkg")), pathErr)} {
		text := baseTestErrorText(e, [2]string{hybrid, "(hybrid tree)"}, [2]string{h.base, "(baseline snapshot)"}, [2]string{h.root, "(harness directory)"})
		if strings.Contains(text, h.root) || strings.Contains(text, filepath.ToSlash(h.root)) || !strings.Contains(text, "open (hybrid tree)") {
			t.Fatalf("error text %q", text)
		}
	}
	artifact, _ := artifactByKind(h, model.ArtifactBaseTestHybridManifest)
	data, _ := os.ReadFile(artifact.Path)
	var manifest baseTestManifest
	if err := json.Unmarshal(data, &manifest); err != nil || strings.Join(manifest.FailedDirs, ",") != "pkg" {
		t.Fatalf("manifest %s: %v", data, err)
	}
	// When every unit fails, nothing runs and the section is not_run.
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	btOversizedBaselineTestdata(t, h)
	runs = btExec(t, h, btAll("pass", "pass"))
	res, err = h.RunBaseTests(context.Background(), btSelected())
	if err != nil || len(*runs) != 0 || res.Status != model.BaseTestsNotRun || res.Reason != baseTestNothingRan {
		t.Fatalf("err %v, section %+v after %d runs", err, res, len(*runs))
	}
}

// A non-test Go file of the candidate at a component of a baseline test
// directory is candidate code of the parent package: it stays in the hybrid
// tree, and only the tests of that directory go without a run.
func TestBaseTestsCandidateGoFileAtTestDirStays(t *testing.T) {
	base, candidate := btTrees()
	base["gen.go/g_test.go"] = "package gen\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n"
	candidate["gen.go"] = "package m\n\nfunc Gen() {}\n"
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "fail"))
	selected := append(btSelected(), model.BaseTest{Name: "TestG", Path: "gen.go/g_test.go", Line: 5, EndLine: 5, Change: model.BaseTestFileDeleted})
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatalf("a candidate layout made the stage operational: %v", err)
	}
	if res.Status != model.BaseTestsRan || len(*runs) != 2 {
		t.Fatalf("section %+v after %d runs", res, len(*runs))
	}
	if g := res.Tests[2]; g.Status != model.StatusUnverified || g.Reason != baseTestDirFailed || g.EvidenceID != "" {
		t.Fatalf("TestG %+v", g)
	}
	for _, bt := range res.Tests[:2] {
		if bt.Status != model.StatusFailsOnCandidate {
			t.Fatalf("test %+v", bt)
		}
	}
	for _, r := range *runs {
		if r.side == "hybrid" && r.files["gen.go"] != candidate["gen.go"] {
			t.Fatalf("the hybrid tree lost the candidate Go file: %q", r.files["gen.go"])
		}
	}
	found := false
	for _, e := range h.Audit() {
		if e.Tool == auditRunBaseTests && e.Status == "ERROR" && strings.Contains(e.Arguments, "the candidate Go file gen.go occupies a baseline test directory") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit %+v", h.Audit())
	}
}

// Only host-side failures are operational: the private candidate copy, and
// the retention of the manifest. Neither records the host error in the audit.
func TestBaseTestsHostFailureIsOperational(t *testing.T) {
	base, candidate := btTrees()
	base["itest/x_test.go"] = "package itest\n"
	selected := []model.BaseTest{{Name: "TestX", Path: "itest/x_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified}}
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, btAll("pass", "pass"))
	if err := os.RemoveAll(h.candidate); err != nil {
		t.Fatal(err)
	}
	res, err := h.RunBaseTests(context.Background(), selected)
	if err == nil || len(*runs) != 0 || res.Status != model.BaseTestsNotRun || res.Reason != baseTestCopyFailed || res.Tests[0].Reason != "not run: "+baseTestCopyFailed {
		t.Fatalf("private copy failure: err %v, section %+v", err, res)
	}
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = btExec(t, h, btAll("pass", "pass"))
	if err := os.WriteFile(filepath.Join(h.opts.ArtifactDir, h.runID+"-base-tests-hybrid-1.json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	res, err = h.RunBaseTests(context.Background(), btSelected())
	if err == nil || len(*runs) != 0 || res.Status != model.BaseTestsNotRun || res.Reason != baseTestManifestLost {
		t.Fatalf("manifest retention failure: err %v, %+v", err, res)
	}
	for _, bt := range res.Tests {
		if bt.Status != model.StatusUnverified || bt.EvidenceID != "" {
			t.Fatalf("test %+v", bt)
		}
	}
	for _, e := range h.Audit() {
		if e.Tool == auditRunBaseTests && (strings.Contains(e.Arguments, `"error"`) || !strings.Contains(e.Arguments, baseTestManifestLost)) {
			t.Fatalf("audit %+v", e)
		}
	}
}

// A test that passed inside a failed hybrid run gets no pair of its own when
// that pair would only repeat the failed run, and its reason says so.
func TestBaseTestsNoRetryReasons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected []model.BaseTest
		why      string
	}{
		{"single_test", btSelected()[:1], "the failed run held only this test"},
		{"every_test_passed", btSelected(), "every test of the failed run passed in it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, candidate := btTrees()
			h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
			runs := btExec(t, h, func(r btRun, n int) (string, execution) {
				output, result := btAll("pass", "pass")(r, n)
				if r.side == "hybrid" {
					// Every test passes, and the run fails as a whole (for
					// example through the exit code of TestMain).
					result.ExitCode = 1
				}
				return output, result
			})
			res, err := h.RunBaseTests(context.Background(), tc.selected)
			if err != nil {
				t.Fatal(err)
			}
			if len(*runs) != 2 {
				t.Fatalf("%d runs, want one pair and no retry", len(*runs))
			}
			for _, bt := range res.Tests {
				if bt.Status != model.StatusUnverified || bt.EvidenceID == "" || bt.Reason != baseTestPassedInsideFailure+"; no run pair of its own was attempted because "+tc.why {
					t.Fatalf("test %+v", bt)
				}
			}
		})
	}
}

// A mixed unit keeps one command for every name; a unit in which no name
// passed on the baseline starts no hybrid run.
func TestBaseTestsHybridOnlyWhenABaselineTestPassed(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs := btExec(t, h, func(r btRun, n int) (string, execution) {
		if r.side == "base" {
			return btEvents("TestA", "pass", "TestB", "skip"), execution{}
		}
		return btEvents("TestA", "fail", "TestB", "skip"), execution{ExitCode: 1}
	})
	res, err := h.RunBaseTests(context.Background(), btSelected())
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 2 || strings.Join((*runs)[1].names, "|") != "TestA|TestB" {
		t.Fatalf("runs %+v", *runs)
	}
	if a, b := res.Tests[0], res.Tests[1]; a.Status != model.StatusFailsOnCandidate || b.Status != model.StatusUnverified || b.EvidenceID == "" || b.Reason != reasonBaselineOutcome {
		t.Fatalf("tests %+v %+v", a, b)
	}
	h = btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	runs = btExec(t, h, btAll("skip", "pass"))
	res, _ = h.RunBaseTests(context.Background(), btSelected())
	if len(*runs) != 1 || res.Status != model.BaseTestsRan || len(h.Evidence()) != 0 {
		t.Fatalf("all skipped: section %+v after %d runs", res, len(*runs))
	}
	for _, bt := range res.Tests {
		if bt.Status != model.StatusUnverified || bt.Reason != "the test was skipped on the baseline tree (check-1), so it was not run on candidate code" {
			t.Fatalf("test %+v", bt)
		}
	}
}

// Every fixed text of the stage reaches the Markdown report through inline(),
// which turns &, <, >, ' and " into HTML entities. None of them holds one.
func TestBaseTestFixedTextsNeedNoEntities(t *testing.T) {
	texts := []string{
		reasonFailsOnCandidate, reasonPassesOnCandidate, reasonCommandMismatch, reasonBaselineNotPassed, reasonBaselineOutcome,
		reasonCandidateTimeout, reasonCandidateIncomplete, reasonCandidateTrunc, reasonCandidateExit, reasonCandidateBuild, reasonCandidateOutcome,
		baseTestNothingRan, baseTestPassedInsideFailure, baseTestCopyFailed, baseTestManifestLost, baseTestDirFailed, baseTestTemplateReason,
		model.BaseTestsNote, budgetExhaustedText, budgetReservedText, deadlineText,
	}
	for _, change := range []string{model.BaseTestRemoved, model.BaseTestModified, model.BaseTestSharedCodeChanged, model.BaseTestFileDeleted, "other"} {
		texts = append(texts, BaseTestChangeText(change), baseTestDescription(model.BaseTest{Name: "TestA", Path: "pkg/a_test.go", Change: change}, baseTestUnit{dir: "pkg"}))
	}
	for _, c := range []model.Check{
		{ID: "check-1", Status: "SKIPPED", Output: budgetExhaustedText}, {ID: "check-1", Status: "TIMEOUT"}, {ID: "check-1", Status: "ERROR"},
		{ID: "check-1", Status: "PASS", Truncated: true}, {ID: "check-1", Status: "FAIL", ExitCode: 1, Output: btEvents("TestA", "fail")},
		{ID: "check-1", Status: "PASS", Output: btEvents("TestA", "skip")}, {ID: "check-1", Status: "FAIL", ExitCode: 1, Output: btEvents("TestA", "pass")},
		{ID: "check-1", Status: "FAIL", ExitCode: 1, Output: "FAIL x [build failed]"}, {ID: "check-1", Status: "PASS"},
	} {
		texts = append(texts, (&baseTestRun{}).baselineReason(c, "", "TestA"))
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), ErrOverallDeadline)
	defer stop()
	for _, run := range []*baseTestRun{{ctx: cancelled}, {ctx: expired}, {ctx: context.Background(), ceiling: -1}, {ctx: context.Background(), spent: baseTestSubCap}} {
		texts = append(texts, run.stopReason())
	}
	// Reasons that only a run produces: the prechecks.
	base, candidate := btTrees()
	base["gone/g.go"] = "package gone\n"
	base["gone/g_test.go"] = "package gone\n"
	base["credentials/c_test.go"] = "package credentials\n"
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	btExec(t, h, btAll("pass", "pass"))
	res, _ := h.RunBaseTests(context.Background(), []model.BaseTest{
		{Name: "TestGone", Path: "gone/g_test.go", Line: 1, EndLine: 1, Change: model.BaseTestFileDeleted},
		{Name: "TestSecret", Path: "credentials/c_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
		{Name: "TestAbsent", Path: "pkg/absent_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
		{Name: "Helper", Path: "pkg/a_test.go", Line: 1, EndLine: 1, Change: model.BaseTestModified},
	})
	texts = append(texts, res.Reason)
	for _, bt := range res.Tests {
		texts = append(texts, bt.Reason)
	}
	// Reasons composed inline in basetests.go, checked against the source so
	// that this list cannot fall behind it.
	src, err := os.ReadFile("basetests.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"not run: the limit of %d runs per review of this stage was reached",
		"no run pair of its own was attempted because the failed run held only this test",
		"no run pair of its own was attempted because every test of the failed run passed in it",
		"; its own run pair gave no result (",
		"the baseline run was replayed from the execution cache and could not be repeated live (",
		"not run: the stage stopped before this test ran",
		"no reason was recorded",
		"no run started: ",
	} {
		if !strings.Contains(string(src), `"`+text+`"`) {
			t.Errorf("basetests.go no longer holds %q; update this test", text)
		}
		texts = append(texts, text)
	}
	for _, text := range texts {
		if text == "" {
			t.Error("an empty fixed text")
		}
		if strings.ContainsAny(text, `&<>'"`) {
			t.Errorf("fixed text holds a character inline() escapes as an entity: %q", text)
		}
	}
}

// §1.11: FAILS_ON_CANDIDATE never rests on a replayed baseline; the baseline
// runs again live with the same kind and command.
func TestBaseTestsReplayedBaselineIsConfirmedLive(t *testing.T) {
	base, candidate := btTrees()
	h := btHarness(t, base, candidate, []string{"go", "test", "{package}"})
	cache := useMemoryCache(h)
	hybridAction, liveBase := "pass", "pass"
	runs := btExec(t, h, func(r btRun, n int) (string, execution) {
		if r.side == "base" && n > 0 {
			return btAll(liveBase, "")(r, n)
		}
		return btAll("pass", hybridAction)(r, n)
	})
	selected := btSelected()[:1]
	// First review: a live baseline, stored once, then promoted to two agreeing runs.
	if res, err := h.RunBaseTests(context.Background(), selected); err != nil || res.Tests[0].Status != model.StatusPassesOnCandidate {
		t.Fatalf("first run: %+v %v", res, err)
	}
	if keys := cache.keys(); len(keys) != 1 {
		t.Fatalf("cache keys %v", keys)
	}
	cache.promote(2)
	// PASSES may rest on a replay of two agreeing live runs.
	before := len(*runs)
	res, _ := h.RunBaseTests(context.Background(), selected)
	e := btEvidence(t, h, res.Tests[0].EvidenceID)
	if res.Tests[0].Status != model.StatusPassesOnCandidate || !btCheck(t, h, e.BaseCheckID).Replayed() || len(*runs)-before != 1 {
		t.Fatalf("replayed PASSES: %+v, evidence %+v, %d executions", res.Tests[0], e, len(*runs)-before)
	}
	// FAILS on a replayed baseline triggers one live baseline run.
	hybridAction = "fail"
	before = len(*runs)
	res, _ = h.RunBaseTests(context.Background(), selected)
	e = btEvidence(t, h, res.Tests[0].EvidenceID)
	live := btCheck(t, h, e.BaseCheckID)
	if res.Tests[0].Status != model.StatusFailsOnCandidate || live.Replayed() || live.Kind != model.CheckBaseTestBase || len(*runs)-before != 2 || (*runs)[len(*runs)-1].side != "base" {
		t.Fatalf("confirmed FAILS: %+v, base %+v, %d executions", res.Tests[0], live, len(*runs)-before)
	}
	replayed := 0
	for _, c := range h.Checks() {
		if c.Replayed() {
			replayed++
		}
	}
	if replayed != 2 {
		t.Fatalf("%d replayed checks kept in the ledger, want 2", replayed)
	}
	// A live baseline that no longer passes leaves the test UNVERIFIED.
	cache.promote(2)
	liveBase = "fail"
	res, _ = h.RunBaseTests(context.Background(), selected)
	if res.Tests[0].Status != model.StatusUnverified || !strings.Contains(res.Tests[0].Reason, "baseline run did not pass") {
		t.Fatalf("contradicted confirmation: %+v", res.Tests[0])
	}
}

// Real sandboxes: the baseline run and the hybrid-tree run of a changed test
// file, the retry pair for a test that passed inside a failed hybrid run, and
// no surviving container.
func TestDockerBaseTestsRealGo(t *testing.T) {
	image := os.Getenv("PROBE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_DOCKER_IMAGE to a preloaded golang Linux image")
	}
	h, names := dockerRunFixture(t, image, brokenTotal)
	h.opts.Commands["generated_test"] = []string{"go", "test", "{package}"}
	selected := []model.BaseTest{
		{Name: "TestTotal", Path: "cart/cart_test.go", Line: 5, EndLine: 9, Change: model.BaseTestModified},
		{Name: "TestCount", Path: "cart/cart_test.go", Line: 11, EndLine: 15, Change: model.BaseTestRemoved},
	}
	res, err := h.RunBaseTests(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.BaseTestsRan || res.Tests[0].Status != model.StatusFailsOnCandidate {
		t.Fatalf("section %+v\nchecks %+v", res, h.Checks())
	}
	var kinds []string
	for _, c := range h.Checks() {
		kinds = append(kinds, c.Kind+":"+c.Status)
		if c.Status == "ERROR" {
			t.Fatalf("check %s ERROR: %s", c.ID, c.Output)
		}
	}
	got := strings.Join(kinds, ",")
	if !strings.HasPrefix(got, "base_test_base:PASS,base_test_hybrid:FAIL") {
		t.Fatalf("checks %s", got)
	}
	// TestCount passed inside the failed hybrid run, so it gets a run pair of
	// its own. On a loaded host the stage's 180 s sub-cap can run out first;
	// the product limit stays, and the test then records why.
	switch count := res.Tests[1]; {
	case count.Status == model.StatusPassesOnCandidate && got == "base_test_base:PASS,base_test_hybrid:FAIL,base_test_base:PASS,base_test_hybrid:PASS":
	case count.Status == model.StatusUnverified && (strings.HasPrefix(count.Reason, baseTestPassedInsideFailure) || count.Reason == reasonCandidateTimeout):
		t.Logf("the retry pair gave no result within the sub-cap: %s", count.Reason)
	default:
		t.Fatalf("TestCount %+v; checks %s", count, got)
	}
	for _, bt := range res.Tests {
		e := btEvidence(t, h, bt.EvidenceID)
		if status, reason := ClassifyExistingTest(btCheck(t, h, e.BaseCheckID), btCheck(t, h, e.CheckID), bt.Name); status != bt.Status {
			t.Fatalf("%s: recorded checks classify as %s (%s)", bt.Name, status, reason)
		}
	}
	assertNoContainers(t, *names)
}
