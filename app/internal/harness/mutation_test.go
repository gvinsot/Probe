package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// treeDigest hashes every regular file of dir by relative path.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mountSource returns the bind source of recorded docker arguments.
func mountSource(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "type=bind,src=") {
			return strings.TrimSuffix(strings.TrimPrefix(arg, "type=bind,src="), ",dst=/source,readonly")
		}
	}
	return ""
}

var mutationCommand = []string{"go", "test", "-json", "-count=1", "./pkg"}

const pkgSource = "package pkg\nfunc Value() int { return 42 }\n"
const pkgMutant = "package pkg\nfunc Value() int { return 43 }\n"

// Mutants run in a private copy: while the mutant runs the workspace file
// holds the mutant, the candidate and baseline snapshots never change, and
// the original is restored afterwards.
func TestMutationWorkspaceIsolation(t *testing.T) {
	h := fixture(t)
	candidateBefore, baseBefore := treeDigest(t, h.candidate), treeDigest(t, h.base)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if w.dir == h.candidate || w.dir == h.base || !strings.HasPrefix(w.dir, h.root) {
		t.Fatalf("workspace %s is not a private copy under %s", w.dir, h.root)
	}
	var seen string
	var args []string
	h.execute = func(_ context.Context, name string, a []string, out io.Writer) execution {
		args = a
		b, _ := os.ReadFile(filepath.Join(mountSource(a), "pkg", "main.go"))
		seen = string(b)
		io.WriteString(out, "{}\n")
		return execution{ExitCode: 1}
	}
	c, err := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, 0)
	if err != nil {
		t.Fatal(err)
	}
	if seen != pkgMutant {
		t.Fatalf("the run did not see the mutant: %q", seen)
	}
	if mountSource(args) != w.dir {
		t.Fatalf("mounted %s, want the workspace %s", mountSource(args), w.dir)
	}
	if c.ID != "mutation-check-1" || c.Kind != model.CheckMutant || c.Status != "FAIL" {
		t.Fatalf("check %+v", c)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "pkg", "main.go")); string(b) != pkgSource {
		t.Fatalf("the original was not restored: %q", b)
	}
	if !reflect.DeepEqual(treeDigest(t, h.candidate), candidateBefore) || !reflect.DeepEqual(treeDigest(t, h.base), baseBefore) {
		t.Fatal("a harness snapshot changed")
	}
	w.Close()
	if _, err := os.Stat(w.dir); !os.IsNotExist(err) {
		t.Fatal("Close did not remove the workspace")
	}
}

// Control and mutant runs keep every isolation flag of dockerArgs; only the
// bind source differs.
func TestMutantArgsKeepIsolation(t *testing.T) {
	h := fixture(t)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var recorded [][]string
	var names []string
	h.execute = func(_ context.Context, name string, a []string, out io.Writer) execution {
		recorded, names = append(recorded, a), append(names, name)
		return execution{ExitCode: 0}
	}
	if _, err := w.RunControl(context.Background(), "./pkg", mutationCommand, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, 0); err != nil {
		t.Fatal(err)
	}
	for i, a := range recorded {
		if want := h.dockerArgs(names[i], w.dir, mutationCommand); !reflect.DeepEqual(a, want) {
			t.Fatalf("run %d args differ from dockerArgs:\n got %q\nwant %q", i, a, want)
		}
		for _, flag := range []string{"--network", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=65534:65534", "--pull=never"} {
			found := false
			for _, arg := range a {
				found = found || arg == flag
			}
			if !found {
				t.Fatalf("run %d lacks %s", i, flag)
			}
		}
	}
}

