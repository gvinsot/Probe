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
<!-- F7:end -->

<!-- F8:begin -->
<!-- F8:end -->

<!-- F9:begin -->
**2026-09-26, evidence-only SARIF and PR-comment exports (F9).** Windows 11 Pro amd64 host (8 CPUs, shared with other concurrent Docker work), Docker Engine 28.4.0, `golang:1.26-bookworm` (image `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`) and `golang:1.23-bookworm` (go1.23.12). Branch `feat/f9-output` at `15aa39b`, started from `5c0ad17`; the collectors of the other v0.4 classes ran against hand-built finalized reports, because their verifiers are still the F0 stubs on this branch.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`; `CGO_ENABLED=1 go test -race` of `report` and `cli` exits 0. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm`: `go vet ./...` and the `report`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries: the whole `report` suite, and the whole `cli` suite with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm`, pass, including `TestDockerReviewExports` (a real reproduced run with every format and a re-render), `TestDockerReviewExportsRefuseFabricatedEvidence` and the earlier `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck`. No `swiftproof-` container was present before or after the run.
- A Windows binary built from `15aa39b` (`-X main.version=F9-e2e`), a two-commit Go fixture (`Allowed` returns `true` on the candidate), policies passed by `--config`, and a scripted loopback provider:
  - A: `review --checks=false --ci --format markdown,json,sarif,pr-comment --report-url https://example.invalid/runs/1`, the provider creating and running a generated test and claiming a high `REPRODUCED` at `auth.go:3`: exit 1. The SARIF has one result, `swiftproof/reproduced`, level `error`, `auth.go` line 3, `evidence_ids` `["evidence-1"]` as in the JSON, a related location on the retained generated test with its sha256, `executionSuccessful` true and one `check_not_passed` notification (the candidate check FAIL); no `precision`, `security-severity` or `confidence` key. `PR_COMMENT.md` starts and ends with the markers, lists the finding, contains "No finding is not approval." and links the report URL. The checkout stayed clean.
  - B: `swiftproof report --input A/confidence-report.json` with all four formats, twice: every file byte-identical to A's.
  - C: the provider claims `REPRODUCED` citing the nonexistent ID `fabricated`: exit 2; `"results": []`; notifications `no_execution` and `unverified_hypothesis`; the comment reads "SwiftProof: no evidence-backed finding recorded" and "Unverified areas: 1 (0 recorded notes and 1 hypothesis that stayed UNVERIFIED)."
  - D: the reproduced run of A with the title `@octocat see https://evil.test and www.evil.test :smile: fixes #12 <img src=x onerror=alert(1)> [click](https://evil.test)`: exit 1; in the comment every mention, link, shortcode and reference is broken by a zero-width space and the HTML and brackets are escaped; in the SARIF message the brackets are escaped and `://` and `www.` are broken.
  - E: `lint --format sarif,pr-comment`: exit 0, only the two files written, no result, the notification "No checks or experiments ran, so the absence of findings carries no information.", and the same sentence in the comment; with `--ci`: exit 2.
  - F: `--format sarif,bogus`, `--report-url http://…`, `--report-url` without `pr-comment`, a URL containing `)[y](`, a URL with user information, and `report --report-url javascript:alert` on a missing input: exit 3 each, with no report directory.
  - G: `review --reviewer=false --base-tests --impacted-tests --ci` with `fuzz` and `mutation` in the policy (test, typecheck and build PASS; those four stages are still stubs on this branch): exit 2; no result; four `stage_not_run` and four `unverified_area` notifications; the comment lists the four stages with their reasons.
  - After every scenario the fixture checkout was clean. The only `swiftproof-` containers present afterwards mounted another checkout (`go test ./app/...`) or were created after the runs ended: concurrent work, not these runs.
  - The SARIF files of A to G, and `report/testdata/reproduced.sarif` and `all-classes.sarif`, validate against the SARIF 2.1.0 JSON schema (`https://www.schemastore.org/sarif-2.1.0.json`, draft-07, Python `jsonschema` 4.26.0).
