# Benchmarks: Go SDK vs the Rust core

Compares the Go SDK's own transports with the shared Rust core (`experimental.WithRustCore`)
for the data-path APIs: **WriteObject** (uploads), **ReadObject** (whole-object and ranged
downloads) and **ListObjects**, over the **JSON API** and **gRPC (CloudPath)**. The Rust SDK has no
DirectPath, so every gRPC client runs on CloudPath.

- `../cmd/rustbench`: the harness (Go). It creates the datasets, runs every (case, backend) pair
  in its own process and writes one JSON line per pair and round.
- `../cmd/rustbench-report`: turns the JSON lines into a Markdown report (Go).
- `vm/`: scripts that create a GCE VM, ship the local code to it, build everything there,
  run the suite and fetch the results.
- `../cmd/rapidbench` and `rapid/`: the SDK consolidation benchmark plan in the same form as the
  Java PoC's (one process per run, the same workloads, windows, output files and charts).

## Results

| Run | What |
|---|---|
| [`RESULTS.md`](RESULTS.md) (2026-11-05) | **current:** workloads 1–6 in the Java PoC's report format, each engine on its default settings (`rapidbench`), plus workloads 4 and 5 over Bidi with 192 gRPC connections on both engines (`run_matched.sh`) |
| [`results/2026-11-03/REPORT.md`](results/2026-11-03/REPORT.md) | Open, Read50x2x4 and Random8K with one worker (`rustbench`, profile `ssb`) |
| [`results/2026-11-04/REPORT.md`](results/2026-11-04/REPORT.md) | workloads 1–6 of the SDK consolidation plan, 48 connections for the 48-worker workloads (`rustbench`, profile `worklog`) |

## Backends

