package execcache

import (
	"errors"
	"os"
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
