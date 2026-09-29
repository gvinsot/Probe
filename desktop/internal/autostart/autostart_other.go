//go:build !windows && !darwin

package autostart

import "errors"

// Enabled is always false where start at login is not implemented.
func Enabled() bool { return false }

// Set is not implemented on this system.
func Set(bool) error { return errors.New("start at login is not supported on this system") }