| backend | client | reads | uploads | listing |
|---|---|---|---|---|
| `go-json` | `NewClient(WithJSONReads())` | JSON, HTTP/2 | JSON | JSON |
| `go-json-h1` | same, HTTP/1.1 only | JSON, HTTP/1.1 | JSON | JSON |
| `go-xml` | `NewClient(WithXMLReads())`, the default read path | XML | (= go-json) | (= go-json) |
| `go-grpc` | `NewGRPCClient()` | ReadObject | BidiWriteObject | v2 ListObjects |
| `go-grpc-bidi` | `NewGRPCClient(WithGRPCBidiReads())` | BidiReadObject | (= go-grpc) | (= go-grpc) |
| `rust-json` | `NewClient(experimental.WithRustCore())` | JSON, HTTP/1.1 | JSON | JSON (core's `storage.objects.list`) |
| `rust-grpc` | `NewGRPCClient(experimental.WithRustCore())` | BidiReadObject | **JSON** (see gaps); appendable uploads to zonal buckets: BidiWriteObject | v2 ListObjects |

`rustbench matrix` prints this matrix and the reason for every skipped pair. Pairs are skipped
when they would repeat another backend's measurement exactly, or are impossible.

Apples-to-apples comparisons in the report:

- `rust-json` vs `go-json-h1`: same API and protocol.
- `rust-grpc` vs `go-grpc-bidi`: same read RPC.
- `rust-json` vs `go-json`, `rust-grpc` vs `go-grpc`, and `go-xml`: what users run today.

## Workloads (profile `default`)

| case | what one op does | workers × ops |
|---|---|---|
| `read-64KiB-c16` | read a whole 64 KiB object | 16 × 100 |
| `read-16MiB-c8` | read a whole 16 MiB object | 8 × 8 |
| `read-256MiB-c1` / `-c4` | read a whole 256 MiB object | 1 × 4 / 4 × 3 |
| `read-16MiB-c8-buf32KiB` | as `read-16MiB-c8`, with 32 KiB `Read` calls | 8 × 8 |
| `range-64KiB-c16` / `range-1MiB-c16` | read a random 64 KiB / 1 MiB range of a 256 MiB object | 16 × 60 / 16 × 30 |
| `write-64KiB-c16` | upload 64 KiB (one request) | 16 × 50 |
| `write-8MiB-c8` | upload 8 MiB (one request, below the 16 MiB chunk size) | 8 × 5 |
| `write-64MiB-c4` / `write-256MiB-c1` | resumable upload in 16 MiB chunks | 4 × 3 / 1 × 3 |
| `write-8MiB-c8-buf32KiB` | as `write-8MiB-c8`, with 32 KiB `Write` calls | 8 × 5 |
| `list-10k-c1` / `-c8` | list 10,000 objects, pages of 1,000 | 1 × 5 / 8 × 3 |
| `list-10k-delim` | delimiter listing of the same dataset (100 prefixes) | 4 × 10 |

Every case also runs warmup ops (connections, TLS, tokens) that are not measured. The `quick`
profile is a few-minute smoke test of the whole pipeline.

## Metrics

Per (case, backend, round):

- throughput (MiB/s; entries/s for listing) and ops/s;
- latency p50/p90/p99 per op, plus time to first byte (or first page);
- CPU time (user + sys) for the whole process during the measured phase: Go and Rust threads,
  per GiB moved (per listing for list cases);
- peak RSS above the pre-client baseline, peak thread count, Go GC cycles;
- FFI calls per op (Rust backends).

The report shows medians over rounds and the min–max spread of throughput. It also compares
each Rust backend with the Go backends on the same transport and marks a difference with `~`
when it lies within the round-to-round spread.

## Fairness controls

- Same VM, same bucket, same objects, same op pattern (`Read`/`Write` call size) for every backend.
- One process per (case, backend, round), so CPU and memory are attributed cleanly.
- Pairs run in a new random order every round, so network drift spreads over all backends.
- `GOOGLE_CLOUD_DISABLE_DIRECT_PATH=true` for every process: Go's gRPC client would otherwise use
  DirectPath on GCE, which the Rust SDK cannot.
- Client defaults are left alone: one gRPC channel for both, 16 MiB upload chunks for both,
  CRC32C computed on uploads and validated on full downloads by both.

## Known gaps and differences (Rust SDK 1.20)

- **No gRPC uploads to standard buckets.** The SDK's only gRPC write path is appendable objects
  in zonal buckets (unstable feature). `rust-grpc` therefore uploads over JSON, and the pair is
  not measured separately.
- **No JSON listing in the SDK.** Its control plane is gRPC-only. The core implements
  `storage.objects.list` itself (`gcs_core/src/json_api.rs`), on the SDK's retry loop, policies,
  credentials and error types.
- **The JSON transport is HTTP/1.1-only** (gax-internal forces `http1_only`; TODO #4298 upstream).
  Go's JSON transport negotiates HTTP/2, which multiplexes concurrent requests on one
  connection. Compare `rust-json` with both `go-json` and `go-json-h1`.
- **BidiReadObject** is what the Rust SDK reads with. Its docs warn it is only enabled for some
  projects/buckets. It worked on the bucket used for the tests (verified by `TestRustCoreGRPCParity`).

## Running on a VM

```sh
export PROJECT=my-project BUCKET=my-bucket       # required by the scripts (vm/config.sh)
cd storage/internal/rustcore/bench/vm
./create_vm.sh                                   # n2-standard-16, us-central1-a, Debian 12, gVNIC
./push_code.sh                                   # local working tree -> VM (nothing is git-pushed)
gcloud compute ssh rustbench-$USER --project=$PROJECT --zone=us-central1-a \
  --command '~/setup_vm.sh'                      # Go, Rust, build the core and the binaries (once)
PROFILE=quick ROUNDS=1 ./start_remote.sh         # smoke test: a few minutes
./start_remote.sh                                # the full run: about an hour (5 rounds)
./fetch_results.sh                               # -> ./rustbench-results/<vm>/results/<run id>/report.md
./delete_vm.sh                                   # asks for confirmation
```

`PROJECT` and `BUCKET` are required; the other settings (zone, machine type, service account) are
in `vm/config.sh` and can be overridden through the environment. The VM's service account needs
object read/write access to the bucket (see `create_vm.sh`). Datasets (about 1.2 GiB plus 10,000
empty objects under `rustbench/<vm>/data/`) are created once and reused by later runs; `CLEANUP=1`
(or `rustbench cleanup`) deletes them. Written objects are deleted by the process that wrote them.

On the VM directly:

```sh
BUCKET=my-bucket ~/run_bench.sh                                     # whole suite + report
BUCKET=... CASES=read-16MiB-c8 BACKENDS=go-json-h1,rust-json ROUNDS=3 ~/run_bench.sh
~/bin/rustbench run -bucket ... -prefix rustbench/$(hostname) -backend rust-grpc -case range-1MiB-c16
```

## Shared-benchmark read workloads (profile `ssb`) and VM CPU

Profile `ssb` runs the Open, Read50x2x4 and Random8K workloads of the
[shared storage benchmarks](https://github.com/googleapis/storage-shared-benchmarking), with one
worker as there. Those use a MultiRangeDownloader, which the Rust core does not have yet, so both
clients read with one range reader per range here:

| Case | One op |
|---|---|
| `open` | `NewRangeReader(random offset, 0)` + `Close` on a 100 KiB object: over gRPC, open a read stream and wait for its first response (with the object) |
| `read50x2x4` | 50 x 2 MiB ranges at a 4 MiB stride of a 200 MiB object, one reader per range, one after the other |
| `read50x2x4-parallel` | the same with all 50 readers at once (closer to a MultiRangeDownloader) |
| `random8k` | one random 8 KiB range of a 100 MiB object |

Every case runs for a fixed time (240 s; profile `ssb-smoke`: 15 s) so that the VM's 1-minute CPU
metric can be attributed to it. Compare `go-grpc-bidi` with `rust-grpc`: the same RPC
(BidiReadObject) and one gRPC connection each (the core follows `option.WithGRPCConnectionPool`,
default 1, not the Rust SDK's default of one connection per CPU).

```sh
rustbench setup -profile ssb -bucket $BUCKET -prefix ffi-bench    # needs write access to the bucket
rustbench suite -profile ssb -bucket $BUCKET -prefix ffi-bench -setup=false \
  -backends go-grpc-bidi,rust-grpc -rounds 2 -cooldown 60s -out results.jsonl
```

To add the VM's CPU utilization to the report, fetch `compute.googleapis.com/instance/cpu/utilization`
of the VM (its instance id is in every result line, `env.instance_id`) for the run and a few idle
minutes before it:

```sh
curl -s -G -H "Authorization: Bearer $(gcloud auth application-default print-access-token)" \
  -H "x-goog-user-project: $PROJECT" \
  --data-urlencode "filter=metric.type=\"compute.googleapis.com/instance/cpu/utilization\" AND resource.labels.instance_id=\"$INSTANCE_ID\"" \
  --data-urlencode "interval.startTime=$START" --data-urlencode "interval.endTime=$END" \
  "https://monitoring.googleapis.com/v3/projects/$PROJECT/timeSeries" > cpu.json
rustbench-report -vm-cpu cpu.json results.jsonl > report.md
```

The report averages the samples that lie entirely inside each measured phase, and the idle samples
before and after the run. On a large VM a single-stream workload barely moves this metric (one vCPU
of 192 is 0.52 %), so compare the per-process CPU columns too.

## SDK consolidation benchmark plan (profile `worklog`) and VM memory

Profile `worklog` runs the benchmark plan of the SDK consolidation POCs, all over gRPC (CloudPath):

| Case | One op | Workers, phase | Backends |
|---|---|---|---|
| `open`, `read50x2x4`, `read50x2x4-parallel`, `random8k` | as in profile `ssb` | 1, 240 s | `go-grpc-bidi`, `rust-grpc`; plus `go-grpc` (non-bidi reads, the baseline) except for `open` |
| `bidiread-16MiB-c48` | the next 16 MiB range of the worker's own 200 MiB object (front to back, one range reader per range; the 8 MiB tail is skipped) | 48, 300 s | `go-grpc-bidi`, `rust-grpc` |
| `bidiwrite-200MiB-c48` | an appendable upload of a new 200 MiB object to the zonal bucket (`-zonal-bucket`), finalized on `Close` | 48, 300 s | `go-grpc`, `rust-grpc` |
| `list-objects-100` | one ListObjects RPC returning 100 objects | 1, 120 s | `go-grpc`, `rust-grpc` |
| `list-buckets-100` | one ListBuckets RPC returning 100 buckets of `-project` | 1, 120 s | `go-grpc`, `rust-grpc` |

The 48-worker cases give both gRPC clients a pool of 48 connections (`option.WithGRPCConnectionPool`),
so that the connection count does not cap throughput. BidiWrite runs as appendable uploads to a zonal
bucket because that is the Rust SDK's only gRPC upload. The project needs at least 100 buckets.

```sh
rustbench suite -profile worklog -bucket $BUCKET -zonal-bucket $ZONAL_BUCKET -project $PROJECT \
  -prefix ffi-bench -rounds 2 -cooldown 60s -out results.jsonl
```

For the VM's memory, install the [Ops Agent](https://cloud.google.com/monitoring/agent/ops-agent) on
the VM (it has no package for non-LTS Ubuntu releases; Debian 12 and 13 work), fetch
`agent.googleapis.com/memory/percent_used` with the filter `metric.labels.state="used"` the same way as
the CPU metric, and pass it with `-vm-mem mem.json`. The report shows the highest sample inside each
measured phase. On a large VM it moves little (1 GiB of 708 GiB is 0.14 %), so compare the per-process
peak RSS too.
