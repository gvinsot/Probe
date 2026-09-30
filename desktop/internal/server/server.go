// Package server serves the Probe Desktop interface on the loopback
// interface. The window (or a browser) is only a client of this server; the
// engine keeps running and watching when no window is open.
//
// The server holds document contents and an API key, so it only answers the
// process that the engine itself launched:
//   - it listens on 127.0.0.1 on a random port chosen at each start;
//   - the Host header must name that address, which defeats DNS rebinding;
//   - the window opens a single-use launch URL that trades a short-lived
//     code for an HttpOnly, SameSite=Strict session cookie;
//   - state-changing requests must carry a custom header and, when present,
//     a same-origin Origin, so another web page cannot forge them;
//   - control requests from a second launch carry the control token
//     published in the user-only instance file.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/gdrive"
	"github.com/gvinsot/Probe/desktop/internal/instance"
	"github.com/gvinsot/Probe/desktop/internal/office"
	"github.com/gvinsot/Probe/desktop/internal/platform"
	"github.com/gvinsot/Probe/desktop/internal/reviewer"
	"github.com/gvinsot/Probe/desktop/internal/secret"
	"github.com/gvinsot/Probe/desktop/internal/watch"
	"github.com/gvinsot/Probe/desktop/web"
)

const (
	cookieName    = "probe_session"
	requestHeader = "X-Probe-Request"
	codeLifetime  = 2 * time.Minute
)

// Keys abstracts the keychain so tests do not touch the real one.
type Keys interface {
	Get(provider string) (string, error)
	Set(provider, key string) error
}

// SystemKeys stores keys in the operating system keychain.
type SystemKeys struct{}

func (SystemKeys) Get(p string) (string, error) { return secret.Get(p) }
func (SystemKeys) Set(p, k string) error        { return secret.Set(p, k) }

// Google connects Google accounts and browses their drives.
type Google interface {
	Connect() error
	ConnectStatus() gdrive.ConnectStatus
	Disconnect(account string) error
	Drives(ctx context.Context, account string) ([]gdrive.Drive, error)
	LookupFolder(ctx context.Context, account, ref string) (gdrive.Folder, error)
}

// Deps are the engine services the interface drives.
type Deps struct {
	Settings *config.Store
	Watcher  *watch.Watcher
	Keys     Keys
	Log      *slog.Logger
	Version  string
	// Google is nil when Google Drive sources are not available.
	Google Google
	// OnShow opens the window (control request from a second launch).
	OnShow func()
	// OnSettings runs after the settings changed.
	OnSettings func()
	// Open opens a document with its default application, or a web
	// address in the default browser.
	Open func(target string) error
}

// Server is the loopback HTTP server of the interface.
type Server struct {
	deps    Deps
	ln      net.Listener
	srv     *http.Server
	port    int
	session string
	control string

	mu    sync.Mutex
	codes map[string]time.Time

	auto *autoExplainer
}

// New listens on a random loopback port.
func New(deps Deps) (*Server, error) {
	if deps.Keys == nil {
		deps.Keys = SystemKeys{}
	}
	if deps.Open == nil {
		deps.Open = platform.Open
	}
	if deps.OnShow == nil {
		deps.OnShow = func() {}
	}
	if deps.OnSettings == nil {
		deps.OnSettings = func() {}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		deps:    deps,
		ln:      ln,
		port:    ln.Addr().(*net.TCPAddr).Port,
		session: randomToken(),
		control: randomToken(),
		codes:   map[string]time.Time{},
	}
	s.auto = newAutoExplainer(s)
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Port is the listening port.
func (s *Server) Port() int { return s.port }

// ControlToken authenticates control requests from a second launch.
func (s *Server) ControlToken() string { return s.control }

// LaunchURL returns a single-use URL that opens an authenticated session.
func (s *Server) LaunchURL() string {
	code := randomToken()
	s.mu.Lock()
	now := time.Now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = now.Add(codeLifetime)
	s.mu.Unlock()
	return fmt.Sprintf("%s/launch?code=%s", s.origin(), code)
}

func (s *Server) origin() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

// Serve blocks until Shutdown.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.auto.stop()
	return s.srv.Shutdown(ctx)
}

