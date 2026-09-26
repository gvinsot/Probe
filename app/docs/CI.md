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
<!-- F8:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F3:begin -->
<!-- F3:end -->

<!-- F6:begin -->
<!-- F6:end -->

<!-- F2:begin -->
<!-- F2:end -->

<!-- F4:begin -->
<!-- F4:end -->

<!-- F1:begin -->
<!-- F1:end -->

<!-- F5:begin -->
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

**Report size (known limit).** The JSON report keeps every recorded check log, including the mutation ledger. Each log is bounded by `sandbox.max_output_bytes` (64 KiB by default, 4 MiB at most) before JSON escaping, which can enlarge it up to six times. Structured results (Jest-compatible reports and fuzz observation streams) add at most 16 MiB per report; beyond that they are kept only as hashed artifacts. `swiftproof report` reads at most 64 MiB of JSON and exits 3 on a larger file, so a review with many checks and a raised `max_output_bytes`, such as a large mutation run, can write a report that `swiftproof report` cannot re-render. The files written by the review itself are not affected. The bound of roughly `max_output_bytes` × number of checks predates v0.4.

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
