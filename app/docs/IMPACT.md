# Impact analysis

Impact analysis points a reviewer at code the change did not touch but that depends on what it changed: the places in unchanged code that reference a changed Go function or method, and the existing Go tests that reach it within a few calls. It also backs the reviewer's `find_references`, `inspect_symbol` and `find_callers` tools.

It is static and approximate. It never executes repository code, never creates evidence, and never removes a signal, lowers a severity or supports a dismissal. It only adds low-severity review targets and tool observations. A listed caller is a place to review, not a defect, and an absent caller is not proof that none exists.

## What the index is

`lint` and `review` build the index on the host, in process, from committed Git objects:

- Files are listed with `git ls-tree` and read with one `git cat-file --batch` stream (`gitrepo.Tree` and `gitrepo.ReadBlobs`), never from the working tree. Only regular files are read: symlinks and submodules are skipped.
- Only `.go` files and `go.mod` files are read, outside the directories the go command ignores (`testdata`, `vendor`, and names starting with `_` or `.`). Files whose path the sandbox also hides (`.env*`, `*.pem`, `.ssh/`, `credentials`, and the rest of the snapshot exclusions) are neither read nor indexed.
- Every `go.mod` defines a module; a directory belongs to its innermost module. Files outside every module are not indexed.
- Files are selected as the go command would select them for `linux/amd64` with cgo enabled (file-name suffixes and `//go:build` lines), then parsed and type-checked with the Go standard library (`go/parser`, `go/types`), one package at a time, in dependency order. A package's in-package test files are checked with it; an external test package (`package x_test`) is checked on its own.
- Imports from outside the repository are **not loaded**: they are replaced by empty packages, so calls into them are not resolved. The index never runs the go command, cgo, `go generate`, build scripts, gopls or any other tool, and uses no network.
- Every recorded position is the file's own path and line. `//line` directives, which generated code often contains and the candidate controls, are ignored, so they cannot move a site to another file name or line.

The index is built only when the change touches at least one Go file it may read: a Go file under `testdata` or `vendor`, in a directory whose name starts with `_` or `.`, or on a sensitive path does not count. Otherwise the section says `not_applicable` and the reviewer tools stay lexical.

## TypeScript/JavaScript, Python and Rust (lexical index)

When a change touches a TypeScript/JavaScript (`.ts`, `.tsx`, `.mts`, `.cts`, `.js`, `.jsx`, `.mjs`, `.cjs`), Python (`.py`) or Rust (`.rs`) file, the same index also holds those sources, read from the same committed Git objects. There is no type checker for them in the Go standard library, so they are **scanned lexically**:

- Declaration files (`.d.ts`), minified bundles, and files under `node_modules`, `bower_components`, `dist`, `build`, `coverage`, `target`, `venv`, `__pycache__`, `site-packages`, `vendor` or a directory whose name starts with `.` are not read, nor are sensitive paths.
- A per-language tokenizer drops comments and keeps strings, template literals, regular expressions, raw strings and char literals as single tokens.
- Declarations: TS/JS `function`s, `const`/`let` arrow and function expressions, and class methods (`Class.method`); Python module-level `def`s and class methods (`Class.method`, nested classes included); Rust `fn`s and methods of `impl` and `trait` blocks (`Type.method`, the self type of `impl Trait for Type`). Nested functions and closures belong to their enclosing declaration.
- Tests: `it(...)`/`test(...)` blocks in `*.test.*`, `*.spec.*` and `__tests__/` files (named `describe > title`); `test*` functions and methods of `Test*`/`TestCase` classes in `test_*.py`, `*_test.py`, `conftest.py` and `tests/`; Rust functions with a `#[test]`, `#[tokio::test]`, `#[rstest]`-style attribute. Helpers inside a Rust `#[cfg(test)]` module are test code.
- Calls are identifiers followed by `(`. A call is linked **by name** to the same-named declarations of the same language: `this.`/`self.` calls to the caller's own class first, `Module.f`/`module::f`/`Type::f` to the declaring file or type, plain calls to the same file first. A name with more than 3 candidates, or a common method name (`get`, `push`, `map`, `unwrap`...) called on a value of unknown type, is recorded as a name match only (reviewer tools), never linked. Paths through external types (`Vec::new`) are not linked.
- Changed functions are compared per file (following renames) and name, by the token digests of their signature and body.

