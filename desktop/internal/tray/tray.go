// Package tray shows Probe Desktop in the Windows notification area or the
// macOS menu bar. It is the only visible part of the engine while no window
// is open.
package tray

import (
	"runtime"
	"sync"

	"fyne.io/systray"

	"github.com/gvinsot/Probe/desktop/internal/i18n"
	"github.com/gvinsot/Probe/desktop/internal/icon"
	"github.com/gvinsot/Probe/desktop/internal/msg"
)

// Actions are the engine operations the menu triggers.
type Actions struct {
	Open         func()
	OpenBrowser  func()
	Scan         func()
	AutoStart    func() bool
	SetAutoStart func(bool) error
	Quit         func()
	// Language returns the language of the menu, from the settings.
	Language func() string
}

// Tray is the running tray icon.
type Tray struct {
	mu       sync.Mutex
	lang     string
	items    []labelled
	status   *systray.MenuItem
	alerting bool
	total    int
	toReview int
}

// labelled is a menu item with its English title and tooltip, relabelled
// when the language changes.
type labelled struct {
	item           *systray.MenuItem
	title, tooltip string
}

// Run shows the icon and blocks until Quit. It must run on the main thread.
// ready is called once the icon exists.
func Run(a Actions, ready func(*Tray)) {
	systray.Run(func() { ready(setup(a)) }, func() {})
}

// Quit removes the icon and makes Run return.
func Quit() { systray.Quit() }

func setup(a Actions) *Tray {
	t := &Tray{lang: "en"}
	if a.Language != nil {
		t.lang = a.Language()
	}
	t.setIcon(false)
	systray.SetTooltip("Probe Desktop")
	if runtime.GOOS == "windows" {
		// A left click opens the window; the menu stays on the right click.
		// macOS shows the menu on click, as users expect in the menu bar.
		systray.SetOnTapped(a.Open)
	}

	add := func(item *systray.MenuItem, title, tooltip string) *systray.MenuItem {
		t.items = append(t.items, labelled{item, title, tooltip})
		item.SetTitle(i18n.T(t.lang, title))
		item.SetTooltip(i18n.T(t.lang, tooltip))
		return item
	}
	open := add(systray.AddMenuItem("", ""), msg.M("Open Probe Desktop"), msg.M("Show the documents to review"))
	t.status = systray.AddMenuItem(i18n.T(t.lang, msg.M("Starting…")), "")
	t.status.Disable()
	systray.AddSeparator()
	scan := add(systray.AddMenuItem("", ""), msg.M("Scan now"), msg.M("Look for modified documents now"))
	browser := add(systray.AddMenuItem("", ""), msg.M("Open in the browser"), msg.M("Show the interface in the default browser"))
	autostart := add(systray.AddMenuItemCheckbox("", "", a.AutoStart()), msg.M("Start at login"), msg.M("Start watching when you log in"))
	systray.AddSeparator()
	quit := add(systray.AddMenuItem("", ""), msg.M("Quit"), msg.M("Stop watching the documents"))

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

// SetLanguage relabels the menu, after the settings changed.
func (t *Tray) SetLanguage(lang string) {
	t.mu.Lock()
	if lang == t.lang {
		t.mu.Unlock()
		return
	}
	t.lang = lang
	total, toReview := t.total, t.toReview
	t.mu.Unlock()
	for _, l := range t.items {
		l.item.SetTitle(i18n.T(lang, l.title))
		l.item.SetTooltip(i18n.T(lang, l.tooltip))
	}
	t.SetCounts(total, toReview)
}

// SetCounts updates the icon, the tooltip and the status line.
func (t *Tray) SetCounts(total, toReview int) {
	t.mu.Lock()
	t.total, t.toReview = total, toReview
	lang := t.lang
	t.mu.Unlock()
	var line string
	switch {
	case total == 0:
		line = i18n.T(lang, msg.M("No document watched"))
	case toReview == 0:
		line = i18n.Tf(lang, msg.M("%d documents · nothing to review"), total)
	case toReview == 1:
		line = i18n.Tf(lang, msg.M("%d documents · 1 to review"), total)
	default:
		line = i18n.Tf(lang, msg.M("%d documents · %d to review"), total, toReview)
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
