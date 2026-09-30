package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTrustedKeysDecode(t *testing.T) {
	if got := len(Keys()); got != len(trustedKeys) || got == 0 {
		t.Fatalf("%d usable keys out of %d", got, len(trustedKeys))
	}
}

func TestParseAndOrderVersions(t *testing.T) {
	for _, bad := range []string{"dev", "", "0.6", "0.6.1.2", "a.b.c", "0.06.1", "-1.0.0", "0.6.x"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	ordered := []string{"0.5.9", "0.5.15", "v0.6.0-rc.1", "0.6.0", "0.10.0", "1.0.0"}
	for i := 1; i < len(ordered); i++ {
		a, ok1 := ParseVersion(ordered[i-1])
		b, ok2 := ParseVersion(ordered[i])
		if !ok1 || !ok2 {
			t.Fatalf("parse %q or %q", ordered[i-1], ordered[i])
		}
		if !a.Less(b) || b.Less(a) {
			t.Errorf("want %s < %s", ordered[i-1], ordered[i])
		}
	}
	v, _ := ParseVersion("v0.6.0")
	w, _ := ParseVersion("0.6.0")
	if v.Less(w) || w.Less(v) {
		t.Error("v0.6.0 and 0.6.0 differ")
	}
}

type release struct {
	key      ed25519.PrivateKey
	manifest []byte
	sig      []byte
	files    map[string][]byte
}

func newRelease(t *testing.T, version string, files map[string][]byte) *release {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{key: key, files: files}
	m := Manifest{Product: product, Version: version}
	for name, data := range files {
		sum := sha256.Sum256(data)
		f := File{Name: name, Component: "desktop", OS: runtime.GOOS, Arch: runtime.GOARCH, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
		if strings.HasPrefix(name, "probe-") && !strings.HasPrefix(name, "probe-desktop") {
			f.Component = "cli"
		}
		m.Files = append(m.Files, f)
	}
	r.sign(t, m)
	return r
}

func (r *release) sign(t *testing.T, m Manifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r.manifest = data
	r.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.key, data)) + "\n")
}

func (r *release) public() ed25519.PublicKey { return r.key.Public().(ed25519.PublicKey) }

func (r *release) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch name := strings.TrimPrefix(req.URL.Path, "/download/"); name {
		case ManifestName:
			w.Write(r.manifest)
		case SignatureName:
			w.Write(r.sig)
		default:
			data, ok := r.files[name]
			if !ok {
				http.NotFound(w, req)
				return
			}
			w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyRejectsWhatIsNotSigned(t *testing.T) {
	r := newRelease(t, "0.6.1", map[string][]byte{"probe-desktop-0.6.1-windows-amd64.exe": []byte("x")})
	keys := []ed25519.PublicKey{r.public()}
	if _, err := Verify(r.manifest, r.sig, keys); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	tampered := append([]byte{}, r.manifest...)
	tampered[len(tampered)-2] ^= 1
	if _, err := Verify(tampered, r.sig, keys); err == nil {
		t.Error("modified manifest accepted")
	}
	other := newRelease(t, "0.6.1", nil)
	if _, err := Verify(r.manifest, r.sig, []ed25519.PublicKey{other.public()}); err == nil {
		t.Error("manifest signed by an unknown key accepted")
	}
	// A second trusted key, as during a key rotation.
	if _, err := Verify(r.manifest, r.sig, []ed25519.PublicKey{other.public(), r.public()}); err != nil {
		t.Errorf("rotation: %v", err)
	}
	for _, m := range []Manifest{
		{Product: "voxforge", Version: "0.6.1"},
		{Product: product, Version: "0.6.1", Files: []File{{Name: "../probe-desktop.exe"}}},
		{Product: product, Version: "0.6.1", Files: []File{{Name: "x?y=1"}}},
	} {
		r.sign(t, m)
		if _, err := Verify(r.manifest, r.sig, keys); err == nil {
			t.Errorf("manifest %+v accepted", m)
		}
	}
}

func testUpdater(t *testing.T, r *release, current string) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "probe-desktop.exe")
	if err := os.WriteFile(exe, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	v, ok := ParseVersion(current)
	if !ok {
		t.Fatal(current)
	}
	srv := r.serve(t)
	return &Updater{
		Current: v,
		Exe:     exe,
		DataDir: t.TempDir(),
		Client:  &Client{BaseURL: srv.URL + "/download/", HTTP: srv.Client(), Keys: []ed25519.PublicKey{r.public()}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Probe:   func(context.Context, string, string) error { return nil },
	}, exe
}

func TestCheckStagesANewerVersion(t *testing.T) {
	r := newRelease(t, "0.6.1", map[string][]byte{
		"probe-desktop-0.6.1-windows-amd64.exe": []byte("new desktop"),
		"probe-0.6.1-linux-amd64.tar.gz":        []byte("cli"),
	})
	u, exe := testUpdater(t, r, "0.6.0")
	var probed string
	u.Probe = func(_ context.Context, path, version string) error {
		probed = path + " " + version
		return nil
	}
	p, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Version != "0.6.1" || p.Staged != stagedPath(exe) {
		t.Fatalf("pending = %+v", p)
	}
	if data, _ := os.ReadFile(p.Staged); string(data) != "new desktop" {
		t.Fatalf("staged content %q", data)
	}
	if probed != p.Staged+" 0.6.1" {
		t.Fatalf("probed %q", probed)
	}

	if err := Swap(exe, p.Staged); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new desktop" {
		t.Fatalf("after Swap, exe holds %q", data)
	}
	if err := Restore(exe); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "running" {
		t.Fatalf("after Restore, exe holds %q", data)
	}
}

