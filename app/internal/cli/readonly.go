package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

func validateReadOnlyFlags(mode string, explicit map[string]bool, readOnly, checks, useReviewer, network, prepareNetwork bool) error {
	if explicit["read-only"] && mode != "review" {
		return fmt.Errorf("--read-only applies to review only")
	}
	if !readOnly {
		return nil
	}
	if explicit["checks"] && checks || explicit["reviewer"] && !useReviewer || network || prepareNetwork {
		return fmt.Errorf("--read-only requires the reviewer and forbids checks or sandbox network access")
	}
	for _, name := range executionOnlyFlags {
		if name != "deadline" && explicit[name] {
			return fmt.Errorf("--%s cannot be combined with --read-only", name)
		}
	}
	return nil
}

func runReadOnlyReview(ctx context.Context, repo *gitrepo.Repository, r *model.Report, symbols harness.SymbolIndex, options reviewer.Options, output string, progress io.Writer) error {
	temp, err := os.MkdirTemp("", "probe-read-only-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	candidate := filepath.Join(temp, "candidate")
	if err := repo.Snapshot(ctx, r.Change.HeadCommit, candidate); err != nil {
		return fmt.Errorf("candidate snapshot: %w", err)
	}
	diff, err := json.Marshal(r.Change)
	if err != nil {
		return err
	}
	// No image, commands, cache or network settings are passed to this harness.
	// Snapshot inspection neither initializes Docker nor executes repository code.
	h, err := harness.NewContext(ctx, harness.Options{CandidateDir: candidate, ArtifactDir: filepath.Join(output, "artifacts"), Diff: string(diff), Symbols: symbols})
	if err != nil {
		return err
	}
	defer h.Close()
	fmt.Fprintln(progress, "Reviewing with the configured LLM (read-only; no code or tests are executed)...")
	options.ReadOnly = true
	err = reviewer.Run(ctx, options, r, reviewer.ReadOnlyTools{Harness: h})
	r.Evidence = h.Evidence()
	r.Audit = append(r.Audit, h.Audit()...)
	sort.SliceStable(r.Audit, func(i, j int) bool { return r.Audit[i].Time.Before(r.Audit[j].Time) })
	return err
}
