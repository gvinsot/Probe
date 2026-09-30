package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gvinsot/Probe/desktop/internal/office"
)

// Folder is a folder of this computer: the folder of a synchronization
// client (OneDrive, Google Drive for desktop, Dropbox, iCloud…), a network
// share or any local folder. It is walked entirely at each scan.
type Folder struct {
	Root string
}

// NewFolder returns the source of a folder.
func NewFolder(root string) *Folder { return &Folder{Root: filepath.Clean(root)} }

// List walks the folder. Only an unreadable root fails the listing: an
// unreadable sub-folder is skipped.
func (f *Folder) List(ctx context.Context, yield func(Entry)) error {
	return filepath.WalkDir(f.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == f.Root {
				return err
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != f.Root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") || strings.EqualFold(name, "$RECYCLE.BIN")) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignored(name) {
			return nil
		}
		kind := office.KindOf(name)
		if kind == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		yield(f.entry(path, kind, info))
		return nil
	})
}

// ignored skips the lock and temporary files of Office and of the
// synchronization clients.
func ignored(name string) bool {
	return strings.HasPrefix(name, "~") || strings.HasPrefix(name, ".~") || strings.HasPrefix(name, "._")
}

func (f *Folder) entry(path string, kind office.Kind, info fs.FileInfo) Entry {
	return Entry{
		Key:       path,
		Name:      filepath.Base(path),
		Folder:    RelFolder(f.Root, path),
		Location:  path,
		Kind:      kind,
		Size:      info.Size(),
		ModTime:   info.ModTime(),
		CloudOnly: cloudOnly(info),
	}
}

// Read reads the file.
func (f *Folder) Read(ctx context.Context, e Entry, max int64) ([]byte, error) {
	if !f.contains(e.Key) {
		return nil, ErrNotFound
	}
	file, err := os.Open(e.Key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ReadLimited(file, max)
}

// Stat returns the current state of a file of the folder.
func (f *Folder) Stat(ctx context.Context, key string) (Entry, error) {
	kind := office.KindOf(key)
	if !f.contains(key) || kind == "" {
		return Entry{}, ErrNotFound
	}
	info, err := os.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	if info.IsDir() {
		return Entry{}, ErrNotFound
	}
	return f.entry(key, kind, info), nil
}

// contains reports whether a path is inside the folder: the keys come from
// the saved state, and nothing outside the watched folder is ever read.
func (f *Folder) contains(path string) bool {
	rel, err := filepath.Rel(f.Root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// RelFolder names the folder of a file, starting with the name of the root.
func RelFolder(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return filepath.Base(root)
	}
	return filepath.Join(filepath.Base(root), rel)
}
