package cli

// The trusted dependency-preparation stage (F8). A configured prepare policy
// runs before any candidate code, on inputs exported from the base commit, and
// produces the image every sandbox run of the review uses. It fails closed: no
// image means no execution and exit 4, never the unprepared image.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/prepare"
)

// preparation is the outcome of the prepare stage.
type preparation struct {
	image      string           // the derived image ID to run checks in; "" unless ok
	record     *model.Prepare   // the report's prepare object
	artifacts  []model.Artifact // prepare_output
	audit      []model.AuditEvent
	unverified []string
	ok         bool
}

// The Docker client of the prepare stage; tests substitute fakes.
var (
	prepareDocker prepare.Docker    = prepare.DockerCLI{}
	prepareRunner dockerutil.Runner = dockerutil.DefaultRunner
)

// prepareShadowedNote is the Unverified entry of a preparation whose every
// change is hidden from checks.
const prepareShadowedNote = "Dependency preparation: prepared outputs are shadowed by check mounts. Every change the prepare command made is under /workspace, /tmp or its HOME, which sandbox checks replace or do not use, so checks may have run without the prepared dependencies."

// prepareSignals returns the prepare_input_changed signals of a change that
// edits a declared prepare input (lint and review).
func prepareSignals(spec *config.Prepare, change model.Change) []model.Signal {
	return prepare.Signals(spec, change)
}

// runPrepare derives the sandbox image from inputs exported from the base
// commit. allowNetwork is the effective permission (policy prepare.network,
// --allow-prepare-network and not --no-network). It never falls back to the
// unprepared image.
func runPrepare(ctx context.Context, repo *gitrepo.Repository, cfg config.Config, change model.Change, artifactDir string, allowNetwork bool, version string, errOut io.Writer) preparation {
	if cfg.Prepare == nil {
		return preparation{}
	}
	var exporter prepare.Exporter
	if repo != nil {
		exporter = repo
	}
	res := prepare.Run(ctx, prepare.Options{
		Spec: *cfg.Prepare, BaseImage: cfg.Sandbox.Image, SourceCommit: change.BaseCommit, Repo: exporter,
		AllowNetwork: allowNetwork, MemoryMB: cfg.Sandbox.MemoryMB, CPUs: cfg.Sandbox.CPUs, MaxOutputBytes: cfg.Sandbox.MaxOutputBytes,
		ArtifactDir: artifactDir, ToolVersion: version, Docker: prepareDocker, Runner: prepareRunner, Progress: errOut,
	})
	record := res.Record
	p := preparation{record: &record, artifacts: res.Artifacts, audit: []model.AuditEvent{res.Audit}}
	switch {
	case res.Ready():
		p.image, p.ok = res.Image, true
		if changed := prepare.ChangedInputs(cfg.Prepare, change); len(changed) > 0 {
			p.unverified = append(p.unverified, "Candidate changes dependency-preparation inputs ("+prepare.ListPaths(changed, 20)+"); sandbox checks used dependencies prepared from the base commit's versions of the declared inputs only. Candidate dependency changes were not installed, so failures they cause are expected and are not evidence about the change.")
		}
		if res.Shadowed {
			p.unverified = append(p.unverified, prepareShadowedNote)
		}
	case record.Status == model.PrepareNotPermitted:
		fmt.Fprintf(errOut, "Dependency preparation was not permitted: %s\n", record.Reason)
		p.unverified = append(p.unverified, "Dependency preparation was not permitted: "+prepareSentence(record.Reason)+" The prepare command did not run; no repository code was executed and no check ran.")
	default:
		fmt.Fprintf(errOut, "Dependency preparation failed: %s\n", record.Reason)
		ran := "The prepare command did not run; no repository code was executed and no check ran."
		if res.Started {
			ran = "The base-branch prepare command was started in its container; no candidate code ran and no check ran."
		}
		p.unverified = append(p.unverified, "Dependency preparation failed: "+prepareSentence(record.Reason)+" "+ran)
	}
	return p
}

// prepareSentence ends s with a period.
func prepareSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no reason was recorded."
	}
	if strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

// prepareNotRun is the prepare object of a review that needed no execution.
func prepareNotRun(cfg config.Config, change model.Change, reason string) *model.Prepare {
	return prepareRecord(cfg, change, model.PrepareNotRun, reason)
}

// prepareRecord builds a prepare object for a stage that produced no image.
// Network stays false: no container ran.
func prepareRecord(cfg config.Config, change model.Change, status, reason string) *model.Prepare {
	p := &model.Prepare{
		Status: status, Reason: reason, SourceCommit: change.BaseCommit, Command: []string{},
		User: cfg.Prepare.EffectiveUser(), BaseImage: cfg.Sandbox.Image, Inputs: []model.PreparedInput{}, Note: model.PrepareNote,
	}
	if cfg.Prepare != nil {
		p.Command = append(p.Command, cfg.Prepare.Command...)
	}
	return p
}
