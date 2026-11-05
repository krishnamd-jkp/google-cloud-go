# Go SDK vs Rust core: data-path benchmark

Run `20261104T102741Z`: 38 results, 2 rounds, 2026-11-04T10:27:41Z to 2026-11-04T13:34:47Z (3h7m0s).

| Environment | |
|---|---|
| Machine | `c4-standard-192` in `us-central1-b` (192 vCPUs, GOMAXPROCS 192), kernel 6.12.113+deb13-cloud-amd64 |
| Bucket / profile | `regional-bucket` / `worklog` (shuffle seed 1793788061563107642) |
| Zonal bucket (appendable uploads) | `zonal-bucket` |
| Build | go1.26.8, revision dca5660d337f64209421575ad9a51522dc61792d + local changes |
| gRPC path | CloudPath (DirectPath disabled for every client) |

## Summary: Rust core relative to the Go SDK (medians over rounds)

Throughput ratio > 1, and latency and CPU ratios < 1, favor the Rust core. `~` marks a throughput difference within the spread between rounds. VM CPU: difference of the VM's utilization (all vCPUs) in percentage points.

| case | comparison | throughput | p50 latency | p99 latency | CPU per op | peak memory (Rust − Go) | VM CPU (Rust − Go) |
|---|---|---|---|---|---|---|---|
| open | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 0.97x ~ | 1.03x | 1.05x | 0.79x | +5 MiB | +0.018 pp |
| read50x2x4 | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 1.02x ~ | 1.03x | 0.96x | 0.67x | +21 MiB | -0.014 pp |
| read50x2x4 | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 0.99x ~ | 1.02x | 1.02x | 0.70x | +25 MiB | -0.004 pp |
| read50x2x4-parallel | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 0.99x ~ | 0.95x | 0.82x | 1.05x | +599 MiB | +0.007 pp |
| read50x2x4-parallel | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 0.97x ~ | 1.00x | 1.02x | 0.94x | +598 MiB | -0.088 pp |
| random8k | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 1.03x | 0.98x | 0.98x | 1.05x | +22 MiB | +0.012 pp |
| random8k | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 0.98x ~ | 1.01x | 1.01x | 1.11x | +24 MiB | +0.011 pp |
| bidiread-16MiB-c48 | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 1.01x ~ | 0.98x | 1.01x | 0.79x | +809 MiB | -0.530 pp |
| bidiwrite-200MiB-c48 | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 0.49x | 1.00x | 1.66x | 0.94x | +15858 MiB | -2.705 pp |
| list-objects-100 | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 0.99x ~ | 1.03x | 0.99x | 2.00x | +18 MiB | +0.044 pp |
| list-buckets-100 | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 1.02x ~ | 1.00x | 1.04x | 1.22x | +9 MiB | +0.035 pp |

## Per case

### open

open a zero-length reader at a random offset of a 100 KiB object and close it; 1 workers, 240 s timed phase — NewRangeReader(random offset, 0) + Close: open a BidiReadObject stream and wait for the first response (with the object).

| backend | rounds ok | ops/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 14.5 (14.1–15.0) | 14.5 | 64.9 | 83.7 | 143.3 | 68.8 | – | 1.403 | – | 0.020 | 19.2 | 56.5 | – | 0 |
| rust-grpc | 2/2 | 14.1 (13.9–14.2) | 14.1 | 66.8 | 85.3 | 150.6 | 71.1 | – | 1.103 | – | 0.015 | 24.1 | 247.5 | 2.0 | 0 |


### read50x2x4

