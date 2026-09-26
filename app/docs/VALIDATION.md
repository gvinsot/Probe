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
**2026-09-26, observation oracle (F1).** Windows 11 Pro amd64 host, Docker Engine 28.4.0, `golang:1.26-bookworm` (go1.26.8, digest `sha256:a688600ca24f…`), `golang:1.23-bookworm` (go1.23.12) and `swiftproof-ts-test:local` (Node 22.23.3, Vitest 3.2.7, Jest 29.7.0; image `sha256:75b21685b1d7…`).

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race` of `internal/harness`, `internal/report`, `internal/observe`, `internal/reviewer` and `internal/cli` exit 0. From `hub/`: `go vet ./...` and `go test ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `observe`, `harness`, `report`, `reviewer` and `model` tests exit 0.
- Windows amd64 test binaries. `harness` with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm` and `SWIFTPROOF_TEST_TS_IMAGE=swiftproof-ts-test:local`: all ten Docker tests pass, including `TestDockerGoObservationIntegration` (a rewritten `Discount` diverges on `Discount(5,33)` only, baseline `4`, candidate `3`, with three PASS checks; a plain `=== ATTR` line printed by candidate code adds no key; a framed line printed by candidate code makes its key `INCOMPARABLE` and the record `UNVERIFIED`, with two checks) and `TestDockerTSObservationIntegration` (Vitest diverges through `task.meta.swiftproof`; a Jest template records an `UNVERIFIED` observation). `cli` with the Go image: `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck` pass. The `harness`, `report`, `observe` and `reviewer` suites also pass natively on Windows. No `swiftproof-` container created by these runs remained.
- A Windows binary built from the branch (`-X main.version=F1-e2e`), with a scripted loopback provider (`create_test`, `run_generated_test`, `submit_hypothesis`, then stop) and policies passed with `--config` from outside the fixture repositories, ran `review --checks=false --ci`:
  - (a) Go fixture whose `Discount` changes from `total - total*percent/100` to `total * (100 - percent) / 100`, observed on three inputs, with a critical `DIVERGED` claim: exit 2. Checks `generated_test_base`, `generated_test_candidate` and `generated_test_base_repeat` PASS; `differential_observation` evidence `DIVERGED`; the hypothesis `DIVERGED`; `reproduced_issues` empty; `divergences[0]` holds exactly one row, `Discount(5,33)` with baseline `4` and candidate `3`, anchored at the hypothesis's changed line; stdout prints "1 recorded behavior divergence (…)"; the retained `generated_test` artifact's sha256 matches the test source; the both-pass `differential_test` is rendered "as stored; not accepted as evidence".
  - (b) An equivalent rewrite with a `NOT_REPRODUCED` claim: exit 0; two checks; `NOT_DIVERGED` (three equal keys); the claim is kept; Behavior Divergences reads "No recorded experiment diverged. …".
  - (c) A test recording `Discount(5,33)` and `time.Now().UnixNano()`: exit 2; the clock key is `UNSTABLE` and the record `DIVERGED` on `Discount(5,33)` alone. A test recording only an equal key and the clock key: exit 2; the record is `UNVERIFIED` (one equal, one unstable), the `DIVERGED` claim becomes `UNVERIFIED`, and the both-pass `NOT_REPRODUCED` of the same test is withdrawn.
  - (d) A `DIVERGED` claim citing the `differential_test`: exit 2; the hypothesis is `UNVERIFIED`; the divergence stays listed with no citing hypothesis.
  - (e) A TypeScript fixture with `swiftproof-ts-test:local` and a Vitest template: exit 2; `DIVERGED` on `discount(5,33)` (`4`, `3`), with a non-string meta value re-encoded canonically. The same experiment through a Jest template, on a CommonJS fixture with a `package.json` (Jest in this image has no TypeScript transform configured): exit 2; the observation record is `UNVERIFIED` ("… Jest reports carry no per-test metadata"), and the `DIVERGED` claim becomes `UNVERIFIED`.
  - Every `swiftproof report` re-render was byte-identical (JSON and Markdown); the seven reports and their re-renders validate against the schema (`TestSchemaValidatesReportFiles`); every fixture checkout stayed clean.

**2026-09-26, observation oracle review fixes (F1).** Same host, Docker Engine 28.4.0 and images as the entry above.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race` of `internal/observe`, `internal/harness`, `internal/report`, `internal/reviewer` and `internal/cli` exit 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `observe`, `harness`, `report`, `reviewer` and `model` tests exit 0. The tests added in this round fail when run against the previous implementation files (with the two new identifiers stubbed).
- Windows amd64 test binaries: `harness` with both test images, all ten Docker tests pass (including `TestDockerGoObservationIntegration` and `TestDockerTSObservationIntegration`, Vitest and Jest); `cli` with the Go image, its three Docker tests pass. No `swiftproof-` container created by these runs remained.
- A Windows binary built from the branch (`-X main.version=F1fix-e2e`) with the scripted loopback provider, `review --checks=false --ci`:
  - Go `Discount` rewrite with a critical `DIVERGED` claim: exit 2, three PASS checks, `divergences[0]` holds `Discount(5,33)` with baseline `4` and candidate `3`. Equivalent rewrite with a `NOT_REPRODUCED` claim: exit 0, `NOT_DIVERGED`, the claim is kept.
  - A candidate `Label(1)` returning 5000 bytes (too long for test2json to convert) with a high `NOT_REPRODUCED` claim: exit 2; the observation record is `UNVERIFIED` (one incomparable key), the `differential_test` is rendered "as stored; not accepted as evidence", and the claim is `UNVERIFIED`.
  - Vitest `discount` rewrite: exit 2, `DIVERGED` on `discount(5,33)` (`4`, `3`). The same test recording an extra 180000-character `"<"` string: exit 2, the three checks PASS (no ERROR), the long value is kept as a stand-in and `INCOMPARABLE`, and the record is `DIVERGED` on `discount(5,33)`. A test recording `discount(100,10)` as a number and as `String(...)` with a `NOT_REPRODUCED` claim: exit 0, `NOT_DIVERGED` with rows `90` and `"90"`, the claim is kept.
  - Every `swiftproof report` re-render was byte-identical (JSON and Markdown), the twelve reports validate against the schema (`TestSchemaValidatesReportFiles`), and every fixture checkout stayed clean.
