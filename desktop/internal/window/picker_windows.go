package window

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The folder dialog is the shell's IFileOpenDialog in folder mode, called
// through its COM vtable.
var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")

	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

const (
	clsctxInprocServer = 0x1
	fosPickFolders     = 0x20
	fosForceFileSystem = 0x40
	fosPathMustExist   = 0x800
	sigdnFileSysPath   = 0x80058000
	hrCancelled        = 0x800704C7 // HRESULT_FROM_WIN32(ERROR_CANCELLED)

	// Vtable slots: IUnknown, then IModalWindow, IFileDialog and IShellItem.
	slotRelease        = 2
	slotShow           = 3
	slotSetOptions     = 9
	slotGetOptions     = 10
	slotSetTitle       = 17
	slotGetResult      = 20
	slotGetDisplayName = 5
)

// comCall calls a method of a COM object by its vtable slot.
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// pickFolder shows the folder dialog over the window and returns the chosen
// path, or "" when the person cancels. It runs on the thread of the web
// view, where COM is already initialized.
func pickFolder(owner unsafe.Pointer) (string, error) {
	var dlg unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
	if failed(hr) || dlg == nil {
		return "", fmt.Errorf("cannot open the folder dialog (0x%08X)", uint32(hr))
	}
	defer comCall(dlg, slotRelease)

	var opts uint32
	if hr := comCall(dlg, slotGetOptions, uintptr(unsafe.Pointer(&opts))); failed(hr) {
		return "", fmt.Errorf("folder dialog options (0x%08X)", uint32(hr))
	}
	comCall(dlg, slotSetOptions, uintptr(opts|fosPickFolders|fosForceFileSystem|fosPathMustExist))
	if title, err := windows.UTF16PtrFromString("Choose a folder to watch"); err == nil {
		comCall(dlg, slotSetTitle, uintptr(unsafe.Pointer(title)))
	}
	switch hr := comCall(dlg, slotShow, uintptr(owner)); {
	case uint32(hr) == hrCancelled:
		return "", nil
	case failed(hr):
		return "", fmt.Errorf("folder dialog (0x%08X)", uint32(hr))
	}

	var item unsafe.Pointer
	if hr := comCall(dlg, slotGetResult, uintptr(unsafe.Pointer(&item))); failed(hr) || item == nil {
		return "", fmt.Errorf("folder dialog result (0x%08X)", uint32(hr))
	}
	defer comCall(item, slotRelease)
	var name *uint16
	if hr := comCall(item, slotGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name))); failed(hr) || name == nil {
		return "", fmt.Errorf("the chosen folder has no path (0x%08X)", uint32(hr))
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	return windows.UTF16PtrToString(name), nil
}