read 50 x 2 MiB at a 4 MiB stride of a 200 MiB object, one range reader per range, one after the other; 1 workers, 240 s timed phase; io.Copy — one NewRangeReader per range, one after the other.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 15.5 (15.3–15.8) | 0.2 | 5820.3 | 8585.6 | 13799.4 | 6435.9 | 112.7 | 339.225 | 3.47 | 0.053 | 28.4 | 55.5 | – | 0 |
| go-grpc-bidi | 2/2 | 16.0 (15.4–16.6) | 0.2 | 5844.2 | 7906.8 | 12987.5 | 6259.3 | 118.0 | 325.193 | 3.33 | 0.052 | 23.9 | 55.0 | – | 0 |
| rust-grpc | 2/2 | 15.9 (15.3–16.4) | 0.2 | 5967.2 | 6716.9 | 13263.3 | 6316.8 | 116.8 | 227.321 | 2.33 | 0.036 | 49.3 | 248.0 | 250.0 | 0 |


### read50x2x4-parallel

read 50 x 2 MiB at a 4 MiB stride of a 200 MiB object, one range reader per range, all at once; 1 workers, 240 s timed phase; io.Copy — all 50 range readers at once.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 199.8 (146.2–253.3) | 2.0 | 524.1 | 634.4 | 856.1 | 539.3 | 475.1 | 365.630 | 3.74 | 0.718 | 436.5 | 61.5 | – | 0 |
| go-grpc-bidi | 2/2 | 204.8 (166.2–243.5) | 2.0 | 498.7 | 567.0 | 690.7 | 506.2 | 417.5 | 409.354 | 4.19 | 0.827 | 437.9 | 59.0 | – | 0 |
| rust-grpc | 2/2 | 198.3 (177.2–219.4) | 2.0 | 499.6 | 589.4 | 701.3 | 510.1 | 420.9 | 385.381 | 3.95 | 0.761 | 1035.6 | 280.5 | 250.0 | 0 |


### random8k

read random 8 KiB ranges of 100 MiB objects; 1 workers, 240 s timed phase; io.Copy — one NewRangeReader per 8 KiB range.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 0.1 (0.1–0.1) | 14.5 | 65.2 | 80.1 | 140.5 | 69.1 | 65.0 | 0.961 | 125.96 | 0.014 | 18.8 | 55.5 | – | 0 |
| go-grpc-bidi | 2/2 | 0.1 (0.1–0.1) | 15.1 | 62.7 | 75.8 | 135.8 | 66.2 | 62.5 | 0.909 | 119.17 | 0.014 | 17.2 | 56.5 | – | 0 |
| rust-grpc | 2/2 | 0.1 (0.1–0.1) | 14.9 | 63.6 | 77.3 | 137.1 | 67.3 | 63.6 | 1.006 | 131.86 | 0.015 | 41.2 | 248.0 | 4.0 | 0 |


### bidiread-16MiB-c48

read 200 MiB objects in 16 MiB ranges, one range reader per range; 48 workers, 300 s timed phase; io.Copy; gRPC clients use 48 connections — each worker reads its own 200 MiB object in 16 MiB ranges (one BidiReadObject range reader each), front to back, again and again.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 1169.7 (1154.7–1184.6) | 73.1 | 659.0 | 718.6 | 830.5 | 656.0 | 129.1 | 50.397 | 3.23 | 3.682 | 368.5 | 80.0 | – | 0 |
| rust-grpc | 2/2 | 1180.8 (1177.7–1184.0) | 73.8 | 649.1 | 716.6 | 834.9 | 649.7 | 121.4 | 39.580 | 2.53 | 2.921 | 1177.2 | 305.5 | 19.0 | 0 |


### bidiwrite-200MiB-c48

appendable upload of 200 MiB objects to the zonal bucket (BidiWriteObject), finalized on Close; 48 workers, 300 s timed phase; 1 MiB Write calls; gRPC clients use 48 connections — appendable upload of a new 200 MiB object to the zonal bucket per op (BidiWriteObject), finalized on Close.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 3508.4 (3444.0–3572.8) | 17.5 | 1983.1 | 2456.2 | 5501.7 | 2086.2 | – | 336.051 | 1.72 | 5.895 | 2465.7 | 110.5 | – | 1920 |
| rust-grpc | 2/2 | 1735.9 (1703.9–1767.8) | 8.7 | 1983.4 | 3361.0 | 9112.2 | 2404.8 | – | 315.510 | 1.62 | 2.739 | 18323.8 | 293.0 | 211.4 | 466 |

