# Go SDK vs the Rust core: Open, Read50x2x4 and Random8K over CloudPath (2026-11-03)

The first VM run: the three read workloads of the storage shared benchmarks, with one worker. It
was made with `rustbench` (profile `ssb`, see [`../../README.md`](../../README.md)) and is kept as
history. The current results are in [`../../RESULTS.md`](../../RESULTS.md).

**Run** `20261103T194206Z`, 19:42–20:58 UTC: 16 phases (4 workloads × 2 clients × 2 rounds),
**0 failed operations**.

## Summary

- **Latency and throughput are the same.** With the same RPC, the same connection count and
  CloudPath, the POC matches the Go SDK in all four workloads: p50 within ±6%, throughput within
  the round-to-round spread.
- **Process CPU per operation is lower for the POC in three of four workloads:** Open −37%,
  Read50x2x4 −23%, Read50x2x4 with 50 parallel readers −10%. Random8K is +9%.
- **The POC uses more memory:** +8 to +20 MiB peak RSS with one stream, and +579 MiB with 50
  parallel readers (1,019 vs 440 MiB). That comes from glibc's allocator arenas across 192 Tokio
  worker threads: `MALLOC_ARENA_MAX=2` cut it from 785 to 258 MiB.
- **VM CPU** (`compute.googleapis.com/instance/cpu/utilization`) stays at the noise floor with one
  stream (one busy vCPU is 0.52% of the VM; idle was 0.064%). With 50 parallel readers: Go 0.557%,
  POC 0.503%. The VM memory metric was not collected: the VM had no Ops Agent.
- **This run found two POC problems, fixed before the measured run** (see Findings). One was the
  Rust SDK's default of one gRPC connection per vCPU, 192 here, which made the POC's Open 2.6×
  slower (234 vs 91 ms p50).

## Setup

| | |
|---|---|
| VM | c4-standard-192 (192 vCPU, 720 GiB), us-central1-f, Ubuntu 26.10 |
| Bucket | regional Standard bucket in us-west1: cross-region, CloudPath |
| Go SDK | `NewGRPCClient(WithGRPCBidiReads())`: BidiReadObject, the RPC the Rust SDK reads with. DirectPath disabled |
| POC | `NewGRPCClient(experimental.WithRustCore())`, the same Go API |
| Connections | 1 gRPC connection each: Go's default pool size, which the Rust core follows |
| Method | Every (workload, client) pair runs in a fresh process, in a new random order each round. 240 s timed phase after warm-up operations, 60 s gaps, 1 worker |
| Data | 8 × 100 KiB, 1 × 100 MiB, 1 × 200 MiB objects |
| Harness | `rustbench suite -profile ssb -backends go-grpc-bidi,rust-grpc -rounds 2 -cooldown 60s`, then `rustbench-report -vm-cpu` |

| Workload | One operation | Shared-benchmark counterpart |
|---|---|---|
| **Open** | `NewRangeReader(random offset, 0)` + `Close` on a 100 KiB object: opens a BidiReadObject stream and waits for the first response, which carries the object metadata | `openReaderStep` (non-Rapid buckets) |
| **Read50x2x4** | From the 200 MiB object, 50 × `NewRangeReader(i·4 MiB, 2 MiB)` + `io.Copy`, one after another. Latency covers all 50 | `read50x2x4WithoutMRDStep` |
| Read50x2x4-parallel | The same with all 50 readers at once, closer to a MultiRangeDownloader with 50 ranges | (extra) |
| **Random8K** | From the 100 MiB object, `NewRangeReader(random offset, 8 KiB)` + `io.Copy`. Latency per read | `downloadRandom8kStep` |

Both clients use range readers: the POC has no MultiRangeDownloader yet.

## Results

Medians over 2 rounds. RSS is peak RSS minus the process RSS before the client existed. CPU counts
every Go and Rust thread of the process.

| Workload | Client | ops/s or MiB/s | p50 ms | p90 ms | p99 ms | mean ms | CPU ms/op | peak RSS MiB | threads |
|---|---|---|---|---|---|---|---|---|---|
| **Open** | Go | 14.0 ops/s | 66.6 | 90.2 | 154.6 | 71.5 | 1.88 | 19.7 | 57 |
| | POC | 14.3 ops/s | 65.3 | 83.7 | 154.3 | 69.7 | **1.19** | 27.5 | 248 |
| **Random8K** | Go | 14.5 ops/s | 64.2 | 82.7 | 148.6 | 69.1 | 1.02 | 18.6 | 56 |
| | POC | 14.6 ops/s | 63.5 | 80.6 | 148.4 | 68.4 | 1.11 | 36.6 | 248 |
| **Read50x2x4** | Go | 15.4 MiB/s | 5963 | 6302 | 13816 | 6482 | 308 (3.16 s/GiB) | 26.8 | 57 |
| | POC | 15.5 MiB/s | 6077 | 6451 | 13473 | 6463 | **236 (2.42 s/GiB)** | 46.5 | 248 |
| **Read50x2x4-parallel** | Go | 148.4 MiB/s (129.8–166.9) | 677 | 781 | 868 | 685 | 389 (3.99 s/GiB) | 440 | 58 |
| | POC | 150.3 MiB/s (143.1–157.6) | 639 | 819 | 1030 | 667 | **351 (3.60 s/GiB)** | **1019** | 283 |

