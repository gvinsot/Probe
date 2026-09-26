# SwiftProof V0.4 — More deterministic evidence, less model dependence

**Status:** contract for the v0.4 release (unreleased). It extends the [V0.2 specification](swiftproof-v0.2-spec.md).  
**Implementation:** portable Go CLI, standard library only.  
**Outputs:** `CONFIDENCE_REPORT.md`, `confidence-report.json`, optionally `confidence-report.sarif` and `PR_COMMENT.md`, and retained experiment artifacts.

## 1. Scope and relation to v0.2

### 1.1 Relation

v0.2 stays binding except the refinements in §2. §2 is the complete list of refinements: every v0.2 MUST / MUST NOT that it does not name stays binding unchanged.

The product principles are unchanged:

- There are no confidence percentages and no automatic approval.
- Model claims stay hypotheses until harness evidence supports them. Models cannot create evidence records or check outcomes.
- Policy comes from the tip of the base branch.
- Repository code runs only in the isolated sandbox. There is no host-execution fallback.
- Missing evidence means `UNVERIFIED`. "Executed" never means "tested".

The v0.4 thesis is **more deterministic evidence, less dependence on a model, and no unproven comments**. Every v0.4 record adds evidence or review requests. None of them deletes a signal, lowers a severity, supports a dismissal or produces exit code 1.

### 1.2 Delivered parts

| Part | What it adds | Enabled by | Rules |
| --- | --- | --- | --- |
| F1 Observation oracle | Recorded values compared between revisions for a generated test that passes on both | Reviewer experiments (no key, no flag) | §F1 |
| F2 Deterministic differential fuzzing | Changed functions run on identical seeded inputs on both revisions, without a model | `fuzz` policy object; `--fuzz=false` disables it | §F2 |
| F3 Baseline versions of changed tests | The baseline version of each changed or removed Go test runs on candidate code | `--base-tests` | §F3 |
| F4 Mutation of added lines | Deterministic mutants of added Go lines, run against the package's tests | `mutation` policy object | §F4 |
| F5 Intent-linked candidate-only tests | Acceptance criteria parsed from the intent; model-written tests for one criterion, run on the candidate | Acceptance criteria in `--intent` / `--intent-file` | §F5 |
| F6 Symbol index and impact analysis | Static Go index, callers of changed functions, and optionally the existing tests that reach them run on both revisions | `--impact` (on by default); `--impacted-tests` | §F6 |
| F7 Execution cache and parallel checks | Opt-in replay of recorded baseline runs; bounded parallel initial checks | `--cache-dir`; `--parallel` | §F7 |
| F8 Trusted dependency preparation | A local image derived by the base branch's own preparation command | `prepare` policy object; `--allow-prepare-network` | §F8 |
| F9 Evidence-only exports | SARIF and PR-comment files that contain only evidence-backed findings | `--format sarif,pr-comment`; `--report-url` | §F9 |

Human decisions recorded on 2026-09-25:

1. **Replay-backed negative conclusions.** The rule of §3.4 applies as written: with an explicit `--cache-dir`, a negative conclusion may rest on a replay that two agreeing live runs recorded. It does not by itself request review, and the report lists it in `execution.replay_backed`.
2. **TS/JS differential fuzzing.** v0.4 includes a real harness for eligible exported TS/JS functions, with the same seed scheme (§F2).
3. **DISMISSED** keeps its v0.2 semantics. A `DISMISSED` claim is not refused merely because another cited record verifies positive.
4. **`FAILS_ON_CANDIDATE`** (§F3, §F6) stays a review request (exit 2 with `--ci`). It never produces exit 1 and cannot support `REPRODUCED`.
5. **Mutation noise.** Surviving mutants never request review on their own (medium signal). A mutation run cut short by `max_mutants` is `incomplete`, which requests review under `--ci`.

### 1.3 Stage order and the shared runtime budget

During `review`, stages run in this order: dependency preparation → initial checks (test, typecheck, build) → coverage → changed baseline tests (F3) → impacted tests (F6) → differential fuzzing (F2) → mutation (F4) → reviewer. The cheapest and strongest deterministic evidence therefore comes first. `lint` executes no repository code; of the v0.4 parts it computes only the static impact index (and the lexical signals of F3 and F8).

Budget rules:

1. **Everything is charged except preparation.** Every sandbox run, of every kind and in both check ledgers, is charged to the one budget `sandbox.max_runtime_seconds`. Dependency preparation runs before any check. It is bounded by `prepare.timeout_seconds` and by `--deadline`, and it is not charged to `sandbox.max_runtime_seconds` (R10).
2. **Atomic reservation.** Before launch, a run computes the available budget: the limit minus the time spent minus the time reserved by runs in flight. The limit is `sandbox.max_runtime_seconds`, or a lower stage ceiling when one is set.
   - When nothing is available, the run is recorded as SKIPPED with "Sandbox runtime budget exhausted.", or "Sandbox runtime reserved for reviewer experiments." when the ceiling was the limit.
   - Otherwise the run's timeout is the smallest of the per-run timeout, any tighter stage timeout and the available budget. That timeout is reserved before launch. After execution the reservation is released and the elapsed time is charged.
   - A replay (§3.4) reserves and charges nothing.
   - Run sequentially, this equals the v0.3 accounting.
3. **Sub-caps.** These stages track their own elapsed time, pass a per-run timeout no larger than their remaining sub-cap, and stop launching when it is used up. A sub-cap is a ceiling inside the shared budget, never an addition to it.

   | Stage | Sub-cap |
   | --- | --- |
   | F2 differential fuzzing | `fuzz.max_runtime_seconds` |
   | F4 mutation | `mutation.max_runtime_seconds` |
   | F3 changed baseline tests | 180 s, or less |
   | F6 impacted tests | 180 s |

4. **Reviewer reserve.** When the reviewer will run, half of `sandbox.max_runtime_seconds` is reserved for its experiments: F3, F6, F2 and F4 use the ceiling `max_runtime_seconds − reserve`. The initial checks and coverage are not limited by the reserve, as in v0.3.
5. **Parallelism** is confined to the initial checks (`--parallel`, 1..4). Each launch reserves its timeout per rule 2, in configured order, so the sum of in-flight timeouts never exceeds the remaining budget. Every other run is sequential.
6. **`--deadline D`** (a Go duration from 1m to 24h) sets a stage deadline at the start of the command plus D − 30 s. The 30 s are kept for cleanup and writing the report.
   - Preparation, sandbox runs and the reviewer observe it. The Git comparison, policy loading, static analysis and snapshots are not interrupted by it, but the time they take counts against D.
   - When it is reached: running containers end as TIMEOUT (bounded forced removal); unstarted runs are recorded as SKIPPED "Overall deadline reached; the run was not started."; the reviewer stops with its existing incomplete-investigation entry; the report gains the Unverified entry "The overall --deadline was reached; later stages did not run or were cut short."; and `execution.budget.deadline_reached` is true.
   - A run whose context expired for another reason before it started, such as the reviewer time limit, is recorded as SKIPPED with a text that does not name `--deadline`.
   - Reaching the deadline never produces exit 4 by itself. Preparation is the exception: a preparation cut short fails, and failed preparation exits 4 (§5).
7. **Worst-case wall clock.** Approximately `T ≈ Git + snapshots + prepare.timeout_seconds + S + reviewer.timeout_seconds + N_runs × 5 s + report write`.
   - S is the sandbox time spent before the reviewer. It never exceeds `sandbox.max_runtime_seconds`, and it exceeds `max_runtime_seconds − reserve` only when the initial checks and coverage alone use more.
   - The reviewer's own sandbox runs fall inside `reviewer.timeout_seconds`. Each run's container cleanup is bounded to 5 s.
   - With the defaults and a reviewer: 600 + 300 + 600 s plus overheads when the initial checks and coverage use less than 300 s, and at most 600 + 600 + 600 s plus overheads.
   - With `--deadline D`: at most D plus up to 5 s of cleanup per run in flight, unless the Git comparison, static analysis and snapshot export alone, which the deadline does not interrupt, take longer than D − 30 s.

## 2. Refinements of v0.2

