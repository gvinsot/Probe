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
### Differential fuzzing (F2)

Measured on 2026-09-26 on the Windows 11 host (AMD Ryzen 7 9800X3D, 8 CPUs shared with other jobs), Docker Engine 28.4.0, `golang:1.26-bookworm`:

| Workload | Measured |
| --- | --- |
| Host-side selection and rendering of the five-function calc fixture (320 inputs), GOMAXPROCS 2 | 9.3 ms/op on a quiet host; 22 to 24 ms/op on the loaded host; 2.9 MB allocated/op |
| Validating and normalizing the largest stream one package run plans (16 functions, 1,024 records), GOMAXPROCS 2 | 49 ms/op on a quiet host; 81 to 96 ms/op on the loaded host; 8.5 MB allocated/op. Most of it is redaction: of each display, of its JSON-escaped form, and of the whole normalized stream |
| One fuzz container of the calc fixture, with the sandbox isolation flags and an empty build cache | 20.7 s to 29.8 s over two runs, mostly compiling the package tests and the standard library into the fresh `GOCACHE` |
| The calc fixture end to end: a first pair and one confirmation pair, four containers run one after another | 94 s and 106 s |

A package costs two containers, plus two more when its first pair shows a difference. Every container starts with an empty build cache, so each one compiles the package's tests again. The host-side numbers come from `go test ./internal/fuzz -run '^$' -bench . -benchtime 20x -benchmem` in a container limited to 2 CPUs; the container numbers come from `TestDockerFuzzCalcFixture`. The quiet-host numbers were measured before the fixes of the F2a review; the loaded-host numbers after them, with other jobs sharing the CPUs. In that loaded session the code before the fixes measured 19 to 32 ms/op and 81 to 98 ms/op, so the fixes changed the time within the noise. They raised allocation from 1.7 MB and 6.7 MB per operation: selection now builds each target's corpus to size it, and every call and display is also checked in its JSON-escaped form.
<!-- F2:end -->

<!-- F4:begin -->
<!-- F4:end -->

<!-- F6:begin -->
<!-- F6:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F8:begin -->
<!-- F8:end -->