// Mutation runs go to the mutation ledger only, keep their logs as
// mutation_check_output artifacts and are audited under the stage names.
func TestMutationChecksSeparateLedger(t *testing.T) {
	h := fixture(t)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	countingExec(h, "log line\n", 0, 1)
	control, _ := w.RunControl(context.Background(), "./pkg", mutationCommand, 0)
	mutant, _ := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, 0)
	if control.ID != "mutation-check-1" || control.Kind != model.CheckMutationControl || mutant.ID != "mutation-check-2" || mutant.Kind != model.CheckMutant {
		t.Fatalf("checks %+v %+v", control, mutant)
	}
	if len(h.Checks()) != 0 || len(h.MutationChecks()) != 2 {
		t.Fatalf("ledgers %d / %d", len(h.Checks()), len(h.MutationChecks()))
	}
	kinds := map[string]int{}
	for _, a := range h.Artifacts() {
		kinds[a.Kind]++
	}
	if kinds[model.ArtifactMutationCheckOutput] != 2 || kinds["check_output"] != 0 {
		t.Fatalf("artifacts %v", kinds)
	}
	var tools []string
	for _, e := range h.Audit() {
		tools = append(tools, e.Tool+":"+e.Status)
		if !strings.Contains(e.Arguments, `"check_id":"mutation-check-`) {
			t.Fatalf("audit arguments %q", e.Arguments)
		}
	}
	if strings.Join(tools, ",") != "stage:run_mutation_control:PASS,stage:run_mutant:FAIL" {
		t.Fatalf("audit %v", tools)
	}
}

// The requested timeout can only tighten the policy timeout.
func TestMutationTimeoutOnlyTightens(t *testing.T) {
	h := fixture(t)
	h.opts.Timeout = time.Minute
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var remaining time.Duration
	h.execute = func(ctx context.Context, _ string, _ []string, _ io.Writer) execution {
		remaining = deadlineRemaining(t, ctx)
		return execution{ExitCode: 0}
	}
	if _, err := w.RunControl(context.Background(), "./pkg", mutationCommand, 5*time.Second); err != nil || remaining > 5*time.Second {
		t.Fatalf("tightened timeout %v (%v)", remaining, err)
	}
	if _, err := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, time.Hour); err != nil || remaining > time.Minute || remaining < 50*time.Second {
		t.Fatalf("loosened timeout %v (%v)", remaining, err)
	}
}

// With a reviewer, mutation stops at the budget minus the reviewer reserve.
func TestMutationRespectsReviewerReserve(t *testing.T) {
	h := fixture(t)
	h.opts.MaxRuntime, h.opts.ReviewerReserve = 10*time.Minute, 5*time.Minute
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	calls := countingExec(h, "", 0)
	h.mu.Lock()
	h.spent = 5 * time.Minute
	h.mu.Unlock()
	c, err := w.RunControl(context.Background(), "./pkg", mutationCommand, 0)
	if err != nil || c.Status != "SKIPPED" || c.Output != budgetReservedText || *calls != 0 {
		t.Fatalf("control %+v, %d calls (%v)", c, *calls, err)
	}
	h.opts.ReviewerReserve = 0
	w2, _ := h.NewMutationWorkspace()
	defer w2.Close()
	if c, _ := w2.RunControl(context.Background(), "./pkg", mutationCommand, 0); c.Status != "PASS" {
		t.Fatalf("without a reviewer the run is limited by the budget only: %+v", c)
	}
}

