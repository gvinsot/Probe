//go:build !windows && !(darwin && cgo)

package window

import "github.com/gvinsot/Probe/desktop/internal/platform"

// Run opens the interface in the default browser where no native web view
// is built in (Linux, or macOS built without cgo).
func Run(url, dataDir string) {
	platform.Open(url)
}
