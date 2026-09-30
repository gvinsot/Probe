---
name: findings
description: Read the findings of the latest Probe report — list them, filter them by severity or path, or show one in detail with its signals, evidence or check output. Use when the user asks what Probe found, about a finding id such as T3, or before fixing a finding.
argument-hint: "[ID | --severity high | --path TEXT | --all]"
allowed-tools: Bash(node *) Read Grep Glob
---

# Probe findings

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

If there is no report, run `/probe:review` first. If the report is **STALE**,
say so: its line numbers describe the commit it reviewed, not `HEAD`.

## Arguments

`$ARGUMENTS`

The helper reads `.probe/confidence-report.json` and prints compact views.
Use it instead of reading the JSON file, which can be very large:

- no argument: `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" summary`
- a filter: `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" list $ARGUMENTS`
  (`--severity high` keeps high and critical, `--path TEXT` keeps paths
  containing TEXT, `--all` adds every raw signal)
- an id (`T3`, a check id, a hypothesis id or a `sig-…` signal id):
  `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" show ID`

`list` prints one finding per line: id, kind or status, severity, location,
reason. Review targets are numbered `T1`, `T2`… most severe first; the
numbering is stable for a given report.

## Presenting a finding

When the user asks about one finding:

1. Show it with `show ID`.
2. Open the code at its `path:line` (Read), a few lines around it, from the
   working copy — and warn if the report is stale.
3. Explain in plain words what Probe observed and why it matters, keeping
   Probe's own status. A signal is a heuristic pointer (for example "exported
   declaration changed"), not a confirmed bug; `REPRODUCED` means a
   differential experiment failed on the candidate and passed on the base;
   `UNVERIFIED` means nobody proved it either way.
4. Say what would settle it: a test to write, a caller to check, a question
   for the author. Offer `/probe:fix ID` when a code change is warranted.

Never soften or dismiss a reproduced issue, and never present a signal as a
proven defect.
