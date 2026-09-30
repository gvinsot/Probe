//go:build darwin && cgo

package window

import (
	"errors"
	"os/exec"
	"strings"
)

// pickFolder asks for a folder with the standard macOS dialog, through
// AppleScript, and returns "" when the person cancels.
func pickFolder() (string, error) {
	out, err := exec.Command("osascript", "-e", `POSIX path of (choose folder with prompt "Choose a folder to watch")`).Output()
	if err != nil {
		var exit *exec.ExitError
		// "User canceled" (-128) exits with status 1.
		if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "-128") {
			return "", nil
		}
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	return path, nil
}
