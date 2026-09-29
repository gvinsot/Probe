package harness

// Baseline versions of changed Go tests on candidate code (F3, --base-tests).
//
// Each selected test is the baseline version of a Go test function that the
// change modified or removed. It runs twice with one identical command: on the
// baseline tree (check kind base_test_base) and on a hybrid tree (check kind
// base_test_hybrid), a private copy of the candidate tree in which the
// *_test.go files and the testdata directory of the test's package directory
// are replaced by the baseline ones. The hybrid tree is built on the host from
// the sanitized snapshots and runs with the unchanged sandbox profile.
//
// Every test whose run pair was recorded gets one base_test_differential
// evidence record, classified by ClassifyExistingTest; report.Finalize
// re-derives the status from the recorded checks. Nothing here can produce a
// reproduced issue. Unexported names carry a baseTest prefix so that they
// never collide with another stage's file in this package.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// baseTestState is the per-harness state of the stage.
type baseTestState struct {
	calls int // RunBaseTests calls; each hybrid manifest gets its own artifact name
}

const (
	// auditRunBaseTests is the audit tool name of one unit of the stage.
	auditRunBaseTests = model.AuditStagePrefix + "run_base_tests"
	// baseTestSubCap is the stage's sub-cap inside the shared runtime budget
	// (§1.7.1): no run starts once the stage's runs have used it up, and each
	// run's timeout is at most what remains of it.
	baseTestSubCap = 180 * time.Second
	// baseTestMaxNames bounds the test names of one -run expression.
	baseTestMaxNames = 50
	// baseTestMaxUnits bounds the units (run pairs) of one review.
	baseTestMaxUnits = 10
	// baseTestManifestLimit bounds the entries of the hybrid-tree manifest.
	baseTestManifestLimit = 2000
	// baseTestFileLimit bounds one test file copied into the hybrid tree, as
	// copySnapshot does.
	baseTestFileLimit = 32 << 20
	// baseTestNothingRan is the section reason when no run of the stage started.
	baseTestNothingRan = "no selected test could run; see the reason of each test"
	// baseTestPassedInsideFailure starts the reason of a test that passed in a
	// hybrid run that failed as a whole and got no result from a pair of its own.
	baseTestPassedInsideFailure = "the test passed inside a candidate-side run that failed as a whole, which supports no result on its own"
	// baseTestCopyFailed and baseTestManifestLost are the operational
	// failures of the stage (exit 4): host-side, never caused by the layout of
	// the candidate tree.
	baseTestCopyFailed   = "the private copy of the candidate snapshot for the hybrid tree could not be made"
	baseTestManifestLost = "the manifest of the hybrid tree could not be retained"
	// baseTestDirFailed is the reason of a test whose package directory could
	// not be reverted in the hybrid tree. Only that unit is affected.
	baseTestDirFailed = "not run: the baseline test files of this package directory could not be restored in the hybrid tree"
	// baseTestTemplateReason is the section reason for a generated_test
	// command that cannot establish which Go tests ran.
	baseTestTemplateReason = "the generated_test command cannot establish which Go tests ran; configure it as go test {package}"
)

// baseTestNamePattern is what a selected name must look like: a Go test
// function identifier. It keeps -run expressions and reports unambiguous.
var baseTestNamePattern = regexp.MustCompile(`^Test[\p{L}\p{N}_]*$`)

