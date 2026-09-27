# Pre-change plans and scope drift

`swiftproof plan` is the pre-change counterpart of `review`. The configured provider simulates, **without modifying or running anything**, how it would implement an intent in the repository at the base commit, and submits a structured plan. SwiftProof then evaluates that plan with fixed, deterministic rules and writes `PLAN.json` and `PLAN.md`.

The model produces a plan; it never judges risk. Whether a plan touches critical parts, needs architecture changes or risks regressions is decided by SwiftProof from what the plan names, measured at the base commit.

The plan then becomes a contract. `review --plan` (or `lint --plan`) compares the real diff with it, deterministically: files changed outside the plan, exported signatures changed without being announced, critical paths touched although the plan did not list them, and dependency manifests changed without being declared. This is what turns an agent's announcement into something checkable: the agent says what it will do, and SwiftProof checks that it did what it said.

## Running a plan

```sh
swiftproof plan --intent-file demande.md            # writes .swiftproof/PLAN.json and PLAN.md
swiftproof plan --intent-file demande.md --base origin/main --ci
```

| Flag | Meaning |
| --- | --- |
| `--intent-file FILE` / `--intent TEXT` | The change to plan (required; at most 64 KiB, UTF-8, read like the review intent: PR-comment blocks removed, secrets redacted, criteria extracted). |
| `--base REF` | The revision the plan starts from (default `main`). Its tip supplies the trusted policy, as for `review`. |
| `--config FILE` | Explicit trusted local policy. |
| `--out DIR` | Output directory, relative to the repository (default `.swiftproof`). |
| `--ci` | Exit 2 when a category is flagged or something was left unverified. |
| `--max-iterations N` | Override the provider iteration budget. |

A provider is **mandatory**: `reviewer.model` in the trusted policy or `SWIFTPROOF_REVIEWER_MODEL`, with the endpoint and credential resolved exactly as for `review` (see the main README). Without one, or with `--reviewer=false`, `plan` exits 3 and writes nothing.

Exit codes: 0 (no category flagged, or no `--ci`), 2 (with `--ci`: a category is flagged or something is unverified), 3 (usage or configuration), 4 (operational: provider failure, or the model ended without an accepted plan).

## What the model can do

The planner receives the intent, its extracted criteria, the base ref and commit, the policy language and up to 400 repository paths (sensitive paths excluded). Its tools are read-only and operate on a snapshot of the base commit:

- `list_files(prefix?)`, `read_file`, `search_code`;
- `find_references`, `inspect_symbol`, `find_callers`, answered by the same static index as [impact analysis](IMPACT.md), built over the whole base tree;
- `submit_plan`, called once.

No tool writes, executes, builds or reaches the network: the harness behind the planner has no sandbox image and no command, and any other tool name is refused and audited as a rejected call. Repository text, tool output and the intent are untrusted data, and the prompt says so. Requests go through the same bounded Chat Completions client as the reviewer (HTTPS, or HTTP on loopback only; no proxy; no redirect; redaction of every message).

`submit_plan` is validated strictly (unknown fields refused); an invalid plan is returned to the model as a tool error so it can correct it:

- `files`: 1 to 500 entries `{path, change, old_path?, reason?}`; `change` is `add`, `modify`, `delete` or `rename` (`old_path` only for a rename). Paths are clean, repository-relative and portable, and each path appears once.
- `symbols`: up to 500 entries `{path, name, change, reason?}`; `name` is `F` or `T.M` as declared (pointer notation is removed), `change` is `add`, `body`, `signature` or `remove`, and `path` must be a planned file. A file the plan adds only receives added symbols; a file it deletes only loses symbols.
- `dependencies`: up to 100 entries `{manifest, name, change, version?}` whose `manifest` is a planned file; `change` is `add`, `upgrade` or `remove`.
- `summary`, `steps`, `assumptions`: prose, recorded as model-written.

## Deterministic assessment

