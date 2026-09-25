//go:build unix

package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A restrictive umask cannot make the export unreadable to the container
// user: directories are 0755 and files 0644 or 0755.
func TestExportMatchingModesIgnoreUmask(t *testing.T) {
	dir, r := newTestRepo(t)
	writeTest(t, dir, "nested/deep/go.mod", "module x\n")
	writeTest(t, dir, "run.sh", "#!/bin/sh\n")
	gitTest(t, dir, "", "add", "-A")
	gitTest(t, dir, "", "update-index", "--chmod=+x", "run.sh")
	commit := commitIndex(t, dir)
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	dest := filepath.Join(t.TempDir(), "inputs")
	if _, err := r.ExportMatching(context.Background(), commit, dest, matchAny("nested/deep/go.mod", "run.sh"), exportTestLimits); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{".": 0755, "nested": 0755, "nested/deep": 0755, "nested/deep/go.mod": 0644, "run.sh": 0755} {
		info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", p, info.Mode().Perm(), want)
		}
	}
}
