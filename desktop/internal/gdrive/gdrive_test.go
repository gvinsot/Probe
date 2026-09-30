package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/office"
	"github.com/gvinsot/Probe/desktop/internal/source"
)

type memKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKeys) Get(n string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.m[n], nil
}

func (k *memKeys) Set(n, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v == "" {
		delete(k.m, n)
	} else {
		k.m[n] = v
	}
	return nil
}

type fakeRevision struct {
	id      string
	at      time.Time
	content string
}

// fakeGoogle serves the token endpoint and the part of the Drive API the
// source uses.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	access    string // the access token currently accepted
	issued    int
	files     map[string]file
	contents  map[string]string
	revisions map[string][]fakeRevision
	changes   []file // change log; the page token is an index into it
	removed   map[int]string
	lists     int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{t: t, files: map[string]file{}, contents: map[string]string{}, revisions: map[string][]fakeRevision{}, removed: map[int]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) endpoints() *Endpoints {
	return &Endpoints{Auth: g.srv.URL + "/auth", Token: g.srv.URL + "/token", Revoke: g.srv.URL + "/revoke", API: g.srv.URL + "/drive/v3"}
}

func (g *fakeGoogle) manager(keys *memKeys) *Manager {
	return &Manager{
		Keys:      keys,
		Client:    func() OAuthClient { return OAuthClient{ID: "client", Secret: "secret"} },
		Endpoints: g.endpoints(),
	}
}

func (g *fakeGoogle) put(f file, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[f.ID] = f
	g.contents[f.ID] = content
	g.changes = append(g.changes, f)
}

func (g *fakeGoogle) remove(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.files, id)
	g.removed[len(g.changes)] = id
	g.changes = append(g.changes, file{ID: id})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (g *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/token" {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "code-ok" || r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-ok" {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]string{"error": "invalid_grant"})
				return
			}
		}
		g.issued++
		g.access = "access-" + strconv.Itoa(g.issued)
		writeJSON(w, map[string]any{"access_token": g.access, "expires_in": 3600, "refresh_token": "refresh-ok"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.access {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "invalid credentials"}})
		return
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "not found"}})
	}
	p := strings.TrimPrefix(r.URL.Path, "/drive/v3")
	q := r.URL.Query()
	switch {
	case p == "/about":
		writeJSON(w, map[string]any{"user": map[string]string{"emailAddress": "Me@Example.com"}})
	case p == "/changes/startPageToken":
		writeJSON(w, map[string]string{"startPageToken": strconv.Itoa(len(g.changes))})
	case p == "/changes":
		from, _ := strconv.Atoi(q.Get("pageToken"))
		var changes []map[string]any
		for i := from; i < len(g.changes); i++ {
			if id, ok := g.removed[i]; ok {
				changes = append(changes, map[string]any{"fileId": id, "removed": true})
			} else {
				changes = append(changes, map[string]any{"fileId": g.changes[i].ID, "file": g.changes[i]})
			}
		}
		writeJSON(w, map[string]any{"changes": changes, "newStartPageToken": strconv.Itoa(len(g.changes))})
	case p == "/files":
		g.lists++
		var files []file
		for _, f := range g.files {
			if !f.Trashed && strings.Contains(q.Get("q"), "'"+f.MimeType+"'") {
				files = append(files, f)
			}
		}
		writeJSON(w, map[string]any{"files": files})
	case p == "/files/root":
		writeJSON(w, map[string]string{"id": "root-id"})
	case strings.HasPrefix(p, "/export/"):
		// An export link of a revision: /export/{file}/{revision}.
		parts := strings.Split(strings.TrimPrefix(p, "/export/"), "/")
		for _, rev := range g.revisions[parts[0]] {
			if rev.id == parts[1] {
				fmt.Fprint(w, rev.content)
				return
			}
		}
		notFound()
	case strings.HasPrefix(p, "/files/"):
		parts := strings.Split(strings.TrimPrefix(p, "/files/"), "/")
		f, ok := g.files[parts[0]]
		if !ok {
			notFound()
			return
		}
		switch {
		case len(parts) == 1 && q.Get("alt") == "media":
			fmt.Fprint(w, g.contents[f.ID])
		case len(parts) == 1:
			writeJSON(w, f)
		case parts[1] == "export":
			fmt.Fprint(w, "export of "+g.contents[f.ID])
		case parts[1] == "revisions" && len(parts) == 2:
			var revs []map[string]any
			for _, rev := range g.revisions[f.ID] {
				revs = append(revs, map[string]any{
					"id": rev.id, "modifiedTime": rev.at,
					"exportLinks": map[string]string{exportMime(office.Word): g.srv.URL + "/drive/v3/export/" + f.ID + "/" + rev.id},
				})
			}
			writeJSON(w, map[string]any{"revisions": revs})
		case parts[1] == "revisions" && len(parts) == 3:
			for _, rev := range g.revisions[f.ID] {
				if rev.id == parts[2] {
					fmt.Fprint(w, rev.content)
					return
				}
			}
			notFound()
		default:
			notFound()
		}
	default:
		notFound()
	}
}

