package execcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxExecutableBytes bounds how much of the running executable ToolIdentity
// reads.
const maxExecutableBytes = 256 << 20

// ToolIdentity returns the tool_version of every key this process computes:
// the version string followed by the SHA-256 of the running executable, so
// that any other build, including a development build with the same version
// string, never reads this build's entries.
func ToolIdentity(version string) (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, maxExecutableBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxExecutableBytes {
		return "", errors.New("the running executable exceeds 256 MiB")
	}
	return fmt.Sprintf("%s sha256:%s", version, hex.EncodeToString(digest.Sum(nil))), nil
}
