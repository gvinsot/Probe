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
### F4 mutation of added lines

**2026-09-26, branch `feat/f4-mutation` at `f395cb2`** (the documentation commit after it changes no code). Windows 11 Pro host with Docker Desktop (Docker Engine 28.4.0), shared with other agents' builds and tests; `golang:1.26-bookworm` (go1.26.8, `sha256:a688600ca24f…`) and `golang:1.23-bookworm` (go1.23.12).

- In `golang:1.26-bookworm`, from `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race -count=1` of `internal/mutation`, `internal/harness`, `internal/cli`, `internal/report` and `internal/coverage` exit 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `mutation`, `report`, `cli`, `coverage`, `config`, `model` and `harness` tests exit 0.
- Windows amd64 test binaries built from `f395cb2`, run on the host from each package directory with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm -test.count=1`: `harness` (103 passed), `cli` (43), `report` (61), `mutation` (34), `coverage` (28) and `config` (25), all exit 0. `TestDockerMutantBoundaryUnchanged` ran a mutant whose `init` panics unless the run is non-root, the source mount and root filesystem are read-only, only loopback exists and no host variable is visible; it passed, and the candidate, baseline and restored workspace trees were byte-identical afterwards. `TestDockerMutationClassifiesRealRuns` classified real runs as `KILLED` (`sum -= p`, failing `TestTotal`), `SURVIVED` (`n >= 100`, 2 tests passed) and `INVALID` (a type error). `TestDockerMutationEndToEnd` ran the Discount scenario below through the CLI. The F0 Docker tests of `harness` and `cli` passed too, and no container of these runs remained.
- An earlier build of the same branch (before `3ececda`) ran the design's fixture, whose only use of `errors` was `return 0, errors.New("negative total")`: the `drop_error` mutant was `INVALID` (the import became unused) instead of the predicted `KILLED`. That run is why `drop_error` is no longer generated when the dropped expression holds every use of an import, and why the fixture below keeps the error in a package-level `ErrNegative`.
- End-to-end runs with a Windows binary built from `f395cb2` (`-X main.version=F4-e2e`), `--reviewer=false`, policies passed with `--config` from outside the fixture (the `init` Go policy with `sandbox.max_runtime_seconds: 900` and `mutation` = `go test -json -count=1 -failfast {package}`, `max_mutants: 10`, `timeout_seconds: 60`, `max_runtime_seconds: 400`) and `--out` outside the fixture. Fixture: module `example.test/shop`; `main` has `price/price.go` (`Clamp`) and `price_test.go`; `feature` adds `Discount(total int) (int, error)` returning `ErrNegative`, a `discount_test.go` asserting only `Discount(200) == 190` and an error for `Discount(-5)`, and `price/fast_windows.go`.
  1. `review --ci`: exit 0 in 92 s. `mutation.status` `ran`, 8 generated, 8 selected, 0 dropped; `KILLED`: both `negate_condition`, `drop_error` (`ErrNegative` → `nil`) and `swap_arithmetic`, each by `TestDiscount`; `SURVIVED`: `<` → `<=`, `0` → `(0+1)`, `>=` → `>`, `100` → `(100+1)`, each with `tests_run` 2. Four `mutant_patch` artifacts whose files hash to `patch_sha256`; four medium `surviving_mutant` signals on lines 15 and 18, both in the review targets; `fast_windows.go` skipped for its GOOS suffix; `mutation-check-1` (control) to `mutation-check-9`, all `go test -json -count=1 -failfast ./price`; `checks` held only test, typecheck, build and coverage (all PASS); no evidence, no hypothesis, no Unverified entry; audit `stage:run_mutation_control` then eight `stage:run_mutant`; no `%` in the Markdown.
  2. The same review again: exit 0; identical mutant IDs, lines, columns, operators, statuses, check IDs, `tests_run`, `failed_tests` and patch hashes.
  3. `max_mutants: 3`: exit 2; `incomplete` ("5 of 8 candidate mutants were not run because of max_mutants (3)"); selected `negate_condition` (line 15), `drop_error` (16) and `negate_condition` (18), all `KILLED`; the Unverified sentence.
  4. `-run=X` in the command: exit 3, `mutation.command must not set the go test flag "-run=X"`, no report.
  5. The published v0.1.0 and v0.2.0 Windows binaries on the same fixture and policy: both exit 3 with `configuration: json: unknown field "mutation"`.
  6. `sandbox.image` `swiftproof.invalid/absent:test-only`: exit 4; `not_run` ("the unmutated control run mutation-check-1 did not pass (status ERROR, exit code 125)"); eight `NOT_RUN` mutants, one ERROR control, no survivor, one Unverified sentence.
  7. `--checks=false --ci`: exit 2; `not_run` ("initial checks disabled (--checks=false)").
  8. `swiftproof report` re-rendered the reports of runs 1, 2, 3, 6 and 7 byte-identically (JSON and Markdown). Editing run 1's JSON (a forged `tests_run`, a killed mutant claimed `SURVIVED`, a patch hash changed in `artifacts`) and re-rendering turned the three mutants into `INCONCLUSIVE` and the section into `incomplete`.
  9. The design's inline `errors.New` variant: exit 0 in 311 s under heavy host load; 7 generated (no `drop_error`), 3 `KILLED`, 4 `SURVIVED`.
  10. `lint` with the same policy: exit 0, no `mutation` object, no check.

  All twelve JSON reports validate against the branch schema (Python `jsonschema` 4.26.0, Draft 2020-12). The fixture checkouts had no change after the review runs, and no container whose command named `./price` remained. Run times are in [performance](PERFORMANCE.md).

Not exercised end to end: a review with several mutated packages (unit tests only), a reviewer together with mutation (the reviewer-reserve ceiling is covered by harness unit tests), the overall `--deadline` during a mutant run (unit tests), `TIMEOUT` mutants from a real run, a module in a subdirectory, and `--cache-dir` (mutation kinds are never cached).
<!-- F4:end -->

<!-- F5:begin -->
<!-- F5:end -->

<!-- F6:begin -->
<!-- F6:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F8:begin -->
<!-- F8:end -->

<!-- F9:begin -->
<!-- F9:end -->
