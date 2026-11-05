# Go SDK vs Rust core: data-path benchmark

Run `20261104T161035Z`: 4 results, 2 rounds, 2026-11-04T16:10:36Z to 2026-11-04T16:29:06Z (18m0s).

| Environment | |
|---|---|
| Machine | `c4-standard-192` in `us-central1-b` (192 vCPUs, GOMAXPROCS 192), kernel 6.12.113+deb13-cloud-amd64 |
| Bucket / profile | `regional-bucket` / `worklog` (shuffle seed 1793808635955465750) |
| Zonal bucket (appendable uploads) | `zonal-bucket` |
| Build | go1.26.8, revision dca5660d337f64209421575ad9a51522dc61792d + local changes |
| gRPC path | CloudPath (DirectPath disabled for every client) |

## Summary: Rust core relative to the Go SDK (medians over rounds)

Throughput ratio > 1, and latency and CPU ratios < 1, favor the Rust core. `~` marks a throughput difference within the spread between rounds. VM CPU: difference of the VM's utilization (all vCPUs) in percentage points.

| case | comparison | throughput | p50 latency | p99 latency | CPU per op | peak memory (Rust − Go) | VM CPU (Rust − Go) |
|---|---|---|---|---|---|---|---|
| bidiwrite-200MiB-c48 | rust-grpc vs go-grpc (Go gRPC client; reads with ReadObject) | 0.97x ~ | 1.00x | 0.80x | 0.97x | +16092 MiB | -0.322 pp |

## Per case

### bidiwrite-200MiB-c48

appendable upload of 200 MiB objects to the zonal bucket (BidiWriteObject), finalized on Close; 48 workers, 300 s timed phase; 1 MiB Write calls; gRPC clients use 48 connections — appendable upload of a 200 MiB object to the zonal bucket per op (BidiWriteObject), finalized on Close; each worker overwrites its own object.

| backend | rounds ok | MiB/s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| go-grpc | 2/2 | 4499.9 (4382.1–4617.7) | 22.5 | 1896.3 | 2501.4 | 8219.3 | 2111.5 | – | 317.138 | 1.62 | 7.136 | 2476.2 | 106.0 | – | 2 |
| rust-grpc | 2/2 | 4378.1 (4196.1–4560.0) | 21.9 | 1891.3 | 3209.2 | 6611.6 | 2182.2 | – | 307.084 | 1.57 | 6.728 | 18567.8 | 276.0 | 204.0 | 1 |

- `go-grpc` first error: `write ffi-bench/write/bidiwrite-200MiB-c48/go-grpc/1793809010914191101/w39: rpc error: code = Internal desc = We encountered an internal error. Please try again. Internal details: ALosWsF/GvV+J+RqrIkOAQXiw9oXKRei+V99mLhHX832A6TA9KJ0IMGQkWWAqc8AuhsYmxlro7MC+h0rXpe45YCev5+bLbsJGoKb994QxxxOPTetaKrIA2CV…`
- `rust-grpc` first error: `close ffi-bench/write/bidiwrite-200MiB-c48/rust-grpc/1793808636536494592/w03: gcs rust core: cannot serialize the request the transport reports an error: code: 'Internal error', message: "We encountered an internal error. Please try again. Internal details: ALosWsHB6X/SS/sk9C/NFPBXYsWZuZqxyUYTKmXLhC…`

## VM CPU utilization (compute.googleapis.com/instance/cpu/utilization)

Mean of the 1-minute samples that lie entirely inside a measured phase (median over rounds). The VM has 192 vCPUs: one fully busy vCPU is 0.52 %. Idle VM (samples before and after the run): 0.642 % (20 samples).

| case | backend | VM CPU % | VM CPU − idle, in vCPUs | process CPU cores | samples |
|---|---|---|---|---|---|
| bidiwrite-200MiB-c48 | go-grpc | 4.951 | 8.273 | 7.136 | 8 |
| bidiwrite-200MiB-c48 | rust-grpc | 4.629 | 7.654 | 6.728 | 8 |

## VM memory (agent.googleapis.com/memory/percent_used, state used)

Highest 1-minute sample inside a measured phase (median over rounds). The VM has 709 GiB: 1 GiB is 0.141 %. Idle VM (samples before and after the run): 0.860 % (20 samples).

| case | backend | VM memory used % (peak) | above idle, GiB | process peak RSS − baseline MiB | samples |
|---|---|---|---|---|---|
| bidiwrite-200MiB-c48 | go-grpc | 1.230 | 2.62 | 2476.2 | 10 |
| bidiwrite-200MiB-c48 | rust-grpc | 3.424 | 18.17 | 18567.8 | 10 |

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
