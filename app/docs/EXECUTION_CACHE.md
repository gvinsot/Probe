# Execution cache

The execution cache lets `swiftproof review` replay the recorded result of a baseline-side sandbox run instead of executing it again, when the same inputs were already run live and two live runs agreed. It is **opt-in**: nothing is cached, read or written unless `--cache-dir DIR` is passed. Candidate-side runs are never cached; they always execute.

A replay is not a fresh execution. It never supports a reproduced issue, a divergence or a `FAILS_ON_CANDIDATE` result, and the report marks every replayed check. The binding rules are §3.3, §3.4 and §F7 of the [v0.4 specification](../../specs/swiftproof-v0.4-spec.md).

## Enabling it

```sh
swiftproof review --base main --cache-dir "$HOME/.cache/swiftproof-exec"
```

`--cache-dir` applies to `review` only; on `lint` it exits 3. The directory is checked before dependency preparation and before any container starts, and every violation exits 3:

- After the symlinks of its nearest existing ancestor are resolved, the directory must not be the repository root or the report directory (`--out`), must not be inside either, and must not contain either. A checkout or a report could otherwise plant entries.
- The directory itself must not be a symlink, a junction or another reparse point, and must be a directory. A missing directory is created with mode 0700.
- On Unix, it must be owned by the effective user and grant no group or other permission (`chmod 700`). **On Windows, ownership and ACLs are not checked**: use a directory that only the account running SwiftProof can write, such as one under `$env:LOCALAPPDATA`.

Problems found later never change the exit code; they disable the cache for the run with a recorded reason (`execution.cache.status: disabled`): the sandbox image cannot be resolved to a local image ID, sandbox networking is enabled for the run, the running executable cannot be hashed, or the directory stops being usable (three consecutive I/O failures).

Several reviews may share one directory, including at the same time: entries are published by renaming complete temporary files, and a conflict counts as a write failure.

## What can be replayed

A run is **eligible** only when all of these hold:

- a cache is enabled, and the sandbox image was resolved to its image ID (`sha256:…`);
- its check kind ends in `_base` (`generated_test_base`, `fuzz_base`, `base_test_base`, `impacted_test_base`), and it runs on the baseline snapshot;
- sandbox networking is off;
- every file and directory of the baseline snapshot is unchanged. Files a stage added for the run (a generated test, a harness file) are hashed into the key; a modified or removed baseline file makes the run uncacheable.

The re-run kinds `generated_test_base_repeat`, `fuzz_base_confirm` and `fuzz_candidate_confirm`, and every candidate-side or hybrid kind, are never cached.

A live result is **stored** only when it completed: status PASS or FAIL, exit code 0 to 124, no timeout, no Docker error, and its log was retained. A payload (a Jest-compatible report or a fuzz observation stream) must be complete and unchanged by redaction. Nothing else is written, so a transient infrastructure failure is never kept.

An entry is **served** only when it is not a live-only run, passes every integrity check (below), was recorded by **at least two live runs that agreed** on status, exit code and truncation, was never contradicted, and its recorded duration is below the run's current per-run timeout.

## The key

The key is the SHA-256 of a canonical JSON preimage (schema `swiftproof-execcache/v1`). The preimage holds hashes and fixed identifiers only, never the raw command or output:

| Field | What it commits to |
| --- | --- |
| `tool_version` | The version string and the SHA-256 of the running executable: another build never reads these entries. |
| `kind` | The check kind. |
| `base_commit` | The base commit of the change. |
| `tree` | The digest of the baseline snapshot's manifest (path, type, size, SHA-256 of every file and directory), and each added entry as `[path, type, size, sha256]` (type `f`, `x` for an executable file, `d` for a directory). |
| `policy_sha256` | The execution settings of the trusted policy: every command, the configured image reference, network, per-run timeout, runtime budget, output, memory and CPU limits, generated-test budget. |
| `image_id` | The image ID the run executes (see below). |
| `docker_server` | The Docker server version, OS type and architecture. |
| `docker_args_sha256` | The complete `docker run` argument vector, built with a fixed container name and mount path: isolation flags, limits, user, mounts, environment, wrapper or capture script and command. |
| `argv_sha256` | The command. |
| `capture` | The in-container payload path, if any. |
| `timeout_ms`, `max_output_bytes` | The per-run timeout after stage tightening (not the budget-clamped value, which changes from run to run) and the output limit. |

