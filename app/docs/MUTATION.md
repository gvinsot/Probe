# Mutation of added lines (`mutation` policy object)

Coverage tells you whether an added line was executed by the package's tests. It does not tell you whether any test would notice if that line did something else. With a `mutation` object in the trusted policy, `swiftproof review` makes small, deterministic changes (mutants) to the **added lines of changed non-test Go files**, runs the package's tests once per mutant inside the sandbox, and records which single changes made a named test fail and which did not. No model is involved.

A mutant with which no test that the command ran for its package failed (`SURVIVED`) becomes a medium `surviving_mutant` review signal. That is an observation about the recorded runs, not a defect: the mutant may be semantically equivalent to the original code, and only the tests of the mutated file's own package ran. A mutant with which a named test failed (`KILLED`) is counted and never listed: it is no reassurance about the tests. Mutation creates no evidence record and no hypothesis, never produces exit 1, never removes, lowers or dismisses anything else in the report, and computes no mutation score and no percentage.

The stage runs during `review` only, mutates Go only, and has no flag of its own.

## Enabling it

Add the object to the policy at the tip of the base branch (or pass a policy with `--config`). All four fields are required and non-zero, so the reviewed policy states its own budget:

```json
"mutation": {
  "command": ["go", "test", "-json", "-count=1", "-failfast", "{package}"],
  "max_mutants": 20,
  "timeout_seconds": 60,
  "max_runtime_seconds": 300
}
```

| Field | Rule |
|---|---|
| `command` | 3 to 128 arguments, each at most 16 KiB and without NUL. `argv[0]` must be `go` (by base name) and `argv[1]` must be `test`. Exactly one argument must be the standalone placeholder `{package}`, and exactly one must be `-json`. Every other argument after `test` must be a flag written as `-flag` or `-flag=value`. No other placeholder (`{file}`, `{coverage_out}`, `{results_out}`) may appear anywhere, including inside `argv[0]`. |
| banned flags | `--`, `-args`, `-C`, `-exec`, `-overlay`, `-run`, `-skip`, `-list`, `-c`, `-o`, `-fuzz`, a second `-json`, and any `-test.*` flag, in their one- or two-dash spellings and with or without `=value`. They would change the directory, the executed binary or the selection of tests, or stop the `-json` stream from being the full record of the package's tests. |
| `max_mutants` | 1 to 200. |
| `timeout_seconds` | 1 to `sandbox.timeout_seconds`: the upper bound of one control or mutant run. |
| `max_runtime_seconds` | `timeout_seconds` to `sandbox.max_runtime_seconds`: a sub-cap of the stage **inside** the shared sandbox budget, never an addition to it. |

An invalid object exits 3 before any container starts. `{package}` expands exactly as for `generated_test`: `./` plus the repository-relative directory of the mutated file, or `.` at the root. Nothing is appended, so the executed argument list is the reviewed one plus that expansion, and the report records it. Keep `-count=1`: a test that runs more than once has no single recorded outcome, and every mutant of its package then stays `NOT_RUN`. `-vet=off` avoids mutants that fail only `go vet`.

