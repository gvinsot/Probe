// Package app assembles the Probe Desktop engine: the single-instance lock,
// the folder watcher, the local interface server, the tray icon and the
// window process.
//
// Everything runs in the background. The executable is built as a GUI
// program (no console on Windows) and bundled with LSUIElement on macOS (no
// Dock icon); the only visible parts are the tray icon and, on demand, the
// window.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/autostart"
	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/instance"
	"github.com/gvinsot/Probe/desktop/internal/platform"
	"github.com/gvinsot/Probe/desktop/internal/server"
	"github.com/gvinsot/Probe/desktop/internal/tray"
	"github.com/gvinsot/Probe/desktop/internal/update"
	"github.com/gvinsot/Probe/desktop/internal/watch"
)

// WindowFlag starts the executable as the window process.
const WindowFlag = "--window"

// RestartFlag marks an engine started by the previous one, which may still
// hold the single-instance lock for a moment.
const RestartFlag = "--restart"

// Options of an engine run.
type Options struct {
	Version string
	// Background starts without opening the window (start at login).
	Background bool
	// Restart waits for the previous engine to release the lock.
	Restart bool
}

// Run starts the engine and blocks until the user quits from the tray.
func Run(opts Options) error {
	dir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	logger, closeLog := openLog(dir)
	defer closeLog()

	lock, err := instance.Acquire(dir)
	for tries := 0; opts.Restart && errors.Is(err, instance.ErrRunning) && tries < 80; tries++ {
		time.Sleep(250 * time.Millisecond)
		lock, err = instance.Acquire(dir)
	}
	if errors.Is(err, instance.ErrRunning) {
		// Launching the app again means "show me": the running engine opens
		// its window and this process leaves.
		if !opts.Background {
			if err := instance.Signal(dir, "show"); err != nil {
				logger.Warn("running instance did not answer", "err", err)
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	logger.Info("starting", "version", opts.Version, "data", dir)

	settings, err := config.Open(dir)
	if err != nil {
		return err
	}
	watcher, err := watch.New(dir, settings.Get, logger)
	if err != nil {
		return err
	}
	a := &engine{dir: dir, log: logger}
	// Taken now: once an update has moved this executable aside, it would
	// report the path of the copy.
	a.exe, err = os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	srv, err := server.New(server.Deps{
		Settings:   settings,
		Watcher:    watcher,
		Log:        logger,
		Version:    opts.Version,
		OnShow:     a.showWindow,
		OnSettings: watcher.ScanNow,
	})
	if err != nil {
		return err
	}
	a.srv = srv
	if err := lock.Publish(instance.Info{PID: os.Getpid(), Port: srv.Port(), Control: srv.ControlToken()}); err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(); err != nil {
			logger.Error("interface server stopped", "err", err)
		}
	}()
	stop := make(chan struct{})
	go watcher.Run(stop)
	if a.updater = update.New(opts.Version, a.exe, dir, logger); a.updater != nil {
		go a.updater.Run(stop, func(p *update.Pending) { a.installWhenIdle(p, stop) })
	}

	tray.Run(tray.Actions{
		Open:         a.showWindow,
		OpenBrowser:  a.openBrowser,
		Scan:         watcher.ScanNow,
		AutoStart:    autostart.Enabled,
		SetAutoStart: autostart.Set,
		Quit:         tray.Quit,
	}, func(t *tray.Tray) {
		watcher.OnUpdate(t.SetCounts)
		if !opts.Background {
			a.showWindow()
		}
	})

	logger.Info("quitting")
	close(stop)
	a.closeWindow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = srv.Shutdown(ctx)
	if p := a.takePending(); p != nil {
		lock.Release()
		a.handoff(p)
	}
	return err
}

type engine struct {
	dir     string
	exe     string
	log     *slog.Logger
	srv     *server.Server
	updater *update.Updater

	mu      sync.Mutex
	window  *os.Process
	pending *update.Pending // set when the engine quits to install it
}

// installWhenIdle quits the engine to install an update as soon as no
// window is open: closing a window the user is reading would be rude.
func (a *engine) installWhenIdle(p *update.Pending, stop <-chan struct{}) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		a.mu.Lock()
		idle := a.window == nil
		if idle {
			a.pending = p
		}
		a.mu.Unlock()
		if idle {
			a.log.Info("restarting to install the update", "version", p.Version)
			tray.Quit()
			return
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func (a *engine) takePending() *update.Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending
	a.pending = nil
	return p
}

// handoff puts the new version in place and starts it. If it has not taken
// over within half a minute, the previous executable comes back and starts
// again, and that version is not downloaded a second time.
func (a *engine) handoff(p *update.Pending) {
	log := a.log.With("version", p.Version)
	if err := update.Swap(a.exe, p.Staged); err != nil {
		log.Error("install update", "err", err)
		a.restartSelf()
		return
	}
	proc, err := platform.Start(a.exe, autostart.BackgroundFlag, RestartFlag)
	if err == nil && a.tookOver(proc, 30*time.Second) {
		log.Info("update installed")
		return
	}
	log.Error("updated engine did not start, restoring the previous version", "err", err)
	if proc != nil {
		proc.Kill()
	}
	// Windows keeps the file of a process that is still exiting.
	var rerr error
	for i := 0; i < 40; i++ {
		if rerr = update.Restore(a.exe); rerr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if rerr != nil {
		log.Error("restore previous version", "err", rerr)
	}
	a.updater.Skip(p.Version)
	a.restartSelf()
}

func (a *engine) restartSelf() {
	if _, err := platform.Start(a.exe, autostart.BackgroundFlag, RestartFlag); err != nil {
		a.log.Error("restart", "err", err)
	}
}

// tookOver waits for proc to publish itself as the running engine.
func (a *engine) tookOver(proc *os.Process, limit time.Duration) bool {
	exited := make(chan struct{})
	go func() {
		proc.Wait()
		close(exited)
	}()
	deadline := time.After(limit)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			return false
		case <-deadline:
			return false
		case <-tick.C:
			if info, ok := instance.Running(a.dir); ok && info.PID == proc.Pid {
				return true
			}
		}
	}
}

// showWindow opens the window process with a fresh single-use launch URL.
// A window already open is replaced, which also brings it to the front.
func (a *engine) showWindow() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.window != nil {
		a.window.Kill()
		a.window = nil
	}
	p, err := platform.Start(a.exe, WindowFlag, a.srv.LaunchURL())
	if err != nil {
		a.log.Error("start window", "err", err)
		a.openBrowserLocked()
		return
	}
	a.window = p
	go func() {
		p.Wait()
		a.mu.Lock()
		if a.window == p {
			a.window = nil
		}
		a.mu.Unlock()
	}()
}

func (a *engine) openBrowser() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.openBrowserLocked()
}

func (a *engine) openBrowserLocked() {
	if err := platform.Open(a.srv.LaunchURL()); err != nil {
		a.log.Error("open browser", "err", err)
	}
}

func (a *engine) closeWindow() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.window != nil {
		a.window.Kill()
		a.window = nil
	}
}

// maxLogSize rotates the log once: the application has no console, so the
// log file is where errors go, but it must not grow forever.
const maxLogSize = 5 << 20

func openLog(dir string) (*slog.Logger, func()) {
	path := filepath.Join(dir, "desktop.log")
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})), func() { f.Close() }
}

// LogPath returns where the engine writes its log, for error messages.
func LogPath() string {
	dir, err := config.Dir()
	if err != nil {
		return "desktop.log"
	}
	return filepath.Join(dir, "desktop.log")
}