<!-- F1:end -->

<!-- F2:begin -->
<!-- F2:end -->

<!-- F3:begin -->
**2026-09-26, changed baseline tests (F3).** Windows 11 Pro amd64 host (8 CPUs, shared with other concurrent Docker work), Docker Engine 28.4.0, `golang:1.26-bookworm` (image `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`) and `golang:1.23-bookworm` (go1.23.12). Branch `feat/f3-base-tests` at `4106be3`, started from `5c0ad17`.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race` of `suites`, `harness`, `report`, `linter` and `cli` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `suites`, `harness`, `report`, `linter`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: `TestDockerBaseTestsRealGo` (harness; containers it launched checked by name afterwards, none left) and `TestDockerBaseTestsEndToEnd` (cli; scenarios A and B below) pass, as do the other harness and cli Docker tests.
- A Windows binary built from `4106be3` (`-X main.version=F3-e2e`) on scratch fixtures, with policies passed by `--config` (commands `test` and `generated_test`) and `TMP` pointed at a scratch directory:
  - A (the candidate drops the upper bound of `Clamp`, loosens `TestClampUpper`, moves `TestClampInside` to a new `extra_test.go`, adds a comment inside `TestClampNegative` and a skipped `TestLater`), `review --base-tests --reviewer=false --ci`: exit 2. `test` PASS. `base_tests` `ran` with two tests: `TestClampUpper` (`modified`) `FAILS_ON_CANDIDATE` from `base_test_base` PASS and `base_test_hybrid` FAIL under one command; `TestClampInside` (`removed`) `PASSES_ON_CANDIDATE` from a run pair of its own after passing inside that failed hybrid run. `TestClampNegative` was not selected. Signals `test_expectation_relaxed`, `test_skip_added` and `test_assertion_removed` on `clamp_test.go`; high review targets on both versions of `TestClampUpper`. Without `--ci`: exit 0, same results.
  - B (the candidate renames `Clamp` to `Limit` and updates its tests): exit 2, not 4. The three selected tests are `UNVERIFIED` ("the candidate-side package did not build or set up, so the test did not run"); the `base_test_hybrid` check is FAIL, and no check is ERROR.
  - C (scenario A without `--base-tests`): exit 2, no `base_tests` object and no `base_test_*` check. The published v0.2.0 binary on the same fixture and policy also exits 2 (a high `public_api_change` signal in both).
  - D: `lint --base-tests` and `review --base-tests --checks=false` exit 3 and write no report directory.
  - E (`generated_test` is `["go", "test", "./..."]`): exit 2; `base_tests` `not_run` with the template reason; no `base_test_*` check.
  - F (`max_runtime_seconds` 1): exit 2, not 4. `test` TIMEOUT; `base_test_base` SKIPPED ("Sandbox runtime budget exhausted."); `base_tests` `not_run` ("no run started: Sandbox runtime budget exhausted.").
  - After every scenario the fixture checkout was clean, no harness directory was left in the scratch temporary directory, and no container with a mount under it remained. `swiftproof report` re-rendered all six reports byte-identically, and all six JSON reports validate against the schema (`TestSchemaValidatesReportFiles`).
  - Each base-test run took 6 to 17 s on this host (the Go build cache starts empty in every sandbox run).

**2026-09-26, changed baseline tests (F3), review fixes.** Same host, Docker Engine 28.4.0, `golang:1.26-bookworm` (image `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`) and `golang:1.23-bookworm` (go1.23.12). Branch `feat/f3-base-tests`, the commit after `7b96ccf`. The fixes: selection now also covers file-level edits (build constraints, package clause, imports, compiler directives, an added `init`, `TestMain`, initialized variable or method, and renames to another directory or platform suffix); candidate entries that block a baseline test path are removed from the hybrid tree instead of failing the stage with exit 4; reason texts hold no character that the Markdown renderer escapes.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race` of `suites`, `harness`, `report`, `linter` and `cli` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `suites`, `harness`, `report`, `linter`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`: harness `TestDockerBaseTestsRealGo` and `TestDockerExistingTestOutcomesFromRealGoTest` pass; cli `TestDockerBaseTestsEndToEnd` passes with scenarios A, B, G and H (G and H below). Every `swiftproof-` container that appeared during these runs had its mount in another process's harness directory.
- A Windows binary built from the same code (`-X main.version=F3-fix-e2e`), same fixtures, policies and scratch `TMP` as the entry above:
  - A: exit 2 with `--ci` and exit 0 without; both give `TestClampUpper` `FAILS_ON_CANDIDATE` and `TestClampInside` `PASSES_ON_CANDIDATE` from a run pair of its own. B: exit 2, three `UNVERIFIED`, hybrid check FAIL, no ERROR check. C: exit 2 (the published v0.2.0 binary: exit 2). D: exit 3 twice, no report directory. E: exit 2, `not_run` with the reason "the generated_test command cannot establish which Go tests ran; configure it as go test {package}". F: exit 2, `not_run` ("no run started: Sandbox runtime budget exhausted").
  - G (the candidate drops the upper bound and adds `//go:build never` before the package clause of `clamp_test.go`; no test function changes): the candidate's `go test ./...` is PASS with `[no test files]` and no risk signal fires. With `--base-tests --ci`: exit 2; all three tests selected as `shared_code_changed`; `TestClampUpper` `FAILS_ON_CANDIDATE`, the other two `PASSES_ON_CANDIDATE` from a run pair of their own. The published v0.2.0 binary exits 0 on the same fixture.
  - H (the candidate drops the upper bound and replaces the test-only package directory `itest` with a regular file): exit 2, not 4; `TestUpperBound` (`file_deleted`) `FAILS_ON_CANDIDATE`; the hybrid manifest records `itest` as `removed_from_candidate` and `itest/bounds_test.go` as `restored_from_baseline`.
  - After every scenario the checkout was clean, no harness directory was left in the scratch temporary directory, and no container with a mount under it remained. `swiftproof report` re-rendered all eight reports byte-identically, all eight JSON reports validate against the schema (`TestSchemaValidatesReportFiles`), and no Markdown report holds an escaped HTML entity.
  - Each base-test run took 11 to 37 s on this host, which other Docker work shared.