func TestCheckInstallsNothingOlderOrUnpublished(t *testing.T) {
	file := map[string][]byte{"probe-desktop-0.6.1-windows-amd64.exe": []byte("new")}
	for _, current := range []string{"0.6.1", "0.7.0"} {
		u, _ := testUpdater(t, newRelease(t, "0.6.1", file), current)
		if p, err := u.Check(context.Background()); p != nil || err != nil {
			t.Errorf("running %s: pending %+v, err %v", current, p, err)
		}
	}
	u, _ := testUpdater(t, newRelease(t, "0.7.0-rc.1", file), "0.6.1")
	if p, err := u.Check(context.Background()); p != nil || err != nil {
		t.Errorf("prerelease: pending %+v, err %v", p, err)
	}
	u, _ = testUpdater(t, newRelease(t, "0.6.2", map[string][]byte{"probe-0.6.2-linux-amd64.tar.gz": []byte("cli")}), "0.6.1")
	if p, err := u.Check(context.Background()); p != nil || err != nil {
		t.Errorf("no desktop download: pending %+v, err %v", p, err)
	}
}

func TestCheckRejectsADownloadThatDiffersFromTheManifest(t *testing.T) {
	r := newRelease(t, "0.6.1", map[string][]byte{"probe-desktop-0.6.1-windows-amd64.exe": []byte("genuine")})
	r.files["probe-desktop-0.6.1-windows-amd64.exe"] = []byte("altered")
	u, exe := testUpdater(t, r, "0.6.0")
	if p, err := u.Check(context.Background()); err == nil || p != nil {
		t.Fatalf("pending %+v, err %v", p, err)
	}
	if _, err := os.Stat(stagedPath(exe)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected download left on disk")
	}
}

func TestAVersionThatDoesNotStartIsSkipped(t *testing.T) {
	r := newRelease(t, "0.6.1", map[string][]byte{"probe-desktop-0.6.1-windows-amd64.exe": []byte("broken")})
	u, exe := testUpdater(t, r, "0.6.0")
	u.Probe = func(context.Context, string, string) error { return errors.New("exit status 1") }
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("broken version accepted")
	}
	if _, err := os.Stat(stagedPath(exe)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("broken download left on disk")
	}
	u.Probe = func(context.Context, string, string) error { return nil }
	if p, err := u.Check(context.Background()); p != nil || err != nil {
		t.Fatalf("skipped version downloaded again: pending %+v, err %v", p, err)
	}
}

func TestNewNeverUpdatesADevelopmentBuild(t *testing.T) {
	if u := New("dev", "probe-desktop.exe", t.TempDir(), slog.Default()); u != nil {
		t.Fatal("updater for a dev build")
	}
	t.Setenv("PROBE_DESKTOP_UPDATE", "off")
	if u := New("0.6.1", "probe-desktop.exe", t.TempDir(), slog.Default()); u != nil {
		t.Fatal("updater despite PROBE_DESKTOP_UPDATE=off")
	}
}