// RunBaseTests runs the baseline version of each selected test on the baseline
// tree and on the hybrid tree and records one base_test_differential evidence
// record per test and recorded run pair. The input is copied; the returned
// section lists the same tests in the same order with their status, reason and
// evidence ID. It returns an error only when the private copy of the candidate
// snapshot cannot be made or the manifest of the hybrid tree cannot be
// retained: host-side operational failures, after which every test is
// UNVERIFIED. A package directory that cannot be reverted in the hybrid tree
// leaves only its own tests UNVERIFIED.
//
// Rules:
//   - The generated_test template must pass VerifiableGoTemplate, or nothing
//     runs and the section is not_run.
//   - Tests are grouped by package directory ({package}) or file ({file}),
//     with at most 50 names per run and 10 units per review.
//   - A baseline run that does not pass as a whole gets no candidate-side run,
//     except that the tests it records as passed run once more on their own.
//   - When the hybrid run fails as a whole, the tests it records as passed get
//     one more run pair on their own (a pass event inside a failed check never
//     supports PASSES_ON_CANDIDATE).
//   - A replayed baseline run never supports FAILS_ON_CANDIDATE: the baseline
//     runs again live with the same kind and command, and those tests are
//     classified against the live run (§1.11).
//   - Every run uses what remains of the 180 s sub-cap as its timeout ceiling
//     and, when a reviewer will run, MaxRuntime minus the reviewer reserve as
//     its budget limit (§1.7.1).
func (h *Harness) RunBaseTests(ctx context.Context, selected []model.BaseTest) (model.BaseTests, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	started := time.Now()
	tests := append([]model.BaseTest{}, selected...)
	for i := range tests {
		tests[i].Status, tests[i].EvidenceID, tests[i].Reason = model.StatusUnverified, "", ""
	}
	result := model.BaseTests{Status: model.BaseTestsNotRun, Tests: tests, Note: model.BaseTestsNote}
	if len(tests) == 0 {
		result.Status = model.BaseTestsNoCandidates
		return result, nil
	}
	h.baseTests.calls++
	if reason := h.baseTestsUnavailable(); reason != "" {
		result.Reason = reason
		for i := range tests {
			markBaseTest(tests, i, "not run: "+reason)
		}
		h.auditBaseTests(started, map[string]any{"tests": len(tests), "reason": reason}, "SKIPPED")
		return result, nil
	}
	units := h.planBaseTestUnits(tests)
	if len(units) == 0 {
		result.Reason = baseTestNothingRan
		h.auditBaseTests(started, map[string]any{"tests": len(tests), "reason": baseTestNothingRan}, "SKIPPED")
		return result, nil
	}
	failure := baseTestCopyFailed
	hybrid, cleanup, manifest, failedDirs, err := h.buildBaseTestHybrid(units)
	if err != nil {
		err = fmt.Errorf("%s: %w", failure, err)
	} else if saveErr := h.saveArtifact(fmt.Sprintf("base-tests-hybrid-%d.json", h.baseTests.calls), model.ArtifactBaseTestHybridManifest, manifest); saveErr != nil {
		cleanup()
		failure = baseTestManifestLost
		err = fmt.Errorf("%s: %w", failure, saveErr)
	}
	if err != nil {
		for _, u := range units {
			for _, i := range u.items {
				markBaseTest(tests, i, "not run: "+failure)
			}
		}
		result.Reason = failure
		// The detailed error, which names host paths, goes to the caller only.
		h.auditBaseTests(started, map[string]any{"tests": len(tests), "reason": failure}, "ERROR")
		return result, err
	}
	defer cleanup()
	if len(failedDirs) > 0 {
		dirs := make([]string, 0, len(failedDirs))
		for d := range failedDirs {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			h.auditBaseTests(started, map[string]any{"dir": d, "reason": strings.TrimPrefix(baseTestDirFailed, "not run: "), "error": failedDirs[d]}, "ERROR")
		}
		var kept []baseTestUnit
		for _, u := range units {
			if _, failed := failedDirs[u.dir]; !failed {
				kept = append(kept, u)
				continue
			}
			for _, i := range u.items {
				markBaseTest(tests, i, baseTestDirFailed)
			}
		}
		if units = kept; len(units) == 0 {
			result.Reason = baseTestNothingRan
			return result, nil
		}
	}
	run := &baseTestRun{h: h, ctx: ctx, hybrid: hybrid, ceiling: h.baseTestsCeiling()}
	for n, u := range units {
		if reason := run.stopReason(); reason != "" {
			for _, rest := range units[n:] {
				for _, i := range rest.items {
					markBaseTest(tests, i, reason)
				}
			}
			break
		}
		run.unit(u, tests)
	}
	switch {
	case run.executed:
		result.Status = model.BaseTestsRan
	case run.skipped != "":
		result.Reason = "no run started: " + run.skipped
	default:
		result.Reason = baseTestNothingRan
	}
	return result, nil
}

// baseTestsUnavailable returns why the stage cannot run at all, or "".
func (h *Harness) baseTestsUnavailable() string {
	switch {
	case h.closed:
		return "the harness is closed"
	case h.base == "":
		return "no baseline snapshot is available"
	case !verifiableGoTemplate(h.opts.Commands["generated_test"]):
		return baseTestTemplateReason
	}
	return ""
}

