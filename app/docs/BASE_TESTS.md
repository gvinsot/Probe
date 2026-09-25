# Changed baseline tests on candidate code (`--base-tests`)

A change can alter behavior and, in the same commit, edit or delete the test that asserted the old behavior. The candidate's own test suite then passes. `swiftproof review --base-tests` runs the **baseline version** of every Go test function the change modified or removed against the candidate code, inside the sandbox, and records what the two runs showed. No model is involved.

The stage is opt-in, runs during `review` only, executes Go tests only, and adds no policy key. It adds evidence and review requests; it never produces exit 1 and never removes, lowers or dismisses anything else in the report.

## Enabling it

```sh
swiftproof review --base main --base-tests --ci
```

- `--base-tests` exits 3 on `lint` and together with `--checks=false`, before any container starts.
- The trusted policy's `generated_test` command must let SwiftProof establish which Go tests ran: `go test` with exactly one standalone `{package}` or `{file}` target and only flags otherwise, without `-C`, `-exec`, `-overlay`, `-args` or `--`. `["go", "test", "{package}"]` is the recommended template. With `{file}`, only that one file is compiled with the package's test files, so tests that use package-internal code usually do not build and stay `UNVERIFIED`. With any other template nothing runs and the section is `not_run` with the reason.
- The flag makes a binary older than v0.4.0 exit 3 while parsing arguments. Do not pass it through a workflow pinned to an older release; re-pin first (see [CI integration](CI.md)).

## What is selected

Selection is static. SwiftProof reads the baseline and candidate versions of each changed Go test file from Git (at most 2 MiB per file) and parses them with `go/parser`. It never runs repository code to select tests.

Considered files are `*_test.go` files that the change modified (`M`), deleted (`D`) or renamed from a `*_test.go` path (`R`). Added and copied files are not considered: their tests have no baseline version. Files that `go test` never compiles are skipped: anything under `testdata` or `vendor`, and any directory or file name starting with `_` or `.`.

A runnable test function is one `go test` would run: `Test` or `Test` followed by a non-lowercase character, not `TestMain`, no receiver, no type parameters, no results, and exactly one `*T` or `*pkg.T` parameter. Each runnable test of the baseline file is selected with one change class:

| Change | Selected when |
|---|---|
| `removed` | The candidate file no longer declares the test as a runnable test: deleted, renamed, moved to another file, or given another signature. |
| `modified` | The test's token digest differs, or the candidate file could not be read or parsed. |
| `shared_code_changed` | Another declaration of the baseline file (a helper, type, variable, constant, `TestMain` or `init`) disappeared or changed. Declarations only the candidate added do not count. |
| `file_deleted` | The file was deleted, or renamed to a non-test file. |

The token digest ignores comments, indentation and spacing, so such edits select nothing. Joining or splitting lines can change the digest because Go inserts semicolons at line ends.

Limits: at most 50 changed test files are analyzed and at most 100 tests are selected. Every file that could not be read or parsed and every overflow is reported as an unverified area. When no test is selected and some file could not be analyzed, the section is `not_run`, not `no_candidates`.

## The two runs

Each selected test runs twice with one identical command:

1. **Baseline tree** (check kind `base_test_base`): the baseline snapshot, unchanged.
2. **Hybrid tree** (check kind `base_test_hybrid`): a private copy of the sanitized candidate snapshot in which, for the package directory of each selected test, every `*_test.go` file directly in that directory and its `testdata` directory are replaced by the baseline's. Candidate-only test files of that directory are removed, so a moved test cannot collide with the restored baseline file. Everything else is the candidate's: the package's code, other packages, `go.mod`, `go.sum`, test files and `testdata` of other directories.

The command is the `generated_test` template with the package (or file) substituted, followed by `-json -count=1 -run ^(Name1|Name2)$`. Both runs use the unchanged sandbox profile: the same image, no network unless the policy and `--allow-network` both allow it, read-only source mount, limits and non-root user. The hybrid tree is built on the host from the sanitized snapshots and is deleted after the stage; a manifest of every file it removed or restored, with SHA-256 hashes, is kept as the hashed artifact `base_test_hybrid_manifest`.

Tests are grouped into units: one package directory (one file with a `{file}` template), at most 50 names per run and at most 10 units per review. Two refinements keep one failing test from hiding the others:

- When the baseline run fails as a whole, the tests it records as passed run again on their own. The others get no candidate-side run.
- When the hybrid run fails as a whole, each test it records as passed gets one more run pair of its own, because a pass inside a failed run never supports `PASSES_ON_CANDIDATE`.

With an execution cache (`--cache-dir`), a baseline run may be replayed. A replay never supports `FAILS_ON_CANDIDATE`: the baseline runs again live with the same kind and command, and the test is classified against the live run. `PASSES_ON_CANDIDATE` may rest on a replay that two agreeing live runs recorded, and the report lists it in `execution.replay_backed`.

