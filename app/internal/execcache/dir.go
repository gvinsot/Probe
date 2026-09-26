package execcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrLocation marks a cache directory that violates the location, link or
// ownership rules. The CLI exits 3 on it before any container starts.
var ErrLocation = errors.New("invalid execution cache directory")

// maxLinks bounds how many links canonicalPath follows, so that a link cycle
// ends with an error.
const maxLinks = 255

// ValidateDir checks an explicit --cache-dir and returns its canonical path.
// Every rule is checked before anything executes:
//   - after resolving the links of its nearest existing ancestor (symlinks,
//     and on Windows junctions and other resolvable reparse points), the
//     directory is neither the repository root or the output directory nor
//     inside either, and neither of them is inside it. The comparison is
//     made twice: on the resolved paths, and by file identity (os.SameFile)
//     of the existing directories, so that a spelling the resolution does not
//     normalize (a short name, a substituted drive) cannot hide a location;
//   - the directory itself is not a symlink, a junction or another reparse
//     point, and is a directory; it is created with mode 0700 when missing,
//     and it must then be the very directory its canonical path names;
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
			return "", fmt.Errorf("%w: %s: %v", ErrLocation, other.name, err)
		}
		if within(path, canonical) || sameOrInside(path, canonical) {
			return "", fmt.Errorf("%w: %s must be outside %s (%s): a checkout or report could otherwise plant entries", ErrLocation, canonical, other.name, path)
		}
		if within(canonical, path) || sameOrInside(canonical, path) {
			return "", fmt.Errorf("%w: %s must not contain %s (%s)", ErrLocation, canonical, other.name, path)
		}
	}
	info, err := os.Lstat(absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Created where the checks above looked, not through the links of
		// the path as given.
		if err := os.MkdirAll(canonical, 0700); err != nil {
			return "", fmt.Errorf("%w: create %s: %v", ErrLocation, canonical, err)
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
	// The path as given and the canonical path must name one directory: the
	// store works in the canonical one, and the location checks judged it.
	if resolved, err := os.Stat(canonical); err != nil || !os.SameFile(info, resolved) {
		return "", fmt.Errorf("%w: %s does not resolve to the directory %s; name the real directory", ErrLocation, absolute, canonical)
	}
	if err := checkOwner(absolute, info); err != nil {
		return "", fmt.Errorf("%w: %v", ErrLocation, err)
	}
	return canonical, nil
}

// canonicalPath resolves every link on the existing prefix of an absolute
// path and appends the components that do not exist yet. It walks the path
// one component at a time with os.Lstat and resolves each symlink, and on
// Windows each junction (mount point), with os.Readlink, then continues from
// the link's target. filepath.EvalSymlinks alone is not enough: since Go 1.23
// it leaves Windows junctions unresolved and fails on a path that continues
// below one. A reparse point that is not a directory and that os.Readlink
// cannot resolve is refused. The link-free existing prefix is then normalized
// by filepath.EvalSymlinks (on Windows: letter case and 8.3 short names).
func canonicalPath(absolute string) (string, error) {
	path, links := filepath.Clean(absolute), 0
	for {
		next, done, err := resolveStep(path)
		if err != nil {
			return "", err
		}
		if done {
			return next, nil
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("%s: too many links", absolute)
		}
		path = next
	}
}

// resolveStep walks path from its volume root. When it meets a link it
// returns the path rewritten through the link's target (done false); when no
// link is left it returns the normalized existing prefix joined with the
// missing components (done true).
func resolveStep(path string) (string, bool, error) {
	volume := filepath.VolumeName(path)
	sep := string(filepath.Separator)
	names := strings.Split(strings.TrimPrefix(path[len(volume):], sep), sep)
	resolved := volume + sep
	for i, name := range names {
		if name == "" {
			continue
		}
		next := filepath.Join(resolved, name)
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.Join(append([]string{normalize(resolved)}, names[i:]...)...), true, nil
		}
		if err != nil {
			return "", false, err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if err == nil && info.Mode()&os.ModeSymlink == 0 && !filepath.IsAbs(target) {
			// A mount point always names an absolute target; anything else
			// (a volume without a drive letter) is not a path to follow.
			err = fmt.Errorf("the mount point target %q is not an absolute path", target)
		}
		if err != nil {
			if info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
				// A reparse point that is itself a directory and names no
				// other path (a cloud-files placeholder, a volume mounted
				// without a drive letter): its contents are here.
				resolved = next
				continue
			}
			return "", false, fmt.Errorf("%s is a link or reparse point that cannot be resolved (%v); name a path without it", next, err)
		}
		switch {
		case filepath.IsAbs(target):
		case filepath.VolumeName(target) == "" && strings.HasPrefix(target, sep):
			target = filepath.VolumeName(resolved) + target // rooted on the link's volume (Windows)
		default:
			target = filepath.Join(resolved, target) // relative to the link's directory, which has no link left
		}
		return filepath.Join(append([]string{target}, names[i+1:]...)...), false, nil
	}
	return normalize(filepath.Clean(resolved)), true, nil
}

// normalize returns the canonical spelling of an existing path that has no
// link left, or the path itself when that cannot be determined.
func normalize(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
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

// sameOrInside reports whether path, or one of its existing ancestors, is
// the existing directory parent by file identity (os.SameFile), whatever the
// spelling of either path. It is false when parent does not exist, and when
// the file system gives parent and its own parent one identity (identity then
// decides nothing, and the comparison of resolved paths stands alone).
func sameOrInside(parent, path string) bool {
	target, err := os.Stat(parent)
	if err != nil {
		return false
	}
	if up := filepath.Dir(parent); up != parent {
		if info, err := os.Stat(up); err == nil && os.SameFile(info, target) {
			return false
		}
	}
	for p := path; ; {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, target) {
			return true
		}
		up := filepath.Dir(p)
		if up == p {
			return false
		}
		p = up
	}
}
