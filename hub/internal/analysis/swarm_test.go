package analysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/hub/internal/config"
)

// The operator's swarm setting reaches the CLI only when a reviewer runs.
func TestRunCLIPassesTheSwarmSetting(t *testing.T) {
	binary := fakeCLI(t, `echo "$@" > args.txt`)
	for _, tc := range []struct {
		name, swarm, endpoint, mode, want, absent string
	}{
		{"default agents", config.SwarmAll, "https://provider.example/v1", config.ModeReadOnly, "--swarm", "--swarm-agents"},
		{"chosen agents", "security,tests", "https://provider.example/v1", config.ModeReadOnly, "--swarm-agents security,tests", ""},
		{"one reviewer", "", "https://provider.example/v1", config.ModeReadOnly, "--read-only", "--swarm"},
		{"no reviewer", config.SwarmAll, "", config.ModeReview, "--reviewer=false", "--swarm"},
		{"lint", config.SwarmAll, "https://provider.example/v1", config.ModeLint, "lint", "--swarm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testRunner(t, binary)
			r.cfg.Swarm = tc.swarm
			t.Setenv(config.EndpointEnvName, tc.endpoint)
			t.Setenv(config.ModelEnvName, "test")
			work := t.TempDir()
			if _, code, err := r.runCLI(context.Background(), work, tc.mode, "base", "head", reviewerInputs{}); code != 0 || err != nil {
				t.Fatalf("exit %d: %v", code, err)
			}
			args, _ := os.ReadFile(filepath.Join(work, "args.txt"))
			if !strings.Contains(string(args), tc.want) || tc.absent != "" && strings.Contains(string(args), tc.absent) {
				t.Fatalf("arguments %q", args)
			}
		})
	}
}