// baseTestsCeiling is the budget limit of the stage's runs: MaxRuntime minus
// the reviewer reserve when a reviewer will run, 0 (the whole budget)
// otherwise, and -1 when the reserve leaves nothing.
func (h *Harness) baseTestsCeiling() time.Duration {
	if h.opts.ReviewerReserve <= 0 {
		return 0
	}
	if c := h.opts.MaxRuntime - h.opts.ReviewerReserve; c > 0 {
		return c
	}
	return -1
}

// markBaseTest records that test i received no classification, and why.
func markBaseTest(tests []model.BaseTest, i int, reason string) {
	tests[i].Status, tests[i].EvidenceID, tests[i].Reason = model.StatusUnverified, "", reason
}

// auditBaseTests appends one stage:run_base_tests audit event. Caller holds h.mu.
func (h *Harness) auditBaseTests(started time.Time, arguments map[string]any, status string) {
	b, _ := json.Marshal(arguments)
	h.audit = append(h.audit, model.AuditEvent{Time: started.UTC(), Tool: auditRunBaseTests, Arguments: truncateUTF8(Redact(string(b)), 4096), Status: status, DurationMS: time.Since(started).Milliseconds()})
}

// --- units -------------------------------------------------------------------

// baseTestUnit is one baseline run and its candidate-side run: the tests of
// one package directory (or one file with a {file} template), at most
// baseTestMaxNames names.
type baseTestUnit struct {
	dir    string   // package directory, slash-separated ("." for the root)
	target string   // path substituted into the generated_test template
	names  []string // sorted, unique
	items  []int    // indexes of the tests of these names, ascending
}

// planBaseTestUnits groups the tests that can run into units and marks the
// others UNVERIFIED with their reason. Caller holds h.mu.
func (h *Harness) planBaseTestUnits(tests []model.BaseTest) []baseTestUnit {
	byFile := baseTestFileTemplate(h.opts.Commands["generated_test"])
	type group struct {
		dir, target string
		items       []int
	}
	groups := map[string]*group{}
	var keys []string
	for i, t := range tests {
		if reason := h.baseTestPrecheck(t); reason != "" {
			markBaseTest(tests, i, reason)
			continue
		}
		dir := path.Dir(t.Path)
		key := dir
		if byFile {
			key = t.Path
		}
		g := groups[key]
		if g == nil {
			g = &group{dir: dir, target: t.Path}
			groups[key] = g
			keys = append(keys, key)
		}
		g.items = append(g.items, i)
	}
	sort.Strings(keys)
	var units []baseTestUnit
	for _, key := range keys {
		g := groups[key]
		byName := map[string][]int{}
		var names []string
		for _, i := range g.items {
			name := tests[i].Name
			if _, seen := byName[name]; !seen {
				names = append(names, name)
			}
			byName[name] = append(byName[name], i)
		}
		sort.Strings(names)
		for start := 0; start < len(names); start += baseTestMaxNames {
			end := min(start+baseTestMaxNames, len(names))
			u := baseTestUnit{dir: g.dir, target: g.target, names: append([]string(nil), names[start:end]...)}
			for _, name := range u.names {
				u.items = append(u.items, byName[name]...)
			}
			sort.Ints(u.items)
			units = append(units, u)
		}
	}
	if len(units) > baseTestMaxUnits {
		for _, u := range units[baseTestMaxUnits:] {
			for _, i := range u.items {
				markBaseTest(tests, i, fmt.Sprintf("not run: the limit of %d runs per review of this stage was reached", baseTestMaxUnits))
			}
		}
		units = units[:baseTestMaxUnits]
	}
	return units
}

// baseTestFileTemplate reports whether the generated_test template targets a
// file ({file}) rather than a package ({package}).
func baseTestFileTemplate(command []string) bool {
	for _, arg := range command {
		switch arg {
		case "{file}":
			return true
		case "{package}":
			return false
		}
	}
	return false
}