func TestConnectStoresTheRefreshToken(t *testing.T) {
	g := newFakeGoogle(t)
	keys := &memKeys{m: map[string]string{}}
	m := g.manager(keys)
	var connected string
	m.OnAccount = func(a string) error { connected = a; return nil }
	m.Open = func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("scope") != Scope || q.Get("access_type") != "offline" {
			return fmt.Errorf("bad consent request: %s", authURL)
		}
		redirect := q.Get("redirect_uri")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			return fmt.Errorf("redirect %q is not a loopback address", redirect)
		}
		go func() {
			// Another local process guessing the listener is refused.
			if resp, err := http.Get(redirect + "?code=code-ok&state=forged"); err == nil {
				resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("forged state accepted: %d", resp.StatusCode)
				}
			}
			resp, err := http.Get(redirect + "?code=code-ok&state=" + url.QueryEscape(q.Get("state")))
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	if err := m.Connect(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.ConnectStatus().State == "pending" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st := m.ConnectStatus()
	if st.State != "done" || st.Account != "me@example.com" {
		t.Fatalf("status %+v", st)
	}
	if connected != "me@example.com" || keys.m[tokenKey("me@example.com")] != "refresh-ok" {
		t.Fatalf("account %q, keys %v", connected, keys.m)
	}

	if err := m.Disconnect("me@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.m[tokenKey("me@example.com")]; ok {
		t.Fatal("token kept after disconnect")
	}
}

func TestConnectNeedsAClient(t *testing.T) {
	m := &Manager{Keys: &memKeys{m: map[string]string{}}, Client: func() OAuthClient { return OAuthClient{} }}
	if err := m.Connect(); !errors.Is(err, ErrNoClient) {
		t.Fatalf("Connect without client = %v", err)
	}
}

func driveSource(t *testing.T, g *fakeGoogle, dir string, cfg config.Source) *Source {
	keys := &memKeys{m: map[string]string{tokenKey("me@example.com"): "refresh-ok"}}
	return g.manager(keys).NewSource(cfg, dir)
}

