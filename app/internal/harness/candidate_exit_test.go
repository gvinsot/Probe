package harness

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// §1.17 / Appendix D.6: a candidate-side capture run (fuzz_candidate, as a
// TS/JS runner passes a process.exit code through) keeps exit codes of 125
// and above for sandbox failures: the script reports a command status of 125
// or above as 124, and the snapshot copy failure stays 125. The script of
// every other capture run is unchanged.
func TestCandidateCaptureScriptKeepsHighExitCodesForTheSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper scripts run in a Linux sandbox")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	const copyStep = "cp -R /source/. /workspace/ 1>&2 || exit 125; "
	const path = "/nonexistent/probe-capture"
	exitOf := func(script string, status string) int {
		t.Helper()
		err := exec.Command(sh, "-c", script, "sh", "sh", "-c", "exit "+status).Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee):
			return ee.ExitCode()
		}
		t.Fatal(err)
		return -1
	}
	for _, s := range []string{candidateCaptureScript(path), captureScript(path)} {
		if !strings.HasPrefix(s, copyStep) {
			t.Fatalf("script %q does not start with the snapshot copy", s)
		}
	}
	candidate := strings.TrimPrefix(candidateCaptureScript(path), copyStep)
	plain := strings.TrimPrefix(captureScript(path), copyStep)
	for status, want := range map[string]int{"0": 0, "1": 1, "124": 124, "125": 124, "126": 124, "127": 124, "137": 124, "200": 124, "255": 124} {
		if got := exitOf(candidate, status); got != want {
			t.Errorf("candidate capture script: command status %s gave %d, want %d", status, got, want)
		}
		if n, _ := strconv.Atoi(status); exitOf(plain, status) != n {
			t.Errorf("capture script: command status %s changed", status)
		}
	}
	// A failed snapshot copy exits 125. The copy source is replaced by a path
	// that does not exist, so the step fails wherever the test runs (inside
	// a Probe sandbox /source exists) and copies nothing.
	if _, err := exec.LookPath("cp"); err == nil {
		failing := strings.Replace(candidateCaptureScript(path), "cp -R /source/. ", "cp -R /nonexistent/probe-source/. ", 1)
		if failing == candidateCaptureScript(path) {
			t.Fatal("the copy step was not found")
		}
		if got := exitOf(failing, "0"); got != 125 {
			t.Errorf("a failed snapshot copy gave %d, want 125", got)
		}
	}
}

// Only candidate-side kinds with a capture channel get the candidate script;
// the v0.2 kinds and the baseline-side v0.4 kinds keep captureScript.
func TestCaptureScriptPerKind(t *testing.T) {
	h := fixture(t)
	var script string
	h.executeCapture = func(_ context.Context, _ string, args []string, _, _ io.Writer) execution {
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				script = args[i+1]
				break
			}
		}
		return execution{}
	}
	for kind, want := range map[string]string{
		model.CheckFuzzCandidate:        candidateCaptureScript(FuzzObservationsPath),
		model.CheckFuzzCandidateConfirm: candidateCaptureScript(FuzzObservationsPath),
		model.CheckFuzzBase:             captureScript(FuzzObservationsPath),
		model.CheckFuzzBaseConfirm:      captureScript(FuzzObservationsPath),
		model.CheckGeneratedCandidate:   captureScript(FuzzObservationsPath),
		model.CheckGeneratedIntent:      captureScript(FuzzObservationsPath),
	} {
		script = ""
		dir := h.candidate
		if strings.HasSuffix(kind, "_base") || strings.Contains(kind, "_base_") {
			dir = h.base
		}
		runLocked(h, context.Background(), kind, dir, baseCommand, runOptions{capture: FuzzObservationsPath})
		if script != want {
			t.Errorf("%s ran %q, want %q", kind, script, want)
		}
	}
}