// Handler returns the full HTTP handler, security checks included.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.state)
	api.HandleFunc("GET /api/documents/{id}", s.document)
	api.HandleFunc("POST /api/documents/{id}/accept", s.accept)
	api.HandleFunc("POST /api/documents/{id}/explain", s.explain)
	api.HandleFunc("POST /api/documents/{id}/open", s.open)
	api.HandleFunc("GET /api/reviewed", s.reviewed)
	api.HandleFunc("GET /api/reviewed/{id}", s.review)
	api.HandleFunc("POST /api/scan", s.scan)
	api.HandleFunc("GET /api/settings", s.getSettings)
	api.HandleFunc("PUT /api/settings", s.putSettings)
	api.HandleFunc("POST /api/google/connect", s.googleConnect)
	api.HandleFunc("GET /api/google/connect", s.googleStatus)
	api.HandleFunc("DELETE /api/google/accounts/{account}", s.googleDisconnect)
	api.HandleFunc("GET /api/google/drives", s.googleDrives)
	api.HandleFunc("GET /api/google/folder", s.googleFolder)
	assets, _ := fs.Sub(web.Assets, "public")
	api.Handle("GET /", http.FileServerFS(assets))

	root := http.NewServeMux()
	root.HandleFunc("GET /launch", s.launch)
	root.HandleFunc("POST /control/{action}", s.controlAction)
	root.Handle("/", s.requireSession(api))
	return s.guard(root)
}

// guard applies the checks every request goes through.
func (s *Server) guard(next http.Handler) http.Handler {
	allowedHosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", s.port): true,
		fmt.Sprintf("localhost:%d", s.port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "session expired: open Probe Desktop from its icon")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Probe Desktop</title><p>Open Probe Desktop from its icon in the taskbar or the menu bar.</p>`)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(requestHeader) != "1" {
				writeError(w, http.StatusForbidden, "missing request header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != s.origin() && o != fmt.Sprintf("http://localhost:%d", s.port) {
				writeError(w, http.StatusForbidden, "cross-origin request")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	s.mu.Lock()
	exp, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if code == "" || !ok || time.Now().After(exp) {
		http.Error(w, "this link has expired: open Probe Desktop from its icon", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) controlAction(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(instance.ControlHeader)), []byte(s.control)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.PathValue("action") {
	case "show":
		go s.deps.OnShow()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

type stateResponse struct {
	watch.State
	Version    string `json:"version"`
	Sources    int    `json:"sources"`
	Configured bool   `json:"ai_configured"`
	// Explaining lists the documents an AI explanation is being written for.
	Explaining []string `json:"explaining"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st := s.deps.Settings.Get()
	writeJSON(w, stateResponse{
		State:      s.deps.Watcher.State(),
		Version:    s.deps.Version,
		Sources:    len(st.Sources),
		Configured: s.aiConfigured(st),
		Explaining: s.auto.running(),
	})
}

func (s *Server) aiConfigured(st config.Settings) bool {
	if st.Provider == config.ProviderNone {
		return false
	}
	key, _ := s.deps.Keys.Get(st.Provider)
	return key != "" || st.BaseURL != ""
}

func (s *Server) document(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deps.Watcher.Document(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown document")
		return
	}
	writeJSON(w, d)
}

// reviewed lists the latest reviews, most recent first.
func (s *Server) reviewed(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.deps.Watcher.History())
}