## Statuses

Each run pair is classified per test name from the `go test -json` events of both recorded checks. The name must have exactly one `run` event and exactly one terminal event, in one package, on both sides; a second terminal event, such as a pass printed after a real failure, makes the result unknown.

| Status | Requires | It is | It is not |
|---|---|---|---|
| `FAILS_ON_CANDIDATE` | Identical commands; baseline check PASS with exit 0, an untruncated log and the test passing; hybrid check FAIL with exit 1–124, an untruncated log and the test failing. | A behavior change accompanied by a test edit, observed in one run each, for a human to judge. | A reproduced issue. Proof that the test edit is wrong, deliberate or hides anything. Proof that the baseline assertion is the intended behavior. A flaky test can produce it. |
| `PASSES_ON_CANDIDATE` | The same baseline conditions; hybrid check PASS with exit 0 and the test passing. | This one test passed in both recorded runs. | Preserved behavior, an equivalent or weaker edited test, or correct code. Tamper-proof: code executing in the sandbox writes the log the result is read from. It says nothing about tests the change did not touch. |
| `UNVERIFIED` | Anything else. | A test for which no result was drawn, with the reason. | A failure or a pass. |

Typical `UNVERIFIED` reasons: the candidate-side package did not build (for example after an API change), the test failed or was skipped on the baseline, a build constraint excluded its file, a run timed out, was skipped for the runtime budget or the overall deadline, or was cut by `sandbox.max_output_bytes`, the package was removed from the candidate, and the stage limits.

`report.Finalize` re-derives every status from the recorded checks, including when `swiftproof report` re-renders a saved report: a test is `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` only when its `base_test_differential` evidence record resolves, names the same test and file, cites a `base_test_base` and a `base_test_hybrid` check with the same command targeting that package, and those checks give that status. A `base_test_differential` record never supports a hypothesis status: a model cannot turn it into a reproduced, not reproduced or dismissed hypothesis.

The section status is `ran` when at least one run started, `no_candidates` when no test was selected, and `not_run` with a reason otherwise.

## Exit codes

| Result | Without `--ci` | With `--ci` |
|---|---|---|
| Any `FAILS_ON_CANDIDATE` or `UNVERIFIED` test, or section `not_run` | 0 | 2 |
| Only `PASSES_ON_CANDIDATE`, or `no_candidates` | 0 | 0 |
| The candidate-side package does not compile (a FAIL check, never ERROR) | 0 | 2 |
| Infrastructure failure of a run (Docker error, exit code 125 or above, lost log): ERROR check | 4 | 4 |
| The hybrid tree could not be built or its manifest could not be kept | 4 | 4 |
| `--base-tests` on `lint` or with `--checks=false` | 3 | 3 |

The stage never produces exit 1: only a reproduced high/critical hypothesis does.

## Runtime budget

Every run is charged to the shared `sandbox.max_runtime_seconds` budget. The stage has a 180 s sub-cap inside it: each run's timeout is at most what remains of the 180 s and of `sandbox.timeout_seconds`, and no run starts once the stage has used 180 s. When a reviewer will run, the stage also stops at `max_runtime_seconds` minus the half reserved for reviewer experiments. A typical change costs two runs per affected package directory; the retries above can add one pair per unit, and a cache confirmation one baseline run. The stage runs after the initial checks and coverage, before impacted tests, fuzzing, mutation and the reviewer.

## Report output

- JSON: the `base_tests` object (`status`, `reason`, `tests[]`, `note`), present exactly when `--base-tests` was passed to `review`. Each test has its baseline location (`path`, `line`, `end_line`), its edited location when the candidate still declares it (`candidate_path`, `candidate_line`, `candidate_end_line`), `change`, `status`, `evidence_id` and `reason`.
- Evidence: one `base_test_differential` record per test and recorded run pair, runner `go_test_json`, exactly one test name, `check_id` (hybrid run) and `base_check_id` (baseline run).
- Markdown: the section **Changed Baseline Tests on Candidate Code**, after Reproduced Issues, lists `FAILS_ON_CANDIDATE` first, then `UNVERIFIED`, then at most 20 `PASSES_ON_CANDIDATE` entries, followed by the fixed note. When an intent was supplied it adds: "Intent was supplied; SwiftProof does not decide whether a behavior change matches it."
- Suggested Human Review: a high target on the baseline test function (old side) and, when present, on its edited version (new side), for each `FAILS_ON_CANDIDATE` test only.
- Unverified Areas: a line for a section that did not run, for planning notes, and one line counting the tests without a result.
- Console: one line, for example `Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED (a failure is a behavior change for a human to judge, not a reproduced issue; see base_tests).`

## Lexical test-edit signals

