# CI integration

Install a **trusted pinned** SwiftProof binary, fetch full base/candidate history, and preload an image containing the project's dependencies. Then run:

```sh
swiftproof review --base "$BASE_COMMIT" --head "$HEAD_COMMIT" --ci --out .swiftproof
```

The base identifies the trusted target branch. SwiftProof uses its merge base with the candidate for comparison and policy; `--exact` selects a direct comparison. Any explicit `--config` must be supplied from a trusted source outside candidate control.

Preserve the exit status while uploading Markdown, JSON and `.swiftproof/artifacts/`, including after failure. Exit 2 requests human review; it is not a confirmed bug. Exit 4 means execution/reporting failed, for example because the image was unavailable. Set branch protection accordingly.

In GitHub Actions use `permissions: contents: read`, checkout with `fetch-depth: 0` and `persist-credentials: false`, and an `if: always()` upload step. Pin action versions to reviewed commit SHAs in production workflows. Follow GitHub's [workflow security guidance](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions).

Fork PR lint needs no provider secret. `review` automatically invokes a model configured in trusted baseline policy; provide its named API-key environment variable if the provider requires authentication. Use `--reviewer=false` in jobs that must not contact a provider or cannot access its credentials. Remote investigation belongs in an execution context that keeps credentials outside candidate processes. Do not build/run candidate-controlled automation with privileged `pull_request_target` tokens. SwiftProof never posts comments or approves/merges PRs.

Start by collecting reports without gating merges. Measure signal usefulness on representative PRs, tune paths/commands, then incorporate high-risk areas and incomplete checks into the review policy.

## Changed-line execution and gating

SwiftProof does not fail a build for added lines that were not executed; they are reported as medium signals, or low when the coverage run did not pass. A coverage run that was configured but could not be measured does request human review with `--ci`. To enforce your own policy, read `coverage.not_executed_lines` from `.swiftproof/confidence-report.json` in a following step and decide there.

Adding the optional `coverage` command to a base-branch policy is a **release-ordered** change. Policy decoding rejects unknown fields and validates command names against a whitelist, so a policy containing the `coverage` key makes an older binary exit 3; the released v0.1.0 whitelist has four command names. Publish a release whose binary accepts the key, re-pin the workflow to that release, and only then commit the key to the base branch. A pipeline that pins v0.1.0 breaks the moment the key lands.

One asymmetry is known and deliberate: `swiftproof report` re-renders a saved report with plain `json.Unmarshal`, which ignores unknown fields. An older binary re-rendering a newer report therefore silently drops the coverage object instead of failing. Render reports with the binary that produced them.

## Release ordering for v0.4

v0.4 extends the coverage rule above to three policy keys, several flags, new report fields and the report schema. Each of them makes a binary that predates v0.4.0 fail or silently lose information, so upgrade in this order: publish v0.4.0, re-pin every workflow that reviews the base branch (download URL and sha256 together), and only then commit a new key or pass a new flag.

1. **Policy keys.** `fuzz`, `mutation` and `prepare` are optional top-level policy objects. Policy decoding rejects unknown fields, so every binary before v0.4.0 exits 3 on a policy that contains one of them. Do not add them to any base-branch policy, including this repository's own `.swiftproof.json` and `app/examples/swiftproof.go.json`, before the re-pin. `swiftproof init` never writes them.
2. **Flags.** `--base-tests`, `--fuzz`, `--impact`, `--impacted-tests`, `--cache-dir`, `--parallel`, `--allow-prepare-network`, `--deadline` and `--report-url`, and the `--format` values `sarif` and `pr-comment`, make an older binary exit 3 while parsing arguments. Do not pass them through a job pinned to an older release. The included `review.yml` and `pr-review.yml` are deliberately unchanged and pass none of them.
3. **Report fields.** An older `swiftproof report` decodes a v0.4 report with plain `json.Unmarshal`. It silently drops the new objects and fields (`prepare`, `base_tests`, `divergences`, `intent_sha256`, `intent_criteria`, `intent_test_failures`, `fuzz`, `mutation`, `impact`, `execution`, and the new check, evidence and hypothesis fields), and it re-derives hypothesis statuses it does not know, such as `DIVERGED` and `INTENT_TEST_FAILED`, as `UNVERIFIED`. Render reports with the binary that produced them.
4. **Schema consumers.** The report schema forbids unknown properties, so a consumer that validates against the v0.3 schema rejects a v0.4 report. Publish the updated schema with the release, and update consumers before they receive v0.4 reports.