Every rule reads the plan and the base commit only. Each raises a plan signal; a category is **flagged** when one of its signals is medium or higher, and the plan is **major** when a category is flagged.

| Category | Signal | Severity | Rule |
| --- | --- | --- | --- |
| Critical parts | `plan_critical_path` | high | A planned path (or rename source) matches a policy `sensitive_paths` glob, with the linter's glob semantics. |
| Critical parts | `plan_sensitive_symbol` | high | A changed or removed symbol's name matches the linter's authentication/authorization or payment name heuristic. |
| Architecture | `plan_exported_signature` | high | An exported Go symbol (by its final identifier) is planned with `signature` or `remove`. |
| Architecture | `plan_dependency_change` | medium | A planned file is a dependency manifest or lock file (`go.mod`, `go.sum`, `go.work`, `package.json`, lock files, `Cargo.toml`, `pyproject.toml`, `requirements*.txt`, ...). |
| Architecture | `plan_new_package` | medium | A non-test Go file is added in a directory with no non-test Go file at the base commit. |
| Regression risk | `plan_wide_impact` | medium | A changed existing symbol has 10 or more reference sites in unchanged non-test code. |
| Regression risk | `plan_untested_impact` | medium | A changed existing symbol has reference sites and no test reaching it within 3 references. |
| Other major change | `plan_file_deletion` | medium | The plan deletes a file. |
| Other major change | `plan_large_scope` | medium | The plan names 20 or more files. |
| Other major change | `plan_inconsistent` | low | The plan does not match the base commit: a modified, deleted or renamed file is absent, an added file already exists, or a changed function or method is not in the index of its file. |
| Other major change | `plan_unmeasured` | low | A planned symbol could not be measured (no index for that file, or a file kind the index does not read). |

Callers and reaching tests come from the static index of the base commit (Go type-checked, TypeScript/JavaScript, Python and Rust lexical) with the bounds of [impact analysis](IMPACT.md): they are approximate, an empty list is not proof of absence, and a measure whose search stopped at a bound is marked incomplete (`+` in `PLAN.md`). Types, variables and constants are not indexed, so they are never measured. Thresholds are constants recorded in `assessment.thresholds`.

`PLAN.json` (schema: [`schema/plan.schema.json`](../schema/plan.schema.json)) holds the redacted intent and its SHA-256, the base commit, the policy source, the planner model, the `proposal` (model-written, untrusted), the `assessment`, the `contract`, Unverified notes, the audit of the session and the exit code. `PLAN.md` renders the same content; the proposal is labelled "model-written, not evidence". `swiftproof plan` recomputes categories, the major flag and the exit code from the signals before writing.

A plan that raises nothing is not proof that the change is safe, and a flagged plan is not a defect: it names what a human should look at before the work starts.

## Scope drift: `review --plan` and `lint --plan`

```sh
swiftproof review --base origin/main --plan .swiftproof/PLAN.json --ci
swiftproof lint   --base origin/main --plan .swiftproof/PLAN.json
```

`--plan` reads the `contract` of a PLAN.json (at most 4 MiB, format `swiftproof-plan` version 1, with a base commit and planned files; anything else exits 3 before Git analysis). The report gains a `plan_drift` section, and every difference that points into the diff is also added to the linter list as a `plan_drift` signal, located on the file's first changed line, so tools that render signals, such as the hub, show it without recomputing anything.

| Kind | Severity | When |
| --- | --- | --- |
| `unplanned_file` | medium | A changed file whose path (and rename source) the contract does not list. |
| `unplanned_test_file` | low | The same, for a test file (linter test-name rules). |
| `unannounced_exported_change` | high | The linter's `public_api_change` signal reports an exported Go declaration **changed** or **removed**, and the contract does not announce that name in that file (or its rename source) as `signature` or `remove`. Added exported declarations are not drift. |
| `unannounced_critical_path` | high | A changed path matches a critical glob of **this review's** trusted policy and the plan did not list it as critical at plan time. |
| `unannounced_dependency_change` | medium | A changed dependency manifest the contract does not list. |
| `planned_file_untouched` | low | A planned path the change does not touch (section only, no signal). |

