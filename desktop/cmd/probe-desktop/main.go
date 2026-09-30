// Command probe-desktop watches the Word, Excel and PowerPoint documents of
// folders (OneDrive, SharePoint, Google Drive, Dropbox, network shares…) and
// of Google Drives read through their API, and flags the risky
// modifications.
//
// The same executable plays two roles:
//
//	probe-desktop                 engine: tray icon, watcher, local server,
//	                              and opens the window
//	probe-desktop --background    engine without opening the window (login)
//	probe-desktop --background --restart
//	                              engine started by the previous one after
//	                              an update, which waits for its lock
//	probe-desktop --window URL    the window process, started by the engine
//
// On Windows it is built with -H=windowsgui: no console window ever appears,
// so errors go to the log file of the data directory.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/app"
	"github.com/gvinsot/Probe/desktop/internal/autostart"
	"github.com/gvinsot/Probe/desktop/internal/config"
	"github.com/gvinsot/Probe/desktop/internal/platform"
	"github.com/gvinsot/Probe/desktop/internal/window"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	defer crashReport()
	// Both the window and the tray menu must be sharp on a scaled display.
	platform.EnableHighDPI()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case app.WindowFlag:
			if len(os.Args) < 3 {
				os.Exit(2)
			}
			dir, err := config.Dir()
			if err != nil {
				os.Exit(1)
			}
			window.Run(os.Args[2], dir)
			return
		case "version", "--version":
			fmt.Printf("probe-desktop %s\n", version)
			return
		}
	}
	opts := app.Options{Version: version}
	for _, arg := range os.Args[1:] {
		switch arg {
		case autostart.BackgroundFlag:
			opts.Background = true
		case app.RestartFlag:
			opts.Restart = true
		}
	}
	if err := app.Run(opts); err != nil {
		logFatal(err.Error())
		os.Exit(1)
	}
}

// crashReport writes a panic to the log: without a console it would vanish.
func crashReport() {
	if r := recover(); r != nil {
		logFatal(fmt.Sprintf("panic: %v\n%s", r, debug.Stack()))
		os.Exit(2)
	}
}

func logFatal(msg string) {
	f, err := os.OpenFile(app.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "time=%s level=ERROR msg=%q\n", time.Now().Format(time.RFC3339), msg)
}
