# Probe for Claude Code

The official Claude Code plugin for [Probe](https://probe.technology/): request
reviews of your committed changes, read the findings, manage the context Probe
reviews against, and fix issues without leaving the session.

## Install

In Claude Code:

```text
/plugin marketplace add gvinsot/Probe
/plugin install probe@probe
```

or from a shell: `claude plugin marketplace add gvinsot/Probe` then
`claude plugin install probe@probe`.

The plugin drives the `probe` binary; install it from
[probe.technology/download](https://probe.technology/download.html), or run
`/probe:setup`, which checks the binary, the trusted `.probe.json` policy and
the sandbox image.

## Commands

| Command | What it does |
| --- | --- |
| `/probe:review [lint\|full\|read-only] [--base REF]` | Runs Probe on the committed changes and presents the findings by category. `lint` (default) needs no Docker and no AI provider; `full` runs the policy's checks in the sandbox; `read-only` asks the configured AI reviewer. |
| `/probe:findings [ID\|--severity high\|--path TEXT\|--all]` | Lists the findings of the latest report, or explains one (`T3`, a check, a hypothesis or a signal) with its evidence and the code it points to. |
| `/probe:fix <ID…>\|--severity high` | Works on findings: understands each one, changes the code when a change is warranted (or explains why the flagged change is intended), then checks with Probe that it is resolved. |
| `/probe:context [show\|base REF\|intent [TEXT]\|plan\|clear]` | Manages what Probe reviews against: the base branch, the intent and acceptance criteria of the change, the pre-change plan (`probe plan`), the trusted policy and the knowledge base. |
| `/probe:setup [check\|init\|sandbox\|provider]` | Gets Probe ready in the repository. |
| `/probe:graph [question\|search\|neighbors\|path]` | Explores the repository graph of the commit — callers, dependents, dependencies, implementations, paths between two parts of the code — to see what a change affects beyond the diff. |

The `probe-reviewer` agent runs a review and returns a short handoff, so Claude
can check a change before handing it over without loading the full report into
the conversation.

## How it works

- Findings are read through `scripts/probe-report.mjs`, a dependency-free Node
  script that prints compact views of `.probe/confidence-report.json`
  (`summary`, `list`, `show ID`, `status`). A real report can weigh megabytes;
  Claude only sees what it needs. Review targets are numbered `T1`, `T2`…
  most severe first.
- `status` is injected at the top of every command: the binary, the base, the
  intent and plan in use, uncommitted changes, and whether the report is stale
  (it reviewed another commit than `HEAD`).
- The review context lives in Probe's output directory, `.probe/`
  (`claude-context.json`, `intent.md`, `PLAN.json`), which `/probe:review`
  passes to Probe as `--base`, `--intent-file` and `--plan`.

## Ground rules the plugin follows

- Probe analyzes **committed** files only; the plugin says when changes are
  outside the analysis and never commits on its own unless you ask it to.
- A zero exit code, a passing check or a model assertion is not proof of
  correctness, and a signal is not a confirmed bug.
- It never edits `.probe.json`, the base branch or a baseline to make a
  finding disappear, and never weakens a test or a validation to silence one.

## Development

```sh
claude plugin validate ./plugins/probe --strict   # the plugin
claude plugin validate . --strict                 # the marketplace at the repository root
node --test plugins/probe/tests/*.test.mjs        # the report helper
claude --plugin-dir ./plugins/probe               # try it without installing
```

Bump `version` in `.claude-plugin/plugin.json` for every release: installed
copies stay on their version until it changes.