// baseTestPrecheck returns why a selected test cannot run, or "". Caller
// holds h.mu.
func (h *Harness) baseTestPrecheck(t model.BaseTest) string {
	if !baseTestNamePattern.MatchString(t.Name) || !isGoTestName(t.Name) || !strings.HasSuffix(t.Path, "_test.go") {
		return "not run: not a Go test function of a Go test file"
	}
	p, err := safePath(h.base, t.Path)
	if err != nil {
		return "not run: the baseline test file is excluded from the sandbox snapshots (sensitive or unsafe path)"
	}
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return "not run: the baseline test file is not in the baseline snapshot"
	}
	dir := path.Dir(t.Path)
	if baseTestHasCode(h.base, dir) && !baseTestHasCode(h.candidate, dir) {
		return "not run: the candidate tree has no non-test Go file in " + dir + " (the package was removed or moved)"
	}
	return ""
}

// baseTestHasCode reports whether dir under root holds a Go file that go
// build compiles by name: *.go, not *_test.go, not starting with _ or ".".
func baseTestHasCode(root, dir string) bool {
	d := root
	if dir != "." {
		p, err := safePath(root, dir)
		if err != nil {
			return false
		}
		d = p
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") && !strings.HasPrefix(n, "_") && !strings.HasPrefix(n, ".") {
			return true
		}
	}
	return false
}

// --- hybrid tree -------------------------------------------------------------

// baseTestManifest records how the hybrid tree differs from the candidate
// tree. It is retained as a hashed base_test_hybrid_manifest artifact.
// FailedDirs lists the directories whose revert failed: none of their tests
// ran, and their entries record what was done before the failure.
type baseTestManifest struct {
	Schema     string                  `json:"schema"`
	Dirs       []string                `json:"dirs"`
	FailedDirs []string                `json:"failed_dirs"`
	Entries    []baseTestManifestEntry `json:"entries"`
	Truncated  bool                    `json:"truncated"`
}

// baseTestManifestEntry is one file removed from the candidate copy or
// restored from the baseline. SHA256 is empty only for a removed entry that
// was neither a regular file nor a directory.
type baseTestManifestEntry struct {
	Path   string `json:"path"`
	Action string `json:"action"` // removed_from_candidate | restored_from_baseline
	SHA256 string `json:"sha256"`
}

func (m *baseTestManifest) add(p, action, sum string) {
	if len(m.Entries) >= baseTestManifestLimit {
		m.Truncated = true
		return
	}
	m.Entries = append(m.Entries, baseTestManifestEntry{Path: p, Action: action, SHA256: sum})
}

// buildBaseTestHybrid copies the candidate snapshot privately and reverts the
// test files and testdata of every unit's directory to the baseline. It
// returns the copy, its cleanup, the manifest, and the directories whose
// revert failed with a description of the failure in which host paths are
// replaced by placeholders. Only a failure of the private copy or of the
// manifest encoding is an error. Caller holds h.mu.
func (h *Harness) buildBaseTestHybrid(units []baseTestUnit) (string, func(), []byte, map[string]string, error) {
	dir, cleanup, err := h.privateCopy("hybrid-")
	if err != nil {
		return "", func() {}, nil, nil, err
	}
	m := baseTestManifest{Schema: "probe-base-tests-hybrid/v1", Dirs: []string{}, FailedDirs: []string{}, Entries: []baseTestManifestEntry{}}
	seen := map[string]bool{}
	for _, u := range units {
		if !seen[u.dir] {
			seen[u.dir] = true
			m.Dirs = append(m.Dirs, u.dir)
		}
	}
	sort.Strings(m.Dirs)
	failed := map[string]string{}
	for _, d := range m.Dirs {
		if err := baseTestRevertDir(h.base, dir, d, &m); err != nil {
			failed[d] = baseTestErrorText(err, [2]string{dir, "(hybrid tree)"}, [2]string{h.base, "(baseline snapshot)"}, [2]string{h.root, "(harness directory)"})
			m.FailedDirs = append(m.FailedDirs, d)
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		cleanup()
		return "", func() {}, nil, nil, err
	}
	return dir, cleanup, data, failed, nil
}

// baseTestErrorText is the text of err with each host path replaced by its
// placeholder, in the order given (callers pass nested paths first), so that
// an audit record names no host directory.
func baseTestErrorText(err error, replacements ...[2]string) string {
	text := err.Error()
	for _, r := range replacements {
		if r[0] == "" {
			continue
		}
		for _, form := range []string{filepath.Clean(r[0]), filepath.ToSlash(filepath.Clean(r[0]))} {
			text = strings.ReplaceAll(text, form, r[1])
		}
	}
	return text
}

// baseTestRevertDir makes dir of the hybrid tree hold exactly the baseline's
// *_test.go files and testdata: it makes every component of dir a directory,
// removes every entry named *_test.go directly in it and its testdata entry,
// whatever their type, then copies the baseline's. The other files of the
// directory (the package's code) stay the candidate's. Every removed file is
// recorded in the manifest.
func baseTestRevertDir(base, hybrid, dir string, m *baseTestManifest) error {
	baseDir := base
	if dir != "." {
		var err error
		if baseDir, err = safePath(base, dir); err != nil {
			return err
		}
	}
	hybridDir, err := baseTestEnsureDir(hybrid, dir, m)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(hybridDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if err := baseTestRemove(filepath.Join(hybridDir, e.Name()), path.Join(dir, e.Name()), m); err != nil {
			return err
		}
	}
	testdata := filepath.Join(hybridDir, "testdata")
	if err := baseTestRemove(testdata, path.Join(dir, "testdata"), m); err != nil {
		return err
	}
	baseEntries, err := os.ReadDir(baseDir)
	if err != nil {
		return err
	}
	for _, e := range baseEntries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		sum, err := baseTestCopyFile(filepath.Join(baseDir, e.Name()), filepath.Join(hybridDir, e.Name()))
		if err != nil {
			return err
		}
		m.add(path.Join(dir, e.Name()), "restored_from_baseline", sum)
	}
	baseTestdata := filepath.Join(baseDir, "testdata")
	info, err := os.Lstat(baseTestdata)
	switch {
	case err == nil && info.IsDir():
		if err := copySnapshot(baseTestdata, testdata); err != nil {
			return err
		}
		return baseTestRecordTree(testdata, path.Join(dir, "testdata"), "restored_from_baseline", m)
	case err != nil && !os.IsNotExist(err):
		return err
	}
	return nil
}

