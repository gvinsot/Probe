// Package platform wraps the few operating system calls of the application:
// opening a URL or a document with the default application, and starting a
// helper process. None of them opens a console window.
package platform

import (
	"os"
	"os/exec"
)

// Start launches a detached helper process. On Windows it never gets a
// console, even if the executable was built as a console program.
func Start(exe string, args ...string) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	hideConsole(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
