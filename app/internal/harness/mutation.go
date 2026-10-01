package harness

// Mutation workspace (F4). Mutants run in a private copy of the sanitized
// candidate snapshot, never in h.candidate or h.base, through the unchanged
// sandbox, and are recorded in the separate mutation ledger.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// A mutation stage keeps its state in its MutationWorkspace, so the harness
// holds nothing for it.

// Audit names of the mutation stage (reserved "stage:" prefix, §1.2).
const (
	auditMutationControl = model.AuditStagePrefix + "run_mutation_control"
	auditMutant          = model.AuditStagePrefix + "run_mutant"
)

// errWorkspaceAborted is returned by every call after a workspace could not
// be verified or restored: no later run may execute on an unknown tree.
var errWorkspaceAborted = errors.New("the mutation workspace was aborted after a failed verification, write or restore")

// Fixed audit texts of an aborted workspace. OS errors are not recorded: they
// name host temporary paths.
const (
	abortResolve = "the file could not be resolved as a regular file in the workspace"
	abortVerify  = "the workspace file did not hold the planned original content"
	abortWrite   = "the mutant could not be written"
	abortRestore = "the original content could not be restored and verified"
)

// MutationWorkspace is a private copy of the candidate snapshot in which one
// file at a time is replaced by a mutant. It implements mutation.Workspace.
// Every method takes h.mu, so a stage using it runs sequentially with every
// other harness run.
type MutationWorkspace struct {
	h       *Harness
	dir     string
	cleanup func()
	ceiling time.Duration
	aborted error
}

// NewMutationWorkspace copies the candidate snapshot into a new directory
// under the harness root. The copy is pristine: generated tests are staged in
// h.candidate only while runGenerated holds h.mu.
func (h *Harness) NewMutationWorkspace() (*MutationWorkspace, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	dir, cleanup, err := h.privateCopy("mutation-")
	if err != nil {
		return nil, err
	}
	return &MutationWorkspace{h: h, dir: dir, cleanup: cleanup, ceiling: h.preReviewerCeiling()}, nil
}

// preReviewerCeiling is the budget limit of the pre-reviewer v0.4 stages: the
// runtime budget minus the reviewer reserve, or 0 (no ceiling) without a
// reviewer (§1.7.1 rule 4). Caller holds h.mu.
func (h *Harness) preReviewerCeiling() time.Duration {
	if h.opts.ReviewerReserve <= 0 {
		return 0
	}
	if c := h.opts.MaxRuntime - h.opts.ReviewerReserve; c > 0 {
		return c
	}
	// The reserve takes the whole budget: a ceiling of one nanosecond leaves
	// nothing, and runs are recorded as SKIPPED for the reviewer reserve.
	return time.Nanosecond
}

// Close removes the workspace. Harness.Close removes it too.
func (w *MutationWorkspace) Close() {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	w.cleanup()
}

// ReadSource returns a repository-relative regular text file of the private
// copy (at most 1 MiB, UTF-8, no NUL). Secret-bearing paths were excluded from
// the snapshot and are refused.
func (w *MutationWorkspace) ReadSource(rel string) ([]byte, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	if err := w.usable(); err != nil {
		return nil, err
	}
	path, err := w.regularFile(rel)
	if err != nil {
		return nil, err
	}
	return readBounded(path)
}

// HasTestFile reports whether the repository-relative directory ("." for the
// root) directly contains a regular *_test.go file.
func (w *MutationWorkspace) HasTestFile(dir string) (bool, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	if err := w.usable(); err != nil {
		return false, err
	}
	path := w.dir
	if dir != "." {
		var err error
		if path, err = safePath(w.dir, dir); err != nil {
			return false, err
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), "_test.go") {
			return true, nil
		}
	}
	return false, nil
}

// RunControl runs command unchanged in the workspace (kind mutation_control,
// mutation ledger). timeout > 0 can only tighten the policy timeout.
func (w *MutationWorkspace) RunControl(ctx context.Context, pkg string, command []string, timeout time.Duration) (model.Check, error) {
	h := w.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := w.usable(); err != nil {
		return model.Check{}, err
	}
	started := time.Now()
	c := w.run(ctx, model.CheckMutationControl, command, runOptions{timeout: timeout, ceiling: w.ceiling, ledger: ledgerMutation})
	w.audit(auditMutationControl, started, c.Status, map[string]string{"package": pkg, "check_id": c.ID})
	return c, nil
}