// review returns a past review with the report that was approved.
func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	rv, ok := s.deps.Watcher.Review(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown review")
		return
	}
	writeJSON(w, rv)
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	err := s.deps.Watcher.Accept(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, watch.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, watch.ErrStale):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) explain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.deps.Watcher.Document(id)
	if !ok || d.Report == nil {
		writeError(w, http.StatusNotFound, "no report to explain")
		return
	}
	s.auto.begin(id)
	e, err := s.explainDocument(r.Context(), d)
	s.auto.end(id)
	switch {
	case errors.Is(err, errKeychain):
		writeError(w, http.StatusInternalServerError, err.Error())
	case errors.Is(err, reviewer.ErrNotConfigured):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, watch.ErrStale), errors.Is(err, watch.ErrNotFound):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, e)
	}
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deps.Watcher.Document(r.PathValue("id"))
	if !ok || d.Status == watch.StatusRemoved {
		writeError(w, http.StatusNotFound, "unknown document")
		return
	}
	target := d.Path
	switch {
	case d.Link != "":
		// A drive document opens in the browser, and only on Google.
		if !googleLink(d.Link) {
			writeError(w, http.StatusBadRequest, "not a document link")
			return
		}
		target = d.Link
	case office.KindOf(d.Path) == "":
		// Only paths the watcher found are opened, and only Office documents.
		writeError(w, http.StatusBadRequest, "not a document")
		return
	}
	if err := s.deps.Open(target); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// googleLink accepts the addresses Drive gives to open a document.
func googleLink(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := u.Hostname()
	return host == "docs.google.com" || host == "drive.google.com"
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	s.deps.Watcher.ScanNow()
	w.WriteHeader(http.StatusAccepted)
}

type settingsResponse struct {
	config.Settings
	Keys          map[string]bool      `json:"keys"`
	CloudFolders  []config.CloudFolder `json:"cloud_folders"`
	DefaultModels map[string]string    `json:"default_models"`
	Google        googleInfo           `json:"google"`
}

