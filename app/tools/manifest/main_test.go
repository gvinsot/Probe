package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteListsOnlyTheVersionFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"probe-0.6.1-linux-amd64.tar.gz":          "cli",
		"probe-0.6.1-windows-arm64.zip":           "cli zip",
		"probe-desktop-0.6.1-windows-amd64.exe":   "desktop",
		"probe-desktop-0.6.0-windows-amd64.exe":   "older build",
		"SHA256SUMS":                              "sums",
		"probe-desktop-0.6.1-darwin-universal.zp": "unknown",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Write(dir, "0.6.1", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Component+" "+f.OS+"/"+f.Arch+" "+f.Name)
	}
	want := []string{
		"cli linux/amd64 probe-0.6.1-linux-amd64.tar.gz",
		"cli windows/arm64 probe-0.6.1-windows-arm64.zip",
		"desktop windows/amd64 probe-desktop-0.6.1-windows-amd64.exe",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	desktop := m.Files[2]
	sum := sha256.Sum256([]byte("desktop"))
	if desktop.Size != int64(len("desktop")) || desktop.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("desktop entry = %+v", desktop)
	}
	if _, err := Write(dir, "9.9.9", time.Now()); err == nil {
		t.Fatal("Write succeeded without any file of the version")
	}
}

func TestSignThenVerify(t *testing.T) {
	var keys bytes.Buffer
	if err := run([]string{"keygen"}, &keys); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(keys.String())
	priv, pub := fields[1], fields[3]

	dir := t.TempDir()
	manifest := filepath.Join(dir, "latest.json")
	data, _ := json.Marshal(Manifest{Product: Product, Version: "0.6.1"})
	os.WriteFile(manifest, data, 0o644)
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte(priv+"\n"), 0o600)

	var sig bytes.Buffer
	if err := run([]string{"sign", "-key", keyFile, manifest}, &sig); err != nil {
		t.Fatal(err)
	}
	sigFile := filepath.Join(dir, "latest.json.sig")
	os.WriteFile(sigFile, sig.Bytes(), 0o644)
	if err := run([]string{"verify", "-pub", pub, manifest, sigFile}, &bytes.Buffer{}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// The same key through the environment signs identically (Ed25519 is
	// deterministic), which is how the container reads an unmounted key.
	t.Setenv(KeyEnv, priv)
	var fromEnv bytes.Buffer
	if err := run([]string{"sign", manifest}, &fromEnv); err != nil {
		t.Fatal(err)
	}
	if fromEnv.String() != sig.String() {
		t.Fatal("key from the environment signed differently")
	}

	os.WriteFile(manifest, append(data, ' '), 0o644)
	if err := run([]string{"verify", "-pub", pub, manifest, sigFile}, &bytes.Buffer{}); err == nil {
		t.Fatal("verify accepted a modified manifest")
	}
	raw, _ := base64.StdEncoding.DecodeString(pub)
	if len(raw) != ed25519.PublicKeySize {
		t.Fatalf("public key has %d bytes", len(raw))
	}
}

func TestSignWithoutKeyFails(t *testing.T) {
	t.Setenv(KeyEnv, "")
	manifest := filepath.Join(t.TempDir(), "latest.json")
	os.WriteFile(manifest, []byte("{}"), 0o644)
	if err := run([]string{"sign", manifest}, &bytes.Buffer{}); err == nil {
		t.Fatal("sign succeeded without a key")
	}
}
