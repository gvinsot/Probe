package config

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// CloudFolder is a synchronized folder found on this computer.
type CloudFolder struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// DetectCloudFolders lists the OneDrive and Google Drive folders the desktop
// clients created, so the user can pick one instead of typing a path.
func DetectCloudFolders() []CloudFolder {
	out := []CloudFolder{}
	seen := map[string]bool{}
	add := func(label, path string) {
		if path == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(path))
		if seen[key] {
			return
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			seen[key] = true
			out = append(out, CloudFolder{Label: label, Path: filepath.Clean(path)})
		}
	}

	switch runtime.GOOS {
	case "windows":
		add("OneDrive (work or school)", os.Getenv("OneDriveCommercial"))
		add("OneDrive (personal)", os.Getenv("OneDriveConsumer"))
		add("OneDrive", os.Getenv("OneDrive"))
		// Google Drive for desktop mounts a virtual drive, G: by default.
		for _, root := range localDriveRoots() {
			for _, name := range []string{"My Drive", "Mon Drive", "Shared drives", "Drives partagés"} {
				add("Google Drive · "+name, filepath.Join(root, name))
			}
		}
		if home, err := os.UserHomeDir(); err == nil {
			add("Google Drive · My Drive (mirrored)", filepath.Join(home, "My Drive"))
			add("Google Drive · Mon Drive (mirrored)", filepath.Join(home, "Mon Drive"))
		}
	case "darwin":
		// Both clients use the File Provider location on current macOS.
		home, err := os.UserHomeDir()
		if err != nil {
			break
		}
		base := filepath.Join(home, "Library", "CloudStorage")
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			name := e.Name()
			switch {
			case strings.HasPrefix(name, "OneDrive"):
				add(strings.ReplaceAll(name, "-", " · "), filepath.Join(base, name))
			case strings.HasPrefix(name, "GoogleDrive-"):
				account := strings.TrimPrefix(name, "GoogleDrive-")
				for _, sub := range []string{"My Drive", "Mon Drive", "Shared drives", "Drives partagés"} {
					add("Google Drive · "+account+" · "+sub, filepath.Join(base, name, sub))
				}
			}
		}
		add("OneDrive", filepath.Join(home, "OneDrive"))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
