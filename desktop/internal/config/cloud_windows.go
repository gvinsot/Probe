package config

import (
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// localDriveRoots lists the local drives, skipping network shares and
// optical drives: probing a disconnected share can block for a long time.
func localDriveRoots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var roots []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		switch windows.GetDriveType(p) {
		case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_RAMDISK:
			roots = append(roots, root)
		}
	}
	return roots
}

// oneDriveLibraries lists the folders the OneDrive client synchronizes,
// SharePoint and Teams libraries included: it records each one under
// HKCU\Software\SyncEngines\Providers\OneDrive with its MountPoint.
func oneDriveLibraries() []CloudFolder {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\SyncEngines\Providers\OneDrive`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var out []CloudFolder
	for _, name := range names {
		sub, err := registry.OpenKey(k, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		mount, _, err := sub.GetStringValue("MountPoint")
		sub.Close()
		if err != nil || mount == "" {
			continue
		}
		out = append(out, CloudFolder{Label: "OneDrive · " + filepath.Base(mount), Path: mount})
	}
	return out
}
