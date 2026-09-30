# Changed baseline tests on candidate code (`--base-tests`)

A change can alter behavior and, in the same commit, edit, delete or turn off the test that asserted the old behavior. The candidate's own test suite then passes. `probe review --base-tests` runs the **baseline version** of the Go test functions of each changed Go test file that the change modified, removed or affected through the rest of the file (see [What is selected](#what-is-selected)) against the candidate code, inside the sandbox, and records what the two runs showed. No model is involved.

The stage is opt-in, runs during `review` only, executes Go tests only, and adds no policy key. It adds evidence and review requests; it never produces exit 1 and never removes, lowers or dismisses anything else in the report.

## Enabling it

```sh
probe review --base main --base-tests --ci
```

- `--base-tests` exits 3 on `lint` and together with `--checks=false`, before any container starts.
- The trusted policy's `generated_test` command must let Probe establish which Go tests ran: `go test` with exactly one standalone `{package}` or `{file}` target and only flags otherwise, without `-C`, `-exec`, `-overlay`, `-args` or `--`. `["go", "test", "{package}"]` is the recommended template. With `{file}`, only that one file is compiled, without the package's other source or test files, so tests that use package-internal code usually do not build and stay `UNVERIFIED`. An external test file that imports the package can build. With any other template nothing runs and the section is `not_run` with the reason.
- A binary without the flag exits 3 while parsing arguments. Re-pin to v0.4.0 or later before using it; intermediate v0.3 builds may accept the flag without the completed stage (see [CI integration](CI.md#release-ordering-for-v04)).

## What is selected

Selection is static. Probe reads the baseline and candidate versions of each changed Go test file from Git (at most 2 MiB per file) and parses them with `go/parser`. It never runs repository code to select tests.

Considered files are `*_test.go` files that the change modified (`M`), deleted (`D`) or renamed from a `*_test.go` path (`R`). Added and copied files are not considered: their tests have no baseline version. Files that `go test` never compiles are skipped: anything under `testdata` or `vendor`, and any directory or file name starting with `_` or `.`.

A runnable test function is one `go test` would run: `Test` or `Test` followed by a non-lowercase character, not `TestMain`, no receiver, no type parameters, no results, and exactly one `*T` or `*pkg.T` parameter. Each runnable test of the baseline file is selected with one change class:

| Change | Selected when |
|---|---|
| `removed` | The candidate file no longer declares the test as a runnable test: deleted, renamed, moved to another file, or given another signature. |
| `modified` | The test's token digest differs, or the candidate file could not be read or parsed. |
| `shared_code_changed` | The test is unchanged, but its file changed outside it in a way that can change whether or how `go test` runs it (next paragraph). |
| `file_deleted` | The file was deleted, or renamed to a non-test file. |

A file counts as changed outside its tests when any of these differ between the baseline and candidate versions:

- another declaration of the baseline file (a helper, type, variable, constant, `TestMain` or `init`) disappeared or changed;
- the package clause, the `//go:build` or `// +build` lines before it, or the import set (name and path; the order does not count);
- a compiler directive comment anywhere in the file (`//go:...`, such as `//go:embed`, or `//line`);
- the candidate added `init`, `TestMain` (which may never call `m.Run`), a package-level variable with an initializer, or a method of a type that the candidate file did not newly declare;
- a rename moved the file to another directory, which is another package even under the same package clause, or changed the `_GOOS`, `_GOARCH` or `_GOOS_GOARCH` suffix of the file name.

Other declarations only the candidate added (tests, functions, types with their methods, constants, variables without initializer) do not count: an unchanged test cannot use them. An import added for a new test does count, so it selects every test of its file. The token digest ignores comments, indentation and spacing, so such edits select nothing when nothing above changed. Joining or splitting lines can change the digest because Go inserts semicolons at line ends.

Limits: at most 50 changed test files are analyzed and at most 100 tests are selected. Every file that could not be read or parsed and every overflow is reported as an unverified area. When no test is selected and some file could not be analyzed, the section is `not_run`, not `no_candidates`. `no_candidates` means only that no test was selected under these rules; it says nothing about other tests.

## The two runs

Each selected test runs twice with one identical command:

1. **Baseline tree** (check kind `base_test_base`): the baseline snapshot, unchanged.
2. **Hybrid tree** (check kind `base_test_hybrid`): a private copy of the sanitized candidate snapshot in which, for the package directory of each selected test, every `*_test.go` file directly in that directory and its `testdata` directory are replaced by the baseline's. Candidate-only test files of that directory are removed, so a moved test cannot collide with the restored baseline file. A candidate entry that blocks a baseline path is removed too: a directory named like a baseline test file or `testdata`, or a file where the baseline has the package directory or one of its parents. A non-test Go file in that position is candidate code and stays; the tests of that directory then stay `UNVERIFIED`. Everything else is the candidate's: the package's code, other packages, `go.mod`, `go.sum`, test files and `testdata` of other directories.

The command is the `generated_test` template with the package (or file) substituted, followed by `-json -count=1 -run ^(Name1|Name2)$`. Both runs use the unchanged sandbox profile: the same image, no network unless the policy and `--allow-network` both allow it, read-only source mount, limits and non-root user. The hybrid tree is built on the host from the sanitized snapshots and is deleted after the stage; a manifest of every file it removed or restored, with SHA-256 hashes, is kept as the hashed artifact `base_test_hybrid_manifest`.

Tests are grouped into units: one package directory (one file with a `{file}` template), at most 50 names per run and at most 10 units per review. Two refinements keep one failing test from hiding the others:

- When the baseline run fails as a whole, the tests it records as passed run again on their own. The others get no candidate-side run.
- When the hybrid run fails as a whole, each test it records as passed gets one more run pair of its own, because a pass inside a failed run never supports `PASSES_ON_CANDIDATE`. No such pair runs when it would only repeat the failed run: the run held only that test, or every test in it passed (the run failed for another reason, such as the exit code of `TestMain`). Those tests stay `UNVERIFIED` with a reason that says so.

With an execution cache (`--cache-dir`), a baseline run may be replayed. A replay never supports `FAILS_ON_CANDIDATE`: the baseline runs again live with the same kind and command, and the test is classified against the live run. `PASSES_ON_CANDIDATE` may rest on a replay that two agreeing live runs recorded, and the report lists it in `execution.replay_backed`.

## Statuses

Each run pair is classified per test name from the `go test -json` events of both recorded checks. The name must have exactly one `run` event and exactly one terminal event, in one package, on both sides; a second terminal event, such as a pass printed after a real failure, makes the result unknown.

| Status | Requires | It is | It is not |
|---|---|---|---|
| `FAILS_ON_CANDIDATE` | Identical commands; baseline check PASS with exit 0, an untruncated log and the test passing; hybrid check FAIL with exit 1–124, an untruncated log and the test failing. | An outcome difference between one recorded run each: possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge. | A reproduced issue. Proof of a behavior change: a flaky test can produce it. Proof that the test edit is wrong, deliberate or hides anything. Proof that the baseline assertion is the intended behavior. |
| `PASSES_ON_CANDIDATE` | The same baseline conditions; hybrid check PASS with exit 0 and the test passing. | This one test passed in both recorded runs. | Preserved behavior, an equivalent or weaker edited test, or correct code. Tamper-proof: code executing in the sandbox writes the log the result is read from. It says nothing about tests the change did not touch. |
| `UNVERIFIED` | Anything else. | A test for which no result was drawn, with the reason. | A failure or a pass. |

Typical `UNVERIFIED` reasons: the candidate-side package did not build (for example after an API change), the test failed or was skipped on the baseline, a build constraint excluded its file, a run timed out, was skipped for the runtime budget or the overall deadline, or was cut by `sandbox.max_output_bytes`, the package was removed from the candidate, the test passed inside a hybrid run that failed as a whole, the baseline test files of its package directory could not be restored in the hybrid tree, and the stage limits. When no selected test of a unit passed on the baseline, the hybrid run is not started, because none of them could get a result.

`report.Finalize` re-derives every status from the recorded checks, including when `probe report` re-renders a saved report: a test is `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` only when its `base_test_differential` evidence record resolves, names the same test and file, cites a `base_test_base` and a `base_test_hybrid` check with the same command targeting that package, and those checks give that status. A `base_test_differential` record never supports a hypothesis status: a model cannot turn it into a reproduced, not reproduced or dismissed hypothesis.

The section status is `ran` when at least one run started, `no_candidates` when no test was selected, and `not_run` with a reason otherwise.

## Exit codes

| Result | Without `--ci` | With `--ci` |
|---|---|---|
| Any `FAILS_ON_CANDIDATE` or `UNVERIFIED` test, or section `not_run` | 0 | 2 |
| Only `PASSES_ON_CANDIDATE`, or `no_candidates` | 0 | 0 |
| The candidate-side package does not compile (a FAIL check, never ERROR) | 0 | 2 |
| The baseline test files of one package directory could not be restored in the hybrid tree: its tests `UNVERIFIED`, the other units run | 0 | 2 |
| Infrastructure failure of a run (Docker error, exit code 125 or above, lost log): ERROR check | 4 | 4 |
| Host-side failure: the private copy of the candidate snapshot could not be made, or the manifest of the hybrid tree could not be kept | 4 | 4 |
| `--base-tests` on `lint` or with `--checks=false` | 3 | 3 |

The stage never produces exit 1: only a reproduced high/critical hypothesis does. The layout of the candidate tree never produces exit 4: a blocking entry is removed (above), and any other failure to revert one package directory affects only that directory's tests.

## Runtime budget

Every run is charged to the shared `sandbox.max_runtime_seconds` budget. The stage has a 180 s sub-cap inside it: each run's timeout is at most what remains of the 180 s and of `sandbox.timeout_seconds`, and no run starts once the stage has used 180 s. When a reviewer will run, the stage also stops at `max_runtime_seconds` minus the half reserved for reviewer experiments. A typical change costs two runs per affected package directory; the retries above can add one pair per unit, and a cache confirmation one baseline run. The stage runs after the initial checks and coverage, before impacted tests, fuzzing, mutation and the reviewer.

## Report output

- JSON: the `base_tests` object (`status`, `reason`, `tests[]`, `note`), present exactly when `--base-tests` was passed to `review`. Each test has its baseline location (`path`, `line`, `end_line`), its edited location when the candidate still declares it (`candidate_path`, `candidate_line`, `candidate_end_line`), `change`, `status`, `evidence_id` and `reason`.
- Evidence: one `base_test_differential` record per test and recorded run pair, runner `go_test_json`, exactly one test name, `check_id` (hybrid run) and `base_check_id` (baseline run).
- Markdown: the section **Changed Baseline Tests on Candidate Code**, after Reproduced Issues, lists `FAILS_ON_CANDIDATE` first, then `UNVERIFIED`, then at most 20 `PASSES_ON_CANDIDATE` entries, followed by the fixed note. When an intent was supplied it adds: "Intent was supplied; Probe does not decide whether a behavior change matches it." With `no_candidates` it reads: "No test was selected, so no baseline version was re-run. Only the tests declared in modified, deleted or renamed Go test files are considered; this says nothing about any other test."
- Suggested Human Review: a high target on the baseline test function (old side) and, when present, on its edited version (new side), for each `FAILS_ON_CANDIDATE` test only.
- Unverified Areas: a line for a section that did not run, for planning notes, and, when some test has no result, the line "Some baseline versions of changed tests have no FAILS\_ON\_CANDIDATE or PASSES\_ON\_CANDIDATE result; see Changed Baseline Tests on Candidate Code."
- Audit: one `stage:run_base_tests` event per unit, and one `ERROR` event per package directory that could not be reverted, naming the directory and the failure with host paths replaced by placeholders such as `(hybrid tree)`.
- Console: one line, for example `Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED (a failure is an outcome difference for a human to judge, not a reproduced issue; see base_tests).`

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

The hybrid tree is assembled on the host from the already sanitized snapshots: secret-bearing file names stay excluded, symlinks are never created or followed, every path is checked, and files are created exclusively. Candidate entries that block a baseline path are removed and listed in the manifest, so the shape of the candidate tree cannot turn the stage into an operational failure. The runs add no mount, volume, network or payload channel. Baseline test code runs against candidate code in the same sandbox, which is no different from the existing experiments. Candidate code can make a baseline test pass on purpose (for example by detecting the sandbox), so `PASSES_ON_CANDIDATE` is an observation only; a forged pass after a real failure makes the result `UNVERIFIED`. Forging a failure only adds a review request. The flag is set by whoever invokes Probe, never by the candidate branch.

## TypeScript and JavaScript tests

With a verifiable Vitest or Jest `generated_test` template (the file as one standalone `{file}` argument and the JSON report written to `{results_out}`), the stage also selects the tests of changed TypeScript and JavaScript test files (`*.test.*`, `*.spec.*`, `__tests__/`, outside `node_modules`), read with the static index's lexical reader, without running repository code:

- A test is one `test()` or `it()` call at any depth, named by its `describe` titles and title joined by ` > `, for example `price > edge > zero percent`. Its digest covers the tokens of the call, so comment and layout edits select nothing.
- `modified`: the call's tokens changed, or the candidate file declares the name more than once. `removed`: the candidate file no longer declares the name (for example a renamed title or `describe`). `shared_code_changed`: a token outside the test calls changed (imports, helpers, `describe` titles, `beforeEach` and other hooks, module-level code), or a rename moved the file to another directory; adding or removing a test selects nothing else. `file_deleted`: as for Go.
- A name the baseline file declares more than once is not re-run; a note in Unverified Areas says so.
- The unit is the test file. Both runs use the template with the file substituted, followed by `-t ^(?:…)$`, which selects the named tests; outcomes are read from the runner's JSON report, as for [impacted tests](IMPACT.md#typescript-and-javascript-tests), and the evidence records runner `jest_json`.
- The hybrid tree reverts only that test file and its snapshot file `__snapshots__/<file>.snap` (Jest's and Vitest's default location) to the baseline; a candidate snapshot without a baseline counterpart is removed. Helpers, fixtures, mocks and configuration stay the candidate's, and a failure may come from any of them. The manifest lists the file under `files`.
- Titles the index cannot read literally (template literals with `${…}`, escapes, titles computed at run time) and tests generated by `test.each` are not matched and stay `UNVERIFIED`.

With a `go test` template, TypeScript and JavaScript tests are listed with the reason "not run: not a Go test function of a Go test file", and with a Vitest or Jest template Go tests get the converse reason.

## Limitations

- Go, and TypeScript and JavaScript with a Vitest or Jest template. Python gets the lexical signals only.
- A test is selected through its own file. A helper changed in another test file of the same package selects the tests of that other file, not of this one. An added test file, or a `TestMain`, `init` or variable initializer added to one file, can change how the tests of other files of the package run; only the tests of the changed file itself are selected, and an added file selects nothing.
- Edits to `testdata` or other fixtures alone select no test: only changed `*_test.go` files select tests. When a `*_test.go` file of the same package directory is also selected, that directory's `testdata` is reverted in the hybrid tree.
- Tests in files the change did not touch are not run (see `--impacted-tests`).
- One run each: a flaky test can produce `FAILS_ON_CANDIDATE`.
- Test files excluded by a build constraint in the sandbox do not run and stay `UNVERIFIED`.
- Only the test's own package directory is reverted: helper packages, `go.mod` and `testdata` elsewhere stay the candidate's, and a failure may come from any of them.
- Benchmarks, examples and fuzz targets are not selected.

## Example

From a real run (Windows binary, `golang:1.26-bookworm`, 2026-09-26; see [validation](VALIDATION.md)). The candidate drops the upper bound of `Clamp`, loosens `TestClampUpper` from `got != 10` to `got < 10`, moves `TestClampInside` to a new file `extra_test.go`, and adds a skipped `TestLater`. The candidate's own `go test ./...` passes. With `review --base-tests --reviewer=false --ci` the run exits 2 and prints:

```text
Changed baseline tests on candidate code: 1 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED (a failure is an outcome difference for a human to judge, not a reproduced issue; see base_tests).
```

The checks are `test` PASS, then `base_test_base` PASS and `base_test_hybrid` FAIL for `go test . -json -count=1 -run ^(TestClampInside|TestClampUpper)$`, then, because `TestClampInside` passed inside that failed hybrid run, `base_test_base` PASS and `base_test_hybrid` PASS for `-run ^(TestClampInside)$`. The Markdown section reads:

```markdown
2 baseline versions of changed Go tests selected: 1 FAILS\_ON\_CANDIDATE, 1 PASSES\_ON\_CANDIDATE, 0 UNVERIFIED.

- **FAILS\_ON\_CANDIDATE** TestClampUpper — clamp\_test.go:11–15 (modified by the change); edited version at clamp\_test.go:12–16. It passed on the baseline tree and failed on the candidate tree with its package's test files reverted to the baseline (baseline check check-2, hybrid-tree check check-3); evidence-1. One recorded run each: possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge.
- **PASSES\_ON\_CANDIDATE** TestClampInside — clamp\_test.go:17–21 (removed by the change). It passed on the baseline tree and on the candidate tree with its package's test files reverted to the baseline (baseline check check-4, hybrid-tree check check-5); evidence-2.
```

The same run records `test_expectation_relaxed` and `test_skip_added` (new side) and `test_assertion_removed` (old side) on `clamp_test.go`, and high review targets on both versions of `TestClampUpper`. When the candidate instead renames `Clamp` to `Limit` and updates its tests, the three selected tests are `UNVERIFIED` ("the candidate-side package did not build or set up, so the test did not run"), the hybrid check is FAIL rather than ERROR, and the run exits 2, not 4.

When the candidate drops the bound and changes no test function, but adds `//go:build never` before the package clause of `clamp_test.go`, its own `go test ./...` passes with no test file and no risk signal fires. With `--base-tests` all three tests of the file are selected as `shared_code_changed` ("its file changed outside the test function"), `TestClampUpper` is `FAILS_ON_CANDIDATE` and the other two are `PASSES_ON_CANDIDATE`, and the run exits 2 with `--ci`; the published v0.2.0 binary exits 0 on the same fixture.

## See also

- [Impact analysis](IMPACT.md#impacted-tests---impacted-tests): unchanged tests that reach changed functions, with the same `FAILS_ON_CANDIDATE` statuses.
- [Execution cache](EXECUTION_CACHE.md): a replayed baseline never supports `FAILS_ON_CANDIDATE`.
- [Exports](EXPORTS.md): the `base_test_fails_on_candidate` class.
- [Security boundaries](SECURITY.md).