Independently of `--base-tests`, `lint` and `review` add text heuristics on changed test files in Go, JavaScript/TypeScript and Python. They read only the diff, are never evidence, and every one says so: "Text heuristic, not evidence that a test got weaker".

| Signal | Severity | Fires when |
|---|---|---|
| `test_assertion_removed` | medium | The file loses more assertion lines than it gains (`t.Error`/`t.Fatal`, testify, `expect(`, `assert`, `self.assert…`, `pytest.raises`). |
| `test_case_removed` | medium | The file loses more test declarations than it gains (`func Test…`, `it(`/`test(`, `def test…`). |
| `test_skip_added` | medium | A skip marker is added (`t.Skip`, `it.skip`, `xit`, `test.fixme`, `@pytest.mark.skip`, `@unittest.skip`, `self.skipTest`, …), except a line that was only moved or re-indented. At most 10 per file. |
| `test_focus_added` | high | A focus marker is added (`it.only`, `describe.only`, `fit`, `fdescribe`), which may stop the other tests of the file from running. At most 5 per file. |
| `test_expectation_relaxed` | medium | In one hunk, exact expectations are removed and looser ones added: for example `toEqual` to `toBeDefined`, `t.Fatalf` to `t.Logf`, `got != 10` to `got < 10`, `assertEqual` to `assertTrue`. A comparison with `nil` or `None` is not an exact expectation. At most 10 per file. |

A high signal requests human review under `--ci`, as every high signal does.

## Security notes

The hybrid tree is assembled on the host from the already sanitized snapshots: secret-bearing file names stay excluded, symlinks are never created or followed, every path is checked, and files are created exclusively. The runs add no mount, volume, network or payload channel. Baseline test code runs against candidate code in the same sandbox, which is no different from the existing experiments. Candidate code can make a baseline test pass on purpose (for example by detecting the sandbox), so `PASSES_ON_CANDIDATE` is an observation only; a forged pass after a real failure makes the result `UNVERIFIED`. Forging a failure only adds a review request. The flag is set by whoever invokes SwiftProof, never by the candidate branch.

## Limitations

- Go only. JavaScript/TypeScript and Python get the lexical signals only.
- A test is selected through its own file. A helper changed in another test file of the same package selects the tests of that other file, not of this one.
- Tests the change did not modify or remove are not run (see `--impacted-tests`).
- One run each: a flaky test can produce `FAILS_ON_CANDIDATE`.
- Test files excluded by a build constraint in the sandbox do not run and stay `UNVERIFIED`.
- Only the test's own package directory is reverted: helper packages, `go.mod` and `testdata` elsewhere stay the candidate's, and a failure may come from any of them.
- Benchmarks, examples and fuzz targets are not selected.

## Example

From a real run (Windows binary, `golang:1.26-bookworm`, 2026-09-26; see [validation](VALIDATION.md)). The candidate drops the upper bound of `Clamp`, loosens `TestClampUpper` from `got != 10` to `got < 10`, moves `TestClampInside` to a new file `extra_test.go`, and adds a skipped `TestLater`. The candidate's own `go test ./...` passes. With `review --base-tests --reviewer=false --ci` the run exits 2 and prints:

```text
Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED (a failure is a behavior change for a human to judge, not a reproduced issue; see base_tests).
```

The checks are `test` PASS, then `base_test_base` PASS and `base_test_hybrid` FAIL for `go test . -json -count=1 -run ^(TestClampInside|TestClampUpper)$`, then, because `TestClampInside` passed inside that failed hybrid run, `base_test_base` PASS and `base_test_hybrid` PASS for `-run ^(TestClampInside)$`. The Markdown section reads:

```markdown
2 baseline versions of changed Go tests selected: 1 FAILS\_ON\_CANDIDATE, 1 PASSES\_ON\_CANDIDATE, 0 UNVERIFIED.

- **FAILS\_ON\_CANDIDATE** TestClampUpper — clamp\_test.go:11–15 (modified by the change); edited version at clamp\_test.go:12–16. It passed on the baseline tree and failed on the candidate tree with its package's test files reverted to the baseline (baseline check check-2, hybrid-tree check check-3); evidence-1. A human decides whether this behavior change is intended.
- **PASSES\_ON\_CANDIDATE** TestClampInside — clamp\_test.go:17–21 (removed by the change). It passed on the baseline tree and on the candidate tree with its package's test files reverted to the baseline (baseline check check-4, hybrid-tree check check-5); evidence-2.
```

The same run records `test_expectation_relaxed` and `test_skip_added` (new side) and `test_assertion_removed` (old side) on `clamp_test.go`, and high review targets on both versions of `TestClampUpper`. When the candidate instead renames `Clamp` to `Limit` and updates its tests, the three selected tests are `UNVERIFIED` ("the candidate-side package did not build or set up, so the test did not run"), the hybrid check is FAIL rather than ERROR, and the run exits 2, not 4.
