package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/source"
)

// KeyStore keeps secrets, the system keychain in the application.
type KeyStore interface {
	Get(name string) (string, error)
	Set(name, value string) error
}

// ClientSecretKey names the secret of the configured OAuth client in the
// keychain.
const ClientSecretKey = "google-client-secret"

func tokenKey(account string) string { return "google:" + account }

// Manager holds the connected Google accounts: their tokens, the connection
// flow and the calls to the Drive API.
type Manager struct {
	// Keys stores the refresh tokens.
	Keys KeyStore
	// Client returns the OAuth client to use.
	Client func() OAuthClient
	// Open opens the consent page in the default browser.
	Open func(url string) error
	// OnAccount runs when an account is connected, to add it to the
	// settings.
	OnAccount func(account string) error
	// Endpoints and HTTP default to Google and http.DefaultClient.
	Endpoints *Endpoints
	HTTP      *http.Client
	Log       *slog.Logger

	mu         sync.Mutex
	tokens     map[string]token
	status     ConnectStatus
	generation int
	cancel     context.CancelFunc
}

// ConnectStatus is the progress of the last connection.
type ConnectStatus struct {
	// State is "" (none yet), "pending", "done" or "error".
	State   string `json:"state"`
	Account string `json:"account,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (m *Manager) endpoints() Endpoints {
	if m.Endpoints != nil {
		return *m.Endpoints
	}
	return Google
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return http.DefaultClient
}

func (m *Manager) open(u string) error {
	if m.Open == nil {
		return errors.New("no browser available")
	}
	return m.Open(u)
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (m *Manager) client() OAuthClient {
	if m.Client != nil {
		return m.Client()
	}
	return OAuthClient{ID: DefaultClientID, Secret: DefaultClientSecret}
}

// Connect starts connecting an account in the background: the consent page
// opens in the browser and ConnectStatus reports the outcome. A connection
// still pending is abandoned.
func (m *Manager) Connect() error {
	c := m.client()
	if c.ID == "" {
		return ErrNoClient
	}
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	m.generation++
	gen := m.generation
	m.cancel = cancel
	m.status = ConnectStatus{State: "pending"}
	m.mu.Unlock()

	go func() {
		defer cancel()
		account, err := m.connect(ctx, c)
		m.mu.Lock()
		defer m.mu.Unlock()
		if gen != m.generation {
			return
		}
		m.cancel = nil
		if err != nil {
			m.log().Warn("google connection failed", "err", err)
			m.status = ConnectStatus{State: "error", Error: err.Error()}
			return
		}
		m.log().Info("google account connected", "account", account)
		m.status = ConnectStatus{State: "done", Account: account}
	}()
	return nil
}

func (m *Manager) connect(ctx context.Context, c OAuthClient) (string, error) {
	tok, err := m.authorize(ctx, c)
	if err != nil {
		return "", err
	}
	if tok.Refresh == "" {
		return "", errors.New("google returned no refresh token: remove Probe Desktop from the third-party access of the account and connect again")
	}
	var about struct {
		User struct {
			Email string `json:"emailAddress"`
		} `json:"user"`
	}
	data, err := m.get(ctx, tok.Access, m.endpoints().API+"/about?fields="+url.QueryEscape("user(emailAddress)"), 1<<20)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &about); err != nil || about.User.Email == "" {
		return "", errors.New("google did not tell which account was connected")
	}
	account := strings.ToLower(about.User.Email)
	if err := m.Keys.Set(tokenKey(account), tok.Refresh); err != nil {
		return "", fmt.Errorf("store the Google token in the keychain: %w", err)
	}
	m.mu.Lock()
	m.cacheToken(account, tok)
	m.mu.Unlock()
	if m.OnAccount != nil {
		if err := m.OnAccount(account); err != nil {
			return "", err
		}
	}
	return account, nil
}

func (m *Manager) cacheToken(account string, t token) {
	if m.tokens == nil {
		m.tokens = map[string]token{}
	}
	m.tokens[account] = t
}

// ConnectStatus reports the progress of the last connection.
func (m *Manager) ConnectStatus() ConnectStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Disconnect forgets an account: its refresh token is revoked at Google (on
// a best effort basis) and deleted from the keychain.
func (m *Manager) Disconnect(account string) error {
	m.mu.Lock()
	delete(m.tokens, account)
	m.mu.Unlock()
	refresh, err := m.Keys.Get(tokenKey(account))
	if err != nil {
		return err
	}
	if refresh != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints().Revoke, strings.NewReader(url.Values{"token": {refresh}}.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if resp, err := m.httpClient().Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	return m.Keys.Set(tokenKey(account), "")
}

// accessToken returns a valid access token of an account, refreshed when
// needed.
func (m *Manager) accessToken(ctx context.Context, account string) (string, error) {
	m.mu.Lock()
	t := m.tokens[account]
	m.mu.Unlock()
	if t.valid() {
		return t.Access, nil
	}
	refresh, err := m.Keys.Get(tokenKey(account))
	if err != nil {
		return "", fmt.Errorf("read the Google token from the keychain: %w", err)
	}
	if refresh == "" {
		return "", ErrRevoked
	}
	c := m.client()
	if c.ID == "" {
		return "", ErrNoClient
	}
	t, err = m.exchange(ctx, url.Values{
		"refresh_token": {refresh},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return "", err
	}
	if t.Refresh != "" && t.Refresh != refresh {
		m.Keys.Set(tokenKey(account), t.Refresh)
	}
	m.mu.Lock()
	m.cacheToken(account, t)
	m.mu.Unlock()
	return t.Access, nil
}

func (m *Manager) invalidate(account string) {
	m.mu.Lock()
	delete(m.tokens, account)
	m.mu.Unlock()
}

// apiError is an error answer of the Drive API.
type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("google drive: HTTP %d", e.Status)
	}
	return "google drive: " + e.Message
}

// fetch downloads a Drive address with the token of an account, refreshing
// the token once if Google refuses it.
func (m *Manager) fetch(ctx context.Context, account, rawURL string, max int64) ([]byte, error) {
	if !m.trusted(rawURL) {
		return nil, fmt.Errorf("refusing to send a Google token to %s", rawURL)
	}
	for attempt := 0; ; attempt++ {
		access, err := m.accessToken(ctx, account)
		if err != nil {
			return nil, err
		}
		data, err := m.get(ctx, access, rawURL, max)
		var ae *apiError
		if attempt == 0 && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			m.invalidate(account)
			continue
		}
		return data, err
	}
}

// get downloads an address with an access token.
func (m *Manager) get(ctx context.Context, access, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		ae := &apiError{Status: resp.StatusCode, Message: body.Error.Message}
		if len(body.Error.Errors) > 0 {
			ae.Reason = body.Error.Errors[0].Reason
		}
		return nil, ae
	}
	return source.ReadLimited(resp.Body, max)
}

// trusted reports whether an address may receive a Google token: the API
// itself, or the Google hosts that serve exports and downloads.
func (m *Manager) trusted(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if api, err := url.Parse(m.endpoints().API); err == nil && u.Scheme == api.Scheme && u.Host == api.Host {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "https" && (host == "www.googleapis.com" || host == "docs.google.com" || strings.HasSuffix(host, ".googleusercontent.com"))
}

// getJSON calls the Drive API and decodes its answer.
func (m *Manager) getJSON(ctx context.Context, account, path string, q url.Values, v any) error {
	u := m.endpoints().API + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	data, err := m.fetch(ctx, account, u, 64<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Drive is a drive an account can watch: My Drive (empty id) or a shared
// drive.
type Drive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Drives lists My Drive and the shared drives of an account.
func (m *Manager) Drives(ctx context.Context, account string) ([]Drive, error) {
	out := []Drive{{ID: "", Name: "My Drive"}}
	page := ""
	for {
		var resp struct {
			NextPageToken string  `json:"nextPageToken"`
			Drives        []Drive `json:"drives"`
		}
		q := url.Values{"pageSize": {"100"}, "fields": {"nextPageToken,drives(id,name)"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		if err := m.getJSON(ctx, account, "/drives", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Drives...)
		if page = resp.NextPageToken; page == "" {
			return out, nil
		}
	}
}

// Folder is a Drive folder chosen as the root of a source.
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveID   string `json:"drive_id"`
	DriveName string `json:"drive_name"`
}

var fileID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// FolderRef extracts a folder id from a Drive link or a bare id.
func FolderRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if u, err := url.Parse(ref); err == nil && u.Host != "" {
		if id := u.Query().Get("id"); id != "" {
			return id
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, p := range parts {
			if p == "folders" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		return ""
	}
	return ref
}

// LookupFolder checks that a link or an id names a folder the account can
// read, and returns its name and drive.
func (m *Manager) LookupFolder(ctx context.Context, account, ref string) (Folder, error) {
	id := FolderRef(ref)
	if !fileID.MatchString(id) {
		return Folder{}, errors.New("not a Google Drive folder link")
	}
	var f file
	q := url.Values{"fields": {"id,name,mimeType,driveId,trashed"}, "supportsAllDrives": {"true"}}
	if err := m.getJSON(ctx, account, "/files/"+url.PathEscape(id), q, &f); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return Folder{}, errors.New("this folder does not exist or the account cannot read it")
		}
		return Folder{}, err
	}
	if f.MimeType != folderMime || f.Trashed {
		return Folder{}, errors.New("this link is not a folder")
	}
	out := Folder{ID: f.ID, Name: f.Name, DriveID: f.DriveID}
	if f.DriveID != "" {
		var d Drive
		if err := m.getJSON(ctx, account, "/drives/"+url.PathEscape(f.DriveID), url.Values{"fields": {"id,name"}}, &d); err == nil {
			out.DriveName = d.Name
		}
	}
	return out, nil
}

// NewSource returns the source of a Google Drive configuration. Its cache
// is kept in dir.
func (m *Manager) NewSource(cfg config.Source, dir string) *Source {
	return &Source{m: m, cfg: cfg, path: filepath.Join(dir, cfg.ID+".json")}
}

// PruneCaches deletes the caches of the sources no longer configured.
func PruneCaches(dir string, sources []config.Source) {
	keep := map[string]bool{}
	for _, s := range sources {
		keep[s.ID+".json"] = true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), config.SourceGoogleDrive+"-") && !keep[e.Name()] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