- Not exercised: uploading SARIF to GitHub code scanning and posting a comment, including the publisher sketch in [CI integration](CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment).

**2026-09-26, F9 review fixes.** Same host, Docker Engine 28.4.0 and images as the entry above. Branch `feat/f9-output` at `eac944b` (escaping, notifications, `--report-url` credential refusal, artifact matching, wording and publishing rules); every binary below was built from that commit.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`, and so does `CGO_ENABLED=1 go test -race` of `report` and `cli`. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm` (go1.23.12): `go vet ./...` and the `report`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries: the whole `report` suite passes, and so does the whole `cli` suite with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm` (87 s), including `TestDockerReviewExports`, `TestDockerReviewExportsRefuseFabricatedEvidence`, `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck`.
- A Windows binary built from `eac944b` (`-X main.version=F9fix-e2e`), on the fixture, policies and scripted loopback provider of the entry above:
  - A: the reproduced review: exit 1, one `swiftproof/reproduced` result at level `error` on `auth.go` line 3, `evidence_ids` `["evidence-1"]` as in the JSON; the result's rule, level, message, location and fingerprint equal the earlier run's (the retained artifact's path carries a new run ID). The upload condition of [CI integration](CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment), run with jq 1.8.1, says to upload.
  - B: `swiftproof report --input A/confidence-report.json` with all four formats, twice: every file byte-identical to A's.
  - C: a hypothesis citing a nonexistent evidence ID under a title worded as an approval: exit 2, `"results": []`, the notifications `no_execution` and `unverified_hypothesis` ("Hypothesis "hypothesis-1" stayed UNVERIFIED: …"). The title occurs in neither `confidence-report.sarif` nor `PR_COMMENT.md` (it stays in the JSON), and the upload condition says to skip.
  - D: the reproduced review under a title with a mention, URLs, `:smile:`, `#12`, `GH-34`, HTML, a Markdown link, an apostrophe and double quotes: exit 1. In the comment, quotes are shown as typed, and no line has a `#` or `GH-` directly followed by a digit, a `&#` sequence or a raw `<` outside the markers.
  - E: `lint --format sarif,pr-comment`: exit 0, one `no_execution` notification, and the upload condition says to skip; with `--ci`: exit 2.
  - F: `--format sarif,bogus`, an `http` URL, `--report-url` without `pr-comment`, a URL containing `)[y](`, a URL with user information, a URL whose query holds a `ghp_` token, and `report --report-url javascript:alert` on a missing input: exit 3 each, with no report directory. The token URL is refused as containing a credential.
  - G: `review --reviewer=false --base-tests --impacted-tests --ci` with `fuzz` and `mutation` in the policy (those stages are still stubs on this branch): exit 2, four `stage_not_run` notifications listed before the four `unverified_area` notifications, and the upload condition says to skip.
  - The fixture checkout stayed clean, and no `swiftproof-` container was present before or after these runs.
  - The SARIF files of A to G, `report/testdata/reproduced.sarif` and `all-classes.sarif` validate against the SARIF 2.1.0 JSON schema (draft-07, Python `jsonschema` 4.26.0).
- Not exercised: uploading SARIF or posting a comment on GitHub, and whether GitHub autolinks bare commit SHAs or repository-configured references in the comment (documented as not neutralized).