// RunMutant writes mutated over rel, which must be a regular file whose
// content still equals original, runs command (kind mutant, mutation ledger),
// then writes the original back and re-reads it. A content mismatch before the
// write, or a failed write or restore, aborts the workspace: the error is
// returned now and by every later call, so no run executes on a tree the
// report does not describe. The check recorded before a failed restore is
// returned with the error.
//
// It also returns the time limit the run had (0 when it did not start): the
// policy timeout tightened by timeout and by what the runtime budget, or the
// pre-reviewer ceiling, left. The stage uses it to tell a mutant that reached
// its own time limit from a run a budget cut short.
func (w *MutationWorkspace) RunMutant(ctx context.Context, id, rel string, original, mutated []byte, command []string, timeout time.Duration) (model.Check, time.Duration, error) {
	h := w.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := w.usable(); err != nil {
		return model.Check{}, 0, err
	}
	started := time.Now()
	arguments := map[string]string{"mutant_id": id, "path": rel}
	path, err := w.regularFile(rel)
	if err != nil {
		return model.Check{}, 0, w.abort(started, arguments, abortResolve)
	}
	current, err := readBounded(path)
	if err != nil || sha256.Sum256(current) != sha256.Sum256(original) {
		return model.Check{}, 0, w.abort(started, arguments, abortVerify)
	}
	if err := overwrite(path, mutated); err != nil {
		if restore(path, original) != nil {
			return model.Check{}, 0, w.abort(started, arguments, abortRestore)
		}
		return model.Check{}, 0, w.abort(started, arguments, abortWrite)
	}
	// Computed under the same hold of h.mu as the reservation runWithOptions
	// makes next, so it is the limit reserveRun gives the run.
	limit := w.runLimit(timeout)
	c := w.run(ctx, model.CheckMutant, command, runOptions{timeout: timeout, ceiling: w.ceiling, ledger: ledgerMutation})
	if c.Status == "SKIPPED" {
		limit = 0
	}
	arguments["check_id"] = c.ID
	if restore(path, original) != nil {
		return c, limit, w.abort(started, arguments, abortRestore)
	}
	w.audit(auditMutant, started, c.Status, arguments)
	return c, limit, nil
}

// runLimit is the time limit runWithOptions gives a mutation run that
// requests timeout: the policy timeout, tightened by timeout, then by what is
// left under MaxRuntime or the workspace ceiling after the time spent and
// reserved (reserveRun, §1.7.1). Caller holds h.mu.
func (w *MutationWorkspace) runLimit(timeout time.Duration) time.Duration {
	h := w.h
	effective := h.opts.Timeout
	if timeout > 0 && timeout < effective {
		effective = timeout
	}
	limit := h.opts.MaxRuntime
	if w.ceiling > 0 && w.ceiling < limit {
		limit = w.ceiling
	}
	if avail := limit - h.spent - h.reserved; avail < effective {
		effective = avail
	}
	if effective < 0 {
		return 0
	}
	return effective
}

// SavePatch retains data, redacted, as a mutant_patch artifact and returns
// the sha256 of the retained bytes.
func (w *MutationWorkspace) SavePatch(name string, data []byte) (string, error) {
	h := w.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", errors.New("harness is closed")
	}
	if name == "" || strings.ContainsAny(name, `/\:`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid patch artifact name %q", name)
	}
	if err := h.saveArtifact(name, model.ArtifactMutantPatch, []byte(Redact(string(data)))); err != nil {
		return "", err
	}
	return h.artifacts[len(h.artifacts)-1].SHA256, nil
}

// usable refuses calls after an abort or on a closed harness. Caller holds h.mu.
func (w *MutationWorkspace) usable() error {
	if w.aborted != nil {
		return w.aborted
	}
	if w.h.closed {
		return errors.New("harness is closed")
	}
	return nil
}

// regularFile resolves rel inside the workspace (safePath: no escape, no
// symlink component, no secret-bearing name) and requires an existing
// regular file. Caller holds h.mu.
func (w *MutationWorkspace) regularFile(rel string) (string, error) {
	path, err := safePath(w.dir, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	return path, nil
}

// abort marks the workspace unusable, audits the fixed cause and returns the
// error every later call returns. Caller holds h.mu.
func (w *MutationWorkspace) abort(started time.Time, arguments map[string]string, cause string) error {
	w.aborted = errWorkspaceAborted
	arguments["error"] = cause
	w.audit(auditMutant, started, "ERROR", arguments)
	return w.aborted
}

// audit records one stage event. Caller holds h.mu.
func (w *MutationWorkspace) audit(tool string, started time.Time, status string, arguments map[string]string) {
	b, _ := json.Marshal(arguments)
	w.h.audit = append(w.h.audit, model.AuditEvent{Time: started.UTC(), Tool: tool, Arguments: truncateUTF8(Redact(string(b)), 4096), Status: status, DurationMS: time.Since(started).Milliseconds()})
}

// overwrite replaces the content of an existing file without creating a new
// path: O_WRONLY|O_TRUNC, never O_CREATE.
func overwrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// restore writes original back and verifies it by re-reading the file.
func restore(path string, original []byte) error {
	if err := overwrite(path, original); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("the workspace file is no longer a regular file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, original) {
		return errors.New("the workspace file does not hold the original content")
	}
	return nil
}

// run records one control or mutant run. A Vitest or Jest command (one that
// writes ResultsPath) returns its JSON report on the payload channel, recorded
// in Check.Results; a report the run did not write leaves the status as it is
// and the mutant inconclusive, never ERROR, since candidate code decides
// whether the runner writes it. Caller holds h.mu.
func (w *MutationWorkspace) run(ctx context.Context, kind string, command []string, o runOptions) model.Check {
	for _, arg := range command {
		if strings.Contains(arg, ResultsPath) {
			o.reportOptional = true
			return w.h.runWithResultsOptions(ctx, kind, w.dir, command, o)
		}
	}
	c, _, _ := w.h.runWithOptions(ctx, kind, w.dir, command, o)
	return c
}
