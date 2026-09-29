package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gvinsot/Probe/hub/internal/config"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// analyzePlan asks the trusted CLI for a read-only proposal at the selected
// commit's parent. Its CLI decision is displayed separately from a review.
func (r *Runner) analyzePlan(ctx context.Context, work, base string, j Job, run *store.Run, repoName string) (*store.Record, error) {
	run.Mode = "plan"
	if os.Getenv(config.EndpointEnvName) == "" || os.Getenv(config.ModelEnvName) == "" {
		return nil, fmt.Errorf("plan analysis requires a deployment-configured reviewer endpoint and model")
	}
	output, code, runErr := r.executeCLI(ctx, work, []string{"plan", "--repo", work, "--base", base, "--intent", j.Intent, "--out", ".probe", "--ci"})
	data, err := readBounded(filepath.Join(work, ".probe/PLAN.json"), 4<<20)
	if err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("probe plan exited %d: %s", code, tail(output))
		}
		return nil, fmt.Errorf("no plan was produced: %w", err)
	}
	var plan struct {
		Format      string `json:"format"`
		Version     int    `json:"version"`
		ToolVersion string `json:"tool_version"`
		BaseCommit  string `json:"base_commit"`
		ExitCode    int    `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if plan.Format != "probe-plan" || plan.Version != 1 || plan.BaseCommit != base {
		return nil, fmt.Errorf("invalid plan format or base commit")
	}
	run.Summary = report.Summary{Verdict: report.Verdict(plan.ExitCode), ExitCode: plan.ExitCode, ToolVersion: plan.ToolVersion}
	run.ToolVersion = plan.ToolVersion
	rec := &store.Record{UserKey: j.UserKey, RepoKey: j.RepoKey, RepoName: repoName, Raw: data}
	if code < 0 || code >= 3 {
		return rec, fmt.Errorf("probe plan could not complete (exit %d): %s", code, tail(output))
	}
	return rec, nil
}

func (r *Runner) executeCLI(ctx context.Context, work string, args []string) (string, int, error) {
	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Dir = work
	cmd.Env = cliEnv(work)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = -1
	}
	return out.String(), code, err
}
