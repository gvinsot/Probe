---
name: fix
description: Work on a Probe finding — understand it, change the code when a change is warranted, and check with Probe that the finding is resolved. Use when the user asks to fix a Probe finding such as T3, or all high-severity findings.
argument-hint: "<ID> [more IDs] | --severity high"
disable-model-invocation: true
allowed-tools: Bash(node *) Bash(probe *) Bash(git diff *) Bash(git status *) Bash(git log *) Read Grep Glob Edit Write
---

# Fix Probe findings

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

## Findings to work on

`$ARGUMENTS`

Resolve the selection:

- ids: `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" show ID` for each
- `--severity LEVEL`: `node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" list --severity LEVEL`,
  then work through them most severe first

If the report is missing, run `/probe:review` first. If it is **STALE**, run
`/probe:review` again before editing: line numbers must match the code.

## For each finding

1. **Understand.** Show the finding, read the code at its location and what
   calls it (Grep), and the tests that cover it. Decide which case applies:
   - a real defect or a missing safeguard: fix it;
   - a missing test for changed behavior: add the test;
   - an intended change that Probe rightly flags for a human (an exported API
     change, a dependency bump, a sensitive path): **no code change**; explain
     why it is intended and what a reviewer should confirm.
   When unsure which case applies, ask the user before editing.
2. **Change** the smallest amount of code that resolves the cause. Keep the
   project's style. Run the relevant tests or build if they are quick.
3. **Report** per finding: what you changed (files and why) or why no change
   is needed.

## Verify with Probe

Probe analyzes **committed** files only, so an uncommitted fix is invisible to
it:

- If the user authorized commits, commit the fix with a message naming the
  finding, then run `/probe:review` again (same mode as the report) and check
  that the finding is gone or changed: `probe-report.mjs list` and compare.
- Otherwise, stop after the change and tell the user to commit and run
  `/probe:review` to confirm; do not commit on your own.

A finding that remains after a correct fix (for example "exported declaration
changed") is expected when the change itself is intended: it asks for a human
look, not for more edits.

## Never

- edit `.probe.json`, the base branch, a baseline or Probe's output to make a
  finding disappear;
- delete or weaken a test, a validation or an error path to silence a signal;
- claim the code is correct because Probe exits 0 or a check passes.
