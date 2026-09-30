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

var procSetDpiAwarenessContext = windows.NewLazySystemDLL("user32.dll").NewProc("SetProcessDpiAwarenessContext")

// dpiPerMonitorAwareV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, -4.
const dpiPerMonitorAwareV2 = ^uintptr(3)

// EnableHighDPI makes the process draw at the resolution of each monitor.
// Otherwise, when the display is scaled above 100%, Windows draws the windows
// at 96 DPI and stretches them as a bitmap, which blurs the text. It must run
// before the process creates its first window. Windows 10 before 1703 lacks
// the call: the process stays DPI unaware there, blurred but correctly sized.
func EnableHighDPI() {
	if procSetDpiAwarenessContext.Find() == nil {
		procSetDpiAwarenessContext.Call(dpiPerMonitorAwareV2)
	}
}

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
