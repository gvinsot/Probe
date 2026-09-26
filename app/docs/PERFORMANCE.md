# Performance measurements

Local microbenchmarks, 2026-09-19: Windows amd64, Go 1.27.1, AMD Ryzen 7 9800X3D, GOMAXPROCS 8. Measured with:

```sh
go test ./internal/linter ./internal/gitrepo ./internal/report -run '^$' -bench . -benchmem -benchtime=200ms
```

| Benchmark | Workload | Time/op | Allocated/op |
| --- | --- | --- | --- |
| GoDeclarations | Small Go declaration fixture | 9.77 µs | 8.1 KB |
| AnalyzePolyglot | 1,000 synthetic changed text files, in memory | 3.25 ms | 2.28 MB |
| ParsePatch | Synthetic unified patch | 236 µs (~419 MB/s) | 669 KB |
| FinalizeLargeDiff | 10,000 changed lines and 100 signals | 2.40 ms | 3.52 MB |

These are indicative single-machine measurements with short benchmark windows. They exclude Git repository I/O where not explicitly part of the fixture, Docker startup, project tests and provider latency. They do not measure review-time savings for people. Re-run on representative repositories before setting latency targets.

The implementation caps concurrent Go source readers at eight, indexes report lines by file, and limits diff/source/snapshot size. Report generation retains the parsed change in memory; very large PRs may hit explicit limits rather than produce incomplete clean results. Every container starts fresh, deliberately trading warm build-cache performance for independent observations.

Future end-to-end evaluation should measure small/large real PRs with and without checks/reviewer, memory peaks, useful findings, false positives, missed regressions and developer review duration. Any caching must key on immutable commits, policy and image identity without letting one untrusted run poison another.

## v0.4 stage costs

