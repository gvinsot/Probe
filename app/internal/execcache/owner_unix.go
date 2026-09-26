//go:build unix

package execcache

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwner requires the cache directory to belong to the effective user and
// to grant no group or other permission, so that no other local account can
// plant or alter entries.
func checkOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the owner of %s cannot be determined", path)
	}
	if euid := os.Geteuid(); int64(stat.Uid) != int64(euid) {
		return fmt.Errorf("%s is owned by uid %d, not by the effective user %d", path, stat.Uid, euid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; it must grant no group or other permission (chmod 700)", path, perm)
	}
	return nil
}