Links from the lexical index carry the resolution `name`, the `impacted_caller` evidence says "Lexical index (approximate, linked by name only)", and reviewer tools answer with the method `lexical_name_index`. The section lists `languages` (for example `["go", "python"]`) whenever such files changed, and the fixed note gains a sentence on the lexical method. In a repository mixing Go and other languages, an unavailable Go index (for example no `go.mod`) makes the section `limited` and leaves the lexical part working. `--impacted-tests` still runs Go tests only: reaching tests of other languages are listed and reported as not run.

## Enabling and disabling

Impact analysis is on by default for `lint` and `review`. `--impact=false` disables it: the report then has no `impact` object, no impact signal is added, and the reviewer tools stay lexical. There is no policy key, so the candidate cannot enable, disable or tune it. Automatic analysis needs no policy migration, but passing `--impact` requires a binary that recognizes the flag (see [release ordering](CI.md#release-ordering-for-v04)). `--impacted-tests` (review only) needs the index and exits 3 with `--impact=false`.

## Changed functions

A changed function is a function or method of a changed non-test Go file whose candidate version differs from its baseline version:

- `signature_changed` when the tokens of the declaration up to its body differ;
- `body_changed` when only the tokens of the body differ.

Tokens are compared with the digest the linter uses (`linter.TokenDigest`): comments and spacing are ignored. Joining or splitting lines can change where Go inserts semicolons, so such a reformatting is reported as `body_changed`, the conservative direction. A function is identified by its directory, package clause and name (the receiver type for a method), not by its file, so a function moved between files of one package without other change is not listed. Added and removed functions are not changed functions. `init` functions, blank (`_`) functions and `main` in package `main` cannot be called and are not listed. Test files are not compared: [changed baseline tests](BASE_TESTS.md) handle changed tests.

## Callers and resolution

The callers of a changed function are its reference sites in **unchanged, non-test code**: a reference inside a changed or added function, on an added line, or in a `_test.go` file belongs to the change or to the tests and is not listed. One site is kept per line. Each site has a resolution:

| Resolution | Meaning |
| --- | --- |
| `static` | `go/types` resolved the identifier to the changed function (generic functions and methods of instantiated types map to their origin; promoted methods resolve to the embedded type's method). This includes references that do not call, such as a function value passed as an argument. |
| `interface` | A call of a method of a named interface that the changed method's receiver type (`T` or `*T`) implements. The call **may** dispatch to the changed method; dispatch is not resolved. For a method of a generic type, these are the interfaces that every instantiation implements (the matching methods' signatures do not use the type parameters). An interface that only some instantiation may implement is not searched, and the section is `limited`. |

A method call on a value whose type could not be resolved (typically a value from a package outside the repository) is recorded by name only. It appears in `find_references` output as a `name_match` and never in callers, signals or test selection.

## Reaching tests

A reaching test is an existing `TestX(t *testing.T)` function (recognized syntactically in a `_test.go` file, since the testing package is not loaded) that reaches the changed function within 3 references, through static references or interface calls. Each test is reported at the smallest depth found, breadth first in index order, with the chain of declarations (`via`) and whether the change touched its file (`file_changed`). A test that reaches a function is not evidence that it asserts that function's behavior. Benchmarks, examples and fuzz targets are not selected.

## Signals

| Kind | Severity | Where | When |
| --- | --- | --- | --- |
| `impacted_caller` | low | the caller's line (new side) | one per listed caller site: "Unchanged caller of a changed Go function", "Unchanged reference to a changed Go function" or "Unchanged interface call that may dispatch to a changed Go method" |
| `analysis_limited` (symbol `impact_index`) | medium | the first caller site left out, else line 1 of the first changed Go file (file-level, `"scope": "file"`) | at most one per run, when caller signals were left out by the caps, or when the index is limited or unavailable and changed functions are concerned |

At most 10 `impacted_caller` signals are added per changed function and 100 per run; the `analysis_limited` signal states how many caller sites were left out. The low targets land on unchanged lines, so the review surface's focused-line count does not change. Neither kind requests human review or changes the exit code.

## Statuses

| `impact.status` | Meaning |
| --- | --- |
| `not_applicable` | No indexable Go file changed; no index was built. Go files may still have changed under `testdata`, `vendor`, ignored directories or sensitive paths, which the index never reads. |
| `indexed` | The index was built and searched without a known gap. It remains approximate (see above). |
| `limited` | The index was built, but part of it or of its searches is known to be missing. `reason` lists each cause: files larger than 2 MiB, files that could not be parsed, files whose package clause differs from their directory's package, `go.mod` files without a readable module path or with a duplicate module path (counted in one reason), packages whose type check stopped, the reference limit, the unresolved-call limit, the time limit, changed files that could not be compared, changed functions beyond the listed 200, the caller or reaching-test search stopping at its visit limit or at the time limit, the reaching-test search stopping at its declaration limit, interface methods not checked because more than 200 share one method name, implementation checks not made because a receiver type is too large for their bound, and interfaces that a generic type may implement only once instantiated. `reason` is cut at 4 KiB. |
| `unavailable` | No index could be built: no `go.mod`, a tree over the file or byte limits, more packages than the limit, or a Git read failure. Changed functions are still listed, as not indexed. |

A changed function the index does not hold (for example one in a `_windows.go` file, excluded by the `linux/amd64` constraints) has `indexed: false` and a `reason`; its callers and tests were not searched. A method declared on an alias receiver (`type C = Cart`) is keyed by the aliased type (`cart.Cart.Total`) and found at its position. An indexed function whose caller or reaching-test search met a bound keeps what was found, has `indexed: true` and a `reason` naming the bound, and the section is `limited`.

## Report fields

`impact` is present in `lint` and `review` unless `--impact=false`.

| Field | Content |
| --- | --- |
| `status`, `reason` | See Statuses. |
| `indexed_files` | Go files the index type-checked. |
| `changed_functions[]` | `path`, `line`, `end_line`, `symbol` (import path and name, for example `example.test/shop/cart.Cart.Total`), `change`, `indexed`, `reason`, `callers` (at most 10: `path`, `line`, `symbol` of the referencing declaration, `depth` 1, `resolution`), `callers_total` (sites the bounded search found), `tests` (at most 20: `name`, `path`, `line`, `package` import path, `depth` 1 to 3, `resolution`, `via`, `file_changed`), `tests_total`. |
| `tests_status`, `tests_reason`, `tests[].evidence_id`, `tests[].status`, `tests[].reason` | Impacted tests, recorded only with `--impacted-tests` (review only). Each test status comes from verified evidence for that test alone; a status stored in a saved report is re-derived when it is rendered. |
| `note` | The fixed note: what the index is and what it does not establish. |

The Markdown report has an `## Impact Analysis` section after Changed-line Execution: the index status, each changed function (at most 20) with up to 5 callers and 5 tests, and the note. The console prints one line, for example:

```text
Impact analysis (static Go index, approximate): 2 changed Go functions; 2 caller sites in unchanged code and 2 reaching tests, counted per function.
```

It prints nothing when no indexable Go file changed, and the reason when the index is unavailable.

## Reviewer tools

When an index exists, `find_references`, `inspect_symbol` and `find_callers` answer from it for Go functions and methods. Every answer carries `"method": "go_static_index"` and a fixed `limitations` text, and one-line snippets of at most 240 bytes taken from the redaction of the whole file, as `read_file` and `search` show it (a credential whose key and value sit on different lines is masked there).

- A symbol may be a full key (`example.test/shop/price.Total`), a key suffix (`price.Total`), a package-qualified name, a bare name (`Total`, `Cart.Total`) or the `(*T).M` form. Several matches return the candidates instead.
- `find_references`: every recorded site, static then through interfaces, at most 100, plus unresolved method calls of the same name (`name_matches`, at most 20).
- `find_callers` (`depth` 1 to 3, default 1): the reference sites of the symbol, then of the declarations holding them, breadth first, at most 100 results and 5,000 visits, each with its depth and `via` chain. Test functions are listed but not followed, and the references of an interface method are followed once per query.
- `inspect_symbol`: the declaration (with `changed` when the change modified it), the number of reference sites, what it references, the interface methods a method may be dispatched from or the methods that implement an interface method, and the reaching tests.

A symbol that is not an indexed Go function or method (a type, a variable, TypeScript code) gets the lexical answer with an `index` note saying why; so does every query when no index exists. Index answers are observations: they never create evidence records. An answer that met one of its bounds (listed entries, visits, work units, time, the 200 interface methods checked per method name, a receiver type too large for an implementation check, or an interface that a generic type may implement only once instantiated) has `"truncated": true` and a `truncated_note`; a query stops with an error when the reviewer's own time limit ends first.

## Limits

| Limit | Value | Effect when reached |
| --- | --- | --- |
| Go and `go.mod` files read | 20,000 | `unavailable` |
| Source bytes read | 64 MiB | `unavailable` |
| One file | 2 MiB | the file is not indexed; `limited` |
| Packages | 5,000 | `unavailable` |
| Recorded references | 2,000,000 | later references dropped; `limited` |
| Unresolved method calls | 200,000 | later ones dropped; `limited` |
| Changed functions listed | 200 | the rest omitted; `limited` |
| Time | 120 s | checked between packages, on each type error, and during the impact searches before every interface-implementation check; remaining packages are not indexed, or remaining callers and tests are not searched; `limited`. The type check of one package that produces no type error is not interrupted and can run past the limit, so the limit does not bound the whole analysis. |
| Impact searches | 10,000,000 visits for the callers of all changed functions, and as many for their reaching tests (each reference visited costs one; each interface-implementation check costs at least one, and more in proportion to its estimated cost) | the functions not fully searched keep what was found and get a `reason`; `limited` |
| Reaching-test search | 50,000 declarations per changed function | the search stops; `limited` |
| Interface methods checked | 200 per method name | the others are not checked, so interface calls may be missing; `limited` |
| One implementation check | 8,388,608 estimated `go/types` steps for checking a receiver type against one interface. The estimate grows with the receiver's methods and fields, with the square of the number of types it embeds at one level, and with the interface's method count; a struct embedding about 1,000 types reaches it. | the check is not made, so interface calls through it may be missing; `limited` |
| Depth | 3 | callers and tests beyond it are not searched |
| Callers listed / signals | 10 per function; 100 signals per run | `callers_total` and the `analysis_limited` signal state the rest |
| Reaching tests listed | 20 per function | `tests_total` counts all found |
| Tool answers | 100 results, 5,000 visits, 2,000,000 work units and 10 s per query (the time is checked before every implementation check), 20 candidates, 20 name matches | `truncated` |

The time limit makes the result depend on the machine when it is reached; it is then reported. `--deadline` counts the analysis against the overall deadline but does not interrupt it. A cancelled run (for example Ctrl-C) stops it between packages and during the searches, before the next reference or implementation check, but not inside the type check of one package. Measured costs are in [performance](PERFORMANCE.md).

## Exit effects

The static section never changes the exit code: `impacted_caller` (low) and `analysis_limited` (medium) do not request human review, and a limited or unavailable index is not an operational failure. Only a cancelled run stops the analysis with an error. Impacted tests (`--impacted-tests`) may request review; nothing in impact analysis produces exit 1.

## Trust and security

The index parses and type-checks untrusted committed source in process, with the standard library only. It runs no repository code, no tool and no network access, reads only committed Git objects of the candidate commit (and of the base commit for the changed files), and skips sensitive paths and symlinks. Its work is bounded by the limits above: reading and parsing by the file and byte limits; type-checking by the package limit and the time limit, which is checked between packages and on each type error; and the impact searches and tool queries by their visit and work budgets and time limits, checked before every interface-implementation check, with each check's estimated `go/types` cost bounded. The type check of one package that produces no type error is not interrupted. Some `go/types` work grows faster than the source (a selector on a struct that embeds thousands of types costs a lookup quadratic in their number), so a small crafted package can keep that check, and the process, busy past the time limit, and Ctrl-C does not stop it before the check ends. A panic inside `go/types` is recovered per package and reported as `limited`. `//line` directives are ignored, so the candidate cannot move a recorded site to another file or line. An imported package is marked so that `go/types` does not build error messages for names it lacks, which would otherwise cost a lookup over the imported package per missing name. The candidate fully controls what is indexed, so it can hide callers (reflection, function values) or add reference sites: the index therefore only adds low-severity signals and tool observations. Snippets and report strings are redacted.

## What this does not claim

- Not "no callers", "unused", "all callers", or a complete call graph: an absent caller is not proof of absence, and callers outside the repository, through function values, reflection, `go:linkname`, assembly, generated code, or imports that are not loaded, are not found.
- Not that an interface call dispatches to the changed method at run time: it is possible dispatch only.
- Not that a caller is affected by the change, or that the change is safe to make.
- Not that a reaching test exercises, covers or asserts the changed behavior: it reaches the function within 3 references in the static index.
- Not the compiler's view: imports from outside the repository are not loaded, `linux/amd64` constraints select the files, and a directory's in-package test files are checked with its package.
- TypeScript and JavaScript are not indexed; their lookups stay lexical.

<!-- F6b:begin -->
## Impacted tests (`--impacted-tests`)

The index lists existing tests that reach a changed function; it does not run them. `probe review --impacted-tests` runs the listed tests whose file the change did not modify on the baseline and on the candidate, inside the sandbox, and records what the two runs showed. No model is involved. It complements [changed baseline tests](BASE_TESTS.md), which covers the tests of modified test files.

The stage is opt-in, runs during `review` only, executes Go tests only, and adds no policy key. It adds evidence and review requests; it never produces exit 1 and never removes, lowers or dismisses anything else in the report. It adds no signal kind: a test that fails on the candidate becomes a high review target at its declaration.

### Enabling it

```sh
probe review --base main --impacted-tests --ci
```

- `--impacted-tests` exits 3 on `lint`, together with `--checks=false`, and together with `--impact=false`, before any container starts.
- The trusted policy's `generated_test` command must let Probe establish which Go tests ran: `go test` with exactly one standalone `{package}` or `{file}` target and only flags otherwise, without `-C`, `-exec`, `-overlay`, `-args` or `--`. `["go", "test", "{package}"]` is the recommended template. With `{file}`, `go test` compiles only that test file, without the package's other files: a test file of the package itself (for example `package price`) that uses package code does not build on the baseline, so its tests get no result and a reason, and the stage requests review. An external test file (`package price_test`) that imports the package can build. With any other template nothing runs and `tests_status` is `not_run` with the reason.
- A binary without the flag exits 3 while parsing arguments. Re-pin to v0.4.0 or later before using it; intermediate v0.3 builds may accept the flag without the completed stage (see [CI integration](CI.md#release-ordering-for-v04)).

### What is selected

Selection is static and reads only the impact section:

- the reaching tests listed under each indexed changed function (at most 20 per function; the ones the index found beyond that are not considered, and `tests_reason` counts them). A changed function that is not in the index (`indexed` false, for example in a file that `linux/amd64` constraints exclude) lists no reaching test because none was searched; `tests_reason` counts such functions;
- whose `file_changed` is false. A reaching test declared in a test file the change modified is not run and gets the reason "not run: its test file was modified by the change (--base-tests runs the baseline versions of changed tests)";
- one per file and name, at its smallest depth, ordered by depth, then path and name.

Before anything runs, each selected test must have a Go test name (`Test`, or `Test` followed by a non-lowercase character, letters, digits and `_` only), and its file must be a regular `*_test.go` file of at most 4 MiB that is byte-identical in the baseline and candidate snapshots and declares a top-level function of that name. A test file on a path the snapshots exclude (secret-bearing names) never runs. Any other test gets a reason starting with "not run: " and no status.

At most 16 tests from at most 4 packages (4 test files with a `{file}` template) run per review, in the order above. The others get the reason "not run: the stage runs at most 16 tests from at most 4 packages per review" ("4 test files" with a `{file}` template), `tests_reason` counts them, and they request review like any selected test without a result: the candidate shapes the index, and so which tests fill the limits.

When nothing can be selected, `tests_status` is `no_candidates` with the reason: no indexable Go file changed; the index lists no reaching test (which is not proof that none exists; a `limited` index adds that reaching tests may be missing, and the reason counts the changed functions that are not in the index); or every listed reaching test is in a modified test file. An `unavailable` index, and a section in which no changed function is in the index, give `not_run`: no reaching test was searched.

### The two runs

The selected tests of one package directory (one test file with a `{file}` template) run twice with one identical command, the `generated_test` template with the package substituted, followed by `-json -count=1 -run ^(Name1|Name2)$`:

1. **Baseline** (check kind `impacted_test_base`): the baseline snapshot.
2. **Candidate** (check kind `impacted_test_candidate`): the candidate snapshot.

Both runs use the unchanged sandbox profile: the same image, no network unless the policy and `--allow-network` both allow it, read-only source mount, limits and non-root user. Two refinements keep one failing test from hiding the others:

- When the baseline run fails as a whole, the tests it records as passed run again on their own. The others get no candidate run and no status, with the reason (for example "the test failed on the baseline (check-5), so it was not run on candidate code").
- When the candidate run fails as a whole, each test it records as passed gets one more run pair of its own, because a pass inside a failed run never supports `PASSES_ON_CANDIDATE`. No such pair runs when it would only repeat the failed run: the run held only that test, or every test in it passed (the run failed for another reason, such as the exit code of `TestMain`). Those tests stay `UNVERIFIED` with a reason that says so.

With an execution cache (`--cache-dir`), a baseline run may be replayed. A replay never supports `FAILS_ON_CANDIDATE`: the baseline runs again live with the same kind and command, and the test is classified against the live run; when that run cannot start, the test is `UNVERIFIED`. `PASSES_ON_CANDIDATE` may rest on a replay that two agreeing live runs recorded, and the report lists it in `execution.replay_backed`. Candidate runs are never cached.

### Statuses

Each run pair is classified per test name from the `go test -json` events of both recorded checks. The name must have exactly one `run` event and exactly one terminal event, in one package, on both sides; a second terminal event, such as a pass printed after a real failure, makes the result unknown.

| Status | Requires | It is | It is not |
|---|---|---|---|
| `FAILS_ON_CANDIDATE` | Identical commands; baseline check PASS with exit 0, an untruncated log and the test passing; candidate check FAIL with exit 1–124, an untruncated log and the test failing. | An outcome difference between one recorded run each, of an unchanged test that the static index links to a changed function, for a human to judge. | A reproduced issue or a regression. Caused by the changed function: the static link is approximate, and the failure may come from any part of the change or from flakiness. Proof that the test encodes the intended behavior: the change may intend to alter it. |
| `PASSES_ON_CANDIDATE` | The same baseline conditions; candidate check PASS with exit 0 and the test passing. | This one test passed in both recorded runs. | Preserved behavior, correct code, or evidence that the test asserts the changed behavior. Tamper-proof: code executing in the sandbox writes the log the result is read from. It says nothing about tests that did not run. |
| `UNVERIFIED` | Anything else with a recorded run pair. | A test for which no result was drawn, with the reason. | A failure or a pass. |

A run pair is a baseline run and a candidate run that both started. A test without a run pair has no status, no evidence record and a reason. Typical reasons: the test failed or was skipped on the baseline, the baseline package did not build, a build constraint excluded its file, the baseline run timed out, ended as ERROR or was cut by `sandbox.max_output_bytes`, a baseline or candidate run did not start (runtime budget, reviewer reserve or overall deadline), the stage's 180 s were used up, the file checks above, and the stage limits. When no selected test of a unit passed on the baseline, the candidate run is not started, because none of them could get a result.

A test with a run pair and neither result is `UNVERIFIED`, with a reason. Typical reasons: the candidate package did not build (for example after an API change), the candidate run timed out, ended as ERROR or was cut by `sandbox.max_output_bytes`, the test was skipped or had no single result on the candidate, a replayed baseline could not be repeated live, and a test that passed inside a candidate run that failed as a whole got no result from a pair of its own.

`report.Finalize` re-derives every status from the recorded checks, including when `probe report` re-renders a saved report: a test is `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` only when its `impacted_test_differential` evidence record resolves, names the same test and file, cites an `impacted_test_base` and an `impacted_test_candidate` check with the same command targeting that package (the candidate one never replayed), and those checks give that status under the rule above. Every listed entry of one test shows the same result. An `impacted_test_differential` record never supports a hypothesis status: a model cannot turn it into a reproduced, not reproduced or dismissed hypothesis.

`tests_status` is `ran` when at least one run started, `no_candidates` when nothing could be selected, and `not_run` with a reason otherwise. With `ran`, `tests_reason` states what was left out (tests over the limits, tests of modified test files, tests beyond the 20 listed per function, changed functions that are not in the index), or is absent.

### Exit codes

| Result | Without `--ci` | With `--ci` |
|---|---|---|
| Any `FAILS_ON_CANDIDATE` or `UNVERIFIED` test, a selected test without a result (including tests left out by the stage limits), or `tests_status` `not_run` | 0 | 2 |
| Only `PASSES_ON_CANDIDATE`, or `no_candidates` | 0 | 0 |
| The candidate package does not compile (a FAIL check, never ERROR) | 0 | 2 |
| Infrastructure failure of a run (Docker error, exit code 125 or above, lost log): ERROR check | 4 | 4 |
| `--impacted-tests` on `lint`, with `--checks=false` or with `--impact=false` | 3 | 3 |

The stage never produces exit 1: only a reproduced high/critical hypothesis does. The log of a candidate run is written by candidate code, so its text never makes the check ERROR.

### Runtime budget

Every run is charged to the shared `sandbox.max_runtime_seconds` budget. The stage has a 180 s sub-cap inside it: each run's timeout is at most what remains of the 180 s and of `sandbox.timeout_seconds`, and no run starts once the stage has used 180 s. When a reviewer will run, the stage also stops at `max_runtime_seconds` minus the half reserved for reviewer experiments. A typical change costs two runs per package with selected tests (at most 4 packages); the refinements above can add one baseline run and one pair per unit, and a cache confirmation one baseline run. The stage runs after the initial checks, coverage and changed baseline tests, before fuzzing, mutation and the reviewer.

### Report output

- JSON: `impact.tests_status` and `impact.tests_reason`, present exactly when `--impacted-tests` was passed to `review`, and on each listed test `evidence_id`, `status` and `reason`.
- Evidence: one `impacted_test_differential` record per test (file and name) and run pair, runner `go_test_json`, exactly one test name, `check_id` (candidate run) and `base_check_id` (baseline run). Several records may cite the same package-level pair of checks. A candidate run that did not start gives no record.
- Markdown: the Impact Analysis section shows the status and evidence ID next to each listed test and ends with "Impacted tests: <tests_status>" and the reason. A test without a `FAILS_ON_CANDIDATE` or `PASSES_ON_CANDIDATE` result also shows its reason; a stored result that the recorded evidence does not support becomes `UNVERIFIED` with the reason "the recorded evidence does not support this result".
- Suggested Human Review: a high target at the declaration of each `FAILS_ON_CANDIDATE` test. The test file is unchanged, so the target lies outside the changed lines.
- Unverified Areas: "Impacted tests did not run: <reason>" when `tests_status` is `not_run`, including a stage that was never reached (for example after failed dependency preparation); "Some impacted tests got no FAILS\_ON\_CANDIDATE or PASSES\_ON\_CANDIDATE result; …" when a test within the limits has no result; and "N selected impacted tests were not run because of the stage limits …" when the limits left tests out.
- Audit: one `stage:run_impacted_tests` event per unit, with its checks and the most severe of their statuses (ERROR, then TIMEOUT, FAIL, SKIPPED, PASS).
- Console: the impact line ends with the finalized counts, for example `Impacted tests: ran; 2 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED.`

### Security notes

The test names come from candidate source through the index. They run only when they are Go test names declared in a test file that is byte-identical in both snapshots, and they reach `-run` quoted with `regexp.QuoteMeta`; the command is otherwise the base-branch `generated_test` template. The runs add no mount, volume, network or payload channel and use the unchanged sandbox profile; unchanged baseline test code runs against candidate code, as in the existing experiments. The candidate shapes the index, so it can add or hide reaching tests and influence which tests fill the limits: selection can only add runs and review requests, within fixed limits, and a selected test that the limits leave out requests review. Candidate code can make a test pass on purpose (for example by detecting the sandbox), so `PASSES_ON_CANDIDATE` is an observation only; a forged pass after a real failure makes the result `UNVERIFIED`, and forging a failure only adds a review request. The flag is set by whoever invokes Probe, never by the candidate branch.

### Limitations

- Go only, and only the tests the index lists (20 per changed function, within 3 references); tests reached through function values, reflection or imports that are not loaded are not found.
- Tests of modified test files are not run here (see `--base-tests`); added test files have no baseline version.
- One run each: a flaky test can produce `FAILS_ON_CANDIDATE`, and a failure may come from any part of the change, not only from the function that selected the test.
- The index selects files with `linux/amd64` constraints; a test file excluded in the sandbox does not run and stays without a result.
- Benchmarks, examples and fuzz targets are not selected.

### Example

From a real run (Windows binary, `golang:1.26-bookworm`, 2026-09-26; see [validation](VALIDATION.md)). On the shop fixture, the candidate's `price.Total` skips the first item; `price/price_test.go` (`TestTotal`, `TestTotalZero`) and `api/handler_test.go` (`TestCheckout`) are unchanged. `review --impacted-tests --reviewer=false --ci` with the default Go policy exits 2 and prints:

```text
Impact analysis (static Go index, approximate): 2 changed Go functions; 2 caller sites in unchanged code and 3 reaching tests, counted per function. Impacted tests: ran; 2 FAILS_ON_CANDIDATE, 1 PASSES_ON_CANDIDATE, 0 UNVERIFIED.
```

The Impact Analysis section lists, under `price.Total`:

```text
  - test TestTotal at price/price\_test.go:5 (depth 1, static; FAILS\_ON\_CANDIDATE (evidence-1))
  - test TestTotalZero at price/price\_test.go:11 (depth 1, static; PASSES\_ON\_CANDIDATE (evidence-2))
  - test TestCheckout at api/handler\_test.go:5 (depth 2, static; FAILS\_ON\_CANDIDATE (evidence-3))
```

`TestTotalZero` passed inside the failed candidate run of `./price` and got a run pair of its own. The two failing tests are high targets in Suggested Human Review; there is no reproduced issue.
<!-- F6b:end -->

## See also

- [Changed baseline tests](BASE_TESTS.md): the other `FAILS_ON_CANDIDATE` source.
- [Execution cache](EXECUTION_CACHE.md): the live-baseline rule for impacted tests.
- [Exports](EXPORTS.md): the `impacted_test_fails_on_candidate` class (always unanchored in SARIF).
- [Security boundaries](SECURITY.md).
