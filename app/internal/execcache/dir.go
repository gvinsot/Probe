package execcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrLocation marks a cache directory that violates the location, link or
// ownership rules. The CLI exits 3 on it before any container starts.
var ErrLocation = errors.New("invalid execution cache directory")

// ValidateDir checks an explicit --cache-dir and returns its canonical path.
// Every rule is checked before anything executes:
//   - after resolving the symlinks of its nearest existing ancestor, the
//     directory is neither the repository root or the output directory nor
//     inside either, and neither of them is inside it;
//   - the directory itself is not a symlink, a junction or another reparse
//     point, and is a directory; it is created with mode 0700 when missing;
//   - on Unix it is owned by the effective user and grants no group or other
//     permission. Windows ownership and ACLs are not checked (documented).
//
// Every error wraps ErrLocation.
func ValidateDir(dir, repoRoot, outputDir string) (string, error) {
	if strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("%w: the path is empty or contains NUL", ErrLocation)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLocation, err)
	}
	canonical, err := canonicalPath(absolute)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLocation, err)
	}
	for _, other := range []struct{ name, path string }{{"the repository", repoRoot}, {"the output directory", outputDir}} {
		if other.path == "" {
			continue
		}
		path, err := filepath.Abs(other.path)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrLocation, err)
		}
		if path, err = canonicalPath(path); err != nil {
			return "", fmt.Errorf("%w: %v", ErrLocation, err)
		}
		if within(path, canonical) {
			return "", fmt.Errorf("%w: %s must be outside %s (%s): a checkout or report could otherwise plant entries", ErrLocation, canonical, other.name, path)
		}
		if within(canonical, path) {
			return "", fmt.Errorf("%w: %s must not contain %s (%s)", ErrLocation, canonical, other.name, path)
		}
	}
	info, err := os.Lstat(absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(absolute, 0700); err != nil {
			return "", fmt.Errorf("%w: create %s: %v", ErrLocation, absolute, err)
		}
		if info, err = os.Lstat(absolute); err != nil {
			return "", fmt.Errorf("%w: %v", ErrLocation, err)
		}
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrLocation, err)
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return "", fmt.Errorf("%w: %s is a link or reparse point; name the real directory", ErrLocation, absolute)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrLocation, absolute)
	}
	if err := checkOwner(absolute, info); err != nil {
		return "", fmt.Errorf("%w: %v", ErrLocation, err)
	}
	return canonical, nil
}

// canonicalPath resolves the symlinks of the nearest existing ancestor of an
// absolute path and appends the components that do not exist yet.
func canonicalPath(absolute string) (string, error) {
	existing, rest := filepath.Clean(absolute), []string(nil)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, rest...)...), nil
}

// within reports whether path is parent or inside it. Windows paths compare
// case-insensitively.
func within(parent, path string) bool {
	if runtime.GOOS == "windows" {
		parent, path = strings.ToLower(parent), strings.ToLower(path)
	}
	rel, err := filepath.Rel(parent, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
