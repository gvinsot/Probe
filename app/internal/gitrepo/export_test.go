package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func matchAny(patterns ...string) func(string) bool {
	return func(p string) bool {
		for _, pattern := range patterns {
			if ok, _ := path.Match(pattern, p); ok {
				return true
			}
		}
		return false
	}
}

var exportTestLimits = ExportLimits{MaxFiles: 16, MaxFileBytes: 1 << 20, MaxTotalBytes: 4 << 20}

// The export writes exact blobs (no checkout filter, no end-of-line
// conversion, no export attribute), only the matched regular files, sorted,
// with the blob SHA-256, size and executable bit.
func TestExportMatchingExactBlobs(t *testing.T) {
	dir, r := newTestRepo(t)
	files := map[string]string{
		"go.sum":            "a v1.0.0 h1:x=\nb v2.0.0 h1:y=\n",
		"go.mod":            "module example.test/x\n\ngo 1.23\n",
		"app/go.mod":        "module example.test/app\r\n",
		"app/go.sum":        "",
		"app/main.go":       "package main\n",
		"tools/install.sh":  "#!/bin/sh\necho install\n",
		".gitattributes":    "go.sum text eol=crlf filter=never-run\ngo.mod export-ignore\n",
		"vendor/x/go.mod":   "module vendored\n",
		"docs/unmatched.md": "not exported\n",
	}
	for p, content := range files {
		writeTest(t, dir, p, content)
	}
	gitTest(t, dir, "", "add", "-A")
	gitTest(t, dir, "", "update-index", "--chmod=+x", "tools/install.sh")
	commit := commitIndex(t, dir)
	// A checkout of this repository would convert go.sum to CRLF.
	gitTest(t, dir, "", "config", "core.autocrlf", "true")
	dest := filepath.Join(t.TempDir(), "inputs")
	got, err := r.ExportMatching(context.Background(), commit, dest, matchAny("go.mod", "go.sum", "app/go.*", "tools/*.sh"), exportTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
		blob, err := exec.Command("git", "-C", dir, "cat-file", "blob", commit+":"+f.Path).Output()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(blob)
		if string(raw) != string(blob) || hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(blob)) != f.Size {
			t.Errorf("%s: recorded %s/%d, file %q, blob %q", f.Path, f.SHA256, f.Size, raw, blob)
		}
		if string(raw) != files[f.Path] {
			t.Errorf("%s: exported %q, committed %q", f.Path, raw, files[f.Path])
		}
		if f.Executable != (f.Path == "tools/install.sh") {
			t.Errorf("%s: executable %v", f.Path, f.Executable)
		}
	}
	if strings.Join(paths, ",") != "app/go.mod,app/go.sum,go.mod,go.sum,tools/install.sh" {
		t.Fatalf("exported %v", paths)
	}
	for _, unexpected := range []string{"docs", "vendor", "app/main.go", ".gitattributes", ".git"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(unexpected))); !os.IsNotExist(err) {
			t.Errorf("%s exported: %v", unexpected, err)
		}
	}
}

