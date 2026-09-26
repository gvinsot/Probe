package mutation_test

// Classification tests against real go test -json logs recorded from
// golang:1.26-bookworm (go1.26.8) and golang:1.23-bookworm (go1.23.12) on
// 2026-09-26 (testdata/gotest-*.jsonl). The external test package can import
// harness, so the single-pass outcome of the mutation package is checked
// against harness.GoTestOutcome here.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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
	ctl := mutation.NewControl(control(t, "ok", "PASS", 0))
	pkg, why := ctl.Package(), ctl.Reason()
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
		if ctl := mutation.NewControl(c); ctl.Reason() == "" {
			t.Errorf("%s: invalid control accepted (package %q)", name, ctl.Package())
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
		// Unmarked prints stay inside output events; the framing-marker route
		// is in TestSandboxCodeCanForgeEvents.
		{"unmarked event text stays output", "forge", "FAIL", 1, model.MutantKilled, 0, []string{"TestForge"}},
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
			v := mutation.Classify(c, m)
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
	if v := mutation.Classify(ok, survivor); v.Status != model.MutantSurvived {
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
		if v := mutation.Classify(pair[0], pair[1]); v.Status != model.MutantInconclusive || v.Reason == "" {
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
	v := mutation.Classify(control(t, "ok", "PASS", 0), m)
	if v.Status != model.MutantKilled || len(v.FailedTests) != 5 || v.FailedTests[0] != "TestA" {
		t.Fatalf("%+v", v)
	}
}

// The single-pass outcome equals harness.GoTestOutcome for every name of every
// recorded fixture, with LF and CRLF line endings.
func TestOnePassMatchesGoTestOutcomeOnFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "gotest-*.jsonl"))
	if err != nil || len(paths) < 10 {
		t.Fatalf("fixtures %v (%v)", paths, err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		lf := strings.ReplaceAll(string(b), "\r\n", "\n")
		for _, output := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
			all, _ := mutation.LogTestNames(output)
			if len(all) == 0 && !strings.Contains(p, "notests") && !strings.Contains(p, "exitzero") && !strings.Contains(p, "build") && !strings.Contains(p, "vet") {
				t.Errorf("%s: no test name read", p)
			}
			compareOutcomes(t, p, output, append(all, "", "TestNope", "TestAdd/sub"))
		}
	}
}

func compareOutcomes(t *testing.T, label, output string, names []string) {
	t.Helper()
	for _, name := range names {
		a1, p1 := mutation.OnePassOutcome(output, name)
		a2, p2 := harness.GoTestOutcome(output, name)
		if a1 != a2 || p1 != p2 {
			t.Fatalf("%s: outcome of %q: one pass (%q, %q), GoTestOutcome (%q, %q)\nlog:\n%s", label, name, a1, p1, a2, p2, output)
		}
	}
}

