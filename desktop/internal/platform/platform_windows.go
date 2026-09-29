package platform

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Open hands a URL or a file to the shell, like a double click: the default
// browser or the associated Office application opens it. ShellExecute starts
// no intermediate process, hence no console flash.
func Open(target string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
