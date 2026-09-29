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
	"github.com/gvinsot/Probe/desktop/internal/watch"
)

// WindowFlag starts the executable as the window process.
const WindowFlag = "--window"

// Options of an engine run.
type Options struct {
	Version string
	// Background starts without opening the window (start at login).
	Background bool
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
	return srv.Shutdown(ctx)
}

type engine struct {
	dir string
	log *slog.Logger
	srv *server.Server

	mu     sync.Mutex
	window *os.Process
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
	exe, err := os.Executable()
	if err != nil {
		a.log.Error("locate executable", "err", err)
		a.openBrowserLocked()
		return
	}
	p, err := platform.Start(exe, WindowFlag, a.srv.LaunchURL())
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
