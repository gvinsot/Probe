package watch

import (
	"io/fs"
	"syscall"
)

// sfDataless marks a File Provider file whose content is still in the cloud.
const sfDataless = 0x40000000

func cloudOnly(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}
