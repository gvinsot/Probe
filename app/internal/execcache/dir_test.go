package execcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateDirRejectsRepositoryAndOutputLocations(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	output := filepath.Join(repo, ".swiftproof")
	external := filepath.Join(root, "reports")
	for _, dir := range []string{repo, external} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct{ dir, output string }{
		"the repository itself":           {repo, output},
		"inside the repository":           {filepath.Join(repo, ".cache", "swiftproof"), output},
		"inside the default output":       {filepath.Join(output, "cache"), output},
		"the output directory itself":     {external, external},
		"inside an external output":       {filepath.Join(external, "cache"), external},
		"containing the repository":       {root, filepath.Join(root, "elsewhere")},
		"containing the output":           {filepath.Join(root, "x"), filepath.Join(root, "x", "report")},
		"relative path into the repo":     {relative(t, filepath.Join(repo, "c")), output},
		"dot-dot path back into the repo": {filepath.Join(root, "other", "..", "repo", "c"), output},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateDir(tc.dir, repo, tc.output)
			if !errors.Is(err, ErrLocation) {
				t.Fatalf("accepted %s: %v", tc.dir, err)
			}
		})
	}
	if runtime.GOOS == "windows" {
		// Windows paths are case-insensitive: another spelling is the same place.
		if _, err := ValidateDir(filepath.Join(strings.ToUpper(repo), "cache"), repo, output); !errors.Is(err, ErrLocation) {
			t.Fatalf("a case variant of the repository was accepted: %v", err)
		}
	}
	for _, bad := range []string{"", "   ", "a\x00b"} {
		if _, err := ValidateDir(bad, repo, output); !errors.Is(err, ErrLocation) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func relative(t *testing.T, path string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil {
		t.Skipf("no relative path on another volume: %v", err)
	}
	return rel
}

func TestValidateDirCreatesAnOwnerOnlyDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "cache")
	got, err := ValidateDir(dir, filepath.Join(root, "repo"), filepath.Join(root, "repo", ".swiftproof"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("mode %v, want 0700", info.Mode().Perm())
	}
	want, _ := filepath.EvalSymlinks(dir)
	if !within(want, got) || !within(got, want) {
		t.Fatalf("canonical path %s, want %s", got, want)
	}
	// An existing, valid directory is accepted again.
	if _, err := ValidateDir(dir, filepath.Join(root, "repo"), ""); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDirRejectsFilesAndLinks(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0700)
	file := filepath.Join(root, "file")
	os.WriteFile(file, []byte("x"), 0600)
	if _, err := ValidateDir(file, repo, ""); !errors.Is(err, ErrLocation) {
		t.Fatalf("a file was accepted: %v", err)
	}
	real := filepath.Join(root, "real")
	os.MkdirAll(real, 0700)
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ValidateDir(link, repo, ""); !errors.Is(err, ErrLocation) || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a symlinked cache directory was accepted: %v", err)
	}
	// A symlinked ancestor is resolved before the location check: a path that
	// looks external but resolves into the repository is refused.
	intoRepo := filepath.Join(root, "alias")
	if err := os.Symlink(repo, intoRepo); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ValidateDir(filepath.Join(intoRepo, "cache"), repo, ""); !errors.Is(err, ErrLocation) {
		t.Fatalf("a path resolving into the repository was accepted: %v", err)
	}
}

// junction creates a Windows directory junction (mount point) at link that
// points to target, or skips the test.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions exist on Windows only")
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v %s", err, out)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
		t.Skipf("mklink /J did not create a reparse point: %v", err)
	}
}

// Junctions are not followed by filepath.EvalSymlinks since Go 1.23: the
// location rule must still see through a junction ancestor, and a cache below
// a junction to an external directory must stay usable on every later run.
func TestValidateDirResolvesJunctionAncestors(t *testing.T) {
	root := t.TempDir()
	repo, outside, output := filepath.Join(root, "repo"), filepath.Join(root, "outside"), filepath.Join(root, "repo", ".swiftproof")
	for _, dir := range []string{filepath.Join(repo, "sub"), filepath.Join(outside, "a", "report")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	intoRepo, external := filepath.Join(root, "j"), filepath.Join(root, "k")
	junction(t, intoRepo, repo)
	junction(t, external, outside)

	// (a) Into the repository through a junction: refused before anything is
	// created, whether or not the path continues below the junction.
	for _, dir := range []string{filepath.Join(intoRepo, "newcache"), filepath.Join(intoRepo, "sub", "cache"), filepath.Join(intoRepo, "x", "y")} {
		if _, err := ValidateDir(dir, repo, output); !errors.Is(err, ErrLocation) || !strings.Contains(err.Error(), "must be outside the repository") {
			t.Errorf("%s was accepted or refused for another reason: %v", dir, err)
		}
	}
	for _, created := range []string{filepath.Join(repo, "newcache"), filepath.Join(repo, "sub", "cache"), filepath.Join(repo, "x")} {
		if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused directory was created inside the repository: %s (%v)", created, err)
		}
	}
	// A repository reached through a junction is compared by what it resolves to.
	if _, err := ValidateDir(filepath.Join(repo, "cache"), intoRepo, ""); !errors.Is(err, ErrLocation) {
		t.Errorf("a cache inside a repository named through a junction was accepted: %v", err)
	}

	// (b) Below a junction to an external directory: accepted on first use
	// and again once the directory exists, with the resolved path.
	for _, dir := range []string{filepath.Join(external, "cache"), filepath.Join(external, "a", "b", "cache")} {
		first, err := ValidateDir(dir, repo, output)
		if err != nil {
			t.Fatalf("first use of %s: %v", dir, err)
		}
		second, err := ValidateDir(dir, repo, output)
		if err != nil {
			t.Fatalf("second use of %s: %v", dir, err)
		}
		if first != second || !within(normalize(outside), first) {
			t.Fatalf("canonical paths %s and %s, want under %s", first, second, outside)
		}
		if s, err := Open(dir, Options{RepoRoot: repo, OutputDir: output}); err != nil || s.DisabledReason() != "" || s.Dir() != first {
			t.Fatalf("open %s: %v", dir, err)
		}
	}

	// (c) An output directory below a junction is resolved as well: a cache
	// inside what it resolves to is refused, one elsewhere is accepted.
	report := filepath.Join(external, "a", "report")
	if _, err := ValidateDir(filepath.Join(root, "elsewhere"), repo, report); err != nil {
		t.Fatalf("an output directory below a junction broke validation: %v", err)
	}
	if _, err := ValidateDir(filepath.Join(outside, "a", "report", "cache"), repo, report); !errors.Is(err, ErrLocation) {
		t.Fatalf("a cache inside the resolved output directory was accepted: %v", err)
	}

	// (d) The directory itself must not be a junction.
	if _, err := ValidateDir(external, repo, output); !errors.Is(err, ErrLocation) || !strings.Contains(err.Error(), "link or reparse point") {
		t.Fatalf("a junction as the cache directory was accepted: %v", err)
	}
}

