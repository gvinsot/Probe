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
### Mutation of added lines

Measured on 2026-09-26 on the Windows 11 host with Docker Desktop (Docker Engine 28.4.0), `golang:1.26-bookworm` (go1.26.8), sandbox limits of 1,024 MiB and 2 CPUs, while other agents' builds and tests shared the 8-CPU machine; Windows binary built from the F4 branch (commit `f395cb2`). Fixture: one stdlib-only package with two test functions; policy command `go test -json -count=1 -failfast {package}`. The figures are the `duration_ms` of the recorded mutation checks.

| Run | Reviews | Control run | One mutant run | Mutation stage (sum of runs) |
| --- | --- | --- | --- | --- |
| 8 mutants, 1 package | 2 | 6,170 and 15,324 ms | 6,036 to 11,708 ms | 70.4 s and 80.0 s |
| 3 mutants (`max_mutants: 3`) | 1 | 6,137 ms | 9,437 to 13,119 ms | 41.2 s |
| 7 mutants, 1 package (the fixture variant with an inline `errors.New`), host under heavier load | 1 | 26,331 ms | 16,456 to 30,130 ms | 197.6 s |

Every control and mutant run is a fresh container with an empty build cache, so one run costs about one package compile plus its tests, and the stage costs about (packages with selected mutants) control runs plus `max_mutants` mutant runs, one at a time. The whole reviews of the first row took 92 s and 110 s wall clock, of which the initial checks and coverage took about 21 s and 29 s. Load on the host roughly tripled the per-run time in the last row. These numbers come from a tiny package; packages with more dependencies compile for longer. They are not a general estimate.
<!-- F4:end -->

<!-- F6:begin -->
<!-- F6:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F8:begin -->
<!-- F8:end -->
