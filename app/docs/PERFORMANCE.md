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

The impact searches were measured on an adversarial shape, `BenchmarkImpactSearchAdversarial` (same day and setup, three runs of three iterations): 1,000 types whose `Do` method calls every changed function, one interface `I{ Do() }` called 200,000 times, and one test reaching a caller (33 indexed files). Every changed function then reaches the 200,000 interface calls through 1,000 implementing methods.

| Changed functions | Time per analysis | Allocated per analysis | `impact.status` |
| --- | --- | --- | --- |
| 1 | 388–528 ms | 205 MB | `indexed` |
| 10 | 624–647 ms | 218 MB | `indexed` |
| 200 | 0.80–1.44 s | 435 MB | `limited`: the reaching-test search reached its 10,000,000-visit budget |

Each search follows an interface method's references once, and every visit is charged to the search budget. Before that, a review run of a similar shape (1,000 calls per caller function) with a 2 s time limit, which the searches did not check then, took 6.9 s with one changed function and 76 s with ten, and stayed `indexed`.

Reproduce with:

```sh
go test ./internal/symbols -run '^$' -bench IndexSynthetic -benchmem
SWIFTPROOF_BENCH_REPO=/path/to/git/repo go test ./internal/symbols -run '^$' -bench AnalyzeRepository -benchtime 3x -benchmem
go test ./internal/symbols -run '^$' -bench ImpactSearchAdversarial -benchtime 3x -benchmem
```
<!-- F6:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F8:begin -->
<!-- F8:end -->
