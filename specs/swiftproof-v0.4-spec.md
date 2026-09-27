# SwiftProof V0.4 — More deterministic evidence, less model dependence

**Status:** consolidated specification of the CLI implemented on `main`, checked against commit `23a1d2e` on 2026-09-27. This describes source capabilities, not the feature set of every published binary. Release compatibility is covered in §7; Hub, website and deployment are covered in [swiftproof-hub-spec.md](swiftproof-hub-spec.md).

**Implementation:** portable Go CLI, standard library only.  
**Outputs:** `CONFIDENCE_REPORT.md`, `confidence-report.json`, optionally `confidence-report.sarif` and `PR_COMMENT.md`, and retained experiment artifacts.

## 1. Scope

### 1.1 Product principles

This is a standalone specification: the core contract is in §2 and the detailed evidence stages are in §F1–F9. Older proposals are retained in Git history only.

The product helps developers identify the changed behavior, inspect recorded experiments and choose human review locations. Public CLI output and documentation use English; freeform intent and hypotheses may use any language. The principles are:

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
3. **DISMISSED** requires a source observation and a rationale (§4.2). A `DISMISSED` claim is not refused merely because another cited record verifies positive.
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
3. **Sub-caps.** These stages track their own elapsed time, pass a per-run timeout no larger than their remaining sub-cap, and stop launching when it is used up. A sub-cap is a ceiling inside the shared budget, never an addition to it.

   | Stage | Sub-cap |
   | --- | --- |
   | F2 differential fuzzing | `fuzz.max_runtime_seconds` |
   | F4 mutation | `mutation.max_runtime_seconds` |
   | F3 changed baseline tests | 180 s, or less |
   | F6 impacted tests | 180 s |

4. **Reviewer reserve.** When the reviewer will run, half of `sandbox.max_runtime_seconds` is reserved for its experiments: F3, F6, F2 and F4 use the ceiling `max_runtime_seconds − reserve`. The initial checks and coverage are not limited by the reserve.
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

## 2. Core CLI contract

### 2.1 Commands, comparisons and policy

The commands are `init`, `lint`, `review`, `report` and `version` (also
`--version`); `help`, `--help` and `-h` expose usage.

- `init` writes a version-1 `.swiftproof.json` with argv arrays and defaults for
  Go, TypeScript/JavaScript, Python or an unknown language; existing policy is
  not overwritten without `--force`. Defaults are starting points, not prepared
  dependency environments. It does not seed the optional F2/F4/F8 objects.
- `lint` works with Git and the binary, without Docker, a model or network.
  It executes no repository code. `--checks=true` or `--reviewer=true` are
  rejected. It includes deterministic signals and the default static impact
  analysis (§F6).
- `review` adds isolated checks and the stages in §1.3. `--checks=false` skips
  initial checks, coverage and deterministic execution stages, but does not
  forbid sandbox execution requested by the reviewer (§6.5).
- `report` reads saved JSON (64 MiB maximum), finalizes evidence again, and
  renders the requested formats. It does not authenticate imported data or
  re-execute experiments. Use the producing binary to preserve its fields.
- `--base main --head HEAD` compares the unique merge base to head;
  `BASE..HEAD` or `--exact` compares exact endpoints; `BASE...HEAD` selects
  the merge base. Resolve refs once and record requested refs and immutable IDs.
- Only committed Git objects are analyzed. Working-tree edits and untracked
  files are outside the comparison. Missing history, ambiguous merge bases,
  malformed revisions and exhausted bounds must not yield a clean partial result.
- Default policy comes from **`BaseRefCommit`, the resolved tip of `--base`**,
  which can differ from the diff's merge-base `BaseCommit`. Candidate edits
  cannot change their own checks or privileges. `--config PATH` explicitly
  selects a trusted local override. Missing baseline policy uses built-in
  defaults with a visible note.
- Policy is strict versioned JSON: unknown fields, duplicate keys, invalid
  bounds, unknown command names and multiple root values fail. The command
  names are `test`, `typecheck`, `build`, `generated_test` and `coverage`.
  Full validation is in [config.go](../app/internal/config/config.go).

### 2.2 Static signals and source boundaries

Signals carry stable IDs, kind, severity, path, old/new side, line range,
summary and evidence text, in deterministic order. Signals are risks to
investigate, not evidence-backed findings.

The linter compares Go AST exported declarations/signatures and sensitive
function bodies, ignoring comment/formatting-only body changes. Lexical rules
cover network/DB/auth operations, removed validation/error handling, unsafe
constructs/type suppressions, TODO/FIXME, branch growth, dependency manifests,
sensitive paths, binaries, deletions and structural changes. Sensitive names
and branch counts are heuristics, not vulnerability or complexity proofs.
Missing associated changed tests means precisely that, not missing coverage.
Test-edit signals are detailed in §F3; Go static caller analysis in §F6.
TypeScript/JavaScript analysis remains lexical.

Git paths use NUL-delimited metadata; renames, deletions, binaries and unusual
names retain their identity. External diff/textconv are disabled. Bounds are
64 MiB of Git output, 2 MiB per analyzed source file, 100,000 tree entries/files
and 250,000 parsed diff lines including context. Parse failures and skipped
oversized files stay visible. Snapshots export exact Git blobs without checkout
filters/export attributes, reject symlinks/submodules for execution and cap
individual files at 512 MiB and a snapshot at 2 GiB.

### 2.3 Harness, reviewer and sandbox

The offered tools are `read_file`, `get_diff`, `search_code`, `find_references`,
`inspect_symbol`, `find_callers`, `run_tests`, `run_test`, `run_typecheck`,
`run_build`, `create_test`, `run_generated_test`, `delete_generated_test`,
`create_intent_test` and `run_intent_test` (the last two are offered only when
acceptance criteria exist).
[ToolDefinitions](../app/internal/harness/harness.go) is the authoritative
argument schema; §F5 and §F6 specify intent tests and index-backed lookup.

Paths stay within sanitized snapshots: reject absolute paths, traversal,
metadata directories, credential filenames and symlink escapes. Reads, searches,
responses and generated tests are bounded; accepted/rejected calls are audited.
Commands are trusted argv arrays without implicit shell interpretation.
`{file}`/`{package}` substitution uses validated test paths. Generated tests
cannot overwrite existing files; deletion does not refund their creation budget.
Secret masking is best effort, including disk reports and provider context.

Checks use preloaded trusted Docker images, fresh disposable state, sanitized
read-only source mounts and bounded ephemeral workspace copies. They run
non-root with a read-only root filesystem, dropped capabilities,
no-new-privileges and CPU/RAM/PID/time/output limits. Do not mount the working
checkout or Docker socket, or forward host secret variables into checks.
Networking requires both trusted policy and `--allow-network`; `--no-network`
forces it off. Timed-out containers are forcibly removed and temporary
snapshots are cleaned up. There is no host fallback and no candidate-directed
image pull/build. Missing Docker, images or toolchains is incomplete execution,
never PASS. §F8 is the sole preparation exception; §F7 describes opt-in replay.
Docker and trusted images are part of the execution boundary, not protection
against kernel/runtime vulnerabilities.

Generated-test evidence validates named execution with Go `go test -json`
(cache disabled) or a Jest-compatible structured report from Vitest/Jest.
Missing, skipped, unrelated or inconclusive named results cannot establish
reproduction. Other frameworks may run checks but lack this evidence adapter.
Recognized setup/compilation failures remain unverified; an arbitrary runner
can emit unfamiliar failures. Even a differential test can encode the wrong
expected behavior. §3–5 define classification and exit effects.

A nonempty reviewer model from trusted policy, explicit config or deployment
activates investigation in `review`. Endpoint/key alone does not activate it;
`--reviewer=false` disables calls, `--reviewer` requires a model, and lint or
empty changes make no provider calls. `SWIFTPROOF_REVIEWER_ENDPOINT` and
`SWIFTPROOF_REVIEWER_MODEL` override policy. The configured API-key variable
(default `SWIFTPROOF_API_KEY`) takes precedence over its `_FILE` path and then
`/run/secrets/<NAME>`. Secret resolution errors fail even when investigation is
disabled; endpoint validation is required only for an active reviewer.

Provider calls use bounded Chat Completions function calling, HTTPS remotely
or HTTP on loopback, with redirects refused. Provider traffic is separate from
sandbox networking; `--no-network` alone does not disable it. Repository text,
intent and tool output remain untrusted input. Provider failures preserve the
deterministic report and record incomplete investigation. A model cannot create
evidence or check outcomes. Defaults: 20 iterations, 10 test creations, 600 s
reviewer timeout, 600 s sandbox runtime, 120 s per command, 64 KiB captured
output, 1 GiB RAM and 2 CPUs. Provider input/output and call budgets are bounded
separately. Exhaustion stays visible as an unverified area.

### 2.4 Coverage, report and review surface

Go-only changed-line execution uses the optional `coverage` argv with exactly
one `{coverage_out}` placeholder. `init --language go` seeds
`go test -covermode=count -coverprofile={coverage_out} ./...`; other languages
seed no coverage command. The harness substitutes the path without appending
coverage flags. It runs after initial checks and before the other stages,
within the shared runtime budget. An absent command is `not_configured`.

A length-declared stdout frame returns at most one separately bounded payload
(profile, Jest-compatible results or fuzz stream), with no writable host mount
or post-exit container access. Mismatched frames are discarded. Profiles are
retained as hashed artifacts and mapped without inference: distinguish executed
added lines, not-executed lines, lines with no instrumented block, and lines
not measured. Missing/truncated/unparsable/unmapped profiles are not measured.
No execution measurement establishes correctness; deleted lines are not measured
on the candidate. Non-Go and baseline coverage comparison remain deferred.

The canonical version-1 report records tool/time, change and policy provenance,
signals, checks, hypotheses, evidence, reproduced issues, unverified areas,
review targets/surface, coverage, artifacts, audit and exit code. §6 lists the
additional sections and outputs; the machine contract is the
[JSON schema](../app/schema/confidence-report.schema.json).

Review targets use path, old/new side and inclusive ranges. Merge overlapping
targets and count each changed line once. The denominator is additions plus
deletions, excluding context and unavailable binary line counts; deleted lines
use baseline coordinates. `0 / N` focused lines does not remove the need for
review, and binary-only changes remain reviewable with a zero denominator.
Escape repository/model text and mask sensitive-file diff bodies. Files are
replaced atomically; reports remain unsigned audit records, not attestations.

### 2.5 Cross-stage rules

The R identifiers are retained for links from implementation documentation;
the rules below are current behavior, not amendments to a deleted document.

| ID | Rule | Detailed contract |
| --- | --- | --- |
| R1 | Trusted policy may prepare a local image using baseline inputs only; without `prepare`, a preloaded dependency image is required. | §F8 |
| R2 | Only preparation has a writable root filesystem and optional policy-selected root user with fixed capabilities; checks keep the profile in §2.3. | §F8 |
| R3 | Explicit cache use may replay baseline PASS after two agreeing live runs; positive conclusions require live confirmation. | §3.4, §F7 |
| R4 | One framed payload per run; recorded structured results are redaction fixed points, bounded to 16 MiB per report with at most half consumed by candidate runs. | §6.6, §F2 |
| R5 | Go lookups may use the approximate host-side static index, never executing code or loading external imports; lexical fallback remains. | §F6 |
| R6 | Coverage follows initial checks and precedes deterministic stages and reviewer. | §1.3 |
| R7 | `--checks=false` skips deterministic execution stages as well as initial checks and coverage; reviewer execution remains sandboxed. | §6.5 |
| R8 | `DIVERGED` and `INTENT_TEST_FAILED` never enter `reproduced_issues` or produce exit 1. | §3–5 |
| R9 | A candidate-only criterion test may support `INTENT_TEST_FAILED` only with validated assertion failure and reference to changed symbols. | §F5 |
| R10 | Preparation has its own timeout and observes `--deadline`; all other sandbox runs share one runtime budget. | §1.3, §F8 |

## 3. Naming conventions

### 3.1 Statuses

Hypothesis statuses. Only `report.Finalize` assigns a final status.

| Status | Meaning |
| --- | --- |
| `REPRODUCED` | Same generated-test command: validated baseline PASS and candidate FAIL without recognized setup failure, supported by differential evidence and a live baseline. The assertion still needs human judgment. |
| `DIVERGED` | A cited observation or fuzz experiment recorded different values between revisions for the same inputs (§F1, §F2). |
| `INTENT_TEST_FAILED` | A cited model-written test for one acceptance criterion failed on the candidate, with no baseline control (§F5). |
| `NOT_REPRODUCED` | Validated generated test passes on baseline and candidate; this experiment did not reproduce the hypothesis (§4.2). |
| `DISMISSED` | Recorded source observation plus non-blank rationale; an investigation explanation, not correctness proof. |
| `UNVERIFIED` | The default for anything unsupported. |

Evidence statuses: `OBSERVED`, `REPRODUCED`, `NOT_REPRODUCED`, `UNVERIFIED`, `DIVERGED`, `NOT_DIVERGED`, `FAILS_ON_CANDIDATE`, `PASSES_ON_CANDIDATE`, `INTENT_TEST_FAILED` and `INTENT_TEST_PASSED`. `NOT_DIVERGED` never supports any hypothesis status.

### 3.2 Evidence kinds and runners

Only harness code creates evidence records. The schema's `evidence.runner` enum stays exactly `go_test_json` and `jest_json`.

| Evidence kind | Part | Runner |
| --- | --- | --- |
| `source_observation` | Core | none |
| `differential_test` | Core | `go_test_json` or `jest_json` |
| `differential_observation` | F1 | `go_test_json` (Go `t.Attr`) or `jest_json` (Vitest meta in the Jest-compatible report) |
| `differential_fuzz` | F2 | `go_test_json` for a Go harness; `jest_json` for a TS/JS harness run by Vitest or Jest, whose single payload is the observation stream (§F2) |
| `base_test_differential` | F3 | `go_test_json` |
| `impacted_test_differential` | F6 | `go_test_json` |
| `intent_test` | F5 | `go_test_json` or `jest_json` |

`base_test_differential` and `impacted_test_differential` hold exactly one test name per record.

### 3.3 Check kinds and the `_base` convention

