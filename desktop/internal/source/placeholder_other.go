//go:build !windows && !darwin

package source

import "io/fs"

func cloudOnly(fs.FileInfo) bool { return false }
