// Package pytestcmd recognizes commands that run pytest directly. The policy
// validation, the harness and the mutation stage share it, so the three
// agree on which argv is a pytest run.
package pytestcmd

import (
	"path/filepath"
	"strings"
)

// Args returns the arguments a command passes to pytest itself: what follows
// pytest (or py.test), or "-m pytest" after a Python interpreter. It returns
// nil for a command that does not run pytest directly, for example through a
// shell, a package manager or a task runner.
func Args(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	switch base := filepath.Base(command[0]); {
	case base == "pytest" || base == "py.test":
		return command[1:]
	case IsInterpreter(base) && len(command) >= 3 && command[1] == "-m" && command[2] == "pytest":
		return command[3:]
	}
	return nil
}

// Is reports whether command runs pytest directly (Args is not nil).
func Is(command []string) bool { return Args(command) != nil }

// IsInterpreter reports whether a command name is a Python interpreter:
// python, python3 or python3.N.
func IsInterpreter(base string) bool {
	if base == "python" || base == "python3" {
		return true
	}
	rest, ok := strings.CutPrefix(base, "python3.")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