// baseTestEnsureDir makes every component of dir under the hybrid root a
// directory and returns its host path. A candidate entry of another type at a
// component (for example a file that replaced a test-only package directory)
// is removed and recorded, because the baseline test files must be restored
// below it. A non-test Go file there is candidate code of the parent package
// and stays: the directory then fails, and only its tests go without a run.
// Lstat never follows a link, so a link at a component would be removed like
// a file; the hybrid tree holds none (copySnapshot skips them), and the final
// safePath rejects one at every component. The caller has validated dir with
// safePath against the baseline snapshot.
func baseTestEnsureDir(hybrid, dir string, m *baseTestManifest) (string, error) {
	if dir == "." {
		return hybrid, nil
	}
	cursor, rel := hybrid, ""
	for _, part := range strings.Split(dir, "/") {
		cursor, rel = filepath.Join(cursor, part), path.Join(rel, part)
		info, err := os.Lstat(cursor)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil && strings.HasSuffix(part, ".go") && !strings.HasSuffix(part, "_test.go"):
			return "", fmt.Errorf("the candidate Go file %s occupies a baseline test directory", rel)
		case err == nil:
			if err := baseTestRemove(cursor, rel, m); err != nil {
				return "", err
			}
		case !os.IsNotExist(err):
			return "", err
		}
		if err := os.Mkdir(cursor, 0755); err != nil {
			return "", err
		}
	}
	return safePath(hybrid, dir)
}

// baseTestRemove removes the hybrid entry at p, whatever its type, and
// records every regular file it held as removed_from_candidate under rel. A
// missing entry is not an error.
func baseTestRemove(p, rel string, m *baseTestManifest) error {
	info, err := os.Lstat(p)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return err
	case info.IsDir():
		if err := baseTestRecordTree(p, rel, "removed_from_candidate", m); err != nil {
			return err
		}
	case info.Mode().IsRegular():
		sum, err := baseTestFileDigest(p)
		if err != nil {
			return err
		}
		m.add(rel, "removed_from_candidate", sum)
	default:
		m.add(rel, "removed_from_candidate", "")
	}
	return os.RemoveAll(p)
}

// baseTestRecordTree adds every regular file under root to the manifest, in
// lexical order, as prefix/relative-path.
func baseTestRecordTree(root, prefix, action string, m *baseTestManifest) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum, err := baseTestFileDigest(p)
		if err != nil {
			return err
		}
		m.add(path.Join(prefix, filepath.ToSlash(rel)), action, sum)
		return nil
	})
}

