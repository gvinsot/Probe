// Package gdrive reads Google Drive through its API: a Google Drive source
// needs no synchronization client, covers Google Docs, Sheets and Slides
// (exported as Word, Excel and PowerPoint documents) and records baselines
// by revision, so that nothing is downloaded until a document changes.
//
// Accounts are connected with the OAuth flow for installed applications:
// the consent page opens in the default browser, Google redirects to a
// one-shot listener on the loopback interface and PKCE binds the code to
// this process. The refresh token goes to the system keychain; the access
// tokens only live in memory. The only scope is drive.readonly: Probe never
// changes a file.
package gdrive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints are the Google addresses; tests point them to a local server.
type Endpoints struct {
	Auth   string // consent page
	Token  string // code exchange and refresh
	Revoke string // token revocation
	API    string // Drive API v3
}

// Google are the production endpoints.
var Google = Endpoints{
	Auth:   "https://accounts.google.com/o/oauth2/v2/auth",
	Token:  "https://oauth2.googleapis.com/token",
	Revoke: "https://oauth2.googleapis.com/revoke",
	API:    "https://www.googleapis.com/drive/v3",
}

// Scope reads the files and their revisions; nothing is ever written.
const Scope = "https://www.googleapis.com/auth/drive.readonly"

// OAuthClient identifies the application to Google: a "Desktop app" OAuth
// client of a Google Cloud project where the Drive API is enabled. Google
// requires its secret at the code exchange even though an installed
// application cannot keep it confidential; PKCE is what protects the flow.
type OAuthClient struct {
	ID     string
	Secret string
}

// DefaultClientID and DefaultClientSecret name an OAuth client built into
// the application with -ldflags "-X". Empty, each company creates its own
// client (an internal application of its Google Workspace needs no review
// by Google) and enters it in the settings.
var DefaultClientID, DefaultClientSecret string

var (
	// ErrNoClient reports that no OAuth client is configured.
	ErrNoClient = errors.New("no Google OAuth client is configured: enter the client id of your Google Cloud project in the settings")
	// ErrRevoked reports a refresh token Google no longer accepts.
	ErrRevoked = errors.New("the Google account must be connected again (access revoked or expired)")
)

// token is an OAuth token of one account.
type token struct {
	Access  string
	Refresh string
	Expiry  time.Time
}

func (t token) valid() bool { return t.Access != "" && time.Until(t.Expiry) > time.Minute }

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// exchange posts a grant to the token endpoint.
func (m *Manager) exchange(ctx context.Context, form url.Values) (token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints().Token, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return token{}, fmt.Errorf("google token endpoint: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		if tr.Error == "invalid_grant" {
			return token{}, ErrRevoked
		}
		return token{}, fmt.Errorf("google token endpoint: %s %s", tr.Error, tr.Description)
	}
	return token{
		Access:  tr.AccessToken,
		Refresh: tr.RefreshToken,
		Expiry:  time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// authorize runs the consent flow and returns the tokens of the account the
// person chose.
func (m *Manager) authorize(ctx context.Context, c OAuthClient) (token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return token{}, err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	state := randomString()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			// Any local process can reach this listener: only the redirect
			// carrying the state of this flow is taken.
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				http.Error(w, "invalid state", http.StatusBadRequest)
				return
			}
			res := result{code: q.Get("code")}
			if e := q.Get("error"); e != "" || res.code == "" {
				if e == "" {
					e = "no authorization code"
				}
				res.err = fmt.Errorf("google did not authorize the connection: %s", e)
			}
			callbackPage(w, res.err)
			select {
			case done <- res:
			default:
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	q := url.Values{
		"client_id":             {c.ID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {Scope},
		"access_type":           {"offline"},
		"prompt":                {"consent select_account"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	if err := m.open(m.endpoints().Auth + "?" + q.Encode()); err != nil {
		return token{}, fmt.Errorf("open the browser: %w", err)
	}
	select {
	case <-ctx.Done():
		return token{}, errors.New("the Google connection was not completed in time")
	case res := <-done:
		if res.err != nil {
			return token{}, res.err
		}
		return m.exchange(ctx, url.Values{
			"code":          {res.code},
			"client_id":     {c.ID},
			"client_secret": {c.Secret},
			"redirect_uri":  {redirect},
			"grant_type":    {"authorization_code"},
			"code_verifier": {verifier},
		})
	}
}

func callbackPage(w http.ResponseWriter, err error) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cache-Control", "no-store")
	msg := "Probe Desktop is connected to your Google account. You can close this tab."
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		msg = "The connection to Google failed. Close this tab and try again from Probe Desktop."
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Probe Desktop</title><p style="font:16px system-ui;margin:3em">%s</p>`, msg)
}

// randomString returns 43 URL-safe characters, a valid PKCE verifier.
func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
