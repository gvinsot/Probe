// Package window runs the native window of the interface in its own
// process. The engine owns the tray icon; keeping the web view in another
// process leaves each one its own main thread, which both need on macOS, and
// closing the window never stops the watching.
package window

// Title of the window.
const Title = "Probe Desktop"

// Size of a new window, in logical pixels.
const (
	Width  = 1200
	Height = 800
)