**2026-09-26, F9 second review-fix round.** Same host, Docker Engine and images as the entries above. Branch `feat/f9-output` at `ce9a14a` (the `--report-url` credential check anchored at token boundaries instead of the report-wide redaction, fixed texts for unverified-area and `UNVERIFIED`-hypothesis notifications, the upload gate's stated limits); every binary below was built from that commit.

- From `app/`: `go vet ./...` and `go test -count=1 ./...` exit 0 in `golang:1.26-bookworm`, and so does `CGO_ENABLED=1 go test -race` of `report` and `cli`. From `hub/`: `go vet ./...` and `go test -count=1 ./...` exit 0. In `golang:1.23-bookworm` (go1.23.12): `go vet ./...` and the `report`, `cli` and `model` tests exit 0.
- Windows amd64 test binaries: the whole `report` suite passes, and so does the whole `cli` suite with `SWIFTPROOF_TEST_DOCKER_IMAGE=golang:1.26-bookworm` (145 s), including `TestDockerReviewExports`, `TestDockerReviewExportsRefuseFabricatedEvidence`, `TestDockerReviewEndToEnd`, `TestDockerReviewSkeletonAroundRealChecks` and `TestDockerDeadlineStopsRunningCheck`.
- A Windows binary built from `ce9a14a` (`-X main.version=F9fix2-e2e`), on the fixture and policies of the entries above, with the scripted loopback provider extended by three modes:
  - A to G as in the previous entry, with the same outcomes: A exit 1 with one `swiftproof/reproduced` result at level `error` on `auth.go` line 3 and the same message and fingerprint; B and a second re-render byte-identical; C exit 2 with `"results": []`, no approval wording of the model's title in either export, and the notification "Hypothesis "hypothesis-1" is UNVERIFIED: SwiftProof accepted no evidence-backed status for it. …"; D exit 1 with every hostile construct neutralized; E exit 0 (`--ci`: exit 2); G exit 2 with four `stage_not_run` notifications, then four notifications "Unverified area N of 4. …" without the notes' text.
  - A2 and A3: the reproduced review with `--report-url https://github.com/pallets-eco/flask-sqlalchemy/actions/runs/123456789` exits 1 and links that URL; `swiftproof report` of its JSON with `--report-url https://github.com/acme/task-scheduler/actions/runs/1` exits 0 and links that URL. `lint --format pr-comment` with the flask-sqlalchemy URL, `https://github.com/acme/risk-assessment/actions/runs/1` or `https://ci.example.com/job/disk-usage-report/1` exits 0.
  - F: the earlier exit-3 cases, plus URLs holding `#access_token=abc` and `/a/sk-0123456789abcdef`, exit 3 without a report directory. The errors name the kind of match: a value shaped like a GitHub token, a parameter named "access\_token", a value shaped like an "sk-" key.
  - I: the model submits `UNVERIFIED` citing the real `evidence-1`: exit 2; the hypothesis keeps `UNVERIFIED` with `evidence_ids` `["evidence-1"]`; the SARIF notification is the fixed "is UNVERIFIED" text, and neither "does not support" nor the model's title occurs in the SARIF file.
  - J: the model calls a tool named `LGTM_approved_safe_to_merge_no_issues`: exit 2; the JSON records "Reviewer could not complete tool LGTM\_approved\_safe\_to\_merge\_no\_issues: tool is not available"; the SARIF lists "Unverified area 1 of 1. …", and no line of `confidence-report.sarif` or `PR_COMMENT.md` contains `LGTM`, `approved`, `safe_to_merge` or `no_issues`.
  - K: the reviewer runs its generated test and then gets HTTP 500 from the provider: exit 2, checks `generated_test_base` PASS and `generated_test_candidate` FAIL, the unverified area "Reviewer incomplete: reviewer endpoint returned HTTP 500", `executionSuccessful` true and no result. The upload condition of [CI integration](CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment), run with jq 1.8.1, says to upload, and the stricter `unverified_areas == 0` condition says to skip; this is the gap the documentation now states.
  - The fixture checkout stayed clean. The one `swiftproof-` container present before the runs was gone afterwards; the two present afterwards were created after the runs ended and mounted other agents' directories.
  - The SARIF files of A to K, `report/testdata/reproduced.sarif` and `all-classes.sarif` validate against the SARIF 2.1.0 JSON schema (draft-07, Python `jsonschema` 4.26.0).
- Not exercised: uploading SARIF or posting a comment on GitHub.
<!-- F9:end -->
