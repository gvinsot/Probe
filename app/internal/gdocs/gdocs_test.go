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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/acceptance"
)

const docID = "1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcde"

func TestParseDocument(t *testing.T) {
	for ref, want := range map[string]string{
		docID: docID,
		"https://docs.google.com/document/d/" + docID + "/edit?tab=t.0#heading=h.x": docID,
		"https://docs.google.com/document/u/1/d/" + docID + "/edit":                 docID,
		"https://drive.google.com/file/d/" + docID + "/view":                        docID,
		"https://drive.google.com/open?id=" + docID:                                 docID,
	} {
		if got, err := ParseDocument(ref); err != nil || got != want {
			t.Errorf("ParseDocument(%q) = %q, %v", ref, got, err)
		}
	}
	for _, ref := range []string{"", "short", "https://evil.example/document/d/" + docID, "http://docs.google.com/document/d/" + docID, "https://docs.google.com/document/", "../" + docID} {
		if _, err := ParseDocument(ref); err == nil {
			t.Errorf("ParseDocument(%q) accepted", ref)
		}
	}
	if ids, err := ParseDocuments(docID + ",https://docs.google.com/document/d/" + docID + "/edit"); err != nil || len(ids) != 1 {
		t.Fatalf("duplicates: %v %v", ids, err)
	}
	many := strings.Repeat(docID[:40]+"a,", 0)
	for i := 0; i < 6; i++ {
		many += fmt.Sprintf("%s%d,", docID[:40], i)
	}
	if _, err := ParseDocuments(many); err == nil {
		t.Fatal("more than 5 documents accepted")
	}
}

func serviceAccountJSON(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	data, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "probe@acme.iam.gserviceaccount.com",
		"private_key": string(pemKey), "private_key_id": "k1", "token_uri": "https://evil.example/token",
	})
	return data
}