// baseTestFileDigest returns the hex SHA-256 of a regular file.
func baseTestFileDigest(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// baseTestCopyFile copies one regular file to a new path (O_EXCL) and returns
// the hex SHA-256 of what it wrote.
func baseTestCopyFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > baseTestFileLimit {
		return "", errors.New("not a regular file within the copy limit")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, sum), io.LimitReader(in, baseTestFileLimit))
	closeErr := out.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// --- runs --------------------------------------------------------------------

// baseTestRun is the state of one stage execution: its sub-cap accounting and
// whether any run started.
type baseTestRun struct {
	h        *Harness
	ctx      context.Context
	hybrid   string
	ceiling  time.Duration
	spent    time.Duration // time of the stage's runs that were not replays
	executed bool          // at least one run was not SKIPPED
	skipped  string        // the recorded text of the first SKIPPED run
}

// stopReason returns why no further run may start, or "".
func (r *baseTestRun) stopReason() string {
	switch err := r.ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return "not run: " + expiredText(r.ctx)
	case err != nil:
		return "not run: the review was cancelled"
	case r.ceiling < 0:
		return "not run: " + budgetReservedText
	case baseTestSubCap-r.spent <= 0:
		return fmt.Sprintf("not run: the %d s time limit of this stage was used up", int(baseTestSubCap/time.Second))
	}
	return ""
}

// exec records one run of the stage with what remains of the sub-cap as its
// timeout ceiling. Callers check stopReason first, so that timeout is positive.
// The cache key holds the fixed sub-cap, not the remainder, so that it does
// not depend on how long the stage's earlier runs took (runOptions.remaining).
func (r *baseTestRun) exec(kind, dir string, command []string, live bool) model.Check {
	started := time.Now()
	c, _, _ := r.h.runWithOptions(r.ctx, kind, dir, command, runOptions{timeout: baseTestSubCap, remaining: baseTestSubCap - r.spent, ceiling: r.ceiling, live: live})
	if !c.Replayed() {
		r.spent += time.Since(started)
	}
	if c.Status == "SKIPPED" {
		if r.skipped == "" {
			r.skipped = c.Output
		}
	} else {
		r.executed = true
	}
	return c
}

// baseTestVerdict is the classification of one test name and the run pair it
// rests on.
type baseTestVerdict struct {
	status, reason string
	base, hybrid   model.Check
}

// unit runs one unit and records evidence for every test that received a run
// pair; the others are marked UNVERIFIED with their reason. When the hybrid run
// failed as a whole, the tests it records as passed get one more run pair on
// their own: a pass event inside a failed check never supports
// PASSES_ON_CANDIDATE, but the same test passing in a pair of its own does.
// Caller holds h.mu.
func (r *baseTestRun) unit(u baseTestUnit, tests []model.BaseTest) {
	h := r.h
	started := time.Now()
	checks := []string{}
	status := "SKIPPED"
	defer func() {
		h.auditBaseTests(started, map[string]any{"dir": u.dir, "tests": len(u.names), "checks": checks}, status)
	}()
	record := func(c model.Check) model.Check {
		checks = append(checks, c.ID)
		status = c.Status
		return c
	}
	verdicts, marks := r.pair(u, u.names, true, record)
	var retry []string
	for _, n := range u.names {
		v, ok := verdicts[n]
		if !ok || v.status != model.StatusUnverified || !baseTestCompletedFail(v.hybrid) {
			continue
		}
		if action, _ := GoTestOutcome(v.hybrid.Output, n); action == "pass" {
			retry = append(retry, n)
		}
	}
	switch {
	case len(retry) == 0:
	case len(retry) == len(verdicts):
		// A pair of its own would repeat the failed run: it held only this
		// test, or every test in it passed, so the run failed for a reason
		// outside these tests (for example the exit code of TestMain).
		why := "no run pair of its own was attempted because the failed run held only this test"
		if len(verdicts) > 1 {
			why = "no run pair of its own was attempted because every test of the failed run passed in it"
		}
		for _, n := range retry {
			v := verdicts[n]
			v.reason = baseTestPassedInsideFailure + "; " + why
			verdicts[n] = v
		}
	default:
		// A retry that gives no result keeps the first pair's evidence, with a
		// reason that says why no result was drawn.
		again, againMarks := map[string]baseTestVerdict{}, map[string]string{}
		if reason := r.stopReason(); reason != "" {
			for _, n := range retry {
				againMarks[n] = reason
			}
		} else {
			again, againMarks = r.pair(u, retry, false, record)
		}
		for _, n := range retry {
			if v, ok := again[n]; ok {
				verdicts[n] = v
				continue
			}
			why := strings.TrimPrefix(againMarks[n], "not run: ")
			if why == "" {
				why = "no reason was recorded"
			}
			v := verdicts[n]
			v.reason = baseTestPassedInsideFailure + "; its own run pair gave no result (" + why + ")"
			verdicts[n] = v
		}
	}
	for _, i := range u.items {
		name := tests[i].Name
		v, ok := verdicts[name]
		if !ok {
			reason := marks[name]
			if reason == "" {
				reason = "not run: the stage stopped before this test ran"
			}
			markBaseTest(tests, i, reason)
			continue
		}
		e := h.appendEvidence(model.Evidence{
			Kind:        model.EvidenceBaseTestDifferential,
			Description: baseTestDescription(tests[i], u.dir),
			Path:        tests[i].Path,
			CheckID:     v.hybrid.ID,
			BaseCheckID: v.base.ID,
			Status:      v.status,
			Runner:      RunnerGo,
			TestNames:   []string{name},
		})
		tests[i].EvidenceID, tests[i].Status, tests[i].Reason = e.ID, v.status, ""
		if v.status == model.StatusUnverified {
			tests[i].Reason = v.reason
		}
	}
}

