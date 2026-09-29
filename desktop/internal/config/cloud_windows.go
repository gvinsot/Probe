package config

import "golang.org/x/sys/windows"

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