| Check kind | Part | Notes |
| --- | --- | --- |
| `test`, `typecheck`, `build`, `coverage`, `existing_test` | Core | Unchanged. |
| `generated_test_base`, `generated_test_candidate` | Core | The base kind is also the kind of a live re-run. |
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
| `NOT_REPRODUCED` | `differential_test`, `NOT_REPRODUCED` | Validated named execution passes on both revisions |
| `DISMISSED` | `source_observation`, `OBSERVED` | Non-blank rationale |

Anything else becomes `UNVERIFIED`. These never support any hypothesis status: `differential_observation` or `differential_fuzz` with `NOT_DIVERGED`; `base_test_differential`; `impacted_test_differential`; `intent_test` with `INTENT_TEST_PASSED`; and every signal, coverage record, mutant, impact record, cache record and prepare record.

A stored evidence status counts only when a verifier re-derives the same status from the recorded checks. Missing or duplicated checks or evidence make a record unverifiable. When two verifiers disagree on an ID, the ID is removed. A `differential_test` record whose test names or test path would be changed by redaction, or contain the redaction marker, supports nothing, because a re-rendered report could select different results with them; a high `REPRODUCED` claim resting only on such a record therefore requests review instead of producing exit 1. A `DIVERGED` claim is kept only when a cited record is listed in `divergences`, so the Investigation Summary and Behavior Divergences never disagree. A `NOT_REPRODUCED` that the verifier re-derived for a generated test is also withdrawn when the same candidate check may have recorded a difference, whatever the observation record's own status: a key recorded on both sides with different full values and, failing closed, a key recorded on one side only, a value that could not be read, a key or value holding `[REDACTED]`, or a channel error on either side; the values are also read from the generated test's own checks (§F1).

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
| F5 intent-test setup failure (ERROR check; core `generated_test_*` rule) | 4 | 4 |
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