This is the complete list. Every v0.2 MUST / MUST NOT that is not named here stays binding unchanged.

| ID | v0.2 text | v0.4 rule | Scope | Compensating controls | Proving tests |
|---|---|---|---|---|---|
| R1 | §6 "Require a preloaded trusted Docker image; do not pull/build images from candidate instructions." and "Dependencies are prepared outside review." | When the **base-branch policy** has `prepare`, SwiftProof may derive a local image by running that command on inputs exported from the **base commit** and committing the container. It never pulls (`--pull=never`) and never builds from candidate content. Without `prepare`, the v0.2 rule applies verbatim. | F8 | Policy from `BaseRefCommit`; inputs from `BaseCommit` only; runs before any candidate code; key and label check before reuse; size cap; fail closed (exit 4); a candidate change to an input is a visible signal and Unverified entry. | F8 `TestDockerPrepareOfflineCommitAndReuse`; export-source test (HeadCommit content never exported); not_permitted and size-cap tests |
| R2 | §6 "Run non-root with read-only root filesystem, dropped capabilities, no new privileges, and CPU/RAM/PID/time/output limits." | **The prepare container only** has a writable root filesystem. It may run as root when the policy says `user: root`, with a fixed minimal capability set, and has network only when the policy and `--allow-prepare-network` both enable it. Every other §6 limit applies to it. **Checks are unchanged.** | F8 | The profile in §F8: `--cap-drop=ALL` plus the fixed list for root, no-new-privileges, sandbox memory/CPU/PID limits, no host env, one read-only inputs mount, bounded log, `--pull=never`. Egress risk documented. | Golden argv test of the prepare profile; Docker test showing no network without the flag |
| R3 | §6 "Each execution uses fresh disposable state." | With an explicit `--cache-dir`, a baseline-side run of byte-identical inputs may be **replayed** from recorded live executions. A replay never supports a positive status. It supports a negative status only after two agreeing live runs, and the report lists such conclusions. | F7 | Opt-in; key over tree, image ID, policy, docker argv, tool version (§F7); integrity re-check on read; directory outside repo and output, owner-only; contradiction evicts; no raw output persisted. | F0 live-confirmation test; F7 key, poisoning, integrity, contradiction and `replay_backed` tests |
| R4 | §6 "A coverage run MAY return one coverage profile to the host as a length-declared framed payload…" | The same framed channel, with the same guarantees, also returns a Jest-compatible report (existing since TS support) and an F2 observation stream (Go or TS/JS). There is at most one payload per run. Every recorded `Results` is a `Redact` fixed point, and the total is ≤ `ResultsBudget` (16 MiB) per report, of which candidate-side runs may use at most half. | F0, F1, F2 | Framing, separate bound, artifact hash, fixed-point rule, overflow to artifact-only, a baseline-side half that candidate output cannot use up | F0 fixed-point and budget tests; F2 overflow test |
| R5 | §5 "Reference/symbol lookup is textual in this version." | Go lookups may use a static **syntactic** index built on the host from git objects, never executing repository code, and labelled approximate. The textual fallback remains. | F6 | No repository code runs; the approximate label; "empty is not absence" wording | F6 index tests; lexical fallback test |
| R6 | §10 coverage "The command runs last…" | Coverage runs after test/typecheck/build and before every v0.4 stage and the reviewer. It still shares the same budget. | F0 | Stage order fixed in §1.3 | cli stage-order test |
| R7 | §8 "`--checks=false` skips initial checks only" | `--checks=false` also disables the v0.4 deterministic stages: fuzz `disabled`, mutation `not_run`, and `--base-tests` / `--impacted-tests` rejected with exit 3. Reviewer-requested execution remains sandboxed. | F0 | Recorded statuses; the existing Unverified entry | flag tests; `record*Skipped` tests |
| R8 | §7 status table | Adds the hypothesis statuses `DIVERGED` and `INTENT_TEST_FAILED`, which never produce exit 1 and never enter `reproduced_issues`, plus the §3.1 evidence kinds and statuses. **`NOT_REPRODUCED` keeps its v0.2 requirement unchanged.** | F0 | Acceptance table (§4.2) | acceptance-table tests |
| R9 | §7 "Models cannot create evidence records or check outcomes." (unchanged) with a new weaker use | A harness-recorded, **candidate-only** run of a model-written criterion test may support `INTENT_TEST_FAILED`, a separately named status that has no baseline control and says so. It requires an assertion failure and a static reference to changed symbols. | F5 | The verifier conditions (§F5); fixed wording in every output; never exit 1 | F5 verifier tests (panic, setup failure, no reference → UNVERIFIED) |
| R10 | §8 "600 seconds total sandbox runtime" | Prepare is bounded by `prepare.timeout_seconds` (default 600) and `--deadline`, and is **not** charged to `sandbox.max_runtime_seconds`. Every other run is charged, and v0.4 sub-caps sit inside the budget. | F0, F8 | §1.3; `--deadline` | run budget tests |

## 3. Naming conventions

### 3.1 Statuses

Hypothesis statuses. Only `report.Finalize` assigns a final status.

| Status | Meaning |
| --- | --- |
| `REPRODUCED` | v0.2 meaning, unchanged. |
| `DIVERGED` | A cited observation or fuzz experiment recorded different values between revisions for the same inputs (§F1, §F2). |
| `INTENT_TEST_FAILED` | A cited model-written test for one acceptance criterion failed on the candidate, with no baseline control (§F5). |
| `NOT_REPRODUCED` | v0.2 meaning and requirement, unchanged. |
| `DISMISSED` | v0.2 meaning, unchanged. |
| `UNVERIFIED` | The default for anything unsupported. |

Evidence statuses: `OBSERVED`, `REPRODUCED`, `NOT_REPRODUCED`, `UNVERIFIED`, `DIVERGED`, `NOT_DIVERGED`, `FAILS_ON_CANDIDATE`, `PASSES_ON_CANDIDATE`, `INTENT_TEST_FAILED` and `INTENT_TEST_PASSED`. `NOT_DIVERGED` never supports any hypothesis status.

### 3.2 Evidence kinds and runners

Only harness code creates evidence records. The schema's `evidence.runner` enum stays exactly `go_test_json` and `jest_json`.

| Evidence kind | Part | Runner |
| --- | --- | --- |
| `source_observation` | v0.2 | none |
| `differential_test` | v0.2 | `go_test_json` or `jest_json` |
| `differential_observation` | F1 | `go_test_json` (Go `t.Attr`) or `jest_json` (Vitest meta in the Jest-compatible report) |
| `differential_fuzz` | F2 | `go_test_json` for a Go harness; `jest_json` for a TS/JS harness run by Vitest or Jest, whose single payload is the observation stream (§F2) |
| `base_test_differential` | F3 | `go_test_json` |
| `impacted_test_differential` | F6 | `go_test_json` |
| `intent_test` | F5 | `go_test_json` or `jest_json` |

`base_test_differential` and `impacted_test_differential` hold exactly one test name per record.

### 3.3 Check kinds and the `_base` convention

| Check kind | Part | Notes |
| --- | --- | --- |
| `test`, `typecheck`, `build`, `coverage`, `existing_test` | v0.2 | Unchanged. |
| `generated_test_base`, `generated_test_candidate` | v0.2 | The base kind is also the kind of a live re-run. |
| `generated_test_base_repeat` | F1 | Always live, never cached. |
| `generated_test_intent` | F5 | Candidate only. |
| `fuzz_base`, `fuzz_candidate` | F2 | First pair. |
| `fuzz_base_confirm`, `fuzz_candidate_confirm` | F2 | Confirmation pair; always live, never cached. |
| `base_test_base`, `base_test_hybrid` | F3 | The base kind is also the kind of a live re-run. |
| `impacted_test_base`, `impacted_test_candidate` | F6 | The base kind is also the kind of a live re-run. |
| `mutation_control`, `mutant` | F4 | Mutation ledger only (`mutation-check-N`). |

