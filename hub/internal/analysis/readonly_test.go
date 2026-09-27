package analysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/hub/internal/config"
)

func TestReadOnlyRunsRestrictedCLIWithoutPolicyApproval(t *testing.T) {
	binary := fakeCLI(t, `echo "$@" > args.txt`)
	r, _ := testRunner(t, binary)
	r.cfg.Mode, r.cfg.Instance = config.ModeReadOnly, config.InstancePublic
	if mode := r.modeFor(context.Background(), nil, nil, "base"); mode != config.ModeReadOnly {
		t.Fatal(mode)
	}
	t.Setenv(config.EndpointEnvName, "https://provider.example/v1")
	t.Setenv(config.ModelEnvName, "test")
	work := t.TempDir()
	output, code, err := r.runCLI(context.Background(), work, config.ModeReadOnly, "base", "head")
	if err != nil || code != 0 {
		t.Fatalf("exit=%d %v %s", code, err, output)
	}
	args, err := os.ReadFile(filepath.Join(work, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"review --repo", "--read-only", "--exact", "--base base", "--head head", "--ci"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing %q: %s", want, args)
		}
	}
	if strings.Contains(string(args), "--reviewer=false") {
		t.Fatal("reviewer disabled")
	}
}

func TestReadOnlyMissingProviderFailsBeforeCLI(t *testing.T) {
	r, _ := testRunner(t, "nonexistent-cli")
	t.Setenv(config.EndpointEnvName, "")
	t.Setenv(config.ModelEnvName, "test")
	_, code, err := r.runCLI(context.Background(), t.TempDir(), config.ModeReadOnly, "base", "head")
	if code != 3 || err == nil || !strings.Contains(err.Error(), "deployment-configured") {
		t.Fatalf("exit=%d error=%v", code, err)
	}
}

func TestCLIForwardsProviderKeyFile(t *testing.T) {
	t.Setenv("SWIFTPROOF_API_KEY_FILE", "/run/secrets/provider-key")
	t.Setenv("SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET", "unrelated-secret")
	env := strings.Join(cliEnv(t.TempDir()), "\n")
	if !strings.Contains(env, "SWIFTPROOF_API_KEY_FILE=/run/secrets/provider-key") || strings.Contains(env, "unrelated-secret") {
		t.Fatal("CLI did not receive only its provider credential source")
	}
}
