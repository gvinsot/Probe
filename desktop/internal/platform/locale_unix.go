//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Locale returns the user's locale name, such as "fr_FR.UTF-8", or "" when it
// cannot be read. The POSIX variables come first; an application opened from
// the macOS Finder has none of them, so macOS falls back to the locale of
// the system settings.
func Locale() string {
	for _, v := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if l := os.Getenv(v); l != "" && l != "C" && l != "POSIX" && !strings.HasPrefix(l, "C.") {
			return l
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
