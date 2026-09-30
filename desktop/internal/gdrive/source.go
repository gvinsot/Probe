package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
	"github.com/gvinsot/Probe/desktop/internal/source"
)

const folderMime = "application/vnd.google-apps.folder"

// native are the Google formats and the Office format they are exported to.
var native = map[string]struct {
	kind   office.Kind
	export string
}{
	"application/vnd.google-apps.document":     {office.Word, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"application/vnd.google-apps.spreadsheet":  {office.Excel, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"application/vnd.google-apps.presentation": {office.PowerPoint, "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
}

// officeMimes are the uploaded Office documents the listing asks for.
var officeMimes = []string{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-word.document.macroEnabled.12",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-excel.sheet.macroEnabled.12",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.ms-powerpoint.presentation.macroEnabled.12",
}

func exportMime(kind office.Kind) string {
	for _, n := range native {
		if n.kind == kind {
			return n.export
		}
	}
	return ""
}

// classify returns the kind of a Drive file and whether it is a Google
// format, exported when read.
func classify(mime, name string) (office.Kind, bool) {
	if n, ok := native[mime]; ok {
		return n.kind, true
	}
	if strings.HasPrefix(mime, "application/vnd.google-apps.") {
		return "", false // folders, shortcuts, forms…
	}
	return office.KindOf(name), false
}

// file is a Drive file resource, reduced to what the source uses.
type file struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	MimeType       string    `json:"mimeType"`
	Parents        []string  `json:"parents"`
	Size           string    `json:"size"`
	ModifiedTime   time.Time `json:"modifiedTime"`
	HeadRevisionID string    `json:"headRevisionId"`
	Trashed        bool      `json:"trashed"`
	WebViewLink    string    `json:"webViewLink"`
	DriveID        string    `json:"driveId"`
}

const fileFields = "id,name,mimeType,parents,size,modifiedTime,headRevisionId,trashed,webViewLink,driveId"

// fullSyncEvery lists the whole drive again once a day, which repairs a
// cache that missed a change.
const fullSyncEvery = 24 * time.Hour

// Source is a Google Drive, or a folder of it, read through the API.
//
// It keeps the list of the folders and documents of the drive in a cache
// saved in the data directory. The first scan lists the drive; the next
// ones only ask Drive for the changes since the previous scan. Nothing is
// downloaded to list: a document is read only once it changed, and its
// baseline is then read from the revisions Drive keeps.
type Source struct {
	m    *Manager
	cfg  config.Source
	path string

	mu    sync.Mutex
	cache *cache
}

type cache struct {
	// Key is the account, drive and folder the cache belongs to.
	Key       string           `json:"key"`
	PageToken string           `json:"page_token"`
	RootID    string           `json:"root_id"`
	RootName  string           `json:"root_name"`
	SyncedAt  time.Time        `json:"synced_at"`
	Files     map[string]*item `json:"files"`
	Folders   map[string]*node `json:"folders"`
}

type item struct {
	Name     string    `json:"n"`
	Mime     string    `json:"m"`
	Parents  []string  `json:"p"`
	Size     int64     `json:"s,omitempty"`
	Head     string    `json:"h,omitempty"`
	Modified time.Time `json:"t"`
	Link     string    `json:"l,omitempty"`
}

type node struct {
	Name    string   `json:"n"`
	Parents []string `json:"p"`
}

func (s *Source) key() string {
	return s.cfg.Account + "\x00" + s.cfg.DriveID + "\x00" + s.cfg.FolderID
}

// List brings the cache up to date and reports the documents under the
// watched root.
func (s *Source) List(ctx context.Context, yield func(source.Entry)) error {
	s.mu.Lock()
	err := s.sync(ctx)
	var entries []source.Entry
	if err == nil {
		entries = s.entries()
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, e := range entries {
		yield(e)
	}
	return nil
}

func (s *Source) sync(ctx context.Context) error {
	if s.cache == nil {
		s.load()
	}
	if s.cache == nil || s.cache.Key != s.key() || s.cache.PageToken == "" || time.Since(s.cache.SyncedAt) > fullSyncEvery {
		return s.fullSync(ctx)
	}
	changed, err := s.changes(ctx)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound || ae.Status == http.StatusGone) {
		// The page token expired: list everything again.
		return s.fullSync(ctx)
	}
	if err != nil {
		return err
	}
	if changed {
		s.save()
	}
	return nil
}

func (s *Source) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var c cache
	if json.Unmarshal(data, &c) == nil && c.Files != nil && c.Folders != nil {
		s.cache = &c
	}
}

func (s *Source) save() {
	data, err := json.Marshal(s.cache)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	config.WriteFileAtomic(s.path, data)
}

// driveParams scopes a request to the shared drive of the source.
func (s *Source) driveParams(q url.Values) url.Values {
	q.Set("supportsAllDrives", "true")
	if s.cfg.DriveID != "" {
		q.Set("driveId", s.cfg.DriveID)
		q.Set("includeItemsFromAllDrives", "true")
	}
	return q
}

func (s *Source) fullSync(ctx context.Context) error {
	// The change token is taken first: a change made during the listing is
	// then seen again at the next scan instead of being lost.
	var start struct {
		Token string `json:"startPageToken"`
	}
	if err := s.m.getJSON(ctx, s.cfg.Account, "/changes/startPageToken", s.driveParams(url.Values{}), &start); err != nil {
		return err
	}
	rootID, rootName, err := s.resolveRoot(ctx)
	if err != nil {
		return err
	}
	c := &cache{
		Key: s.key(), PageToken: start.Token, RootID: rootID, RootName: rootName,
		SyncedAt: time.Now(), Files: map[string]*item{}, Folders: map[string]*node{},
	}
	mimes := append([]string{folderMime}, officeMimes...)
	for m := range native {
		mimes = append(mimes, m)
	}
	sort.Strings(mimes)
	clauses := make([]string, len(mimes))
	for i, m := range mimes {
		clauses[i] = "mimeType = '" + m + "'"
	}
	q := s.driveParams(url.Values{
		"q":        {"trashed = false and (" + strings.Join(clauses, " or ") + ")"},
		"fields":   {"nextPageToken,files(" + fileFields + ")"},
		"pageSize": {"1000"},
	})
	if s.cfg.DriveID != "" {
		q.Set("corpora", "drive")
	} else {
		q.Set("corpora", "user")
	}
	for {
		var resp struct {
			NextPageToken string `json:"nextPageToken"`
			Files         []file `json:"files"`
		}
		if err := s.m.getJSON(ctx, s.cfg.Account, "/files", q, &resp); err != nil {
			return err
		}
		for i := range resp.Files {
			c.apply(&resp.Files[i])
		}
		if resp.NextPageToken == "" {
			break
		}
		q.Set("pageToken", resp.NextPageToken)
	}
	s.cache = c
	s.save()
	return nil
}

// resolveRoot returns the id and the name of the watched root.
func (s *Source) resolveRoot(ctx context.Context) (string, string, error) {
	switch {
	case s.cfg.FolderID != "":
		var f file
		q := url.Values{"fields": {"id,name,mimeType,trashed"}, "supportsAllDrives": {"true"}}
		if err := s.m.getJSON(ctx, s.cfg.Account, "/files/"+url.PathEscape(s.cfg.FolderID), q, &f); err != nil {
			return "", "", err
		}
		if f.MimeType != folderMime || f.Trashed {
			return "", "", errors.New("the watched folder was deleted")
		}
		return f.ID, f.Name, nil
	case s.cfg.DriveID != "":
		var d Drive
		if err := s.m.getJSON(ctx, s.cfg.Account, "/drives/"+url.PathEscape(s.cfg.DriveID), url.Values{"fields": {"id,name"}}, &d); err != nil {
			return "", "", err
		}
		return s.cfg.DriveID, d.Name, nil
	default:
		var f file
		if err := s.m.getJSON(ctx, s.cfg.Account, "/files/root", url.Values{"fields": {"id"}}, &f); err != nil {
			return "", "", err
		}
		return f.ID, "My Drive", nil
	}
}

// changes applies the changes since the last scan and reports whether
// there was any.
func (s *Source) changes(ctx context.Context) (bool, error) {
	token := s.cache.PageToken
	changed := false
	for {
		var resp struct {
			NextPageToken     string `json:"nextPageToken"`
			NewStartPageToken string `json:"newStartPageToken"`
			Changes           []struct {
				FileID  string `json:"fileId"`
				Removed bool   `json:"removed"`
				File    *file  `json:"file"`
			} `json:"changes"`
		}
		q := s.driveParams(url.Values{
			"pageToken":      {token},
			"pageSize":       {"1000"},
			"includeRemoved": {"true"},
			"fields":         {"nextPageToken,newStartPageToken,changes(fileId,removed,file(" + fileFields + "))"},
		})
		if err := s.m.getJSON(ctx, s.cfg.Account, "/changes", q, &resp); err != nil {
			return changed, err
		}
		for _, ch := range resp.Changes {
			changed = true
			if ch.Removed || ch.File == nil || ch.File.Trashed {
				delete(s.cache.Files, ch.FileID)
				delete(s.cache.Folders, ch.FileID)
				continue
			}
			ch.File.ID = ch.FileID
			s.cache.apply(ch.File)
			if ch.FileID == s.cache.RootID && ch.File.MimeType == folderMime && s.cfg.FolderID != "" {
				s.cache.RootName = ch.File.Name
			}
		}
		if resp.NewStartPageToken != "" {
			if resp.NewStartPageToken != s.cache.PageToken {
				changed = true
			}
			s.cache.PageToken = resp.NewStartPageToken
			return changed, nil
		}
		if resp.NextPageToken == "" {
			return changed, errors.New("google drive: incomplete list of changes")
		}
		token = resp.NextPageToken
	}
}

// apply records the state of a file in the cache.
func (c *cache) apply(f *file) {
	if f.MimeType == folderMime {
		c.Folders[f.ID] = &node{Name: f.Name, Parents: f.Parents}
		delete(c.Files, f.ID)
		return
	}
	delete(c.Folders, f.ID)
	if kind, _ := classify(f.MimeType, f.Name); kind == "" {
		delete(c.Files, f.ID)
		return
	}
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	c.Files[f.ID] = &item{
		Name: f.Name, Mime: f.MimeType, Parents: f.Parents, Size: size,
		Head: f.HeadRevisionID, Modified: f.ModifiedTime, Link: f.WebViewLink,
	}
}

// maxDepth bounds the walk up the folders, against a cycle.
const maxDepth = 100

// folderPath returns the path of a folder from the root, "" for the root
// itself, and whether the folder is under the root at all.
func (c *cache) folderPath(id string, memo map[string]*string) (string, bool) {
	var walk func(id string, depth int) (string, bool)
	walk = func(id string, depth int) (string, bool) {
		if id == c.RootID {
			return "", true
		}
		if p, ok := memo[id]; ok {
			if p == nil {
				return "", false
			}
			return *p, true
		}
		n, ok := c.Folders[id]
		if !ok || depth > maxDepth {
			memo[id] = nil
			return "", false
		}
		memo[id] = nil // a cycle comes back here as "outside"
		for _, parent := range n.Parents {
			if p, ok := walk(parent, depth+1); ok {
				full := n.Name
				if p != "" {
					full = p + "/" + n.Name
				}
				memo[id] = &full
				return full, true
			}
		}
		return "", false
	}
	return walk(id, 0)
}

// entries lists the documents under the root.
func (s *Source) entries() []source.Entry {
	c := s.cache
	memo := map[string]*string{}
	var out []source.Entry
	for id, it := range c.Files {
		for _, parent := range it.Parents {
			rel, ok := c.folderPath(parent, memo)
			if !ok {
				continue
			}
			out = append(out, s.entry(id, it, rel))
			break
		}
	}
	return out
}

func (s *Source) entry(id string, it *item, rel string) source.Entry {
	folder := s.cache.RootName
	if rel != "" {
		folder += "/" + rel
	}
	kind, exported := classify(it.Mime, it.Name)
	e := source.Entry{
		Key:      id,
		Name:     it.Name,
		Folder:   folder,
		Location: "Google Drive · " + s.cfg.Account + " · " + folder + "/" + it.Name,
		Link:     it.Link,
		Kind:     kind,
		ModTime:  it.Modified,
		Exported: exported,
	}
	if exported {
		// A Google Doc has no revision id in its listing: its baseline is
		// found by time among its revisions.
		e.Size = -1
		e.Revision = "t:" + it.Modified.UTC().Format(time.RFC3339Nano)
	} else {
		e.Size = it.Size
		e.Version = it.Head
		e.Revision = it.Head
	}
	return e
}

// Read downloads a document, exporting a Google format to Office.
func (s *Source) Read(ctx context.Context, e source.Entry, max int64) ([]byte, error) {
	if !fileID.MatchString(e.Key) {
		return nil, source.ErrNotFound
	}
	var u string
	if e.Exported {
		u = s.m.endpoints().API + "/files/" + url.PathEscape(e.Key) + "/export?" + url.Values{"mimeType": {exportMime(e.Kind)}}.Encode()
	} else {
		u = s.m.endpoints().API + "/files/" + url.PathEscape(e.Key) + "?alt=media&supportsAllDrives=true"
	}
	data, err := s.m.fetch(ctx, s.cfg.Account, u, max)
	return data, mapError(err)
}

// mapError translates the Drive errors the watcher handles.
func mapError(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusNotFound:
		return source.ErrNotFound
	case ae.Reason == "exportSizeLimitExceeded":
		return source.ErrTooLarge
	}
	return err
}