- A kind ending in `_base` is a baseline-side run of an experiment family. Only these kinds are cache-eligible (§F7).
- **A live re-run keeps the first-run kind.** When a would-be positive result rests on a replayed baseline, the harness re-runs the baseline with the same kind, never served from the cache. Write-through to the cache is still allowed, so the live check may carry `cache.status: stored`.
- The only distinct re-run kinds are `generated_test_base_repeat`, `fuzz_base_confirm` and `fuzz_candidate_confirm`. Their verifiers need a separately identifiable check. They do not end in `_base`, so they are never cached.
- Candidate-side and hybrid kinds never end in `_base`.
- Signal, artifact and check kinds are free strings in the schema.

### 3.4 Live-baseline rule and replay-backed conclusions

A positive status needs a live check:

| Positive status | Live check that supports it |
| --- | --- |
| `REPRODUCED` | The `generated_test_base` check cited as `base_check_id`: the first run if it was live, else the live re-run of the same kind. |
| F1 `DIVERGED` | The `generated_test_base_repeat` check. |
| F2 `DIVERGED` | The `fuzz_base_confirm` check. |
| F3 / F6 `FAILS_ON_CANDIDATE` | The base-kind check cited as `base_check_id`: the first run if it was live, else the live re-run of the same kind. |

- A negative status (`NOT_REPRODUCED`, `NOT_DIVERGED`, `PASSES_ON_CANDIDATE`) may rest on a replayed baseline only when the entry recorded at least two agreeing live runs (`cache.live_runs >= 2`).
- The report lists every such evidence ID, sorted, in `execution.replay_backed`, and the Markdown says so per check and in the execution summary.
- Such a conclusion does not, by itself, request human review (R3; §1.2 decision 1).

### 3.5 Section statuses

| Section | Statuses |
| --- | --- |
| `fuzz` | `ran`, `no_candidates`, `not_run`, `disabled`; function outcomes `diverged`, `not_diverged`, `inconclusive` |
| `base_tests` | `no_candidates`, `ran`, `not_run`; items `FAILS_ON_CANDIDATE`, `PASSES_ON_CANDIDATE`, `UNVERIFIED` |
| `mutation` | `not_run`, `no_candidates`, `ran`, `incomplete`; mutants `KILLED`, `SURVIVED`, `INVALID`, `TIMEOUT`, `INCONCLUSIVE`, `NOT_RUN` |
| `impact` | `not_applicable`, `indexed`, `limited`, `unavailable`; `tests_status` `ran`, `no_candidates`, `not_run` |
| `prepare` | `built`, `reused`, `failed`, `not_permitted`, `not_run` |
| `execution.cache` | `enabled`, `disabled`; check-level `cache.status` `hit`, `stored` |

`disabled` is used only for an explicit operator choice (`--fuzz=false` or `--checks=false`). An attempted execution that failed is always `not_run`. Mutation `ran` means that every selected mutant reached a terminal status and none was dropped; it never means complete or exhaustive.

### 3.6 Signal and artifact kinds

New signal kinds: `test_assertion_removed`, `test_case_removed`, `test_skip_added`, `test_expectation_relaxed` (F3, medium), `test_focus_added` (F3, high), `surviving_mutant` (F4, medium), `impacted_caller` (F6, low, capped), `prepare_input_changed` (F8, medium). F6 reuses the existing `analysis_limited` kind (medium, symbol `impact_index`) for capped remainders.

New artifact kinds: `fuzz_harness`, `fuzz_observations`, `fuzz_payload_rejected`, `base_test_hybrid_manifest`, `mutation_check_output`, `mutant_patch`, `intent_test` and `prepare_output`. F1 retains diverging tests as `generated_test` artifacts.

### 3.7 Audit names

Reviewer-callable tools keep plain names: `find_callers` (F6), `create_intent_test` and `run_intent_test` (F5). Harness stages use the reserved prefix `stage:`, which no tool name can take: `stage:prepare` (F8), `stage:run_base_tests` (F3), `stage:run_impacted_tests` (F6), `stage:run_fuzz` and `stage:run_fuzz_confirm` (F2), `stage:run_mutation_control` and `stage:run_mutant` (F4), `stage:execution_cache` (F7, one event per hit). A reviewer call to a name outside the offered set is audited as `rejected_tool_call`, with the requested name in its arguments; a model-supplied name is never used as an audit tool name.

### 3.8 PR-comment markers

The PR comment body sits between `<!-- swiftproof:pr-comment:begin v1 -->` and `<!-- swiftproof:pr-comment:end -->`. Intent parsing removes every such block from the intent text, with a recorded note (§F5).

### 3.9 Retired names

Design drafts used other names. They MUST NOT appear in code, schema, output or documentation: `NO_DIVERGENCE`, `BASE_TEST_FAILS_ON_CANDIDATE`, `BASE_TEST_PASSES_ON_CANDIDATE`, `CONTRADICTS_INTENT`, `CONTRADICTED`, `NOT_CONTRADICTED`, `intent_contradictions`, `existing_test_differential`, `impacted_test_regression`, `masked_test_change`, `existing_test_regression`, `intent_contradiction`, `fuzz_confirm_base`, `fuzz_confirm_candidate`, `base_test_baseline`, `base_test_candidate`, the section statuses `not_requested`, `not_configured` and `completed`, the impact statuses `complete`, `partial` and `disabled`, and a `--no-cache` flag.

## 4. Evidence strength ladder

### 4.1 Ladder

| Rank | Status / record | Basis | Exit effect |
| --- | --- | --- | --- |
| 1 | Hypothesis `REPRODUCED` | Model-written generated test; live baseline PASS; candidate FAIL; validated named execution | 1 if high/critical, else 2 with `--ci` through its FAIL check |
| 2 | F3 `FAILS_ON_CANDIDATE` (base_tests item) | Human-written baseline test; live baseline PASS; hybrid FAIL | 2 with `--ci`, never 1 |
| 3 | F6 `FAILS_ON_CANDIDATE` (impacted test) | Unchanged existing test that statically reaches changed code; live base PASS; candidate FAIL | 2 with `--ci`, never 1 |
| 4 | `DIVERGED` from F2 (`differential_fuzz`) | No model; seeded inputs; four runs; confirmation live | 2 with `--ci`, never 1 |
| 5 | `DIVERGED` from F1 (`differential_observation`) | Model-chosen inputs; recorded values; live baseline repeat | 2 with `--ci`, never 1 |
| 6 | Hypothesis `INTENT_TEST_FAILED` | Model-written test for a verbatim criterion that references changed symbols; candidate only; no baseline control; assertion failure | 2 with `--ci`, never 1 |
| 7 | F4 surviving mutant | Deterministic single change; control and mutant both pass; re-derived by `Finalize` from retained logs | none by itself (medium signal) |
| 8 | `NOT_REPRODUCED` / `NOT_DIVERGED` / `PASSES_ON_CANDIDATE` / `INTENT_TEST_PASSED` / `KILLED` | Observations about the recorded experiment only | none |
| 9 | `DISMISSED` | Source observation plus rationale | none |
| 10 | `UNVERIFIED` (anything unsupported) | Missing or invalid evidence | 2 with `--ci` |

### 4.2 Hypothesis acceptance

