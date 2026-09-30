---
name: setup
description: Get Probe ready in the current repository — check the probe binary, create and commit the trusted .probe.json policy, prepare the sandbox image for full reviews, and optionally configure an AI provider. Use when Probe is not installed, when the repository has no .probe.json, or when the user asks to set Probe up.
argument-hint: "[check | init | sandbox | provider]"
disable-model-invocation: true
allowed-tools: Bash(node *) Bash(probe *) Bash(git ls-files *) Bash(git check-ignore *) Bash(docker image inspect *) Read Glob
---

# Set up Probe

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

## Action: `$ARGUMENTS` (default: `check`, then whatever is missing)

### 1. The binary

When the state says **NOT INSTALLED**, give the user the install options and
let them choose; do not download and run a binary on your own:

- a release for Windows, macOS or Linux from
  https://probe.technology/download.html (every archive has a SHA-256 in
  `SHA256SUMS`), then put `probe` on the PATH;
- or build from source with Go 1.23+:
  `git clone https://github.com/gvinsot/Probe && cd Probe/app && go build -o probe ./cmd/probe`.

Then check with `probe version`.

### 2. The trusted policy (`init`)

Probe reads `.probe.json` **at the tip of the base branch**, so a pull request
cannot weaken its own review. Without one, it uses built-in defaults for the
detected language.

- If the state shows no committed `.probe.json`: run `probe init` (add
  `--language go|typescript|javascript|python|rust` when detection is wrong),
  then read the generated file to the user: its commands run in the sandbox,
  and `sandbox.image` must contain the toolchain and dependencies.
- The user reviews it, commits it and merges it into the base branch. Do not
  push or merge it yourself.

### 3. Ignore the output directory

Check `git check-ignore -q .probe/`; when it is not ignored, suggest adding
`.probe/` to `.gitignore`, since reports and the context of `/probe:context`
live there.

### 4. The sandbox image (`sandbox`), for `/probe:review full`

A full review runs the policy's commands in Docker without network, from an
image that must already be present: Probe never pulls images and never
installs dependencies from the change under review. Read `sandbox.image` from
`.probe.json` and check it with `docker image inspect IMAGE`. When it is
missing, tell the user to pull or build it in a trusted environment (for a Go
project without dependencies, `docker pull golang:1.26-bookworm`), or to let
the trusted policy build it with a `prepare` object. `lint` needs none of this.

### 5. AI provider (`provider`), optional

`lint` and `full` work without any provider. An AI reviewer adds
investigations and plans (`read-only`, `/probe:context plan`): it is set by
`reviewer.model` in the trusted policy, or by `PROBE_REVIEWER_MODEL` and
`PROBE_REVIEWER_ENDPOINT` in the environment, with the key in the variable the
policy's `api_key_env` names. Never write a key into a file of the repository.

### Finally

Summarize what is ready and what is missing, and suggest `/probe:review` to
try it on the latest commits.