// Stat reads the current state of one document.
func (s *Source) Stat(ctx context.Context, key string) (source.Entry, error) {
	if !fileID.MatchString(key) {
		return source.Entry{}, source.ErrNotFound
	}
	var f file
	q := url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}
	if err := s.m.getJSON(ctx, s.cfg.Account, "/files/"+url.PathEscape(key), q, &f); err != nil {
		return source.Entry{}, mapError(err)
	}
	if f.Trashed {
		return source.Entry{}, source.ErrNotFound
	}
	if kind, _ := classify(f.MimeType, f.Name); kind == "" {
		return source.Entry{}, source.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.load()
	}
	if s.cache == nil {
		s.cache = &cache{Files: map[string]*item{}, Folders: map[string]*node{}}
	}
	s.cache.apply(&f)
	it := s.cache.Files[f.ID]
	rel, _ := s.cache.folderPath(firstParent(f.Parents), map[string]*string{})
	return s.entry(f.ID, it, rel), nil
}

func firstParent(parents []string) string {
	if len(parents) == 0 {
		return ""
	}
	return parents[0]
}

// revision is a Drive revision resource.
type revision struct {
	ID           string            `json:"id"`
	ModifiedTime time.Time         `json:"modifiedTime"`
	ExportLinks  map[string]string `json:"exportLinks"`
}

