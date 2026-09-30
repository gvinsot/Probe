package config

import (
	"encoding/json"
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

// DetectCloudFolders lists the folders that the OneDrive, SharePoint,
// Google Drive, Dropbox, Box and iCloud clients created, so the user can pick
// one instead of typing a path.
func DetectCloudFolders() []CloudFolder {
	out := []CloudFolder{}
	seen := map[string]bool{}
	add := func(label, path string) {
		if path == "" {
			return
		}
		key := FoldPath(path)
		if seen[key] {
			return
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			seen[key] = true
			out = append(out, CloudFolder{Label: label, Path: filepath.Clean(path)})
		}
	}
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		add("OneDrive (work or school)", os.Getenv("OneDriveCommercial"))
		add("OneDrive (personal)", os.Getenv("OneDriveConsumer"))
		add("OneDrive", os.Getenv("OneDrive"))
		// SharePoint and Teams libraries synchronized by the OneDrive client.
		for _, f := range oneDriveLibraries() {
			add(f.Label, f.Path)
		}
		// Google Drive for desktop mounts a virtual drive, G: by default.
		for _, root := range localDriveRoots() {
			for _, name := range []string{"My Drive", "Mon Drive", "Shared drives", "Drives partagés"} {
				add("Google Drive · "+name, filepath.Join(root, name))
			}
		}
		if home != "" {
			add("Google Drive · My Drive (mirrored)", filepath.Join(home, "My Drive"))
			add("Google Drive · Mon Drive (mirrored)", filepath.Join(home, "Mon Drive"))
			add("Box", filepath.Join(home, "Box"))
			add("iCloud Drive", filepath.Join(home, "iCloudDrive"))
		}
		for _, f := range dropboxFolders(
			filepath.Join(os.Getenv("APPDATA"), "Dropbox", "info.json"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Dropbox", "info.json"),
		) {
			add(f.Label, f.Path)
		}
	case "darwin":
		if home == "" {
			break
		}
		// Current clients use the File Provider location.
		base := filepath.Join(home, "Library", "CloudStorage")
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			name := e.Name()
			switch {
			case strings.HasPrefix(name, "GoogleDrive-"):
				account := strings.TrimPrefix(name, "GoogleDrive-")
				for _, sub := range []string{"My Drive", "Mon Drive", "Shared drives", "Drives partagés"} {
					add("Google Drive · "+account+" · "+sub, filepath.Join(base, name, sub))
				}
			case strings.HasPrefix(name, "OneDrive"), strings.HasPrefix(name, "Dropbox"), strings.HasPrefix(name, "Box"):
				add(strings.ReplaceAll(name, "-", " · "), filepath.Join(base, name))
			}
		}
		add("OneDrive", filepath.Join(home, "OneDrive"))
		add("Box", filepath.Join(home, "Box"))
		add("iCloud Drive", filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs"))
		for _, f := range dropboxFolders(filepath.Join(home, ".dropbox", "info.json")) {
			add(f.Label, f.Path)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// dropboxFolders reads the folders of the Dropbox accounts from the
// info.json file the client writes.
func dropboxFolders(infoFiles ...string) []CloudFolder {
	var out []CloudFolder
	for _, file := range infoFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var info map[string]struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(data, &info) != nil {
			continue
		}
		for _, kind := range []string{"personal", "business"} {
			if a, ok := info[kind]; ok && a.Path != "" {
				out = append(out, CloudFolder{Label: "Dropbox (" + kind + ")", Path: a.Path})
			}
		}
	}
	return out
}