A v0.4 binary changes its output even without a new key or flag: JSON reports always contain the `intent_criteria`, `divergences` and `intent_test_failures` arrays, lint and review reports contain an `impact` object unless `--impact=false` is passed, and the Markdown report always contains a Behavior Divergences section.

## Gating on v0.4 results

None of the v0.4 stages produces exit 1: only a reproduced high/critical hypothesis does. Behavior divergences, `FAILS_ON_CANDIDATE` results, intent-test failures, inconclusive fuzz results, incomplete mutation runs and configured stages that did not run request human review, which is exit 2 with `--ci`. To enforce a stricter policy of your own, read the corresponding fields of `.swiftproof/confidence-report.json` in a following step, as for coverage above.

<!-- F8:begin -->
### Dependency preparation in CI

- Commit a `prepare` object to the base branch, and pass `--allow-prepare-network`, only after the workflow is re-pinned to a v0.4 binary (release ordering above). Pass the flag only in jobs whose base-branch policy needs network for preparation; checks never get that network.
- A preparation that failed or was not permitted exits 4 and runs no check: treat it like a missing sandbox image. Upload the report directory as usual; it contains the redacted `prepare_output` log of a build.
- Ephemeral runners start without derived images and build on every job, and every new base commit builds again. Nothing is shared through a registry in v0.4. On persistent self-hosted runners, derived images accumulate: prune them with `docker image prune -a --filter label=org.swiftproof.prepare.schema`.
- A network-enabled prepare container can reach the runner's network and cloud metadata endpoints. Restrict egress at the Docker network or firewall level; SwiftProof does not.
- `prepare.timeout_seconds` (600 s by default) comes on top of the sandbox budget in the worst-case formula below; `--deadline` bounds it too. When `--deadline` ends a preparation, its cleanup runs on its own time limits (at most 10 s to remove the container and 10 s to remove an image it committed but will not use): count up to 20 s for an in-flight preparation where the formula counts 5 s per in-flight run.
<!-- F8:end -->

<!-- F7:begin -->
### Execution cache in CI

`--cache-dir` is optional; an ephemeral runner gains nothing from it unless the directory survives between reviews. When you persist it:

- **Never restore it from a cache scope that a pull request can write.** With `actions/cache`, a `pull_request` job runs the pull request's own workflow file and can save entries under the pull request's scope, which later runs of that pull request restore. Save the directory only from a job that runs the base branch's workflow on the base branch (for example a `push` to `main`), restore it in pull-request jobs with `actions/cache/restore` only, and key it on the base commit.
- Keep it outside the checkout and outside `--out` (for example under `$RUNNER_TEMP` or a runner-local directory); a location inside either exits 3. On Unix runners it must be owned by the runner user with mode 0700.
- A forged or stale entry cannot produce exit 1, a divergence or a `FAILS_ON_CANDIDATE` result. It can turn an otherwise `UNVERIFIED` experiment into a negative conclusion, which the report lists in `execution.replay_backed` (see the damage bound in [Execution cache](EXECUTION_CACHE.md#trust-and-privacy)). Only baseline PASS entries are replayed; a FAIL entry is never served. Treat the directory like the SwiftProof binary: only trusted jobs write it.
- Entries are tied to the exact SwiftProof executable, so re-pinning the release starts a new set of entries. A cache hit, rejection or contradiction has no exit code of its own.
- Give each concurrent job its own directory, or run them one after another: reviews that share a directory at the same time are not coordinated by a lock and can lose a contradiction.
- With `--cache-dir`, the image probe at sandbox setup (at most 15 s) also ends at `--deadline`, so the wall-clock bound of [Runtime bounds, deadline and report size](#runtime-bounds-deadline-and-report-size) holds with the cache too.

### Parallel initial checks in CI

- `--parallel N` needs a binary that has the flag (release ordering above). The timeout, budget and exit-code rules are the same as with `--parallel 1`. The outcomes are not always the same: under contention, checks can take longer, reach their timeout and be charged more in total (see [measured costs](PERFORMANCE.md#parallel-initial-checks)), which leaves less of `sandbox.max_runtime_seconds` for coverage, the v0.4 stages and reviewer experiments, and can shorten the timeout a later check gets from what remains.
- With `--deadline`, every check of a group passes the deadline gate when the group starts. If the deadline passes while they run, each check still running ends `TIMEOUT`, where one at a time the later ones would have been `SKIPPED` as not started. Both are incomplete checks: exit 2 under `--ci`, as with `--parallel 1`.
- The effective value is capped by the Docker daemon's CPUs and memory divided by `sandbox.cpus` and `sandbox.memory_mb`. With the default `sandbox.cpus: 2`, a daemon with 2 CPUs runs the checks one at a time and says so in `execution.parallelism.note`; a hosted GitHub Linux runner reports the CPUs of its VM. Lowering `sandbox.cpus` in the base-branch policy makes room for more sandboxes but gives each one less CPU.
- Concurrent checks share the runner. A check that ran close to `sandbox.timeout_seconds` alone can reach it when others run beside it; keep `--parallel 1` for such suites, or raise the timeout in the policy.
- A group of checks starts together only while the remaining budget covers each one's full per-run timeout, so a small `sandbox.max_runtime_seconds` (below twice `sandbox.timeout_seconds`) makes `--parallel` run them one at a time.
- `--parallel` adds one `docker info` call (at most 15 s, bounded by `--deadline`) before the initial checks when more than one could run at a time.
<!-- F7:end -->

<!-- F3:begin -->
### Changed baseline tests (`--base-tests`)

- The stage is a flag, not a policy key. Add `--base-tests` to a review job only after the job is re-pinned to a v0.4 binary: an older binary exits 3 on the flag.
- It needs the base policy's `generated_test` command to be a verifiable Go template, such as `["go", "test", "{package}"]`. With any other template the `base_tests` section is `not_run`, which requests review under `--ci`.
- Budget: the stage uses at most 180 s of the shared `sandbox.max_runtime_seconds`, typically two sandbox runs per package directory whose tests changed. Tests beyond the sub-cap stay `UNVERIFIED` with the reason. Verbose tests can exceed `sandbox.max_output_bytes`; a truncated log leaves a test `UNVERIFIED`, so raise the limit if that happens.
- Exit codes: `FAILS_ON_CANDIDATE`, `UNVERIFIED` and `not_run` give exit 2 with `--ci` and never 1. A candidate-side compile failure is a FAIL check (exit 2), not an operational failure. Only an infrastructure failure of a run, or a host-side failure to copy the candidate snapshot or keep the hybrid-tree manifest, gives exit 4; the layout of the candidate tree never does.
- To route only `FAILS_ON_CANDIDATE` results, for example to a required reviewer, read the JSON in a following step:

  ```sh
  jq -e '[.base_tests.tests[]? | select(.status == "FAILS_ON_CANDIDATE")] | length == 0' .swiftproof/confidence-report.json
  ```

  Treat a match as a request for a human decision, not as a reproduced issue: the baseline test may encode behavior the change intends to replace.
- Independently of the flag, `lint` and `review` add lexical test-edit signals. `test_focus_added` is high, so an added `.only` requests review under `--ci` from the first v0.4 run.
<!-- F3:end -->

<!-- F6:begin -->
### Impact analysis in CI

Impact analysis needs no policy key. It runs in `lint` and `review` by default, so its only release-ordering step is the `--impact` flag itself (item 2 above). Its signals never gate: `impacted_caller` (low) and `analysis_limited` (medium) do not request review under `--ci`, and a limited or unavailable index is not an operational failure. The index reads at most 64 MiB of committed Go source and has a 120 s limit, checked between packages and during its impact searches (`--deadline` counts this time but does not interrupt it), so on a large Go repository `lint` takes seconds longer and uses memory in proportion to the indexed source ([measured costs](PERFORMANCE.md)). The limit does not bound the whole analysis: the type check of one package that produces no type error is not interrupted, and a small crafted package (for example selectors on a struct that embeds thousands of types) can keep it running far longer, so keep the CI job's own timeout. Pass `--impact=false` to skip it; `--impacted-tests` then exits 3.

`review --impacted-tests` runs the unchanged Go tests that the index lists as reaching a changed function, on the baseline and on the candidate (at most 16 tests from 4 packages, inside a 180 s sub-cap of the shared runtime budget). It is a release-ordered flag (item 2 above): pass it only through a job pinned to a v0.4 binary. Under `--ci`, an impacted test that fails on the candidate (`FAILS_ON_CANDIDATE`), a selected test without a result (including tests left out by the 16-test and 4-package limits), and a stage that did not run request review (exit 2), never exit 1; an infrastructure failure of one of its runs is exit 4 like any other ERROR check. Its `generated_test` command must be a verifiable Go template such as `["go", "test", "{package}"]`, or the stage records `not_run`. Treat a failure as a request for a human decision: the static link between the test and the changed function is approximate, the test may encode behavior the change intends to alter, and one run each cannot rule out flakiness.
<!-- F6:end -->

<!-- F2:begin -->
### Differential fuzzing and gating

- Commit a `fuzz` object to the base branch only after the workflow is re-pinned to a v0.4 binary (release ordering above); older binaries exit 3 on it. For Go, keep `generated_test` a single-package `go test` template with `{package}`, or a change with Go functions to fuzz records `not_run` (with a Vitest or Jest template that runs TS/JS functions of the same change, the Go functions are listed as not fuzzed instead, with one Unverified entry; both request review under `--ci`). For TypeScript and JavaScript, use a verifiable Vitest or Jest template such as `["vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"]`, run from an image that has the runner and the project's test dependencies (prepared with the `prepare` policy object if they are not preinstalled); with another template the TS/JS functions are listed as not fuzzed. One template serves one language per review.
- For TS/JS, check that the runner picks up `<dir>/swiftproof-fuzz-<suffix>.test.ts` (or `.test.js`) next to the changed module: a Vitest `include` or `exclude`, or a Jest `testMatch` or `testPathIgnorePatterns`, configured to leave it out, or a Jest project without a TypeScript transform, cannot load the harness on the baseline, which exits 4 like any harness setup failure. SwiftProof itself skips, without a container and without an Unverified entry, the modules that a runner's own rules would leave out: under Jest, whose `{file}` argument is a regular expression, paths with `( ) [ ] { } $ ^ + * ? |` or `\` (Next.js route groups and dynamic segments, SvelteKit's `$lib`); under Vitest, modules under `dist` or `cypress` directories (its default `exclude`). Keep the runner's default of running one file's tests one after another.
- A `diverged` function never fails the build by itself: it is a behavior divergence, which requests human review (exit 2 with `--ci`, 0 without), as do `inconclusive` functions, budget cuts and a section that did not run. `not_diverged`, `disabled` and `no_candidates` request nothing. A candidate whose package does not build gives `inconclusive` functions (a FAIL check, never exit 4); only a baseline-side harness failure or an infrastructure failure of a fuzz run exits 4.
- To gate on divergences, read `.divergences[] | select(.kind == "differential_fuzz")`, or `.fuzz.functions[].outcome`, from `.swiftproof/confidence-report.json` in a following step, as for coverage. A divergence may be the intended change: treat a match as a request for a human decision, never as a reproduced issue.
- Budget the stage: a package costs two fresh containers, plus two when its first pair differs, each compiling the package's tests with an empty build cache, and `fuzz.max_runtime_seconds` (240 s by default, never more than `sandbox.max_runtime_seconds`) is carved out of the shared budget. Raise `sandbox.max_runtime_seconds` so that the initial checks, coverage, the other stages, the fuzz sub-cap and the reviewer all fit, and keep `sandbox.max_output_bytes` large enough for one package's `go test -json` log, including what the fuzzed functions print: a cut log gives no outcome (its functions are `inconclusive`, never exit 4).
- Upload `.swiftproof/artifacts/` with the report if reviewers should re-run a harness: `fuzz_harness` is the exact file that ran, and `fuzz_observations` the normalized streams.
- Render reports with the binary that produced them: an older `swiftproof report` silently drops the `fuzz` object and cannot re-derive fuzz evidence.
<!-- F2:end -->

<!-- F4:begin -->
### Mutation analysis and gating

- Commit a `mutation` object to the base branch only after the workflow is re-pinned to a v0.4 binary (release ordering above); older binaries exit 3 on it.
- Surviving mutants never fail the build: they are medium review signals, with exit 0 even under `--ci`. A section that is `incomplete` (including candidates dropped by `max_mutants`) or `not_run` requests human review, which is exit 2 with `--ci`. An infrastructure failure of a mutation run, or a mutation workspace that could not be created, checked or restored, exits 4.
- To gate on survivors, read `mutation.survived`, or the `mutation.mutants` entries whose `status` is `SURVIVED`, from `.swiftproof/confidence-report.json` in a following step, as for coverage. Do not read killed mutants as assurance, and do not compute a score from the counts.
- Budget the stage explicitly: every mutant is a fresh container that compiles the package and runs its tests, and `mutation.max_runtime_seconds` is carved out of `sandbox.max_runtime_seconds`. Raise `sandbox.max_runtime_seconds` so that the initial checks, coverage, the other stages, the mutation sub-cap and the reviewer all fit, and keep `sandbox.max_output_bytes` large enough for one package's `go test -json` log: a cut log gives no outcome. The mutation ledger holds one control run per package with selected mutants plus at most `max_mutants` mutant runs, so at most 400 logs of up to `sandbox.max_output_bytes` each (before JSON escaping); see the report size note below.
- Render reports with the binary that produced them: an older `swiftproof report` silently drops the `mutation` object.
<!-- F4:end -->

<!-- F1:begin -->
**Observation experiments.** They need no policy key and no flag, so they add no release-ordering step: a reviewer running with a verifiable `generated_test` template (Go `["go", "test", "{package}"]` with a Go 1.25 or later image, or Vitest with `{file}` and `{results_out}`) may record them in any v0.4 run. A validated divergence, cited or not, requests human review: exit 2 with `--ci`, never 1. To act on divergences in a following step, read `.divergences | length` from `.swiftproof/confidence-report.json`. Each entry carries both recorded values, the test path and names, and the three check IDs; it records a difference, not which revision is correct. Render reports with the binary that produced them: an older binary's `swiftproof report` silently drops `divergences`, and a consumer validating against an older schema rejects the new fields.
<!-- F1:end -->

<!-- F5:begin -->
**Intent criteria in CI.** Intent criteria need no policy key and no flag, so they add no release-ordering step, but a v0.4 binary exits 3 on an intent that is not UTF-8 or contains NUL, where earlier binaries accepted it. Pass the pull request description through an environment variable written to a file, never interpolated into a `run:` line, which would let the PR author inject shell:

```yaml
- name: Write the PR intent
  env:
    PR_BODY: ${{ github.event.pull_request.body }}
  run: printf '%s' "$PR_BODY" > "$RUNNER_TEMP/intent.md"
- run: swiftproof review --base "origin/$GITHUB_BASE_REF" --intent-file "$RUNNER_TEMP/intent.md" --ci
```

Put the criteria under an `## Acceptance criteria` heading as a Markdown list. An accepted intent-test failure requests human review, exit 2 with `--ci`, never 1; to act on it in a following step, read `.intent_test_failures | length` from `.swiftproof/confidence-report.json`, and key any comparison between runs on `intent_sha256`, since criterion IDs are positional (the hash covers the recorded, redacted intent, so `sha256sum` of the raw description matches it only when redaction changed nothing). Post SwiftProof's PR comment as a comment, never into the description: the marked block is removed from the intent with an Unverified note, but anything else copied there is read as intent. An older binary's `swiftproof report` drops `intent_criteria`, `intent_test_failures` and the new evidence and hypothesis fields, and re-derives `INTENT_TEST_FAILED` as `UNVERIFIED`; render with the binary that produced the report.
<!-- F5:end -->

<!-- F9:begin -->
### Publishing evidence-backed findings (SARIF and PR comment)

`--format sarif,pr-comment` writes `confidence-report.sarif` and `PR_COMMENT.md`, which list only findings backed by recorded sandbox evidence ([exports](EXPORTS.md)). SwiftProof itself never contacts GitHub; a workflow may post the rendered files with its own token. The formats and `--report-url` make an older binary exit 3, so pass them only after the re-pin described above; the included `review.yml` and `pr-review.yml` do not pass them.

- **Upload SARIF only when the exit code is 0, 1 or 2, `executionSuccessful` is true** (`.runs[0].invocations[0]` in the SARIF file), **and no notification of kind `no_execution`, `stage_not_run` or `omitted_findings` is present.** An upload closes every earlier alert of its category that it lacks as "fixed". The gate skips the uploads where the report records that execution failed, did not happen or was cut: an operational failure, a SKIPPED, TIMEOUT or ERROR check, or a reached deadline, and a run where:
  - nothing executed (`lint`, or `review` without checks and reviewer), where `executionSuccessful` is still true;
  - a configured stage did not run;
  - the 1000-result cap cut findings.

  **The gate protects only against these cases.** Reproduced, observed-divergence and intent-test findings come from the reviewer model's experiments and exist only for what the model chose to test in that run, so any upload, after a complete reviewer run as well as after one that stopped early, may close earlier alerts of these classes as "fixed" although nothing re-examined them. A reviewer that stopped early ("Reviewer incomplete: …", an exhausted iteration, tool-call or input budget) shows only as an `unverified_area` notification and passes the gate, and so does a stage that ran without finishing every item (for example a mutation section `incomplete`). A stricter gate also requires `jq -e '.runs[0].properties.swiftproof.unverified_areas == 0'` to succeed on the rendered SARIF file: that count includes every recorded unverified area and `UNVERIFIED` hypothesis, also those the notification cap leaves out. It skips more uploads, and it still does not make an alert's "fixed" state a re-examination.

  Upload only `review` output, with one category per workflow; `lint` output never has results. The "fixed" state is GitHub's, never a SwiftProof claim, and "no new alerts" or an empty comment is not an approval: keep branch protection with human review.
- **Post `PR_COMMENT.md` as a comment, never into the pull-request description.** The description is the usual `--intent-file` source.
- **Treat artifacts from fork runs as attacker-controlled.** A `pull_request` workflow runs the pull request's own workflow definition, so its JSON, `PR_COMMENT.md` and SARIF may be forged. Publish from a separate workflow triggered by `workflow_run` that:
  - never checks out or executes pull-request code;
  - downloads only `confidence-report.json`, uploaded by the review job as an artifact of its own, and refuses one larger than 64 MiB;
  - re-renders it with **its own pinned binary** (`swiftproof report --format sarif,pr-comment`) and never posts artifact Markdown or SARIF verbatim;
  - has only `actions: read` (to download the artifact), `pull-requests: write` (the comment) and `security-events: write` (the SARIF upload);
  - posts only to the pull request that triggered the run, after matching the head SHA recorded in the JSON (`change.head_commit`) with the run's head SHA and the pull request's current head.
- Re-rendering makes the exports consistent with the JSON; it does not authenticate it. Exports from fork runs may be forged.
- Without a checkout, `upload-sarif` cannot compute its own line fingerprints; the results carry `partialFingerprints` `swiftproof/v1`.

The review job uploads the JSON on its own, whatever the exit code:

```yaml
      - if: always()
        uses: actions/upload-artifact@<pinned-sha>
        with:
          name: swiftproof-json
          path: .swiftproof/confidence-report.json
          if-no-files-found: ignore
```

A publisher sketch (pin every action by commit SHA and the binary by URL and sha256; this repository's validation did not run it against GitHub):

```yaml
name: swiftproof-publish
on:
  workflow_run:
    workflows: ["swiftproof-review"]
    types: [completed]
permissions: {}
jobs:
  publish:
    if: github.event.workflow_run.event == 'pull_request'
    runs-on: ubuntu-latest
    permissions:
      actions: read
      pull-requests: write
      security-events: write
    env:
      GH_TOKEN: ${{ github.token }}
      REPO: ${{ github.repository }}
      RUN_ID: ${{ github.event.workflow_run.id }}
      HEAD_SHA: ${{ github.event.workflow_run.head_sha }}
      HEAD_OWNER: ${{ github.event.workflow_run.head_repository.owner.login }}
      HEAD_BRANCH: ${{ github.event.workflow_run.head_branch }}
      RUN_URL: ${{ github.event.workflow_run.html_url }}
    steps:
      - name: Install the publisher's pinned SwiftProof
        run: |
          curl --fail --location --retry 3 -o swiftproof.tar.gz "$SWIFTPROOF_URL"
          echo "$SWIFTPROOF_SHA256  swiftproof.tar.gz" | sha256sum --check
          tar -xzf swiftproof.tar.gz
        env:
          SWIFTPROOF_URL: https://github.com/gvinsot/SwiftProof/releases/download/v0.4.0/swiftproof-v0.4.0-linux-amd64.tar.gz
          SWIFTPROOF_SHA256: <pinned sha256>
      - name: Download only the review JSON (at most 64 MiB)
        run: |
          size=$(gh api "repos/$REPO/actions/runs/$RUN_ID/artifacts" --jq '.artifacts[] | select(.name == "swiftproof-json") | .size_in_bytes')
          test -n "$size"
          test "$size" -le 67108864
          gh run download "$RUN_ID" --repo "$REPO" --name swiftproof-json --dir untrusted
          test "$(stat -c %s untrusted/confidence-report.json)" -le 67108864
      - name: Re-render with the pinned binary
        run: ./swiftproof-v0.4.0-linux-amd64/swiftproof report --input untrusted/confidence-report.json --out rendered --format sarif,pr-comment --report-url "$RUN_URL"
      - name: Match the pull request and the recorded head
        id: pr
        run: |
          test "$(jq -r .change.head_commit untrusted/confidence-report.json)" = "$HEAD_SHA"
          number=$(gh api -X GET "repos/$REPO/pulls" -f state=open -f head="$HEAD_OWNER:$HEAD_BRANCH" \
            --jq "[.[] | select(.head.sha == \"$HEAD_SHA\")][0].number")
          test -n "$number"
          test "$number" != null
          echo "number=$number" >> "$GITHUB_OUTPUT"
          jq -e '.runs[0].invocations[0] | .executionSuccessful and .exitCode <= 2 and
                   all(.toolExecutionNotifications[]; .properties.swiftproof_kind | IN("no_execution", "stage_not_run", "omitted_findings") | not)' \
            rendered/confidence-report.sarif \
            && echo "upload=true" >> "$GITHUB_OUTPUT" || echo "upload=false" >> "$GITHUB_OUTPUT"
      - name: Upload SARIF only when the gate passes
        if: steps.pr.outputs.upload == 'true'
        uses: github/codeql-action/upload-sarif@<pinned-sha>
        with:
          sarif_file: rendered/confidence-report.sarif
          category: swiftproof
          ref: refs/pull/${{ steps.pr.outputs.number }}/head
          sha: ${{ github.event.workflow_run.head_sha }}
      - name: Post or update the comment
        env:
          PR: ${{ steps.pr.outputs.number }}
        run: |
          body=rendered/PR_COMMENT.md
          test "$(wc -c < "$body")" -le 65000
          test "$(head -n 1 "$body")" = '<!-- swiftproof:pr-comment:begin v1 -->'
          id=$(gh api "repos/$REPO/issues/$PR/comments" --paginate \
            --jq '[.[] | select(.user.login == "github-actions[bot]" and (.body | startswith("<!-- swiftproof:pr-comment:begin v1 -->")))][0].id')
          if [ -n "$id" ] && [ "$id" != null ]; then
            gh api -X PATCH "repos/$REPO/issues/comments/$id" -F body=@"$body"
          else
            gh pr comment "$PR" --repo "$REPO" --body-file "$body"
          fi
```

Every value taken from the triggering run (branch, owner, SHA, URL) reaches the scripts through environment variables, never through `${{ }}` inside a script. The comment step updates SwiftProof's own earlier comment, identified by the begin marker and the bot account, instead of adding one per run.
<!-- F9:end -->

## Runtime bounds, deadline and report size

Every sandbox run of a review is charged to one budget, `sandbox.max_runtime_seconds`, except dependency preparation, which is bounded by `prepare.timeout_seconds` (600 s by default) instead. The v0.4 stages have sub-caps inside that budget, never in addition to it: `fuzz.max_runtime_seconds`, `mutation.max_runtime_seconds`, and 180 s each for changed baseline tests and impacted tests. When a reviewer will run, half of the budget is reserved for its experiments: changed baseline tests, impacted tests, fuzzing and mutation stop launching once the time spent reaches `max_runtime_seconds` minus that reserve. The initial checks and coverage are not limited by the reserve. `--parallel` changes wall-clock time, not the budget: every launch reserves its timeout from what remains.

The worst-case wall-clock time of a review is approximately:

```text
T ≈ Git comparison + snapshots + prepare.timeout_seconds + S + reviewer.timeout_seconds
    + N_runs × 5 s (bounded container cleanup) + report write
```

S is the sandbox time spent before the reviewer. It never exceeds `max_runtime_seconds`, and it exceeds `max_runtime_seconds` minus the reserve only when the initial checks and coverage alone use more. The reviewer's own sandbox runs fall inside `reviewer.timeout_seconds`. With the defaults and a reviewer, T is 600 + 300 + 600 s plus overheads when the initial checks and coverage take less than 300 s, and at most 600 + 600 + 600 s plus overheads.

`--deadline D` (from 1m to 24h) bounds the whole command instead. The deadline counts from the start of the `review` command: preparation, sandbox runs and the reviewer stop at that start plus D minus 30 s, and the last 30 s are kept for cleanup and writing the report. The Git comparison, policy loading, static analysis and snapshot export are not interrupted, but the time they take counts against D. Running containers end as TIMEOUT, runs that had not started are recorded as SKIPPED ("Overall deadline reached; the run was not started."), and the report records the deadline as an unverified area, which is exit 2 with `--ci`. When sandbox runs happened, `execution.budget.deadline_reached` is true. Wall-clock time then stays within D plus up to 5 s of cleanup per run in flight, unless the Git comparison, static analysis and snapshot export alone take longer than D minus 30 s; they are bounded by their size limits. Reaching the deadline never produces exit 4 by itself, except that dependency preparation cut short fails, and failed preparation exits 4. Once a workflow is pinned to a v0.4 binary, set `--deadline` to the job's `timeout-minutes` minus 5 minutes, so that the job writes a report instead of being cancelled.

**Report size (known limit).** The JSON report keeps every recorded check log, including the mutation ledger. Each log is bounded by `sandbox.max_output_bytes` (64 KiB by default, 4 MiB at most) before JSON escaping, which can enlarge it up to six times. Structured results (Jest-compatible reports and fuzz observation streams) add at most 16 MiB per report; beyond that they are kept only as hashed artifacts. `swiftproof report` reads at most 64 MiB of JSON and exits 3 on a larger file, so a review with many checks and a raised `max_output_bytes`, such as a large mutation run, can write a report that `swiftproof report` cannot re-render. The files written by the review itself are not affected. The bound of roughly `max_output_bytes` × number of checks predates v0.4. With the default limits, the largest report the test suite builds (the structured-results budget filled by the streams of 16 fuzz functions, 200 mutants with one control run each, so 400 mutation logs, and 78 other checks, every log 64 KiB of `go test -json` lines) is 52.9 MiB and re-renders byte-identically (`TestLargestReportReRendersWithinTheInputLimit`); logs made mostly of characters that JSON escapes as six bytes (`<`, `>`, `&`) or a raised `max_output_bytes` can still exceed 64 MiB.

## Included GitHub workflows

`.github/workflows/pr-review.yml` starts an informational pilot on this
repository's PRs. `.github/workflows/review.yml` is reusable: it installs the
published v0.1.0 binary with a pinned checksum, preloads a trusted Docker image,
reviews immutable SHAs and retains reports and experiments for 30 days.
The workflow explicitly selects the reviewer flag for v0.1.0 compatibility.

After publishing these workflow files, another repository can use:

```yaml
name: SwiftProof
on: [pull_request]
permissions:
  contents: read
jobs:
  review:
    uses: gvinsot/SwiftProof/.github/workflows/review.yml@REPLACE_WITH_REVIEWED_COMMIT_SHA
    with:
      enforce: false
      reviewer: false
```

Replace the placeholder with a reviewed commit **containing this workflow**;
the existing v0.1.0 tag predates it. The example is not usable until then.
Supply `base-sha` and `head-sha` explicitly when calling outside a PR event.
For another language/dependency image, set `sandbox-image` to match the trusted
baseline policy's image. It must already contain the project's dependencies.

To investigate with a model on same-repository PRs, configure it in trusted
baseline policy, set `reviewer: true` and explicitly map the
`reviewer-api-key` secret. The workflow exports it as `SWIFTPROOF_API_KEY`.
Fork PRs force the investigator off. The repository pilot starts without a
provider. The PulsarCD provider bridge is a separate deployment integration;
GitHub-hosted runners do not automatically acquire its internal credentials.

A runner or container may instead supply the provider through the environment:
`SWIFTPROOF_REVIEWER_ENDPOINT` and `SWIFTPROOF_REVIEWER_MODEL` override the
policy values, and the key is taken from `SWIFTPROOF_API_KEY`, then
`SWIFTPROOF_API_KEY_FILE`, then the Docker secret mounted at
`/run/secrets/SWIFTPROOF_API_KEY`. A deployed model activates `review` exactly
as a policy model does, so keep these variables out of any job that runs
untrusted fork code. See [provider settings from the deployment](../README.md#provider-settings-from-the-deployment).

`enforce: true` fails on code 1 or operational/configuration failure. Code 2
produces a warning and still requires normal human PR approval: the check's
green status is not that approval. Configure required reviews, dismissal of
stale approvals and protection of workflow/policy changes in repository rules
before making this check mandatory. The workflow does not change those rules.

See the [agent loop](AGENT_WORKFLOW.md) and [PulsarCD integration](PULSARCD.md)
for checks before the PR and before production deployment.
