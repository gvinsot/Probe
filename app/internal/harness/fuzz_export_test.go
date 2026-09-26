package harness

import (
	"context"
	"io"
	"sync"
)

// Test hooks for the external Docker test of differential fuzzing
// (fuzz_docker_test.go, package harness_test), which needs the fuzz package
// and therefore cannot live in package harness.

// WatchContainers records the name of every container the harness starts from
// now on, so that a Docker test can check what its own runs left behind among
// the containers other jobs on the host start concurrently.
func WatchContainers(h *Harness) func() []string {
	var mu sync.Mutex
	var names []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, name)
	}
	execute, capture := h.execute, h.executeCapture
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		record(name)
		return execute(ctx, name, args, out)
	}
	h.executeCapture = func(ctx context.Context, name string, args []string, log, payload io.Writer) execution {
		record(name)
		return capture(ctx, name, args, log, payload)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), names...)
	}
}

// SnapshotDirs returns the harness's private baseline and candidate
// snapshots.
func SnapshotDirs(h *Harness) (base, candidate string) {
	return h.base, h.candidate
}
