//go:build darwin && cgo

package window

import webview "github.com/webview/webview_go"

// Run shows the interface in a WKWebView window and blocks until it closes.
func Run(url, dataDir string) {
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle(Title)
	w.SetSize(Width, Height, webview.HintNone)
	w.Bind(PickFolderBinding, pickFolder)
	w.Navigate(url)
	w.Run()
}
