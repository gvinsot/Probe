// Package tray shows Probe Desktop in the Windows notification area or the
// macOS menu bar. It is the only visible part of the engine while no window
// is open.
package tray

import (
	"fmt"
	"runtime"

	"fyne.io/systray"

	"github.com/gvinsot/Probe/desktop/internal/icon"
)

// Actions are the engine operations the menu triggers.
type Actions struct {
	Open         func()
	OpenBrowser  func()
	Scan         func()
	AutoStart    func() bool
	SetAutoStart func(bool) error
	Quit         func()
}

// Tray is the running tray icon.
type Tray struct {
	status   *systray.MenuItem
	alerting bool
}

// Run shows the icon and blocks until Quit. It must run on the main thread.
// ready is called once the icon exists.
func Run(a Actions, ready func(*Tray)) {
	systray.Run(func() { ready(setup(a)) }, func() {})
}

// Quit removes the icon and makes Run return.
func Quit() { systray.Quit() }

func setup(a Actions) *Tray {
	t := &Tray{}
	t.setIcon(false)
	systray.SetTooltip("Probe Desktop")
	if runtime.GOOS == "windows" {
		// A left click opens the window; the menu stays on the right click.
		// macOS shows the menu on click, as users expect in the menu bar.
		systray.SetOnTapped(a.Open)
	}

	open := systray.AddMenuItem("Open Probe Desktop", "Show the documents to review")
	t.status = systray.AddMenuItem("Starting…", "")
	t.status.Disable()
	systray.AddSeparator()
	scan := systray.AddMenuItem("Scan now", "Look for modified documents now")
	browser := systray.AddMenuItem("Open in the browser", "Show the interface in the default browser")
	autostart := systray.AddMenuItemCheckbox("Start at login", "Start watching when you log in", a.AutoStart())
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Stop watching the documents")

	go func() {
		for {
			select {
			case <-open.ClickedCh:
				a.Open()
			case <-t.status.ClickedCh:
				a.Open()
			case <-scan.ClickedCh:
				a.Scan()
			case <-browser.ClickedCh:
				a.OpenBrowser()
			case <-autostart.ClickedCh:
				want := !autostart.Checked()
				if err := a.SetAutoStart(want); err == nil {
					if want {
						autostart.Check()
					} else {
						autostart.Uncheck()
					}
				}
			case <-quit.ClickedCh:
				a.Quit()
				return
			}
		}
	}()
	return t
}

// SetCounts updates the icon, the tooltip and the status line.
func (t *Tray) SetCounts(total, toReview int) {
	var line string
	switch {
	case total == 0:
		line = "No document watched"
	case toReview == 0:
		line = fmt.Sprintf("%d documents · nothing to review", total)
	case toReview == 1:
		line = fmt.Sprintf("%d documents · 1 to review", total)
	default:
		line = fmt.Sprintf("%d documents · %d to review", total, toReview)
	}
	t.status.SetTitle(line)
	systray.SetTooltip("Probe Desktop · " + line)
	if alert := toReview > 0; alert != t.alerting {
		t.setIcon(alert)
	}
}

func (t *Tray) setIcon(alert bool) {
	t.alerting = alert
	if runtime.GOOS == "windows" {
		systray.SetIcon(icon.ICO(alert))
		return
	}
	systray.SetIcon(icon.PNG(44, alert))
}
