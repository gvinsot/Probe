# Evidence-only exports: SARIF and PR comment (`--format sarif,pr-comment`)

Probe can render two extra files from a finalized report: `confidence-report.sarif`, a SARIF 2.1.0 log for code-scanning tools, and `PR_COMMENT.md`, a pull-request comment. Both list only **findings backed by recorded sandbox evidence**. A model's assertion, a risk signal, a review range or a coverage observation never becomes a finding, and an empty list of findings is not an approval: **No finding is not approval.**

The exports are opt-in, add no policy key, and change nothing else: the exit code, every status and the Markdown and JSON reports are identical with or without them. Probe never publishes them. It makes no network request, contains no GitHub API client, and never posts a comment or uploads a SARIF file; a separate CI job may do that with its own token (see [Publishing](#publishing) and [CI integration](CI.md)).

## Enabling them

```sh
probe review --base main --format markdown,json,sarif,pr-comment --report-url "$RUN_URL"
probe lint   --base main --format sarif,pr-comment
probe report --input .probe/confidence-report.json --format sarif,pr-comment
```

| Format | File | Content |
|---|---|---|
| `markdown` | `CONFIDENCE_REPORT.md` | The full report (default). |
| `json` | `confidence-report.json` | The full report (default). |
| `sarif` | `confidence-report.sarif` | Evidence-backed findings with a location in the changed files, plus notifications. |
| `pr-comment` | `PR_COMMENT.md` | Status block, then every evidence-backed finding. |
| `pr-summary` | `PR_SUMMARY.md` | Not an evidence-only export: the AI-written pull request summary, model output, written with `markdown` whenever a summary exists ([PR summary](PR_SUMMARY.md)). |

- `--format` takes a comma-separated list; the default stays `markdown,json`. An unknown value exits 3 before anything runs or is written.
- Every requested format is rendered in memory before any file is written, and each file is replaced atomically. A render error writes nothing.
- `--report-url URL` (review, lint and report) adds one link to the full report in `PR_COMMENT.md`. It exits 3 unless `pr-comment` is requested and the URL is `https`, names a host, has no user information, is at most 512 bytes long and uses only the characters `A-Z a-z 0-9 . _ ~ : / ? # ! $ & * + , ; = % -`. Parentheses, brackets, quotes, backslashes, backticks, spaces, angle brackets and `@` are refused, so the URL cannot close the Markdown link or start HTML. The URL is the one string of the exports that the report's sanitizing never sees, so a URL that appears to carry a credential is refused as well: a private-key header, one of the token shapes of the credential redaction (`sk-`, `ghp_` and the other GitHub prefixes, `github_pat_`, `AKIA`, a JSON Web Token or a bearer token) starting at a token boundary, that is at the start of the URL or after a character other than a letter, digit, `_` or `-`, or a query, fragment or path parameter (`name=value` or `name:value`) whose name reads as a credential, such as `token`, `access_token`, `client_secret`, `password`, `api-key`, `key`, `sig` or `X-Amz-Signature`. Both checks also run on the percent-decoded URL. A token prefix inside a word, as in `flask-sqlalchemy` or `task-scheduler`, is not refused. Probe never fetches the URL.
- `probe report` re-renders a saved report: `Finalize` runs first, so every finding is re-derived from the recorded checks. Re-rendering shows consistency, not authenticity.
- The formats `sarif` and `pr-comment` and the flag `--report-url` make a binary older than v0.4.0 exit 3 while parsing arguments. Re-pin a workflow to a v0.4 release before passing them (see [CI integration](CI.md#release-ordering-for-v04)).

## What can be a finding

A finding is read from the fields `Finalize` derives, and it is kept only when all of these hold:

- its record has the one status of its class, and every cited evidence record re-derives, from the recorded checks and at render time, to that status (a surviving mutant re-derives through the mutation verifier instead);
- it cites at least one check, and every cited check ID resolves exactly once in `checks` together with `mutation.checks` (an ID in both ledgers resolves to nothing);
- every cited evidence ID resolves exactly once in `evidence`.

A persisted status alone is never enough: removing the checks or evidence from a saved report, or writing a status without them, removes the finding.

| Order | Class | Rule ID | SARIF level | Read from | What it records |
|---|---|---|---|---|---|
| 1 | `reproduced` | `probe/reproduced` | `error` when the hypothesis is high or critical, else `warning` | `reproduced_issues` and their `differential_test` evidence | A model-written generated test passed on a live baseline run and failed on the candidate under the same command. |
| 2 | `base_test_fails_on_candidate` | `probe/base-test-fails-on-candidate` | `warning` | `base_tests.tests[]` with `FAILS_ON_CANDIDATE` | The baseline version of a test the change edited passed on the baseline and failed on candidate code. |
| 3 | `impacted_test_fails_on_candidate` | `probe/impacted-test-fails-on-candidate` | `warning` | `impact.changed_functions[].tests[]` with `FAILS_ON_CANDIDATE` | An unchanged test that the approximate static index links to a changed function passed on the baseline and failed on the candidate. |
| 4 | `fuzz_divergence` | `probe/fuzz-divergence` | `warning` | `divergences[]` of kind `differential_fuzz` | Identical seeded inputs gave different recorded values on the two revisions, confirmed by a second pair. |
| 5 | `observed_divergence` | `probe/observed-divergence` | `warning` | `divergences[]` of kind `differential_observation` | A generated test recorded different values on the two revisions, after two agreeing baseline runs. |
| 6 | `intent_test_failed` | `probe/intent-test-failed` | `note` | `intent_test_failures` and their `intent_test` evidence | A model-written test for one acceptance criterion failed on the candidate only. |
| 7 | `surviving_mutant` | `probe/surviving-mutant` | `note` | `mutation.mutants[]` with `SURVIVED` | The package's tests passed with a single change to an added line, as they did without it. |

What each class does **not** establish:

- `reproduced`: not a confirmed defect, and not proof that the test's assertion is the intended behavior. The severity, and therefore the `error` level, is the reviewer model's classification. `error` is used only for a reproduced high or critical hypothesis, the one case that sets exit code 1. The two do not always appear together: after an operational failure the recorded exit code is 4 while such a result keeps level `error`, and a reproduced hypothesis without a location in the changed files is only a notification, so exit code 1 can come with no `error` result.
- `base_test_fails_on_candidate`: not a reproduced issue; the change may intend a different outcome, and it does not show that the test edit is wrong or deliberate.
- `impacted_test_fails_on_candidate`: not caused by the linked function as far as Probe knows; the static link is approximate and the failure may come from any part of the change or from flakiness.
- `fuzz_divergence` and `observed_divergence`: a difference between recorded values for recorded inputs, never which revision is correct and never a defect report. Values are bounded, redacted serializations. For observations, the reviewer model chose the inputs.
- `intent_test_failed`: each finding opens with the fixed text "A model-written test for AC-N failed on the candidate; there is no baseline control, and the test or its reading of the criterion may be wrong.", followed by the retained intent test's sha256. It is not a reproduction and does not show that the change departs from the intent.
- `surviving_mutant`: the mutant may be equivalent to the original code and other packages were not run; it is not a defect, dead code or a missing test by itself. No mutation score is computed.

## What is never a finding

Risk signals of every kind (including `uncovered_change`, the lexical `test_*` heuristics, `impacted_caller`, `analysis_limited`, `surviving_mutant` signals and `prepare_input_changed`), review targets, coverage records, impact and index records, prepare and cache records, checks by themselves (a failing `test` check is not a differential result), killed, invalid, timed-out, inconclusive and not-run mutants, `NOT_REPRODUCED`, `NOT_DIVERGED`, `PASSES_ON_CANDIDATE` and `INTENT_TEST_PASSED` records, `UNVERIFIED` and `DISMISSED` hypotheses, and every model judgment. `intent_judgment` is never read or rendered. Unverified areas, checks that did not pass and stages that did not run are reported as status, never as findings.

## Locations

- A location is emitted only when its path is a changed, non-deleted, non-binary file of the recorded change.
- **Model-chosen locations** (the path and line of a reproduced or intent-test hypothesis, and the hypothesis anchor of an observed divergence) are labelled "model-chosen location". Their line is kept only when the recorded diff has that line on the candidate side (an added or context line). Otherwise the location is file-level: SARIF uses line 1 with `location_precision` `file`, and both files say that the model-chosen line is not a candidate-side line of the recorded diff.
- **Deterministic locations** keep their lines: the changed function of a fuzz divergence, the candidate range of a changed baseline test, and the mutated added line.
- A finding without such a location is **unanchored**: it is listed in the PR comment ("Location: none in the changed files") and reported in SARIF as a notification, not as a result. This covers a changed baseline test that no longer exists on the candidate side and every impacted test, whose declaration is in an unchanged file (it is shown as a detail).
- A reproduced or observed-divergence finding links the retained `generated_test` artifact, and an intent-test finding the retained `intent_test` artifact, when exactly one retained artifact of that kind carries the test's file name; its sha256 is in the finding. A surviving mutant links its `mutant_patch` artifact by hash. Artifact paths are relative to the report directory.

## SARIF file

`confidence-report.sarif` is a minimal SARIF 2.1.0 log (`$schema` `https://json.schemastore.org/sarif-2.1.0.json`) with one run:

- `tool.driver`: name `Probe`, the tool version (and `semanticVersion` for a `vMAJOR.MINOR.PATCH` version), `informationUri` `https://github.com/gvinsot/Probe`, and one rule per class that has a result, in class order. Each rule has a short and full description, a help text stating what the evidence does not establish, a default level and the tags `probe` and the class.
- `results`: the anchored findings, in class order, then severity, path and line, at most 1000 (the rest are counted). Each result has `ruleId`, `ruleIndex`, `level`, a plain-text `message.text` (never `message.markdown`), exactly one location (a percent-encoded, repository-relative URI with `uriBaseId` `%SRCROOT%`, and a region with `startLine` and `endLine`), `partialFingerprints` `probe/v1`, optional `relatedLocations` for retained artifacts (`uriBaseId` `PROBE_REPORT`, the report directory), and `properties.probe`: class, status, severity and title with their source ("reviewer model") where they exist, location source and precision, evidence, check and hypothesis IDs, replayed checks, criterion or mutant ID, labelled details, retained artifacts with their sha256, and a `truncated` flag. `results` is `[]` when there is none, never absent.
- `invocations[0]`: `exitCode`, and `executionSuccessful`, which is `false` when the exit code is 4, when a check in `checks` is `ERROR`, `SKIPPED` or `TIMEOUT`, or when `execution.budget.deadline_reached` is true. `toolExecutionNotifications` (level `warning`, each with `properties.probe_kind`) list, in this order: an operational failure (`operational_failure`); "No checks or experiments ran, so the absence of findings carries no information." (`no_execution`); each stage that did not run, with the reason (`stage_not_run`: prepare `failed` or `not_permitted`, and `base_tests`, `impact.tests_status`, `fuzz` or `mutation` `not_run`); findings omitted by the result cap (`omitted_findings`); each unanchored finding (`unanchored_finding`); each check in `checks` that did not pass (`check_not_passed`); each unverified area (`unverified_area`), by its position only ("Unverified area 2 of 3."), because a recorded note can quote reviewer-model output such as a rejected tool name or a generated test title; and each hypothesis that stayed `UNVERIFIED` (`unverified_hypothesis`), by its ID only, with the fixed text "is UNVERIFIED: Probe accepted no evidence-backed status for it.". The text of the notes and the reviewer model's title and rationale of an `UNVERIFIED` hypothesis are never exported; they stay in `confidence-report.json` and `CONFIDENCE_REPORT.md`. At most 100 notifications are listed: past that, the last one (`omitted_notifications`) counts the rest. The order puts the per-check, per-area and per-hypothesis items last, so they are the ones a cut drops, and the run properties below count every kind.
- `properties.probe` of the run: the report version, tool version, `generated_at` copied from the report, refs and commits, the policy source and commit, the exit code, and counts (findings, unanchored and omitted findings, unverified areas and hypotheses, checks that did not pass, stages that did not run, risk signals, review targets), plus the note: "Only evidence-backed findings are listed as results. An empty result list is not an approval and does not establish that the change is correct; GitHub's fixed state of an alert is not a Probe claim. A re-rendered or fork-produced file is not authenticated."
- The log's `properties.attribution` is "Generated by Probe VERSION (https://github.com/gvinsot/Probe)." (NOTICE, section 7(b)).

No rule or result carries `precision`, `security-severity`, a confidence, a rank or any percentage, and `kind` is never `pass`, `informational` or `notApplicable`.

**Messages.** A message starts with the class sentence. Every untrusted fragment (model titles, test names, values, IDs) follows a constant label in double quotes; control and bidirectional characters become spaces, `[` and `]` are escaped so they cannot form a SARIF embedded link, double quotes inside a fragment become single quotes, and `://` and `www.` get a zero-width space (U+200B) so viewers do not link them. A message is at most 1024 bytes; titles and values are cut at 256 bytes, reasons at 512.

**Fingerprints.** `probe/v1` is the first 32 hex digits of the SHA-256 of the class, the anchored path and a line-independent identity (the test file and names, the function, the criterion, or the mutated text), with an occurrence number when several findings share them. A finding keeps its fingerprint when unrelated lines move. The identity of a reproduced, observed or intent finding comes from a model-written test file and names, so a later run whose model writes another test produces another fingerprint.

## PR comment

`PR_COMMENT.md` is GitHub-flavored Markdown:

1. The first line is `<!-- probe:pr-comment:begin v1 -->` and the last is `<!-- probe:pr-comment:end -->`, the only raw HTML in the file. When the comment is quoted into text later used as an intent, Probe removes everything between the markers from the intent, with a recorded note ([intent criteria](INTENT.md)).
2. A heading: `## Probe: N evidence-backed findings`, or `## Probe: no evidence-backed finding recorded`.
3. **Status (not a finding)**: the exit code with a fixed explanation; the number of unverified areas (recorded notes plus hypotheses that stayed `UNVERIFIED`); the checks in `checks` that did not pass (the mutation ledger is not counted, since mutants are expected to fail); the stages that did not run, with their reasons; "No checks or experiments ran, so the absence of findings carries no information." when nothing ran; the numbers of risk signals and review ranges, which are not findings; and "No finding is not approval.".
4. One group per class, in class order, each with a fixed caveat, then one item per finding: severity (reproduced only, "assigned by the reviewer model"), the fixed intent-test text, the model's title labelled as such, the location, evidence, check and hypothesis IDs, labelled details and retained artifacts.
5. When findings do not fit, the fixed pointer "N further evidence-backed findings are not shown in this comment, which lists at most 50 findings in at most 60000 bytes; the underlying records are in confidence-report.json.". The whole file is at most 60 000 bytes (GitHub accepts 65 536 characters).
6. The link to the full report (`--report-url`), or the file names when there is no URL, then `---` and the attribution line of `CONFIDENCE_REPORT.md`.

**Escaping.** Every repository, model or report string, including IDs read back from a re-rendered report, goes through the comment escaper:

- control and bidirectional characters become spaces;
- a zero-width space (U+200B) follows `@`, the `:` of `://`, the `www` of `www.`, a `:` before a letter, digit, `_`, `+` or `-`, a `#` before a digit, and the `-` of `GH-` before a digit, so that mentions, URL autolinks, emoji shortcodes and `#12` or `GH-12` issue references stay inert;
- then `&`, `<` and `>` are HTML-escaped and Markdown punctuation is backslash-escaped, as in `CONFIDENCE_REPORT.md`.

Like `CONFIDENCE_REPORT.md`, the comment keeps quotes as they are. Their numeric entities would display as `&#39;` and `&#34;` and would put a `#` before a digit back into the text, and a quote starts nothing once `<` and the brackets and parentheses of a link are escaped. No `#` in the comment's untrusted text is directly followed by a digit. Titles and values are cut at 256 bytes, reasons at 512. The only link is the validated `--report-url`, besides the constant `https://github.com/gvinsot/Probe` of the attribution notice. Two autolink forms are **not** neutralized: bare commit SHAs, which GitHub links to commits of the same repository, and the custom autolink references that a repository configures for itself (a prefix such as `TICKET-` followed by a number).

## Publishing

Probe renders; a CI job may publish. [CI integration](CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment) has a workflow sketch. Its rules:

- **Upload SARIF only when the exit code is 0, 1 or 2, `executionSuccessful` is true, and no notification of kind `no_execution`, `stage_not_run` or `omitted_findings` is present.** Code scanning closes an earlier alert as "fixed" when a later upload of the same category lacks it. The gate skips the uploads where the report itself records that execution failed, did not happen or was cut: a failed run; a run where nothing executed (`lint`, or `review` without checks and reviewer, whose `executionSuccessful` is still true because nothing failed); a run where a configured stage did not run; and a run where the 1000-result cap cut findings. **It protects only against these cases**; an upload that passes it can still close alerts that nothing re-examined:
  - Reproduced, observed-divergence and intent-test findings come from the reviewer model's experiments, so a run has them only for what the model chose to test in that run. Any upload, after a complete reviewer run as well as after one that stopped early, may close earlier alerts of these classes. A reviewer that stopped early ("Reviewer incomplete: …", an exhausted iteration, tool-call or input budget) shows only as an `unverified_area` notification and passes the gate.
  - A stage that ran but did not finish every item (for example a mutation section `incomplete`) is not a stage that did not run.

  A stricter gate also requires `.runs[0].properties.probe.unverified_areas` to be 0; that count includes every recorded unverified area and `UNVERIFIED` hypothesis, also those the notification cap leaves out. It skips the early-stopped reviewer runs, and more, but it does not turn an alert's "fixed" state into a re-examination. Upload only `review` output and keep one category per workflow; `lint` output never has results. GitHub's "fixed" state is never a Probe claim, and "no new alerts" is not an approval: keep branch protection and human review.
- **Post `PR_COMMENT.md` as a comment, never into the pull-request description.** The description is the usual `--intent-file` source, so feeding a report back into it would turn report text into intent; Probe strips its own marked block anyway.
- **Treat artifacts from fork runs as attacker-controlled.** A pull-request workflow runs the workflow definition of the pull request itself, so its JSON, `PR_COMMENT.md` and SARIF may be forged. The publisher job, triggered by `workflow_run`: never checks out or executes pull-request code; downloads only `confidence-report.json` and refuses one larger than 64 MiB; re-renders it with **its own pinned binary** (`probe report --format sarif,pr-comment`) and never posts artifact Markdown or SARIF verbatim; has only `pull-requests: write` for the comment, `security-events: write` for SARIF and `actions: read` to download the artifact; and posts only to the pull request that triggered the run, after matching the head SHA recorded in the JSON.
- Re-rendering makes the export consistent with the JSON; it does not authenticate the JSON. Exports from fork runs may be forged.

## Exit codes

Rendering has no effect on the exit code. An unknown format, an invalid `--report-url`, or `--report-url` without `pr-comment` exits 3 before any report is written or any container starts. A report write failure exits 4, as before.

## Security notes

Findings come only from `Finalize`-derived records whose evidence re-derives from recorded checks: a model's asserted status, a persisted evidence status, a signal or a coverage record cannot create one. A prompt-injected model cannot aim an alert at an arbitrary file, because every location must be a changed, non-deleted file of the recorded change, and a model-chosen line outside the recorded diff becomes file-level. Untrusted text is neutralized as described above. The files are unsigned re-renderings; provenance comes from the storage of the run that produced the JSON. See [security boundaries](SECURITY.md).

## Limitations

- GitHub acceptance is not exercised by Probe's validation: no upload and no comment was posted. The SARIF files of the validation runs validate against the SARIF 2.1.0 JSON schema.
- A file-level location uses line 1. An unanchored finding appears only as a SARIF notification.
- A retained test is linked only when exactly one retained artifact of its kind carries the test's file name.
- Alert identity for reproduced, observed and intent findings follows model-written test names, so alerts can churn between runs.
- A zero-width space is inserted after `:` before a digit, so `file.go:12` inside a value contains one; it is invisible.
- Bare commit SHAs and repository-configured custom autolinks in untrusted text can still become links in the PR comment (see Escaping).
- The `--report-url` credential check is a heuristic in both directions. A URL whose path segment starts with a token shape, such as a repository named `sk-` followed by eight or more letters, digits, `_` or `-`, is refused with exit 3; a secret in a form it does not know is not detected.
- `executionSuccessful` follows the contract's definition: it is `true` for a run in which nothing executed. The `no_execution` notification says so, and the upload rule above checks for it.

## Example

From a validation run of the second F9 fixer round's binary (a scripted provider, `review --checks=false --ci --format markdown,json,sarif,pr-comment --report-url https://example.invalid/runs/1`, exit 1), the SARIF result:

```json
{
  "ruleId": "probe/reproduced",
  "level": "error",
  "message": {
    "text": "A generated test passed on the baseline and failed on the candidate. Severity high was assigned by the reviewer model. Reviewer-model title: \"Guest is allowed through authorization\". Generated test: \"probe_guest_test.go (TestProbeRejectGuest)\". Runs: \"baseline check check-1 PASS; candidate check check-2 FAIL (go_test_json)\". Evidence: \"evidence-1\". Location: model-chosen location."
  },
  "locations": [
    {
      "physicalLocation": {
        "artifactLocation": {
          "uri": "auth.go",
          "uriBaseId": "%SRCROOT%"
        },
        "region": {
          "startLine": 3,
          "endLine": 3
        }
      }
    }
  ],
  "partialFingerprints": {
    "probe/v1": "988e3b7852e95cf6bca55c9308372933"
  }
}
```

and the PR comment:

```markdown
<!-- probe:pr-comment:begin v1 -->
## Probe: 1 evidence-backed finding

**Status (not a finding)**

- Exit code 1: a reproduced high or critical hypothesis was recorded.
- Unverified areas: 0 (0 recorded notes and 0 hypotheses that stayed UNVERIFIED).
- Checks that did not pass: 1 (the mutation ledger is not counted): check-2 generated\_test\_candidate FAIL.
- Stages that did not run: 0.
- Not findings, listed only in the full report: 1 risk signal and 2 suggested review ranges.
- No finding is not approval.

Only findings backed by recorded sandbox evidence are listed; signals, review ranges, unverified hypotheses and model judgments are never findings.

### Reproduced hypotheses (1)

A generated test passed on the baseline and failed on the candidate. This does not confirm a defect: a human judges whether the test's assertion is the intended behavior.

- **high** severity, assigned by the reviewer model. Reviewer-model title: Guest is allowed through authorization. Location: auth.go:3 (model-chosen location).
  - Evidence: evidence-1. Checks: check-1, check-2.
  - Hypotheses: hypothesis-1.
  - Generated test: probe\_guest\_test.go \(TestProbeRejectGuest\)
  - Runs: baseline check check-1 PASS; candidate check check-2 FAIL \(go\_test\_json\)
  - Retained generated\_test artifact: sha256 7174f65b42a35aea0a9a7f419396d660c2549859e3f9414d4beb3e7587fb108b (artifacts/92a7ee90d94274046c70de50-generated-test-1-probe\_guest\_test.go)

Full report: [CONFIDENCE\_REPORT.md and confidence-report.json](https://example.invalid/runs/1)

---

Generated by Probe F9fix2-e2e (https://github.com/gvinsot/Probe).
<!-- probe:pr-comment:end -->
```

The same review with a provider that cites a nonexistent evidence ID, under a title worded as an approval, exits 2. It writes `"results": []` with the notifications "No checks or experiments ran, so the absence of findings carries no information." and "Hypothesis "hypothesis-1" is UNVERIFIED: Probe accepted no evidence-backed status for it. The reviewer model's text is not exported; the record is in confidence-report.json.", and a comment headed "Probe: no evidence-backed finding recorded" that counts one hypothesis that stayed `UNVERIFIED`. The model's title appears in neither file, and the upload gate of [CI integration](CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment) skips the upload because of the `no_execution` notification.

A review whose reviewer creates and runs its generated test and then stops on a provider error exits 2 with no result: the report records the two generated-test checks and the unverified area "Reviewer incomplete: reviewer endpoint returned HTTP 500", which the SARIF file lists only as "Unverified area 1 of 1. Its text can quote reviewer-model output, so it is not exported; the record is in confidence-report.json (unverified).". `executionSuccessful` is true and no `no_execution`, `stage_not_run` or `omitted_findings` notification is present, so the upload gate of the sketch says to upload; the stricter `unverified_areas == 0` condition says to skip.

## See also

- The pages of the seven classes: [reproduced issues](../README.md#optional-ai-investigation), [changed baseline tests](BASE_TESTS.md), [impacted tests](IMPACT.md#impacted-tests---impacted-tests), [differential fuzzing](FUZZ.md), [observation experiments](OBSERVATIONS.md), [intent criteria](INTENT.md) and [mutation of added lines](MUTATION.md).
- [Agent workflow](AGENT_WORKFLOW.md): post the comment as a comment, never into the PR description.
