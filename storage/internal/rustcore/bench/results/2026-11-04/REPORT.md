# Go SDK vs the Rust core: the SDK consolidation benchmark plan with 48 connections (2026-11-04)

The second VM run: workloads 1–6 of the SDK consolidation benchmark plan, made with `rustbench` (profile `worklog`, see [`../../README.md`](../../README.md)). The 48-worker workloads use 48 gRPC connections on both clients. It is kept as history: the current results, with each engine's default settings and in the same format as the Java PoC's report, are in [`../../RESULTS.md`](../../RESULTS.md).

## Summary

| Area | Result for the POC, compared with the Go SDK |
|---|---|
| **Latency** | Same on every read and listing workload: p50 within 3%, p99 within about 5%. BidiWrite: p50 the same, p99 noisy for both clients. |
| **Throughput** | Same on every workload, inside the round-to-round spread. 48-thread BidiWrite: 4,378 vs 4,500 MiB/s. |
| **CPU** | Less where data moves: **−22%** on 48-thread BidiRead, **−30%** on Read50x2x4, **−21%** on Open, −3% on BidiWrite. More on small calls: Random8K +11% (+0.1 ms per read), ListBuckets +22%, ListObjects **2×** (+1.5 ms per call). |
| **Memory** | Higher everywhere. +5 to +25 MiB on one stream, **+0.6 GiB** with 50 parallel readers and **+0.8 GiB** with 48 BidiRead threads. With 48 BidiWrite threads it is **18.1 GiB vs 2.4 GiB**; the cause is in the Rust SDK ([Findings](#findings)). |
| **NIC** | Never saturated. With 48 threads both clients reach the same rates: about 10 Gb/s reading and 37 Gb/s writing, out of 100 Gb/s. The limit is outside the clients: each used fewer than 10 of the VM's 192 vCPUs. |

## Setup

| | |
|---|---|
| Clients | **Go SDK:** `storage.NewGRPCClient`, plus `WithGRPCBidiReads()` for BidiReadObject reads. **POC:** the same Go API, `storage.NewGRPCClient(experimental.WithRustCore())`; every call is served by the Rust SDK (`google-cloud-storage` 1.20) through cgo. Both run in the same Go binary. |
| Path | gRPC over **CloudPath**: the VM and the buckets are in different regions, and DirectPath is disabled for Go because the Rust SDK has none. |
| VM | c4-standard-192 (192 vCPUs, 708 GiB), Debian 13, us-central1-b, gVNIC, default network tier (100 Gb/s). The Ops Agent provides the memory metric. |
| Buckets | Reads and listing: a Standard regional bucket in us-west1 (no HNS). BidiWrite: a Rapid zonal bucket in us-west1-a. |
| Versions | Go 1.26.8; `cloud.google.com/go/storage` at upstream `867fe707ef` plus the POC; gRPC-Go 1.84.0; Rust SDK 1.20.0. |
| Runs | 2 rounds. Each (workload, client) pair runs once per round, in its own process, in a new random order, with a 60 s pause between pairs. |

**How the numbers are measured.** Every table shows the median of the 2 rounds; ranges in parentheses are the two rounds.
- **Latency:** time per operation, measured around the Go API call.
- **Throughput:** bytes, or operations, completed divided by the length of the phase.
- **Process CPU:** user plus system CPU time of the whole process (Go and Rust threads), per operation or per GiB.
- **VM CPU:** `compute.googleapis.com/instance/cpu/utilization`, the mean of the 1-minute samples inside the phase.
- **Memory:** peak RSS of the process minus its RSS before the client was created. The VM metric `agent.googleapis.com/memory/percent_used` (state `used`) is the highest sample in the phase.

## Results

### Workloads 1 to 3: Open, Read50x2x4 and Random8K (1 thread, 240 s)

These are the three Rapid workloads of the storage shared benchmarks. Both clients use **one range reader per range** because the POC has no MultiRangeDownloader yet. "Go (non-bidi)" is the baseline those workloads ask for: Go's default gRPC reads, which use ReadObject. "Go" without a label means BidiReadObject, the same RPC as the POC.

| Workload | Client | Throughput | p50 | p90 | p99 | CPU per op | Peak memory |
|---|---|---|---|---|---|---|---|
| **Open**: open a read stream and wait for the first response | Go | 14.5 ops/s | 64.9 ms | 83.7 ms | 143 ms | 1.40 ms | 19 MiB |
| | **POC** | 14.1 ops/s | 66.8 ms | 85.3 ms | 151 ms | **1.10 ms** | 24 MiB |
| **Read50x2x4**: 50 × 2 MiB at a 4 MiB stride, one after another; latency covers all 50 | Go (non-bidi) | 15.5 MiB/s | 5.82 s | 8.59 s | 13.8 s | 339 ms | 28 MiB |
| | Go | 16.0 MiB/s | 5.84 s | 7.91 s | 13.0 s | 325 ms | 24 MiB |
| | **POC** | 15.9 MiB/s | 5.97 s | 6.72 s | 13.3 s | **227 ms** | 49 MiB |
| **Read50x2x4, all at once**: the same 50 ranges in parallel | Go (non-bidi) | 200 MiB/s (146–253) | 524 ms | 634 ms | 856 ms | 366 ms | 437 MiB |
| | Go | 205 MiB/s (166–244) | 499 ms | 567 ms | 691 ms | 409 ms | 438 MiB |
| | **POC** | 198 MiB/s (177–219) | 500 ms | 589 ms | 701 ms | 385 ms | **1,036 MiB** |
| **Random8K**: one random 8 KiB range of a 100 MiB object | Go (non-bidi) | 14.5 ops/s | 65.2 ms | 80.1 ms | 141 ms | 0.96 ms | 19 MiB |
| | Go | 15.1 ops/s | 62.7 ms | 75.8 ms | 136 ms | 0.91 ms | 17 MiB |
| | **POC** | 14.9 ops/s | 63.6 ms | 77.3 ms | 137 ms | 1.01 ms | 41 MiB |

### Workload 4: BidiRead of 16 MiB ranges from 200 MiB objects (48 threads, 5 min)

Each thread reads its own 200 MiB object from start to end in 16 MiB ranges, then starts over. Each range is one BidiReadObject range reader. Both clients use 48 gRPC connections.

| Client | Throughput | p50 (one 16 MiB range) | p90 | p99 | CPU per GiB | Process CPU | VM CPU (busy vCPUs) | Peak memory |
|---|---|---|---|---|---|---|---|---|
| Go | 1,170 MiB/s (9.8 Gb/s) | 659 ms | 719 ms | 831 ms | 3.23 s | 3.7 cores | 3.06% (5.8) | 0.36 GiB |
| **POC** | 1,181 MiB/s (9.9 Gb/s) | 649 ms | 717 ms | 835 ms | **2.53 s** | **2.9 cores** | **2.53% (4.7)** | **1.15 GiB** |

### Workload 5: BidiWrite of 200 MiB objects (48 threads, 5 min)

Each operation is one appendable upload (BidiWriteObject) of 200 MiB to the zonal bucket, written in 1 MiB `Write` calls and finalized on `Close`. Each thread overwrites its own object. Both clients use 48 gRPC connections.

| Client | Throughput | p50 (one 200 MiB upload) | p90 | p99 | CPU per GiB | Process CPU | VM CPU (busy vCPUs) | Peak memory | Failed uploads |
|---|---|---|---|---|---|---|---|---|---|
| Go | 4,500 MiB/s (4,382–4,618), 37.7 Gb/s | 1.90 s | 2.50 s | 8.22 s | 1.62 s | 7.1 cores | 4.95% (9.4) | 2.4 GiB | 2 of 13,687 |
| **POC** | 4,378 MiB/s (4,196–4,560), 36.7 Gb/s | 1.89 s | 3.21 s | 6.61 s | 1.57 s | 6.7 cores | 4.63% (8.8) | **18.1 GiB** | 1 of 13,261 |

All three failures were transient `Internal` errors from the service, one per round at most, in both clients. Both clients also have rare uploads that take tens of seconds (the slowest: 22 to 44 s for Go and 10 to 59 s for the POC), which makes p99 noisy.

### Workload 6: ListObjects and ListBuckets returning 100 items (1 thread, 120 s)

Each operation is one RPC returning 100 entries.

| Call | Client | Calls/s | p50 | p90 | p99 | CPU per call | Peak memory |
|---|---|---|---|---|---|---|---|
| **ListObjects** (100 objects) | Go | 16.0 | 59.0 ms | 67.8 ms | 135 ms | 1.52 ms | 10 MiB |
| | **POC** | 15.8 | 60.6 ms | 68.3 ms | 133 ms | **3.04 ms** | 27 MiB |
| **ListBuckets** (100 buckets) | Go | 1.1 | 911 ms | 941 ms | 966 ms | 7.6 ms | 14 MiB |
| | **POC** | 1.1 | 910 ms | 944 ms | 1,003 ms | 9.3 ms | 24 MiB |

### VM metrics

| Workload | VM CPU, Go | VM CPU, POC | VM memory peak, Go | VM memory peak, POC |
|---|---|---|---|---|
| Idle VM (no benchmark) | 0.060% | | 0.70% | |
| Open | 0.073% | 0.091% | 0.73% | 0.72% |
| Read50x2x4 | 0.106% | 0.102% | 0.73% | 0.72% |
| Read50x2x4, all at once | 0.752% | 0.663% | 0.78% | 0.95% |
| Random8K | 0.070% | 0.081% | 0.75% | 0.72% |
| BidiRead, 48 threads | 3.06% | 2.53% | 0.78% | 0.99% |
| BidiWrite, 48 threads | 4.95% | 4.63% | 1.23% (+3.8 GiB) | **3.42% (+19.3 GiB)** |
| ListObjects | 0.078% | 0.121% | 0.75% | 0.73% |
| ListBuckets | 0.060% | 0.094% | 0.73% | 0.74% |

For the read workloads, Go here means BidiReadObject reads, as in the tables above. VM memory also includes kernel memory such as TCP socket buffers, which is why it is higher than process memory for Go's BidiWrite.

> [!NOTE]
> On this VM one busy vCPU is 0.52% CPU, and 1 GiB is 0.14% memory. The single-thread workloads therefore stay within noise of an idle VM. Compare them with the per-process CPU and peak-memory columns above, which measure the client alone.

## Findings

1. **BidiWrite memory: the Rust SDK queues up to 200 MiB per upload.**
   - In SDK 1.20, each BidiWriteObject stream holds up to 100 messages of up to 2 MiB between the SDK and its gRPC transport before it slows the writer down (`MAX_QUEUED_REQUESTS = 100` in `bidi_write/connector.rs`).
   - Uploading from memory is faster than the network, so every upload fills that queue. 48 uploads × 200 MiB is about 9.4 GiB.
   - Confirmed in a 60 s test of the POC: peak memory was 14.0 GiB with the default allocator settings, and still 9.4 GiB with `MALLOC_ARENA_MAX=2`, about 196 MiB per upload.
   - The rest, which grows to about 9 GiB over a 5-minute run, is memory that glibc's allocator kept instead of returning to the system (finding 2). `MALLOC_ARENA_MAX=2` is not a fix: it cost 78% more CPU per GiB.
   - Fixes: a smaller or configurable queue in the Rust SDK, or a flush every few MiB in the POC; plus the allocator change in finding 2.
2. **Many parallel streams cost the POC 0.6 to 0.8 GiB more memory.** This shows in Read50x2x4 all at once and in BidiRead with 48 threads. The cause is glibc's allocator: the Rust runtime's 192 worker threads each get their own memory pool, and freed memory stays in those pools. An earlier test with `MALLOC_ARENA_MAX=2` reduced it about 3×. Candidate fixes: jemalloc or mimalloc in the Rust core, or fewer runtime threads.
3. **Small calls cost a little more CPU.** Requests and responses cross the Go–Rust boundary as JSON. For one 100-object list page that adds about 1.5 ms per call (measured separately), which matches the ListObjects difference. Sending binary protobuf across the boundary would remove most of it. Latency doesn't change.
4. **Where data moves, the POC uses less CPU.** This holds for BidiRead (−22% per GiB), Read50x2x4 (−30% per op) and Open (−21%): the Rust SDK's gRPC data path costs less per byte than Go's.
5. **The NIC was never the limit.** Both clients reach the same rates, at most 37 Gb/s. A 200 Gb/s NIC (Tier_1 networking, as the plan lists) would not change these results.

## Methodology: the previous run compared with the plan

| Plan item | [Previous run (2026-11-03)](../2026-11-03/REPORT.md) | This run |
|---|---|---|
| Workloads 1–3: Open, Read50x2x4, Random8K | Yes | Yes |
| Baseline with Go's non-bidi reads, for Read50x2x4 and Random8K (asked for by the shared benchmarks' design) | **Missing** | Added (Go, non-bidi) |
| 4) BidiRead, 16 MB chunks of 200 MB, 48 threads, 5 min | **Missing** | Added |
| 5) BidiWrite, 200 MB objects, 48 threads, 5 min | **Missing** | Added: appendable uploads to a zonal bucket (see deviations) |
| 6) ListBuckets and ListObjects, 100 items | **Missing** | Added |
| Latency p50/p90/p99, throughput | Yes | Yes |
| CPU: `compute.googleapis.com/instance/cpu/utilization` | Yes | Yes |
| Memory: `agent.googleapis.com/memory/percent_used` | **Missing**: no Ops Agent (no package for Ubuntu 26.10) | Added: the VM was recreated on Debian 13 with the Ops Agent |
| c4-standard-192 in us-central1, bucket in us-west1, Standard and non-HNS, CloudPath | Yes | Yes |
| 200 Gb/s NIC | No (default tier, 100 Gb/s) | No: not a bottleneck (finding 5) |