<!-- F3:end -->

<!-- F4:begin -->
### F4 mutation of added lines

**2026-09-26, branch `feat/f4-mutation` at `f395cb2`** (the commit after it adds only documentation and a schema test). Windows 11 Pro host with Docker Desktop (Docker Engine 28.4.0), shared with other agents' builds and tests; `golang:1.26-bookworm` (go1.26.8, `sha256:a688600ca24f…`) and `golang:1.23-bookworm` (go1.23.12).

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

**2026-09-26, review fixes, branch `feat/f4-mutation` at `3030892`** (the commit after it adds only documentation). Same host and images, shared with other agents' Docker tests.

- In `golang:1.26-bookworm`, from `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race -count=1` of `internal/mutation`, `internal/report`, `internal/cli`, `internal/harness` and `internal/coverage` exit 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `mutation`, `report`, `cli`, `harness` and `model` tests exit 0.
- A throwaway test (not committed) enumerated the mutants of every non-test `.go` file under `/usr/local/go/src` of go1.26.8 (without `testdata` and files over 1 MiB) with every line treated as added, and applied each one. Before the neighbouring-character rule: 2,977 files not skipped, 241,506 mutants, 2 refused by `Apply` (the `swap_arithmetic` of `-1+-4` in `internal/diff/diff.go` and of `w+-a.dp` in `internal/strconv/decimal.go`, both "does not parse"). After it: 241,504 mutants, none refused.
- A real `go test -json -count=1 ./marker` run in `golang:1.26-bookworm` with the sandbox flags recorded `testdata/gotest-marker.jsonl`: a passing test that printed `\x16=== RUN` and `\x16--- PASS` lines for `TestMarkerForged` and wrote raw `run` and `pass` events for `TestProcForged` to `/proc/1/fd/1` got both names recorded as passing tests.
- Windows amd64 test binaries built from the same tree, run on the host from each package directory with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm -test.count=1`: `mutation`, `report`, `harness` and `cli` exit 0 (61, 151, 231 and 118 `--- PASS` lines, subtests included, no failure or skip), including `TestDockerMutantBoundaryUnchanged`, `TestDockerMutationClassifiesRealRuns` and `TestDockerMutationEndToEnd`. On the host `TestEverySiteApplies` found no standard library sources and checked its handwritten corpus only.
- End-to-end runs with a Windows binary built from the same tree (`-X main.version=F4fix-e2e`), the fixtures and policies of the entry above, `--reviewer=false` and `--out` outside the fixtures:
  1. Discount fixture, `review --ci`: exit 0 in 206 s; `ran`, 8 generated and selected, the same statuses, check IDs, `tests_run`, `failed_tests` and patch hashes as in the entry above; the survivor list now reads "no test that the command ran for the package failed with the change; skipped tests are not counted". The same review again: identical mutants.
  2. `max_mutants: 3`: exit 2, `incomplete`, the same three `KILLED` mutants.
  3. `--checks=false --ci`: exit 2, `not_run`; Unverified now holds "Mutation analysis did not run: initial checks disabled (--checks=false)" after the generic sentence.
  4. Absent image: exit 4, `not_run`, one ERROR control, eight `NOT_RUN`, one mutation sentence. `-run=X`: exit 3. `lint`: exit 0 and no `mutation` object.
  5. A fixture (module `example.test/load`) whose change adds `Parse(name string, n int) (int, error)` returning a gofmt'd two-line `fmt.Errorf(...)` and adds `load/fast_windows.impl.go`: exit 0 under `--ci`; `ran`, 4 generated and selected; `negate_condition` and `drop_error` (lines 13 to 14, replaced by `nil` and one line break) `KILLED` by `TestParse`, `boundary` and `increment_constant` `SURVIVED`; `fast_windows.impl.go` skipped for its GOOS suffix.
  6. `swiftproof report` re-rendered the reports of the two Discount runs, the load fixture, `max_mutants: 3`, `--checks=false` and the absent image byte-identically. A copy of the first report with an added `mutant-9` at line 19 citing `mutant-2`'s `check_id` and `patch_sha256` re-rendered with both mutants `INCONCLUSIVE` and the section `incomplete`; a copy whose control and `mutation-check-3` both ran `go test -json -run=TestClamp ./price` re-rendered with all eight mutants `INCONCLUSIVE`.

  All fifteen JSON reports validate against the branch schema (Python `jsonschema` 4.26.0). The fixture checkouts had no change, and no container of these runs remained.

Not exercised: a mutant run that a budget really cut short (fake-workspace unit tests and a harness unit test of the reported limit only), and the `/proc/1/fd/1` route through a whole review (recorded fixture only).
<!-- F4:end -->

<!-- F5:begin -->
<!-- F5:end -->

<!-- F6:begin -->
**2026-09-26, F6a static index, callers and tools (branch `feat/f6a-index`).** Windows 11 Pro host with Docker Desktop (the containers saw 2 CPUs; other agents built and tested concurrently); `golang:1.26-bookworm` (Go 1.26.8) and `golang:1.23-bookworm` (Go 1.23.12).

- From `app/` in `golang:1.26-bookworm`: `go vet ./...` and `go test -count=1 ./...` exit 0, including the new `internal/symbols` tests and the impact tests of `cli`, `harness`, `model` and `report`; `CGO_ENABLED=1 go test -race -count=1` of `internal/symbols`, `internal/cli`, `internal/harness` and `internal/report` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` from `app/` (with and without the workspace) and the `symbols`, `cli`, `report`, `model` and `harness` tests exit 0.
- Windows amd64 test binaries: the `symbols`, `report`, `harness` and `cli` suites pass natively on the host; with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, the `cli` Docker tests pass (`TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks`, `TestDockerDeadlineStopsRunningCheck`). F6a adds no Docker-gated test: the index executes nothing.
- A Windows binary built from the branch (`-X main.version=F6a-e2e`) on the shop fixture (`price.Total` called by `api.Checkout`, `cart.Cart.Total` reachable through `cart.Totaler` from `notify.Message`; the candidate changes both bodies):
  - `lint --base main`: exit 0, no check; `impact.status` `indexed`, 6 indexed files, both functions `body_changed`; low `impacted_caller` signals at `api/handler.go:7` (static) and `notify/notify.go:7` (interface); reaching tests `TestTotal` (depth 1) and `TestCheckout` (depth 2); the Impact Analysis section and the stdout line are present.
  - `lint --impact=false`: no `impact` object, no `impacted_caller` signal, the same review surface (2 of 4 changed lines focused).
  - Two `lint` runs: identical JSON apart from `generated_at`, identical Markdown.
  - `review --config policy.json --reviewer=false --ci` (the default Go policy, image `golang:1.26-bookworm`): exit 2 after 65 s; test FAIL, typecheck and build PASS, coverage FAIL; the `impact` object and signals as in lint; no reproduced issue.
  - `review --checks=false` with a scripted loopback provider calling `find_callers` (`price.Total`, depth 2), `inspect_symbol` (`cart.Totaler.Total`), `find_references` (`(*Cart).Total` and `Totaler`) and `find_callers` with depth 4: exit 0. The index answered the first three with `method: go_static_index` (callers `api.Checkout` at depth 1, `price.TestTotal` at depth 1 and `api.TestCheckout` at depth 2; the implementation `cart.Cart.Total`; the interface call in `notify/notify.go`); `Totaler` got the lexical answer with the index note; depth 4 was refused ("depth must be between 1 and 3"). Every call is audited under its tool name; no evidence and no check were recorded.
  - `swiftproof report` re-renders of the three reports are byte-identical; the fixture checkout stayed clean; no `swiftproof-` container created by these runs remained.