The status is `drifted` when an item is medium or higher, `conforming` otherwise. A drifted change, and a plan made against another base commit than the one the review compares from (`base_matches: false`, with an Unverified note), request human review: exit 2 with `--ci`, never exit 1.

`report.Finalize` recomputes the items, the status, `base_matches` and the `plan_drift` signals from the recorded contract, critical globs, diff and `public_api_change` signals. `swiftproof report` therefore corrects a saved report whose section was edited or emptied. The section records `plan_sha256`, the SHA-256 of the PLAN.json file that was read, and the plan's `intent_sha256`: compare them with the plan that was approved, since the file itself lives in the working tree and could be regenerated.

Drift checks that the change stays within what was announced. It does not check that the change implements the intent, that the plan was a good one, or that planned files were changed correctly.

## The plan gate: when a human review is not required

The point of planning first is to know the impact of a change **before** it is written, so that a predictable, low-risk change does not need a human to read its diff. `review --plan` ends with a **plan gate**, a decision recorded in `plan_drift.decision`, printed on stdout and at the top of the Plan Conformance section:

- `no_human_review_required` only when **all** of the following hold:
  1. **Low-risk plan.** The review re-assesses the plan itself: it re-applies the fixed rules to the plan's proposal at the plan's base commit, with the sensitive paths of **this review's** trusted policy, and no category is flagged. The flags and the contract stored in PLAN.json are never trusted: the contract is re-derived from the proposal, so a plan edited after planning cannot widen the scope without the wider scope being assessed.
  2. **Fully measured.** No planned symbol is left unmeasured, the plan matches its base commit, the static index of the base commit is complete, and every planned symbol's callers and tests were fully searched (`assessment.gaps` is empty). An unknown risk is not a low risk.
  3. **Conforming change.** The diff stays within the plan (status `conforming`) and the plan was made against the base the review compares from.
  4. **Checks passed, nothing else to look at.** At least one check ran and every check passed, no issue was reproduced, no high or critical signal was raised, nothing is unverified, and no other section of the report (divergences, failing impacted or baseline tests, fuzzing, mutation) requests review.
- `human_review_required` otherwise, with every reason listed in `plan_drift.decision_reasons`.

With `--ci`, the decision is the exit code: `review --plan --ci` exits 0 only when the gate lets the change through, and 2 when a human must review it (1 still means a reproduced high or critical issue). `lint --plan` runs no check, so its gate always requires review; use it in the agent loop, and `review --plan` for the merge decision. `report` recomputes the gate from the recorded report, so an edited decision is corrected on re-render.

The gate is a **process** decision, not a verdict on correctness: it says the change did what a low-risk, measured plan announced and that the automated checks found nothing to look at. It does not say that the change implements the intent or is free of defects. Keep critical paths in `sensitive_paths`, keep meaningful tests in the policy's commands, and let a human validate every plan that raises a category.

### The process

1. **Plan and check the intent**: `swiftproof plan --intent-file task.md --ci`. Exit 0: no category flagged. Exit 2: a human validates the plan (or the task is split or reworded) before any code is written.
2. **Implement the plan**: an agent codes the plan, and only the plan.
3. **Check conformance**: `swiftproof review --base origin/main --plan .swiftproof/PLAN.json --ci` runs the checks and the gate.
4. **Merge or review**: exit 0 lets the pipeline merge without a human review; exit 2 requests a human review of the pull request, with the reasons in the report.

See [CI integration](CI.md#merging-without-review-behind-the-plan-gate) for a workflow that applies the decision.

## Agent loop

1. `swiftproof plan --intent-file task.md` before writing code; read `PLAN.md`, and raise a flagged category with a human when the task requires it.
2. Implement, commit.
3. `swiftproof lint --plan .swiftproof/PLAN.json` (and `review --plan` before PR review); report every drift item in the handoff, and either explain it or update the plan and have it approved again.