`docker_server` is recorded in addition to the fields the integration contract lists, so that a Docker engine upgrade starts new entries. The kernel version and runtime internals beyond the server version are **not** part of the key.

**Image pinning.** With a cache, SwiftProof resolves the policy's image reference once, when the review's sandbox is set up (`docker image inspect`, `docker info`; it never pulls). From the first eligible baseline run on, every run of the review, baseline and candidate, executes that image ID rather than the tag, so a tag re-pointed during the review cannot change what a key describes. In this build, runs that happen before the first eligible run (the initial checks, coverage) still use the configured reference. With `prepare`, the image is already an image ID.

## What a replay records

A replayed check keeps its own check ID in the current run and records:

- the stored status, exit code, truncation flag and log, with `duration_ms: 0`; the log is saved again as the check's artifact, and a payload is normalized and saved again by the stage that asked for it;
- `cache: {status: "hit", key, recorded_at, recorded_run, recorded_check, recorded_duration_ms, live_runs}`;
- one audit event `stage:execution_cache` with status `HIT`.

A replay charges nothing to the runtime budget. A live eligible run that was written through carries `cache.status: "stored"` and its agreement count; it is a live run, and every verifier accepts it as one.

## The live-baseline rule

A positive status never rests on a replay:

- **Generated tests.** When a replayed baseline PASS and a live candidate FAIL would record `REPRODUCED`, the harness runs the baseline again, live, with the same kind, staged file and command. The evidence then cites the live check as `base_check_id`, and its description says that the baseline was replayed and run again. If the live run passes, the result is `REPRODUCED`; if it completes without passing (including a named-test validation that the replay passed and the live run fails), the result is `UNVERIFIED` and the entry is removed as contradicted; if it does not complete (SKIPPED, TIMEOUT, ERROR), the result is `UNVERIFIED` and the entry stays.
- `report.Finalize` independently refuses `REPRODUCED` when the cited baseline is a replay, including when `swiftproof report` re-renders a saved report. The observation repeat, the fuzz confirmation pair and the live re-runs of changed or impacted tests follow the same rule (§3.4 of the specification).

A **negative** status (`NOT_REPRODUCED`, `NOT_DIVERGED`, `PASSES_ON_CANDIDATE`) may rest on a replayed baseline, because a served entry needs two agreeing live runs. `Finalize` lists every such evidence ID, sorted, in `execution.replay_backed`, and the Markdown names them. Such a conclusion does not by itself request human review (refinement R3 of the specification).

**Agreement and contradiction.** Every live eligible run is written through: without an entry it starts one with `live_runs: 1`; with an agreeing entry it adds one and keeps the latest log; with a disagreeing entry it removes the entry and counts it as contradicted and evicted. Live runs are never served from the cache.

## Report fields

- `checks[].cache` on stored and replayed baseline-side checks only (the schema refuses it on any kind that does not end in `_base`).
- `execution`, present whenever a sandbox harness was created:
  - `cache`: `status` (`enabled` or `disabled`), `reason` when disabled, `scope` (`baseline_only`), `image_id`, `policy_sha256`, `runtime` (the Docker server identity), the counters `hits`, `stored`, `misses`, `uncacheable`, `rejected`, `write_failures`, `evicted`, `contradicted`, and a fixed `note`;
  - `parallelism`: requested and effective concurrency of the initial checks;
  - `budget`: `max_runtime_ms`, `spent_ms`, `reviewer_reserve_ms`, `deadline_reached`;
  - `replay_backed`: evidence IDs, always an array.
- The Markdown notes each stored or replayed check under **Automated Checks** and ends that section with the execution summary and the replay-backed evidence IDs.
- Standard output gets one line when the cache was requested, for example `Execution cache: 1 baseline results replayed (not executed in this run), 0 recorded; candidate-side runs always execute.`

