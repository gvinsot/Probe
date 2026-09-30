// Package update keeps Probe Desktop current.
//
// The website publishes, next to the downloads, a manifest (latest.json)
// signed with Ed25519 (latest.json.sig). The engine reads it now and then;
// when it names a newer version, the executable for this system is
// downloaded next to the running one, checked against the size and SHA-256
// of the signed manifest, started once to confirm it runs, and kept aside
// until the engine can restart into it. The signature is what makes the
// manifest trustworthy: the digests would be worthless if whoever changed a
// download could change them too.
//
// Only newer stable versions are installed, so an old signed manifest served
// again can never bring a previous version back. A development build ("dev")
// never updates itself.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// DefaultURL is where the website publishes the manifest and the downloads.
const DefaultURL = "https://probe.technology/download/"

// Manifest files, relative to the base URL.
const (
	ManifestName  = "latest.json"
	SignatureName = "latest.json.sig"
)

// product must match the manifest: a signed manifest of another product can
// never pass for this one.
const product = "probe"

// trustedKeys verify the manifest. Several keys may be listed while one
// replaces another: a release signed with either is accepted.
var trustedKeys = []string{
	"qh+aMD4pABu5LVc+sCaWvl9Pa8fL/2bLn6pYCTW33Yo=",
}

// Size limits of what is read from the network.
const (
	maxManifest  = 1 << 20
	maxSignature = 1 << 10
	maxFile      = 512 << 20
)

// Manifest describes the latest release.
type Manifest struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Files   []File `json:"files"`
}

// File is one download of the release.
type File struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Find returns the download of a component for a system.
func (m *Manifest) Find(component, goos, goarch string) (File, bool) {
	for _, f := range m.Files {
		if f.Component == component && f.OS == goos && f.Arch == goarch {
			return f, true
		}
	}
	return File{}, false
}

// Keys decodes the trusted public keys.
func Keys() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, k := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	return keys
}

// Verify checks the signature of the manifest bytes before reading them.
func Verify(data, sig []byte, keys []ed25519.PublicKey) (*Manifest, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return nil, fmt.Errorf("manifest signature: %w", err)
	}
	signed := false
	for _, k := range keys {
		if ed25519.Verify(k, data, raw) {
			signed = true
			break
		}
	}
	if !signed {
		return nil, errors.New("manifest signature does not match a trusted key")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Product != product {
		return nil, fmt.Errorf("manifest is for %q, not %q", m.Product, product)
	}
	for _, f := range m.Files {
		// The name becomes part of a URL: never a path or a query.
		if f.Name == "" || strings.ContainsAny(f.Name, `/\?#%:`) || strings.HasPrefix(f.Name, ".") {
			return nil, fmt.Errorf("manifest file name %q", f.Name)
		}
	}
	return &m, nil
}

// Version is a MAJOR.MINOR.PATCH version, with an optional prerelease suffix.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseVersion reads "0.6.1", "v0.6.1" or "0.6.1-rc.1". Anything else, such
// as "dev", is not a version.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(s, "v")
	s, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2], pre}, true
}

// Less orders versions; a prerelease comes before its release.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	return v.Pre != "" && o.Pre == ""
}

// Client reads the manifest and the downloads from the website.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Keys    []ed25519.PublicKey
}

// Latest downloads and verifies the manifest.
func (c *Client) Latest(ctx context.Context) (*Manifest, error) {
	data, err := c.get(ctx, ManifestName, maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := c.get(ctx, SignatureName, maxSignature)
	if err != nil {
		return nil, err
	}
	return Verify(data, sig, c.Keys)
}

func (c *Client) get(ctx context.Context, name string, limit int64) ([]byte, error) {
	resp, err := c.open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}

func (c *Client) open(ctx context.Context, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", name, resp.Status)
	}
	return resp, nil
}

// Download writes a file of the manifest to dest, and removes it again
// unless its size and SHA-256 are those the manifest announced.
func (c *Client) Download(ctx context.Context, f File, dest string) (err error) {
	if f.Size <= 0 || f.Size > maxFile {
		return fmt.Errorf("%s: announced size %d", f.Name, f.Size)
	}
	want, err := hex.DecodeString(f.SHA256)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("%s: announced digest %q", f.Name, f.SHA256)
	}
	resp, err := c.open(ctx, f.Name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.Size+1))
	if err != nil {
		return err
	}
	if n != f.Size {
		return fmt.Errorf("%s: received %d bytes, the manifest announced %d", f.Name, n, f.Size)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		return fmt.Errorf("%s: SHA-256 differs from the manifest", f.Name)
	}
	return out.Sync()
}
