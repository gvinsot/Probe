package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// The CLI runs every check of a review in the prepared image: a Go test that
// needs a file only the base-branch prepare command wrote passes there, a
// second review reuses the image without running the command, and the same
// review without prepare fails that test on the plain sandbox image.
func TestDockerReviewUsesPreparedImage(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	prepared := "/go/pkg/swiftproof-cli-prepare-" + hex.EncodeToString(suffix)
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/prepared\n\ngo 1.23\n")
	write(t, dir, "deps.txt", "dep v1\n")
	write(t, dir, "calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	write(t, dir, "calc_test.go", "package calc\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestPreparedDependency(t *testing.T) {\n\tb, err := os.ReadFile(os.Getenv(\"PREPARED_DIR\") + \"/deps.txt\")\n\tif err != nil || string(b) != \"dep v1\\n\" {\n\t\tt.Fatalf(\"prepared file: %q %v\", b, err)\n\t}\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"2+3\")\n\t}\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "candidate")
	write(t, dir, "calc.go", "package calc\n\nfunc Add(a, b int) int { return b + a }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "candidate")

	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Commands = map[string][]string{"test": {"go", "test", "./..."}}
	cfg.Prepare = &config.Prepare{Command: []string{"sh", "-c", `mkdir -p "$PREPARED_DIR" && cp deps.txt "$PREPARED_DIR/"`}, Inputs: []string{"deps.txt"}, Env: map[string]string{"PREPARED_DIR": prepared}}
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeReviewerPolicy(t, policy, cfg)
	review := func(policy string) (int, model.Report, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", "report"}, &out, &errOut, "prepare-docker-test")
		return code, readReviewerReport(t, dir), errOut.String()
	}
	code, r, stderr := review(policy)
	if r.Prepare != nil && r.Prepare.ImageID != "" {
		defer exec.Command("docker", "image", "rm", r.Prepare.ImageID).Run()
	}
	if code != 0 || r.Prepare == nil || r.Prepare.Status != model.PrepareBuilt || len(r.Checks) != 1 || r.Checks[0].Status != "PASS" {
		t.Fatalf("exit %d prepare %+v checks %+v unverified %q\n%s", code, r.Prepare, r.Checks, r.Unverified, stderr)
	}
	code, again, stderr := review(policy)
	if code != 0 || again.Prepare.Status != model.PrepareReused || again.Prepare.ImageID != r.Prepare.ImageID || again.Checks[0].Status != "PASS" || !strings.Contains(stderr, "Reusing prepared image") {
		t.Fatalf("second review exit %d prepare %+v checks %+v\n%s", code, again.Prepare, again.Checks, stderr)
	}
	cfg.Prepare = nil
	cfg.Commands = map[string][]string{"test": {"sh", "-c", "PREPARED_DIR=" + prepared + " go test ./..."}}
	control := filepath.Join(t.TempDir(), "control.json")
	writeReviewerPolicy(t, control, cfg)
	code, plain, _ := review(control)
	if code != 2 || plain.Prepare != nil || plain.Checks[0].Status != "FAIL" || !strings.Contains(plain.Checks[0].Output, "prepared file") {
		t.Fatalf("control exit %d checks %+v", code, plain.Checks)
	}
}