A review without `--cache-dir` records `execution.cache.status: disabled` with the reason `not requested (the execution cache is opt-in with --cache-dir)` and prints no cache line.

## Storage

- One file per key: `DIR/v1/<first two hex digits>/<key>.json`, mode 0600, in directories of mode 0700. A file holds the entry (status, exit code, log, truncation, duration, payload, live-run count, recording time, run and check) and a `content_sha256` of it. The log and the payload are stored as exact bytes (base64).
- **No raw output is stored.** The log is the recorded, redacted and bounded log of the check; a payload is kept only when it is complete and unchanged by redaction.
- **Checks on every read.** The content hash, the key recomputed from the stored preimage, the key named by the file, strict decoding without unknown fields, the status and exit code, the sizes (16 MiB per file), the recording time (no later than five minutes ahead) and the provenance fields must all hold. A file that fails is removed and counted as `rejected`. An entry older than 30 days is removed and counted as `evicted`.
- **Trimming** when a review opens the directory: temporary files older than an hour and entries older than 30 days are removed, and when more than 20,000 entries or 512 MiB remain, the oldest are removed until nine tenths of either limit remain. More than 40,000 files remove the whole layout. Removed entries count as `evicted`.
- One review writes at most 256 entries and 256 MiB; further writes count as write failures.
- To clear the cache, delete the directory.

## Incremental re-review

When a pull request is updated, a review that shares the `--cache-dir` of earlier reviews replays each baseline experiment whose key did not change: same base commit, same experiment content (for example the same generated test), same policy, image, Docker server and SwiftProof build. The candidate side always runs again. `--previous-report` and `--since` are not provided: a saved report is not authenticated and does not hold the inputs a key needs.

The payoff depends on how often baseline experiments repeat. Model-written generated tests rarely repeat across reviews; deterministic stages (changed baseline tests, impacted tests, differential fuzzing) repeat more often when the base commit is unchanged. Measure hit rates on your own reviews before expecting shorter reviews ([measured costs](PERFORMANCE.md)).

## Trust and privacy

- The cache directory is a **trusted input**, at the level of the SwiftProof binary and the report directory. The integrity checks detect corruption, torn writes and misplaced files; they do **not** authenticate an entry. Anyone who can write the directory can forge entries.
- **Damage bound.** A forged or stale entry cannot produce `REPRODUCED`, a divergence, a `FAILS_ON_CANDIDATE` result or exit 1: the harness re-runs the baseline live, and `Finalize` refuses a replayed baseline for every positive status. Its largest effect is a negative conclusion (`NOT_REPRODUCED`, `NOT_DIVERGED`, `PASSES_ON_CANDIDATE`) where a live baseline would have given `UNVERIFIED`, and such conclusions are listed in `execution.replay_backed`.
- Containers never see the cache directory: the sandbox keeps its one read-only mount of the snapshot, and no volume or writable host path is added.
- A nondeterministic baseline can be replayed for up to 30 days. `recorded_at`, `recorded_run` and `recorded_check` tell a reviewer where a result came from; a run without `--cache-dir` executes everything.
- Entries hold only redacted recorded logs, but they are logs of baseline code: keep the directory private, and never upload it or share it across trust boundaries. SwiftProof never uploads it.

## Exit codes

| Situation | Without `--ci` | With `--ci` |
| --- | --- | --- |
| Cache hit, miss, rejection, contradiction, write failure or runtime disable | 0 | 0 (no effect of its own) |
| Invalid `--cache-dir` (syntax, location, ownership, link) | 3 | 3 |

A replay never produces exit 1 by itself, and a replay-backed negative conclusion requests no review by itself.

## What the cache does not claim

- A replay is not a fresh execution, a re-verification or a re-test, and it is not more reliable than a live run.
- The integrity hash is not authenticity.
- The key does not capture kernel or runtime internals beyond the recorded Docker server identity.
- A replayed baseline says nothing about the candidate.
- No speed-up is claimed without a measurement of the workload and environment.