// ReadRevision downloads a previous version. An uploaded document names it
// by revision id. A Google Doc names it by the time of the version; Drive
// merges the revisions of an editing session, so the latest revision saved
// at or before that time is taken: when a merge moved the boundary, the
// report shows more changes than strictly happened, never fewer.
func (s *Source) ReadRevision(ctx context.Context, e source.Entry, rev string, max int64) ([]byte, error) {
	if !fileID.MatchString(e.Key) {
		return nil, source.ErrNotFound
	}
	if !e.Exported {
		if !fileID.MatchString(rev) {
			return nil, source.ErrRevisionGone
		}
		u := s.m.endpoints().API + "/files/" + url.PathEscape(e.Key) + "/revisions/" + url.PathEscape(rev) + "?alt=media"
		data, err := s.m.fetch(ctx, s.cfg.Account, u, max)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, source.ErrRevisionGone
		}
		return data, mapError(err)
	}
	stamp, ok := strings.CutPrefix(rev, "t:")
	if !ok {
		return nil, source.ErrRevisionGone
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil, source.ErrRevisionGone
	}
	var best *revision
	page := ""
	for {
		var resp struct {
			NextPageToken string     `json:"nextPageToken"`
			Revisions     []revision `json:"revisions"`
		}
		q := url.Values{"pageSize": {"200"}, "fields": {"nextPageToken,revisions(id,modifiedTime,exportLinks)"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		if err := s.m.getJSON(ctx, s.cfg.Account, "/files/"+url.PathEscape(e.Key)+"/revisions", q, &resp); err != nil {
			return nil, mapError(err)
		}
		for i := range resp.Revisions {
			r := &resp.Revisions[i]
			if !r.ModifiedTime.After(at) && (best == nil || r.ModifiedTime.After(best.ModifiedTime)) {
				best = r
			}
		}
		if page = resp.NextPageToken; page == "" {
			break
		}
	}
	if best == nil {
		return nil, source.ErrRevisionGone
	}
	link := best.ExportLinks[exportMime(e.Kind)]
	if link == "" {
		return nil, fmt.Errorf("google drive offers no %s export of revision %s", e.Kind, best.ID)
	}
	data, err := s.m.fetch(ctx, s.cfg.Account, link, max)
	return data, mapError(err)
}
