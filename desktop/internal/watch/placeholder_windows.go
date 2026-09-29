package watch

import (
	"io/fs"
	"syscall"
)

// Attributes the OneDrive and Google Drive clients set on files whose
// content is not on disk yet ("online-only").
const (
	attrOffline            = 0x00001000
	attrRecallOnOpen       = 0x00040000
	attrRecallOnDataAccess = 0x00400000
)

func cloudOnly(info fs.FileInfo) bool {
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && a.FileAttributes&(attrOffline|attrRecallOnOpen|attrRecallOnDataAccess) != 0
}