- The costs in [performance](PERFORMANCE.md) were measured with `BenchmarkIndexSynthetic` and `BenchmarkAnalyzeRepository` (this repository and `GOROOT/src`). `--impacted-tests` was not exercised: it still records `not_run` until F6b.

**2026-09-26, F6a review fixes (branch `feat/f6a-index`).** Same host and images. The fixes: positions ignore `//line` directives; the impact searches and tool queries have visit and work budgets, check the time limit and cancellation, and follow an interface method's references once per search; the interface-candidate and reaching-test caps are reported; alias receivers resolve; snippets come from the whole-file redaction; the duplicate-module reason is counted and `impact.reason` is cut at 4 KiB; the note is always the fixed text.

- From `app/` in `golang:1.26-bookworm`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race -count=1` of `internal/symbols`, `internal/cli`, `internal/harness` and `internal/report` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` from `app/` with and without the workspace, and `GOWORK=off go test -count=1` of `symbols`, `cli`, `report`, `model` and `harness`, exit 0.
- Windows amd64 test binaries: the `symbols`, `report`, `cli` and `harness` suites pass on the host; with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck` pass. The `harness` Docker tests were not re-run: in `harness` this round changes only the text of the lexical-fallback note in `symbols.go`, after which `go vet ./...` and `go test -count=1 ./...` from `app/` were run again (exit 0).
- A Windows binary built from the fixes (`-X main.version=F6afix-e2e`):
  - `lint --base main` on a fixture whose candidate puts `//line price.y:900` above the changed `lib.F`, `//line other/other.go:3` inside an added `init` that calls it, and only the comment `//line use/hidden_test.go:1` above the unchanged caller `use.Genuine` (the base already has callers under `//line /etc/passwd:1` and `//line hidden_test.go:1`): exit 0, `indexed`; `lib.F` at `lib/lib.go:4`; callers `use/gen.go:6`, `use/hide.go:6` and `use/use.go:8`, with forward slashes; the `init` reference on added lines is not a caller.
  - `lint --base main` on the shop fixture: exit 0, the same impact as the first entry (2 changed functions, 2 caller sites, 2 reaching tests).
  - `review --config <policy with the scripted loopback provider> --ci` on the shop fixture: exit 2 after 70 s (test and coverage FAIL, as in the first entry); the provider's `find_callers`, `inspect_symbol` and `find_references` calls were answered with `method: go_static_index`, `Totaler` lexically with the index note, and depth 4 was refused. No `swiftproof-` container created by these runs remained.
