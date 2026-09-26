//go:build !unix

package execcache

import "os"

// checkOwner does not check ownership outside Unix. On Windows the directory
// keeps the ACLs it inherits; EXECUTION_CACHE.md tells operators to use a
// directory only their account can write.
func checkOwner(string, os.FileInfo) error { return nil }