A claimed hypothesis status is kept only when it is **valid** (at least one evidence ID is cited, and every cited ID resolves to exactly one evidence record) and **supported** (at least one cited record's kind and re-derived status is accepted for the claim):

| Claimed status | Accepted evidence (kind, re-derived status) | Extra condition |
| --- | --- | --- |
| `REPRODUCED` | `differential_test`, `REPRODUCED` | Live baseline (§3.4) |
| `DIVERGED` | `differential_observation`, `DIVERGED`; `differential_fuzz`, `DIVERGED` | Live repeat or confirmation (§3.4) |
| `INTENT_TEST_FAILED` | `intent_test`, `INTENT_TEST_FAILED` | The evidence's criterion equals the hypothesis's, and that criterion occurs exactly once in the report |
| `NOT_REPRODUCED` | `differential_test`, `NOT_REPRODUCED` | v0.2 requirement unchanged |
| `DISMISSED` | `source_observation`, `OBSERVED` | Non-blank rationale |

Anything else becomes `UNVERIFIED`. These never support any hypothesis status: `differential_observation` or `differential_fuzz` with `NOT_DIVERGED`; `base_test_differential`; `impacted_test_differential`; `intent_test` with `INTENT_TEST_PASSED`; and every signal, coverage record, mutant, impact record, cache record and prepare record.

A stored evidence status counts only when a verifier re-derives the same status from the recorded checks. Missing or duplicated checks or evidence make a record unverifiable. When two verifiers disagree on an ID, the ID is removed. A `differential_test` record whose test names or test path would be changed by redaction, or contain the redaction marker, supports nothing, because a re-rendered report could select different results with them; a high `REPRODUCED` claim resting only on such a record therefore requests review instead of producing exit 1. A `DIVERGED` claim is kept only when a cited record is listed in `divergences`, so the Investigation Summary and Behavior Divergences never disagree. A `NOT_REPRODUCED` that the verifier re-derived for a generated test is also withdrawn when the same candidate check's observation record holds a key recorded on both sides with different full values, whatever that record's own status (§F1).

### 4.3 Export classes

Only these classes can become SARIF results or PR-comment findings (§F9). Rule IDs and levels are fixed; everything else is never a finding.

| Order | Class | Rule ID | Level |
| --- | --- | --- | --- |
| 1 | `reproduced` | `swiftproof/reproduced` | `error` if high/critical, else `warning` |
| 2 | `base_test_fails_on_candidate` | `swiftproof/base-test-fails-on-candidate` | `warning` |
| 3 | `impacted_test_fails_on_candidate` | `swiftproof/impacted-test-fails-on-candidate` | `warning` |
| 4 | `fuzz_divergence` | `swiftproof/fuzz-divergence` | `warning` |
| 5 | `observed_divergence` | `swiftproof/observed-divergence` | `warning` |
| 6 | `intent_test_failed` | `swiftproof/intent-test-failed` | `note` |
| 7 | `surviving_mutant` | `swiftproof/surviving-mutant` | `note` |

## 5. Exit codes

Precedence:

- **3** exits early, before any report is written and before any container starts.
- For a written report, **4 > 1 > 2 > 0**.
- Only a reproduced high/critical hypothesis sets 1. The operational code 4 is applied after the report is finalized.
- `--ci` turns "human review required" into 2 only when the code is still 0.

| Source | Without `--ci` | With `--ci` |
| --- | --- | --- |
| Hypothesis `REPRODUCED` high/critical | 1 | 1 |
| Hypothesis `REPRODUCED` low/medium | 0 | 2 (its candidate check FAILed) |
| Hypothesis `DIVERGED`, or any `divergences[]` entry | 0 | 2 — **never 1** |
| Hypothesis `INTENT_TEST_FAILED` | 0 | 2 — **never 1** |
| Hypothesis `UNVERIFIED` | 0 | 2 |
| Hypothesis `NOT_REPRODUCED` or `DISMISSED` | 0 | 0 |
| F3 item `FAILS_ON_CANDIDATE` or `UNVERIFIED`; `base_tests.status` `not_run` | 0 | 2 — never 1 |
| F3 `PASSES_ON_CANDIDATE`; status `no_candidates` | 0 | 0 |
| F3 compile failure on candidate code (FAIL check) | 0 | 2 (not 4) |
| F6 impacted test `FAILS_ON_CANDIDATE` or `UNVERIFIED`; `tests_status` `not_run` | 0 | 2 |
| F6 `impacted_caller` (low), `analysis_limited` (medium) | 0 | 0 |
| F2 function `inconclusive`; `fuzz.status` `not_run` | 0 | 2 |
| F2 `not_diverged`; `disabled`; `no_candidates` | 0 | 0 |
| F2 **candidate-side** failure (`fuzz_candidate*` FAIL, including compile or setup failure) | 0 | 2 (function `inconclusive`) — **never 4** |
| F2 **baseline-side** harness build or setup failure (`fuzz_base*`), or infrastructure failure (exit ≥125, −1, artifact loss) — ERROR check | 4 | 4 |
| F4 `mutation.status` `incomplete` or `not_run` | 0 | 2 |
| F4 `ran` (survivors only) / `no_candidates` | 0 | 0 |
| F4 infrastructure ERROR (exit ≥125 or −1) or workspace create/restore failure | 4 | 4 |
| F5 intent link discarded by `Finalize` (Unverified note) | 0 | 2 |
| F5 `INTENT_TEST_PASSED`; any retained `intent_judgment` | 0 | 0 |
| F5 intent not UTF-8 or contains NUL | 3 | 3 |
| F5 intent-test setup failure (ERROR check; inherited `generated_test_*` rule, unchanged v0.2 behavior) | 4 | 4 |
| F7 cache hit, miss, rejected, contradicted, or disabled at runtime | 0 | 0 |
| Invalid `--cache-dir` (syntax, location, ownership, symlink) | 3 | 3 |
| `--deadline` reached (TIMEOUT / SKIPPED checks, Unverified entry) | 0 | 2 |
| F8 `built` / `reused` / `not_run` | 0 | 0 |
| F8 `failed` / `not_permitted` | 4 | 4 |
| F8 candidate changed a prepare input (Unverified entry) | 0 | 2 |
| F9 rendering | no effect | no effect |
| Any check in `checks` with status ERROR (all kinds, including v0.4) | 4 | 4 |
| Any check in `checks` not PASS (the mutation ledger is excluded) | 0 | 2 |
| Any high/critical signal (includes `test_focus_added`) | 0 | 2 |
| Invalid flag combination (§6.5), unknown format, bad `--report-url`, invalid policy key value | 3 | 3 |
| Report write failure | 4 | 4 |

The log of a candidate-side v0.4 check (`fuzz_candidate`, `fuzz_candidate_confirm`, `base_test_hybrid`, `impacted_test_candidate`, and the mutation ledger's `mutation_control` and `mutant`) is written by candidate code, so its text never makes the check ERROR: such a check is ERROR only for an infrastructure cause (a Docker or executor error, exit code 125 or above, a lost log artifact), and a compile, setup or run failure stays FAIL. The inherited v0.2 log-text rules (a recognized setup failure of a generated test, a `fork/exec` failure line) still apply to the v0.2 kinds, to `generated_test_intent` and to the baseline-side kinds.

## 6. Report fields and section order

### 6.1 JSON

`version` stays `1`: every change is additive, and no new root property is required by the schema, so earlier reports still validate. New root properties:

| Property | Type | Present |
| --- | --- | --- |
| `intent_sha256` | SHA-256 of the intent text used for criteria | when an intent was supplied |
| `intent_criteria` | array of `{id, text, line}` (at most 100) | always (possibly `[]`) |
| `prepare` | object | review mode and the policy has `prepare` (status `not_run` when no execution was needed); absent in lint |
| `base_tests` | object | review with `--base-tests` |
| `divergences` | array | always (possibly `[]`) |
| `intent_test_failures` | array of hypotheses with status `INTENT_TEST_FAILED` | always (possibly `[]`) |
| `mutation` | object, with its own check ledger | review mode and the policy has `mutation`, **whatever `--checks` says**; absent in lint |
| `fuzz` | object | review mode and the policy has `fuzz`; absent in lint |
| `impact` | object; `tests_status` only with `--impacted-tests` | lint or review, unless `--impact=false` |
| `execution` | object: cache, parallelism, budget, `replay_backed` | whenever a sandbox harness was created |

**Present means requested.** The object of an opt-in part is present exactly when that part was requested or configured for the run, and its `status` then says what happened, including `not_run` with a reason. The three always-present arrays serialize as `[]`, never `null`.

Changed shared definitions:

- `check` gains `cache` (only on base-kind checks that were stored or replayed). Its `results` field is normalized structured output that a verifiable runner returned on the payload channel (a Jest-compatible report or a fuzz observation stream). It is kept apart from `output` so that log text cannot impersonate it; code executing in the sandbox can still write it.
- `evidence` gains `repeat_check_id` (F1), `criterion_id` and `referenced_symbols` (F5), and the kind and status enums of §3.
- `hypothesis` gains `criterion_id` and `intent_judgment` (`expected_change` or `unexpected_change`). `intent_judgment` is model judgment, not evidence, allowed only on a `DIVERGED` hypothesis, and never affects status, severity, targets or exit code.
- The `exit_code` description adds: "Only a reproduced high/critical hypothesis produces 1. DIVERGED, INTENT_TEST_FAILED and every other v0.4 record never produce 1."

A divergence entry records a validated difference between recorded values for recorded inputs. It records a difference, not which revision is correct. Each entry carries its evidence ID, kind, anchor, test path and names, at least 3 (observation) or 4 (fuzz) resolving check IDs, the citing `DIVERGED` hypotheses, 1 to 32 `DIVERGED` rows, and the fixed note: "Recorded values differ between revisions for the same inputs: the candidate differed from baseline runs that agreed with each other. This does not establish which revision is correct." An observation entry without its own path is anchored at the first citing hypothesis whose path names a changed, non-deleted file, labelled model-chosen; otherwise it stays unanchored.

### 6.2 Markdown section order

Headings are exact:

1. `# Change Confidence Report`
2. `## Change Summary` (including the Policy, Intent and Exit code lines)
3. `## Dependency Preparation`, only when `prepare` is present
4. `## Automated Checks`, with a cache note per replayed or stored check and the execution summary after the list
5. `## Investigation Summary`, with the intent link after each hypothesis
6. `## Reproduced Issues`, with the intent link after each issue
7. `## Changed Baseline Tests on Candidate Code`, only when `base_tests` is present
8. `## Behavior Divergences`, **always rendered**:
   - with no observation or fuzz evidence: "No observation or fuzz experiment was recorded."
   - with such evidence, no divergence, and every such record re-derived as `NOT_DIVERGED`: "No recorded experiment diverged. Where values were compared, they were equal for the recorded inputs; values are bounded, redacted serializations, and this does not establish equivalent behavior, even for those inputs."
   - with such evidence, no divergence, and at least one such record not re-derived as `NOT_DIVERGED` (unverified, unstable or incomparable values, or a stored status the recorded checks do not support): "No validated divergence was recorded. N of M observation or fuzz records are inconclusive or were not accepted as evidence (see Recorded Evidence), so nothing is said about their values; this does not establish equivalent behavior."
   - otherwise one entry per divergence, up to 20 value rows each ("… N more in confidence-report.json"), then the note once.
9. `## Intent Test Failures` and `## Intent Criteria`, only when an intent, criteria or failures exist
10. `## Unverified Areas`
11. `## Suggested Human Review`
12. `## Review Surface`
13. `## Changed-line Execution`
14. `## Mutation of Added Lines`, only when `mutation` is present; killed mutants are counted, never listed
15. `## Differential Fuzzing`, only when `fuzz` is present; it summarizes outcomes and points to Behavior Divergences for the values
16. `## Impact Analysis`, only when `impact` is present
17. `## Recorded Evidence`: an `intent_test` record prints "Candidate check: X (candidate-only; no baseline control)"; a `base_test_differential` record prints "Hybrid-tree check: X; baseline check: Y"; a record with a repeat check adds "; baseline repeat: Z". A stored status other than `UNVERIFIED` that `Finalize` did not re-derive from the recorded checks (or withdrew) is followed by "; as stored; not accepted as evidence".
18. `## Artifacts`, then the attribution

`intent_judgment` is rendered only after the hypothesis's evidence line, with the fixed label "Model judgment (not evidence): expected change | unexpected change". Every repository or model string goes through the inline escaper, with no code spans and no links.

### 6.3 Console lines

After the existing coverage line, `review` and `lint` print at most one line per part, in this order, and only when there is something to say: changed baseline tests, divergences, intent, fuzzing, mutation, impact, execution. The `Reports:` line comes last. The divergence line reads "N recorded behavior divergences (baseline and candidate recorded different values; a human decides which is intended)." The initial checks are announced on one progress line, for example "Running test, typecheck, build in isolated Docker sandboxes...".

### 6.4 Files and formats

`--format` accepts `markdown` (`CONFIDENCE_REPORT.md`), `json` (`confidence-report.json`), `sarif` (`confidence-report.sarif`) and `pr-comment` (`PR_COMMENT.md`); the default stays `markdown,json`. Every requested format is rendered in memory first; a render error writes nothing. Each file is replaced atomically. Both `review`/`lint` and `report` finalize before rendering, and rendering never changes the exit code.

### 6.5 Flags

| Flag | Commands | Default | Validation (every error exits 3 before any container starts) |
| --- | --- | --- | --- |
| `--format` | review, lint, report | `markdown,json` | Each value must be a known format. |
| `--report-url URL` | review, lint, report | none | Only with `pr-comment`; `https`, a host, no userinfo, ≤512 bytes, restricted character set. |
| `--base-tests` | review | false | Exits 3 with `--checks=false`. |
| `--fuzz` | review | true | — |
| `--impact` | lint, review | true | — |
| `--impacted-tests` | review | false | Exits 3 with `--checks=false` or `--impact=false`. |
| `--cache-dir DIR` | review | none (no cache) | Non-blank, ≤4096 bytes, no NUL; location and ownership rules of §F7. |
| `--parallel N` | review | 1 | 1..4; accepted and without effect with `--checks=false`. |
| `--allow-prepare-network` | review | false | — |
| `--deadline D` | review | none | A Go duration from 1m to 24h. |

An execution-only flag (`--base-tests`, `--fuzz`, `--impacted-tests`, `--cache-dir`, `--parallel`, `--allow-prepare-network`, `--deadline`) set explicitly on `lint` exits 3 with "lint does not execute sandbox checks; --X applies to review only". `--allow-network` and `--no-network` keep their v0.2 accept-and-ignore behavior in lint. `--no-network` also keeps the prepare container offline.

### 6.6 Finalize

`Finalize` re-derives every status from recorded checks, in both `review` and `report`. Section finalizers may change only their own section and never set the exit code. Running `Finalize` twice, or `Write` → decode → `Finalize`, MUST yield byte-identical JSON; every recorded `results` value is therefore a fixed point of redaction. Redaction itself is applied repeatedly until the text no longer changes (a single v0.2 pass could leave text that a second pass would still change); text that would need more than 8 changing passes, which only adversarially nested input does, is replaced as a whole by the redaction marker, keeping its line breaks. `reproduced_issues` and `intent_test_failures` copy a hypothesis only after its intent link is normalized, so each copy equals its hypothesis.

## 7. Release ordering

Mandatory, and repeated in the CI documentation and the v0.4.0 release notes:

1. Any of `fuzz`, `mutation` or `prepare` makes every binary before v0.4.0 exit 3. Do not add them to the repository's own `.swiftproof.json`, to `app/examples/swiftproof.go.json`, or to any base-branch policy until a release that accepts them is published and the workflows are re-pinned (URL and sha256).
2. The new CLI flags (§6.5) make older binaries exit 3 at flag parsing. Do not add them to `review.yml` or `pr-review.yml` before re-pinning.
3. An older binary's `swiftproof report` silently drops the new report fields, because it decodes with plain `json.Unmarshal`. Render with the producing binary.
4. Consumers validating against the old schema reject new reports. Publish the schema with the release.

The new policy keys are optional top-level objects. `swiftproof init` never writes them, `null` means absent, unknown nested fields and duplicate keys fail, and every validation error exits 3 with an English message. The command-name whitelist is unchanged. F1, F3, F5, F6, F7 and F9 add no policy key.

## 8. Non-claims

These wording rules are binding for every output and document. A status or record MUST NEVER be presented as follows:

| Status / record | Must never be presented as |
| --- | --- |
| `REPRODUCED` | A confirmed bug. Proof that the assertion encodes intended behavior. A general correctness verdict. |
| `DIVERGED` (both kinds) / `divergences[]` | Which revision is correct. A regression, bug, breaking change or defect. Equivalence elsewhere. The function's complete output (values are lossy serializations). Determinism beyond the recorded runs. Tamper-proof against code that sabotages its own process. A reason for exit 1. |
| `NOT_DIVERGED` / fuzz `not_diverged` / "no recorded experiment diverged" | Equivalence, a safe refactor, a verified change, equivalent behavior **even for the recorded inputs** (values are bounded, redacted serializations), or anything beyond the recorded projection. **It never supports any hypothesis status**, a dismissal, a severity change or signal removal. |
| Every `NOT_*` / `PASSES_ON_CANDIDATE` / `INTENT_TEST_PASSED` | Tamper-proof. The values and results come from channels that code executing in the sandbox can write (log, payload file, Vitest meta). They are kept apart from the log so that log text cannot impersonate them, and nothing more. |
| fuzz `inconclusive` / unstable / timeout / crash | A divergence or a defect. |
| `FAILS_ON_CANDIDATE` (F3) | A reproduced issue or regression. Proof that the test edit is wrong, deliberate or hides anything. Proof that the baseline assertion is the intended behavior. |
| `FAILS_ON_CANDIDATE` (F6) | Caused by the changed function: the static link is approximate, and the failure may come from any part of the change or from flakiness. A regression. |
| `PASSES_ON_CANDIDATE` | Preserved behavior, an equivalent or weaker test, or correct code. It says nothing about tests the PR did not touch. |
| F3 lexical `test_*` signals | Proof that a test got weaker. They are heuristics and never evidence. |
| Surviving mutant | A bug, a missing test, dead code, or "untested". The mutant may be semantically equivalent, and other packages were not run. There is no mutation score or percentage. |
| Killed mutant | "Well tested" or any reassurance. It is only counted, never listed in Markdown, stdout or exports (the JSON records it for verification). |
| Mutation `ran` | "Complete" or exhaustive: it means every selected mutant reached a terminal status and none was dropped. |
| `INTENT_TEST_FAILED` | A bug, a reproduction, a regression, or a contradiction of the intent. Proof that the code is wrong: the test, the reading of the criterion and the inputs are model-written, and there is no baseline control. Never in `reproduced_issues`. |
| `INTENT_TEST_PASSED` | A criterion satisfied, met, implemented, accepted or tested. No "x/y criteria", no check marks, no ratios. |
| `intent_judgment` | Evidence, or a reason to hide, demote, dismiss or change the exit code of anything. It exists only on DIVERGED hypotheses and is always labelled "Model judgment (not evidence)". |
| Criteria extraction | Understanding of the intent. "No criteria" does not mean "no requirements". |
| `impacted_caller`, index tool output, impact `indexed` | "No callers", "unused", "all callers", "complete", "safe to change", "tests/covers/verifies". An absent caller is not proof of absence. Interface edges are possible dispatch only. |
| Cache hit / replay-backed conclusion | A fresh execution, re-verification or re-test. More reliable. An authenticated record: the integrity hash is not authenticity. It captures kernel or runtime internals beyond the recorded identity. A speed-up without a measurement. |
| Parallel checks | Any change in semantics or budget. |
| Prepared image | Safe, unmodified, license-clean or vulnerability-free dependencies. A reproducible image (equal key means equal inputs, not equal content). Candidate dependencies installed or tested. Labels as an attestation. Network available to checks. |
| SARIF / PR comment | An approval, "LGTM", "no issues", "safe to merge", or confidence, precision, security-severity or any percentage. An empty export is not approval ("No finding is not approval"). GitHub's "fixed" alert state is not a SwiftProof claim. Re-rendered exports are not authenticated, and exports from fork runs may be forged. |
| Any v0.4 record | "Executed" as "tested". Any path to exit 1 other than a reproduced high/critical hypothesis. Deleting a signal, lowering a severity or supporting a dismissal. The new evidence only ever adds review requests. |

Documentation rule: about v0.4 statuses, never write "tested", "verified", "safe", "correct", "approved", "regression", "bug", "masked", "contradicts", "complete" (about coverage, callers or mutation) or "score", and never use "%", except inside the explicit negations above. Examples of report output come from real runs.

## 9. Acceptance criteria

v0.4 is acceptable when, in addition to the v0.2 criteria and the acceptance bullets of each §F section:

- Exit 1 arises only from a reproduced high/critical hypothesis. For every v0.4 record, a fixture shows the exit codes of §5 without and with `--ci`, and never 1.
- Every record listed in §4.2 as never supporting a hypothesis status fails to support each status in tests, and `NOT_REPRODUCED` keeps its v0.2 requirement.
- A positive status resting on a replayed baseline becomes `UNVERIFIED` (or the part's inconclusive status); a negative status rests on a replay only after two agreeing live runs and is listed in `execution.replay_backed`.
- Removing checks or evidence from a saved report, or forcing statuses without evidence, makes every v0.4 positive result fall back to `UNVERIFIED`, `inconclusive` or `INCONCLUSIVE` when it is re-rendered, and yields no export finding for that class.
- `Finalize` is idempotent, including across `Write` → decode → `Finalize` of a report whose results contain secret-shaped text.
- Every invalid flag combination, format, `--report-url`, `--cache-dir`, intent encoding and policy value exits 3 before any container starts.
- The always-present arrays serialize as `[]`, the Markdown always contains `## Behavior Divergences`, and each opt-in object is present exactly when requested (§6.1).
- A configured part that cannot run records `not_run` with a reason and requests human review under `--ci`. A failed preparation runs no repository code and exits 4.
- Total reserved and charged sandbox time never exceeds `sandbox.max_runtime_seconds`, including with `--parallel 4`, and sub-caps stay inside it.
- The check sandbox profile is unchanged; only the prepare container differs, as R2 allows.
- Reports validate against the published schema, and the schema reflects every model field.
- Outputs and documentation follow §8.
- A combined end-to-end review using every part (a policy with `fuzz`, `mutation` and `prepare`, `--base-tests`, `--impacted-tests`, an intent, a scripted provider, every format, `--cache-dir`, `--parallel 2` and `--deadline`) exits 2 (never 1 unless a reproduced high/critical hypothesis is scripted), renders the sections in §6.2 order, re-renders identically, leaves the checkout clean and leaves no `swiftproof-` container behind.

Product validation remains open, as in v0.2: whether these parts save review time or catch important changes must be measured on representative pull requests.

## F1. Observation oracle

A generated test may record values instead of asserting them: Go through `testing.T.Attr` (Go 1.25 or later in the image), Vitest through `task.meta.swiftproof`; Jest is not supported. When the test passes on both revisions and a recorded key differs, the harness runs exactly one live baseline repeat. Evidence kind `differential_observation`, hypothesis status `DIVERGED`. There is no policy key and no flag.

<!-- F1:begin -->
<!-- F1:end -->

## F2. Deterministic differential fuzzing

Opt-in through the `fuzz` policy object, review only; `--fuzz=false` disables it for a run. Changed package-level Go functions whose signature is unchanged and whose parameters can be generated run on identical seeded inputs (seed scheme `swiftproof-fuzz/v1`) on both revisions, with one confirmation pair when the first pair differs. No model is involved. TS/JS support follows §1.2 decision 2 and the rules below. Evidence kind `differential_fuzz`; check kinds `fuzz_base`, `fuzz_candidate`, `fuzz_base_confirm` and `fuzz_candidate_confirm`.

<!-- F2:begin -->
### F2.1 Selection (Go)

Selection MUST only parse the baseline and candidate snapshots on the host (`go/parser`, and `go/build.MatchFile` over in-memory files). It MUST NOT execute repository code. It reads at most 1,000 Go files per package directory and revision, 2 MiB per file and 32 MiB per directory. A bound that is hit, or a file that does not parse, MUST become a skip reason, never a silent drop.

A **changed function** is a package-level function declared in a changed non-test `.go` file (status modified, added, or renamed within its directory), outside `testdata`, `vendor` and directories the go tool ignores, whose body token digest differs between the revisions. The digest ignores comments and layout. Statement separators count, whether written or inserted by the scanner at a line end, except directly before a closing brace or parenthesis: `return g()` split over two lines is a rewrite. A function that exists on one revision only, or whose body digest is unchanged, is not rewritten and is not listed. A function declared in several files of its package (build-constrained variants) is rewritten only when the body digests of its declarations, counted with multiplicity, differ between the revisions.

A changed function is planned only when every rule below holds. Otherwise `fuzz.skipped` lists it with the reason of the first rule that fails:

| Rule | Skip reason |
| --- | --- |
| No receiver | methods are not fuzzed in this version |
| No type parameters on either revision | generic functions are not fuzzed in this version |
| Declared once in its package on each revision | function is declared more than once in the package |
| A Go body on both revisions | function has no Go body on one revision |
| On both revisions, its file has no `//go:build` or `// +build` line and its name builds on linux/amd64 and linux/arm64 | file has build constraints |
| No file of the package imports `"C"` | package uses cgo |
| One package clause per revision, the same on both | package has more than one package clause; package name differs between revisions |
| No package-level declaration of the package, test files included, reuses a predeclared identifier | predeclared identifier shadowed |
| Identical types-only signature text on both revisions | signature changed |
| Every parameter type is generated | parameter type T is not generated in this version; named type T differs between revisions |
| At least one seeded input has a call text that redaction leaves unchanged (§F2.2) | no seeded input has a call text that redaction leaves unchanged |

File-level reasons (line 0): sensitive path excluded from the sandbox; file moved to another directory; file is not in the candidate snapshot; package could not be read within the selection bounds; package could not be parsed. These reasons name files relative to the snapshot with a fixed cause. They MUST NOT quote host paths or operating system error text.

**Generated parameter types:** `bool`, `string`, `int`, `int8` to `int64`, `rune`, `uint`, `uint8` to `uint64`, `byte`, `float32` and `float64`; a package-local named type or alias whose underlying type is one of them and whose declaration prints identically on both revisions; and `[]E`, `[N]E` (decimal `N` from 0 to 16) and `...E` of those. `int` and `uint` are 64-bit, as on the sandbox platforms linux/amd64 and linux/arm64. Maps, pointers, structs, interfaces, functions, channels, complex numbers, `uintptr`, qualified types such as `time.Duration`, and nested composites are not generated.

**Order and budget.** Functions are ordered by the highest severity of the new-side signals overlapping them, then exported before unexported, then path and line. Packages are taken in the order of their best function up to `max_packages`, then functions up to `max_functions`. The rest are skipped with "fuzz budget reached (max_packages)" or "fuzz budget reached (max_functions)". One package run plans at most 1,024 inputs: each function gets as many inputs as its corpus yields within `min(max_inputs, 1024 / functions of the package)`, so a function without parameters gets exactly one and `func(bool)` two.

### F2.2 Seeded corpus

- The seed of a function MUST be the first eight bytes, big-endian, of SHA-256 over `swiftproof-fuzz/v1`, NUL, the package directory, NUL, the function name, NUL and the signature text. It MUST NOT depend on commits, file paths, run identifiers or time, so the inputs stay the same across updates of a pull request.
- The generator is SplitMix64, implemented by SwiftProof rather than taken from `math/rand`, so the corpus does not change with the Go version. Any change to the corpus MUST come with a new seed scheme.
- Order: first, every parameter at its first edge value. Then each parameter runs through its other edge values while the others stay at their first. Then seeded random inputs follow: each value is an edge value or a random value with equal probability; strings have at most 64 bytes, from a biased alphabet with occasional multibyte characters, control characters and invalid UTF-8; slices have 0 to 8 elements and are nil one time in ten.
- Edge values: for integers, 0, 1, -1, 2, -2, 7, 10, 100, -100, 255, 256, 1000, 65535, the 32-bit bounds, and each kind's minimum, maximum and their neighbours; for runes also letters, a multibyte rune, U+10FFFF, a surrogate and 0x110000; for floats also 0.5, 0.1, 1e-9, 1e6, the largest and smallest magnitudes, both infinities, NaN and negative zero; for strings the empty string, spaces, separators, a newline, a tab, NUL, multibyte text, a right-to-left override, an invalid byte, `../` and 64 bytes; for slices nil, empty, one element, two elements in both orders, a repeated element, and four elements ascending and descending; for arrays the zero value and the first edge values in both orders.
- Inputs are deduplicated by their call text, for example `Discount(Cents(1000))`. An input whose call text redaction would alter, as it is or in its JSON-escaped form, is dropped, so every call is a stable observation key that redaction leaves unchanged. This applies to the one call of a function without parameters too. A function none of whose inputs remains (typically because its name or a named parameter type looks like a credential) is skipped at selection.

### F2.3 Observation harness

- One internal `_test.go` file per package, `<dir>/swiftproof_fuzz_<suffix>_test.go`, is rendered on the host. `<suffix>` is 16 hexadecimal digits drawn at random for each run. Every import name, package-level identifier and test-function local of the file starts with `swiftproofFuzz<suffix>`, and the tests are named `TestSwiftProofFuzz_<suffix>_<n>`, so code under review cannot declare a colliding identifier in advance. Rendering MUST refuse an identifier that the package declares on either revision, and then draws a new suffix.
- The only repository-derived text in the file is the package name, the function names and the named parameter types, each re-checked as an identifier the package declares. Literals are produced by `strconv`. The file MUST NOT use `fmt`, and MUST compile with the language features of Go 1.13.
- The harness MUST NOT assert anything. Its tests pass unless the observation file cannot be written.
- For every input, each function is evaluated twice with fresh arguments, each evaluation in its own goroutine and bounded by `call_timeout_ms`. The canonical encoding of an evaluation records the results, a recovered panic as `panic(...)`, and each slice argument after the call. Results carry a type prefix; numbers use `strconv` forms that keep NaN, the infinities and negative zero apart; nil stays distinct from empty; maps are sorted by encoded key; struct fields are read in order through reflection, unexported ones included; pointers are followed with a cycle marker and never printed as addresses; and errors are encoded as their messages. The encoding is bounded to 64 KiB, depth 16 and 1,024 elements per container, and a record says when a bound was reached.
- An evaluation that exceeds `call_timeout_ms` ends the function with a `timeout` stop and poisons the process: later functions record only a `poisoned` stop. A `runtime.Goexit`, or a panic outside the called function, ends the function with a `goexit` or `abort` stop.
- The same file runs on both revisions and in the confirmation pair.

### F2.4 Observation stream

- The harness appends one JSON line per record to `/tmp/swiftproof-observations.jsonl`, which returns on the framed payload channel (R4). A `begin` record carries the planned count. One `obs` record per input carries its index, the SHA-256 and byte length of the full encoding, a display cut of at most 256 bytes of valid UTF-8 (smaller when the payload budget requires it), and three flags: panicked, unstable (the two evaluations differed) and truncated. A `stop` record carries its reason. An `end` record repeats the count.
- Validation MUST be all-or-nothing. Every line MUST be the byte-exact canonical form of one record. Functions MUST appear in harness order, each opening with a `begin` of its planned count. Indices MUST be consecutive from 0, and nothing may follow a function's `end` or `stop`. After a `timeout`, later functions may only record `poisoned`. Any violation rejects the whole stream.
- The normalized stream stored in `Check.Results` gives, per function, its state: `complete`, `stopped`, `interrupted` (the records end without an `end` or `stop`, so the process ended) or `not_started`. It also gives the stop reason and the input being evaluated when the function ended. Per record, it gives the host-rendered call, the hash, the length, the redacted display and the flags. It MUST be canonical JSON with every object member and array element on its own line (Go's `json.MarshalIndent` with an empty prefix and indent), and unchanged by redaction; otherwise it is rejected. The line layout keeps a redaction rule from matching across fields: in compact JSON, a display ending in `http://host` and a later record holding `@` formed one match and rejected a stream of harmless values. A display that redaction would still alter in its JSON-escaped form is replaced by `[REDACTED]` as a whole. Redaction changes displays only: comparisons use the hashes computed in the sandbox.
- A rejected stream makes every function of its package inconclusive for that run. It does not by itself make a check ERROR.

### F2.5 Comparison and outcomes

- The records of a run MAY be used for a function only when all of these hold: the check is PASS with exit code 0, or FAIL with exit code 1 to 124; its log is not truncated; the log records exactly one run and one pass of that function's test, in one package; and its stream parses and plans the same number of inputs. A FAIL check still supports the functions whose tests passed before its process ended. The checks used for one function MUST share one recorded command and one package, and their streams MUST describe the same inputs.
- Live-baseline rule (§3.4): candidate-side and confirmation checks MUST NOT be replays. The first baseline run MAY be a replay only when its cache entry recorded at least two agreeing live runs. `diverged` always rests on the live `fuzz_base_confirm` check.
- Per input: *not recorded* when either first-pair record is missing; *unstable* when either first-pair record is flagged unstable; *compared* when the hashes and lengths are equal. When they differ, the confirmation pair decides: *unstable* when either revision does not reproduce its own first hash or is flagged unstable, *compared* and *diverged* otherwise, and *unconfirmed* without confirmation records. `inputs = compared + unstable + unconfirmed + not_recorded`, and `diverged <= compared`. Two kinds of unstable input separate the revisions: one flagged unstable on the candidate only, and a first-pair difference that the confirmation pair did not reproduce.
- Outcome: `diverged` when any input diverged; otherwise `inconclusive` when a difference could not be confirmed; otherwise `not_diverged` when both first-pair functions are complete, at least one input was compared, and no unstable input separates the revisions; otherwise `inconclusive`, with a reason that names the input being evaluated when a process ended or a function stopped. A timeout, a crash, a process exit, a `runtime.Goexit` or instability MUST NEVER yield `diverged`.
- The counterexample is the divergent input with the shortest call text, then the lowest index, with both display values, among the divergent inputs whose two displays differ. When every divergent input shows identical displays (the difference lies in a cut or redacted part), it is the shortest call overall, and the report says that the values shown are cut or redacted. It is the smallest divergent input tried with a visible difference; there is no shrinking. A divergence lists at most 32 divergent inputs, the counterexample first, then by call length and index. A row whose display is cut, redacted or bounded in the sandbox is marked truncated.
- Packages run sequentially in plan order. One confirmation pair runs for a package when its first pair shows a difference and the `fuzz.max_runtime_seconds` sub-cap has time left.
- A function whose package run recorded checks gets one `differential_fuzz` evidence record: `DIVERGED`, `NOT_DIVERGED` or `UNVERIFIED`, runner `go_test_json`, the harness path, the one test name, and the first pair as `check_id` and `base_check_id`. A function without a recorded run (sub-cap or deadline reached, harness not rendered or not staged) is `inconclusive` without evidence, and gets an Unverified line like every inconclusive function.
- Outcomes are derived only from recorded checks, by the same comparison whenever they are computed, so deriving them again from a saved report shows whether its recorded checks support the stored outcome. An edit that breaks a stream's structure rejects the stream. The comparison reads hashes, lengths, indices, flags, function states, commands, check kinds and the test outcomes in the logs; a consistent edit of those records gives the outcome they describe and is not detected. Display values, the counterexample text and log lines that do not decide a test outcome are not protected: editing them leaves the outcome unchanged while showing other values.

**Non-claims specific to fuzzing.** `not_diverged` says that the recorded encodings of the tried inputs were equal. It never establishes equivalent behavior, not even for those inputs: encodings are bounded and redacted, errors are compared by message, and other side effects (files, globals, standard output, goroutines) are not observed. `diverged` records a difference, never which revision is right. Code under review writes the stream of its own revision and can hide or fabricate its own differences. It cannot write the baseline's stream or change the comparison. The functions of one package run in one process, in plan order: state that an earlier function left behind (globals, caches, files) can cause a difference recorded for a later function, so a divergence is attributed to the function whose input was being evaluated, not necessarily to its cause. The report keeps the unsalted SHA-256 and the length of every recorded encoding, also when its display was redacted, so a low-entropy value such as a short password can be recovered from the report by hashing guesses.

**Acceptance (Go core).**

- The design's calc fixture, run in real containers with the sandbox isolation flags: `Percent` is `diverged` at `Percent(0, 0)`, with `int(0)` against `panic(error("runtime error: integer divide by zero"))`; `Discount` is `diverged` at `Discount(Cents(1000))`, with `calc.Cents(900)` against `calc.Cents(1000)`; `Join` is `not_diverged`; `Stamp` is `inconclusive` because every input is unstable; `Halt` is `inconclusive` with the candidate process-exit pointer at `Halt(7)`. It takes four checks, and neither snapshot keeps a harness file.
- The rendered harness compiles and runs with the go tool in a module that declares `go 1.13`, and its stream distinguishes panics, slice mutation after the call, NaN, negative zero, and nil versus empty.
- Every malformed stream in the test table is rejected as a whole, and every structurally edited normalized stream in the test table fails to parse.
- Streams of harmless values are accepted: a compiled `Endpoint(host string) string { return "tcp://" + host }` harness with its 64 seeded inputs, a display ending in a bare-host URL followed by records holding `@`, and 3,000 random streams of displays built from the fragments the redaction rules react to.
- A function whose every call text looks like a credential is skipped at selection, never rendered with zero inputs.
<!-- F2:end -->

## F3. Baseline versions of changed tests on candidate code

Opt-in with `--base-tests`, review only, Go execution only. The baseline version of each Go test function the change modified or removed runs on the baseline tree and on a hybrid tree: the candidate tree with the affected packages' test files reverted to the baseline. Evidence kind `base_test_differential`, one record per test name; check kinds `base_test_base` and `base_test_hybrid`. The lexical test-edit signals for Go, JS/TS and Python are heuristics, never evidence.

<!-- F3:begin -->
<!-- F3:end -->

## F4. Mutation of added lines

Opt-in through the `mutation` policy object, review only, Go only. Deterministic single-change mutants of added lines in changed non-test Go files run the package's tests in a private copy of the candidate, after one passing control run per package, in a separate check ledger (`mutation-check-N`). Surviving mutants become medium `surviving_mutant` signals. Mutation creates no evidence record and no hypothesis, and computes no score.

<!-- F4:begin -->
<!-- F4:end -->

## F5. Intent-linked candidate-only tests

Acceptance criteria are parsed from `--intent` / `--intent-file` (criteria grammar v1), with positional IDs `AC-N` and the SHA-256 of the intent text. When criteria exist, the reviewer is offered `create_intent_test` and `run_intent_test`, which run a model-written test for one criterion on the candidate only. Evidence kind `intent_test`; hypothesis status `INTENT_TEST_FAILED`; `intent_judgment` only on `DIVERGED` hypotheses. There is no policy key and no flag.

<!-- F5:begin -->
<!-- F5:end -->

## F6. Symbol index and impact analysis

The core (on by default, `--impact=false` disables it, lint and review) builds a static Go index on the host from committed Git objects, never executing repository code. It records callers of changed functions as capped, low `impacted_caller` signals, backs the reviewer's `find_references`, `inspect_symbol` and `find_callers` tools with a lexical fallback, and fills the `impact` object. With `--impacted-tests` (review only), the existing tests that statically reach changed functions run on both revisions: evidence kind `impacted_test_differential`, one record per test name; check kinds `impacted_test_base` and `impacted_test_candidate`.

<!-- F6:begin -->
<!-- F6:end -->

## F7. Execution cache and parallel checks

The baseline execution cache is opt-in with an explicit `--cache-dir` and replays only baseline-side runs (§3.3, §3.4). Incremental re-review on a pull-request update is cache reuse across runs that share a cache directory. `--parallel` (1..4) runs the initial checks concurrently without changing their order in the report, their semantics or the budget. The `execution` object records cache, parallelism, budget and `replay_backed`.

<!-- F7:begin -->
<!-- F7:end -->

## F8. Trusted dependency preparation

Opt-in through the base-branch policy's `prepare` object, review only. Before any candidate code runs, the policy's command runs in one bounded container on inputs exported from the base commit, and the container is committed as a local derived image keyed by its inputs and reused by key. Every check of the run then uses that image. Network requires both the policy's `prepare.network` and `--allow-prepare-network`. Preparation fails closed (exit 4).

<!-- F8:begin -->
<!-- F8:end -->

## F9. Evidence-only exports

`--format sarif` and `--format pr-comment` write findings of the §4.3 classes only, read from finalized fields; `--report-url` adds a validated link to the PR comment. SwiftProof publishes nothing itself.

<!-- F9:begin -->
<!-- F9:end -->