// googleInfo tells the interface what Google Drive needs.
type googleInfo struct {
	// Available is false when the engine was built without Google Drive.
	Available bool `json:"available"`
	// BuiltinClient is true when the application carries an OAuth client,
	// so the person does not need to enter one.
	BuiltinClient bool `json:"builtin_client"`
	// SecretSaved is true when the secret of the configured client is in
	// the keychain.
	SecretSaved bool `json:"secret_saved"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	keys := map[string]bool{}
	for _, p := range []string{config.ProviderAnthropic, config.ProviderOpenAI} {
		k, _ := s.deps.Keys.Get(p)
		keys[p] = k != ""
	}
	secret, _ := s.deps.Keys.Get(gdrive.ClientSecretKey)
	writeJSON(w, settingsResponse{
		Settings:     s.deps.Settings.Get(),
		Keys:         keys,
		CloudFolders: config.DetectCloudFolders(),
		DefaultModels: map[string]string{
			config.ProviderAnthropic: config.DefaultAnthropicModel,
			config.ProviderOpenAI:    config.DefaultOpenAIModel,
		},
		Google: googleInfo{
			Available:     s.deps.Google != nil,
			BuiltinClient: gdrive.DefaultClientID != "",
			SecretSaved:   secret != "",
		},
	})
}

type settingsRequest struct {
	config.Settings
	// APIKey, when set, is stored in the keychain for the chosen provider.
	APIKey string `json:"api_key"`
	// ClearKey deletes the stored key of the chosen provider.
	ClearKey bool `json:"clear_key"`
	// GoogleClientSecret, when set, is stored in the keychain.
	GoogleClientSecret string `json:"google_client_secret"`
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings")
		return
	}
	// The connected accounts only change through the connection flow.
	req.Settings.GoogleAccounts = s.deps.Settings.Get().GoogleAccounts
	if err := s.deps.Settings.Save(req.Settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.storeClientSecret(w, req.GoogleClientSecret) {
		return
	}
	provider := s.deps.Settings.Get().Provider
	if provider != config.ProviderNone {
		switch {
		case req.ClearKey:
			if err := s.deps.Keys.Set(provider, ""); err != nil {
				writeError(w, http.StatusInternalServerError, "cannot delete the API key: "+err.Error())
				return
			}
		case strings.TrimSpace(req.APIKey) != "":
			if err := s.deps.Keys.Set(provider, strings.TrimSpace(req.APIKey)); err != nil {
				writeError(w, http.StatusInternalServerError, "cannot store the API key in the keychain: "+err.Error())
				return
			}
		}
	}
	s.deps.Log.Info("settings saved", "sources", len(s.deps.Settings.Get().Sources), "provider", provider)
	s.deps.OnSettings()
	// A new provider, model or key deserves a new attempt on the documents
	// whose explanation failed.
	s.auto.retry()
	s.getSettings(w, r)
}

func (s *Server) storeClientSecret(w http.ResponseWriter, secret string) bool {
	if secret = strings.TrimSpace(secret); secret == "" {
		return true
	}
	if err := s.deps.Keys.Set(gdrive.ClientSecretKey, secret); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store the Google client secret in the keychain: "+err.Error())
		return false
	}
	return true
}

func (s *Server) google(w http.ResponseWriter) (Google, bool) {
	if s.deps.Google == nil {
		writeError(w, http.StatusNotFound, "Google Drive is not available in this version")
		return nil, false
	}
	return s.deps.Google, true
}

// connectRequest can carry the OAuth client typed in the settings, saved
// before the consent page opens.
type connectRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (s *Server) googleConnect(w http.ResponseWriter, r *http.Request) {
	g, ok := s.google(w)
	if !ok {
		return
	}
	var req connectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if id := strings.TrimSpace(req.ClientID); id != "" {
		if err := s.deps.Settings.Update(func(st *config.Settings) { st.GoogleClientID = id }); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if !s.storeClientSecret(w, req.ClientSecret) {
		return
	}
	if err := g.Connect(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) googleStatus(w http.ResponseWriter, r *http.Request) {
	g, ok := s.google(w)
	if !ok {
		return
	}
	writeJSON(w, g.ConnectStatus())
}

func (s *Server) googleDisconnect(w http.ResponseWriter, r *http.Request) {
	g, ok := s.google(w)
	if !ok {
		return
	}
	account := strings.ToLower(r.PathValue("account"))
	for _, src := range s.deps.Settings.Get().Sources {
		if src.Type == config.SourceGoogleDrive && src.Account == account {
			writeError(w, http.StatusConflict, "remove the Google Drive sources of this account first")
			return
		}
	}
	if err := g.Disconnect(account); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete the Google token: "+err.Error())
		return
	}
	err := s.deps.Settings.Update(func(st *config.Settings) {
		kept := []string{}
		for _, a := range st.GoogleAccounts {
			if a != account {
				kept = append(kept, a)
			}
		}
		st.GoogleAccounts = kept
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.deps.Log.Info("google account disconnected", "account", account)
	w.WriteHeader(http.StatusNoContent)
}

// connectedAccount returns the account named by a request, which must be
// connected.
func (s *Server) connectedAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	account := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("account")))
	for _, a := range s.deps.Settings.Get().GoogleAccounts {
		if a == account {
			return account, true
		}
	}
	writeError(w, http.StatusBadRequest, "this Google account is not connected")
	return "", false
}

func (s *Server) googleDrives(w http.ResponseWriter, r *http.Request) {
	g, ok := s.google(w)
	if !ok {
		return
	}
	account, ok := s.connectedAccount(w, r)
	if !ok {
		return
	}
	drives, err := g.Drives(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, drives)
}

func (s *Server) googleFolder(w http.ResponseWriter, r *http.Request) {
	g, ok := s.google(w)
	if !ok {
		return
	}
	account, ok := s.connectedAccount(w, r)
	if !ok {
		return
	}
	folder, err := g.LookupFolder(r.Context(), account, r.URL.Query().Get("ref"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, folder)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