func TestExportMatchingRefusesMatchedSymlinkAndSubmodule(t *testing.T) {
	_, r, _, special := treeFixture(t)
	for _, pattern := range []string{"link", "vendor/sub"} {
		dest := filepath.Join(t.TempDir(), "inputs")
		_, err := r.ExportMatching(context.Background(), special, dest, matchAny(pattern), exportTestLimits)
		if err == nil || !strings.Contains(err.Error(), "only regular files are exported") {
			t.Fatalf("%s: %v", pattern, err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatalf("%s: a refused export left its destination behind", pattern)
		}
	}
}

func TestExportMatchingIgnoresUnmatchedSymlinkAndSubmodule(t *testing.T) {
	_, r, _, special := treeFixture(t)
	dest := filepath.Join(t.TempDir(), "inputs")
	got, err := r.ExportMatching(context.Background(), special, dest, matchAny("README.md"), exportTestLimits)
	if err != nil || len(got) != 1 || got[0].Path != "README.md" {
		t.Fatalf("export %+v %v", got, err)
	}
}

// Every limit is checked against the tree listing before any file is
// written, and a failed export leaves nothing behind.
func TestExportMatchingLimits(t *testing.T) {
	dir, r := newTestRepo(t)
	writeTest(t, dir, "a.lock", strings.Repeat("a", 100))
	writeTest(t, dir, "b.lock", strings.Repeat("b", 100))
	writeTest(t, dir, "c.lock", strings.Repeat("c", 100))
	commit := commitTest(t, dir)
	for name, limits := range map[string]ExportLimits{
		"files": {MaxFiles: 2, MaxFileBytes: 1000, MaxTotalBytes: 1000},
		"file":  {MaxFiles: 10, MaxFileBytes: 99, MaxTotalBytes: 1000},
		"total": {MaxFiles: 10, MaxFileBytes: 1000, MaxTotalBytes: 299},
	} {
		dest := filepath.Join(t.TempDir(), "inputs")
		got, err := r.ExportMatching(context.Background(), commit, dest, matchAny("*.lock"), limits)
		if !errors.Is(err, ErrLimit) || got != nil {
			t.Fatalf("%s: %v %v", name, got, err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatalf("%s: partial export left behind", name)
		}
	}
	dest := filepath.Join(t.TempDir(), "inputs")
	if got, err := r.ExportMatching(context.Background(), commit, dest, matchAny("*.lock"), ExportLimits{MaxFiles: 3, MaxFileBytes: 100, MaxTotalBytes: 300}); err != nil || len(got) != 3 {
		t.Fatalf("limits at the exact bound: %v %v", got, err)
	}
}

func TestExportMatchingDestinationAndArguments(t *testing.T) {
	dir, r := newTestRepo(t)
	writeTest(t, dir, "go.mod", "module x\n")
	commit := commitTest(t, dir)
	ctx := context.Background()
	full := filepath.Join(t.TempDir(), "full")
	writeTest(t, full, "existing", "x")
	if _, err := r.ExportMatching(ctx, commit, full, matchAny("go.mod"), exportTestLimits); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("non-empty destination: %v", err)
	}
	empty := t.TempDir()
	if got, err := r.ExportMatching(ctx, commit, empty, matchAny("go.mod"), exportTestLimits); err != nil || len(got) != 1 {
		t.Fatalf("empty existing destination: %v %v", got, err)
	}
	for name, call := range map[string]func() error{
		"branch name": func() error {
			_, err := r.ExportMatching(ctx, "main", filepath.Join(t.TempDir(), "x"), matchAny("go.mod"), exportTestLimits)
			return err
		},
		"nil matcher": func() error {
			_, err := r.ExportMatching(ctx, commit, filepath.Join(t.TempDir(), "x"), nil, exportTestLimits)
			return err
		},
		"zero files": func() error {
			_, err := r.ExportMatching(ctx, commit, filepath.Join(t.TempDir(), "x"), matchAny("go.mod"), ExportLimits{})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// No match is not an error here: the caller decides.
	if got, err := r.ExportMatching(ctx, commit, filepath.Join(t.TempDir(), "none"), matchAny("package.json"), exportTestLimits); err != nil || len(got) != 0 {
		t.Fatalf("no match: %v %v", got, err)
	}
}

func TestExportMatchingRejectsSymlinkAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	dir, r := newTestRepo(t)
	writeTest(t, dir, "go.mod", "module x\n")
	commit := commitTest(t, dir)
	target, parent := t.TempDir(), t.TempDir()
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExportMatching(context.Background(), commit, filepath.Join(link, "inputs"), matchAny("go.mod"), exportTestLimits); err == nil || !strings.Contains(err.Error(), "symlink ancestor") {
		t.Fatalf("symlink ancestor: %v", err)
	}
}
