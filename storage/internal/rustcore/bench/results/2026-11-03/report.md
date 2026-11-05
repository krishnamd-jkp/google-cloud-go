# Go SDK vs Rust core: data-path benchmark

Run `20261103T194206Z`: 16 results, 2 rounds, 2026-11-03T19:42:06Z to 2026-11-03T20:58:33Z (1h16m0s).

| Environment | |
|---|---|
| Machine | `c4-standard-192` in `us-central1-f` (192 vCPUs, GOMAXPROCS 192), kernel 7.3.0-1004-gcp |
| Bucket / profile | `regional-bucket` / `ssb` (shuffle seed 1793734926850387426) |
| Build | go1.26.8, revision db4e45e5dd8dc3bfa7079db793e46012e3dcaa93 + local changes |
| Source | google-cloud-go rust-ffi-datapath working tree (local, unpushed) + gcs_core ABI v4 on google-cloud-storage 1.20 |
| gRPC path | CloudPath (DirectPath disabled for every client) |

## Summary: Rust core relative to the Go SDK (medians over rounds)

Throughput ratio > 1, and latency and CPU ratios < 1, favor the Rust core. `~` marks a throughput difference within the spread between rounds. VM CPU: difference of the VM's utilization (all vCPUs) in percentage points.

| case | comparison | throughput | p50 latency | p99 latency | CPU per op | peak memory (Rust − Go) | VM CPU (Rust − Go) |
|---|---|---|---|---|---|---|---|
| open | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 1.02x ~ | 0.98x | 1.00x | 0.63x | +8 MiB | -0.014 pp |
| read50x2x4 | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 1.00x ~ | 1.02x | 0.98x | 0.77x | +20 MiB | +0.000 pp |
| read50x2x4-parallel | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 1.01x ~ | 0.94x | 1.19x | 0.90x | +579 MiB | -0.054 pp |
| random8k | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 1.01x ~ | 0.99x | 1.00x | 1.09x | +18 MiB | -0.007 pp |

## Per case

### open

open a zero-length reader at a random offset of a 100 KiB object and close it; 1 workers, 240 s timed phase — NewRangeReader(random offset, 0) + Close: open a read stream and wait for the first response (with the object).

| backend | rounds ok | ops/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 14.0 (13.8–14.2) | 14.0 | 66.6 | 90.2 | 154.6 | 71.5 | – | 1.883 | – | 0.026 | 19.7 | 57.0 | – | 0 |
| rust-grpc | 2/2 | 14.3 (14.2–14.5) | 14.3 | 65.3 | 83.7 | 154.3 | 69.7 | – | 1.187 | – | 0.017 | 27.5 | 247.5 | 2.0 | 0 |


### read50x2x4

read 50 x 2 MiB at a 4 MiB stride of a 200 MiB object, one range reader per range, one after the other; 1 workers, 240 s timed phase; io.Copy — one NewRangeReader per range, one after the other (the shared benchmarks' non-MRD variant).

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 15.4 (14.9–16.0) | 0.2 | 5962.8 | 6301.8 | 13815.7 | 6482.2 | 108.6 | 308.224 | 3.16 | 0.048 | 26.8 | 57.0 | – | 0 |
| rust-grpc | 2/2 | 15.5 (15.1–15.9) | 0.2 | 6077.3 | 6451.2 | 13472.8 | 6463.1 | 115.0 | 236.068 | 2.42 | 0.037 | 46.5 | 247.5 | 250.0 | 0 |


### read50x2x4-parallel

read 50 x 2 MiB at a 4 MiB stride of a 200 MiB object, one range reader per range, all at once; 1 workers, 240 s timed phase; io.Copy — all 50 range readers at once: closer to a MultiRangeDownloader with 50 ranges.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 148.4 (129.8–166.9) | 1.5 | 677.1 | 780.5 | 867.5 | 684.7 | 628.6 | 389.162 | 3.99 | 0.574 | 440.4 | 57.5 | – | 0 |
| rust-grpc | 2/2 | 150.3 (143.1–157.6) | 1.5 | 638.8 | 819.2 | 1030.1 | 666.9 | 567.2 | 351.389 | 3.60 | 0.527 | 1018.9 | 283.0 | 250.0 | 0 |


### random8k

read random 8 KiB ranges of 100 MiB objects; 1 workers, 240 s timed phase; io.Copy — one NewRangeReader per 8 KiB range.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc-bidi | 2/2 | 0.1 (0.1–0.1) | 14.5 | 64.2 | 82.7 | 148.6 | 69.1 | 64.1 | 1.016 | 133.18 | 0.015 | 18.6 | 55.5 | – | 0 |
| rust-grpc | 2/2 | 0.1 (0.1–0.1) | 14.6 | 63.5 | 80.6 | 148.4 | 68.4 | 63.5 | 1.106 | 144.98 | 0.016 | 36.6 | 247.5 | 4.0 | 0 |


## VM CPU utilization (compute.googleapis.com/instance/cpu/utilization)

Mean of the 1-minute samples that lie entirely inside a measured phase (median over rounds). The VM has 192 vCPUs: one fully busy vCPU is 0.52 %. Idle VM (samples before and after the run): 0.064 % (8 samples).

| case | backend | VM CPU % | VM CPU − idle, in vCPUs | process CPU cores | samples |
|---|---|---|---|---|---|
| open | go-grpc-bidi | 0.084 | 0.039 | 0.026 | 6 |
| open | rust-grpc | 0.070 | 0.012 | 0.017 | 6 |
| read50x2x4 | go-grpc-bidi | 0.104 | 0.077 | 0.048 | 6 |
| read50x2x4 | rust-grpc | 0.104 | 0.078 | 0.037 | 6 |
| read50x2x4-parallel | go-grpc-bidi | 0.557 | 0.947 | 0.574 | 6 |
| read50x2x4-parallel | rust-grpc | 0.503 | 0.843 | 0.527 | 6 |
| random8k | go-grpc-bidi | 0.082 | 0.036 | 0.015 | 6 |
| random8k | rust-grpc | 0.075 | 0.022 | 0.016 | 6 |

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
