# Differential fuzzing (`fuzz` policy object)

A changed function can behave differently on inputs that neither the change's tests nor a reviewer thought of. With a `fuzz` object in the trusted policy, `swiftproof review` takes the changed package-level Go functions whose signature is unchanged, runs the **same seeded inputs** through each of them on the baseline and on the candidate inside the sandbox, and compares what the two revisions recorded. No model is involved, and nothing asserts an expected value: a difference is recorded, never judged.

A function whose two revisions recorded different values for an input, and each repeated its own value in a second, independent run, is `diverged`. That is an observation about the recorded runs: it does not say which revision is correct, and the change may be intended. A function whose recorded values were equal for every compared input is `not_diverged`; that does not establish equivalent behavior, not even for the tried inputs, because only bounded, redacted encodings of results, recovered panics and slice arguments after the call are compared. Anything else is `inconclusive`, which is neither a divergence nor a defect.

Fuzzing never produces exit 1, never creates a reproduced issue, and never removes, lowers or dismisses anything else in the report. A divergence, an inconclusive function and a stage that did not run request human review (exit 2 with `--ci`).

## Enabling it

Add the object to the policy at the tip of the base branch (or pass a policy with `--config`). `{}` enables it with the defaults:

```json
"fuzz": { "max_functions": 8, "max_packages": 4, "max_inputs": 64, "call_timeout_ms": 1000, "max_runtime_seconds": 240 }
```

| Field | Default | Range |
|---|---|---|
| `max_functions` | 8 | 1 to 32 functions per review |
| `max_packages` | 4 | 1 to 16 packages per review |
| `max_inputs` | 64 | 1 to 256 seeded inputs per function (one package run plans at most 1,024 inputs in total) |
| `call_timeout_ms` | 1000 | 10 to 10,000, and at most `sandbox.timeout_seconds` × 1000: the bound of one evaluation of one input |
| `max_runtime_seconds` | 240, lowered to `sandbox.max_runtime_seconds` when that is smaller | 1 to 7,200, and at most `sandbox.max_runtime_seconds`: a sub-cap of the stage **inside** the shared sandbox budget |

`0` means the default; any other value outside its range, and any unknown field, exits 3 before any container starts. An absent key or `"fuzz": null` disables fuzzing.

The stage runs the reviewed `generated_test` command, which must be a single-package `go test` template with the standalone `{package}` target and flags only otherwise, such as the Go default `["go", "test", "{package}"]`. The harness appends `-json -count=1 -run ^(names)$`, as for generated tests; there is no new command key and no new placeholder. With changed Go functions to fuzz and any other template (`{file}`, `./...`, `-exec`, `-C`, `-overlay`, a non-Go runner), the section is `not_run` with the reason and an Unverified entry.

