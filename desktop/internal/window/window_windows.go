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
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		// The runtime keeps its profile (cookies, cache) next to the data,
		// not next to the executable, which may be read-only.
		DataPath: filepath.Join(dataDir, "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  Title,
			Width:  Width,
			Height: Height,
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
)

const (
	wmSetIcon  = 0x0080
	iconSmall  = 0
	iconBig    = 1
	lrDefault  = 0
	iconFormat = 0x00030000
)

// setWindowIcon gives the window the Probe icon: the executable carries no
// icon resource, so the title bar and the taskbar would show a blank one.
func setWindowIcon(hwnd unsafe.Pointer) {
	for _, s := range []struct {
		size int
		kind uintptr
	}{{16, iconSmall}, {32, iconBig}} {
		data := icon.PNG(s.size, false)
		h, _, _ := procCreateIconFromResEx.Call(
			uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, iconFormat,
			uintptr(s.size), uintptr(s.size), lrDefault)
		if h != 0 {
			procSendMessage.Call(uintptr(hwnd), wmSetIcon, s.kind, h)
		}
	}
}
