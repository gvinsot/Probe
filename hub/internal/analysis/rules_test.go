package analysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/hub/internal/config"
)

// The coding rules reach the CLI as a private file outside the checkout, only
// when a reviewer runs, and the file is removed once the CLI has finished.
func TestRunCLIPassesCodingRulesToTheReviewer(t *testing.T) {
	binary := fakeCLI(t, `echo "$@" > args.txt
while [ $# -gt 0 ]; do
  if [ "$1" = "--rules-file" ]; then cat "$2" > rules.txt; echo "$2" > rules-path.txt; fi
  shift
done`)
	r, _ := testRunner(t, binary)
	const rules = "- Never log credentials."
	for _, tc := range []struct {
		name, mode, endpoint string
		want                 bool
	}{
		{"review with a reviewer", config.ModeReview, "https://provider.example/v1", true},
		{"read-only review", config.ModeReadOnly, "https://provider.example/v1", true},
		{"review without a reviewer", config.ModeReview, "", false},
		{"lint", config.ModeLint, "https://provider.example/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EndpointEnvName, tc.endpoint)
			t.Setenv(config.ModelEnvName, "test")
			work := t.TempDir()
			if _, code, err := r.runCLI(context.Background(), work, tc.mode, "base", "head", reviewerInputs{rules: rules}); code != 0 || err != nil {
				t.Fatalf("exit %d: %v", code, err)
			}
			args, _ := os.ReadFile(filepath.Join(work, "args.txt"))
			if strings.Contains(string(args), "--rules-file") != tc.want {
				t.Fatalf("arguments %q", args)
			}
			if !tc.want {
				return
			}
			if got, _ := os.ReadFile(filepath.Join(work, "rules.txt")); string(got) != rules {
				t.Fatalf("rules file held %q", got)
			}
			path, _ := os.ReadFile(filepath.Join(work, "rules-path.txt"))
			name := strings.TrimSpace(string(path))
			if strings.HasPrefix(name, work) {
				t.Fatalf("rules file %s inside the checkout", name)
			}
			if _, err := os.Stat(name); !os.IsNotExist(err) {
				t.Fatalf("rules file %s left behind: %v", name, err)
			}
		})
	}
}