// The single-pass outcome equals harness.GoTestOutcome on generated logs that
// mix every rule: repeated and missing run or terminal events, terminal events
// before the run, subtests, several packages, events without a package,
// non-JSON and malformed lines, keys in another case, fields of the wrong JSON
// type, and lines longer than the 1 MiB line limit.
func TestOnePassMatchesGoTestOutcomeOnGeneratedLogs(t *testing.T) {
	rng := rand.New(rand.NewSource(20260926))
	names := []string{"TestA", "TestB", "TestA/sub", "TestC", "TestA/sub/deep"}
	packages := []string{"p", "p", "p", "q", ""}
	actions := []string{"run", "run", "pass", "fail", "skip", "output", "pause", "cont", "start", "bench"}
	pick := func(list []string) string { return list[rng.Intn(len(list))] }
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		lines := 1 + rng.Intn(14)
		for j := 0; j < lines; j++ {
			name, pkg, action := pick(names), pick(packages), pick(actions)
			switch rng.Intn(14) {
			case 0:
				b.WriteString("FAIL\tp [build failed]")
			case 1:
				fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q,"Output":5}`, action, pkg, name)
			case 2:
				fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q,"FailedBuild":true}`, action, pkg, name)
			case 3:
				fmt.Fprintf(&b, `{"action":%q,"package":%q,"test":%q}`, action, pkg, name)
			case 4:
				fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":`, action, pkg)
			case 5:
				fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":7}`, action, pkg)
			case 6:
				fmt.Fprintf(&b, `  {"Action":%q,"Package":%q,"Test":%q}  `, action, pkg, name)
			case 7:
				fmt.Fprintf(&b, `{"Action":%q,"Test":%q}`, action, name)
			case 8:
				b.WriteString("")
			default:
				fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`, action, pkg, name)
			}
			b.WriteString("\n")
		}
		if i%1000 == 999 {
			b.WriteString(strings.Repeat("x", 1<<20+1) + "\n")
			fmt.Fprintf(&b, `{"Action":"run","Package":"p","Test":"TestZ"}`+"\n"+`{"Action":"pass","Package":"p","Test":"TestZ"}`+"\n")
		}
		compareOutcomes(t, fmt.Sprintf("generated log %d", i), b.String(), append(names, "", "TestZ", "TestNope"))
	}
}

// Code executing in the sandbox can forge go test -json events: a line with
// the test2json framing marker (0x16) printed by a test, or raw JSON written to
// the standard output of the go process (PID 1 of the container, same user).
// The fixture was recorded from a real run of
// go test -json -count=1 ./marker in golang:1.26-bookworm (go1.26.8) with the
// sandbox flags (read-only root filesystem and source mount, --cap-drop=ALL,
// no-new-privileges, uid 65534, no network): one real test, TestReal, forged
// a run and a pass of TestMarkerForged through the marker and of
// TestProcForged through /proc/1/fd/1.
//
// The documented consequence is limited to what is recorded about mutants:
// the forged names count as passing tests, so a forged pass can make a control
// valid or a mutant SURVIVED and changes tests_run. Mutation creates no
// evidence and never affects the exit code except through an incomplete or
// not_run section (report tests).
func TestSandboxCodeCanForgeEvents(t *testing.T) {
	log := fixtureLog(t, "marker")
	_, names := mutation.LogTestNames(log)
	if strings.Join(names, ",") != "TestReal,TestMarkerForged,TestProcForged" {
		t.Fatalf("top-level names %v", names)
	}
	for _, name := range names {
		if action, pkg := harness.GoTestOutcome(log, name); action != "pass" || pkg != "example.test/probe/marker" {
			t.Fatalf("GoTestOutcome(%s) = %q %q", name, action, pkg)
		}
	}
	compareOutcomes(t, "marker", log, names)
	ctl := mutation.NewControl(check("mutation-check-1", model.CheckMutationControl, "PASS", 0, log))
	if ctl.Reason() != "" || ctl.Package() != "example.test/probe/marker" {
		t.Fatalf("control %q %q", ctl.Package(), ctl.Reason())
	}
	v := ctl.Classify(check("mutation-check-2", model.CheckMutant, "PASS", 0, log))
	if v.Status != model.MutantSurvived || v.TestsRun != 3 {
		t.Fatalf("verdict %+v: the forged names are counted as passing tests", v)
	}
}

// Classifying a mutant costs time linear in the size of its logs, whatever
// the number of test names: 20,000 top-level tests (about 3 MiB per log)
// classify in well under the bound, where a per-name rescan of the log would
// take hours.
func TestClassifyTimeIsLinear(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, `{"Action":"run","Package":"example.test/big","Test":"Test%05d"}`+"\n", i)
		fmt.Fprintf(&b, `{"Action":"pass","Package":"example.test/big","Test":"Test%05d","Elapsed":0}`+"\n", i)
	}
	b.WriteString(`{"Action":"pass","Package":"example.test/big","Elapsed":1}` + "\n")
	log := b.String()
	started := time.Now()
	v := mutation.Classify(check("mutation-check-1", model.CheckMutationControl, "PASS", 0, log), check("mutation-check-2", model.CheckMutant, "PASS", 0, log))
	elapsed := time.Since(started)
	if v.Status != model.MutantSurvived || v.TestsRun != 20000 {
		t.Fatalf("verdict %+v", v)
	}
	if elapsed > time.Minute {
		t.Fatalf("classification took %v", elapsed)
	}
	all, top := mutation.LogTestNames(log)
	sort.Strings(all)
	if len(all) != 20000 || len(top) != 20000 || all[0] != "Test00000" {
		t.Fatalf("names %d / %d", len(all), len(top))
	}
}