// baseTestCompletedFail reports whether c is a completed FAIL: exit code
// 1..124 and a complete log, the only failed run whose events can be read.
func baseTestCompletedFail(c model.Check) bool {
	return c.Status == "FAIL" && c.ExitCode >= 1 && c.ExitCode <= 124 && !c.Truncated
}

// pair runs one baseline run for names and, when it passed, one hybrid run
// with the identical command, and classifies each name. With narrowBase, a
// baseline run that failed as a whole is repeated once for the names it
// records as passed. A replayed baseline that would support
// FAILS_ON_CANDIDATE is repeated live (§1.11) and those names are classified
// against the live run. Names that got no hybrid run are returned in marks
// with their reason. Caller holds h.mu.
func (r *baseTestRun) pair(u baseTestUnit, names []string, narrowBase bool, record func(model.Check) model.Check) (map[string]baseTestVerdict, map[string]string) {
	h := r.h
	verdicts, marks := map[string]baseTestVerdict{}, map[string]string{}
	markAll := func(set []string, reason func(name string) string) {
		for _, n := range set {
			marks[n] = reason(n)
		}
	}
	if reason := r.stopReason(); reason != "" {
		markAll(names, func(string) string { return reason })
		return verdicts, marks
	}
	command := selectGoTests(h.testCommand(u.target), names)
	base := record(r.exec(model.CheckBaseTestBase, h.base, command, false))
	if narrowBase && baseTestCompletedFail(base) {
		var passed, other []string
		for _, n := range names {
			if action, _ := GoTestOutcome(base.Output, n); action == "pass" {
				passed = append(passed, n)
			} else {
				other = append(other, n)
			}
		}
		if len(passed) > 0 && len(other) > 0 {
			failed := base
			markAll(other, func(n string) string { return baseTestBaselineReason(failed, n) })
			if reason := r.stopReason(); reason != "" {
				markAll(passed, func(string) string { return reason })
				return verdicts, marks
			}
			names = passed
			command = selectGoTests(h.testCommand(u.target), names)
			base = record(r.exec(model.CheckBaseTestBase, h.base, command, false))
		}
	}
	if base.Status != "PASS" || base.ExitCode != 0 || base.Truncated {
		markAll(names, func(n string) string { return baseTestBaselineReason(base, n) })
		return verdicts, marks
	}
	// A test that did not pass on the baseline can only end UNVERIFIED, so a
	// hybrid run in which no selected test can get a result is not started.
	// A unit with at least one passing test keeps the identical command.
	anyPassed := false
	for _, n := range names {
		if action, _ := GoTestOutcome(base.Output, n); action == "pass" {
			anyPassed = true
			break
		}
	}
	if !anyPassed {
		markAll(names, func(n string) string { return baseTestBaselineReason(base, n) })
		return verdicts, marks
	}
	if reason := r.stopReason(); reason != "" {
		markAll(names, func(string) string { return reason })
		return verdicts, marks
	}
	hybrid := record(r.exec(model.CheckBaseTestHybrid, r.hybrid, command, false))
	confirm := false
	for _, n := range names {
		s, reason := ClassifyExistingTest(base, hybrid, n)
		verdicts[n] = baseTestVerdict{s, reason, base, hybrid}
		confirm = confirm || s == model.StatusFailsOnCandidate && base.Replayed()
	}
	if !confirm {
		return verdicts, marks
	}
	// §1.11: a replayed baseline never supports FAILS_ON_CANDIDATE. The
	// baseline runs again live with the same kind and command; the replayed
	// check stays in the ledger, and a live run that does not reproduce it
	// evicts the entry it was replayed from.
	reason := r.stopReason()
	var live model.Check
	if reason == "" {
		live = h.settleReplayedGoBaseline(base, record(r.exec(model.CheckBaseTestBase, h.base, command, true)), names)
	}
	for _, n := range names {
		if verdicts[n].status != model.StatusFailsOnCandidate {
			continue
		}
		if reason != "" {
			verdicts[n] = baseTestVerdict{model.StatusUnverified, "the baseline run was replayed from the execution cache and could not be repeated live (" + strings.TrimPrefix(reason, "not run: ") + ")", base, hybrid}
			continue
		}
		s, why := ClassifyExistingTest(live, hybrid, n)
		verdicts[n] = baseTestVerdict{s, why, live, hybrid}
	}
	return verdicts, marks
}