func list(t *testing.T, s *Source) map[string]source.Entry {
	t.Helper()
	out := map[string]source.Entry{}
	if err := s.List(context.Background(), func(e source.Entry) { out[e.Key] = e }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSourceListsTheDriveThenFollowsChanges(t *testing.T) {
	g := newFakeGoogle(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	g.put(file{ID: "contracts", Name: "Contracts", MimeType: folderMime, Parents: []string{"root-id"}}, "")
	g.put(file{ID: "a", Name: "a.docx", MimeType: officeMimes[0], Parents: []string{"contracts"}, Size: "7", HeadRevisionID: "r1", ModifiedTime: t0, WebViewLink: "https://docs.google.com/a"}, "content")
	g.put(file{ID: "plan", Name: "Plan", MimeType: "application/vnd.google-apps.document", Parents: []string{"root-id"}, ModifiedTime: t0}, "plan")
	g.put(file{ID: "photo", Name: "photo.jpg", MimeType: "image/jpeg", Parents: []string{"root-id"}}, "")
	g.put(file{ID: "elsewhere", Name: "b.docx", MimeType: officeMimes[0], Parents: []string{"shared-with-me"}}, "")

	dir := t.TempDir()
	cfg := config.Source{ID: "gdrive-0123456789abcdef", Type: config.SourceGoogleDrive, Account: "me@example.com"}
	s := driveSource(t, g, dir, cfg)
	got := list(t, s)
	if len(got) != 2 {
		t.Fatalf("entries %v, want a.docx and Plan", got)
	}
	a := got["a"]
	if a.Folder != "My Drive/Contracts" || a.Kind != office.Word || a.Revision != "r1" || a.Version != "r1" || a.Size != 7 || a.Exported || a.Link == "" {
		t.Fatalf("uploaded document: %+v", a)
	}
	plan := got["plan"]
	if !plan.Exported || plan.Size != -1 || plan.Revision != "t:2026-09-01T10:00:00Z" || plan.Folder != "My Drive" {
		t.Fatalf("Google Doc: %+v", plan)
	}
	data, err := s.Read(context.Background(), plan, 1<<20)
	if err != nil || string(data) != "export of plan" {
		t.Fatalf("export: %q %v", data, err)
	}

	// Renamed and moved to the root, and a document deleted.
	g.put(file{ID: "a", Name: "a-v2.docx", MimeType: officeMimes[0], Parents: []string{"root-id"}, Size: "8", HeadRevisionID: "r2", ModifiedTime: t0.Add(time.Hour)}, "content2")
	g.remove("plan")
	got = list(t, s)
	if len(got) != 1 || got["a"].Name != "a-v2.docx" || got["a"].Folder != "My Drive" || got["a"].Revision != "r2" {
		t.Fatalf("after changes: %+v", got)
	}
	if g.lists != 1 {
		t.Fatalf("%d full listings, want only the first", g.lists)
	}

	// The cache survives a restart: no full listing again.
	s2 := driveSource(t, g, dir, cfg)
	if got := list(t, s2); len(got) != 1 {
		t.Fatalf("reloaded cache: %+v", got)
	}
	if g.lists != 1 {
		t.Fatalf("restart listed the drive again (%d listings)", g.lists)
	}
}

func TestSourceWatchesOnlyItsFolder(t *testing.T) {
	g := newFakeGoogle(t)
	g.put(file{ID: "legal", Name: "Legal", MimeType: folderMime, Parents: []string{"root-id"}}, "")
	g.put(file{ID: "deep", Name: "2026", MimeType: folderMime, Parents: []string{"legal"}}, "")
	g.put(file{ID: "in", Name: "in.xlsx", MimeType: officeMimes[2], Parents: []string{"deep"}}, "")
	g.put(file{ID: "out", Name: "out.xlsx", MimeType: officeMimes[2], Parents: []string{"root-id"}}, "")
	cfg := config.Source{ID: "gdrive-0123456789abcdef", Type: config.SourceGoogleDrive, Account: "me@example.com", FolderID: "legal"}
	got := list(t, driveSource(t, g, t.TempDir(), cfg))
	if len(got) != 1 || got["in"].Folder != "Legal/2026" || got["in"].Kind != office.Excel {
		t.Fatalf("entries %+v", got)
	}
}

func TestReadRevision(t *testing.T) {
	g := newFakeGoogle(t)
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	g.put(file{ID: "doc", Name: "Doc", MimeType: "application/vnd.google-apps.document", Parents: []string{"root-id"}, ModifiedTime: t1.Add(2 * time.Hour)}, "v3")
	g.put(file{ID: "bin", Name: "bin.docx", MimeType: officeMimes[0], Parents: []string{"root-id"}, HeadRevisionID: "b2"}, "b2")
	g.revisions["doc"] = []fakeRevision{{"1", t1, "v1"}, {"2", t1.Add(time.Hour), "v2"}, {"3", t1.Add(2 * time.Hour), "v3"}}
	g.revisions["bin"] = []fakeRevision{{"b1", t1, "b1"}}
	s := driveSource(t, g, t.TempDir(), config.Source{ID: "gdrive-0123456789abcdef", Type: config.SourceGoogleDrive, Account: "me@example.com"})
	entries := list(t, s)
	ctx := context.Background()

	// The version reviewed at 11:30 is revision 2, saved at 11:00.
	at := "t:" + t1.Add(90*time.Minute).Format(time.RFC3339Nano)
	if data, err := s.ReadRevision(ctx, entries["doc"], at, 1<<20); err != nil || string(data) != "v2" {
		t.Fatalf("revision at a time: %q %v", data, err)
	}
	if _, err := s.ReadRevision(ctx, entries["doc"], "t:"+t1.Add(-time.Hour).Format(time.RFC3339Nano), 1<<20); !errors.Is(err, source.ErrRevisionGone) {
		t.Fatalf("revision older than the history = %v, want ErrRevisionGone", err)
	}
	if data, err := s.ReadRevision(ctx, entries["bin"], "b1", 1<<20); err != nil || string(data) != "b1" {
		t.Fatalf("uploaded revision: %q %v", data, err)
	}
	if _, err := s.ReadRevision(ctx, entries["bin"], "purged", 1<<20); !errors.Is(err, source.ErrRevisionGone) {
		t.Fatalf("purged revision = %v, want ErrRevisionGone", err)
	}
	if _, err := s.Read(ctx, entries["bin"], 1); !errors.Is(err, source.ErrTooLarge) {
		t.Fatalf("size limit = %v, want ErrTooLarge", err)
	}
}

func TestExpiredAccessTokenIsRefreshed(t *testing.T) {
	g := newFakeGoogle(t)
	g.put(file{ID: "a", Name: "a.docx", MimeType: officeMimes[0], Parents: []string{"root-id"}}, "x")
	s := driveSource(t, g, t.TempDir(), config.Source{ID: "gdrive-0123456789abcdef", Type: config.SourceGoogleDrive, Account: "me@example.com"})
	list(t, s)
	g.mu.Lock()
	g.access = "rotated-by-google"
	g.mu.Unlock()
	if _, err := s.Stat(context.Background(), "a"); err != nil {
		t.Fatalf("Stat after the token expired: %v", err)
	}
}

func TestRevokedAccountIsReported(t *testing.T) {
	g := newFakeGoogle(t)
	keys := &memKeys{m: map[string]string{tokenKey("me@example.com"): "revoked"}}
	s := g.manager(keys).NewSource(config.Source{ID: "gdrive-0123456789abcdef", Type: config.SourceGoogleDrive, Account: "me@example.com"}, t.TempDir())
	if err := s.List(context.Background(), func(source.Entry) {}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("List with a revoked token = %v, want ErrRevoked", err)
	}
}

func TestTokenIsOnlySentToGoogle(t *testing.T) {
	g := newFakeGoogle(t)
	m := g.manager(&memKeys{m: map[string]string{tokenKey("me@example.com"): "refresh-ok"}})
	for _, u := range []string{"https://evil.example/x", "http://docs.google.com/x", "https://docs.google.com.evil.example/x"} {
		if _, err := m.fetch(context.Background(), "me@example.com", u, 1<<20); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("fetch %s = %v, want a refusal", u, err)
		}
	}
	for _, u := range []string{"https://docs.google.com/feeds/download", "https://doc-0s-2g-docs.googleusercontent.com/x", g.srv.URL + "/drive/v3/files"} {
		if !m.trusted(u) {
			t.Errorf("%s not trusted", u)
		}
	}
}

func TestFolderRef(t *testing.T) {
	for in, want := range map[string]string{
		"https://drive.google.com/drive/folders/1AbC_d-9?usp=sharing": "1AbC_d-9",
		"https://drive.google.com/drive/u/1/folders/XYZ":              "XYZ",
		"https://drive.google.com/open?id=QRS":                        "QRS",
		" 1AbC ":                                                      "1AbC",
		"https://example.com/nothing":                                 "",
	} {
		if got := FolderRef(in); got != want {
			t.Errorf("FolderRef(%q) = %q, want %q", in, got, want)
		}
	}
}