func TestFromEnv(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	account := serviceAccountJSON(t, key)
	env := func(v map[string]string) func(string) string { return func(n string) string { return v[n] } }
	files := func(v map[string][]byte) func(string) ([]byte, error) {
		return func(n string) ([]byte, error) {
			if d, ok := v[n]; ok {
				return d, nil
			}
			return nil, fs.ErrNotExist
		}
	}
	c, err := FromEnv(env(nil), files(nil))
	if err != nil || c.Configured() {
		t.Fatalf("unset: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{AccessTokenEnv: "ya29.x", APIKeyEnv: "k"}), files(nil))
	if err != nil || c.AccessToken != "ya29.x" || c.APIKey != "" {
		t.Fatalf("access token first: %+v %v", c, err)
	}
	c, err = FromEnv(env(nil), files(map[string][]byte{"/run/secrets/PROBE_GOOGLE_CREDENTIALS": account}))
	if err != nil || c.Account == nil || c.Account.ClientEmail != "probe@acme.iam.gserviceaccount.com" || c.TokenURL != DefaultTokenURL {
		t.Fatalf("secret key file: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{ApplicationCredsEnv: "/keys/sa.json"}), files(map[string][]byte{"/keys/sa.json": account}))
	if err != nil || c.Account == nil {
		t.Fatalf("GOOGLE_APPLICATION_CREDENTIALS: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{APIKeyEnv: "AIza"}), files(nil))
	if err != nil || c.APIKey != "AIza" {
		t.Fatalf("api key: %+v %v", c, err)
	}
	for name, tc := range map[string]struct {
		env   map[string]string
		files map[string][]byte
	}{
		"missing key file": {env: map[string]string{CredentialsEnv: "/absent"}},
		"not a key":        {env: map[string]string{CredentialsEnv: "/k"}, files: map[string][]byte{"/k": []byte(`{"type":"authorized_user"}`)}},
		"bad pem":          {env: map[string]string{CredentialsEnv: "/k"}, files: map[string][]byte{"/k": []byte(`{"type":"service_account","client_email":"a","private_key":"x"}`)}},
		"http api":         {env: map[string]string{APIURLEnv: "http://googleapis.test"}},
		"bool":             {env: map[string]string{AllowInsecureHTTPEnv: "maybe"}},
	} {
		if _, err := FromEnv(env(tc.env), files(tc.files)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

const exported = "# Checkout design\n\nPrices are integer cents.\n\n* Services talk over gRPC\n\n![][image1]\n\n## Acceptance criteria\n\n* Totals never use floats\n* Refunds are validated\n\n## Risks\n\n* Legacy client\n\n[image1]: <data:image/png;base64,iVBORw0KGgo=>\n"

// googleServer serves one document; tokenKey, when set, is the service
// account whose assertions the token endpoint accepts.
func googleServer(t *testing.T, tokenKey *rsa.PublicKey, markdown bool) (*httptest.Server, *[]string) {
	t.Helper()
	var auth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(claimsJSON, &claims)
		if rsa.VerifyPKCS1v15(tokenKey, crypto.SHA256, sum[:], sig) != nil || claims["scope"] != Scope || claims["iss"] != "probe@acme.iam.gserviceaccount.com" || !strings.HasSuffix(claims["aud"].(string), "/token") {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad assertion"}`, 400)
			return
		}
		fmt.Fprint(w, `{"access_token":"ya29.minted","expires_in":3599,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/drive/v3/files/"+docID, func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization")+"|"+r.URL.Query().Get("key"))
		fmt.Fprint(w, `{"name":"Checkout design","mimeType":"application/vnd.google-apps.document","webViewLink":"https://docs.google.com/document/d/`+docID+`/edit","modifiedTime":"2026-09-01T10:00:00Z"}`)
	})
	mux.HandleFunc("/drive/v3/files/"+docID+"/export", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mimeType") == "text/markdown" && !markdown {
			http.Error(w, `{"error":{"message":"Export only supports these formats"}}`, 400)
			return
		}
		fmt.Fprint(w, exported)
	})
	mux.HandleFunc("/drive/v3/files/"+strings.Repeat("s", 44), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &auth
}

func TestFetchWithServiceAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server, auth := googleServer(t, &key.PublicKey, true)
	account, err := parseServiceAccount(serviceAccountJSON(t, key))
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{Config: Config{APIURL: server.URL, TokenURL: server.URL + "/token", Account: account, Source: "test", AllowInsecureHTTP: true}}
	doc, err := client.Fetch(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	if (*auth)[0] != "Bearer ya29.minted|" || doc.Title != "Checkout design" || !strings.HasPrefix(doc.URL, "https://docs.google.com/") {
		t.Fatalf("doc %+v auth %v", doc, *auth)
	}
	if strings.Contains(doc.Markdown, "base64") || !strings.Contains(doc.Markdown, "[image]") {
		t.Fatalf("images not cleaned:\n%s", doc.Markdown)
	}
	text := doc.Intent()
	for _, want := range []string{
		"Google Doc: Checkout design\n\nLast modified: 2026-09-01T10:00:00Z · Link: https://docs.google.com/document/d/",
		"## Document content\n\n### Checkout design\n\nPrices are integer cents.\n\n• Services talk over gRPC",
		"#### Acceptance criteria\n\n* Totals never use floats\n* Refunds are validated",
		"#### Risks\n\n• Legacy client",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("intent lacks %q:\n%s", want, text)
		}
	}
	parsed, err := acceptance.Parse(text)
	if err != nil || len(parsed.Criteria) != 2 {
		t.Fatalf("criteria %+v %v", parsed.Criteria, err)
	}
	// The minted token is reused.
	if _, err := client.Fetch(context.Background(), docID); err != nil || (*auth)[1] != "Bearer ya29.minted|" {
		t.Fatalf("second fetch: %v %v", err, *auth)
	}

	if _, err := client.Fetch(context.Background(), strings.Repeat("s", 44)); err == nil || !strings.Contains(err.Error(), "not a Google Doc") {
		t.Fatalf("spreadsheet: %v", err)
	}
	if _, err := client.Fetch(context.Background(), strings.Repeat("n", 44)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	wrong, _ := parseServiceAccount(serviceAccountJSON(t, other))
	refused := &Client{Config: Config{APIURL: server.URL, TokenURL: server.URL + "/token", Account: wrong, AllowInsecureHTTP: true}}
	if _, err := refused.Fetch(context.Background(), docID); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestFetchWithAPIKeyFallsBackToText(t *testing.T) {
	server, auth := googleServer(t, nil, false)
	client := &Client{Config: Config{APIURL: server.URL, APIKey: "AIza-test", AllowInsecureHTTP: true}}
	doc, err := client.Fetch(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	if (*auth)[0] != "|AIza-test" || !strings.Contains(doc.Markdown, "Totals never use floats") {
		t.Fatalf("api key %v\n%s", *auth, doc.Markdown)
	}
	if _, err := (&Client{}).Fetch(context.Background(), docID); err == nil {
		t.Fatal("unconfigured client fetched")
	}
}

func TestCleanMarkdown(t *testing.T) {
	in := "a ![logo](data:image/png;base64,AAAA) b\n![Diagram of the flow][image2]\n\n\n\nc\n[image2]: <data:image/png;base64,BBBB>"
	if got := CleanMarkdown(in); got != "a [image] b\n[image: Diagram of the flow]\n\nc" {
		t.Fatalf("%q", got)
	}
}
