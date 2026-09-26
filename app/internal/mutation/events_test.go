package mutation_test

// Classification tests against real go test -json logs recorded from
// golang:1.26-bookworm (go1.26.8) and golang:1.23-bookworm (go1.23.12) on
// 2026-09-26 (testdata/gotest-*.jsonl). The external test package can use
// harness.GoTestOutcome, which production code injects.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/mutation"
)

var command = []string{"go", "test", "-json", "-count=1", "-failfast", "./p"}

func fixtureLog(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gotest-"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// check builds a recorded check with the exit code go test returned for the log.
func check(id, kind, status string, exit int, output string) model.Check {
	return model.Check{ID: id, Kind: kind, Status: status, ExitCode: exit, Command: append([]string(nil), command...), Output: output}
}

func control(t *testing.T, name, status string, exit int) model.Check {
	return check("mutation-check-1", model.CheckMutationControl, status, exit, fixtureLog(t, name))
}

func mutant(t *testing.T, name, status string, exit int) model.Check {
	return check("mutation-check-2", model.CheckMutant, status, exit, fixtureLog(t, name))
}

func TestControlValidity(t *testing.T) {
	pkg, why := mutation.Control(control(t, "ok", "PASS", 0), harness.GoTestOutcome)
	if why != "" || pkg != "example.test/probe/ok" {
		t.Fatalf("valid control rejected: %q %q", pkg, why)
	}
	truncated := control(t, "ok", "PASS", 0)
	truncated.Truncated = true
	wrongKind := control(t, "ok", "PASS", 0)
	wrongKind.Kind = model.CheckMutant
	noCommand := control(t, "ok", "PASS", 0)
	noCommand.Command = nil
	for name, c := range map[string]model.Check{
		"fail":              control(t, "fail", "FAIL", 1),
		"build":             control(t, "build", "FAIL", 1),
		"vet":               control(t, "vet", "FAIL", 1),
		"build go1.23 text": control(t, "build-go123", "FAIL", 1),
		"no test files":     control(t, "notests", "PASS", 0),
		"TestMain exit 0":   control(t, "exitzero", "PASS", 0),
		"all skipped":       control(t, "skip", "PASS", 0),
		"truncated":         truncated,
		"wrong kind":        wrongKind,
		"no command":        noCommand,
		"pass with exit 1":  control(t, "ok", "PASS", 1),
		"timeout":           control(t, "ok", "TIMEOUT", -1),
	} {
		if pkg, why := mutation.Control(c, harness.GoTestOutcome); why == "" {
			t.Errorf("%s: invalid control accepted (package %q)", name, pkg)
		}
	}
}

func TestClassifyRealLogs(t *testing.T) {
	ok := control(t, "ok", "PASS", 0)
	for _, tc := range []struct {
		name, log, status string
		exit              int
		want              string
		testsRun          int
		failed            []string
	}{
		{"survivor", "ok", "PASS", 0, model.MutantSurvived, 2, nil},
		{"killed", "fail", "FAIL", 1, model.MutantKilled, 0, []string{"TestAdd"}},
		{"killed by a failing subtest", "subtest", "FAIL", 1, model.MutantKilled, 0, []string{"TestParent"}},
		{"killed by a panic", "panic", "FAIL", 1, model.MutantKilled, 0, []string{"TestPanics"}},
		{"forged events are output", "forge", "FAIL", 1, model.MutantKilled, 0, []string{"TestForge"}},
		{"build failure", "build", "FAIL", 1, model.MutantInvalid, 0, nil},
		{"vet failure", "vet", "FAIL", 1, model.MutantInvalid, 0, nil},
		{"build failure go1.23 text", "build-go123", "FAIL", 1, model.MutantInvalid, 0, nil},
		{"vet failure go1.23 text", "vet-go123", "FAIL", 1, model.MutantInvalid, 0, nil},
		{"test binary timeout", "timeout", "FAIL", 1, model.MutantTimeout, 0, nil},
		{"container timeout", "ok", "TIMEOUT", -1, model.MutantTimeout, 0, nil},
		{"os.Exit(1) without a named failure", "exit1", "FAIL", 1, model.MutantInconclusive, 0, nil},
		{"TestMain exit 0", "exitzero", "PASS", 0, model.MutantInconclusive, 0, nil},
		{"all skipped", "skip", "PASS", 0, model.MutantInconclusive, 0, nil},
		{"infrastructure error", "ok", "ERROR", 125, model.MutantInconclusive, 0, nil},
		{"not executed", "ok", "SKIPPED", -1, model.MutantNotRun, 0, nil},
		{"fail outside the test-failure range", "fail", "FAIL", 125, model.MutantInconclusive, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mutant(t, tc.log, tc.status, tc.exit)
			// The fixtures come from several probe packages; the mutant must
			// report the control's package, so give each case a control of its
			// own package where the package differs.
			c := ok
			if pkgOf(m.Output) != "" && pkgOf(m.Output) != "example.test/probe/ok" {
				c = controlFor(t, pkgOf(m.Output))
			}
			v := mutation.Classify(c, m, harness.GoTestOutcome)
			if v.Status != tc.want || v.TestsRun != tc.testsRun || strings.Join(v.FailedTests, ",") != strings.Join(tc.failed, ",") {
				t.Fatalf("got %+v, want %s tests_run %d failed %v", v, tc.want, tc.testsRun, tc.failed)
			}
			if (tc.want == model.MutantKilled || tc.want == model.MutantSurvived) != (v.Reason == "") {
				t.Fatalf("reason %q for %s", v.Reason, v.Status)
			}
		})
	}
}

