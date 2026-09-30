---
name: context
description: Manage what Probe reviews against — the base branch, the intent and acceptance criteria of the change, the pre-change plan, and the team's trusted policy and knowledge base. Use when the user wants to set or show the review context, record the task's acceptance criteria, or make a plan before coding.
argument-hint: "[show | base REF | intent [TEXT] | plan | clear]"
allowed-tools: Bash(node *) Bash(probe *) Bash(git show *) Bash(git rev-parse *) Bash(git ls-files *) Read Write Glob
---

# Probe review context

Probe judges a change against its context: which base it starts from, what
the change was meant to do, what plan was agreed, and the team's trusted
policy. This skill keeps that context in `.probe/` (Probe's output directory,
normally git-ignored), where `/probe:review` picks it up:

| File | Used as |
| --- | --- |
| `.probe/claude-context.json` | `{"base": "REF"}`, the base `/probe:review` uses by default |
| `.probe/intent.md` | `--intent-file`: the intent and its acceptance criteria |
| `.probe/PLAN.json`, `.probe/PLAN.md` | `--plan`: the pre-change plan written by `probe plan` |

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

## Action: `$ARGUMENTS`

### `show` (default)

Summarize the state above, then:

- intent: the criteria in `.probe/intent.md`, if any;
- plan: the head of `.probe/PLAN.md`, if any;
- policy: `git show BASE:.probe.json` — the **trusted** policy is the one at
  the tip of the base, not the working copy; point out a local `.probe.json`
  that differs from it (Probe will not use local edits);
- knowledge: whether `PROBE_KNOWLEDGE.md` exists at the base
  (`git show BASE:PROBE_KNOWLEDGE.md`), the codebase notes given to the AI
  reviewer.

### `base REF`

Check that REF resolves with `git rev-parse --verify REF`, then write `.probe/claude-context.json` with `{"base": "REF"}`
(create `.probe/` when missing). Use the remote branch the pull request
targets, usually `origin/main`.

### `intent [TEXT]`

Write `.probe/intent.md` from TEXT, or when TEXT is empty from the task the
user described in this conversation. Use this shape, because Probe turns the
list items under an "Acceptance criteria" heading into criteria `AC-1`,
`AC-2`… that the AI reviewer may test:

```markdown
# <one-line summary of the change>

<two or three sentences: what the change does and why>

## Acceptance criteria

- <observable, testable behavior>
- <…>
```

Write only criteria the user stated or clearly agreed to; ask when the task is
vague. Show the file and ask for corrections. The intent must be UTF-8 text.

### `plan`

Make Probe announce the change before code is written (needs the AI provider
of the trusted policy or `PROBE_REVIEWER_MODEL`): write the intent first if
missing, then run
`probe plan --intent-file .probe/intent.md --base BASE --ci`, and read
`.probe/PLAN.md` to the user: the files, exported signatures, critical paths
and dependency manifests the plan touches, and every flagged category. A
flagged plan needs a human decision before coding. Later, `/probe:review`
passes `--plan .probe/PLAN.json` and reports every difference between the diff
and the plan.

Never edit `PLAN.json` to make a change conform: write a new intent and run
`plan` again, and have it approved again.

### `clear`

Delete `.probe/claude-context.json`, `.probe/intent.md`, `.probe/PLAN.json`
and `.probe/PLAN.md` after confirming with the user. Reports stay.
