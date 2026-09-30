// Package gdocs reads Google Docs (design documents, requirement documents)
// and renders them as context for the intent of a change, so that review,
// lint and plan understand a code change against the documents it
// implements. It is unrelated to the desktop application, which compares
// versions of Office documents: here documents explain code.
//
// A document is exported as Markdown through the Google Drive API. Like a
// Notion page, it is context, not a ticket: its list items are not
// acceptance criteria unless the document puts them under an "Acceptance
// criteria" heading (issuetext.ProseLists). The document is untrusted text,
// fed through the usual intent parsing; nothing is executed.
//
// Credentials come from the operator's environment, never from the
// repository, in this order:
//
//   - PROBE_GOOGLE_ACCESS_TOKEN: an OAuth access token with a Drive read
//     scope (for example from `gcloud auth print-access-token` or workload
//     identity federation), also from its _FILE or Docker secret;
//   - a service-account key (JSON) named by PROBE_GOOGLE_CREDENTIALS, or
//     mounted at /run/secrets/PROBE_GOOGLE_CREDENTIALS, or named by
//     GOOGLE_APPLICATION_CREDENTIALS: Probe signs a JWT with it and
//     exchanges it for a drive.readonly token; share the documents with the
//     service account's email;
//   - PROBE_GOOGLE_API_KEY: an API key, which only reads documents shared
//     publicly ("anyone with the link").
package gdocs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/issuetext"
)

// Environment variables read by FromEnv.
const (
	AccessTokenEnv       = "PROBE_GOOGLE_ACCESS_TOKEN"
	CredentialsEnv       = "PROBE_GOOGLE_CREDENTIALS"
	ApplicationCredsEnv  = "GOOGLE_APPLICATION_CREDENTIALS"
	APIKeyEnv            = "PROBE_GOOGLE_API_KEY"
	APIURLEnv            = "PROBE_GOOGLE_API_URL"
	TokenURLEnv          = "PROBE_GOOGLE_TOKEN_URL"
	AllowInsecureHTTPEnv = "PROBE_GOOGLE_ALLOW_INSECURE_HTTP"
)

// Defaults of the Google endpoints.
const (
	DefaultAPIURL   = "https://www.googleapis.com"
	DefaultTokenURL = "https://oauth2.googleapis.com/token"
	// Scope is the only scope Probe asks for.
	Scope = "https://www.googleapis.com/auth/drive.readonly"
)

