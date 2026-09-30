package cli

import (
	"os"
	"testing"
)

// TestMain points the user cache directory, where the repository graph is
// cached by default, at a temporary directory: the tests never write to the
// cache of the person running them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "probe-cli-test-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir) // Linux and other Unix systems
	os.Setenv("LocalAppData", dir)   // Windows
	os.Setenv("HOME", dir)           // macOS: $HOME/Library/Caches
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