- `BenchmarkImpactSearchAdversarial` (three runs of three iterations) gave the adversarial-shape costs in [performance](PERFORMANCE.md).

**2026-09-26, F6a second review fixes (branch `feat/f6a-index`).** Same host and images. The fixes:

- Each interface-implementation check is estimated from the receiver type's embedded types, fields and methods and the interface's methods. A check estimated above 8,388,608 `go/types` steps is not made: the section is `limited` with a reason, and tool answers are `truncated`.
- The time limit and cancellation are checked before every check, and each check is charged to the budget in proportion to its estimate.
- A method of a generic type is checked on the generic type. An interface that only some instantiation may implement is reported as a gap.
- The documentation no longer presents the 120 s limit as a bound on the whole analysis: the type check of one package without type errors is not interrupted.

- From `app/` in `golang:1.26-bookworm`: `go vet ./...` and `go test -count=1 ./...` exit 0; `CGO_ENABLED=1 go test -race -count=1` of `internal/symbols`, `internal/cli`, `internal/harness` and `internal/report` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` from `app/` with and without the workspace, and `GOWORK=off go test -count=1` of `symbols`, `cli`, `report`, `model` and `harness`, exit 0.
- New tests: `TestLookupSteps`, `TestUsesTypeParams`, `TestWideEmbeddingReceiverNotChecked`, `TestImplementersCheckDeadlineBeforeEachCheck`, `TestImplementationChecksStopAtTimeLimitsAndCancel` and `TestGenericReceiverInterfaceCallers`. Copies of the last three, adapted to compile against the previous commit `b17b451`, failed there:
  - a budget past its deadline did not stop the implementation checks;
  - the searches took 15.1 s against a 1 s deadline;
  - `Box.Size` got no interface caller.
- Windows amd64 test binaries: the `symbols`, `report`, `harness` and `cli` suites pass on the host; with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck` pass.
- A Windows binary built from the fixes (`-X main.version=F6afix2-e2e`):
  - `lint --base main` on the shop fixture: exit 0, `indexed`, the same impact as the first entry.
  - `lint --base main` on a fixture with 1,000 types, each embedding a struct of 3,000 empty types and each with a `Do` method calling the changed `f.F`, and 200 interfaces `I{ Do() int; Xk() }`: exit 0 in 0.3 s. `limited` with the implementation-check reason; `callers_total` 1,000 and `tests_total` 1 for `f.F`; 10 `impacted_caller` signals and one `analysis_limited` signal.
  - `lint --base main` on a fixture where `Box[T].Size() int` and `Box[T].Get() T` changed, with `Sizer{ Size() int }` and `IntGetter{ Get() int }` called in `u/u.go`: exit 0. `Box.Size` has the `interface` caller `u/u.go:5`; `Box.Get` has the generic-receiver reason, and the section is `limited`.
  - `review --checks=false` with a scripted loopback provider on the wide fixture: exit 0 in 0.35 s. `find_references` (`ts.T40.Do`), `inspect_symbol` (`ifs.I0.Do`) and `find_callers` (`f.F`, depth 2) were answered with `method: go_static_index` and `"truncated": true` (`callers_total` 1,001), and each call is audited.
  - `review --config policy.json --reviewer=false --ci` on the shop fixture: exit 2 after 45 s. Test and coverage FAIL, typecheck and build PASS, as in the first entry; the impact is as in lint.
  - The fixture checkouts stayed clean, and no `swiftproof-` container created by these runs remained.
- The review's probe tests, run in a scratch copy of the fixed code and not committed, with 2 CPUs:
  - The shapes the review had measured on the previous code at 1 min 44 s (3 s limit), 2 min 33 s (default limits) and 6 min 58 s (6,000 embedded types) ended in 24–307 ms, `limited`.
  - With 1,000 embedded types and 1,000 types (each check within the bound), a 3 s limit ended the analysis at 3.013 s. A cancellation at 3 s returned `context canceled` at 3.021 s, and `find_references ts.T40.Do` took 5.0 s against its 10 s bound.
- `BenchmarkImplementsWideEmbedding` (three runs), `BenchmarkImpactSearchWideEmbedding` and `BenchmarkImpactSearchAdversarial` (three runs of three iterations each) gave the costs in [performance](PERFORMANCE.md).
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