- `go-grpc` first error: `write ffi-bench/write/bidiwrite-200MiB-c48/go-grpc/1793790308212933932/w26-00097: rpc error: code = ResourceExhausted desc = You have exhausted your project's Rapid storage class byte (rapid_zonal_bytes) quota in zone us-west1-a. This quota applies at a per-project, per-zone level to the total numbe…`
- `rust-grpc` first error: `close ffi-bench/write/bidiwrite-200MiB-c48/rust-grpc/1793792478954071835/w03-00078: gcs rust core: cannot serialize the request the transport reports an error: code: 'Some resource has been exhausted', message: "You have exhausted your project's Rapid storage class byte (rapid_zonal_bytes) quota in …`

### list-objects-100

list 100 objects (pages of 100); one page per op; 1 workers, 120 s timed phase — one ListObjects RPC returning 100 objects.

| backend | rounds ok | entries/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 1604.8 (1582.7–1626.9) | 16.0 | 59.0 | 67.8 | 134.5 | 62.3 | 59.0 | 1.522 | – | 0.024 | 9.6 | 56.5 | – | 0 |
| rust-grpc | 2/2 | 1582.7 (1564.9–1600.4) | 15.8 | 60.6 | 68.3 | 132.8 | 63.2 | 60.6 | 3.036 | – | 0.048 | 27.1 | 249.0 | 1.0 | 0 |


### list-buckets-100

list one page of 100 buckets of the project per op; 1 workers, 120 s timed phase — one ListBuckets RPC returning 100 buckets.

| backend | rounds ok | entries/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 110.4 (110.3–110.5) | 1.1 | 910.8 | 940.7 | 966.3 | 905.7 | 910.8 | 7.574 | – | 0.008 | 14.3 | 56.0 | – | 0 |
| rust-grpc | 2/2 | 112.1 (110.5–113.8) | 1.1 | 909.7 | 943.8 | 1003.1 | 891.9 | 909.7 | 9.275 | – | 0.010 | 23.7 | 248.5 | 1.0 | 0 |


## VM CPU utilization (compute.googleapis.com/instance/cpu/utilization)

Mean of the 1-minute samples that lie entirely inside a measured phase (median over rounds). The VM has 192 vCPUs: one fully busy vCPU is 0.52 %. Idle VM (samples before and after the run): 0.060 % (80 samples).

| case | backend | VM CPU % | VM CPU − idle, in vCPUs | process CPU cores | samples |
|---|---|---|---|---|---|
| open | go-grpc-bidi | 0.073 | 0.026 | 0.020 | 6 |
| open | rust-grpc | 0.091 | 0.060 | 0.015 | 6 |
| read50x2x4 | go-grpc | 0.116 | 0.108 | 0.053 | 6 |
| read50x2x4 | go-grpc-bidi | 0.106 | 0.089 | 0.052 | 6 |
| read50x2x4 | rust-grpc | 0.102 | 0.081 | 0.036 | 6 |
| read50x2x4-parallel | go-grpc | 0.657 | 1.146 | 0.718 | 6 |
| read50x2x4-parallel | go-grpc-bidi | 0.752 | 1.328 | 0.827 | 6 |
| read50x2x4-parallel | rust-grpc | 0.663 | 1.159 | 0.761 | 6 |
| random8k | go-grpc | 0.069 | 0.018 | 0.014 | 6 |
| random8k | go-grpc-bidi | 0.070 | 0.020 | 0.014 | 6 |
| random8k | rust-grpc | 0.081 | 0.041 | 0.015 | 6 |
| bidiread-16MiB-c48 | go-grpc-bidi | 3.056 | 5.754 | 3.682 | 8 |
| bidiread-16MiB-c48 | rust-grpc | 2.527 | 4.736 | 2.921 | 8 |
| bidiwrite-200MiB-c48 | go-grpc | 4.517 | 8.558 | 5.895 | 7 |
| bidiwrite-200MiB-c48 | rust-grpc | 1.812 | 3.365 | 2.739 | 16 |
| list-objects-100 | go-grpc | 0.078 | 0.035 | 0.024 | 2 |
| list-objects-100 | rust-grpc | 0.121 | 0.119 | 0.048 | 2 |
| list-buckets-100 | go-grpc | 0.060 | 0.000 | 0.008 | 2 |
| list-buckets-100 | rust-grpc | 0.094 | 0.067 | 0.010 | 2 |