### VM CPU utilization (`compute.googleapis.com/instance/cpu/utilization`)

Mean of the 1-minute samples that lie entirely inside a timed phase (6 samples per client and
workload), median over rounds. Idle VM before and after the run: 0.064% (8 samples).

| Workload | Go | POC | Go, vCPUs above idle | POC, vCPUs above idle |
|---|---|---|---|---|
| Open | 0.084% | 0.070% | 0.04 | 0.01 |
| Random8K | 0.082% | 0.075% | 0.04 | 0.02 |
| Read50x2x4 | 0.104% | 0.104% | 0.08 | 0.08 |
| Read50x2x4-parallel | **0.557%** | **0.503%** | 0.95 | 0.84 |

On 192 vCPUs one busy core is 0.52% of the VM, so the single-stream rows are within noise of idle.
Only the parallel row is a usable VM-level signal; it agrees with the per-process CPU (0.57 vs 0.53
cores).

## Findings

1. **The Rust SDK opens one gRPC connection per vCPU by default.** `Storage::builder()` sets the
   subchannel count to `available_parallelism()`, 192 here. A 15 s smoke run before the fix
   ([`smoke-rust-sdk-default-connections.jsonl`](smoke-rust-sdk-default-connections.jsonl)):

   | | Go (1 connection) | POC, Rust SDK default (192 connections) | POC, 1 connection |
   |---|---|---|---|
   | TCP connects per 15 s Open phase | 3 | **195**, all at client start | 2 |
   | Open p50 | 91 ms | **234 ms** | 106 ms (65 ms in the full run) |
   | Open CPU/op | 1.64 ms | **4.65 ms** | 1.50 ms |
   | Read50x2x4 p50 | 6.3 s | **18.6 s** | 6.1 s |

   Each connection is rarely used, so it goes idle (the VM had `tcp_slow_start_after_idle=1`); that
   is the likely cause, not proven. **Fix:** the core's connection count follows Go's
   `option.WithGRPCConnectionPool` (default 1), which is also the right behavior for keeping the Go
   interface.
2. **Zero-length gRPC reads were not like-for-like.** The POC served `NewRangeReader(off, 0)` with
   one unary GetObject and dropped the offset, while the Go SDK opens a read stream. The POC now
   opens BidiReadObject at the offset and closes it after the first response.
3. **Memory with many parallel readers comes from glibc arenas.** The POC process has 192 Tokio
   workers (one per CPU) plus the blocking pool. A 60 s parallel run of the POC:

   | | peak RSS | MiB/s | p50 |
   |---|---|---|---|
   | default | 785 MiB | 182 | 534 ms |
   | `MALLOC_ARENA_MAX=2` | 258 MiB | 136 | 726 ms |

   A clear 3× memory reduction; the speed difference of single runs is inconclusive. Options: a
   different global allocator in the core (jemalloc or mimalloc), or fewer Tokio workers.
4. **CPU:** the POC's per-operation CPU is lower wherever bytes flow or streams open (−23% and −10%
   on the reads, −37% on Open). Random8K is +9%: 0.09 ms per 8 KiB read, roughly the cost of the
   Go–Rust handoff on a tiny transfer.

## Caveats

- No MultiRangeDownloader: Read50x2x4 with sequential range readers pays one stream open per range
  (about 50 × 120 ms), so it says nothing about MultiRangeDownloader performance.
- One worker, as in the shared benchmarks, so VM-level CPU sits at the noise floor except in the
  parallel case.
- Objects were reused, not uploaded fresh for each operation, for both clients.
- One VM, one time window, 2 rounds × 240 s. Cross-region variance is large: the Read50x2x4 p99 is
  about 13.5 s for both clients.

## Files

| File | What it is |
|---|---|
| [`results.jsonl`](results.jsonl) | one line per (workload, client, round) from `rustbench suite` |
| [`report.md`](report.md) | `rustbench-report -vm-cpu vm-cpu.json results.jsonl` |
| [`vm-cpu.json`](vm-cpu.json) | Cloud Monitoring `compute.googleapis.com/instance/cpu/utilization` of the VM during the run |
| [`suite.log`](suite.log) | the suite's log |
| [`smoke-rust-sdk-default-connections.jsonl`](smoke-rust-sdk-default-connections.jsonl) | the smoke run with the Rust SDK's default connection count (finding 1) |

Bucket and VM names are replaced with generic ones.
