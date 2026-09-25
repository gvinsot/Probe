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
<!-- F6:end -->

<!-- F7:begin -->
<!-- F7:end -->

<!-- F8:begin -->
### Dependency preparation

Measured on 2026-09-26 on the Windows 11 host with Docker Desktop (Docker Engine 28.4.0, containerd image store) while other builds shared the machine, with a Windows binary built from the F8 branch, `golang:1.26-bookworm` and a Go module requiring `github.com/google/go-cmp v0.7.0`; policy `prepare` = `go mod download` on `go.mod` and `go.sum` with network permitted. The figure is the `prepare.duration_ms` of the report.

| Case | Runs | `prepare.duration_ms` | Committed layer |
| --- | --- | --- | --- |
| Cold: no image for the key, `go mod download` with network, commit, read-back | 4 | 3,535 to 5,241 ms | 897,024 bytes |
| Warm: image reused, no container started | 4 | 759 to 1,473 ms | — |

A cold build pays the download and the commit once per key and base commit; a new base commit builds again. Preparation is not charged to `sandbox.max_runtime_seconds`. These numbers depend on the registry, the network and the size of the dependencies; they are not a general estimate.
<!-- F8:end -->
