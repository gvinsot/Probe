package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Paths next to the running executable. Windows lets a running executable
// be renamed, not replaced: the engine moves itself to Old, then puts the
// new version at its path.
func stagedPath(exe string) string { return exe + ".new.exe" }
func oldPath(exe string) string    { return exe + ".old" }

// Swap puts the staged executable at exe and keeps the running one as
// exe.old, so that Restore can bring it back.
func Swap(exe, staged string) error {
	old := oldPath(exe)
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%w; restoring the previous executable: %v", err, rerr)
		}
		return err
	}
	return nil
}

// Restore undoes Swap after the new version failed to start.
func Restore(exe string) error {
	if err := os.Remove(exe); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(oldPath(exe), exe)
}

// Cleanup removes what an earlier update left. The previous executable may
// still be running for a few seconds after a restart; it is removed later.
func Cleanup(exe string) {
	os.Remove(oldPath(exe))
	os.Remove(stagedPath(exe))
}

// Pending is an update ready to be installed.
type Pending struct {
	Version string
	Staged  string
}

// Updater checks for and prepares updates of the running executable.
type Updater struct {
	Current Version
	Exe     string // the executable, as it was when the engine started
	DataDir string
	Client  *Client
	Log     *slog.Logger
	// Probe runs a downloaded executable once to confirm that it starts and
	// reports the expected version. Tests replace it.
	Probe func(ctx context.Context, path, version string) error
}

// New returns the updater of a running engine, or nil when it must not
// update itself: a development build, a system without published
// executables, or PROBE_DESKTOP_UPDATE=off.
func New(current, exe, dataDir string, log *slog.Logger) *Updater {
	v, ok := ParseVersion(current)
	if !ok || runtime.GOOS != "windows" || strings.EqualFold(os.Getenv("PROBE_DESKTOP_UPDATE"), "off") {
		return nil
	}
	base := os.Getenv("PROBE_DESKTOP_UPDATE_URL")
	if base == "" {
		base = DefaultURL
	}
	return &Updater{
		Current: v,
		Exe:     exe,
		DataDir: dataDir,
		Client:  &Client{BaseURL: base, HTTP: &http.Client{Timeout: 10 * time.Minute}, Keys: Keys()},
		Log:     log,
		Probe:   probeExecutable,
	}
}

// Check prepares the latest version when it is newer than the running one.
// It returns nil, nil when there is nothing to install.
func (u *Updater) Check(ctx context.Context) (*Pending, error) {
	m, err := u.Client.Latest(ctx)
	if err != nil {
		return nil, err
	}
	latest, ok := ParseVersion(m.Version)
	if !ok || latest.Pre != "" || !u.Current.Less(latest) || u.skipped(m.Version) {
		return nil, nil
	}
	f, ok := m.Find("desktop", runtime.GOOS, runtime.GOARCH)
	if !ok {
		return nil, nil
	}
	staged := stagedPath(u.Exe)
	if err := u.Client.Download(ctx, f, staged); err != nil {
		return nil, err
	}
	if err := u.Probe(ctx, staged, m.Version); err != nil {
		os.Remove(staged)
		u.Skip(m.Version)
		return nil, fmt.Errorf("version %s does not start: %w", m.Version, err)
	}
	return &Pending{Version: m.Version, Staged: staged}, nil
}

// Timing of the checks. The first waits for the engine to settle.
var (
	firstCheck = time.Minute
	interval   = 6 * time.Hour
)

// Run checks for updates until one is prepared, which it hands to ready, or
// until stop is closed.
func (u *Updater) Run(stop <-chan struct{}, ready func(*Pending)) {
	wait := firstCheck
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		wait = interval
		Cleanup(u.Exe)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		p, err := u.Check(ctx)
		cancel()
		switch {
		case errors.Is(err, os.ErrPermission):
			u.Log.Warn("cannot update: the folder of the executable is not writable", "exe", u.Exe, "err", err)
			return
		case err != nil:
			u.Log.Warn("update check failed", "err", err)
		case p != nil:
			u.Log.Info("update ready", "version", p.Version)
			ready(p)
			return
		}
	}
}

// Skip records a version that failed to start, so that it is not
// downloaded again; a later version replaces the record.
func (u *Updater) Skip(version string) {
	if err := os.WriteFile(filepath.Join(u.DataDir, "update-skip"), []byte(version+"\n"), 0o600); err != nil {
		u.Log.Warn("record failed update", "err", err)
	}
}

func (u *Updater) skipped(version string) bool {
	data, err := os.ReadFile(filepath.Join(u.DataDir, "update-skip"))
	return err == nil && strings.TrimSpace(string(data)) == version
}

// probeExecutable runs "exe version", which prints "probe-desktop VERSION".
func probeExecutable(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != "probe-desktop "+version {
		return fmt.Errorf("reports %q", got)
	}
	return nil
}
