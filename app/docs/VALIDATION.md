# Implementation validation

Validated locally on 2026-09-19. These checks establish tested implementation behavior, not a measurement of human review-time savings.

- Full Go tests and `go vet` pass on Windows amd64 with Go 1.27.1.
- Full tests with `-race`, and `go vet`, pass in Linux with Go 1.26 and networking disabled.
- Cross-compilation succeeds for Windows, Linux and macOS on amd64/arm64, with CGO disabled and version metadata injected. Cross-compilation does not substitute for native execution on those platforms.
- Real Docker tests use the official `golang:1.26-bookworm` image (observed digest `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`).
- Container tests check absent host secrets, excluded credential files, blocked network, non-root identity, read-only source/root, ephemeral writes and unchanged host input.
- Real generated Go experiments verify both a baseline-pass/candidate-fail regression and a passing generated test despite an unrelated failing existing candidate test.
- A complete CLI test uses real commits, a scripted local provider and real containers. It asserts the reproduced-issue exit code, named-test evidence, retained source artifacts, merged audit history and unchanged checkout.
- Provider tests cover structured tool calling, input/iteration/time limits, unsupported tools, malformed responses and redirect refusal. No live paid/cloud model was required for these tests.
- The public JSON schema is checked as Draft 2020-12. Reports, output escaping, credential masking, evidence references and focused-line calculations have targeted tests.

Reproduce ordinary checks:

```sh
go test ./...
go vet ./...
go test -race ./...
```

For actual container tests, preload a trusted Go image and set `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, then run:

```sh
go test ./internal/harness ./internal/cli -run Docker -count=1 -v
```

Tests without that environment variable intentionally skip real Docker integration. The included CI workflow enables it on Linux.

The published v0.1.0 binaries are used by the new PR and deployment adapters.
The reusable PR workflow and pilot pass `actionlint`. The companion PulsarCD
implementation passes 528 targeted tests (API, security, build scripts and
SwiftProof), including an actual SwiftProof CLI invocation through its temporary
provider bridge with a simulated local provider. Tests cover changed image
digests, missing/corrupt evidence, human approval, absent LLM gates and separate
QA/production baselines. These integration changes have been validated locally;
their GitHub workflows and deployment gate have not been activated on the live
cluster by this task.

## v0.4 validation

Each block below records runs actually performed for one part of v0.4: the date, the environment, the commands and their exit codes.

<!-- F0:begin -->
### F0 foundation

**2026-09-25, harness foundation (F0c).** Environment: Windows 11 Pro host with Docker; Go in `golang:1.26-bookworm` (image `sha256:a688600ca24f…`, Go 1.26) and `golang:1.23-bookworm` (Go 1.23.12). Scope: the run primitives (`run.go`: budget reservation, per-run timeout and ceiling, capped tee, mutation ledger, execution-cache consult and store), `stage.go`, `existingtests.go`, the harness stubs, `internal/dockerutil` and `gitrepo.Tree`/`ReadBlobs` with `Snapshot` rebuilt on them.

- `go vet ./...` and `go test ./...` pass in `golang:1.26-bookworm`. In `golang:1.23-bookworm`, `go vet ./...` and the `internal/harness`, `internal/gitrepo` and `internal/dockerutil` tests pass.
- `go test -race` passes for `internal/harness`, `internal/gitrepo` and `internal/dockerutil`.
- Windows amd64 test binaries run on the host with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm` and `-test.run Docker` pass: the eight harness Docker tests (including `TestDockerRunOptionsTimeoutTeeLedgerAndCache` and `TestDockerExistingTestOutcomesFromRealGoTest`), `TestDockerInspectAndInfo` (dockerutil) and `TestDockerReviewEndToEnd` (cli). The full harness, dockerutil and gitrepo suites also pass natively on Windows.
- End-to-end runs with a Windows binary built from the branch (`-X main.version=F0c-e2e`) on a two-commit Go fixture repository, with the policy passed through `--config` from outside the fixture:
  - `review` with checks and no reviewer: test, typecheck, build and coverage checks PASS; exit 0, and 0 with `--ci`.
  - `review --checks=false --ci` with a scripted local provider (`create_test`, `run_generated_test`, `find_callers`, `submit_hypothesis`): one `differential_test` evidence record REPRODUCED from `check-1` (baseline PASS) and `check-2` (candidate FAIL), one high reproduced issue, exit 1. `find_callers` answered through the lexical fallback. The checkout stayed clean and no `swiftproof-` container remained.
  - `report` re-render of that JSON: Markdown and JSON byte-identical to the originals.

