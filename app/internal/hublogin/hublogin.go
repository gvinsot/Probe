// Package hublogin connects the CLI to a Probe Hub account so the reviewer can
// use the hub's LLM gateway without a provider key of its own.
//
// `probe login` runs the device authorization flow (RFC 8628): the hub prints
// nothing secret, the user approves the request in a browser where they are
// signed in, and the CLI receives a token that reaches only the gateway, is
// limited and counted per account, and is revoked by `probe logout` or from
// the hub's dashboard. The token is stored in the user configuration
// directory, readable by the user only. CI passes it as PROBE_HUB_TOKEN.
//
// The gateway is a fallback: a deployment that configures a provider endpoint
// or credential keeps using it, and nothing here is consulted.
package hublogin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultHub is the public hub `probe login` connects to.
	DefaultHub = "https://app.probe.technology"
	// HubURLEnv selects another hub, for login and for HubTokenEnv.
	HubURLEnv = "PROBE_HUB_URL"
	// HubTokenEnv holds an llm token created in the hub's dashboard, for
	// machines that cannot open a browser, such as CI.
	HubTokenEnv = "PROBE_HUB_TOKEN"
	// CredentialsFileEnv names the credential file; Off disables it.
	CredentialsFileEnv = "PROBE_CREDENTIALS_FILE"
	Off                = "off"
	// GatewayPath is the gateway's OpenAI-compatible base, under the hub URL.
	GatewayPath = "/llm/v1"
	// PlaceholderModel is sent when the token comes from HubTokenEnv: the
	// gateway always imposes the deployment's model.
	PlaceholderModel = "probe-hub"
	// maxCredentialsBytes bounds the credential file.
	maxCredentialsBytes = 16 << 10
)

