# Trusted dependency preparation

Checks run offline in a preloaded image, and without the project's dependencies most checks cannot run at all. The optional `prepare` policy object lets the trusted base-branch policy build that dependency layer itself: before any candidate code runs, SwiftProof runs the policy's own command once, in one bounded container, on dependency files exported from the base commit, and commits the container as a local derived image. Every sandbox run of the review then uses that image by ID. A later review of the same base commit with the same inputs reuses the image without starting a container.

Preparation is part of the trusted environment, like the configured sandbox image. It is never a check, never evidence and never supports a hypothesis. It fails closed: when it produces no image, the review runs nothing else and exits 4, and it never falls back to the unprepared image.

The binding rules are in the [v0.4 specification](../../specs/swiftproof-v0.4-spec.md#f8-trusted-dependency-preparation) (refinements R1, R2 and R10).

## When it runs

- Only in `review`, and only when the trusted policy has a `prepare` object. The policy comes from the tip of `--base`, or from an explicit `--config`; a candidate branch cannot add, change or enable `prepare`.
- Only when the review will execute something: at least one changed file, and `--checks` or a reviewer. Otherwise the report records `prepare` with status `not_run` and the reason ("no changed files", "automated execution was explicitly disabled"), and no Docker call is made.
- First: before the initial checks, coverage, every v0.4 stage and the reviewer.
- `lint` never prepares and never calls Docker. It still records the `prepare_input_changed` signals described below.

## Configuration

```json
"prepare": {
  "command": ["go", "mod", "download"],
  "inputs": ["go.mod", "go.sum"],
  "network": true
}
```

| Field | Rules | Default |
| --- | --- | --- |
| `command` | Required argv, 1 to 128 arguments, each at most 16 KiB, no NUL, a non-blank first argument. The placeholders `{file}`, `{package}`, `{coverage_out}` and `{results_out}` are refused: nothing is substituted. It is not a command key, so the harness can never run it on candidate code. | — |
| `inputs` | Required, 1 to 64 repository-relative [`path.Match`](https://pkg.go.dev/path#Match) patterns. `*` never crosses `/`, and a pattern without metacharacters is an exact path. Refused: an empty pattern, over 512 bytes, NUL, a backslash, a leading `/` or `./`, an empty, `.` or `..` segment, `**`, and a `.git` segment. | — |
| `network` | Whether the build container needs the network. It gets it only when `--allow-prepare-network` is also passed and `--no-network` is not. | `false` |
| `user` | `"sandbox"` (UID/GID 65534, the identity of checks) or `"root"`. | `"sandbox"` |
| `timeout_seconds` | 1 to 3600. | 600 (`0`) |
| `env` | At most 32 variables for the build container, baked into the derived image so that checks find what was installed outside their mounts. Names match `^[A-Z_][A-Z0-9_]{0,63}$`; values are at most 4096 bytes on one line. Refused names: `HOME`, `TMPDIR`, `GOCACHE`, `GOTOOLCHAIN`, `GOPROXY`, `GOSUMDB` and any `SWIFTPROOF_` name (checks set or override them), and `GOFLAGS`, `GOENV`, `GOWORK`, `NODE_OPTIONS` and `LD_PRELOAD` (they would change what every check executes beyond its reviewed command). | none |
| `max_added_mb` | 1 to 65536: how much the derived image may add to the base image. | 4096 (`0`) |

Unknown fields and duplicate keys are rejected, and every policy error exits 3 before any container starts. `swiftproof init` never writes `prepare`.

`prepare` is **release-ordered**: every binary before v0.4.0 rejects a policy that contains it (exit 3), and `--allow-prepare-network` makes an older binary exit 3 at flag parsing. Publish and re-pin first; see [release ordering](CI.md#release-ordering-for-v04).

Never put a credential in `command` or `env`. The command is redacted in the report, but both are stored in the derived image's configuration.

## What SwiftProof does

1. **Base image.** It inspects `sandbox.image` locally and records its ID. It never pulls: an image that is not present fails the stage.
2. **Inputs.** It exports the files matching `inputs` from the review's base commit, `change.BaseCommit`: the commit `--base` resolves to, or its merge base with the head. That is the tree the baseline snapshot uses, always reachable from the trusted base ref. The head commit is never exported. The files are exact Git blobs (no checkout filter, end-of-line conversion or export attribute), written to a fresh temporary directory with modes 0644/0755 and directories 0755. A matched symlink or submodule, a matched credential-bearing name (such as `.npmrc`, `.env*`, `*.key` or `credentials.json`), more than 256 files, a file over 32 MiB, over 128 MiB in total, or no matching file at all fails the stage.
3. **Key.** The key is the SHA-256 of a canonical JSON of: the schema `swiftproof-prepare/v1` and the SwiftProof version; the base image ID; the argv, the user, the `network` setting of the build and the sorted `env`; the sorted path and SHA-256 of every exported file; the SHA-256 of the container arguments (limits, mounts, user, network, script); and `max_added_mb`. Input patterns, the timeout, the file modes of the exported inputs and the commit are not part of it; reuse also requires the same base commit (step 4).
4. **Reuse.** It looks for the image tagged `swiftproof-prepared:<first 32 hex of the key>-<first 32 hex of the base commit>` that carries the key label. It reuses it only when exactly one such image exists and its labels equal the full key, the base commit and the base image ID; its layers are the base image's layers plus exactly one; its environment holds the policy `env`; and its added size is within `max_added_mb`. Anything else is a miss. Reuse runs no container, so it needs no network.
5. **Permission.** On a miss, when `network` is `true` but the network was not permitted, the status is `not_permitted`: no container runs, and SwiftProof never tries an offline build instead.
6. **Build.** It creates one container (`docker create`) and checks that the container's only mount is the read-only inputs mount: a `VOLUME` declared by the sandbox image adds a volume whose content `docker commit` does not keep, so such an image fails the stage before the command starts. It copies in two empty directories owned by the container user, `/swiftproof/work` and `/swiftproof/home`, and starts the container. A wrapper copies the read-only inputs from `/swiftproof/inputs` into `/swiftproof/work` and replaces itself with the policy's command, which runs there with `HOME=/swiftproof/home`. After a zero exit, it reads the container's whole list of changes (`docker diff`, whatever its length), commits the container with its labels, reads the committed image back, checks it as for reuse, and removes the container with its anonymous volumes. Anything that goes wrong fails the stage, and an image it committed but will not use is removed. One exception: when the overall `--deadline` ends the wait for `docker commit`, Docker may still finish that commit; the tagged image then stays, and a later review reuses it only after the checks of step 4.
7. **Checks.** Every sandbox run of the review (initial checks, coverage, every v0.4 stage and the reviewer's experiments) uses the derived image by its `sha256:` ID, under the unchanged check profile: read-only root, no network unless the policy and `--allow-network` allow it, non-root, tmpfs `/workspace` and `/tmp`, `HOME=/tmp`.

### The build container

The prepare container is the only container with a writable root filesystem: its changes are the product (refinement R2). Its profile, argument for argument:

- `docker create --pull=never --name swiftproof-prepare-<random>`;
- `--network none`, or `bridge` when the build needs and was permitted the network;
- `--cap-drop=ALL`; with `user: root` only, `--cap-add` `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETGID` and `SETUID`;
- `--security-opt=no-new-privileges`;
- `--memory` and `--memory-swap` equal to `sandbox.memory_mb`, `--cpus` equal to `sandbox.cpus`, `--pids-limit=256`, `--ulimit=nofile=1024:1024`, `--log-driver=none`, `--no-healthcheck`;
- `--user=65534:65534`, or `--user=0:0` for `user: root`;
- one read-only bind mount of the exported inputs at `/swiftproof/inputs`, and no other mount: no Docker socket, no host environment, no writable host path (the created container's mounts are checked, see step 6);
- `--workdir=/swiftproof/work`, `--env=HOME=/swiftproof/home`, then exactly the policy `env`;
- `--entrypoint=/bin/sh` and the base image **ID**, never the policy reference, so that a tag moved during the review cannot swap the image.

Its combined output is bounded by `sandbox.max_output_bytes`, redacted, and retained as a hashed `prepare_output` artifact.

## Where dependencies must go

Checks mount a fresh tmpfs over `/workspace` and `/tmp` and set `HOME=/tmp`. They therefore do not see what the command leaves under `/workspace` or `/tmp`, and they do not use its `HOME` (`/swiftproof/home`) as theirs. Install into a location that stays in the image and that checks read:

- `/swiftproof/work`, owned by the prepare user; point checks at it through `env`, for example `"PYTHONPATH": "/swiftproof/work/site-packages"` (an illustration only: no Python layout was exercised);
- a directory of the base image the prepare user can write, such as the Go module cache `/go/pkg/mod` of the official `golang` images (their `/go` is world-writable);
- with `user: root`, any image path, such as `/opt/...`.

**Nothing the command writes is discarded.** The prepare container has no tmpfs, so everything the command and its lifecycle scripts write, including `/tmp` and `/swiftproof/home` (package-manager caches, configuration files, anything fetched with network), is committed into the derived image. It counts toward `max_added_mb`, and anyone who can read the image can read it. `/swiftproof/home` also stays readable by checks with `user: sandbox`: it belongs to UID 65534, the identity that checks, and the code under review, run as. Make sure the command writes no token or credential anywhere in the container, and prefer commands or flags that keep no cache, or that clean their caches before the command exits.

When the command exits 0 but changed nothing outside the directories SwiftProof creates, the stage fails. When every change is under `/workspace`, `/tmp` or `/swiftproof/home`, the image is built but the report adds the Unverified entry "Dependency preparation: prepared outputs are shadowed by check mounts. …", which requests human review under `--ci`, and a reuse of that image repeats it. SwiftProof classifies the whole `docker diff` listing, whatever its length; a path too long to read in full (over 64 KiB) counts as a change that checks can see, never as a shadowed one.

JavaScript projects whose tooling requires `node_modules` inside the workspace, which ES module resolution and most bundlers do, are **not supported** by `prepare` in v0.4: use a preloaded image that contains the dependencies. Only the Go module recipe below was exercised end to end. Other layouts that read dependencies from an image path, such as a Python directory on `PYTHONPATH`, follow the same rules but were not exercised.

### Go module recipe

This recipe is the one exercised by the end-to-end runs recorded in [validation](VALIDATION.md):

```json
"sandbox": { "image": "golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d" },
"prepare": { "command": ["go", "mod", "download"], "inputs": ["go.mod", "go.sum"], "network": true }
```

`go mod download` fills `/go/pkg/mod` as the sandbox user, and the checks, which run with `GOPROXY=off`, find the modules there. A `go.work` layout declares every `go.mod`, `go.sum`, `go.work` and `go.work.sum` it needs as inputs.

## Statuses, report and exit codes

| `prepare.status` | Meaning | Exit code |
| --- | --- | --- |
| `built` | The command ran in this review and its container was committed as the image every sandbox run used. | unchanged (0/1/2) |
| `reused` | An existing image for this key and base commit was used; the command did not run in this review. | unchanged |
| `not_run` | The review executed nothing, so nothing was prepared. | unchanged |
| `failed` | No image was used for checks: the base image is not local or declares a volume, an input could not be exported, the command failed or timed out, the overall `--deadline` was reached, the review was interrupted, nothing changed, Docker failed, or the image was over `max_added_mb`. An image committed before the failure is removed, except in the `--deadline` case of step 6. No check ran. | **4**, with or without `--ci` |
| `not_permitted` | A build needed the network and it was not permitted. No container ran and no check ran. | **4** |

The report records:

- the `prepare` object: `status`, `reason`, `source_commit` (the base commit), the redacted `command`, `user`, `network` (the network of the container that built the recorded image, for `reused` a build of an earlier review, or the setting this run would have used; it is never the network of checks, and the Markdown section says which it is), `key`, `base_image`, `base_image_id`, `image_id`, `added_bytes` (the size Docker reports for the committed layer), `inputs` (path, SHA-256 and size of each exported file), `log_sha256`, `duration_ms` and a fixed `note`;
- a `## Dependency Preparation` Markdown section before `## Automated Checks`;
- the `prepare_output` artifact (built or failed builds only);
- one audit event `stage:prepare` with status `OK`, `ERROR`, `TIMEOUT` or `SKIPPED`;
- one medium `prepare_input_changed` signal per changed file whose new or old path matches an input pattern, in lint and review, anchored at the file's first changed line.

When a review with a built or reused image changes a declared input, the Unverified list gains "Candidate changes dependency-preparation inputs (…); sandbox checks used dependencies prepared from the base commit's versions of the declared inputs only. Candidate dependency changes were not installed, so checks may fail or behave differently for that reason alone; SwiftProof attributes no check result to it.", which requests human review under `--ci`. **Candidate dependency changes are never installed.** The entry neither explains nor discounts any check result: checks keep their recorded statuses. A failed or not permitted preparation adds an Unverified entry that says whether the base-branch prepare command was started; no candidate code ever runs in the prepare stage.

## Network and egress

The build container gets the network only when the policy says `"network": true` **and** the operator passes `--allow-prepare-network`, and never with `--no-network`. That permission is separate from `--allow-network`, which governs candidate check containers only; network in the prepare container never gives checks network.

A network-enabled prepare container uses Docker's default bridge. It can reach anything the Docker host can reach, including cloud metadata endpoints such as `169.254.169.254` and the local network of a self-hosted runner. SwiftProof does not restrict egress. Operators must restrict it at the Docker network or host firewall level, and should prefer hash-pinned lockfiles and install flags that skip lifecycle scripts (for example `npm ci --ignore-scripts`). Private registries that need credentials are not supported: no credential is ever passed in.

## Image lifecycle

- Each build tags its image `swiftproof-prepared:<key>-<commit>` (both prefixes) and labels it with `org.swiftproof.prepare.schema`, `.key`, `.source_commit`, `.base_image_id`, `.tool_version`, `.outputs` and `.log_sha256`. The labels are unsigned local metadata: whoever can write to the Docker daemon can forge them, as they can replace any image.
- Reuse requires the same base commit, so every new base commit, for example each merge to the base branch, builds again, and a different SwiftProof version or base image does too. Concurrent builds of one key and commit both commit; the last tag wins, and each review uses the image it committed.
- Derived images accumulate. List and remove them with:
  ```sh
  docker image ls --filter label=org.swiftproof.prepare.schema
  docker image prune -a --filter label=org.swiftproof.prepare.schema
  ```
- The derived image holds everything the command wrote, including `/tmp` and `/swiftproof/home` (see [where dependencies must go](#where-dependencies-must-go)).
- The derived image keeps the prepare container's configuration: entrypoint `/bin/sh`, the prepare command as its command, the prepare user, `/swiftproof/work` as working directory, `HOME=/swiftproof/home` and the policy `env`. Checks override the entrypoint, user, working directory and `HOME`.
- Docker Desktop copies its bind-mount labels (`desktop.docker.io/mounts/…`) into committed images, including the host path of the temporary inputs directory. SwiftProof ignores them.

## Budget

Preparation runs before the harness exists and is **not charged** to `sandbox.max_runtime_seconds` (refinement R10). It is bounded by `prepare.timeout_seconds` and by `--deadline`. A preparation cut short by either fails, and exits 4; the reason names the deadline only when the overall `--deadline` ended it, and any other interruption (such as Ctrl-C) is reported as an interruption. Everything up to the read-back of a committed image stops at the deadline. Cleanup runs past it on its own time limits: at most 10 s to remove the prepare container, and at most 10 s to remove an image it committed but will not use. The worst-case wall-clock formula is in [CI integration](CI.md#runtime-bounds-deadline-and-report-size); count up to 20 s of cleanup for an in-flight preparation where it counts 5 s per in-flight run.

## What this does not claim

- A prepared image is not safe, unmodified, license-clean or free of vulnerabilities, and SwiftProof makes no such claim about its dependencies.
- A prepared image is not reproducible: an equal key means equal inputs, not equal image content. Installing the same inputs again may resolve differently. `image_id` identifies the image that was used.
- Candidate dependencies were not installed and were not exercised.
- Image labels are not an attestation.
- The prepare command is arbitrary trusted argv; nothing establishes that it only installed dependencies.
- Network in the prepare container is not network for checks.

## Limitations

- Ephemeral CI runners start without derived images and build on every job; nothing is shared across runners or through a registry in v0.4.
- Docker's reported size of the committed layer is the only size measure; `added_bytes` is that figure.
- The prepare container has no egress allowlist.
- A sandbox image that declares a `VOLUME` cannot be used with `prepare`: the stage fails before the command starts.
- Nothing the command writes is discarded before the commit, including `/tmp` and `/swiftproof/home`.
- Only the Docker CLI is supported; no claim is made for Podman.