**Release ordering.** Every binary before v0.4.0 exits 3 on a policy that contains `mutation` (`json: unknown field "mutation"`). Publish v0.4.0, re-pin every workflow that reviews the base branch (URL and sha256), and only then commit the object to a base-branch policy; see [CI integration](CI.md#release-ordering-for-v04). `swiftproof init` never writes it. A candidate branch cannot enable, disable or change it: the policy comes from the base branch or `--config`.

The stage runs only in `review`, only when initial checks are enabled (the default) and the change has files. It runs after the initial checks, coverage, changed baseline tests, impacted tests and differential fuzzing, and before the reviewer. Reviewer experiments never start it and cannot cite it.

## What is mutated

A changed file is considered when it is not deleted, not binary, ends in `.go` and not in `_test.go`. It is skipped, with a fixed reason listed in the report, when:

- it has no added line;
- a path component starts with `_` or `.`, is `testdata` or `vendor`, or the path contains `...` (the go command ignores or treats these specially);
- its file name ends in a GOOS or GOARCH element (`x_windows.go`, `x_linux_arm64.go`; as in `go/build`, the name is read up to its first dot, so `x_windows.impl.go` counts too), or it has a `//go:build` or `// +build` constraint: a file a build may exclude would let every mutant survive, so such files are skipped whatever the sandbox platform;
- it is marked as generated (`// Code generated ... DO NOT EDIT.`), imports `C`, or does not parse;
- it cannot be read from the sanitized candidate snapshot (an excluded credential-bearing path, not a regular text file, or larger than 1 MiB);
- its directory contains no `*_test.go` file.

Only function bodies are mutated, and a mutant is generated only when **every line of the replaced span is an added line** of the change. Deleted lines, context lines and the baseline are never mutated. The operators, in the order in which they rank on one line:

| Operator | Change | Example |
|---|---|---|
| `drop_error` | The last result of a `return` in a function whose last result is `error` becomes `nil` (not for `return nil`, bare returns or `return f()`; a function literal's own results decide inside it). A result that spans several lines, such as a gofmt'd multi-line `fmt.Errorf(...)`, becomes `nil` followed by as many line breaks, so no other line moves. | `return 0, ErrNegative` → `return 0, nil` |
| `negate_condition` | An `if` condition `c` becomes `!(c)`. | `if total < 0` → `if !(total < 0)` |
| `boundary` | `<` ↔ `<=`, `>` ↔ `>=`. | `total >= 100` → `total > 100` |
| `negate_comparison` | `==` ↔ `!=`. | `a == b` → `a != b` |
| `swap_logical` | `&&` ↔ `\|\|`. | `a && b` → `a \|\| b` |
| `increment_constant` | An integer literal operand of a comparison `n` becomes `(n+1)`. | `total < 0` → `total < (0+1)` |
| `flip_boolean` | `true` ↔ `false` as a direct `return` result. | `return true` → `return false` |
| `swap_arithmetic` | `+` ↔ `-`, `*` ↔ `/` (`+` with a string literal operand is left alone). | `total - 10` → `total + 10` |

`drop_error` is not generated when the dropped expression holds every use of an imported package in the file (for example the only `errors.New` of a file that imports `errors`): the import would become unused and the mutant could not compile. The analysis is syntactic and host-side (`go/parser` over at most 1 MiB per file); repository code is never executed or type-checked with importers to find mutants. Enumeration stops at 2,000 candidate mutants per file and 20,000 per review, and the report says so. A mutant whose replacement would merge with a neighbouring character into another token is not generated either (`-1+-4` would become the decrement in `-1--4`, `a**p` would open a comment in `a/*p`).

Every mutant is applied as one byte splice that must find its original text at the recorded position, keep the number of lines, not merge with a neighbouring character, and still parse; otherwise it is `INCONCLUSIVE` without a run. Every mutant the enumeration generates is meant to meet these conditions: on 2026-09-26, with every line treated as added, Apply accepted all 241,504 candidate mutants generated for the 2,977 non-test files of the go1.26.8 standard library that were not skipped (before the neighbouring-character rule, two of 241,506 were refused).

## Selection and order

When a coverage run passed and was measured, candidate mutants on added lines that it reported as not executed are skipped (`coverage_skipped`): those lines are already reported as `uncovered_change` signals, and a mutant there would only spend budget. Without a measured coverage run nothing is skipped.

At most `max_mutants` of the remaining candidates (`generated`) are selected, breadth-first and deterministically: the first operator of the first mutated line of every file (files in path order), then of the second line of every file, and so on, then the second operator of each line. The rest are counted in `dropped`. The selected mutants are then grouped by package and numbered `mutant-1` … `mutant-N` in execution order (package, path, line, column, operator). The same change and policy always give the same IDs, operators and patches.

## The runs

For each package with selected mutants:

1. **Control run** (check kind `mutation_control`): the command, unchanged, on an unmutated private copy of the candidate. It must pass (status PASS, exit code 0, a log that `sandbox.max_output_bytes` did not cut, no build, vet or timeout marker) with at least one top-level test that passed, none that failed, and every recorded test in one package. Otherwise every mutant of that package stays `NOT_RUN` with the reason, and the next package is tried.
2. **Mutant runs** (check kind `mutant`), one per mutant: in the same private copy, the one mutated file is overwritten, the identical command runs, and the original bytes are written back and read again.

The private copy is made from the sanitized candidate snapshot under the harness's temporary directory; the candidate and baseline snapshots are never written. A write goes to an existing regular file only (never a new path, never through a symlink), after checking that the file still holds the planned original; a failed check, write or restore aborts the workspace, stops the stage, and nothing runs on a tree the report would not describe. Every run uses the unchanged sandbox profile: same image, no network unless the policy and `--allow-network` both allow it, read-only source mount and root filesystem, dropped capabilities, no new privileges, non-root user, memory, CPU, PID, time and output limits, and a fresh container.

Control and mutant runs are recorded in a **separate check ledger**, `mutation.checks` (`mutation-check-N`), with their logs retained as hashed `mutation_check_output` artifacts. They are not in `checks`, so the expected failures of mutant runs never count as failing checks. Harness audit events are `stage:run_mutation_control` and `stage:run_mutant`.

## Outcomes

Each outcome is read from the recorded control and mutant checks only: their status, exit code, truncation flag and the `go test -json` events of their retained logs, per top-level test name (exactly one `run` and one terminal event in one package, the rule of the other v0.4 stages that read Go test logs). Each log is read once, so classification time grows with the size of the logs, not with the number of tests in them.

Outcomes come from the recorded log, which code executing in the sandbox can write: a test can print lines with the framing marker that `go test -json` uses since go1.24, or write raw events to the standard output of the `go` process, and `test2json` records them as events of any test name. A plain print that looks like an event stays inside an `output` event. Forging can only change which mutant statuses and test counts are recorded; mutation never creates evidence, and it affects the exit code only through an `incomplete` or `not_run` section.

| Status | Requires | It is | It is not |
|---|---|---|---|
| `SURVIVED` | A valid control; identical command; mutant PASS with exit code 0 and an uncut log; at least one top-level test passed in the control's package and none failed; the redacted patch retained as a `mutant_patch` artifact. | No test that this command ran for this package failed with this one change, and at least one passed (skipped tests are not counted). | A bug, a missing test, dead code, or a line nobody executes. The mutant may be semantically equivalent; tests of other packages did not run. |
| `KILLED` | A valid control; identical command; mutant FAIL with exit code 1 to 124 and an uncut log; at least one named test failed in the control's package (up to five names are recorded). | Some named test failed with the change. | Reassurance of any kind. Any test failure counts, including a flaky one, so it is counted and never listed. |
| `INVALID` | A build or vet failure of the mutant (a `build-fail` event, a `FailedBuild` field, or a package-level `[build failed]` or `[setup failed]` marker). | The mutant did not build or did not pass `go vet`. | A conclusion about the line. |
| `TIMEOUT` | The container reached the mutant's own time limit (see the runtime budget), or the test binary reported its own timeout. | The run did not finish in time. | A kill or a defect. |
| `INCONCLUSIVE` | An infrastructure ERROR, a cut or unreadable log, tests of another package, a pass or failure without a named test result (for example `TestMain` exiting early), a mutant that could not be applied, a patch artifact that could not be kept, a run ended by the overall deadline, a run that a runtime budget left less than its own time limit and that used all of it, or a status `Finalize` could not re-derive. | No outcome. | Anything. |
| `NOT_RUN` | The control was not valid, the sub-cap, the shared budget, the reviewer reserve or the deadline stopped the stage, or the workspace was aborted. | No run. | Anything. |

A truncated log is never read: size `sandbox.max_output_bytes` to hold the package's `go test -json` log, or its mutants stay `INCONCLUSIVE` and its control invalid.

**Section status** (`mutation.status`):

- `no_candidates`: nothing to mutate (no eligible added line, or every candidate was on a line the coverage run did not execute);
- `ran`: every selected mutant reached `KILLED`, `SURVIVED`, `INVALID` or `TIMEOUT`, and `max_mutants` dropped none. It is not exhaustive: other operators, other lines and other packages were not tried;
- `incomplete`: anything else, including candidates dropped by `max_mutants`, and any `INCONCLUSIVE` or `NOT_RUN` mutant;
- `not_run`: the sandbox never ran a control or mutant command, with the reason (for example a missing image, a failed dependency preparation, `--checks=false`, or an exhausted budget).

`incomplete` and `not_run` add one Unverified sentence, "Mutation analysis is incomplete: …" or "Mutation analysis did not run: …", whether the stage recorded the status or never started (`--checks=false`, a failed dependency preparation).

## Re-derivation by `Finalize`

`report.Finalize` re-derives every `KILLED` and `SURVIVED` mutant from the saved report, including when `swiftproof report` re-renders it. A mutant keeps its status only when its ID is `mutant-N` and unique; its `check_id` and `control_check_id` differ, match `mutation-check-N`, each occur exactly once in `mutation.checks`, collide with no ID in `checks`, and have the kinds `mutant` and `mutation_control`; no other mutant cites its `check_id` (a control may serve several mutants, a mutant run only one); its package is the `{package}` expansion of its path, and the recorded mutant command is `mutation.command` with `{package}` expanded to it; and the outcome rules above give the same status from those two checks. `SURVIVED` also needs `tests_run` to equal the number of top-level tests that passed in the mutant's log, no `failed_tests`, a `mutant_patch` artifact whose sha256 is `patch_sha256`, and no other surviving mutant citing that hash. `KILLED` also needs `failed_tests` to be the first (at most five) top-level tests that failed in the mutant's log. Every other `KILLED` or `SURVIVED` becomes `INCONCLUSIVE` ("the recorded mutation checks do not support this status"), the counts are recomputed, and a section that no longer satisfies `ran` becomes `incomplete`. Survivor signals are not rewritten: a stale one can only add a review target.

## Exit codes

| Result | Without `--ci` | With `--ci` |
|---|---|---|
| Section `ran` or `no_candidates` (survivors are medium signals only) | 0 | 0 |
| Section `incomplete` or `not_run` | 0 | 2 |
| Infrastructure ERROR of a control or mutant run (Docker error, exit code 125 or above, lost log), or a workspace that could not be created, checked or restored | 4 | 4 |
| Invalid `mutation` object | 3 | 3 |

Mutation never produces exit 1: only a reproduced high/critical hypothesis does. For candidate-side kinds such as `mutation_control` and `mutant`, log text never makes a check ERROR; a mutant that does not compile is a FAIL check and an `INVALID` mutant, not exit 4.

## Runtime budget

Every run is charged to the shared `sandbox.max_runtime_seconds` budget. The stage additionally stops at `mutation.max_runtime_seconds`, counted from the recorded run durations. A control run may take `min(timeout_seconds, what is left of the sub-cap)`; a mutant run `min(timeout_seconds, 3 × the control duration + 30 s, what is left of the sub-cap)`, since it does the same compile and test work as its control. The stage stops launching when what is left is below the last control duration. When the sub-cap, the shared budget or the reviewer reserve leaves a mutant run less than its own time limit and the run uses all of it, the budget rather than the mutant ended the run: the mutant is `INCONCLUSIVE`, not `TIMEOUT`, and the stage stops. When a reviewer will run, the stage also stops at `sandbox.max_runtime_seconds` minus the reviewer reserve (half the budget), so reviewer experiments keep their share. Runs execute one at a time.

Every run is a fresh container with an empty build cache, so each mutant costs about one package compile plus its tests; see [performance](PERFORMANCE.md) for measured numbers. Size `sandbox.max_runtime_seconds` for the initial checks, coverage, the other stages, `mutation.max_runtime_seconds` and the reviewer together.

## Report output

- **JSON** (`mutation`): `status`, `reason`, the redacted command template, `limits`, `files` (per considered file: `eligible` or `skipped` with the reason, added and mutated line counts; at most 500 listed), `mutants` (every selected mutant, including killed ones, for re-derivation: path, line, column, end line of a multi-line span, enclosing function, package argument, operator, display cuts of the original and mutated text of at most 256 bytes, status, check IDs, `tests_run`, `failed_tests`, `patch_sha256`, reason), the counters `generated`, `dropped`, `killed`, `survived`, `invalid`, `timed_out`, `inconclusive`, `not_run` and `coverage_skipped`, the mutation ledger `checks`, and a fixed `note`. The object is present exactly when the trusted policy has `mutation` in review mode, whatever `--checks` says; `lint` never writes it.
- **Markdown**: "Mutation of Added Lines", after "Changed-line Execution". It gives the status, the command, the counts, every surviving mutant with its location, change, check IDs, test count and patch hash, up to 20 mutants without an outcome, up to 20 skipped files, and the note. Killed mutants are counted, never listed. No percentage and no score appear.
- **stdout**: one line, for example `Mutation of added lines (ran): 8 mutants selected of 8 generated; 4 killed, 4 survived, 0 did not build or pass go vet, 0 timed out, 0 inconclusive, 0 not run.`
- **Signals**: one medium `surviving_mutant` signal per survivor, on the new side of the mutated line with the enclosing function, which reaches "Suggested Human Review" through the usual targets. The reviewer sees these signals like any other untrusted signal.
- **Artifacts**: `mutation_check_output` (the bounded, redacted log of every control and mutant run) and `mutant_patch` (per survivor: a header naming the mutant and the executed command, then a one-hunk diff of the lines it changed; it records what ran and is not a proposed change).

## Security notes

Only the trusted policy chooses the command and the budgets; `{package}` comes from a diff path that already passed path validation and the skip rules, so it always starts with `./` (or is `.`) and cannot be read as a flag; nothing runs through a shell. Mutants run in a private copy under the unchanged sandbox profile, with a hash check before each write and a read-back after each restore; any mismatch aborts the stage as an operational failure. Mutant generation reads bounded snapshot bytes on the host and never runs repository code. Logs and patches are redacted, and patches are hashed after redaction.

The candidate controls its own tests, and code executing in the sandbox can write the logs the outcomes are read from (see [Outcomes](#outcomes)), so it can force any mutant status. Forcing `KILLED`, `INVALID`, `TIMEOUT` or `INCONCLUSIVE` only removes claims or requests review; forcing `SURVIVED` only adds review targets against the candidate itself. That is why mutation may add signals and sentences and never deletes a signal, lowers a severity, supports a dismissal, creates evidence or influences a hypothesis.

## Limitations

- Go only; TypeScript and JavaScript are not mutated.
- One package per mutant: tests of other packages that exercise the mutated function do not run, so a survivor may be killed by them.
- Equivalent mutants are not detected, and some survivors cannot be killed by any test.
- Files with build constraints or GOOS/GOARCH names are skipped rather than evaluated against the sandbox platform.
- `drop_error` of a variable declared with `:=` whose only use was the dropped return makes an `INVALID` mutant (declared and not used).
- A module in a subdirectory needs a root `go.work`, as for coverage; without it the controls fail and the stage is `incomplete`.
- Each run is a cold container; there is no parallel or warm-cache mutant execution, and mutant results are never cached.

## Example

From a real run (2026-09-26, `golang:1.26-bookworm`) on a fixture whose change adds `Discount(total int) (int, error)` to package `price`, with a test that asserts only `Discount(200) == 190` and an error for `Discount(-5)`, a Windows-only helper, and the policy above with `max_mutants: 10`, `timeout_seconds: 60` and `max_runtime_seconds: 400`. `review --reviewer=false --ci` exited 0 and printed:

```text
Mutation of added lines (ran): 8 mutants selected of 8 generated; 4 killed, 4 survived, 0 did not build or pass go vet, 0 timed out, 0 inconclusive, 0 not run.
```

The Markdown section listed the four survivors, none of the killed mutants, and the skipped file:

```text
Surviving mutants (no test that the command ran for the package failed with the change; skipped tests are not counted):

- **mutant-2** boundary at price/price.go:15 in Discount: replaced &lt; with &lt;=; control check mutation-check-1, mutant check mutation-check-3; 2 named tests passed in package ./price; patch sha256 182beec344cc1715f681a18a1389bc2e2f62b04341ef83b5e6cb1a512c7941f9
- **mutant-3** increment\_constant at price/price.go:15 in Discount: replaced 0 with \(0+1\); control check mutation-check-1, mutant check mutation-check-4; 2 named tests passed in package ./price; patch sha256 345cda6583935d9b0bc5da14af581a64f08b22608365f870441d3f00a8cbea11
- **mutant-6** boundary at price/price.go:18 in Discount: replaced &gt;= with &gt;; control check mutation-check-1, mutant check mutation-check-7; 2 named tests passed in package ./price; patch sha256 463f60190b79acd703b770e52aa99e4cac9ea06902ee55f70c006de72236fbcf
- **mutant-7** increment\_constant at price/price.go:18 in Discount: replaced 100 with \(100+1\); control check mutation-check-1, mutant check mutation-check-8; 2 named tests passed in package ./price; patch sha256 8c07cae514ca7eba08bf72cdf697ed6c6c298a5a6efcc3a41a1129d234eab25e

Changed Go files that were not mutated:

- price/fast\_windows.go: the file name has a GOOS or GOARCH suffix; files a build may exclude are not mutated
```

The survivors record only that no test this command ran for `./price` failed, and both of its named tests passed, with `total < 0` changed to `total <= 0`, with `0` changed to `(0+1)`, with `total >= 100` changed to `total > 100`, and with `100` changed to `(100+1)`, one change at a time. Whether that matters is for a human to decide.