The execution-cache paths were exercised only with an in-memory test store; the on-disk cache behind `--cache-dir` is not part of this foundation.

**2026-09-25, documentation skeletons (F0e).** Windows 11 Pro amd64 host, Docker Engine 28.4.0, `golang:1.26-bookworm` (go1.26.8, digest `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`). This change edits documentation only.

- `go vet ./...` and `go test -count=1 ./...` in that image: both exit 0.
- A Windows amd64 binary built from the branch (`CGO_ENABLED=0`) ran `lint` on a scratch Git fixture: exit 0, both report files written, checkout unchanged. An intent of `<<<<` is stored in the JSON as `\u003c\u003c\u003c\u003c`, six bytes per character, which the report-size note in [CI integration](CI.md#runtime-bounds-deadline-and-report-size) relies on.
- `swiftproof report --input` with a 67,108,865-byte report (64 MiB + 1 byte): exit 3, "exceeds 67108864 bytes". The same report at 67,107,840 bytes re-rendered with exit 0.

The three entries below record the runs that the F0a, F0b and F0d implementers reported for their sub-branches before the foundation was merged; they were not repeated for this page.

**2026-09-25, model, schema and redaction (F0a), as reported by its implementer.** Windows 11 Pro host with Docker. `go vet ./...` and `go test ./...` passed in `golang:1.26-bookworm` and `golang:1.23-bookworm`. `FuzzRedactIdempotent` ran for 180 s without a failure. The implementer reported a Python `jsonschema` (Draft 2020-12) cross-check of the 93 valid and invalid report fixtures dumped from the schema tests; its output was not retained. The schema's `differential_fuzz` runner rule changed afterwards (review fixes below), and that cross-check was not repeated. A Windows binary built from the branch ran `lint`, `review` and `report` re-renders on a scratch fixture, and the reports validated against the schema.

**2026-09-25, report foundation (F0b), as reported by its implementer.** Windows 11 Pro host, `golang:1.26-bookworm`. `go vet ./...` and `go test ./...` exit 0; the report Windows test binary passes; the cli `TestDockerReviewEndToEnd` passes. A Windows binary built from the branch ran `lint --ci` (exit 2, no checks) and `review` (four PASS checks, exit 0), each re-rendered byte-identically. Hand-built reports through `swiftproof report`: a live confirmation gives REPRODUCED and exit 1; a replayed baseline gives UNVERIFIED and exit 2; `NOT_REPRODUCED` needs `live_runs` of at least 2; a DIVERGED claim with the stub verifiers stays UNVERIFIED; forged mutants become INCONCLUSIVE with status `incomplete`; fuzz `not_run` gives exit 2.

**2026-09-25, cli, config, reviewer and linter foundation (F0d), as reported by its implementer.** Windows 11 Pro amd64 host, Docker Engine 28.4.0, `golang:1.26-bookworm`.

- `go vet ./...` and `go test -count=1 ./...` in that image: exit 0. `go test -race` of `cli`, `config`, `linter` and `reviewer`: exit 0. `go vet ./...` in `golang:1.23-bookworm` (go1.23.12): exit 0.
- `cli` Windows test binary with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck` pass; the whole `cli` suite passes natively on Windows.
- A Windows binary built from the branch, on a scratch Go fixture (a behavior-preserving change with a passing test):
  - `lint --ci` with a policy containing `fuzz` and `mutation`: exit 2 (changes without checks); no `prepare`, `fuzz`, `mutation`, `base_tests` or `execution` object; `impact` present with status `unavailable`; `intent_criteria`, `divergences` and `intent_test_failures` are `[]`. With `--impact=false` the `impact` object is absent.
  - Each of `--base-tests`, `--fuzz=false`, `--impacted-tests`, `--cache-dir`, `--parallel`, `--allow-prepare-network` and `--deadline` on `lint`: exit 3 ("lint does not execute sandbox checks; --X applies to review only"). `review --parallel 0`, `--deadline 30s`, `--base-tests --checks=false`, `--impacted-tests --impact=false`, `--report-url https://…` and `--cache-dir DIR`: exit 3, no report directory written, no container started.
  - `review --parallel 2 --deadline 15m --ci`: exit 0; test, typecheck, build and coverage PASS under one progress line.
  - The same review with `fuzz` and `mutation` in policy plus `--base-tests --impacted-tests --ci`: exit 2; the four checks PASS; `fuzz`, `mutation`, `base_tests` and `impact.tests_status` are `not_run` ("not implemented in this build").
  - A policy with `prepare`: exit 4, no check ran, `prepare.status` `failed`, and every requested stage `not_run` with "dependency preparation did not produce an image".
  - A test command `sh -c "sleep 120"` with `--deadline 1m --ci`: exit 2 after 31 s; test TIMEOUT, typecheck and build SKIPPED; the deadline Unverified entry is recorded.
  - A scripted loopback provider calling `stage:run_fuzz`, `find_callers`, a DIVERGED claim citing a missing evidence ID and a claim with `criterion_id`: exit 2; the forged call is audited as `rejected_tool_call`; the DIVERGED claim finalizes UNVERIFIED; the criterion link is refused.
  - `swiftproof report` re-renders of these reports are byte-identical. No `swiftproof-` container remained and the fixture checkout stayed clean.
- The published v0.2.0 binary exits 3 on a policy containing `fuzz` ("unknown field") and on `--parallel`.

**2026-09-26, foundation review fixes (merged F0).** Windows 11 Pro host with Docker; `golang:1.26-bookworm` (digest `sha256:a688600ca24f…`) and `golang:1.23-bookworm`. Scope: the merged foundation after the review fixes (Finalize order, divergence selection and texts, Recorded Evidence marker, the intent-link normalization, candidate-side log text and ERROR, the results budget split, deadline texts, streaming snapshots, policy validation, the TS/JS fuzz runner rule, stub reasons and stdout sanitizing).

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race` of `internal/harness`, `internal/cli`, `internal/report` and `internal/gitrepo` exit 0. From `hub/`: `go vet ./...` and `go test ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `harness`, `gitrepo`, `dockerutil`, `config`, `report`, `cli`, `model` and `redact` tests exit 0.
- Windows amd64 test binaries with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: the full `harness` (all eight Docker tests), `gitrepo`, `dockerutil`, `report` and `cli` suites pass (`TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks`, `TestDockerDeadlineStopsRunningCheck`). No `swiftproof-` container created by these runs remained.
- A Windows binary built from the branch (`-X main.version=F0fix2-e2e`), on a two-commit Go fixture, with a policy containing `fuzz` and `mutation`: `review --base-tests --impacted-tests --reviewer=false --ci` exited 2; test, typecheck, build and coverage PASS; the Unverified Areas list one line for each stage that did not run (base tests, fuzzing, impacted tests, mutation); Behavior Divergences reads "No observation or fuzz experiment was recorded."; the checkout stayed clean; `swiftproof report` re-rendered both files byte-identically.
- Hand-built reports through `swiftproof report`: a high REPRODUCED hypothesis carrying `intent_judgment` and an unknown `criterion_id` keeps exit 1, loses both fields in `hypotheses` and in `reproduced_issues` alike, gains one Unverified note, and re-renders byte-identically; a REPRODUCED record resting on a replayed baseline is listed as "REPRODUCED; as stored; not accepted as evidence"; a `NOT_DIVERGED` observation record that no verifier accepted gives the "No validated divergence was recorded. 1 of 1 …" sentence instead of the equal-values sentence.
<!-- F0:end -->

<!-- F1:begin -->
<!-- F1:end -->

<!-- F2:begin -->
<!-- F2:end -->

<!-- F3:begin -->
<!-- F3:end -->

<!-- F4:begin -->
<!-- F4:end -->

<!-- F5:begin -->
<!-- F5:end -->

<!-- F6:begin -->
<!-- F6:end -->

<!-- F7:begin -->
### F7a execution cache

**2026-09-26, opt-in baseline execution cache (F7a).** Windows 11 Pro amd64 host, Docker Engine 28.4.0; `golang:1.26-bookworm` (image `sha256:a688600ca24f…`) and `golang:1.23-bookworm` (go1.23.12).

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race -count=1` of `internal/execcache`, `internal/harness`, `internal/report` and `internal/cli` exit 0. From `hub/`: `go vet ./...` and `go test ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `execcache`, `harness`, `report`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries run natively: the `execcache` suite (including the Windows case-variant and symlink cases) and the `report` suite pass. With `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, the whole `harness` suite passes, including `TestDockerExecutionCachePinsAndReplays` (the image pinned to its ID, `docker run` of that ID, a third identical baseline run replayed), and the whole `cli` suite passes, including `TestDockerExecutionCacheEndToEnd` (five reviews against one cache directory, 174 s).
- End-to-end runs with a Windows binary built from the branch (`-X main.version=F7a-e2e`), on a three-branch Go fixture (`main`, a `candidate` that drops a lower bound, a `benign` branch that only adds a function), with the policy passed through `--config` from outside the fixture, a scripted loopback provider (`create_test`, `run_generated_test`, then `submit_hypothesis` citing the returned evidence with its status) and one new `--cache-dir` outside the fixture and the report directories:
  - `review --base main --head candidate --ci --parallel 3`: exit 1. Test, typecheck, build and coverage PASS with no cache field; `generated_test_base` PASS with `cache.status` `stored`, `live_runs` 1; candidate FAIL; `REPRODUCED`. `execution.cache`: `enabled`, `image_id` `sha256:a688600c…`, `runtime` `28.4.0 linux/x86_64`, `stored` 1, `hits` 0; `parallelism` requested 3, effective 1.
  - The same review with `--checks=false`: exit 1; the baseline run was not served after one live run: `stored`, `live_runs` 2, `hits` 0.
  - Repeated: exit 1. `generated_test_base` replayed (`hit`, `live_runs` 2, 0 ms), candidate FAIL, then `generated_test_base` run again live (`stored`, `live_runs` 3); the evidence cites the live check as `base_check_id` and its description records the confirmation; one `stage:execution_cache` HIT audit event; `replay_backed` empty.
  - `--head benign`: exit 0 with `--ci`. Baseline replayed (`live_runs` 3), candidate PASS, `NOT_REPRODUCED`; `execution.replay_backed` is `["evidence-1"]`, and the Markdown lists it.
  - One byte of the entry file changed (`live_runs` 3 to 9) and the benign review repeated: exit 0; `rejected` 1; the baseline ran live and started a new entry (`live_runs` 1); `replay_backed` empty.
  - `--cache-dir` inside the fixture repository, inside `--out`, and a directory junction: exit 3 before any container, no report directory, nothing created in the repository. `lint --cache-dir` and `lint --parallel 2`: exit 3.
  - `review` without `--cache-dir`: exit 1; `execution.cache.status` `disabled` with "not requested (the execution cache is opt-in with --cache-dir)"; no check has cache provenance; no cache line on stdout; the cache directory's files unchanged.
  - `swiftproof report` re-renders of five of these reports: exit 0, Markdown and JSON byte-identical.
  - The fixture checkout stayed clean, no generated test file leaked into it, and no `swiftproof-` container of these runs remained. The key of the same experiment differed between two builds of the branch, as the executable digest is part of it.
- Not run: the `--parallel` concurrency (F7b's part; this build runs the initial checks one at a time and records effective 1) and a comparison of `--parallel 1` and `--parallel 3` wall clocks.

**2026-09-26, F7a review fixes** (junction ancestors in the directory rule, cache provenance after an eviction, counters of a store disabled during the run, one shared probe limit, the damage-bound and concurrency wording). Same host, Docker Engine 28.4.0, shared with other concurrent builds and reviews (sandbox runs took two to three times longer than on an idle host).

- From `app/` in `golang:1.26-bookworm`: `go vet ./...` and `go test -count=1 ./...` exit 0, and `CGO_ENABLED=1 go test -race -count=1` of `internal/execcache`, `internal/harness`, `internal/report`, `internal/cli` and `internal/model` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm` (go1.23.12): `go vet ./...` and the `execcache`, `harness`, `report`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries run natively:
  - `execcache`: every test passes, including `TestValidateDirResolvesJunctionAncestors` (junctions made with `mklink /J`), the symlink-ancestor test and the 8.3 short-name case of `TestSameOrInside`; the Unix permission test skips by design. The new junction test, built against the previous `dir.go` (commit `8d1417d`), fails with both defects of the review: `j\newcache` and `j\x\y` through a junction into the repository were accepted and created inside it, and `k\cache` below a junction to an external directory failed on its second use ("The system cannot find the path specified.").
  - `report`: passes.
  - With `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: the whole `harness` suite (244 passing tests, none skipped, including `TestDockerExecutionCachePinsAndReplays`) and the whole `cli` suite (including `TestDockerExecutionCacheEndToEnd`, 203 s) pass. These two binaries were built before a last wording change of the Markdown summary of a store disabled during the run; the `report` binary and the Linux runs above cover the final text.
- End-to-end runs with a Windows binary built from the fixes (`-X main.version=F7a-fix2-e2e`), on a clone of the three-branch clamp fixture, the policy through `--config`, a scripted loopback provider, and directory junctions `j` (to the fixture) and `k` (to a directory outside it) made with `mklink /J`:
  - `review --checks=false --reviewer=false --head candidate`: `--cache-dir j\newcache` exits 3 ("must be outside the repository", with the resolved path) before anything is created in the repository; `--cache-dir k\cache` exits 0 on the first and on the second review and prints the resolved directory; `--cache-dir k` (a junction) exits 3; `--repo j --cache-dir <fixture>\cache7` exits 3; `--out k\out` exits 3 in the existing output validation ("output path must contain only directories, not links"). `lint --cache-dir` exits 3.
  - `review --ci --head candidate` with a new cache directory: exit 1, `generated_test_base` stored with `live_runs` 1. Again with `--checks=false`: exit 1, `live_runs` 2, nothing replayed. Again: exit 1; the baseline was replayed (`hit`, `live_runs` 2), then run again live (`stored`, `live_runs` 3), and the evidence cites the live check and records `REPRODUCED`. `--head benign --ci`: exit 0, `NOT_REPRODUCED` on the replay, `replay_backed` `["evidence-1"]`, listed in the Markdown.
  - The damage bound, on copies of that directory with the entry rewritten and only its content hash recomputed: as a FAIL whose log is the candidate's real failing run of the named test, the candidate review exits 2 with `--ci` and 0 without it (evidence and hypothesis `UNVERIFIED`, `reproduced_issues` empty, the baseline replayed and not run again, the Markdown note under the replayed check); as a FAIL that keeps the passing log, the replayed check becomes ERROR and the review exits 4 with and without `--ci`.
  - `swiftproof report` re-renders of five of these reports: exit 0, Markdown and JSON byte-identical. The fixture checkout stayed clean, and no `swiftproof-` container of these runs remained.
- Not run: a store that disables itself during a real review, a live re-run that agrees on status but fails the named-test validation with the real binary (unit tests cover both), concurrent reviews sharing one directory, and a volume mounted without a drive letter on the cache path.

### F7b parallel initial checks

**2026-09-26, bounded parallel initial checks (F7b).** Windows 11 Pro amd64 host; Docker Engine 28.4.0, whose VM reports 2 CPUs and 30,865 MiB; `golang:1.26-bookworm` (image `sha256:a688600ca24f…`) and `golang:1.23-bookworm` (go1.23.12). Other agents' builds and reviews shared the Docker daemon during these runs.

- From `app/` in `golang:1.26-bookworm`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race -count=1` of `internal/harness`, `internal/cli`, `internal/report` and `internal/execcache` exits 0, and the batch, run and cache tests of `internal/harness` pass 20 times in a row under the race detector in a container limited to one CPU (`--cpus=1`, `-count=20`). From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `harness`, `cli`, `model` and `report` tests exit 0. `run_test.go` (F0's guard for `run.go`) is unchanged and passes.
- Windows amd64 test binaries run natively with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: the whole `harness` suite (265 passing tests and subtests, none skipped), including `TestDockerRunChecksRunsInitialChecksConcurrently` (with room for two sandboxes of 1 CPU, the test and typecheck containers overlapped in time, the checks kept configured order with their real statuses, test PASS, typecheck PASS, build FAIL with exit 3, and none of its containers remained), and the whole `cli` suite (129 passing, none skipped), including `TestDockerReviewSkeletonAroundRealChecks` (`--parallel 2`), `TestDockerDeadlineStopsRunningCheck` and `TestDockerExecutionCacheEndToEnd`; the `report` and `model` suites pass.
- End-to-end runs with Windows binaries built from the branch (`-X main.version=F7b-e2e`; commit `b62e179`, and `e22251a`, which changes only the wording of the notes), on a two-branch Go fixture (`main`; `candidate` drops the lower bound of `Clamp`, so test and coverage fail), with policies derived from the example Go policy and passed through `--config` from outside the fixture, `--reviewer=false` unless stated. Each run had its own `TMP`, so its sandboxes could be told apart from other agents' by their mounts.
  - `review --ci --parallel 3` with `sandbox.cpus: 1`: exit 2. `check-1` test FAIL, `check-2` typecheck PASS, `check-3` build PASS, `check-4` coverage FAIL, in that order; test and typecheck started together (their `run_test` and `run_typecheck` audit events carry the same time), build ran next, alone. `execution.parallelism`: requested 3, effective 2, note "The Docker server reports 2 CPUs and 30865 MiB of memory: room for 2 sandboxes of 1 CPU and 1024 MiB at a time." followed by the fixed rules; stdout `Initial checks: up to 2 at a time (requested 3).`; the Markdown ends Automated Checks with the same line and note.
  - The same review with `--parallel 1`: exit 2, the same checks, statuses and exit codes in the same order, effective 1, note "Initial checks run one at a time.", no stdout parallelism line.
  - `--parallel 4` without `--ci`: exit 0; effective 2, the note starts with "Only 3 initial checks were requested."
  - With the default `sandbox.cpus: 2` and `--parallel 3`: exit 2; effective 1; note "Initial checks ran one at a time. The Docker server reports 2 CPUs and 30865 MiB of memory: room for 1 sandbox of 2 CPUs and 1024 MiB at a time."
  - With `sandbox.timeout_seconds: 120`, `sandbox.max_runtime_seconds: 200` and `--parallel 3`: exit 2; the checks ran one at a time (distinct audit times) and the note adds the budget sentence, since two full timeouts (240 s) exceed the 200 s budget.
  - With test and typecheck commands that sleep 120 s, `--parallel 3 --deadline 1m --ci`: exit 2 after 30.3 s; test and typecheck ran together and both ended TIMEOUT at the work deadline; build SKIPPED with "Overall deadline reached; the run was not started."; `execution.budget.deadline_reached` true and the deadline Unverified entry; no ERROR check.
  - `--checks=false --parallel 2` with a scripted loopback provider that ends the investigation at once: exit 0; no check; `execution.parallelism` requested 2, effective 1, note "The initial checks did not run in this review, so --parallel had no effect: every sandbox run ran one at a time."
  - `lint --parallel 2`: exit 3 ("lint does not execute sandbox checks; --parallel applies to review only"), no report directory. `review --parallel 5`: exit 3.
  - `swiftproof report` re-renders of seven of these reports: exit 0, Markdown and JSON byte-identical.
  - After every run, no container whose mounts are under that run's `TMP` remained and its harness directory was removed; the fixture checkout stayed clean on `main`.
  - Three alternating pairs of `--parallel 1` and `--parallel 3` reviews, plus the runs above, give the numbers in [PERFORMANCE](PERFORMANCE.md#parallel-initial-checks); they do not establish a speed-up on this shared host.
- Not run: `--parallel` together with `--cache-dir` or with `prepare` in a real review (the initial checks are never cached, and both paths only change the image the checks use), a Docker server with more than 2 CPUs (so never three sandboxes at once for real; the unit tests cover groups of 3 and 4 with a fake executor), and TypeScript initial checks.

<!-- F7:end -->

<!-- F8:begin -->
### F8 dependency preparation

**2026-09-26, branch `feat/f8-prepare` at `8f05f99`.** Windows 11 Pro host with Docker Desktop (Docker Engine 28.4.0, containerd image store), shared with other builds; `golang:1.26-bookworm` (go1.26.8, `sha256:a688600ca24f…`) and `golang:1.23-bookworm` (go1.23.12).

- In `golang:1.26-bookworm`, from `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race` of `internal/prepare`, `internal/cli`, `internal/report` and `internal/gitrepo` exit 0. From `hub/`: `go vet ./...` and `go test ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `prepare`, `gitrepo`, `cli`, `report`, `config` and `model` tests exit 0.
- Windows amd64 test binaries run on the host with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm -test.count=1`: `prepare` (31 passed), `gitrepo` (20 passed; the symlink-ancestor test skips on Windows), `report` (57 passed) and `cli` (46 passed) all exit 0. `TestDockerPrepareOfflineCommitAndReuse` committed an image with the base layers plus one and the policy env; a check in it found the prepared file and the same check failed on the base image; the prepare container listed only `lo` under `/sys/class/net`, ran as UID 65534 in `/swiftproof/work` with `HOME=/swiftproof/home` and no host variable; the second run reused the image without starting a container; a `user: root` build had `CapEff` `00000000000000db`. `TestDockerPrepareFailureAndShadowedOutputs` and `TestDockerReviewUsesPreparedImage` passed, as did the F0 `cli` Docker tests.
- End-to-end runs with a Windows binary built from `8f05f99` (`-X main.version=F8-e2e`), on a scratch Go module requiring `github.com/google/go-cmp v0.7.0`, with `--reviewer=false --ci` and a policy passed through `--config` from outside the fixture: `sandbox.image` `golang:1.26-bookworm@sha256:a688600c…`, `prepare` = `go mod download` on `go.mod` and `go.sum`, `network: true`.
  1. Without `--allow-prepare-network`: exit 4, `prepare.status` `not_permitted`, no check, audit `stage:prepare` SKIPPED.
  2. With `--allow-prepare-network`: exit 0, `built` (3,640 ms; committed layer 897,024 bytes); test, typecheck and build PASS offline in the derived image; a `prepare_output` artifact; the image tagged `swiftproof-prepared:<key>-<commit>` with the key, source-commit and base-image labels.
  3. The same review without the flag: exit 0, `reused` (579 ms), checks PASS; `docker events` recorded no `swiftproof-prepare-*` container created during the run.
  4. A branch adding `github.com/google/uuid v1.6.0` to `go.mod` and `go.sum`: exit 2, `reused`; `prepare_input_changed` signals on both files; test, typecheck and build FAIL (the module is not in the prepared cache); the changed-inputs Unverified entry; no reproduced issue.
  5. The same fixture with a policy without `prepare`: exit 2, checks FAIL with "could not create module cache", showing that run 2 used the derived image.
  6. After `docker image rm` of the tag, `--allow-prepare-network --no-network`: exit 4, `not_permitted`.
  7. `max_added_mb: 1` with an offline command writing 2,000,000 random bytes: exit 4, `failed` ("adds 2031616 bytes … over prepare.max_added_mb (1 MiB); the image was removed").
  8. `sleep 300` as the command with `--deadline 1m`: exit 4 after 30 s, `failed` ("the overall --deadline was reached during dependency preparation"), audit TIMEOUT, and the deadline Unverified entry.

  `swiftproof report` re-rendered all eight reports byte-identically, and all eight validate against the branch schema (Python `jsonschema`, Draft 2020-12). The fixture checkout stayed clean, and no `swiftproof-prepare-` container and no prepared image remained. The same scenarios gave the same statuses and exit codes with a binary built from the working tree before the commit; its timings are in [performance](PERFORMANCE.md).

**2026-09-26, review fixes, branch `feat/f8-prepare` at `dd96a1f`.** Same host and images, shared with other builds.

- In `golang:1.26-bookworm`, from `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0; from `hub/`: the same, exit 0; `CGO_ENABLED=1 go test -count=1 -race` of `internal/prepare`, `cli`, `report`, `gitrepo`, `config` and `model` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `prepare`, `gitrepo`, `cli`, `report`, `config` and `model` tests exit 0.
- Windows amd64 test binaries with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm -test.count=1`: `prepare` (37 passed), `report` (58 passed), `gitrepo` (20 passed, the symlink-ancestor test skipped on Windows) and `cli` (46 passed, including `TestDockerReviewUsesPreparedImage`) exit 0. `TestDockerPrepareLongListingAndImageVolume` built, with outputs `persistent`, a command that wrote 20,000 files under its `HOME` and one directory under `/var/tmp`, and a check read the file there; a sandbox image declaring `VOLUME /data` failed before the command started, and the container's anonymous volume did not outlive it.
- End-to-end runs with a Windows binary built from `dd96a1f` (`-X main.version=F8fix2-e2e`) on the fixtures of the entry above, with `--reviewer=false --ci`: the eight scenarios above gave the same statuses, exit codes and audit statuses (built 3,982 ms; reused 973 ms, and no `swiftproof-prepare-*` container creation in `docker events` around the run, where the build run showed one; changed inputs exit 2 with the Unverified entry "… Candidate dependency changes were not installed, so checks may fail or behave differently for that reason alone; SwiftProof attributes no check result to it."). The Markdown read "network during the build: enabled" for the build and "the image was built with network enabled" for the reuse, and "No image was used for checks, so no check ran." for the size cap. Three more:
  9. `user: root`, a command writing 20,000 files under `$HOME` and one file under `/usr/local`, on a second fixture: exit 0, `built`, outputs label `persistent`, checks PASS, and a read-only, offline container as UID 65534 read the file from the derived image. A binary built from `ce46292` (before the fixes) gave exit 2 with the false "prepared outputs are shadowed by check mounts" entry on the same fixture and policy.
  10. A local image committed from `golang:1.26-bookworm` with `VOLUME /data` as `sandbox.image`: exit 4, `failed` ("the prepare container has mounts other than the read-only inputs (volume at /data) …"), the Unverified entry says the prepare command did not run; `docker volume ls` before and after the run showed no volume left by it, and no `swiftproof-prepare-` container remained.
  11. A command writing only under `$HOME` and `/tmp`: exit 2, `built`, checks PASS, and the "prepared outputs are shadowed by check mounts" Unverified entry.

  `swiftproof report` re-rendered all eleven reports byte-identically, and all eleven validate against the branch schema (Python `jsonschema`, Draft 2020-12). The fixture checkouts stayed clean, and no `swiftproof-prepare-` container and no prepared image of these binaries remained.

Not exercised: the classic `overlay2` image store (the layer and size checks ran on the containerd store only), Node and Python recipes, and an interrupted review (Ctrl-C) during preparation, which only the package tests cover.
<!-- F8:end -->

<!-- F9:begin -->
<!-- F9:end -->