// baseTestBaselineReason says why a test got no candidate-side run after the
// baseline run base.
func baseTestBaselineReason(base model.Check, name string) string {
	const tail = ", so it was not run on candidate code"
	switch {
	case base.Status == "SKIPPED":
		return fmt.Sprintf("the baseline run %s did not start (%s)%s", base.ID, strings.TrimSuffix(strings.TrimSpace(base.Output), "."), tail)
	case base.Status == "TIMEOUT":
		return fmt.Sprintf("the baseline run %s timed out%s", base.ID, tail)
	case base.Status == "ERROR":
		return fmt.Sprintf("the baseline run %s did not complete (ERROR)%s", base.ID, tail)
	case base.Truncated:
		return fmt.Sprintf("the baseline log of %s was truncated (raise sandbox.max_output_bytes)%s", base.ID, tail)
	}
	switch action, _ := GoTestOutcome(base.Output, name); action {
	case "fail":
		return fmt.Sprintf("the test failed on the baseline tree (%s)%s", base.ID, tail)
	case "skip":
		return fmt.Sprintf("the test was skipped on the baseline tree (%s)%s", base.ID, tail)
	case "pass":
		return fmt.Sprintf("the baseline run %s failed although this test passed in it%s", base.ID, tail)
	}
	if base.Status == "FAIL" && goBuildFailure(base.Output) {
		return fmt.Sprintf("the baseline package did not build or set up in %s%s", base.ID, tail)
	}
	return fmt.Sprintf("the baseline log of %s does not record exactly one run and one result of this test (for example a build constraint excluded its file)%s", base.ID, tail)
}

// baseTestDescription is the evidence description of one test.
func baseTestDescription(t model.BaseTest, dir string) string {
	where := "of " + dir
	if dir == "." {
		where = "of the repository root"
	}
	return truncateUTF8(Redact(fmt.Sprintf("Baseline version of %s from %s (%s), run on the baseline tree and on the candidate tree with the *_test.go files and testdata %s reverted to the baseline.", t.Name, t.Path, BaseTestChangeText(t.Change), where)), 1024)
}

// BaseTestChangeText describes a base_tests change class in words.
func BaseTestChangeText(change string) string {
	switch change {
	case model.BaseTestRemoved:
		return "removed by the change"
	case model.BaseTestModified:
		return "modified by the change"
	case model.BaseTestSharedCodeChanged:
		return "its file changed outside the test function"
	case model.BaseTestFileDeleted:
		return "its file was deleted or renamed to a non-test file"
	}
	return "changed"
}
