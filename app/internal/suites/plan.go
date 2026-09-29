// Package suites selects the Go test functions whose baseline version runs on
// candidate code (probe review --base-tests). Selection is static: it
// parses committed blobs with go/parser and never executes repository code.
package suites

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Reader reads one committed file. (*gitrepo.Repository).ReadFile satisfies
// it; the caller maps its size-limit error to ErrTooLarge.
type Reader func(ctx context.Context, commit, path string) ([]byte, error)

// ErrTooLarge is what a Reader returns for a file over its read limit.
var ErrTooLarge = errors.New("file exceeds the read limit")

// Planning limits. Every overflow is reported as a note, never dropped silently.
const (
	MaxTestFiles     = 50  // changed Go test files analyzed
	MaxSelectedTests = 100 // selected test functions
)

// Selection is the result of Plan: the selected tests, sorted by (Path, Line,
// Name), with Status UNVERIFIED until they run, and the notes that name what
// could not be analyzed. Notes are Unverified entries.
type Selection struct {
	Tests []model.BaseTest
	Notes []string
}

// notePrefix starts every planning note, so a reader knows which stage it is from.
const notePrefix = "Baseline versions of changed tests: "

// Plan selects every runnable Go test function of a changed Go test file whose
// baseline version the change modified or removed, and every test of a file
// that changed outside its test functions:
//   - removed: the candidate file no longer declares it (deleted, renamed, moved
//     or no longer runnable);
//   - modified: its token digest differs, so comment and layout edits are
//     ignored, or the candidate file could not be read or parsed;
//   - shared_code_changed: a baseline declaration of the file other than a test
//     disappeared or changed; the package clause, the build constraints, the
//     import set or a compiler directive changed; the candidate added init,
//     TestMain, a package-level variable with an initializer or a method of a
//     type it did not newly declare; or a rename moved the file to another
//     directory (another package) or changed the GOOS/GOARCH suffix of its
//     name;
//   - file_deleted: the file was deleted, or renamed to a non-test file.
//
// Added, copied and type-changed files, files Go never compiles (under
// testdata, vendor, or a directory or name starting with _ or .) and binary
// changes are not considered, and a change in one file never selects the
// tests of another. Plan returns an error only when ctx ends.
func Plan(ctx context.Context, read Reader, change model.Change) (Selection, error) {
	type candidate struct{ basePath, candPath string }
	var files []candidate
	for _, f := range change.Files {
		if basePath, candPath, ok := goTestPaths(f); ok {
			files = append(files, candidate{basePath, candPath})
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].basePath < files[j].basePath })
	var sel Selection
	if len(files) > MaxTestFiles {
		sel.Notes = append(sel.Notes, fmt.Sprintf("%s%d changed Go test files beyond the limit of %d were not analyzed, so their tests were not re-run.", notePrefix, len(files)-MaxTestFiles, MaxTestFiles))
		files = files[:MaxTestFiles]
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Selection{}, err
		}
		src, err := read(ctx, change.BaseCommit, f.basePath)
		if err != nil {
			if ctx.Err() != nil {
				return Selection{}, ctx.Err()
			}
			sel.Notes = append(sel.Notes, readNote(f.basePath, err))
			continue
		}
		base, err := parseTestFile(f.basePath, src)
		if err != nil {
			sel.Notes = append(sel.Notes, fmt.Sprintf("%sthe baseline version of %s could not be parsed as Go, so its tests were not re-run.", notePrefix, f.basePath))
			continue
		}
		var cand testFile
		var candErr error
		changed := false
		if f.candPath != "" {
			src, err := read(ctx, change.HeadCommit, f.candPath)
			if err != nil {
				if ctx.Err() != nil {
					return Selection{}, ctx.Err()
				}
				candErr = err
			} else {
				cand, candErr = parseTestFile(f.candPath, src)
			}
			changed = candErr == nil && (fileChanged(base, cand) || renameChanged(f.basePath, f.candPath))
		}
		for _, t := range base.tests {
			kind := classifyChange(f.candPath, candErr, cand, t, changed)
			if kind == "" {
				continue
			}
			bt := model.BaseTest{Name: t.name, Path: f.basePath, Line: t.line, EndLine: t.endLine, Change: kind, Status: model.StatusUnverified}
			if f.candPath != "" && candErr == nil {
				if c, ok := cand.test(t.name); ok {
					bt.CandidatePath, bt.CandidateLine, bt.CandidateEndLine = f.candPath, c.line, c.endLine
				}
			}
			sel.Tests = append(sel.Tests, bt)
		}
	}
	sort.SliceStable(sel.Tests, func(i, j int) bool {
		a, b := sel.Tests[i], sel.Tests[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
	if len(sel.Tests) > MaxSelectedTests {
		sel.Notes = append(sel.Notes, fmt.Sprintf("%s%d selected tests beyond the limit of %d were not re-run.", notePrefix, len(sel.Tests)-MaxSelectedTests, MaxSelectedTests))
		sel.Tests = sel.Tests[:MaxSelectedTests]
	}
	return sel, nil
}

// readNote is the note for a baseline file that could not be read.
func readNote(p string, err error) string {
	if errors.Is(err, ErrTooLarge) {
		return fmt.Sprintf("%sthe baseline version of %s exceeds the read limit, so its tests were not re-run.", notePrefix, p)
	}
	return fmt.Sprintf("%sthe baseline version of %s could not be read, so its tests were not re-run.", notePrefix, p)
}

// classifyChange returns the change class of one baseline test, or "" when the
// test is not selected. fileChanged says whether the file changed outside its
// test functions (see Plan).
func classifyChange(candPath string, candErr error, cand testFile, t testFunc, fileChanged bool) string {
	switch {
	case candPath == "":
		return model.BaseTestFileDeleted
	case candErr != nil:
		return model.BaseTestModified
	}
	c, ok := cand.test(t.name)
	switch {
	case !ok:
		return model.BaseTestRemoved
	case c.digest != t.digest:
		return model.BaseTestModified
	case fileChanged:
		return model.BaseTestSharedCodeChanged
	}
	return ""
}

// knownOS and knownArch are the GOOS and GOARCH values go/build matches in
// file names (internal/syslist of Go 1.26).
var (
	knownOS   = map[string]bool{"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true}
	knownArch = map[string]bool{"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true, "arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true}
)

// renameChanged reports whether renaming a test file from basePath to
// candPath changes whether or where go test compiles it: another directory is
// another package, and a GOOS/GOARCH suffix is an implicit build constraint.
func renameChanged(basePath, candPath string) bool {
	return path.Dir(basePath) != path.Dir(candPath) || fileNameTags(basePath) != fileNameTags(candPath)
}

// fileNameTags returns the implicit build constraint of a Go file name, as
// go/build derives it: "GOOS_GOARCH", "GOOS" or "GOARCH" from the suffix
// before _test.go, or "" when the name carries none.
func fileNameTags(p string) string {
	name, _, _ := strings.Cut(path.Base(p), ".")
	i := strings.Index(name, "_")
	if i < 0 {
		return ""
	}
	l := strings.Split(name[i:], "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return l[n-2] + "_" + l[n-1]
	}
	if n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
		return l[n-1]
	}
	return ""
}

// goTestPaths returns the baseline path of a changed Go test file and its
// candidate path ("" when the candidate has no test file there).
func goTestPaths(f model.ChangedFile) (basePath, candPath string, ok bool) {
	if f.Binary {
		return "", "", false
	}
	switch f.Status {
	case "M":
		if isGoTestFile(f.Path) {
			return f.Path, f.Path, true
		}
	case "D":
		if isGoTestFile(f.Path) {
			return f.Path, "", true
		}
	case "R":
		if isGoTestFile(f.OldPath) {
			if isGoTestFile(f.Path) {
				return f.OldPath, f.Path, true
			}
			return f.OldPath, "", true
		}
	}
	return "", "", false
}

// isGoTestFile reports whether go test compiles p as a test file of its
// directory's package: a *_test.go name, not under testdata or vendor, and no
// directory or file name starting with _ or ".".
func isGoTestFile(p string) bool {
	if p == "" || !strings.HasSuffix(p, "_test.go") || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "testdata" || part == "vendor" || strings.HasPrefix(part, "_") || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