Each block below records measured costs of one v0.4 stage, and nothing that was not measured. The worst-case wall-clock formula for a whole review is in [CI integration](CI.md#runtime-bounds-deadline-and-report-size).

<!-- F2:begin -->
<!-- F2:end -->

<!-- F4:begin -->
<!-- F4:end -->

<!-- F6:begin -->
### Impact analysis (F6a static index)

Measured on 2026-09-26 in `golang:1.26-bookworm` (Go 1.26.8, linux/amd64) on the Windows host of the earlier measurements, in a Docker Desktop VM that exposed 2 CPUs to the container while other builds ran on the host, so times varied by up to 2x between runs; the range over the runs is given (three runs of the first two workloads, two of the third). Each run includes the Git reads (`ls-tree` of both commits and one `cat-file --batch` stream), parsing, type-checking, and the impact of the change. Heap is the heap in use after a garbage collection with the index still referenced, as it is during a review.

| Workload | Indexed Go files | Time per analysis | Allocated per analysis | Heap in use |
| --- | --- | --- | --- | --- |
| `BenchmarkIndexSynthetic`: 40 packages of 25 files, each file 10 functions calling into the previous package, one changed function | 1,000 | 286–418 ms | 62 MB | not measured |
| The SwiftProof repository (`app` and `hub` modules at `8d114b6`), `HEAD~1..HEAD` (15 changed files) | 136 | 274–541 ms | 44 MB | 12.4–13.6 MiB |
| `GOROOT/src` of Go 1.26.8 as a Git repository (5,609 Go files outside `testdata` and `vendor`), one changed function | 4,072 (the others excluded by the linux/amd64 build constraints, and one file over 2 MiB skipped, so `limited`) | 6.6–8.4 s | 1.9 GB | 429–431 MiB |

The standard library is a stress case, not a typical repository: its module path `std` is not a prefix of its import paths, so every import is treated as outside the repository, and one skipped 2 MiB generated file leaves its large package with many unresolved names. Before imported packages were marked so that `go/types` skips building error messages for missing names, the same run took 20–39 s (three runs), spent mostly sorting that package's names once per unresolved reference; the 120 s limit, also checked on every type error, bounds such cases.

The impact searches were measured on an adversarial shape, `BenchmarkImpactSearchAdversarial` (same day and setup, three runs of three iterations, re-measured after the implementation-check bound below was added): 1,000 types whose `Do` method calls every changed function, one interface `I{ Do() }` called 200,000 times, and one test reaching a caller (33 indexed files). Every changed function then reaches the 200,000 interface calls through 1,000 implementing methods.

| Changed functions | Time per analysis | Allocated per analysis | `impact.status` |
| --- | --- | --- | --- |
| 1 | 233–334 ms | 204–206 MB | `indexed` |
| 10 | 254–394 ms | 218 MB | `indexed` |
| 200 | 0.60–1.12 s | 435 MB | `limited`: the reaching-test search reached its 10,000,000-visit budget |

Each search follows an interface method's references once, and every visit is charged to the search budget. Before that, a review run of a similar shape (1,000 calls per caller function) with a 2 s time limit, which the searches did not check then, took 6.9 s with one changed function and 76 s with ten, and stayed `indexed`.

One implementation check can cost far more than a visit. `go/types` compares each embedded type of one embedding level with every distinct type kept so far, so a lookup on a type that embeds a wide struct costs time quadratic in the struct's width. `BenchmarkImplementsWideEmbedding` (same day and setup, three runs) checks `T` and `*T` against a two-method interface that `T` does not implement. It measured 0.74–0.92 ms per check at width 100, 5.5–5.6 ms at 300 and 44–61 ms at 1,000, or 5.5 to 11 ns per estimated step. A check estimated above 8,388,608 steps is not made. `BenchmarkImpactSearchWideEmbedding` (three runs of three iterations, with a 3 s time limit) uses 1,000 types whose `Do` method calls the changed function and embeds a struct of 1,000 or 3,000 empty types, 200 interfaces `I{ Do() int; Xk() }`, and one test reaching a caller:

| Embedded types | Time per analysis | Allocated per analysis | `impact.status` |
| --- | --- | --- | --- |
| 1,000 | 3.009–3.028 s | 158–240 MB | `limited`: each check is estimated within the bound, and the reaching-test search stopped at the 3 s time limit |
| 3,000 | 138–485 ms | 177 MB | `limited`: no check is made, since each is estimated above the bound |

Before the bound, and before the time limit was checked ahead of every implementation check, the searches noticed the limit only every 1,024 work units, about 340 checks. On that code, the test `TestImplementationChecksStopAtTimeLimitsAndCancel` (900 embedded types, 50 types) took 15.1 s against a 1 s search deadline (one run). The review of that code measured 1 min 44 s with a 3 s limit for 3,000 embedded types and 50 types. The type check is not bounded this way: a package without type errors that uses selectors on such a type is checked to its end (see [impact analysis](IMPACT.md#trust-and-security)).

Reproduce with:

```sh
go test ./internal/symbols -run '^$' -bench IndexSynthetic -benchmem
SWIFTPROOF_BENCH_REPO=/path/to/git/repo go test ./internal/symbols -run '^$' -bench AnalyzeRepository -benchtime 3x -benchmem
go test ./internal/symbols -run '^$' -bench ImpactSearchAdversarial -benchtime 3x -benchmem
go test ./internal/symbols -run '^$' -bench ImplementsWideEmbedding -count 3
go test ./internal/symbols -run '^$' -bench ImpactSearchWideEmbedding -benchtime 3x -benchmem
```

### Impacted tests (F6b, `--impacted-tests`)

The stage costs sandbox runs, not host work: two runs per package with selected tests, plus the narrowed baseline run, the retry pair and the live confirmation described in [impacted tests](IMPACT.md#impacted-tests---impacted-tests), all inside its 180 s sub-cap of the shared budget. Measured on 2026-09-26 with a Windows binary built from the branch, `golang:1.26-bookworm`, on the same host while other agents ran containers, on the shop fixture of [validation](VALIDATION.md) (one package of two selected tests, one package of one):

| Run | Stage runs executed | Their recorded durations | Budget spent by the whole review |
| --- | --- | --- | --- |
| Default Go policy, TestTotal and TestCheckout fail on the candidate, TestTotalZero gets a pair of its own | 6 | 72.3 s in total, 11.4–13.5 s each | 110.8 s |
| The same with a test that also fails on the baseline (narrowed baseline run) | 7 | 107.1 s, 11.6–17.8 s each | 126.7 s |
| Policy with only `test` and `generated_test`, `--cache-dir`, first and second run | 6 each | 37.4 s and 37.2 s, 5.9–7.0 s each | 44.0 s and 43.1 s |
| The same, third run: 3 baseline runs replayed, 2 live confirmations of `FAILS_ON_CANDIDATE` | 5 | 67.1 s, 11.1–17.5 s each | 77.5 s |

Each run compiles its package in a fresh container, so its duration is mostly compilation. The third cached run executed one run fewer, yet took longer than the first two because the host load differed: these numbers show no speed-up from the cache, and a `FAILS_ON_CANDIDATE` result always costs a live baseline run.
<!-- F6:end -->

<!-- F7:begin -->
### Execution cache

Measured on 2026-09-26 on a Windows 11 Pro host (AMD Ryzen 7 9800X3D) with Docker Engine 28.4.0 and `golang:1.26-bookworm`, while other jobs shared the Docker daemon.

- **Keying cost per eligible baseline run.** `go test ./internal/harness -run '^$' -bench BenchmarkCacheKey -benchmem -benchtime=2s` walks and hashes a baseline tree of 1,000 files of 4 KiB in 50 directories plus one staged test on every key: 8.5 ms/op and 1.0 MB allocated per op in the `golang:1.26-bookworm` container (2 CPUs), 36.7 to 37.9 ms/op and 1.5 MB natively on Windows (three runs). The cost grows with the size of the baseline tree, up to the snapshot limits (100,000 files, 512 MiB). The manifest of the pristine snapshot is computed once per review.
- **Reviews with the scripted clamp fixture** (`--checks=false`, one generated experiment per review; see [validation](VALIDATION.md)): a live `generated_test_base` run recorded 16.7 to 25.2 s, a replayed one 0 ms, and `execution.budget.spent_ms` was 28,014 ms for the review whose baseline was replayed (only the candidate ran) against 36,694 ms for the preceding live one. Review wall clocks were 37.2 s (live baseline), 28.4 s (replayed baseline, `NOT_REPRODUCED`), 52.0 s (the same review after its entry was rejected) and 50.8 s (no cache). A review that records `REPRODUCED` on a replayed baseline runs the baseline again live, so it runs as many containers as without a cache (49.5 s here).
- Single runs of the same work varied more than a replay saved on this shared host, so these numbers do not establish a speed-up. How much a cache saves depends on how often baseline experiments repeat across your reviews; measure it on them.
<!-- F7:end -->

<!-- F8:begin -->
### Dependency preparation

Measured on 2026-09-26 on the Windows 11 host with Docker Desktop (Docker Engine 28.4.0, containerd image store) while other builds shared the machine, with Windows binaries built from the F8 branch (commit `8f05f99`, and the working tree shortly before it, whose later edits did not change the behavior these runs depend on), `golang:1.26-bookworm` and a Go module requiring `github.com/google/go-cmp v0.7.0`; policy `prepare` = `go mod download` on `go.mod` and `go.sum` with network permitted. The figure is the `prepare.duration_ms` of the report.

| Case | Runs | `prepare.duration_ms` | Committed layer |
| --- | --- | --- | --- |
| Cold: no image for the key, `go mod download` with network, commit, read-back | 5 | 3,535 to 5,241 ms | 897,024 bytes |
| Warm: image reused, no container started | 6 | 450 to 1,473 ms | — |

After the review fixes, which read the whole `docker diff` listing, a binary built from `dd96a1f` on the same host gave one cold build of 3,982 ms (the same 897,024-byte layer) and warm reuses of 549 and 973 ms, inside the ranges above.

A cold build pays the download and the commit once per key and base commit; a new base commit builds again. Preparation is not charged to `sandbox.max_runtime_seconds`. These numbers depend on the registry, the network and the size of the dependencies; they are not a general estimate.
<!-- F8:end -->