const (
	// MaxDocuments bounds the documents of one run.
	MaxDocuments     = 5
	maxResponseBytes = 4 << 20
	maxKeyFileBytes  = 64 << 10
	defaultTimeout   = 30 * time.Second
	docMimeType      = "application/vnd.google-apps.document"
)

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{20,128}$`)
	pathID     = regexp.MustCompile(`/d/([A-Za-z0-9_-]{20,128})`)
	queryField = regexp.MustCompile(`^[A-Za-z0-9_-]{20,128}$`)
)

// ParseDocument returns the file ID of a document given as an ID or as a
// Google Docs or Drive URL (https://docs.google.com/document/d/ID/edit,
// https://drive.google.com/file/d/ID/view, https://drive.google.com/open?id=ID).
func ParseDocument(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if !strings.Contains(ref, "://") {
		if !idPattern.MatchString(ref) {
			return "", fmt.Errorf("%q is not a Google Docs ID or URL", ref)
		}
		return ref, nil
	}
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("%q is not a Google Docs URL", ref)
	}
	switch strings.ToLower(u.Hostname()) {
	case "docs.google.com", "drive.google.com":
	default:
		return "", fmt.Errorf("%q is not a Google Docs URL", ref)
	}
	if m := pathID.FindStringSubmatch(u.Path); m != nil {
		return m[1], nil
	}
	if id := u.Query().Get("id"); queryField.MatchString(id) {
		return id, nil
	}
	return "", fmt.Errorf("%q names no document ID", ref)
}

// ParseDocuments splits a comma-separated list, parses every document and
// removes duplicates; at most MaxDocuments.
func ParseDocuments(list string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, ref := range strings.Split(list, ",") {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		id, err := ParseDocument(ref)
		if err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("name at least one Google Doc")
	}
	if len(ids) > MaxDocuments {
		return nil, fmt.Errorf("at most %d Google Docs", MaxDocuments)
	}
	return ids, nil
}

// serviceAccount is the part of a service-account key file Probe uses. The
// file's token_uri is ignored: the token endpoint comes from the environment
// only, so a key file cannot send its signed assertion elsewhere.
type serviceAccount struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	KeyID       string `json:"private_key_id"`
	key         *rsa.PrivateKey
}

// Config is the operator's Google configuration.
type Config struct {
	APIURL            string
	TokenURL          string
	AccessToken       string
	Account           *serviceAccount
	APIKey            string
	Source            string // where the credential came from, for messages
	AllowInsecureHTTP bool
}

// Configured reports whether any credential is available.
func (c Config) Configured() bool {
	return c.AccessToken != "" || c.Account != nil || c.APIKey != ""
}

// FromEnv reads the configuration; getenv and readFile are injected for
// tests. No credential is not an error (Configured is false); an unusable
// value is.
func FromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	c := Config{
		APIURL:   strings.TrimRight(strings.TrimSpace(getenv(APIURLEnv)), "/"),
		TokenURL: strings.TrimSpace(getenv(TokenURLEnv)),
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	if c.TokenURL == "" {
		c.TokenURL = DefaultTokenURL
	}
	if v := strings.TrimSpace(getenv(AllowInsecureHTTPEnv)); v != "" {
		allowed, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", AllowInsecureHTTPEnv)
		}
		c.AllowInsecureHTTP = allowed
	}
	for name, value := range map[string]string{APIURLEnv: c.APIURL, TokenURLEnv: c.TokenURL} {
		if err := checkURL(name, value, c.AllowInsecureHTTP); err != nil {
			return Config{}, err
		}
	}
	token, source, err := issuetext.Secret(AccessTokenEnv, getenv, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("Google access token: %w", err)
	}
	if token != "" {
		c.AccessToken, c.Source = token, source
		return c, nil
	}
	account, source, err := loadServiceAccount(getenv, readFile)
	if err != nil {
		return Config{}, err
	}
	if account != nil {
		c.Account, c.Source = account, source
		return c, nil
	}
	key, source, err := issuetext.Secret(APIKeyEnv, getenv, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("Google API key: %w", err)
	}
	c.APIKey, c.Source = key, source
	return c, nil
}

func checkURL(name, value string, insecure bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials, query or fragment", name)
	}
	switch {
	case u.Scheme == "https", u.Scheme == "http" && insecure:
		return nil
	case u.Scheme == "http":
		return fmt.Errorf("%s must use https (set %s=true to permit http)", name, AllowInsecureHTTPEnv)
	default:
		return fmt.Errorf("%s must use https", name)
	}
}

// loadServiceAccount reads the key file named by PROBE_GOOGLE_CREDENTIALS,
// else /run/secrets/PROBE_GOOGLE_CREDENTIALS, else
// GOOGLE_APPLICATION_CREDENTIALS. A key file is JSON over several lines, so
// it is read whole rather than as a one-line secret.
func loadServiceAccount(getenv func(string) string, readFile func(string) ([]byte, error)) (*serviceAccount, string, error) {
	if readFile == nil {
		return nil, "", nil
	}
	candidates := []struct {
		file     string
		explicit bool
	}{
		{strings.TrimSpace(getenv(CredentialsEnv)), true},
		{issuetext.SecretsDir + "/" + CredentialsEnv, false},
		{strings.TrimSpace(getenv(ApplicationCredsEnv)), true},
	}
	for _, c := range candidates {
		if c.file == "" {
			continue
		}
		data, err := readFile(c.file)
		if err != nil {
			if !c.explicit && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, "", fmt.Errorf("Google credentials %s: %w", c.file, err)
		}
		account, err := parseServiceAccount(data)
		if err != nil {
			return nil, "", fmt.Errorf("Google credentials %s: %w", c.file, err)
		}
		return account, c.file, nil
	}
	return nil, "", nil
}

func parseServiceAccount(data []byte) (*serviceAccount, error) {
	if len(data) > maxKeyFileBytes {
		return nil, errors.New("key file exceeds 64 KiB")
	}
	var a serviceAccount
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, errors.New("key file is not JSON")
	}
	if a.Type != "service_account" || a.ClientEmail == "" || a.PrivateKey == "" {
		return nil, errors.New(`key file must be a service-account key ("type": "service_account" with client_email and private_key)`)
	}
	block, _ := pem.Decode([]byte(a.PrivateKey))
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k, err1 := x509.ParsePKCS1PrivateKey(block.Bytes); err1 == nil {
			parsed = k
		} else {
			return nil, errors.New("private_key is not an RSA key")
		}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private_key is not an RSA key")
	}
	a.key = key
	return &a, nil
}

// Document is one exported Google Doc.
type Document struct {
	ID           string
	Title        string
	URL          string
	LastModified string
	Markdown     string
}

// Client fetches documents.
type Client struct {
	Config Config
	HTTP   *http.Client // nil uses a client with a 30s timeout
	token  string
	expiry time.Time
	now    func() time.Time
}

// ErrNotFound is returned when a document does not exist or the credential
// cannot read it.
var ErrNotFound = errors.New("document not found or not shared with the configured Google account")

// Fetch reads a document's metadata and exports it as Markdown.
func (c *Client) Fetch(ctx context.Context, id string) (Document, error) {
	if !c.Config.Configured() {
		return Document{}, fmt.Errorf("no Google credential configured: set %s, %s or %s", AccessTokenEnv, CredentialsEnv, APIKeyEnv)
	}
	if !idPattern.MatchString(id) {
		return Document{}, fmt.Errorf("%q is not a Google Docs ID", id)
	}
	var meta struct {
		Name         string `json:"name"`
		MimeType     string `json:"mimeType"`
		WebViewLink  string `json:"webViewLink"`
		ModifiedTime string `json:"modifiedTime"`
	}
	query := url.Values{"fields": {"name,mimeType,webViewLink,modifiedTime"}, "supportsAllDrives": {"true"}}
	body, err := c.get(ctx, "/drive/v3/files/"+id, query)
	if err != nil {
		return Document{}, err
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return Document{}, fmt.Errorf("decode metadata: %w", err)
	}
	if meta.MimeType != docMimeType {
		return Document{}, fmt.Errorf("%s is not a Google Doc (%s); export it to Google Docs first", id, meta.MimeType)
	}
	doc := Document{ID: id, Title: meta.Name, LastModified: meta.ModifiedTime}
	if u, err := url.Parse(meta.WebViewLink); err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "docs.google.com") {
		doc.URL = meta.WebViewLink
	}
	content, err := c.get(ctx, "/drive/v3/files/"+id+"/export", url.Values{"mimeType": {"text/markdown"}})
	var status *httpStatus
	if errors.As(err, &status) && status.code == http.StatusBadRequest {
		// A deployment whose Drive does not export Markdown still exports text.
		content, err = c.get(ctx, "/drive/v3/files/"+id+"/export", url.Values{"mimeType": {"text/plain"}})
	}
	if err != nil {
		return Document{}, err
	}
	doc.Markdown = CleanMarkdown(string(content))
	return doc, nil
}

type httpStatus struct {
	code    int
	message string
}

func (e *httpStatus) Error() string {
	if e.message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.code, e.message)
	}
	return fmt.Sprintf("HTTP %d", e.code)
}

func (c *Client) httpClient() http.Client {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	guarded := *client
	// A request carrying a credential never follows a redirect.
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return guarded
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	auth, err := c.authorization(ctx)
	if err != nil {
		return nil, err
	}
	if c.Config.APIKey != "" && auth == "" {
		query.Set("key", c.Config.APIKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Config.APIURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", c.redact(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("document exceeds %d bytes", maxResponseBytes)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("HTTP 401: the Google credential from %s was refused", c.Config.Source)
	case http.StatusForbidden:
		return nil, fmt.Errorf("HTTP 403: %s (is the Drive API enabled, and the document shared with the account?)", googleMessage(body))
	default:
		return nil, &httpStatus{code: resp.StatusCode, message: googleMessage(body)}
	}
	if !utf8.Valid(body) {
		return nil, errors.New("response is not UTF-8")
	}
	return body, nil
}

func googleMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return issuetext.OneLine(truncate(e.Error.Message, 200))
}

func (c *Client) redact(err error) error {
	msg := err.Error()
	for _, secret := range []string{c.Config.AccessToken, c.Config.APIKey, c.token} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
		}
	}
	return errors.New(msg)
}

// authorization returns the Authorization header: the access token, or a
// token obtained with the service account; "" for an API key.
func (c *Client) authorization(ctx context.Context) (string, error) {
	switch {
	case c.Config.AccessToken != "":
		return "Bearer " + c.Config.AccessToken, nil
	case c.Config.Account == nil:
		return "", nil
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	if c.token != "" && now().Before(c.expiry) {
		return "Bearer " + c.token, nil
	}
	assertion, err := c.Config.Account.assertion(c.Config.TokenURL, now())
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("service-account token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("service-account token: %w", err)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		reason := issuetext.OneLine(truncate(out.Error+" "+out.Description, 200))
		return "", fmt.Errorf("service-account token for %s refused (HTTP %d) %s", c.Config.Account.ClientEmail, resp.StatusCode, reason)
	}
	c.token = out.AccessToken
	lifetime := time.Duration(out.ExpiresIn) * time.Second
	if lifetime <= 0 || lifetime > time.Hour {
		lifetime = time.Hour
	}
	c.expiry = now().Add(lifetime - time.Minute)
	return "Bearer " + c.token, nil
}

// assertion signs the JWT a service account exchanges for an access token.
func (a *serviceAccount) assertion(audience string, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if a.KeyID != "" {
		header["kid"] = a.KeyID
	}
	claims := map[string]any{
		"iss": a.ClientEmail, "scope": Scope, "aud": audience,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cl, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(cl)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign service-account assertion: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

var (
	dataReference = regexp.MustCompile(`(?m)^\[[^\]\n]+\]:\s*<?data:[^\n]*$`)
	dataImage     = regexp.MustCompile(`!\[[^\]\n]*\]\((?:<)?data:[^)\n]*\)`)
	refImage      = regexp.MustCompile(`!\[([^\]\n]*)\]\[[^\]\n]*\]`)
	blankLines    = regexp.MustCompile(`\n{3,}`)
)

// CleanMarkdown removes what Drive's Markdown export embeds but a reviewer
// cannot use: images, which it inlines as base64 data URIs. An image keeps
// its alt text, if any, as "[image: alt]".
func CleanMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = dataReference.ReplaceAllString(s, "")
	s = dataImage.ReplaceAllString(s, "[image]")
	s = refImage.ReplaceAllStringFunc(s, func(m string) string {
		alt := refImage.FindStringSubmatch(m)[1]
		if strings.TrimSpace(alt) == "" {
			return "[image]"
		}
		return "[image: " + alt + "]"
	})
	return blankLines.ReplaceAllString(strings.TrimSpace(s), "\n\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// Intent renders the document as intent context: its headings nest under the
// document, and its list items are prose unless they sit under an
// "Acceptance criteria" heading of the document.
func (d Document) Intent() string {
	var b strings.Builder
	// Prose, not a heading: a title such as "Acceptance criteria for
	// checkout" must not open a criteria section.
	fmt.Fprintf(&b, "Google Doc: %s\n\n", issuetext.OneLine(d.Title))
	var meta []string
	if d.LastModified != "" {
		meta = append(meta, "Last modified: "+issuetext.OneLine(d.LastModified))
	}
	if d.URL != "" {
		meta = append(meta, "Link: "+d.URL)
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	if md := strings.TrimSpace(d.Markdown); md != "" {
		b.WriteString("\n## Document content\n\n" + issuetext.ProseLists(issuetext.NestHeadings(md, false)) + "\n")
	}
	return b.String()
}