**Release ordering.** Every binary before v0.4.0 exits 3 on a policy that contains `fuzz`. Publish v0.4.0, re-pin every workflow that reviews the base branch (URL and sha256), and only then commit the object; see [CI integration](CI.md#release-ordering-for-v04). `swiftproof init` never writes it. A candidate branch cannot enable, widen or redirect fuzzing: the policy comes from the base branch or `--config`.

**When it runs.** In `review` only, with initial checks enabled (the default), after the initial checks, coverage, changed baseline tests and impacted tests, and before mutation and the reviewer. `--fuzz=false` records the section as `disabled` for one run, and so does `--checks=false`. `lint` never fuzzes and writes no `fuzz` object. A change without changed files records `no_candidates`; a review that executes nothing (a failed dependency preparation, for example) records `not_run` with the reason.

## Which functions are fuzzed

Selection only parses the two sanitized snapshots on the host (`go/parser`, `go/build.MatchFile` over in-memory files); it never runs repository code. A function is fuzzed when it is a package-level function of a changed non-test Go file whose body changed (comments and layout do not count), whose types-only signature is textually identical on both revisions, and whose parameters can be generated:

- `bool`, `string`, `int`, `int8` to `int64`, `rune`, `uint`, `uint8` to `uint64`, `byte`, `float32`, `float64`;
- a package-local named type (or alias) of one of them whose declaration is identical on both revisions;
- `[]E`, `[N]E` with a decimal `N` from 0 to 16, and `...E`, of those.

Every other changed function is listed in `fuzz.skipped` with a fixed reason: methods, generic functions, a changed signature, a parameter type that is not generated (maps, pointers, structs, interfaces, functions, channels, complex numbers, `uintptr`, qualified types such as `time.Duration`, nested composites), a named type that differs between revisions, build constraints, cgo, a package clause that differs, a shadowed predeclared identifier, a sensitive path, a file moved to another directory, a package that cannot be read or parsed within the bounds, and "fuzz budget reached (max_packages)" or "(max_functions)". Functions that exist on one revision only are not rewrites and are not listed.

Functions are ordered by the highest severity of the new-side signals overlapping them, then exported before unexported, then path and line. The full rules are in the [v0.4 specification](../../specs/swiftproof-v0.4-spec.md#f2-deterministic-differential-fuzzing), §F2.1.

**TypeScript and JavaScript.** No TS/JS harness runs in this build. Changed exported TS/JS functions whose body changed and whose signature text is unchanged (function declarations, and `const`, `let` or `var` bindings of function expressions and arrow functions, at the top level of a modified or renamed non-test module) are listed in `fuzz.skipped` with "TS/JS differential fuzzing is not implemented in this build". The enumeration is lexical and best-effort: a construct it does not recognize is simply not listed. They add no Unverified entry and no review request. Because the modules are candidate content read on the host, the enumeration is bounded linearly in their size whatever they contain: at most 2 MiB per file and 32 MiB in total, 1,048,576 tokens per file, template literals nested at most 64 deep, and at most 16 token visits per token for the parse of a file; it also stops at `fuzz.max_runtime_seconds` and at `--deadline`. A file that one of these bounds stops gets one file-level entry (line 0, no symbol) with the bound as its reason, never a partial list.

## Seeded inputs

Each function gets a deterministic corpus derived only from its identity: the seed is the SHA-256 of `swiftproof-fuzz/v1`, the package directory, the function name and the signature, and the generator is SplitMix64. The same function therefore gets the same inputs on every run and across updates of a pull request. Edge values come first (zero, one, minus one, the bounds of each integer kind, NaN, the infinities and negative zero, the empty string, separators, multibyte and invalid UTF-8, nil and empty slices, and so on), then one parameter at a time through its edge values, then seeded random values. Inputs are deduplicated by their call text, for example `Discount(Cents(1000))`, which is also the key under which values are shown.

## The harness and the runs

For each package, SwiftProof renders **one internal `_test.go` file** on the host, `<dir>/swiftproof_fuzz_<suffix>_test.go`, with one test per function. Every identifier it declares carries a suffix drawn at random for each run, so code under review cannot declare a colliding name in advance. For every input it evaluates the function twice with fresh arguments, each evaluation in its own goroutine bounded by `call_timeout_ms`, and appends one JSON line per input to `/tmp/swiftproof-observations.jsonl`: the SHA-256 and length of a canonical encoding of the results, a recovered panic as `panic(...)`, and each slice argument after the call, a display cut of at most 256 bytes, and whether the two evaluations differed. It asserts nothing.

The identical file is staged in both snapshots (never overwriting anything) and removed after the runs. It runs with the unchanged sandbox profile: same image, no network unless the policy and `--allow-network` both allow it, read-only root filesystem and source mount, dropped capabilities, no new privileges, non-root user, memory, CPU, PID, time and output limits, and a fresh container. The observation file returns on the same bounded, framed payload channel as a coverage profile.

1. **First pair**: `fuzz_base` on the baseline, then `fuzz_candidate` on the candidate. The first baseline run may be served from the opt-in execution cache.
2. **Confirmation pair**, only when the first pair recorded a different value for an input that neither revision flagged as unstable, and the sub-cap has time left: `fuzz_base_confirm` and `fuzz_candidate_confirm`, always live, with the same file and command.

Packages run one after another in plan order. Each harness is retained as a hashed `fuzz_harness` artifact, and each normalized stream as a hashed `fuzz_observations` artifact. Harness audit events are `stage:run_fuzz` and `stage:run_fuzz_confirm`.

**ERROR or FAIL.** A baseline-side run (`fuzz_base`, `fuzz_base_confirm`) becomes ERROR only in these cases:

- its log is complete and shows that the harness did not build or start on the baseline (no harness test recorded a `run` event), or, for a passing run, that some harness test recorded no `run` event;
- it passed but returned no observation stream, a payload that is not one complete frame, or a stream the validator rejects;
- its stream could not be retained as an artifact, or an infrastructure cause: a Docker error, exit code 125 or above, or a lost log artifact.

The baseline is trusted code, so these are failures of the tool on it (exit 4). A log cut by `sandbox.max_output_bytes` never decides ERROR: when baseline functions print enough to cut the log, the run keeps PASS or FAIL and its functions are `inconclusive` ("the baseline run log was truncated"). The cause of an ERROR is appended as the last line of the check's recorded output (`swiftproof: <cause>`, within the output bound) after the `check-N.log` artifact was retained, so that line is not in the artifact; the `stage:run_fuzz` or `stage:run_fuzz_confirm` audit event also records it as `baseline_error`.

A candidate-side run keeps FAIL for a compile, setup or run failure, whatever its log says; only an infrastructure cause (a Docker error, exit code 125 or above, a lost artifact) makes it ERROR. Neither a candidate-side log nor a cut baseline log decides ERROR, so what code under review writes cannot force exit 4 through fuzzing, and a candidate that does not build makes its functions `inconclusive`. The change does choose which baseline functions are fuzzed; a baseline function that itself deletes or overwrites the observation file while a passing run evaluates it still gives a baseline ERROR.

**Stream validation.** A stream is accepted only when every line is the byte-exact canonical form of one record in the expected order; otherwise the whole stream is rejected and a bounded, redacted copy is kept as a `fuzz_payload_rejected` artifact. The normalized stream stored in `Check.Results` must be unchanged by redaction. The report keeps at most 16 MiB of structured results (candidate-side runs at most half of it): a stream beyond what remains is kept only as its artifact, `fuzz.functions[].results_sha256` names it, and the function is `inconclusive` ("observation stream exceeded the report budget").

## Outcomes

Per input: *not recorded* when either first-pair record is missing; *unstable* when either first-pair evaluation pair differed; *compared* when the two hashes and lengths are equal. A difference needs the confirmation pair: each revision must repeat its own first value there (else *unstable*), and then the input is *compared* and *diverged*; without confirmation records it is *unconfirmed*. A run's records are used only when it is PASS with exit code 0 or FAIL with exit code 1 to 124, its log is not truncated and records exactly one run and one pass of the function's test in one package, and its stream plans the same inputs; a FAIL run still supports the functions whose tests passed before its process ended. All the runs used for a function must share one command and one package.

| Outcome | Evidence status | Meaning |
|---|---|---|
| `diverged` | `DIVERGED` | At least one input diverged. The counterexample is the divergent input with the shortest call text (then the lowest index) among those whose displayed values differ. It is the smallest divergent input tried; there is no shrinking. |
| `not_diverged` | `NOT_DIVERGED` | Both first-pair streams are complete, at least one input was compared, every compared input recorded equal encodings, and no unstable input separates the revisions. |
| `inconclusive` | `UNVERIFIED`, or no record | Anything else: a timeout, a crash, a process exit, `runtime.Goexit`, instability, an unconfirmed difference, a rejected or missing stream, a run that did not complete, a budget cut. The reason names the input being evaluated when a process ended or a function stopped. |

Every function whose package run recorded checks gets one `differential_fuzz` evidence record: runner `go_test_json`, the harness path, the one test name, the first pair as `check_id` and `base_check_id`. A `DIVERGED` record can support a `DIVERGED` hypothesis; no fuzz record supports `REPRODUCED`, `NOT_REPRODUCED` or `DISMISSED`, and `NOT_DIVERGED` supports nothing.

## Re-derivation by `Finalize`

`report.Finalize` derives every outcome again from the saved report, including when `swiftproof report` re-renders it, with the comparison the stage used. A `diverged` or `not_diverged` function keeps its outcome only when exactly one function cites its evidence record; the record is `differential_fuzz` with runner `go_test_json`, the function's test name and a harness path that report sanitizing leaves unchanged; its `check_id` and `base_check_id` are the function's first pair (kinds `fuzz_candidate` and `fuzz_base`), each recorded once; the confirmation pair is complete (kinds `fuzz_candidate_confirm` and `fuzz_base_confirm`) or absent; the baseline command runs the harness's package and test; the derived status equals the stored one; and the function records exactly the derived outcome, reason, counts and counterexample.

The live-baseline rule applies: `DIVERGED` needs a confirmation pair that was executed, never replayed; `NOT_DIVERGED` may rest on a replayed first baseline run only when two agreeing live runs recorded its cache entry; a candidate-side check is never a replay. Every other `diverged` or `not_diverged` function becomes `inconclusive` ("the recorded outcome could not be derived again from the recorded checks and observation streams") without a counterexample, and its divergence is not listed. An edit that breaks a stream's structure rejects the stream; a consistent edit of hashes, lengths, flags, commands and logs gives the outcome it describes. Displays are not protected, and reports are unsigned.

## Exit codes

| Result | Without `--ci` | With `--ci` |
|---|---|---|
| A `diverged` function (a behavior divergence) | 0 | 2 — never 1 |
| An `inconclusive` function, a budget cut, or section `not_run` | 0 | 2 |
| Every function `not_diverged`; section `disabled` or `no_candidates` | 0 | 0 |
| A candidate-side fuzz run that fails (compile, setup, crash) | 0 | 2 (its function is `inconclusive`) — never 4 |
| A baseline-side harness build, start or stream failure, or an infrastructure failure of any fuzz run | 4 | 4 |
| Invalid `fuzz` object | 3 | 3 |

## Runtime budget

Every fuzz run is charged to the shared `sandbox.max_runtime_seconds` budget. The stage stops at `fuzz.max_runtime_seconds`, counted from its start: no run starts after it, a run's timeout is at most what remains of it (and at most `sandbox.timeout_seconds`), and a run still going when it ends is stopped as TIMEOUT. A run of a started package that the sub-cap reaches before it starts (typically the candidate run after a baseline run that used up the sub-cap) is SKIPPED with "The time limit of the requesting stage (for example reviewer.timeout_seconds) expired before the run started; the run was not started.": for a fuzz check, that limit is `fuzz.max_runtime_seconds`, not the reviewer's. A package that the sub-cap or the overall `--deadline` stops before it starts gives `inconclusive` functions without checks. When a reviewer will run, fuzz runs also stop at `sandbox.max_runtime_seconds` minus the reviewer reserve (half the budget).

A package costs two containers, plus two more when its first pair shows a difference, so at most `4 × max_packages`. Every container starts with an empty build cache and compiles the package's tests again; see [performance](PERFORMANCE.md) for measured numbers. Size `sandbox.max_runtime_seconds` for the initial checks, coverage, the other stages, `fuzz.max_runtime_seconds`, mutation and the reviewer together.

## Report output

- **JSON** (`fuzz`): `status` (`ran`, `no_candidates`, `not_run`, `disabled`), `reason`, `seed_scheme`, the effective `limits`, `functions` (path, lines, symbol, signature, test name, outcome, reason, evidence ID, check IDs, counterexample, and the counts `inputs`, `compared`, `diverged`, `unstable`, `unconfirmed`, `not_recorded`), `skipped` (at most 200) with `skipped_total`, and a fixed `note`. The object is present exactly when the trusted policy has `fuzz` in review mode. The fuzz checks are in `checks`, with the normalized stream in `results`; the records are in `evidence`; every validated divergence is in `divergences` with its changed-function anchor, the four check IDs and up to 32 divergent inputs with both values.
- **Markdown**: "Differential Fuzzing", after "Mutation of Added Lines" (or "Changed-line Execution"). One line per function with its outcome, counts and, for a divergence, the smallest divergent input tried with both values; the other values are under "Behavior Divergences". Then the skipped functions (up to 20) and the note. No percentage.
- **stdout**: one line, for example `Differential fuzzing: 7 changed functions planned, 7 with recorded fuzz checks; 2 diverged, 2 not diverged, 3 inconclusive; 1 skipped.` A function that the sub-cap, the deadline or a harness failure kept from running has no recorded fuzz checks and is counted apart; the stdout line and the Markdown lead never say that it ran. The fixed note describes the method whatever the section status and never says that fuzzing ran.
- **Review targets**: the candidate lines of each diverged function (high) and each inconclusive function (medium).
- **Unverified**: one entry per inconclusive function and one for the functions cut by `max_functions` or `max_packages`, at most 20 in total.

## Security notes

Selection and rendering parse and print Go source on the host; repository code runs only through the sandbox, with the reviewed `generated_test` command. The harness is rendered from fixed template text, `strconv` literals and identifiers the package declares, each re-checked. Each revision writes its own observation stream from inside its own process: code under review can suppress, pad or fabricate the observations of its own revision, which can hide a divergence or show one that is itself candidate behavior, but it cannot write the baseline's stream or change the comparison. The report keeps the unsalted SHA-256 and length of every recorded encoding, also when its display was redacted, so a low-entropy value a function returned (a short password) can be recovered by hashing guesses; handle such reports as you would those values. Models cannot start, parameterize or create fuzz runs or records. See [security](SECURITY.md).

## Limitations

- Go only; no TS/JS harness runs in this build (the functions are listed as not fuzzed).
- Functions that print can fill the `go test -json` log: once it is cut at `sandbox.max_output_bytes`, the functions of that run are `inconclusive`. Raise the bound for packages whose functions write to standard output.
- Methods, generic functions, and struct, map, pointer, interface and qualified-type parameters are not generated; functions whose body is unchanged but whose callees changed are not fuzzed.
- Only results, recovered panics and slice arguments after the call are observed; files, globals, standard output, goroutines and logs are not. Encodings are bounded (64 KiB, depth 16, 1,024 elements) and errors are compared by message.
- The functions of one package run in one process in plan order: state an earlier function left behind can cause a difference recorded for a later function.
- A timeout poisons the process: later functions of the package are `inconclusive`. A candidate crash where the baseline completed is an inconclusive pointer to the input, not a divergence.
- There is no shrinking and no coverage-guided search; `int` and `uint` are assumed 64-bit, as on the sandbox platforms linux/amd64 and linux/arm64.
- Each run is a cold container; runs are sequential, and confirmation runs are never cached. The per-run random suffix changes the harness bytes on every review, so a first baseline run is in practice never served from the cache either.

## Example

From a real run (2026-09-26, `golang:1.26-bookworm`, a Windows binary built from the branch) on a two-commit fixture whose candidate rewrites the calc functions of the design (`Percent` loses its zero guard, `Join` is rewritten with a `strings.Builder`, `Discount` moves its boundary from `>=` to `>`, `Stamp` appends the current time, `Halt` exits the process at 7, and `Boundary` reports the sandbox user), breaks the build of a second package, and rewrites an exported TypeScript function. The policy had `"fuzz": {"max_runtime_seconds": 3000}` and `generated_test` `["go", "test", "{package}"]`. `review --reviewer=false --ci` exited 2 after 70 s (the binary of the F2b review fixes) and printed:

```text
2 recorded behavior divergences (baseline and candidate recorded different values; a human decides which is intended).
Differential fuzzing: 7 changed functions planned, 7 with recorded fuzz checks; 2 diverged, 2 not diverged, 3 inconclusive; 1 skipped.
```

The Markdown section:

```text
## Differential Fuzzing

Seeded inputs (swiftproof-fuzz/v1) were planned for 7 changed Go functions, 7 of them with recorded fuzz checks on the baseline and the candidate: 2 diverged, 2 not diverged, 3 inconclusive.

- **inconclusive** broken.Double (broken/broken.go:4): the candidate run failed without recording an observation stream \(for example, the package did not build; see the check log\).
- **not diverged** calc.Boundary (calc/calc.go:18): 64 of 64 inputs compared; the recorded encodings were equal for each compared input. Evidence evidence-2 (checks check-4, check-5, check-6, check-7).
- **diverged** calc.Percent (calc/calc.go:25): 20 of 64 compared inputs recorded different values on the baseline and the candidate; each revision repeated its own value in a second run. Smallest divergent input tried: Percent\(0, 0\); baseline int\(0\); candidate panic\(error\(&\#34;runtime error: integer divide by zero&\#34;\)\). Evidence evidence-3 (checks check-4, check-5, check-6, check-7); the values are listed under Behavior Divergences.
- **not diverged** calc.Join (calc/calc.go:30): 64 of 64 inputs compared; the recorded encodings were equal for each compared input. Evidence evidence-4 (checks check-4, check-5, check-6, check-7).
- **diverged** calc.Discount (calc/calc.go:45): 1 of 64 compared inputs recorded different values on the baseline and the candidate; each revision repeated its own value in a second run. Smallest divergent input tried: Discount\(Cents\(1000\)\); baseline calc.Cents\(900\); candidate calc.Cents\(1000\). Evidence evidence-5 (checks check-4, check-5, check-6, check-7); the values are listed under Behavior Divergences.
- **inconclusive** calc.Stamp (calc/calc.go:53): all 64 inputs gave different observations on repeated evaluation.
- **inconclusive** calc.Halt (calc/calc.go:58): the candidate process ended while evaluating input 5: Halt\(7\).

Not fuzzed (1):

- web/price.ts:1 price: TS/JS differential fuzzing is not implemented in this build
```

All four calc streams recorded `string("uid=65534 source-write-refused=true")` for every `Boundary` input: the harness ran as the unprivileged sandbox user on both revisions and could not write to the read-only source mount. The fuzz checks of the package that does not build and the calc candidate runs (which `Halt(7)` ended) are FAIL, never ERROR. Whether the `Percent` and `Discount` differences are intended is for a human to decide.