## VM memory (agent.googleapis.com/memory/percent_used, state used)

Highest 1-minute sample inside a measured phase (median over rounds). The VM has 709 GiB: 1 GiB is 0.141 %. Idle VM (samples before and after the run): 0.696 % (79 samples).

| case | backend | VM memory used % (peak) | above idle, GiB | process peak RSS − baseline MiB | samples |
|---|---|---|---|---|---|
| open | go-grpc-bidi | 0.726 | 0.22 | 19.2 | 8 |
| open | rust-grpc | 0.715 | 0.14 | 24.1 | 8 |
| read50x2x4 | go-grpc | 0.719 | 0.17 | 28.4 | 8 |
| read50x2x4 | go-grpc-bidi | 0.729 | 0.24 | 23.9 | 8 |
| read50x2x4 | rust-grpc | 0.722 | 0.18 | 49.3 | 8 |
| read50x2x4-parallel | go-grpc | 0.820 | 0.88 | 436.5 | 8 |
| read50x2x4-parallel | go-grpc-bidi | 0.775 | 0.56 | 437.9 | 8 |
| read50x2x4-parallel | rust-grpc | 0.953 | 1.82 | 1035.6 | 8 |
| random8k | go-grpc | 0.714 | 0.13 | 18.8 | 8 |
| random8k | go-grpc-bidi | 0.748 | 0.37 | 17.2 | 8 |
| random8k | rust-grpc | 0.718 | 0.16 | 41.2 | 8 |
| bidiread-16MiB-c48 | go-grpc-bidi | 0.779 | 0.59 | 368.5 | 10 |
| bidiread-16MiB-c48 | rust-grpc | 0.994 | 2.11 | 1177.2 | 10 |
| bidiwrite-200MiB-c48 | go-grpc | 1.274 | 4.10 | 2465.7 | 9 |
| bidiwrite-200MiB-c48 | rust-grpc | 3.431 | 19.38 | 18323.8 | 18 |
| list-objects-100 | go-grpc | 0.753 | 0.41 | 9.6 | 4 |
| list-objects-100 | rust-grpc | 0.731 | 0.25 | 27.1 | 4 |
| list-buckets-100 | go-grpc | 0.729 | 0.24 | 14.3 | 4 |
| list-buckets-100 | rust-grpc | 0.744 | 0.34 | 23.7 | 4 |

## How to read this

- Each (case, backend) pair ran once per round in its own process, in a new random order every round; the
  tables show the median over rounds (min–max for throughput).
- Throughput: payload bytes (or list entries, or ops when nothing is transferred) of successful ops divided by
  the wall time of the measured phase (all workers). Latency: per op, from opening the reader/writer/iterator
  until the op completed (for strided cases: all ranges of the op).
- CPU: user + system time of the whole process (Go and Rust threads) during the measured phase, per op and per
  GiB moved; CPU cores is that time divided by the phase's wall time. Warmup ops are excluded.
- Memory: peak RSS (VmHWM) minus the RSS before the client was created (after the harness allocated its buffers).
- Backends use each client's defaults (chunk sizes, checksums; one gRPC connection unless the case sets a pool for
  both). Known transport differences: the Rust SDK's JSON transport is HTTP/1.1-only (compare with go-json-h1);
  Go's gRPC client reads with ReadObject unless WithGRPCBidiReads (compare rust-grpc with go-grpc-bidi);
  rust-grpc uploads to standard buckets use JSON (the Rust SDK's only gRPC upload is appendable objects).
