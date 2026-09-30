---
name: review
description: Run Probe on the committed changes of the current branch and present its findings — reproduced issues, unresolved hypotheses, incomplete checks and the prioritized review targets. Use when the user asks for a Probe review, before handing over a committed change, or before opening a pull request.
argument-hint: "[lint|full|read-only] [--base REF] [extra probe flags]"
allowed-tools: Bash(probe *) Bash(git *) Bash(node *) Read Grep Glob
---

# Probe review

Probe reviews **committed** changes between a base and `HEAD`. It maps them to
risk signals, can run the repository's checks in a sandbox, and writes
`.probe/confidence-report.json` and `.probe/CONFIDENCE_REPORT.md`.

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

## Arguments

`$ARGUMENTS`

- First word selects the mode (default `lint`):
  - `lint` — static analysis only: no Docker, no AI provider, nothing of the
    repository is executed. Fast; use it by default.
  - `full` — `probe review`: runs the configured test, typecheck, build and
    coverage commands in disposable Docker containers. Needs Docker and the
    sandbox image of the policy preloaded (Probe never pulls images).
  - `read-only` — `probe review --read-only`: the configured AI reviewer
    inspects the diff without Docker; its suspicions stay unverified.
- `--base REF` overrides the base. Otherwise use the base saved by
  `/probe:context` (shown above), else the default branch shown above.
- Pass any other flag through to Probe unchanged.

## Steps

1. If the binary is missing, stop and run `/probe:setup` instead.
2. If there are uncommitted changes (see the state above), tell the user they
   are **outside** the analysis. Do not create a commit only to include them
   unless the user asked for commits.
3. Build the command, always with `--ci` so the exit code carries the
   decision:
   - `lint`: `probe lint --base BASE --ci`
   - `full`: `probe review --base BASE --ci`, plus `--reviewer=false` when no
     AI provider is configured (no `reviewer.model` in `.probe.json` and no
     `PROBE_REVIEWER_MODEL`)
   - `read-only`: `probe review --read-only --base BASE --ci`
   - add `--intent-file .probe/intent.md` when that file exists, and
     `--plan .probe/PLAN.json` when that file exists (see `/probe:context`)
4. Run it with Bash. A full review can take minutes: use a generous timeout
   (up to 10 minutes) or `--deadline`. Exit codes: 0 no reproduced blocker,
   1 reproduced high/critical issue, 2 human review required, 3 invalid
   arguments or configuration (fix the command, nothing ran), 4 operational
   error (report the message).
5. Read the result compactly — never paste the whole JSON into the
   conversation:
   `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" summary`
6. Present it with these categories kept **separate**, each only if non-empty:
   reproduced issues; behavior divergences (both recorded values; a
   divergence does not say which revision is right); intent-test failures
   (model-written tests, candidate only); unresolved hypotheses; checks that
   did not pass or did not run; unverified areas; then the top review targets
   with their `T` ids and `path:line`, and plan conformance when present.
7. Offer the next step: `/probe:findings T3` for details, `/probe:fix T3` to
   work on one, `/probe:review full` after a lint.

## Rules

- A zero exit code, a passing check or a model assertion is **not** proof of
  correctness. Signals are heuristics that point where to look.
- Never edit `.probe.json`, the base branch or a baseline to make a finding
  disappear.
- Report Probe's own words for statuses (`REPRODUCED`, `UNVERIFIED`…); do not
  upgrade a signal to a bug or downgrade a reproduced issue.
