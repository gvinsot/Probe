package gitrepo

// Export of declared files from one commit into a host directory (F8). The
// trusted dependency-preparation stage mounts that directory read-only into its
// container. The commit is read only through Tree and ReadBlobs: Git objects,
// never the working tree, checkout filters or export attributes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/fsutil"
)

// ExportedFile is one file ExportMatching wrote: its repository path, the
// SHA-256 and size of the exact blob, and whether Git records it executable.
type ExportedFile struct {
	Path       string
	SHA256     string
	Size       int64
	Executable bool
}

// ExportLimits bound one export. Every limit is checked against the tree
// listing before any blob is read.
type ExportLimits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// ExportMatching writes the regular files of commit whose paths match into
// dest, which must be absent or empty and must not have a symlink ancestor.
// match is called once per tree entry. A matched symlink, submodule or other
// non-regular entry is an error; unmatched entries are never inspected beyond
// their path. Directories are created with mode 0755 and files with 0644, or
// 0755 when Git records them executable; the modes are set explicitly so that
// a restrictive umask cannot make the export unreadable to a non-root
// container user. The result is sorted by path. On any error nothing the call
// wrote is left in dest.
func (r *Repository) ExportMatching(ctx context.Context, commit, dest string, match func(string) bool, limits ExportLimits) ([]ExportedFile, error) {
	if !validObjectID(commit) {
		return nil, errors.New("ExportMatching requires a resolved commit identifier")
	}
	if match == nil || limits.MaxFiles <= 0 || limits.MaxFileBytes < 0 || limits.MaxTotalBytes < 0 {
		return nil, errors.New("ExportMatching requires a matcher and non-negative limits")
	}
	tree, err := r.Tree(ctx, commit)
	if err != nil {
		return nil, err
	}
	var selected []TreeEntry
	var total int64
	for _, e := range tree {
		if !match(e.Path) {
			continue
		}
		if err := SafePath(e.Path); err != nil {
			return nil, err
		}
		if e.Type != "blob" || e.Mode != "100644" && e.Mode != "100755" {
			return nil, fmt.Errorf("matched path %q is a symlink, submodule or unsupported mode %s; only regular files are exported", e.Path, e.Mode)
		}
		if len(selected) == limits.MaxFiles {
			return nil, fmt.Errorf("more than %d files matched: %w", limits.MaxFiles, ErrLimit)
		}
		if e.Size > limits.MaxFileBytes {
			return nil, fmt.Errorf("%q has %d bytes, over the %d-byte limit per file: %w", e.Path, e.Size, limits.MaxFileBytes, ErrLimit)
		}
		if e.Size > limits.MaxTotalBytes-total {
			return nil, fmt.Errorf("matched files exceed %d bytes in total: %w", limits.MaxTotalBytes, ErrLimit)
		}
		total += e.Size
		selected = append(selected, e)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e == nil && info.Mode()&os.ModeSymlink != 0 && !fsutil.IsSystemAlias(p) {
			return nil, fmt.Errorf("export destination has symlink ancestor %q", p)
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	created := false
	if items, e := os.ReadDir(abs); e == nil && len(items) != 0 {
		return nil, errors.New("export destination must be empty")
	} else if os.IsNotExist(e) {
		created = true
	} else if e != nil {
		return nil, e
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, err
	}
	if err := os.Chmod(abs, 0755); err != nil {
		return nil, err
	}
	files, err := r.writeExport(ctx, abs, selected, limits.MaxFileBytes)
	if err != nil {
		// Leave nothing half-written behind.
		if created {
			_ = os.RemoveAll(abs)
		} else if items, e := os.ReadDir(abs); e == nil {
			for _, item := range items {
				_ = os.RemoveAll(filepath.Join(abs, item.Name()))
			}
		}
		return nil, err
	}
	return files, nil
}

func (r *Repository) writeExport(ctx context.Context, abs string, selected []TreeEntry, limit int64) ([]ExportedFile, error) {
	files := make([]ExportedFile, 0, len(selected))
	oids := make([]string, len(selected))
	for i, e := range selected {
		oids[i] = e.OID
	}
	next := 0
	err := r.ReadBlobs(ctx, oids, limit, func(oid string, data []byte) error {
		e := selected[next]
		next++
		if oid != e.OID || int64(len(data)) != e.Size {
			return errors.New("Git blob stream does not match the tree listing")
		}
		if err := mkdirExport(abs, filepath.Dir(filepath.FromSlash(e.Path))); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if e.Mode == "100755" {
			mode = 0755
		}
		target := filepath.Join(abs, filepath.FromSlash(e.Path))
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, ExportedFile{Path: e.Path, SHA256: hex.EncodeToString(sum[:]), Size: e.Size, Executable: e.Mode == "100755"})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if next != len(selected) {
		return nil, errors.New("Git blob stream ended before the tree listing")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// mkdirExport creates rel under root one component at a time, each with mode
// 0755 set explicitly. An existing component must be a directory.
func mkdirExport(root, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cursor := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cursor = filepath.Join(cursor, part)
		info, err := os.Lstat(cursor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("export path component %q is not a directory", cursor)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(cursor, 0755); err != nil {
			return err
		}
		if err := os.Chmod(cursor, 0755); err != nil {
			return err
		}
	}
	return nil
}
