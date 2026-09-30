// Package source defines where the watched documents come from.
//
// A source lists documents and reads their content; the watcher does the
// rest (baselines, comparison, reports) the same way for every source. The
// folder source reads a folder of this computer; the Google Drive source
// (package gdrive) reads a drive through its API.
package source

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/office"
)

// Entry describes one document of a source.
type Entry struct {
	// Key identifies the document within its source: the file path of a
	// folder source, the file id of a drive.
	Key string
	// Name is the file name, Folder the folder shown in lists (starting with
	// the name of the source root) and Location the full place shown in the
	// detail.
	Name     string
	Folder   string
	Location string
	// Link is the web address that opens the document, for a document that
	// has no local path.
	Link string
	Kind office.Kind
	// Size is -1 when the source only knows it once the content is read.
	Size    int64
	ModTime time.Time
	// Version changes with the content when size and time are not enough
	// (a drive revision).
	Version string
	// Revision references this content in the history of a source that
	// implements History; empty otherwise.
	Revision string
	// CloudOnly marks a file whose content is not on this computer: reading
	// it downloads it.
	CloudOnly bool
	// Exported marks a content converted when it is read (a Google Doc
	// exported as .docx): two reads of the same version can differ byte for
	// byte, so a review relies on Version and on the copy read for the
	// report instead of reading again.
	Exported bool
}

// Source lists and reads documents.
type Source interface {
	// List reports every document of the source to yield. An error means the
	// listing is incomplete: the watcher then keeps the documents it did not
	// see instead of reporting them as deleted.
	List(ctx context.Context, yield func(Entry)) error
	// Read returns the current content of a document. A content larger than
	// max bytes returns ErrTooLarge.
	Read(ctx context.Context, e Entry, max int64) ([]byte, error)
	// Stat returns the current state of one document, or ErrNotFound.
	Stat(ctx context.Context, key string) (Entry, error)
}

// History is implemented by sources that keep the previous versions of a
// document. The watcher then records a baseline by its Revision, without
// downloading anything, and reads it only once the document has changed.
type History interface {
	// ReadRevision returns the content of a previous version, or
	// ErrRevisionGone when the source no longer keeps it.
	ReadRevision(ctx context.Context, e Entry, revision string, max int64) ([]byte, error)
}

var (
	// ErrNotFound reports a document that no longer exists.
	ErrNotFound = errors.New("document not found")
	// ErrTooLarge reports a content above the size limit.
	ErrTooLarge = errors.New("document too large")
	// ErrRevisionGone reports a version the source no longer keeps.
	ErrRevisionGone = errors.New("this version is no longer kept by the source")
)

// ReadLimited reads at most max bytes of r, or returns ErrTooLarge.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}