The log of a candidate-side v0.4 check (`fuzz_candidate`, `fuzz_candidate_confirm`, `base_test_hybrid`, `impacted_test_candidate`, and the mutation ledger's `mutation_control` and `mutant`) is written by candidate code, so its text never makes the check ERROR: such a check is ERROR only for an infrastructure cause (a Docker or executor error, exit code 125 or above, a lost log artifact), and a compile, setup or run failure stays FAIL. The core log-text rules (a recognized setup failure of a generated test, a `fork/exec` failure line) still apply to the core check kinds, to `generated_test_intent` and to the baseline-side kinds.

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

`intent_judgment` is rendered only after the hypothesis's evidence line, with the fixed label "Model judgment (not evidence): expected change | unexpected change". Every repository or model string goes through the inline escaper, with no code spans and no links: it replaces control and bidirectional characters, HTML-escapes `&`, `<` and `>`, and backslash-escapes the Markdown punctuation; quotes and apostrophes stay as text.

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

An execution-only flag (`--base-tests`, `--fuzz`, `--impacted-tests`, `--cache-dir`, `--parallel`, `--allow-prepare-network`, `--deadline`) set explicitly on `lint` exits 3 with "lint does not execute sandbox checks; --X applies to review only". `--allow-network` and `--no-network` are accepted and ignored in lint. `--no-network` also keeps the prepare container offline.

### 6.6 Finalize

`Finalize` re-derives every status from recorded checks, in both `review` and `report`. Section finalizers may change only their own section and never set the exit code. Running `Finalize` twice, or `Write` → decode → `Finalize`, MUST yield byte-identical JSON; every recorded `results` value is therefore a fixed point of redaction. Redaction itself is applied repeatedly until the text no longer changes (a single pass could leave text that a second pass would still change); text that would need more than 8 changing passes, which only adversarially nested input does, is replaced as a whole by the redaction marker, keeping its line breaks. `reproduced_issues` and `intent_test_failures` copy a hypothesis only after its intent link is normalized, so each copy equals its hypothesis.

## 7. Release ordering

Mandatory, and repeated in the CI documentation and the v0.4.0 release notes:

1. Any of `fuzz`, `mutation` or `prepare` makes a binary built from v0.3.4 or earlier exit 3. Do not add them to the repository's own `.swiftproof.json`, to `app/examples/swiftproof.go.json`, or to any base-branch policy until a release that accepts them is published and the workflows are re-pinned (URL and sha256).
2. The new CLI flags (§6.5) make those older binaries exit 3 at flag parsing. Do not add them to `review.yml` or `pr-review.yml` before re-pinning.

The deployment pipeline tagged `main` as v0.3.5 and later v0.3 versions during the v0.4 integration. Binaries built from those tags accept the new keys and flags without implementing all of v0.4 (v0.3.5 to v0.3.9: the foundation, whose stages record `not_run`; v0.3.10 and later: the stages without all the integration fixes). They MUST NOT be presented or pinned as v0.4 releases; the rules above apply to v0.4.0 and later.
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

The implementation must meet the core criteria below and the acceptance bullets of each §F section:

- Static lint needs only Git and the binary; configured reviewer activation, explicit disabling, empty changes and provider failures follow §2.3.
- Real Git fixtures cover immutable comparisons, baseline-tip policy trust, additions, deletions, renames and binaries.
- Fabricated/duplicate IDs, setup failures, timeouts and unrelated or skipped tests cannot establish reproduction.
- Traversal, floods, unauthorized tools, redirects, sensitive inputs and Markdown injection are covered by boundary tests.
- Reports agree on evidence and deduplicated surface counts, including deletions. Builds and tests target Linux, Windows and macOS; race and real Docker integration require suitable runners.

- Exit 1 arises only from a reproduced high/critical hypothesis. For every v0.4 record, a fixture shows the exit codes of §5 without and with `--ci`, and never 1.
- Every record listed in §4.2 as never supporting a hypothesis status fails to support each status in tests, and `NOT_REPRODUCED` requires named generated-test execution passing on both revisions.
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

Product validation remains open: whether these parts save review time or catch important changes must be measured on representative pull requests.

## 10. Later work

The following capabilities are implemented in the bounded forms described here:

- symbol indexes: a static Go index parsed and type-checked from committed source, approximate (§F6);
- mutation testing: single-change mutants of added lines of Go files (§F4);
- property-style testing, in the narrow form of deterministic differential fuzzing of changed Go functions and exported TS/JS functions of basic types (§F2);
- differential existing suites: Go only, as baseline versions of changed tests (§F3) and, opt-in, impacted existing tests (§F6);
- commit-, policy- and image-keyed caching: baseline-side runs only, opt-in, local (§F7);
- SARIF: evidence-only files, never uploaded by SwiftProof (§F9).

These remain future extensions, not current capabilities:

- semantic TypeScript/JavaScript analysis (every TS/JS part of v0.4 is lexical);
- mutation of TypeScript/JavaScript and other non-Go code, and differential existing suites for languages other than Go;
- shrinking of fuzz counterexamples, coverage-guided fuzzing, and fuzzing of methods, generic functions and composite parameter types;
- non-Go coverage formats, base-side coverage comparison and coverage regression detection on unchanged code;
- signed attestations of reports, exports or prepared images;
- automatic PR-comment posting or SARIF upload by the CLI (the Hub already has forge authentication, push webhooks and commit statuses; see its separate specification);
- remote or shared caches, and incremental re-review against a previous report (`--previous-report`) or an interdiff target (`--since`).

## F1. Observation oracle

A generated test may record values instead of asserting them: Go through `testing.T.Attr` (Go 1.25 or later in the image), Vitest through `task.meta.swiftproof`; Jest is not supported. When the test passes on both revisions and a recorded key differs, the harness runs exactly one live baseline repeat. Evidence kind `differential_observation`, hypothesis status `DIVERGED`. There is no policy key and no flag.

<!-- F1:begin -->
### F1.1 Channels

- Go observations MUST be read only from `go test -json` events with `Action` `attr`, whose `Test` is exactly one of the generated top-level test names and whose `Key` starts with `swiftproof.`; the recorded key is what follows the prefix. Subtest events, other tests, keys without the prefix and ordinary output events MUST be ignored. A framed line too long for the runner to convert (an output event starting with the framing byte, `=== ATTR  `, the test name and the prefix) MUST make its key unreadable. An attr line that no longer parses (for example after redaction) or has no key MUST make the whole run's observations a channel error.
- Vitest observations MUST be read only from `assertionResults[].meta.swiftproof` of the report entry for exactly the generated file, for top-level assertions whose title is a generated test name. Jest reports carry no per-test metadata; a Jest template MUST NOT be treated as recording observations.
- The normalized Vitest meta MUST be a JSON object of at most 32 keys with redacted keys and values, each value kept as canonical JSON text with its type (sorted object keys, numbers as written, strings quoted, so that `4` and `"4"` differ), and its encoding MUST be a `Redact` fixed point; anything else MUST be replaced by a fixed marker that retains no recorded text. A value whose canonical text exceeds 1024 bytes, and a key longer than 200 bytes, MUST be kept only as a fixed stand-in carrying its length and the sha256 of its redacted text, which is never compared. Meta decoding MUST NOT make a runner report unreadable, and observations MUST NOT turn an otherwise readable report into one that redaction would alter or that exceeds the per-run payload limit or its side's remaining results budget (all metas of that report are then replaced by a marker).
- F1 MUST NOT add a policy key, flag, tool, container channel, mount or environment variable.

### F1.2 Comparison

- Values MUST be compared in full, as recorded in the check `output` (Go) or `results` (Vitest), never as display cuts. Rows carry UTF-8-safe display cuts of 256 bytes of the redacted value, with `truncated` set.
- A row MUST be `INCOMPARABLE` when its key is empty, longer than 200 bytes, not UTF-8, contains whitespace or control characters, or is altered by redaction; when the key was recorded more than once for the named test in any run; when it was recorded on one revision only; when any recorded value contains `[REDACTED]`, is not a `Redact` fixed point, exceeds 1024 bytes or is the stand-in of a longer value; when its value was unreadable; or when two differing values look like a memory address or a source position.
- A row MUST be `DIVERGED` only when the baseline, the baseline repeat and the candidate each recorded the key exactly once, the baseline and the repeat recorded the same value, and the candidate recorded a different one. It is `UNSTABLE` when the repeat did not record the first baseline value, and `EQUAL` when every recorded value is equal.
- A record MUST be `DIVERGED` when any row is `DIVERGED`, `NOT_DIVERGED` when at least one row was compared and every row is `EQUAL`, and `UNVERIFIED` otherwise, including when either run did not pass with exit code 0 under a validated named execution, when the commands differ, when more than 32 distinct keys were recorded in a run, and when nothing was recorded.
- When a key differs and no repeat has run, the harness MUST run exactly one live baseline repeat of the same staged test and command, as a check of kind `generated_test_base_repeat`, never served from the execution cache and charged to the runtime budget. It MUST NOT run a second repeat or a candidate-side repeat.
- A `DIVERGED` record MUST retain the generated test source as a hashed `generated_test` artifact and refuse its deletion; when retention fails, the record MUST be `UNVERIFIED`.

### F1.3 Re-derivation and report

- `report.Finalize` MUST re-derive every `differential_observation` record with the function the harness used, from a base check of kind `generated_test_base`, a candidate check of kind `generated_test_candidate` (never replayed) and, when cited, a repeat of kind `generated_test_base_repeat` with the base command, each resolving exactly once. The record's test names and path MUST be `Redact` fixed points without the marker. `DIVERGED` MUST require the cited repeat to have been executed, not replayed; `NOT_DIVERGED` MAY rest on a replayed base only when `cache.live_runs >= 2`. Only a re-derived status equal to the stored one counts.
- `NOT_DIVERGED` MUST NOT support any hypothesis status. A `DIVERGED` hypothesis MUST cite a listed divergence (§4.2).
- `divergences` MUST list every validated `DIVERGED` record, cited or not, with `check_ids` [base, candidate, repeat] and only its `DIVERGED` rows sorted by (test, key). The feed MUST leave `path` empty; Finalize anchors it at a citing hypothesis's changed, non-deleted path (`anchor_source` `hypothesis`).
- A re-derived `differential_test` `NOT_REPRODUCED` MUST be withdrawn when its runs may have recorded a difference: a key recorded on both revisions with different full values (§4.2), and also, failing closed, a key recorded on one revision only, a key whose value could not be read on either revision, a key or value that holds `[REDACTED]`, and a channel error on either revision (including Vitest observations replaced by a marker). The values MUST be read from the checks of every `differential_observation` record, whatever its status, and of every `differential_test` record itself, so that removing the observation record does not lift the rule.
- Outputs MUST NOT present a divergence as a defect, a breaking change or a statement of which revision is correct, and MUST NOT present `NOT_DIVERGED` as equivalence (§8).

### F1.4 Acceptance

- A real Go 1.26 run of a rewritten `Discount` records `DIVERGED` on `Discount(5,33)` only (baseline `4`, candidate `3`), with three PASS checks, and a scripted review exits 2 with `--ci` (never 1), with `reproduced_issues` empty and an identical re-render.
- An equivalent rewrite gives `NOT_DIVERGED`, two checks, and an accepted `NOT_REPRODUCED` claim.
- A key whose baseline value changes between runs is `UNSTABLE`; when no other key diverges, the record is `UNVERIFIED` and a `DIVERGED` claim is downgraded, and the both-pass `NOT_REPRODUCED` of the same test is withdrawn.
- A `DIVERGED` claim citing the `differential_test` is `UNVERIFIED`, while the divergence stays listed uncited.
- A Vitest run diverges through `task.meta.swiftproof`; a Jest template records an `UNVERIFIED` observation.
- Plain `=== ATTR` output from code under test adds no key; a forged framed line makes its key `INCOMPARABLE`, never `NOT_DIVERGED`.
- A candidate value too long for the Go test runner to convert, and values made equal by redaction, withdraw the both-pass `NOT_REPRODUCED` of the same test. A large Vitest value never turns a readable `PASS` or `FAIL` generated-test check into `ERROR`. An ordinary generated test that records no observation keeps its single `differential_test` record, also when a run produced no report.
- Stripping or altering the repeat, the checks, the evidence, the stored status or the recorded values, or marking the repeat replayed, makes a `DIVERGED` claim `UNVERIFIED` with no divergence; secret-shaped values are `INCOMPARABLE`, never `DIVERGED`, and divergence rows survive `Write` → decode → `Finalize` byte for byte.
<!-- F1:end -->

## F2. Deterministic differential fuzzing

Opt-in through the `fuzz` policy object, review only; `--fuzz=false` disables it for a run. Changed package-level Go functions, and changed exported TS/JS functions (§F2.8), whose signature is unchanged and whose parameters can be generated run on identical seeded inputs (seed scheme `swiftproof-fuzz/v1`) on both revisions, with one confirmation pair when the first pair differs. No model is involved. TS/JS support follows §1.2 decision 2 and §F2.8. Evidence kind `differential_fuzz`; check kinds `fuzz_base`, `fuzz_candidate`, `fuzz_base_confirm` and `fuzz_candidate_confirm`.

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
- A rejected stream makes every function of its package inconclusive for that run. A rejected candidate-side stream never makes a check ERROR; a baseline-side run that passed with a rejected stream is ERROR (§F2.6).

### F2.5 Comparison and outcomes

- The records of a Go run (runner `go_test_json`) MAY be used for a function only when all of these hold: the check is PASS with exit code 0, or FAIL with exit code 1 to 124; its log is not truncated; the log records exactly one run and one pass of that function's test, in one package; and its stream is a Go stream (no runner mark) that parses and plans the same number of inputs. A FAIL check still supports the functions whose tests passed before its process ended. The records of a TS/JS run (runner `jest_json`) follow §F2.8. The checks used for one function MUST share one recorded command and one package (for TS/JS, one harness file), and their streams MUST describe the same inputs.
- Live-baseline rule (§3.4): candidate-side and confirmation checks MUST NOT be replays. The first baseline run MAY be a replay only when its cache entry recorded at least two agreeing live runs. `diverged` always rests on the live `fuzz_base_confirm` check.
- Per input: *not recorded* when either first-pair record is missing; *unstable* when either first-pair record is flagged unstable; *compared* when the hashes and lengths are equal. When they differ, the confirmation pair decides: *unstable* when either revision does not reproduce its own first hash or is flagged unstable, *compared* and *diverged* otherwise, and *unconfirmed* without confirmation records. `inputs = compared + unstable + unconfirmed + not_recorded`, and `diverged <= compared`. Two kinds of unstable input separate the revisions: one flagged unstable on the candidate only, and a first-pair difference that the confirmation pair did not reproduce.
- Outcome: `diverged` when any input diverged; otherwise `inconclusive` when a difference could not be confirmed; otherwise `not_diverged` when both first-pair functions are complete, at least one input was compared, and no unstable input separates the revisions; otherwise `inconclusive`, with a reason that names the input being evaluated when a process ended or a function stopped. A timeout, a crash, a process exit, a `runtime.Goexit` or instability MUST NEVER yield `diverged`.
- The counterexample is the divergent input with the shortest call text, then the lowest index, with both display values, among the divergent inputs whose two displays differ. When every divergent input shows identical displays (the difference lies in a cut or redacted part), it is the shortest call overall, and the report says that the values shown are cut or redacted. It is the smallest divergent input tried with a visible difference; there is no shrinking. A divergence lists at most 32 divergent inputs, the counterexample first, then by call length and index. A row whose display is cut, redacted or bounded in the sandbox is marked truncated.
- Packages run sequentially in plan order. One confirmation pair runs for a package when its first pair shows a difference and the `fuzz.max_runtime_seconds` sub-cap has time left.
- A function whose package or module run recorded checks gets one `differential_fuzz` evidence record: `DIVERGED`, `NOT_DIVERGED` or `UNVERIFIED`, runner `go_test_json` for a Go harness and `jest_json` for a TS/JS harness, the harness path, the one test name, and the first pair as `check_id` and `base_check_id`. A function without a recorded run (sub-cap or deadline reached, harness not rendered or not staged) is `inconclusive` without evidence, and gets an Unverified line like every inconclusive function.
- Outcomes are derived only from recorded checks, by the same comparison whenever they are computed, so deriving them again from a saved report shows whether its recorded checks support the stored outcome. An edit that breaks a stream's structure rejects the stream. The comparison reads hashes, lengths, indices, flags, function states, commands, check kinds and the test outcomes in the logs; a consistent edit of those records gives the outcome they describe and is not detected. Display values, the counterexample text and log lines that do not decide a test outcome are not protected: editing them leaves the outcome unchanged while showing other values.

**Non-claims specific to fuzzing.** `not_diverged` says that the recorded encodings of the tried inputs were equal. It never establishes equivalent behavior, not even for those inputs: encodings are bounded and redacted, errors are compared by message, and other side effects (files, globals, standard output, goroutines) are not observed. `diverged` records a difference, never which revision is right. Code under review writes the stream of its own revision and can hide or fabricate its own differences. It cannot write the baseline's stream or change the comparison. The functions of one package run in one process, in plan order: state that an earlier function left behind (globals, caches, files) can cause a difference recorded for a later function, so a divergence is attributed to the function whose input was being evaluated, not necessarily to its cause. The report keeps the unsalted SHA-256 and the length of every recorded encoding, also when its display was redacted, so a low-entropy value such as a short password can be recovered from the report by hashing guesses.

**Acceptance (Go core).**

- The design's calc fixture, run in real containers with the sandbox isolation flags: `Percent` is `diverged` at `Percent(0, 0)`, with `int(0)` against `panic(error("runtime error: integer divide by zero"))`; `Discount` is `diverged` at `Discount(Cents(1000))`, with `calc.Cents(900)` against `calc.Cents(1000)`; `Join` is `not_diverged`; `Stamp` is `inconclusive` because every input is unstable; `Halt` is `inconclusive` with the candidate process-exit pointer at `Halt(7)`. It takes four checks, and neither snapshot keeps a harness file.
- The rendered harness compiles and runs with the go tool in a module that declares `go 1.13`, and its stream distinguishes panics, slice mutation after the call, NaN, negative zero, and nil versus empty.
- Every malformed stream in the test table is rejected as a whole, and every structurally edited normalized stream in the test table fails to parse.
- Streams of harmless values are accepted: a compiled `Endpoint(host string) string { return "tcp://" + host }` harness with its 64 seeded inputs, a display ending in a bare-host URL followed by records holding `@`, and 3,000 random streams of displays built from the fragments the redaction rules react to.
- A function whose every call text looks like a credential is skipped at selection, never rendered with zero inputs.

### F2.6 Harness runs and stage wiring

- The stage MUST run only in `review`, only when the trusted policy has `fuzz`, initial checks are enabled and `--fuzz` is not false, after the initial checks, coverage, changed baseline tests and impacted tests, and before mutation and the reviewer. `--fuzz=false` and `--checks=false` MUST record `disabled`; no changed files MUST record `no_candidates`; a review that executes nothing MUST record `not_run` with the stage reason and an Unverified entry. `lint` MUST NOT write a `fuzz` object.
- A change without an eligible Go function MUST record `no_candidates` without starting a container. Eligible functions with a `generated_test` template that is not a verifiable single-package `go test` template with the standalone `{package}` target MUST record `not_run` with the fixed reason and an Unverified entry; nothing runs. §F2.8 refines both rules for TS/JS functions and for a Vitest or Jest template.
- `Harness.RunObserved` MUST refuse, recording no check, a closed harness, a missing baseline, an unsupported template, a path that is not a clean new `_test.go` path in both snapshots, content whose parsed test declarations differ from the expected names (in order), and a test name already declared in the package directory on either revision. It MUST stage the identical bytes in both private snapshots without overwriting and remove them after the runs, run the reviewed template with `-json -count=1 -run ^(names)$` and the capture wrapper on the fixed observation path, and record the baseline run, then the candidate run. The runs MUST NOT use the reviewer's generated-test budget.
- Each run's timeout MUST be at most what remains of `fuzz.max_runtime_seconds`, counted from the start of the stage; no run may start after it. When a reviewer will run, the budget limit of every fuzz run MUST be `sandbox.max_runtime_seconds` minus the reviewer reserve.
- A baseline-side run (`fuzz_base`, `fuzz_base_confirm`) MUST become ERROR when its log is complete and no harness test recorded a `run` event, when its log is complete, the run passed and some harness test recorded no `run` event, or when a passing run returned no stream, a payload that is not one complete frame, or a stream the validator rejects. A log is complete when the output bound did not cut it and it is at least 4 bytes shorter than that bound (redaction could otherwise have cut its end). A cut log MUST NOT decide ERROR: the run keeps PASS or FAIL, and the comparison makes its functions `inconclusive`. The ERROR cause MUST be the last line of the recorded output, within the output bound, and MUST also be recorded in the audit event. A candidate-side run MUST keep the status the run gave it; its log MUST NOT decide ERROR (Appendix D.6).
- The normalized stream MUST be retained as a hashed `fuzz_observations` artifact, and stored in `Check.Results` only when it fits what remains of the results budget for the run's side; otherwise `results_sha256` names the artifact and the function is `inconclusive`. A rejected payload MUST be kept only as a bounded, redacted `fuzz_payload_rejected` artifact. The harness source MUST be retained as a hashed `fuzz_harness` artifact before its first run.
- `Harness.AddFuzzEvidence` MUST refuse any record that is not `differential_fuzz` with status `DIVERGED`, `NOT_DIVERGED` or `UNVERIFIED`, runner `go_test_json` or `jest_json`, exactly one test name, a path, and `check_id` and `base_check_id` naming exactly one recorded `fuzz_candidate` and `fuzz_base` check that `Harness.RunObserved` ran with that runner. No reviewer tool reaches it.
- Audit events MUST be `stage:run_fuzz` and `stage:run_fuzz_confirm`, one per run pair, naming the runner.

### F2.7 Report verification and rendering

- `verifyFuzz` MUST return a status only for a `differential_fuzz` record cited by exactly one fuzz function, whose test name and first-pair check IDs match that function, whose runner is `go_test_json` or `jest_json`, whose checks resolve once with the four fuzz kinds (the confirmation pair complete or absent), whose baseline command runs the harness's package and test (`go_test_json`) or names the harness file and selects no Go test (`jest_json`), and for which the comparison of §F2.5 and §F2.8 over those checks, with the record's runner, gives exactly the stored status and the function's stored outcome, reason, counts and counterexample. `DIVERGED` MUST rest on a live confirmation pair; `NOT_DIVERGED` MAY rest on a replayed first baseline run only with `live_runs >= 2`.
- `finalizeFuzz` MUST keep `diverged` or `not_diverged` only for a function whose evidence status was derived again with the matching status; every other function MUST become `inconclusive` with a reason and without a counterexample. It MUST restore the fixed note and seed scheme, MUST be idempotent, and MUST request human review for `not_run` and for every function that is not `not_diverged`.
- `fuzzDivergences` MUST emit one entry per diverged function whose evidence was derived again as `DIVERGED`: its path, line and symbol with anchor `changed_function`, the harness path and test name, the four check IDs, and up to 32 divergent inputs derived again from the streams, the counterexample first.
- Review targets MUST cover the candidate lines of every diverged function (high) and inconclusive function (medium). The Markdown section MUST show at most one value pair per function (the counterexample), MUST NOT show a percentage, and MUST pass every string through the Markdown escaper. When it lists a TS/JS function or skip, it MUST say that TS/JS functions, their signatures and their parameter types were read lexically, without a type checker.
- The fixed note is shown whatever the section status, so it MUST describe the method without saying that fuzzing ran. The stdout line and the Markdown lead of a section that ran MUST count the planned functions and, apart, those with recorded fuzz checks; they MUST NOT say that a function without recorded checks ran.

### F2.8 TS/JS differential fuzzing

Changed exported TypeScript and JavaScript functions run on the same seed scheme through a test file that the policy's Vitest or Jest `generated_test` template executes (Appendix D.13 of the integration contract). The rules of §F2.2 to §F2.7 apply except where this section says otherwise.

**Selection (lexical).**

- Selection MUST be lexical and MUST NOT execute anything: a bounded tokenizer and parser on the host, with no type checker and no compiler. Documentation and the Markdown section MUST label it as lexical.
- It reads modified or renamed non-test modules (`.ts`, `.tsx`, `.mts`, `.cts`, `.js`, `.jsx`, `.mjs`, `.cjs`), outside declaration files, test files, hidden directories, `node_modules`, `vendor`, `testdata` and sensitive paths. It recognizes function declarations (async, generator, default) and `const`, `let` or `var` bindings of function expressions and arrow functions at the top level, exported directly or through an export list without `from`. A **changed** function is one whose body tokens differ and whose signature text (type parameters, parameters, return type, `async` and generator markers, compared token by token) is identical on both revisions; others are not listed.
- Its cost MUST be linear in the size of the modules it reads, whatever they contain: at most 2 MiB per file and 32 MiB in total, at most 1,048,576 tokens per file, template literals nested at most 64 deep, and at most 16 token visits per token for the parse of a file; it stops at `fuzz.max_runtime_seconds` and at `--deadline`. A file that one of these bounds stops MUST get one file-level entry (line 0, no symbol) naming the bound, never a partial list.
- A changed function is planned only when every rule below holds; otherwise `fuzz.skipped` lists it with the reason of the first rule that fails:

| Rule | Skip reason |
| --- | --- |
| Not a `.cjs` or `.cts` module | CommonJS modules (.cjs, .cts) are not fuzzed in this version |
| The module is at the same path on both revisions | module was renamed or moved; the harness imports one path on both revisions |
| The module file name has only letters, digits, `.`, `_` and `-` | the module file name is not supported by the fuzz harness (letters, digits, '.', '_' and '-' only) |
| The exported name is `default` or an ASCII identifier | the exported name is not an ASCII identifier |
| Not a generator on either revision | generator functions are not fuzzed in this version |
| No type parameters on either revision | generic functions are not fuzzed in this version |
| No destructured parameter | destructured parameters are not fuzzed in this version |
| No `this` parameter | functions with a this parameter are not fuzzed in this version |
| Every parameter has a type: a TypeScript annotation in a TypeScript module, a JSDoc `@param {T} name` tag in the `/** ... */` comment right before the declaration in a JavaScript module | parameter a has no type annotation; parameter a has no JSDoc @param type (number, string, boolean or an array of them) |
| Every type is generated: `number`, `string` or `boolean`; an array of one of them (`T[]`, `readonly T[]`, `Array<T>`, `ReadonlyArray<T>`, JSDoc `Array.<T>`); for a rest parameter an array type or JSDoc `...T` | parameter type T is not generated in this version |
| JavaScript: the JSDoc types are identical on both revisions | JSDoc parameter types differ between revisions |
| At least one seeded input has a call text that redaction leaves unchanged | no seeded input has a call text that redaction leaves unchanged |
| A verifiable Vitest or Jest template (below) | TS/JS differential fuzzing needs a verifiable Vitest or Jest generated_test template (a {file} target and a {results_out} report) |
| Jest: the module path contains none of `( ) [ ] { } $ ^ + * ? \|` and `\` (Jest reads the `{file}` argument as a regular expression) | the module path contains a character that Jest reads as a regular expression in its test path pattern (one of ( ) [ ] { } $ ^ + * ? \| \\) |
| Vitest: no directory of the module path is `dist` or `cypress` (Vitest's default `exclude`) | the module is under a dist or cypress directory, which Vitest excludes from test files by default |

- An optional parameter or one with a default value MUST get a value on every call. The recorded symbol is the exported name; the recorded signature is the lexical signature text, followed for JavaScript by `with JSDoc types (...)`. A module is budgeted as one package and ordered by the rules of §F2.1. Functions of the language that the template cannot run MUST be skipped before the budget is applied, so that they take no share of `max_packages` or `max_functions`.
- When nothing is planned and at least one function meets the eligibility rules but was skipped only because the template cannot run it (the last three rules above, or the Go template rule below), the section reason MUST be "no changed function can run with this generated_test template"; otherwise it is "no changed Go or TS/JS function is eligible for differential fuzzing". Rendering MUST refuse a module that the runner rules above refuse.

**Template.** A TS/JS function MUST run only when `generated_test` is a verifiable JavaScript/TypeScript template (exactly one standalone `{file}` target, the runner's JSON report on `{results_out}`, no `{package}`, and no package-manager script or shell as the command) whose arguments name `vitest` or `jest` (not both). The harness file is its `{file}` target and no test-name filter is added. The runner's report written to `{results_out}` MUST NOT be captured: the observation stream is the only payload of the run (R4). A Vitest or Jest template is never a Go fuzz template: with one, every eligible Go function MUST be listed in `fuzz.skipped` with "Go differential fuzzing needs a single-package go test generated_test template with the {package} target; this template is a Vitest or Jest template", the TS/JS functions of the change MUST run, and the skipped Go functions MUST get one Unverified entry ("Differential fuzzing did not run on N changed Go functions: ..."), within the bound of 20 fuzz Unverified entries. When no TS/JS function is planned either, the section is `not_run` with the reason and the Unverified entry of §F2.6.

**Corpus.** The seed and the order are those of §F2.2, with the module path in place of the package directory. The value tables are JavaScript: numbers are IEEE 754 doubles, with edge values 0, ±1, ±2, ±0.5, 7, 10, ±100, 255, 256, 1000, 65535, the 32-bit bounds, 4294967295, ±(2^53 − 1), 2^53, 0.1, 1e-7, 1e21, ±1e300, the largest and smallest doubles, both infinities, NaN and negative zero, and random values that are small integers, 32-bit integers, arbitrary doubles or values in [−1000, 1000); strings are UTF-16, with the edge values of §F2.2 plus a lone surrogate, `"undefined"`, `"NaN"`, `__proto__` and `constructor`, and random strings with lone surrogates in place of invalid UTF-8; booleans take both values; arrays take empty, one element, two elements in both orders, a repeated element, and four elements ascending and descending, and random arrays have 0 to 8 elements (there is no nil array). Call texts MUST be JavaScript, with numbers written as ECMAScript's `Number::toString` writes them except negative zero (`-0`), strings as double-quoted literals that evaluate to the same code units, and a rest parameter spread (`sum(1, ...[2, 3])`).

**Harness.**

- One test file per module, `<dir>/swiftproof-fuzz-<suffix>.test.ts` (`.test.js` for a JavaScript module), next to the module, with `<suffix>` drawn at random for each run. Its tests MUST be top-level `test(...)` calls at column 0 with the static titles `TestSwiftProofFuzz_<suffix>_<n>`, one per function, in plan order, so that the file satisfies the rules of generated JavaScript/TypeScript tests. With Vitest it imports `test` from `vitest`; with Jest it uses the global. Its first line MUST be `// @ts-nocheck`.
- The only repository-derived text in the file MUST be the import specifier (`./<name>` without the extension for `.ts`, `.tsx`, `.js` and `.jsx`, `./<file name>` for `.mjs` and `.mts`) and the exported names, each re-checked; the functions are imported under aliases that carry the per-run prefix. Every literal MUST be rendered by SwiftProof.
- The harness MUST NOT assert anything. For every input it evaluates the function twice with fresh arguments. The call MUST run inside `node:vm` with `call_timeout_ms`, which interrupts synchronous code; a returned native promise is awaited for the rest of that time. The encoding records the result as `resolved(...)`, `rejected(...)` or, for a synchronous exception, `throw(...)`, and each array argument after the call (not a rest parameter, which is a fresh array inside the function). Numbers keep NaN, the infinities and negative zero apart; strings are quoted in printable ASCII; `undefined` and `null` stay apart; objects carry their constructor name and their own enumerable keys in sorted order, and getters MUST NOT be called; `Map` and `Set` entries are sorted by encoding; errors are encoded as name and message, never with a stack; cycles are marked; proxies are not inspected. The encoding is bounded as in §F2.3.
- A timeout ends the function with a `timeout` stop and poisons the process; a failure of the harness itself ends it with an `abort` stop. There is no `goexit` stop.
- Each test writes, before its function's records, a **head** record `{"f":<n>,"k":"head","s":"<suffix>","w":"<test name>"}`, and after its end or stop record a **done** record `{"f":<n>,"k":"done","w":"<test name>","n":<observations written>}`. The per-test framework timeout is two evaluations of every input at `call_timeout_ms`, plus 60 s.

**Stream and named execution.**

- Validation is all-or-nothing as in §F2.4. In addition, every function MUST open with its head record carrying this run's suffix and its test name, then its begin record, and after its end or stop record MUST come its done record with the number of observation records, before the head record of the next function; `goexit` stops are refused, also when a stored stream marked `jest_json` is parsed again. A function whose records end before its done record MUST be `interrupted`, even after an end or stop record. The normalized stream is the §F2.4 stream marked `"runner": "jest_json"`; a Go stream carries no runner mark, and any other mark MUST NOT parse.
- The records of a TS/JS run MAY be used for a function only when the check is PASS with exit code 0, its stream is a TS/JS stream that parses and plans the same number of inputs (so the payload was one complete frame), and the function's test wrote its head and done records (its state is `complete` or `stopped`). The framework's log MUST NOT be read for a verdict, and a truncated log does not matter. A failing run supports no test; its stream only names the input being evaluated when the process ended. The same holds for the tests of a passing run whose stream ended inside a test: the reason MUST name the input being evaluated when the process ended, and the later tests "ended before this function was evaluated". A `jest_json` fuzz record MUST NOT be validated with the Jest-compatible report of generated tests.

**Runs and status rules.** `Harness.RunObserved` with runner `jest_json` MUST refuse, recording no check, a template that is not a verifiable JavaScript/TypeScript template, a path that is not a clean new `.test.ts` or `.test.js` path in both snapshots, content whose top-level test declarations are not exactly the expected names in order, and a run without the started-test counter that the status rules below need. A TS/JS baseline-side run MUST become ERROR when it returned no payload at all, when it passed and its stream ended between two tests (fewer started tests, by head record, than the harness declares, and every started test wrote its done record), and in the cases of §F2.6 that do not read the log (a passing run without a readable stream, a lost artifact, an infrastructure cause). A baseline run whose stream ended inside a test (a head record without its done record) MUST keep PASS or FAIL: the baseline process ended while it evaluated an input, which is behavior of the baseline code (under Jest, a `process.exit(0)` in the code under test ends the run with exit code 0 and leaves the later tests unrun). A failing baseline run that started the harness stays FAIL. A baseline function that leaves a timer behind which ends the process with exit code 0 between two tests is not told apart from a runner that skipped a test, and gives ERROR. A candidate-side run MUST keep the status the run gave it.

**Acceptance (TS/JS).**

- The unit tests cover every selection rule and reason above (the Jest and Vitest path rules with every refused character and directory, and their refusal at rendering), the JSDoc reading, the language decision of the template (Go functions skipped before the budget under a Vitest or Jest template, TS/JS functions under any other, and the section reason when nothing can run), the template families (`vitest`, `jest`, `npx`, refused package scripts and templates without a report), the JavaScript number and string renderings, the corpus scheme (order, prefix property, module-dependent seed, no nil arrays, rest parameters), the rendered harness (column-0 titles equal to the test names, prefixed import aliases, refused unsafe names and file names, halving to fit the payload), every malformed framing, a stored `jest_json` stream with a `goexit` stop, the TS/JS usability rule (including a passing run whose stream ended inside a test), the baseline-side status rules (a stream that ended between two tests is ERROR, one that ended inside a test is not) and the runner check of `Harness.AddFuzzEvidence`, and a report whose record names the Go runner, whose command does not run the harness file, whose stream is a Go stream, whose candidate run failed or whose streams lack done records: each makes the diverged function `inconclusive` on re-render.
- With real Docker and real Vitest and Jest (ts-jest) runs through `Harness.RunObserved`, a cart module gives the same outcomes with both runners: `discount` `diverged`, `label` `not_diverged`, `lookup` `diverged` at `lookup("")` with `rejected(error("Error", "empty id"))` against `resolved("")`, `sorted` `diverged` at `sorted([1, 0])` (an argument sorted in place), `stamp` `inconclusive` (every input unstable), `spin` `inconclusive` (a synchronous loop stopped by the call timeout at `spin(7)`); four PASS checks, `jest_json` evidence, no harness file left and no container of the run left. Jest without a TypeScript transform gives a baseline ERROR and a candidate FAIL.
- Through the CLI with real Docker and a Vitest template, the cart module and a JavaScript module with JSDoc types exit 2 with `--ci`, never 1 or 4; three divergences with four check IDs; an identical re-render; one tampered hash at `lookup`'s counterexample makes only `lookup` inconclusive on re-render; the checkout unchanged. Through the CLI with real Docker and a Jest (ts-jest) template, a module whose `run(args: string[])` calls `process.exit(0)` on the empty array on both revisions, with a second changed function: exit 2 with `--ci`, two PASS fuzz checks with exit code 0 and no ERROR check, both functions `inconclusive` ("the baseline process ended while evaluating input 0: run([])", "the baseline process ended before this function was evaluated"). Through the CLI with a Vitest template and an image that does not exist, a change to a Go function and a TS/JS function: the TS/JS function runs (fuzz checks), the Go function is skipped with the Go template reason and one Unverified entry, and takes no share of `max_packages` 1.

**Acceptance (end to end, Go).**

- The calc fixture plus `Boundary` through `Harness.RunObserved` with real Docker: four checks (`fuzz_base` PASS, `fuzz_candidate` FAIL, `fuzz_base_confirm` PASS, `fuzz_candidate_confirm` FAIL, never ERROR), the outcomes of §F2.5, `Boundary` `not_diverged` with the baseline display `string("uid=65534 source-write-refused=true")`, six evidence records in plan order, hash-verified `fuzz_harness` and `fuzz_observations` artifacts, no harness file left in any snapshot, and no container of the run left.
- Through the CLI with real Docker, on a repository whose candidate also breaks the build of a second package, declares the unsuffixed harness names, and rewrites an exported TypeScript function: exit 2 with `--ci`, never 1 or 4; no ERROR check; `Percent` and `Discount` diverged, `Join` and `Boundary` not diverged, `Stamp`, `Halt` and the function of the package that does not build inconclusive with FAIL candidate checks; the TypeScript function in `fuzz.skipped` with the template reason (the template is a Go one); two divergences with four check IDs; an identical re-render; one tampered hash at `Discount`'s counterexample makes only `Discount` inconclusive on re-render; the checkout unchanged; the run's temporary directory empty.
<!-- F2:end -->

## F3. Baseline versions of changed tests on candidate code

Opt-in with `--base-tests`, review only, Go execution only. The baseline version of each Go test function the change modified or removed runs on the baseline tree and on a hybrid tree: the candidate tree with the affected packages' test files reverted to the baseline. Evidence kind `base_test_differential`, one record per test name; check kinds `base_test_base` and `base_test_hybrid`. The lexical test-edit signals for Go, JS/TS and Python are heuristics, never evidence.

<!-- F3:begin -->
**Selection.**

- Selection MUST be static: committed baseline and candidate blobs, parsed with `go/parser`; repository code MUST NOT run to select tests.
- A changed `*_test.go` file with status modified, deleted, or renamed from a test path is considered; added and copied files, and files under `testdata` or `vendor` or below a name starting with `_` or `.`, MUST NOT be. A runnable baseline test is selected as `removed`, `modified` (token digest differs, or the candidate file cannot be read or parsed), `shared_code_changed` or `file_deleted`. `shared_code_changed` MUST cover: another baseline declaration of the file disappeared or changed; the package clause, the build constraints before it, the import set or a compiler directive comment changed; the candidate added `init`, `TestMain`, a package-level variable with an initializer, or a method of a type it did not newly declare; a rename moved the file to another directory or changed the GOOS/GOARCH suffix of the file name. Comment-only and indentation-only edits MUST NOT select a test unless they change a build constraint or a compiler directive.
- At most 50 files are analyzed and 100 tests selected; every unreadable or unparsable file and every overflow MUST be reported as an unverified area. No selection together with such a note is `not_run`, never `no_candidates`. A `no_candidates` text MUST say only that no test was selected, never that the change modified no test.

**Execution.**

- The `generated_test` command MUST pass the verifiable Go template rule; otherwise nothing runs and the section is `not_run`.
- Each unit (a package directory, or a file with a `{file}` template; at most 50 names, at most 10 units) runs one `base_test_base` check on the baseline snapshot and, only when that check passed, one `base_test_hybrid` check with the identical command on the hybrid tree. The hybrid tree MUST be a private copy of the sanitized candidate snapshot in which exactly the unit directory's `*_test.go` files and `testdata` are replaced by the baseline's, built on the host, recorded in a hashed `base_test_hybrid_manifest` artifact and deleted after the stage. A candidate entry that blocks a baseline path MUST be removed and recorded; a directory that still cannot be reverted MUST leave only its own tests `UNVERIFIED`. Both checks MUST use the unchanged sandbox profile.
- A baseline check that failed as a whole MAY be repeated for the tests it records as passed, and a test that passed inside a hybrid check that failed as a whole MAY get one run pair of its own. Neither changes the classification rules. A hybrid check MAY be skipped when no selected test of its unit passed on the baseline.
- A replayed baseline check MUST NOT support `FAILS_ON_CANDIDATE`: the baseline runs again live with the same kind and command, and the test is classified against that run.
- Runs MUST honor the 180 s sub-cap, the reviewer reserve and the shared budget (§1.3). A candidate-side compile or run failure MUST stay FAIL, never ERROR.

**Evidence and statuses.**

- One `base_test_differential` record per test and recorded run pair: runner `go_test_json`, exactly one test name, `check_id` the hybrid check, `base_check_id` the baseline check.
- `FAILS_ON_CANDIDATE` and `PASSES_ON_CANDIDATE` MUST follow `ClassifyExistingTest`: identical commands; baseline PASS with exit 0, an untruncated log, and exactly one run and one pass; candidate FAIL with exit 1–124 and exactly one fail, or PASS with exit 0 and exactly one pass. Everything else is `UNVERIFIED` with a reason.
- `Finalize` MUST re-derive every item status from the recorded checks, including on re-render, and MUST NOT let a `base_test_differential` record support any hypothesis status.
- Outputs MUST NOT present `FAILS_ON_CANDIDATE` as a reproduced issue, as proof that the test edit is wrong or deliberate, or as proof that the baseline assertion is intended; nor `PASSES_ON_CANDIDATE` as preserved behavior, an equivalent test, or tamper-proof.
- Exit effects: never 1; `FAILS_ON_CANDIDATE`, `UNVERIFIED` and `not_run` request review; an infrastructure ERROR of a run, or a host-side failure to make the private candidate copy or retain the manifest, is exit 4. The layout of the candidate tree MUST NOT produce exit 4.

**Lexical signals.** `test_assertion_removed`, `test_case_removed`, `test_skip_added` (at most 10 per file), `test_expectation_relaxed` (per hunk, at most 10 per file) and `test_focus_added` (high, at most 5 per file) apply to Go, JavaScript/TypeScript and Python test files in `lint` and `review`. They MUST be labelled as text heuristics and MUST NOT be evidence.

**Acceptance.**

- A candidate that drops a bound and loosens the test asserting it yields `FAILS_ON_CANDIDATE` for that test, with a live baseline PASS and a hybrid FAIL under one command: exit 2 with `--ci`, 0 without, never 1.
- A test moved to a new candidate file of the same package still compiles on the hybrid tree.
- A candidate that turns a test file off with a build constraint, without editing any test function, selects every test of that file, and the baseline test that asserted the dropped bound yields `FAILS_ON_CANDIDATE`.
- A candidate file that replaces a test-only package directory yields a normal run pair (exit 2 with `--ci`), not exit 4.
- An API rename yields `UNVERIFIED` tests and FAIL checks, never ERROR or exit 4.
- An unverifiable template records `not_run` and runs nothing; `--base-tests` on `lint` or with `--checks=false` exits 3 before any container.
- Tampered records (wrong kinds, differing commands, duplicate IDs, a replayed baseline for `FAILS_ON_CANDIDATE`, forged pass events) finalize `UNVERIFIED`, and `Finalize` is idempotent.
<!-- F3:end -->

## F4. Mutation of added lines

Opt-in through the `mutation` policy object, review only, Go only. Deterministic single-change mutants of added lines in changed non-test Go files run the package's tests in a private copy of the candidate, after one passing control run per package, in a separate check ledger (`mutation-check-N`). Surviving mutants become medium `surviving_mutant` signals. Mutation creates no evidence record and no hypothesis, and computes no score.

<!-- F4:begin -->
### F4.1 Rules

Trust and configuration:

- Only the trusted policy (the tip of the base ref, or `--config`) MAY define `mutation`. A candidate policy MUST NOT add, change or enable it. `init` MUST NOT write the key (§7).
- All four fields are required and non-zero. `command` MUST have 3 to 128 arguments of at most 16 KiB without NUL; `argv[0]` MUST be `go` by base name and `argv[1]` MUST be `test`; exactly one argument MUST be the standalone `{package}` and exactly one MUST be `-json`; no other placeholder MAY appear anywhere; every other argument MUST be a `-flag` or `-flag=value`, and `--`, `-args`, `-C`, `-exec`, `-overlay`, `-run`, `-skip`, `-list`, `-c`, `-o`, `-fuzz`, a second `-json` and `-test.*` MUST be refused in their one- or two-dash spellings. `max_mutants` MUST be 1 to 200, `timeout_seconds` 1 to `sandbox.timeout_seconds`, and `max_runtime_seconds` from `timeout_seconds` to `sandbox.max_runtime_seconds`. A violation MUST exit 3 before any container starts.
- The stage MUST run only in `review`, only with initial checks enabled and a non-empty change, after differential fuzzing and before the reviewer. Reviewer experiments MUST NOT start, parameterize or cite it.

What is mutated:

- Only added (new-side) lines of changed, non-deleted, non-binary `.go` files that do not end in `_test.go` MAY be mutated, and only inside function bodies. A mutant MUST be generated only when every line of its replaced span is an added line.
- A file MUST be skipped, with a fixed reason recorded in `mutation.files`, when a path component starts with `_` or `.` or is `testdata` or `vendor`, the path contains `...`, its name has a GOOS or GOARCH suffix (the `go/build` rule, which reads the name up to its first dot), it has a `//go:build` or `// +build` constraint, it is generated, it imports `C`, it does not parse, it cannot be read from the sanitized candidate snapshot, or its directory has no `*_test.go` file.
- Mutants MUST be found by host-side parsing only (`go/parser` over at most 1 MiB per file). Repository code MUST NOT be executed or type-checked with importers to find them. The operators are `drop_error`, `negate_condition`, `boundary`, `negate_comparison`, `swap_logical`, `increment_constant`, `flip_boolean` and `swap_arithmetic` (MUTATION.md). `drop_error` MUST NOT be generated when the dropped expression holds every use of an imported package in the file.
- Applying a mutant MUST find its original text at the recorded offsets, keep the number of lines, not merge with a neighbouring character into another token, and re-parse; otherwise the mutant is `INCONCLUSIVE` and is not run. Enumeration MUST NOT generate a mutant that cannot be applied: a dropped result spanning several lines becomes `nil` followed by as many line breaks, and a replacement that would merge with a neighbouring character is not generated.
- When a coverage run passed and was measured, candidate mutants on added lines it reported as not executed SHOULD be skipped and counted in `coverage_skipped`. Coverage data MUST NOT otherwise change the stage.
- At most `max_mutants` candidates MUST be selected, breadth-first and deterministically (the first operator of each line across files in path order, line by line, then further operators); the rest are counted in `dropped`. IDs `mutant-N` MUST follow the execution order (package, path, line, column, operator).

Runs:

- Each package with selected mutants MUST get one unmutated control run of the command with `{package}` expanded and nothing appended (kind `mutation_control`). Its mutants MUST run only when the control passed with exit code 0 and an uncut log, recorded at least one passing and no failing top-level test, all in one package, and no build, vet or timeout marker.
- Every control and mutant MUST run in a private copy of the sanitized candidate snapshot, under the unchanged sandbox profile, in the mutation ledger (`mutation-check-N`, logs kept as `mutation_check_output` artifacts). The candidate and baseline snapshots MUST NOT be written. A mutant MUST overwrite one existing regular file only after its content matched the planned original, and MUST be restored and read back afterwards; any failure MUST abort the stage before another run and MUST be an operational failure.
- Runs MUST be sequential and charged to the shared budget, MUST stop at `mutation.max_runtime_seconds` counted from recorded durations, MUST respect the pre-reviewer ceiling (§1.3), and MUST NOT exceed `timeout_seconds` each; a mutant run MUST NOT exceed three control durations plus 30 s. No verdict MAY depend on tee data.

Outcomes:

- Outcomes MUST be derived from the recorded control and mutant checks only, per top-level test name: `SURVIVED` (mutant PASS, exit 0, uncut log, at least one passing and no failing test in the control's package, identical command, patch retained), `KILLED` (mutant FAIL, exit 1 to 124, uncut log, at least one failing named test in that package, identical command), `INVALID` (build or vet failure), `TIMEOUT`, `INCONCLUSIVE` and `NOT_RUN` as in MUTATION.md. A run ended by the overall deadline MUST be `INCONCLUSIVE`, not `TIMEOUT`. A run that the sub-cap, the shared budget or the reviewer reserve left less than its own time limit (the policy timeout or three control durations plus 30 s), and that reached that limit, MUST be `INCONCLUSIVE`, not `TIMEOUT`, and the stage MUST stop. Classification MUST read each log once, so that its cost grows with the size of the logs and not with the number of test names. Outcomes are read from logs that code executing in the sandbox can write; outputs MUST NOT present them as tamper-proof.
- A `SURVIVED` mutant MUST keep its redacted patch as a `mutant_patch` artifact whose sha256 is `patch_sha256`, and MUST add one medium `surviving_mutant` signal on the new side of its line. If the patch cannot be kept, the mutant MUST be `INCONCLUSIVE` and add no signal.
- Section status: `no_candidates` when nothing was selected; `ran` only when every selected mutant is `KILLED`, `SURVIVED`, `INVALID` or `TIMEOUT` and `dropped` is 0; `not_run` when the sandbox never ran a control or mutant command; `incomplete` otherwise. `incomplete` and `not_run` MUST add one Unverified sentence and request human review, including a `not_run` section recorded because the stage never started (`--checks=false`, a failed preparation).
- `report.Finalize` MUST re-derive every `KILLED` and `SURVIVED` mutant from `mutation.checks` and `artifacts` as MUTATION.md describes (including that no other mutant cites its mutant check, no other surviving mutant cites its patch hash, and its mutant command is `mutation.command` with `{package}` expanded), without mutating the report, MUST turn every other one into `INCONCLUSIVE`, MUST recompute the counters, MUST downgrade a section that no longer satisfies `ran` to `incomplete`, and MUST be idempotent.
- Mutation MUST NOT create an evidence record or a hypothesis, support any hypothesis status, delete a signal, lower a severity, support a dismissal or produce exit 1. Mutation checks MUST NOT count as failing checks. An infrastructure ERROR of a mutation run, or a workspace failure, MUST exit 4; log text MUST NOT make these checks ERROR.
- Outputs MUST NOT present a surviving mutant as a bug, a missing test, dead code or an unexecuted line, a killed mutant as assurance, `ran` as exhaustive, or any mutation score or percentage (§8). Killed mutants MUST NOT be listed in Markdown, stdout or exports.

### F4.2 Acceptance

- On a fixture adding `Discount(total int) (int, error)` whose test asserts only `Discount(200) == 190` and an error for `Discount(-5)`, with `max_mutants: 10`: exit 0 with `--ci`; `mutation.status` `ran`; 8 generated and selected; the two `negate_condition`, the `drop_error` and the `swap_arithmetic` mutants `KILLED`; the two `boundary` and the two `increment_constant` mutants `SURVIVED`, each with a patch artifact whose hash matches and a medium signal that reaches the review targets; the Windows-only file skipped; one control and eight mutant checks in the mutation ledger, all with `./price`; `checks` holding only the initial checks; a clean checkout and no remaining container. A second run gives identical mutant IDs, operators, statuses and patches, and `swiftproof report` re-renders byte-identically.
- `max_mutants: 3` selects the first operator of each of the first three mutated lines, and the section is `incomplete` with exit 2 under `--ci`.
- A `-run` flag in the command exits 3; a binary before v0.4.0 exits 3 on the policy; an absent sandbox image gives `not_run`, exit 4 and no survivor; `--checks=false` gives `not_run` and exit 2 under `--ci`; `lint` writes no `mutation` object.
- Tampering with a saved report (colliding or duplicated check IDs, a patch hash mismatch, a cut log, a forged `tests_run`, a claimed `SURVIVED` for a killed mutant, a mutant citing another mutant's check or patch, a run whose command is not the section's command) makes the mutant `INCONCLUSIVE` and the section `incomplete` on re-render.
- A mutant inside the private copy sees the unchanged sandbox boundary (non-root, read-only source and root filesystem, loopback only, no host environment), and the candidate, baseline and restored workspace trees are byte-identical before and after.
<!-- F4:end -->

## F5. Intent-linked candidate-only tests

Acceptance criteria are parsed from `--intent` / `--intent-file` (criteria grammar v1), with positional IDs `AC-N` and the SHA-256 of the intent text. When criteria exist, the reviewer is offered `create_intent_test` and `run_intent_test`, which run a model-written test for one criterion on the candidate only. Evidence kind `intent_test`; hypothesis status `INTENT_TEST_FAILED`; `intent_judgment` only on `DIVERGED` hypotheses. There is no policy key and no flag.

<!-- F5:begin -->
### F5.1 Intent input and criteria

- The intent (`--intent` or `--intent-file`, at most 64 KiB) MUST be valid UTF-8 without NUL; otherwise the command MUST exit 3 ("intent: intent must be UTF-8 text without NUL bytes") before Git analysis, any container and any provider call.
- Every block from `<!-- swiftproof:pr-comment:begin v1 -->` to `<!-- swiftproof:pr-comment:end -->` MUST be removed before parsing, an unterminated begin marker removing the rest and a stray end marker being removed, with the Unverified note "SwiftProof PR-comment output was removed from the intent text." The removal MUST repeat until neither marker occurs (at most 8 passes, after which the text is cut at the first remaining marker). The stripped text MUST then be redacted as report sanitizing redacts strings; `intent` MUST be that text, `intent_sha256` its SHA-256, and the criteria MUST be extracted from it, so that the hash never covers text that redaction hides from the report.
- Criteria grammar v1: criteria MUST be Markdown list items (`-`, `*`, `+`, `N.`, `N)`), with a leading task checkbox removed and indented continuation lines joined with one space; when any ATX heading contains "acceptance criteria" or "acceptance criterion" (case-insensitive), only items inside such sections count (a matching heading of level L opens a section that the next heading of level L or higher closes); fenced code blocks and thematic breaks MUST be ignored. IDs MUST be positional (`AC-1` … `AC-100`); items beyond 100 and items longer than 1024 bytes MUST NOT be numbered and MUST add a fixed Unverified note with their count.
- Extraction MUST be host-side, deterministic and executed by `lint` too, with no execution and no provider call. Outputs MUST NOT present extraction as understanding of the intent, or the absence of criteria as the absence of requirements.

### F5.2 Intent tests

- `create_intent_test` and `run_intent_test` MUST be offered only when the run has criteria; without criteria the provider-facing tools, hypothesis schema and prompt MUST be unchanged. `criterion_id` MUST be refused on every other tool.
- `create_intent_test` MUST apply every `create_test` rule, MUST refuse a criterion ID that does not occur exactly once, MUST refuse a test for which the trusted `generated_test` template has no named-test runner SwiftProof can check (`go_test_json` or `jest_json`), and MUST limit intent tests to half of `reviewer.max_generated_tests`, rounded up, inside the shared budget. A refused call MUST create nothing.
- `run_intent_test` MUST stage the test in the candidate snapshot only, run it once as a check of kind `generated_test_intent`, remove it, and record exactly one `intent_test` record with `check_id`, `criterion_id`, `runner`, `path` and `test_names`, and no `base_check_id`. It MUST NOT run anything on the base snapshot.
- The record MUST be `INTENT_TEST_FAILED` only when the validated named execution failed with exit 1..124, untruncated, on an assertion of the test's own file (Go: a `<file>:<line>: <message>` output line of the named test or its subtests from the test's own file with a non-empty message, and no `panic:` output in the run; Jest-compatible: a failed named top-level test with a non-empty failure message, an empty file-level message, and every non-empty failure message of a failed named test starting, color codes removed, with `AssertionError:`, `AssertionError [`, `Error: expect(` or `Error: expect.`), and `referenced_symbols` holds 1 to 32 identifiers of which at least one occurs as a whole word on a redacted added line of a changed, non-deleted, non-test file whose path is neither secret-bearing nor altered by redaction. It MUST be `INTENT_TEST_PASSED` for a validated pass with exit 0, untruncated, whose `referenced_symbols` is not empty, and `UNVERIFIED` otherwise, with a fixed reason; an empty intersection MUST record "the intent test references no symbol the change added or modified" whether the test failed or passed. (`report.Finalize` re-derives it with the same rule.)
- `referenced_symbols` MUST be computed by the harness: Go test identifiers and selector names (`go/ast`) intersected with the top-level declarations that contain an added line of a changed non-test Go file; JavaScript/TypeScript lexical identifiers intersected with lexically found changed top-level declarations, labelled lexical. Test files for these rules MUST include `_test.go`, JavaScript/TypeScript `<name>.test.<ext>` and `<name>.spec.<ext>` for `.js`, `.jsx`, `.mjs`, `.cjs`, `.ts`, `.tsx`, `.mts` and `.cts`, and JavaScript/TypeScript files under `__tests__`. The changed declarations and the words of the added lines MUST be computed at most once per run, so that the host-side cost stays linear in the size of the diff whatever the number of intent tests and symbols, and that work MUST stop when the run's context ends, without recording symbols.
- A test recorded `INTENT_TEST_FAILED` MUST be retained as a hashed `intent_test` artifact and MUST NOT be deletable; when retention fails the record MUST be `UNVERIFIED`. A setup failure, an inherited setup-failure marker in the log (which code under test can also write), a skip, an unrelated failure, broken framing, truncated output or a missing Jest-compatible report MUST remain an ERROR check (exit 4) under the inherited generated-test rules.

### F5.3 Hypotheses, links and report

- `report.Finalize` MUST re-derive every `intent_test` record from a check that resolves exactly once, has kind `generated_test_intent` and a command, and was not replayed, with the harness's function, and only a re-derived status equal to the stored one counts. A record whose criterion does not occur exactly once, whose path or test names redaction would alter, or which cites a base or repeat check MUST support nothing.
- A hypothesis MUST be `INTENT_TEST_FAILED` only when it cites a record re-derived as `INTENT_TEST_FAILED` whose criterion equals the hypothesis's `criterion_id`; it MUST then be copied to `intent_test_failures`, MUST request human review and MUST NOT enter `reproduced_issues` or produce exit 1. `intent_test` evidence MUST NOT support any other status, and `INTENT_TEST_PASSED` MUST support none.
- `intent_judgment` MUST be accepted only on a `DIVERGED` claim with a known criterion, MUST be dropped by `Finalize` from any other final status with one fixed Unverified note, MUST be rendered only with the label "Model judgment (not evidence)", and MUST NOT be read by any status, list, severity, target, surface or exit-code computation. An unknown `criterion_id` MUST be dropped with the same note.
- The Markdown MUST render "Intent Test Failures" and "Intent Criteria" (§6.2 item 9) only when an intent, criteria or failures exist, quote criteria from `intent_criteria` only, list at most 10 evidence IDs per criterion, and never count criteria as a ratio or present a passing test as the criterion holding. The stdout line MUST appear only with criteria; without an accepted failure it MUST give the extraction count alone when no intent test ran, and otherwise say that no failure was accepted and that this says nothing about whether the criteria hold.

### F5.4 Acceptance

- A real Go review with a scripted provider: the intent test for AC-1 fails on an assertion naming the new `Discount`, and the hypothesis citing it is `INTENT_TEST_FAILED`, listed in `intent_test_failures`, with exit 2 with `--ci` and 0 without, never 1; the passing AC-2 test is `INTENT_TEST_PASSED` and a claim citing it is `UNVERIFIED`; two `generated_test_intent` checks and no baseline check; one `intent_test` artifact; the checkout stays clean and the re-render is identical.
- A test that references no changed symbol (failing or passing), a panic, a runtime error and a thrown `Error` give `UNVERIFIED`; a compile failure gives an ERROR check (exit 4) and an `UNVERIFIED` record.
- The same review of a TypeScript change with a Vitest template records `INTENT_TEST_FAILED` through `jest_json`, with lexically matched symbols.
- A judgment on a non-`DIVERGED` final status and an unknown criterion are dropped with a note; a marker block in the intent is stripped with a note; a binary intent exits 3; `lint` with an intent makes no provider call.
- A forged, stripped or tampered intent-test failure in a saved report becomes `UNVERIFIED` on re-render.
<!-- F5:end -->

## F6. Symbol index and impact analysis

The core (on by default, `--impact=false` disables it, lint and review) builds a static Go index on the host from committed Git objects, never executing repository code. It records callers of changed functions as capped, low `impacted_caller` signals, backs the reviewer's `find_references`, `inspect_symbol` and `find_callers` tools with a lexical fallback, and fills the `impact` object. With `--impacted-tests` (review only), the existing tests that statically reach changed functions run on both revisions: evidence kind `impacted_test_differential`, one record per test name; check kinds `impacted_test_base` and `impacted_test_candidate`.

<!-- F6:begin -->
### F6a. Static index, callers and tools

Rules:

- The index MUST read the candidate only as committed Git objects, through the tree and blob readers snapshots use, and MUST NOT execute repository code or any tool (the go command, cgo, code generation, gopls), load imports from outside the repository, or use the network. It MUST skip symlinks, submodules, `testdata`, `vendor`, directories and files whose name starts with `_` or `.`, and every path snapshots exclude.
- Files MUST be selected with linux/amd64 build constraints and type-checked per package in dependency order, with the repository's own packages only. A panic inside the type checker MUST be contained to its package. Recorded positions MUST be the parsed file's own path and line: `//line` directives MUST NOT move them.
- `impact` MUST be present in `lint` and `review` unless `--impact=false`, and absent then. Its status is `not_applicable` when no indexable Go file changed (Go files under `testdata`, `vendor`, ignored directories or sensitive paths are not indexed), `indexed`, `limited` with a reason naming every known gap (skipped, unparsable or misplaced files, unreadable or duplicate modules, stopped type checks, reference and time limits, changed files that could not be compared, changed functions beyond 200, caller or reaching-test searches stopped by their visit, declaration or time limits, interface methods left unchecked beyond 200 per method name, implementation checks not made because their estimated cost exceeds the bound, interfaces that a generic receiver type may implement only once instantiated), or `unavailable` when no index could be built (no `go.mod`, file, byte or package limits, Git failure). Only a cancelled run MAY end the analysis with an error.
- A changed function is a function or method of a changed non-test Go file whose token digest (`linter.TokenDigest`) differs from its baseline version: `signature_changed` when the tokens before the body differ, else `body_changed`. Added and removed functions, `init`, blank functions and `main` in package `main` MUST NOT be listed. A changed function the index does not hold MUST be listed with `indexed: false` and a reason.
- Callers MUST be reference sites in unchanged, non-test code, outside changed or added functions and added lines, one per line, with resolution `static` or `interface` (a call of an interface method the receiver type implements; for a generic receiver type, one that every instantiation implements). Method calls on values of unresolved type MUST NOT become callers, signals or tests. Each changed function lists at most 10 callers and records `callers_total`, and at most 20 reaching tests (`TestX` functions within 3 references) with `tests_total`. The caller and reaching-test searches MUST be bounded by visit budgets and the analysis time limit, and MUST check the time limit and cancellation before every interface-implementation check. A check whose estimated type-checker cost exceeds a fixed bound MUST NOT be made. A changed function whose search met a bound or left a check unmade MUST keep what was found and carry a reason, and the section is then `limited`. The type check of one package that produces no type error cannot be interrupted, so documentation MUST NOT present the time limit as a bound on the whole analysis.
- Signals: at most 10 low `impacted_caller` signals per changed function and 100 per run, at unchanged coordinates; at most one medium `analysis_limited` signal (symbol `impact_index`) that states how many caller sites were left out, or why a limited or unavailable index concerns changed functions. Neither MAY request review or change the exit code.
- `find_references`, `inspect_symbol` and `find_callers` (depth 1 to 3, default 1) MUST answer from the index for indexed Go functions and methods, with `method` and `limitations`, bounded results, work and time, snippets taken from the whole-file redaction, and MUST fall back to the lexical search with a note otherwise. They MUST NOT create evidence.
- An impacted test's status MUST be derived by `Finalize` from verified evidence alone: `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` only when its `evidence_id` resolves to exactly one `impacted_test_differential` record for the same test name and path whose stored status a verifier re-derived; `UNVERIFIED` for any other cited record; no status without evidence. Each `FAILS_ON_CANDIDATE` test gets one high review target at its declaration. A `FAILS_ON_CANDIDATE` or `UNVERIFIED` test, or `tests_status: not_run`, requests review.
- Output and documentation MUST follow §8 for `impacted_caller`, tool output and `indexed`.

Acceptance:

- On the shop fixture (`price.Total` called by `api.Checkout`; `cart.Cart.Total` reachable through `cart.Totaler` from `notify.Message`; both bodies changed), `lint` exits 0 without checks, reports `indexed` with both functions `body_changed`, a `static` caller at `api/handler.go:7` and an `interface` caller at `notify/notify.go:7` as low `impacted_caller` signals, and `TestTotal` (depth 1) and `TestCheckout` (depth 2) as reaching tests. `--impact=false` removes the section and those signals and leaves the review surface unchanged. Two runs give identical JSON apart from `generated_at`.
- 150 unchanged callers of 15 changed functions give 100 `impacted_caller` signals and one `analysis_limited` signal; 12 callers of one function give 10 signals, 10 listed callers and `callers_total` 12.
- Tiny limits give `limited` or `unavailable` with one `analysis_limited` signal and never a panic; a tree without `go.mod` is `unavailable` and still lists its changed functions; sensitive files and symlinks are not indexed, and snippets are redacted.
- A scripted reviewer's `find_callers`, `inspect_symbol` and `find_references` calls are answered by the index (`method: go_static_index`) or lexically with a note, are audited, and create no evidence; `depth` 4 is refused.
- A stored impacted-test status that no verifier re-derived is `UNVERIFIED` after re-rendering, requests review under `--ci` and never produces exit 1.
- `//line` directives naming another path, a `_test.go` file or another line move no caller, declaration or changed function, and hide no unchanged caller. A search over many methods implementing one heavily called interface follows that interface's references once per search, and a search that reaches its visit or time bound makes the section `limited` with a reason instead of running unbounded.
- Types that each embed a struct of 3,000 types, whose methods the reaching-test search meets with 200 interfaces of the same method name, make the section `limited` with a reason well within the time limit, and tool queries on them answer promptly with `truncated`. With checks each estimated just within the bound, the searches stop at the analysis time limit and at cancellation, and a tool query stops at its own time limit, each before the next check. A changed method of a generic type gets the interface callers that every instantiation may dispatch to. An interface that only some instantiation may implement makes the section `limited`.

### F6b. Impacted tests

Rules:

- `--impacted-tests` MUST be accepted by `review` only, and MUST exit 3 on `lint`, with `--checks=false` and with `--impact=false`, before any container starts. It adds no policy key and no signal kind; `impacted_test_regression` MUST NOT exist.
- Selection MUST read only the impact section: the reaching tests listed under indexed changed functions whose `file_changed` is false, one per file and name at its smallest depth, ordered by depth, then path and name. At most 16 tests from at most 4 packages (4 test files with a `{file}` template) MAY run per review; every test left out MUST get a reason, and `tests_reason` MUST count what the limits, modified test files, the per-function listing and changed functions that are not in the index left out. When no changed function is in the index, `tests_status` MUST be `not_run`.
- A test MUST run only when its name is a Go test name, its file is a regular `*_test.go` file that is byte-identical in the baseline and candidate snapshots and declares a top-level function of that name, and the `generated_test` template is verifiable; the name MUST reach `-run` quoted. Otherwise it MUST NOT run, and it gets a reason (the stage is `not_run` for an unverifiable template).
- The selected tests of one package directory (one file with `{file}`) MUST run with one identical command on the baseline snapshot (check kind `impacted_test_base`) and on the candidate snapshot (`impacted_test_candidate`), with the unchanged sandbox profile, the 180 s sub-cap and, when a reviewer will run, the reviewer-reserve ceiling. A baseline run that fails as a whole MAY re-run the tests it records as passed on their own; a test that passed inside a candidate run that failed as a whole MAY get one run pair of its own; neither MAY repeat a run that would only repeat the failed one.
- Each test (file and name) with a run pair (a baseline run and a candidate run that both started) MUST get exactly one `impacted_test_differential` evidence record with one test name, runner `go_test_json`, `check_id` (candidate run) and `base_check_id` (baseline run), classified by `ClassifyExistingTest`. A test whose candidate run did not start MUST NOT get an evidence record. Raw check statuses MUST NOT be overwritten. A candidate run's log text MUST NOT make its check ERROR.
- A replayed baseline MUST NOT support `FAILS_ON_CANDIDATE`: the baseline runs again live with the same kind and command. `PASSES_ON_CANDIDATE` MAY rest on a replay of two agreeing live runs.
- `verifyImpactedTests` MUST re-derive `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` only from an `impacted_test_base` and an `impacted_test_candidate` check, each recorded once, the candidate one not replayed, with identical commands targeting the test's package or file, a verifiable test name and `*_test.go` path, and the live-baseline rule; everything else is not verified. The record MUST NOT support any hypothesis status.
- `tests_status` MUST be `ran` when a run started, `no_candidates` when nothing could be selected (including a `not_applicable` index), and `not_run` with a reason otherwise (including an `unavailable` index). A stage that did not run (including one never reached), and a selected test without a result (including one left out by the limits), MUST add an Unverified entry.
- Output and documentation MUST follow §8 for `FAILS_ON_CANDIDATE` (F6) and `PASSES_ON_CANDIDATE`.

Acceptance:

- On the shop fixture with an unchanged `price/price_test.go` holding `TestTotal` and `TestTotalZero`, `review --impacted-tests --reviewer=false --ci` exits 2 (never 1) with no reproduced issue: `impacted_test_base` and `impacted_test_candidate` checks; `TestTotal` and `TestCheckout` `FAILS_ON_CANDIDATE` with high targets at their declarations; `TestTotalZero`, its passing sibling in the same package, `PASSES_ON_CANDIDATE` from a run pair of its own; no ERROR check and no new signal kind. The re-render is identical, the checkout clean, and no `swiftproof-` container remains.
- The same fixture with a test that fails on the baseline too gives that test no evidence and a reason, narrows the baseline run to the other tests, and exits 2.
- A forged pass after a real failure, a truncated or timed-out candidate run, a skipped test and a candidate compile failure (FAIL, not ERROR) give `UNVERIFIED`; a replayed baseline is confirmed live before `FAILS_ON_CANDIDATE`; a stored status that the checks do not support is `UNVERIFIED` after re-rendering.
- 30 reaching tests in 6 packages run 16 tests from 4 packages, and the others carry the limit reason and request review; modified, missing, excluded and unparsable test files and undeclared names run nothing. A candidate run that did not start gives its tests a reason and no evidence record.
<!-- F6:end -->

## F7. Execution cache and parallel checks

The baseline execution cache is opt-in with an explicit `--cache-dir` and replays only baseline-side runs (§3.3, §3.4). Incremental re-review on a pull-request update is cache reuse across runs that share a cache directory. `--parallel` (1..4) runs the initial checks concurrently without changing their order in the report, their semantics or the budget. The `execution` object records cache, parallelism, budget and `replay_backed`.

<!-- F7:begin -->
### F7a. Baseline execution cache

**Rules.**

- SwiftProof MUST use an execution cache only when `--cache-dir DIR` is passed to `review`. There is no default directory and no `--no-cache` flag.
- Before dependency preparation and before any container starts, SwiftProof MUST exit 3 when DIR, after the links of its nearest existing ancestor are resolved (symlinks, and on Windows junctions and other reparse points that name another directory; the repository root and the report directory are resolved the same way), is the repository root or the report directory, is inside either, or contains either; when DIR is a symlink, a junction or another reparse point, or not a directory; when a reparse point on its path cannot be resolved; and, on Unix, when DIR is not owned by the effective user or grants any group or other permission. Existing directories MUST also be compared by file identity, so that another spelling of the same directory does not pass. A missing DIR MUST be created with mode 0700. Windows ownership and ACLs are not checked, and the documentation MUST say so.
- A run MUST be eligible only when a cache is enabled, the sandbox image was resolved to a local image ID, the check kind ends in `_base`, the run uses the baseline snapshot, sandbox networking is off, and every pristine file and directory of the baseline snapshot is unchanged. Entries a stage added under the snapshot MUST be hashed into the key. Every other run MUST NOT read or write the cache.
- The key MUST be the SHA-256 of a canonical preimage (schema `swiftproof-execcache/v1`) that holds hashes and identifiers only: tool version and executable digest, kind, base commit, the pristine-manifest digest and the added entries, the digest of the trusted policy's execution settings, the image ID, the Docker server identity, the digest of the complete docker argument vector built with fixed placeholders, the argv digest, the capture path, the per-run timeout before budget clamping, and the output limit.
- With a cache, SwiftProof MUST resolve the image once (`docker image inspect` and `docker info`, no pull) and MUST execute every run of the review on that image ID. The probe MUST end at the review's deadline or cancellation. A resolution failure, sandbox networking or an unusable store MUST disable the cache with a recorded reason and MUST NOT change execution or the exit code.
- Only a live result with status PASS or FAIL, exit code 0 to 124, no timeout, no executor error and a retained log MAY be stored, with a payload only when it is complete and a redaction fixed point. The stored log MUST be the recorded, redacted log; raw output MUST NOT be stored.
- Every live eligible run MUST be written through: a new entry starts at one live run, an agreeing entry (same status, exit code and truncation) gains one, and a disagreeing entry MUST be removed and counted as contradicted and evicted.
- An entry MUST be served only when the run is not live, the entry passes every integrity check, at least two live runs agreed on it, its status is PASS, it was never contradicted, and its recorded duration is below the current per-run timeout. A FAIL entry MUST NOT be served. Every read MUST check the content hash, recompute the key from the stored preimage and compare it with the file name and the requested key, decode strictly and check bounds and age; a file that fails MUST be removed and counted as rejected.
- A replay MUST keep the current run's check ID, record `cache` provenance with status `hit`, record `duration_ms` 0, charge nothing to the budget, save the log again as the check's artifact, return the payload to its stage for normalization, and add one `stage:execution_cache` audit event with status `HIT`.
- When a replayed `generated_test_base` PASS and a live candidate FAIL would record `REPRODUCED`, the harness MUST run the baseline again live with the same kind, staged file and command, validate it, and cite the live check as `base_check_id`. A live re-run that completes without passing MUST yield `UNVERIFIED` and remove the entry, and the evidence MUST say whether the removal succeeded; a live check whose entry was removed MUST NOT keep `cache` provenance; a re-run that does not complete MUST yield `UNVERIFIED`.
- `Finalize` MUST recompute `execution.replay_backed` on every run from the evidence statuses it re-derives from the recorded checks: the sorted IDs of evidence whose re-derived status is `NOT_REPRODUCED`, `NOT_DIVERGED` or `PASSES_ON_CANDIDATE` and that cites a replayed check. It MUST NOT change a status or the exit code.
- The cache directory MUST NOT be mounted into any container. Reports and documentation MUST NOT present a replay as a fresh execution, a re-verification or a re-test, as more reliable than a live run or as authenticated, and MUST NOT state a speed-up without a measurement.

**Acceptance.**

- Unit tests: the key commits to every preimage field, and a golden key pins the encoding; tampered, misfiled, expired, oversized, future-dated, symlinked and wrong-preimage entries are rejected and removed; nothing is served before two agreeing live runs; a contradiction evicts; the directory rules (inside or containing the repository or report directory, symlinked and junctioned ancestors, relative paths, Unix permissions, Windows case variants and short names) exit 3 before any container, and a cache below a link to an external directory is accepted on every use; a probe failure disables only the cache; the image is pinned; the live confirmation cites the live check; a contradicted or unfinished confirmation yields `UNVERIFIED`, and a removed entry leaves no provenance on the live check; a replay-backed `NOT_REPRODUCED` is listed in `replay_backed`, recomputed and idempotent across a re-render.
- Docker-gated tests: a real image is pinned to its ID and a third identical baseline run is replayed; the CLI end-to-end test runs five reviews against one directory (two live runs, a replay with live confirmation and `REPRODUCED`, a replay-backed `NOT_REPRODUCED` on a benign head, a rejected tampered entry) and leaves the checkout clean.

### F7b. Parallel initial checks

**Rules.**

- `--parallel N` (1 to 4, default 1) MUST apply only to the initial checks (test, typecheck, build). Coverage, the v0.4 stages, reviewer experiments and every other sandbox run MUST stay sequential. With `--checks=false` the flag is accepted and has no effect.
- The concurrency limit MUST be the smallest of N, the number of initial checks, and the number of sandboxes the Docker server can hold at once without oversubscribing its CPUs or its memory, `min(NCPU / sandbox.cpus, MemTotal / sandbox.memory_mb)`, read with one `docker info` call bounded by 15 seconds and by `--deadline`. That call MUST be made only when more than one check could run at a time (N and the number of initial checks are both above 1, and the budget left under `sandbox.max_runtime_seconds` covers two full per-run timeouts) and a container can start (an open harness, an image, a review that is neither past its deadline nor cancelled, a budget not used up); when it fails, the checks MUST run one at a time.
- The checks MUST run in groups of at most the limit. A group MUST be planned in configured order, each run with the check ID, the gates and the budget reservation (§1.3 rule 2) that a run one at a time would get. A group of n > 1 MUST start only while the budget left under `sandbox.max_runtime_seconds` covers n full per-run timeouts; otherwise it MUST shrink, down to a single run, which reserves what remains exactly as a sequential run does. Every concurrent run therefore reserves the policy's full per-run timeout (the context of the review, which carries `--deadline`, still ends it, as it ends any run), and the sum of the timeouts in flight never exceeds the remaining budget.
- With `--deadline`, every run of a group passes the deadline gate when the group starts. When the deadline expires during a group, each run still in progress ends TIMEOUT through the existing context path, where one at a time the later runs would have been SKIPPED as not started. Documentation MUST state this difference in outcome. It changes no exit code: both are incomplete checks.
- Each run MUST be charged its own elapsed time, as a sequential run is. Parallelism MUST NOT change a check's gates, timeout, classification, redaction, output bound, log retention or cache eligibility.
- The checks, their log artifacts and their `run_<kind>` audit events MUST be recorded in configured order once every run of the group has ended. The audit events of one group MUST carry the group's start time, so that ordering the audit by time keeps the configured order. No other run and no reader of the harness state may interleave with a group.
- `execution.parallelism` MUST record the requested value, the effective value (the most initial checks whose sandboxes ran at the same time, at least 1) and a fixed note that gives the reasons the effective value is below the requested one (the number of checks, the Docker capacity as the probe found it, including no room for a single sandbox, or its unreadable probe, the budget rule, checks of a group recorded without starting a sandbox) and, when checks ran at the same time, the rules above. When no initial check started a sandbox (no image, a closed harness, an expired or cancelled review, a used-up budget), the note MUST say so and MUST NOT say that checks ran. A review that does not run its initial checks (`--checks=false`, or no initial command) MUST say that `--parallel` had no effect.
- Reports and documentation MUST NOT present parallel checks as a change in semantics or budget, and MUST NOT state a speed-up without a measurement. They MUST say that concurrent sandboxes share the Docker host, so that a check can take longer than it would alone, reach its timeout and be charged more of the budget.

**Acceptance.**

- Unit tests with a fake executor, also under the race detector: checks that end in reverse order are recorded, saved and audited in configured order, and did run at the same time; concurrency never exceeds the limit; with `--parallel 4` the reserved and charged time never exceeds `max_runtime_seconds`, every concurrent run gets the full per-run timeout, and a group shrinks when the budget cannot cover full timeouts (four checks run as two groups of two); an exhausted budget gives the sequential outcome (the first run takes what remains, the rest are SKIPPED); with `--parallel` 1, 3 and 4, the recorded checks, artifacts and audit events equal those of one `Run` per kind; the Docker capacity caps the limit by CPUs and by memory, a capacity of 0 is reported as no room for a single sandbox, and a failed or unreadable probe means one at a time; there is no Docker call when one check at a time is requested, when there is one check, without an image, after the deadline, after a cancellation, on a closed harness, when the budget was used up before the checks, or when the budget left covers fewer than two full per-run timeouts (the checks then run one at a time and the note gives the budget rule); when no container can start (no image, the deadline, a cancellation, a closed harness, a used-up budget), the note says that no initial check started a sandbox; an overall deadline during a group ends its runs as TIMEOUT and skips later groups; a reader of the ledger never sees part of a group.
- Docker-gated test: on a real image, with room for two sandboxes, the test and typecheck containers overlap in time, the checks keep configured order and their real statuses, each run's time is charged, and no container of the test survives.
- F0's `run_test.go`, which pins the `run.go` semantics, passes unchanged after the plan / execute / record split.
<!-- F7:end -->

## F8. Trusted dependency preparation

Opt-in through the base-branch policy's `prepare` object, review only. Before any candidate code runs, the policy's command runs in one bounded container on inputs exported from the base commit, and the container is committed as a local derived image keyed by its inputs and reused by key. Every check of the run then uses that image. Network requires both the policy's `prepare.network` and `--allow-prepare-network`. Preparation fails closed (exit 4).

<!-- F8:begin -->
### F8.1 Rules

Trust and inputs:

- Only the trusted policy (§1: the tip of the base ref, or `--config`) MAY define `prepare`. A candidate policy MUST NOT add, change or enable it.
- Inputs MUST be exported only from `change.BaseCommit` (the base ref commit or its merge base with head), as exact Git blobs of regular files whose paths match `prepare.inputs`, and recorded as `prepare.source_commit`. They MUST NOT come from the head commit. A matched symlink or submodule, a matched credential-bearing path, no matching file, or an export over 256 files, 32 MiB per file or 128 MiB in total MUST fail the stage.
- The prepare container MUST NOT receive candidate content, the full tree, credentials, the Docker socket, the host environment or a writable host path.

Container and network:

- Preparation MUST run before any candidate code, in one container created from the local `sandbox.image` resolved to its image ID, with `--pull=never`. SwiftProof MUST NOT pull images, and MUST NOT build them from a Dockerfile or from candidate content; the only image it derives is the committed prepare container.
- The container MUST use the §2 R2 profile: `--cap-drop=ALL` plus, only for `user: root`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETGID` and `SETUID`; no-new-privileges; the sandbox memory and CPU limits; `--pids-limit=256`; `--ulimit=nofile=1024:1024`; UID/GID 65534 unless the policy says `user: root`; exactly one read-only bind mount (the exported inputs at `/swiftproof/inputs`) and no other mount, so a created container with another mount, such as a `VOLUME` the sandbox image declares, MUST fail the stage before the command starts; `--workdir=/swiftproof/work`; `HOME=/swiftproof/home` and exactly the policy `env`. Its output MUST be bounded by `sandbox.max_output_bytes`, redacted and retained as a hashed `prepare_output` artifact.
- The container MUST have network only when `prepare.network` is true, `--allow-prepare-network` is passed and `--no-network` is not. That permission MUST NOT change the network of any check. On a miss that needs network without permission, the status MUST be `not_permitted`, and SwiftProof MUST NOT start a container.

Key and reuse:

- The key MUST be the SHA-256 of a canonical JSON of the schema `swiftproof-prepare/v1`, the tool version, the base image ID, the argv, the user, the build's network setting, the sorted `env`, the sorted path and SHA-256 of every exported input, the SHA-256 of the container-argument template and `max_added_mb`.
- An image MAY be reused only when exactly one image carries this key's tag and key label, and its labels equal the full key, the source commit and the base image ID, its layers are the base image's layers plus exactly one, its environment holds the policy `env`, and its committed layer is within `max_added_mb`. Otherwise SwiftProof MUST build. Reuse MUST NOT start a container.
- A build MUST fail when the command exits non-zero, times out, or changes nothing outside the directories SwiftProof creates. When every change is under `/workspace`, `/tmp` or `/swiftproof/home`, the report MUST add the Unverified entry "prepared outputs are shadowed by check mounts". Both decisions MUST rest on the whole `docker diff` listing; a change that was not read in full MUST NOT be taken as shadowed or as one SwiftProof made. After `docker commit`, an image whose committed layer exceeds `max_added_mb` MUST be removed and the stage MUST fail.
- Every sandbox run of the review MUST use the derived image by its ID. The check profile MUST NOT change.

Outcome:

- `failed` and `not_permitted` MUST run no check and no reviewer, MUST NOT fall back to the unprepared image, MUST exit 4 with or without `--ci`, and MUST add an Unverified entry that says whether the base-branch prepare command was started. No candidate code ever runs in this stage.
- A changed file whose new or old path matches an input MUST produce a medium `prepare_input_changed` signal, in lint and review. When checks ran on a built or reused image, the report MUST add an Unverified entry naming the changed inputs. Candidate dependencies MUST NOT be installed.
- Preparation MUST NOT be a check, evidence or support for any hypothesis status, and MUST NOT produce exit 1. Its only exit effects are exit 4 for `failed` and `not_permitted`, and the review requests of its Unverified entries (exit 2 with `--ci`). It MUST NOT be charged to `sandbox.max_runtime_seconds` (R10); `prepare.timeout_seconds` and `--deadline` bound it, and a preparation cut short fails. The reason MUST name the overall deadline only when `--deadline` ended the stage; any other interruption MUST NOT be reported as the deadline.
- Outputs MUST NOT present a prepared image as safe, unmodified, license-clean, free of vulnerabilities or reproducible, its labels as an attestation, candidate dependencies as installed, or preparation network as check network (§8).

### F8.2 Acceptance

- The container argv matches the golden profile for `sandbox` and `root`, with and without network, and never gains a second mount, a volume, `--privileged`, `--rm`, `--read-only` or the Docker socket.
- A real offline build commits an image with the base layers plus one and the policy env; a check in it finds the prepared file and fails on the base image; the build container had only a loopback interface, ran as 65534 in `/swiftproof/work` and saw no host environment; a root build has exactly the six capabilities; a second run reuses the image without starting a container.
- Inputs come from the base commit only: a head-only file and the head version of a changed input are never exported.
- An image with a matching key label but a different source-commit label, base image label, schema, layer structure, environment or size is rebuilt, not reused.
- Without `--allow-prepare-network`, or with `--no-network`, a build that needs network is `not_permitted`, exits 4 and starts no container; an existing image for the key is still reused.
- A failing, timed-out, deadline-cut or no-change command, a missing base image, a base image that declares a `VOLUME`, a credential-bearing or unmatched input, a commit or verification failure and an image over `max_added_mb` each give `failed`, exit 4 and no check; the container is always removed with its anonymous volumes, an image whose `docker commit` returned before the failure is removed, and the log is retained redacted. When the overall `--deadline` ends the wait for `docker commit`, Docker may still finish that commit; that tagged image may remain and is reused only after the reuse checks.
- A command that leaves thousands of files under its `HOME` and also writes a directory that checks see is `built` with persistent outputs, not shadowed, wherever `docker diff` lists that directory.
- `lint` makes no Docker call and still records `prepare_input_changed`.
<!-- F8:end -->

## F9. Evidence-only exports

`--format sarif` and `--format pr-comment` write findings of the §4.3 classes only, read from finalized fields; `--report-url` adds a validated link to the PR comment. SwiftProof publishes nothing itself.

<!-- F9:begin -->
**Formats and options.**

- `--format` MUST accept `sarif` (`confidence-report.sarif`) and `pr-comment` (`PR_COMMENT.md`) in `lint`, `review` and `report`; the default stays `markdown,json`. Every requested format MUST be rendered in memory before any file is written, and a render error MUST write nothing.
- `--report-url` MUST be refused with exit 3, before any container starts, unless `pr-comment` is requested and the URL is `https`, names a host, has no user information, is at most 512 bytes and uses only `[A-Za-z0-9._~:/?#!$&*+,;=%-]`. Since the report's sanitizing never sees it, it MUST also be refused when it, or its percent-decoded form, contains a private-key header or a token shape of the credential redaction that starts at a token boundary (the start of the URL or a character other than `[A-Za-z0-9_-]`), or a query, fragment or path parameter whose name reads as a credential. A token prefix inside a word (`flask-sqlalchemy`, `task-scheduler`) MUST NOT cause a refusal. The pr-comment renderer MUST re-validate it. SwiftProof MUST NOT fetch it.
- Rendering MUST NOT change the exit code, any status, or the Markdown and JSON reports, and MUST NOT execute anything or make a network request. SwiftProof MUST NOT post a comment or upload SARIF.

**Findings.**

- Only the seven §4.3 classes MAY become findings, each read from its Finalize-derived source (`reproduced_issues`; `base_tests.tests[]` and `impact.changed_functions[].tests[]` with `FAILS_ON_CANDIDATE`; `divergences[]` by kind; `intent_test_failures`; `mutation.mutants[]` with `SURVIVED`).
- A finding MUST be kept only when every cited evidence record resolves exactly once in `evidence`, has the class's kind, and re-derives from the recorded checks, at render time, to the class's status (a surviving mutant re-derives through the mutation verifier), and when it cites at least one check and every cited check resolves exactly once in `checks` together with `mutation.checks`.
- Signals, review targets, coverage, impact, prepare and cache records, killed and other non-surviving mutants, `NOT_*`, `PASSES_ON_CANDIDATE` and `INTENT_TEST_PASSED` records, `UNVERIFIED`, `NOT_REPRODUCED` and `DISMISSED` hypotheses MUST NOT be findings. `intent_judgment` MUST NOT be read or rendered.
- Findings MUST be ordered by class, then severity, path, line and identity, and capped at 1000 with the remainder counted. Titles and values MUST be cut at 256 bytes and reasons at 512.

**Locations.**

- A location MUST be emitted only for a changed, non-deleted, non-binary file of the recorded change. A model-chosen location MUST be labelled "model-chosen location", and its line MUST be kept only when the recorded diff has it on the candidate side; otherwise it MUST be file-level. Fuzz changed functions, candidate test ranges and mutated lines keep their lines.
- A finding without a location MUST NOT be a SARIF result; it MUST be listed in the PR comment and reported as a SARIF notification. Impacted tests are always unanchored.
- Reproduced and observed-divergence findings MUST carry the retained `generated_test` artifact and its sha256 when exactly one retained artifact matches the test file name (SARIF `relatedLocations` and properties).

**SARIF.**

- The log MUST be SARIF 2.1.0 with one run, `message.text` only (never `message.markdown`), exactly one location per result, `partialFingerprints` `swiftproof/v1`, and the attribution in the log's `properties.attribution`. Rule IDs and levels MUST follow §4.3; `error` MUST occur only for a reproduced high or critical hypothesis.
- `invocations[0].executionSuccessful` MUST be `false` when the exit code is 4, when a check in `checks` is `ERROR`, `SKIPPED` or `TIMEOUT`, or when `execution.budget.deadline_reached` is true. `toolExecutionNotifications` MUST list, at level `warning`, each unverified area, each stage that did not run, each check in `checks` that did not pass, each unanchored finding, omitted findings, and the absence of any execution. The operational-failure, no-execution, stage, omitted-findings and unanchored-finding notifications MUST come before the per-check, per-area and per-hypothesis ones, so that the notification cap drops the latter first. A hypothesis that stayed `UNVERIFIED` MUST be named by its ID only, with a fixed text that holds for every `UNVERIFIED` hypothesis (including one the model submitted as `UNVERIFIED`): the reviewer model's text of such a hypothesis MUST NOT appear in either export. An unverified area MUST be listed by its position with a fixed text: its note can quote model output and MUST NOT appear in either export.
- No rule, result or property MAY carry a precision, a security severity, a confidence, a percentage, or a result `kind` of `pass`, `informational` or `notApplicable`. Untrusted message fragments MUST have control and bidirectional characters removed and brackets escaped.

**PR comment.**

- The body MUST sit between `model.PRCommentBegin` and `model.PRCommentEnd` and MUST start with a status block that is not a finding: the exit code; the counts of unverified areas, of checks in `checks` that did not pass (the mutation ledger excluded) and of stages that did not run (sections `not_run`, `tests_status` `not_run`, prepare `failed` or `not_permitted`); and the sentence "No finding is not approval."
- Every repository, model or report string MUST go through `commentText`: control and bidirectional characters replaced; a zero-width space after `@`, inside `://` and `www.`, after a `:` followed by `[A-Za-z0-9_+-]`, after a `#` followed by a digit, and after the `-` of `GH-` followed by a digit; then `&`, `<` and `>` HTML-escaped and the Markdown punctuation backslash-escaped as `inline()` does. Quotes MUST NOT become numeric entities, and the output MUST NOT contain a `#` directly followed by a digit. The only link MUST be the validated `--report-url`, besides the constant attribution URL, and the only raw HTML the two markers. Bare commit SHAs and custom autolink references that a repository configures are not neutralized.
- The file MUST be at most 60 000 bytes; findings that do not fit MUST be replaced by a fixed pointer to `confidence-report.json` with their count.
- The `intent_test_failed` text MUST be "A model-written test for <criterion ID> failed on the candidate; there is no baseline control, and the test or its reading of the criterion may be wrong.", followed by the `intent_test` artifact sha256.
- No export MAY present a finding as an approval, "LGTM", "no issues", "safe to merge", a confirmed defect or the revision that is right.

**Acceptance.**

- A reproduced high hypothesis through Docker gives exit 1, one SARIF result `swiftproof/reproduced` at level `error` on the hypothesis's changed line, whose evidence IDs equal the JSON's, and a PR comment listing it; `swiftproof report` re-renders both files byte for byte.
- A hypothesis citing a nonexistent evidence ID gives no finding in either file, `"results": []`, and a status block counting the unverified hypothesis.
- For every class, a valid record gives one finding; removing the checks and evidence, or forcing the statuses without them and finalizing, gives none; each tampered record (unresolved or duplicated IDs, another kind or status, a record not re-derived, an ID in both check ledgers) gives none.
- `lint` exports have no result and say that no checks or experiments ran; an unknown format and every invalid `--report-url` combination exit 3 without writing a report.
- Hostile titles, paths, IDs, versions and reasons (including quotes and `GH-12`) produce no heading, mention, link, HTML, emoji shortcode or `#` followed by a digit, and a 500-finding comment stays within 60 000 bytes with the count of findings not shown.
- An `UNVERIFIED` hypothesis whose title reads as an approval appears in neither export except by its ID, and an unverified-area note naming a rejected tool `LGTM_approved_safe_to_merge_no_issues` appears in neither export; a `--report-url` carrying a token exits 3, and the run URLs of `pallets-eco/flask-sqlalchemy` and `acme/task-scheduler` are accepted.
- The SARIF files of the validation runs validate against the SARIF 2.1.0 JSON schema.
<!-- F9:end -->

## 11. Implementation traceability and validation

| Contract | Implementation and existing checks |
| --- | --- |
| Comparisons, exact blobs and source bounds | [gitrepo](../app/internal/gitrepo/) |
| Policy, flags, provider activation and stage order | [config](../app/internal/config/), [cli](../app/internal/cli/) |
| Static signals and changed-line measurement | [linter](../app/internal/linter/), [coverage](../app/internal/coverage/) |
| Controlled execution, named-test adapters and audit | [harness](../app/internal/harness/), [reviewer](../app/internal/reviewer/) |
| Evidence validation and all renderers | [report](../app/internal/report/), [model/schema tests](../app/internal/model/) |
| F1–F6 | [observe](../app/internal/observe/), [fuzz](../app/internal/fuzz/), [suites](../app/internal/suites/), [mutation](../app/internal/mutation/), [acceptance](../app/internal/acceptance/), [symbols](../app/internal/symbols/) |
| F7–F8 | [execcache](../app/internal/execcache/), [prepare](../app/internal/prepare/), harness and CLI integration tests |

Run `go test ./...` and `go vet ./...` from `app/`. Real Docker tests need
`SWIFTPROOF_TEST_DOCKER_IMAGE` and a preloaded trusted image. Their absence is
an incomplete integration check, not evidence that isolation or experiments
worked. [VALIDATION.md](../app/docs/VALIDATION.md) records prior runs and
[PERFORMANCE.md](../app/docs/PERFORMANCE.md) bounds measured performance claims.
The requirements here are not a claim that every environment or release has
passed them.
