package window

import (
	"path/filepath"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"github.com/gvinsot/Probe/desktop/internal/icon"
	"github.com/gvinsot/Probe/desktop/internal/platform"
)

// Run shows the interface in a WebView2 window and blocks until it closes.
// Without the WebView2 runtime (preinstalled on Windows 10 and 11), the
// interface opens in the default browser instead.
func Run(url, dataDir string) {
	width, height := size()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		// The runtime keeps its profile (cookies, cache) next to the data,
		// not next to the executable, which may be read-only.
		DataPath: filepath.Join(dataDir, "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  Title,
			Width:  width,
			Height: height,
			Center: true,
		},
	})
	if w == nil {
		platform.Open(url)
		return
	}
	defer w.Destroy()
	setWindowIcon(w.Window())
	w.Navigate(url)
	w.Run()
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procCreateIconFromResEx = user32.NewProc("CreateIconFromResourceEx")
	procSendMessage         = user32.NewProc("SendMessageW")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
)

const (
	smCXScreen = 0
	smCYScreen = 1
	smCXIcon   = 11
	smCXSmIcon = 49
	wmSetIcon  = 0x0080
	iconSmall  = 0
	iconBig    = 1
	lrDefault  = 0
	iconFormat = 0x00030000
)

// size returns the size of a new window in physical pixels, which the web
// view library expects once the process is DPI aware. It stays within 90% of
// the primary screen, where the library centers it: a scaled 1080p display
// is shorter than the window.
func size() (uint, uint) {
	dpi := uint(96)
	// GetDpiForSystem also answers 96 to a process left DPI unaware, whose
	// pixels are then logical.
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d != 0 {
			dpi = uint(d)
		}
	}
	return fit(Width*dpi/96, metric(smCXScreen, 0)), fit(Height*dpi/96, metric(smCYScreen, 0))
}

// fit caps a window dimension to 90% of the screen dimension, when known.
func fit(n, screen uint) uint {
	if limit := screen * 9 / 10; screen != 0 && n > limit {
		return limit
	}
	return n
}

// metric returns a system metric, scaled to the DPI of the process, or def
// when it is not available.
func metric(index, def uint) uint {
	if v, _, _ := procGetSystemMetrics.Call(uintptr(index)); v != 0 {
		return uint(v)
	}
	return def
}

// setWindowIcon gives the window the Probe icon: the executable carries no
// icon resource, so the title bar and the taskbar would show a blank one.
func setWindowIcon(hwnd unsafe.Pointer) {
	for _, s := range []struct {
		size int
		kind uintptr
	}{{int(metric(smCXSmIcon, 16)), iconSmall}, {int(metric(smCXIcon, 32)), iconBig}} {
		data := icon.PNG(s.size, false)
		h, _, _ := procCreateIconFromResEx.Call(
			uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, iconFormat,
			uintptr(s.size), uintptr(s.size), lrDefault)
		if h != 0 {
			procSendMessage.Call(uintptr(hwnd), wmSetIcon, s.kind, h)
		}
	}
}