// Credentials is what `probe login` stores.
type Credentials struct {
	Hub       string    `json:"hub"`
	Endpoint  string    `json:"endpoint"`
	Model     string    `json:"model"`
	Token     string    `json:"token"`
	Login     string    `json:"login,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Gateway is the reviewer configuration a login provides.
type Gateway struct {
	Endpoint string
	Model    string
	Token    string
	// Source describes where it came from, for the reviewer configuration line.
	Source string
}

// NormalizeHub checks a hub URL: HTTPS, or HTTP on the loopback interface
// for a local hub. The token is sent to it, so nothing else is accepted.
func NormalizeHub(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("the hub URL must be an absolute URL without credentials, query or fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("the hub URL must use HTTPS")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Path returns the credential file, or "" when PROBE_CREDENTIALS_FILE=off or
// no user configuration directory can be derived. getenv is injected so that
// resolution never depends on the process environment in tests.
func Path(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv(CredentialsFileEnv)); v != "" {
		if strings.EqualFold(v, Off) {
			return ""
		}
		return v
	}
	dir := configDir(getenv)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "probe", "credentials.json")
}

// configDir mirrors os.UserConfigDir over an injected environment.
func configDir(getenv func(string) string) string {
	var dir string
	switch runtime.GOOS {
	case "windows":
		dir = getenv("AppData")
	case "darwin", "ios":
		if home := getenv("HOME"); home != "" {
			dir = filepath.Join(home, "Library", "Application Support")
		}
	case "plan9":
		if home := getenv("home"); home != "" {
			dir = filepath.Join(home, "lib")
		}
	default:
		dir = getenv("XDG_CONFIG_HOME")
		if dir == "" || !filepath.IsAbs(dir) {
			dir = ""
			if home := getenv("HOME"); home != "" {
				dir = filepath.Join(home, ".config")
			}
		}
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}

// Parse decodes and checks a credential file.
func Parse(data []byte) (Credentials, error) {
	if len(data) > maxCredentialsBytes {
		return Credentials{}, errors.New("credential file exceeds 16 KiB")
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, errors.New("credential file is not valid JSON")
	}
	hub, err := NormalizeHub(c.Hub)
	if err != nil {
		return Credentials{}, err
	}
	endpoint, err := NormalizeHub(c.Endpoint)
	if err != nil || !strings.HasPrefix(endpoint, hub+"/") {
		return Credentials{}, errors.New("credential file names a gateway outside its hub")
	}
	if c.Token == "" || strings.ContainsAny(c.Token, "\r\n\x00 ") || len(c.Token) > 512 {
		return Credentials{}, errors.New("credential file holds no usable token")
	}
	if strings.TrimSpace(c.Model) == "" {
		c.Model = PlaceholderModel
	}
	c.Hub, c.Endpoint = hub, endpoint
	return c, nil
}

// Load reads the credential file. A missing file, or a switched-off one, is
// not an error: found is then false.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Credentials, string, bool, error) {
	path := Path(getenv)
	if path == "" {
		return Credentials{}, "", false, nil
	}
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credentials{}, path, false, nil
	}
	if err != nil {
		return Credentials{}, path, false, fmt.Errorf("probe login credentials %s: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return Credentials{}, path, false, fmt.Errorf("probe login credentials %s: %w (run probe login again, or probe logout)", path, err)
	}
	return c, path, true, nil
}

// Resolve returns the gateway settings of this machine: PROBE_HUB_TOKEN
// first, then the credential file of `probe login`, unless it has expired.
func Resolve(getenv func(string) string, readFile func(string) ([]byte, error), now time.Time) (Gateway, bool, error) {
	if token := strings.TrimSpace(getenv(HubTokenEnv)); token != "" {
		hub := DefaultHub
		if v := strings.TrimSpace(getenv(HubURLEnv)); v != "" {
			hub = v
		}
		hub, err := NormalizeHub(hub)
		if err != nil {
			return Gateway{}, false, fmt.Errorf("%s: %w", HubURLEnv, err)
		}
		if strings.ContainsAny(token, "\r\n\x00 ") || len(token) > 512 {
			return Gateway{}, false, fmt.Errorf("%s is not a usable token", HubTokenEnv)
		}
		return Gateway{Endpoint: hub + GatewayPath, Model: PlaceholderModel, Token: token, Source: "Probe Hub LLM gateway " + hub + " with " + HubTokenEnv}, true, nil
	}
	c, _, found, err := Load(getenv, readFile)
	if err != nil || !found {
		return Gateway{}, false, err
	}
	if !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt) {
		return Gateway{}, false, nil
	}
	who := "probe login"
	if c.Login != "" {
		who += " as " + c.Login
	}
	return Gateway{Endpoint: c.Endpoint, Model: c.Model, Token: c.Token, Source: "Probe Hub LLM gateway " + c.Hub + " (" + who + ")"}, true, nil
}

// Save writes the credentials, readable by the user only, replacing any
// previous file atomically.
func Save(path string, c Credentials) error {
	if path == "" {
		return fmt.Errorf("no credential file: %s=off or no user configuration directory", CredentialsFileEnv)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Client talks to a hub.
type Client struct {
	HTTP *http.Client
	// Wait sleeps between polls; nil waits for real.
	Wait func(context.Context, time.Duration) error
}

// NewClient returns a client that never follows a redirect: the token must
// reach the hub it was issued by and nothing else.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the hub redirected the request")
	}}}
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.Wait != nil {
		return c.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Prompt is what the user must do to approve a login.
type Prompt struct {
	UserCode        string
	VerificationURI string
	CompleteURI     string
	ExpiresIn       time.Duration
}

// Login runs the device flow against hub. prompt is called once, with what
// to show the user; Login then polls until the request is approved, denied
// or expired.
func (c *Client) Login(ctx context.Context, hub, client string, prompt func(Prompt)) (Credentials, error) {
	hub, err := NormalizeHub(hub)
	if err != nil {
		return Credentials{}, err
	}
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		CompleteURI     string `json:"verification_uri_complete"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	status, err := c.post(ctx, hub+"/api/device/code", "", map[string]string{"client": client}, &start)
	if err != nil {
		return Credentials{}, err
	}
	if status == http.StatusNotFound {
		return Credentials{}, fmt.Errorf("%s does not lend its LLM (or is not a Probe Hub)", hub)
	}
	if status != http.StatusOK || start.DeviceCode == "" || start.UserCode == "" {
		return Credentials{}, fmt.Errorf("the hub refused to start the login (HTTP %d)", status)
	}
	// The page the user opens must belong to the hub being logged into.
	for _, link := range []string{start.VerificationURI, start.CompleteURI} {
		if link != "" && !strings.HasPrefix(link, hub+"/") {
			return Credentials{}, errors.New("the hub sent an approval page outside its own address")
		}
	}
	expires := time.Duration(start.ExpiresIn) * time.Second
	if expires <= 0 || expires > time.Hour {
		expires = 10 * time.Minute
	}
	prompt(Prompt{UserCode: start.UserCode, VerificationURI: start.VerificationURI, CompleteURI: start.CompleteURI, ExpiresIn: expires})

	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second || interval > time.Minute {
		interval = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, expires)
	defer cancel()
	for {
		if err := c.wait(ctx, interval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return Credentials{}, errors.New("the code expired before it was approved; run probe login again")
			}
			return Credentials{}, err
		}
		var reply struct {
			Error       string `json:"error"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
			Endpoint    string `json:"endpoint"`
			Model       string `json:"model"`
			Login       string `json:"login"`
			Provider    string `json:"provider"`
		}
		status, err := c.post(ctx, hub+"/api/device/token", "", map[string]string{"device_code": start.DeviceCode}, &reply)
		if err != nil {
			return Credentials{}, err
		}
		switch {
		case status == http.StatusOK:
			creds := Credentials{Hub: hub, Endpoint: reply.Endpoint, Model: reply.Model, Token: reply.AccessToken, Login: reply.Login, Provider: reply.Provider}
			if reply.ExpiresIn > 0 {
				creds.ExpiresAt = time.Now().UTC().Add(time.Duration(reply.ExpiresIn) * time.Second).Truncate(time.Second)
			}
			data, _ := json.Marshal(creds)
			if creds, err = Parse(data); err != nil {
				return Credentials{}, fmt.Errorf("the hub returned unusable credentials: %w", err)
			}
			return creds, nil
		case reply.Error == "authorization_pending":
		case reply.Error == "slow_down":
			interval += 5 * time.Second
		case reply.Error == "access_denied":
			return Credentials{}, errors.New("the login was denied in the browser")
		case reply.Error == "expired_token" || reply.Error == "invalid_grant":
			return Credentials{}, errors.New("the code expired before it was approved; run probe login again")
		case reply.Error != "":
			return Credentials{}, fmt.Errorf("the hub refused the login: %s", reply.Error)
		default:
			return Credentials{}, fmt.Errorf("the hub refused the login (HTTP %d)", status)
		}
	}
}

// Account is what the hub reports about a login.
type Account struct {
	Login       string    `json:"login"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	DailyTokens int64     `json:"daily_tokens"`
	UsedToday   int64     `json:"used_today"`
	ResetsAt    time.Time `json:"resets_at"`
}

// ErrUnauthorized means the hub no longer accepts the token.
var ErrUnauthorized = errors.New("the hub no longer accepts this login: run probe login again")

// Account asks the hub which account the credentials stand for.
func (c *Client) Account(ctx context.Context, creds Credentials) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, creds.Endpoint+"/account", nil)
	if err != nil {
		return Account{}, err
	}
	var out Account
	status, err := c.do(req, creds.Token, &out)
	if err != nil {
		return Account{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return Account{}, ErrUnauthorized
	}
	if status != http.StatusOK {
		return Account{}, fmt.Errorf("the hub answered HTTP %d", status)
	}
	return out, nil
}

// Revoke asks the hub to revoke the token. A token the hub no longer knows
// is already revoked.
func (c *Client) Revoke(ctx context.Context, creds Credentials) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, creds.Endpoint+"/token", nil)
	if err != nil {
		return err
	}
	status, err := c.do(req, creds.Token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusUnauthorized {
		return fmt.Errorf("the hub answered HTTP %d", status)
	}
	return nil
}

func (c *Client) post(ctx context.Context, target, token string, body, out any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, token, out)
}

// do sends a request and decodes a bounded JSON reply into out, whatever the
// status: the device flow reports its states as errors.
func (c *Client) do(req *http.Request, token string, out any) (int, error) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "probe-cli")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		// The transport error can echo the URL; it holds no secret.
		return 0, fmt.Errorf("cannot reach the hub: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, errors.New("the hub's answer could not be read")
	}
	if out != nil && len(data) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(data, out)
	}
	return res.StatusCode, nil
}