// Symlinked ancestors, relative link targets, chains and cycles.
func TestValidateDirResolvesSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	repo, outside := filepath.Join(root, "repo"), filepath.Join(root, "outside")
	os.MkdirAll(repo, 0700)
	os.MkdirAll(outside, 0700)
	external := filepath.Join(root, "ext")
	if err := os.Symlink(outside, external); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for i := 0; i < 2; i++ { // first use, then reuse of the existing directory
		got, err := ValidateDir(filepath.Join(external, "cache"), repo, "")
		if err != nil {
			t.Fatalf("use %d of a cache below an external symlink: %v", i+1, err)
		}
		if !within(normalize(outside), got) {
			t.Fatalf("canonical path %s is not under %s", got, outside)
		}
	}
	relative := filepath.Join(root, "rel")
	if err := os.Symlink("repo", relative); err != nil {
		t.Skipf("relative symlinks unavailable: %v", err)
	}
	chain := filepath.Join(root, "chain")
	os.Symlink(relative, chain)
	for _, dir := range []string{filepath.Join(relative, "cache"), filepath.Join(chain, "c", "d")} {
		if _, err := ValidateDir(dir, repo, ""); !errors.Is(err, ErrLocation) {
			t.Errorf("%s resolves into the repository but was accepted: %v", dir, err)
		}
	}
	loopA, loopB := filepath.Join(root, "loopA"), filepath.Join(root, "loopB")
	os.Symlink(loopB, loopA)
	os.Symlink(loopA, loopB)
	if _, err := ValidateDir(filepath.Join(loopA, "cache"), repo, ""); !errors.Is(err, ErrLocation) {
		t.Fatalf("a link cycle was accepted: %v", err)
	}
}

// File identity catches a spelling the path resolution leaves as is.
func TestSameOrInside(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, "a", "b"), 0700)
	os.MkdirAll(filepath.Join(root, "other"), 0700)
	for path, want := range map[string]bool{
		repo:                                   true,
		filepath.Join(repo, "a", "b"):          true,
		filepath.Join(repo, "a", "missing"):    true,
		filepath.Join(root, "other"):           false,
		filepath.Join(root, "other", "x", "y"): false,
		root:                                   false,
	} {
		if got := sameOrInside(repo, path); got != want {
			t.Errorf("sameOrInside(repo, %s) = %v", path, got)
		}
	}
	if sameOrInside(filepath.Join(root, "missing"), filepath.Join(root, "missing", "x")) {
		t.Error("a missing parent matched")
	}
	// Another spelling of the same directory (on Windows, an 8.3 short name
	// in the temporary directory's path) is recognized both ways.
	if long := normalize(repo); !strings.EqualFold(long, repo) {
		if !sameOrInside(long, filepath.Join(repo, "a")) || !sameOrInside(repo, filepath.Join(long, "a")) {
			t.Errorf("the spellings %s and %s were not recognized as one directory", repo, long)
		}
		if _, err := ValidateDir(filepath.Join(repo, "cache"), long, ""); !errors.Is(err, ErrLocation) {
			t.Errorf("a cache under another spelling of the repository was accepted: %v", err)
		}
	} else {
		t.Logf("the temporary directory has one spelling (%s); the short-name case did not run", repo)
	}
}

func TestValidateDirRequiresOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ownership and permission bits are not checked on Windows (documented)")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	os.Mkdir(dir, 0700)
	for _, mode := range []os.FileMode{0770, 0707, 0750, 0705} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateDir(dir, filepath.Join(root, "repo"), ""); !errors.Is(err, ErrLocation) {
			t.Errorf("mode %04o accepted", mode)
		}
	}
	os.Chmod(dir, 0700)
	if _, err := ValidateDir(dir, filepath.Join(root, "repo"), ""); err != nil {
		t.Fatalf("mode 0700 refused: %v", err)
	}
}

func TestOpenRefusesInvalidLocations(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, ".cache", layoutDir), 0700)
	if _, err := Open(filepath.Join(repo, ".cache"), Options{RepoRoot: repo}); !errors.Is(err, ErrLocation) {
		t.Fatalf("a cache directory inside the repository was opened: %v", err)
	}
}
