//go:build !windows

package platform

import (
	"os/exec"
	"runtime"
)

// Open hands a URL or a file to the desktop: "open" on macOS, xdg-open
// elsewhere.
func Open(target string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// EnableHighDPI does nothing: macOS and Linux scale the windows themselves.
func EnableHighDPI() {}

func hideConsole(*exec.Cmd) {}
