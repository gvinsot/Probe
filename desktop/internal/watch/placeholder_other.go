//go:build !windows && !darwin

package watch

import "io/fs"

func cloudOnly(fs.FileInfo) bool { return false }