// A workspace file that no longer holds the planned original, or is not a
// regular file, aborts the workspace before anything runs.
func TestMutationRejectsStaleOriginalAndNonRegular(t *testing.T) {
	h := fixture(t)
	calls := countingExec(h, "", 0)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte("stale"), []byte(pkgMutant), mutationCommand, 0); err == nil {
		t.Fatal("a stale original was accepted")
	}
	if *calls != 0 {
		t.Fatal("a run started after a failed verification")
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "pkg", "main.go")); string(b) != pkgSource {
		t.Fatal("the workspace was written after a failed verification")
	}
	if _, err := w.RunControl(context.Background(), "./pkg", mutationCommand, 0); err == nil {
		t.Fatal("an aborted workspace ran a control")
	}
	if _, err := w.ReadSource("pkg/main.go"); err == nil {
		t.Fatal("an aborted workspace was read")
	}
	var abortAudit string
	for _, e := range h.Audit() {
		if e.Tool == auditMutant && e.Status == "ERROR" {
			abortAudit = e.Arguments
		}
	}
	if !strings.Contains(abortAudit, abortVerify) || strings.Contains(abortAudit, h.root) {
		t.Fatalf("abort audit %q", abortAudit)
	}

	h2 := fixture(t)
	for _, rel := range []string{"pkg/dir.go", "pkg/new.go", "../escape.go", ".env"} {
		w3, _ := h2.NewMutationWorkspace()
		if rel == "pkg/dir.go" {
			_ = os.Mkdir(filepath.Join(w3.dir, "pkg", "dir.go"), 0755)
		}
		if _, err := w3.RunMutant(context.Background(), "mutant-1", rel, []byte(""), []byte("x"), mutationCommand, 0); err == nil {
			t.Fatalf("%s: accepted", rel)
		}
		if _, err := os.Stat(filepath.Join(w3.dir, "pkg", "new.go")); !os.IsNotExist(err) {
			t.Fatal("RunMutant created a new path")
		}
		w3.Close()
	}
}

// A restore that cannot be verified aborts the workspace; the recorded check
// is returned with the error and nothing runs afterwards.
func TestMutationAbortAfterRestoreFailure(t *testing.T) {
	h := fixture(t)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	calls := 0
	h.execute = func(_ context.Context, _ string, a []string, _ io.Writer) execution {
		calls++
		// Replace the mutated file by a directory: the restore cannot write it.
		path := filepath.Join(mountSource(a), "pkg", "main.go")
		_ = os.Remove(path)
		_ = os.Mkdir(path, 0755)
		return execution{ExitCode: 0}
	}
	c, err := w.RunMutant(context.Background(), "mutant-1", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, 0)
	if err == nil || c.ID != "mutation-check-1" {
		t.Fatalf("restore failure: check %+v err %v", c, err)
	}
	if _, err := w.RunMutant(context.Background(), "mutant-2", "pkg/main.go", []byte(pkgSource), []byte(pkgMutant), mutationCommand, 0); err == nil || calls != 1 {
		t.Fatalf("a run started after a failed restore (%d calls, %v)", calls, err)
	}
}

func TestMutationSourceAccess(t *testing.T) {
	h := fixture(t)
	for _, dir := range []string{h.candidate} {
		if err := os.WriteFile(filepath.Join(dir, "pkg", "main_test.go"), []byte("package pkg\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package root\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if b, err := w.ReadSource("pkg/main.go"); err != nil || string(b) != pkgSource {
		t.Fatalf("ReadSource %q %v", b, err)
	}
	for _, rel := range []string{".env", "../x.go", "pkg", "missing.go"} {
		if _, err := w.ReadSource(rel); err == nil {
			t.Errorf("ReadSource(%q) succeeded", rel)
		}
	}
	for dir, want := range map[string]bool{"pkg": true, ".": false} {
		if got, err := w.HasTestFile(dir); err != nil || got != want {
			t.Errorf("HasTestFile(%q) = %v, %v", dir, got, err)
		}
	}
	if _, err := w.HasTestFile("../outside"); err == nil {
		t.Error("HasTestFile escaped the workspace")
	}
}

func TestSavePatchRedactsAndHashes(t *testing.T) {
	h := fixture(t)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	secret := "token := \"ghp_" + strings.Repeat("a", 36) + "\"\n"
	sha, err := w.SavePatch("mutant-1.patch", []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := h.Artifacts()
	a := artifacts[len(artifacts)-1]
	b, _ := os.ReadFile(a.Path)
	sum := sha256.Sum256(b)
	if a.Kind != model.ArtifactMutantPatch || a.SHA256 != sha || hex.EncodeToString(sum[:]) != sha || strings.Contains(string(b), "ghp_") {
		t.Fatalf("artifact %+v content %q", a, b)
	}
	for _, name := range []string{"", "../x", "a/b", `a\b`} {
		if _, err := w.SavePatch(name, []byte("x")); err == nil {
			t.Errorf("SavePatch(%q) accepted", name)
		}
	}
}