**Deviations from the plan in this run:**
- **BidiWrite runs as appendable uploads to a Rapid zonal bucket.** The Rust SDK's only gRPC upload is appendable objects, which standard buckets don't support. The Go SDK runs the same appendable uploads, for a like-for-like comparison.
- **48-thread workloads use 48 gRPC connections in both clients.** The default is 1, which caps throughput far below the NIC.
- **No MultiRangeDownloader.** Open, Read50x2x4, Random8K and BidiRead use one range reader per range in both clients, because the POC doesn't have the MRD yet.
- **16 MiB ranges.** "16 MB chunks of 200 MB" is read as 16 MiB ranges of 200 MiB objects. Each object yields 12 full ranges, and the 8 MiB tail is skipped so every operation moves exactly 16 MiB.
- **Each BidiWrite thread overwrites its own object.** The first attempt created a new object per upload and filled the project's Rapid storage quota for the zone (roughly 1 TiB) within one 5-minute phase, so those runs were discarded. The BidiWrite numbers above come from a separate 2-round rerun.

## Caveats

- **Run-to-run noise.** Both rounds go cross-region over the public CloudPath, and some workloads vary between rounds by up to ±25%: Read50x2x4 all at once ranged from 146 to 253 MiB/s for Go. Treat differences inside the round ranges as ties.
- **Scope.** These numbers are for this POC build: Rust SDK 1.20 and Go–Rust interface version 5. DirectPath was not tested, because the Rust SDK has no DirectPath.

## Files

| File | What it is |
|---|---|
| [`results.jsonl`](results.jsonl) | the main run (`20261104T102741Z`): one line per (workload, client, round) from `rustbench suite` |
| [`report.md`](report.md) | `rustbench-report -vm-cpu vm-cpu.json -vm-mem vm-mem.json results.jsonl`. Its BidiWrite rows are the discarded quota-limited runs |
| [`vm-cpu.json`](vm-cpu.json), [`vm-mem.json`](vm-mem.json) | Cloud Monitoring CPU utilization and memory (`agent.googleapis.com/memory/percent_used`) of the VM during the main run |
| [`suite.log`](suite.log) | the main run's log |
| [`bidiwrite-rerun/`](bidiwrite-rerun/) | the BidiWrite rerun (`20261104T161035Z`): results, report, VM metrics and log. The BidiWrite numbers above come from here |
| [`bidiwrite-memory-check/`](bidiwrite-memory-check/) | the 60 s POC BidiWrite runs with glibc's defaults and with `MALLOC_ARENA_MAX=2` (finding 1) |

Bucket, project and VM names are replaced with generic ones.
