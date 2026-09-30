// Command manifest writes and signs the update manifest the website serves
// under /download/, which Probe Desktop reads to update itself.
//
//	manifest write -version 0.6.1 -dir DIR     prints latest.json for DIR
//	manifest sign  -key FILE MANIFEST           prints the base64 signature
//	manifest verify -pub KEY MANIFEST SIG       checks a signature
//	manifest keygen                             prints a new key pair
//
// The signature is Ed25519 over the exact bytes of the manifest, so a client
// verifies what it downloaded before parsing it. The private key never enters
// an image: the website container signs at start-up with the key Swarm mounts
// as a secret (see web/docker-entrypoint.d).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Product names the manifest, so that a signature for another product's
// manifest can never pass for this one.
const Product = "probe"

// KeyEnv holds the private key when no key file is mounted.
const KeyEnv = "PROBE_UPDATE_SIGNING_KEY"

// Manifest is the content of latest.json.
type Manifest struct {
	Product  string    `json:"product"`
	Version  string    `json:"version"`
	Released time.Time `json:"released"`
	Files    []File    `json:"files"`
}

// File is one downloadable file of the release.
type File struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: manifest write|sign|verify|keygen")
	}
	switch args[0] {
	case "write":
		fs := flag.NewFlagSet("write", flag.ContinueOnError)
		version := fs.String("version", "", "release version")
		dir := fs.String("dir", "", "directory holding the release files")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		m, err := Write(*dir, *version, time.Now().UTC())
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", data)
		return err
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		keyFile := fs.String("key", "", "file holding the base64 private key (default: $"+KeyEnv+")")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: manifest sign -key FILE MANIFEST")
		}
		key, err := readPrivateKey(*keyFile)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(ed25519.Sign(key, data)))
		return err
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		pub := fs.String("pub", "", "base64 public key")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return errors.New("usage: manifest verify -pub KEY MANIFEST SIG")
		}
		key, err := decodeKey(*pub, ed25519.PublicKeySize)
		if err != nil {
			return fmt.Errorf("public key: %w", err)
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(fs.Arg(1))
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil || !ed25519.Verify(ed25519.PublicKey(key), data, raw) {
			return errors.New("signature does not match")
		}
		_, err = fmt.Fprintln(stdout, "signature valid")
		return err
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "private %s\npublic  %s\n",
			base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub))
		return err
	}
	return fmt.Errorf("unknown command %q", args[0])
}

var (
	cliName     = regexp.MustCompile(`^probe-(v?[0-9A-Za-z._+-]+)-(linux|darwin|windows)-(amd64|arm64)\.(tar\.gz|zip)$`)
	desktopName = regexp.MustCompile(`^probe-desktop-(v?[0-9A-Za-z._+-]+)-(windows)-(amd64|arm64)\.exe$`)
)

// Write describes the release files of dir that carry version in their name.
// Other files (SHA256SUMS, older builds) are left out.
func Write(dir, version string, released time.Time) (*Manifest, error) {
	if version == "" {
		return nil, errors.New("version is required")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Product: Product, Version: version, Released: released.Truncate(time.Second), Files: []File{}}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		f := File{Name: e.Name()}
		if s := desktopName.FindStringSubmatch(f.Name); s != nil && s[1] == version {
			f.Component, f.OS, f.Arch = "desktop", s[2], s[3]
		} else if s := cliName.FindStringSubmatch(f.Name); s != nil && s[1] == version {
			f.Component, f.OS, f.Arch = "cli", s[2], s[3]
		} else {
			continue
		}
		if f.Size, f.SHA256, err = digest(filepath.Join(dir, f.Name)); err != nil {
			return nil, err
		}
		m.Files = append(m.Files, f)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("no release file for version %s in %s", version, dir)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	return m, nil
}

func digest(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// readPrivateKey reads the seed from a file, or from $PROBE_UPDATE_SIGNING_KEY
// when no file is named.
func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	value := os.Getenv(KeyEnv)
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		value = string(data)
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("no signing key: pass -key or set " + KeyEnv)
	}
	seed, err := decodeKey(value, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func decodeKey(value string, size int) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	if len(raw) != size {
		return nil, fmt.Errorf("want %d bytes, got %d", size, len(raw))
	}
	return raw, nil
}
