//go:build !windows

package update

import "os/exec"

func hideConsole(*exec.Cmd) {}