// pkgOf returns the package of the first test event of a log.
func pkgOf(output string) string {
	const key = `"Package":"`
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, `"Test":"`) {
			if i := strings.Index(line, key); i >= 0 {
				rest := line[i+len(key):]
				return rest[:strings.Index(rest, `"`)]
			}
		}
	}
	return ""
}

// controlFor is a passing control log of pkg with one test.
func controlFor(t *testing.T, pkg string) model.Check {
	log := strings.ReplaceAll(fixtureLog(t, "ok"), "example.test/probe/ok", pkg)
	return check("mutation-check-1", model.CheckMutationControl, "PASS", 0, log)
}

func TestClassifyRejectsInconsistentRecords(t *testing.T) {
	ok := control(t, "ok", "PASS", 0)
	survivor := mutant(t, "ok", "PASS", 0)
	if v := mutation.Classify(ok, survivor, harness.GoTestOutcome); v.Status != model.MutantSurvived {
		t.Fatalf("baseline case %+v", v)
	}
	otherCommand := survivor
	otherCommand.Command = append([]string{"go", "test", "-json", "./q"}, "")
	truncated := survivor
	truncated.Truncated = true
	otherPackage := mutant(t, "fail", "FAIL", 1)
	wrongKind := survivor
	wrongKind.Kind = model.CheckMutationControl
	badControl := control(t, "fail", "FAIL", 1)
	overlong := survivor
	overlong.Output = strings.Repeat("x", 2<<20) + "\n" + survivor.Output
	for name, pair := range map[string][2]model.Check{
		"different command": {ok, otherCommand},
		"truncated log":     {ok, truncated},
		"other package":     {ok, otherPackage},
		"wrong kind":        {ok, wrongKind},
		"invalid control":   {badControl, survivor},
		"overlong line":     {ok, overlong},
	} {
		if v := mutation.Classify(pair[0], pair[1], harness.GoTestOutcome); v.Status != model.MutantInconclusive || v.Reason == "" {
			t.Errorf("%s: %+v, want INCONCLUSIVE with a reason", name, v)
		}
	}
}

// At most five failing names are recorded.
func TestKilledRecordsAtMostFiveNames(t *testing.T) {
	var b strings.Builder
	for _, name := range []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"} {
		b.WriteString(`{"Action":"run","Package":"example.test/probe/ok","Test":"` + name + `"}` + "\n")
		b.WriteString(`{"Action":"fail","Package":"example.test/probe/ok","Test":"` + name + `"}` + "\n")
	}
	b.WriteString(`{"Action":"fail","Package":"example.test/probe/ok"}` + "\n")
	m := check("mutation-check-2", model.CheckMutant, "FAIL", 1, b.String())
	v := mutation.Classify(control(t, "ok", "PASS", 0), m, harness.GoTestOutcome)
	if v.Status != model.MutantKilled || len(v.FailedTests) != 5 || v.FailedTests[0] != "TestA" {
		t.Fatalf("%+v", v)
	}
}
