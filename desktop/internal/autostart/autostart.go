// Package autostart registers Probe Desktop to start, in the background,
// when the user logs in: a Run registry value on Windows, a LaunchAgent on
// macOS. Both are per user and need no administrator rights.
package autostart

// BackgroundFlag starts the engine without opening its window.
const BackgroundFlag = "--background"
