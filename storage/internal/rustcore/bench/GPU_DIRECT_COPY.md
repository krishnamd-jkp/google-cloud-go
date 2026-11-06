# Reading Cloud Storage data straight into GPU memory: Go PoC

Measured on 2026-11-05, 14:09–15:01 UTC, on the same VM and bucket as [RESULTS.md](RESULTS.md).
**No GPU was used:** a stand-in device did the CPU work of each path (see [The test](#the-test)).

## Summary

- **Direct copies into GPU-ready memory work through the unchanged Go API.** `Reader.Read` can be
  given page-locked ("pinned") host memory allocated outside the Go heap, for example with
  `cuMemHostAlloc`. The Rust core copies each range once, from the Rust SDK's receive buffers into
  that memory, and the GPU can then copy it by DMA, without the CPU.
- **On the Rust core this costs the same CPU as reading into a Go heap buffer:** −0.1% per GiB over
  JSON, +3.2% over Bidi.
- **The usual alternative costs 9–13% more CPU:** reading into a heap buffer and then calling a
  host-to-device copy, which first copies the data into the driver's own pinned buffer.
- **The cgo boundary adds no copy.** The Go SDK's own transports also copy once into the caller's
  buffer. A sidecar process connected by a Unix socket would add a socket hop, which costs 3.2× a
  `memcpy` per GiB, unless it used shared memory.
- **Correctness:** 12,394 ranges (194 GiB) were read back from the device and compared, with 0
  mismatches.
- **No SDK can copy from the network card straight into GPU memory for Cloud Storage** (GPUDirect
  RDMA or similar): the data arrives encrypted and is decrypted on the CPU, into the SDK's memory.
- **Not measured:** a real GPU, so neither host-to-device bandwidth nor how well the copies overlap
  the reads.

## Three ways data can reach a GPU

| Path | How | Possible for Cloud Storage? |
|---|---|---|
| Network card → GPU | GPUDirect RDMA, device-memory TCP | **No, for any SDK design.** The data arrives encrypted (TLS, or ALTS on DirectPath) and is decrypted on the CPU. |
| SDK → pinned host memory → GPU | The caller passes pinned memory to `Read`; the GPU copies from it by DMA | **Yes,** if the SDK writes into the caller's memory without an intermediate copy. This is what the test checks. |
| SDK → Go heap buffer → GPU | A host-to-device copy from ordinary (pageable) memory | Yes, but the driver first copies the data into its own pinned buffer: one more CPU copy, and the call blocks. |

## How the PoC's read path allows it

1. The Rust SDK decrypts each response chunk into its own buffer (`bytes::Bytes`). The core
   prefetches up to 8 MiB of chunks per reader ([`reader.rs`](../gcs_core/src/reader.rs)).
2. `Reader.Read(p)` passes `&p[0]` to `gcs_reader_read` ([`rustcore.go`](../rustcore.go)), and the
   core copies the queued chunks into `p` (`drain_into`). This is the only copy between the SDK and
   the caller.
3. The binding pins `p` with `runtime.Pinner`, which does nothing for memory outside the Go heap.
   So `p` can be a slice over pinned memory (built with `unsafe.Slice`), and the copy lands there
   directly.
4. From pinned memory, an asynchronous host-to-device copy (`cuMemcpyHtoDAsync`) is a DMA: it takes
   no CPU time and overlaps the next read.

The Go SDK's own transports make the same single copy, from their HTTP or gRPC buffers into `p`.

## The test

[`cmd/gpucopy`](../cmd/gpucopy/main.go) reads random 16 MiB ranges of a 200 MB object through the
public API (`NewRangeReader` and `io.ReadFull`) with 48 workers, and puts each range in one of
three places:

| Mode | Flag | Where each range goes |
|---|---|---|
| **Heap** (the baseline) | `--mode=host` | A Go heap buffer, with no device step |
| **Direct** | `--mode=pinned` | Pinned memory outside the Go heap (two buffers per worker), then an asynchronous DMA |
| **Heap + copy** | `--mode=pageable` | A Go heap buffer, then a blocking host-to-device copy |

- **Engines and transports:** the Rust core (`--engine=rust`) and the Go SDK's own transports
  (`--engine=go`), each over JSON and over Bidi (gRPC `BidiReadObject`).
- **Device:** `--device=sim`, a stand-in that keeps only the CPU work of each path. Pinned memory
  is `mmap`'ed, pre-faulted and locked; a pageable copy is a `memcpy` into a pinned staging buffer;
  a pinned copy costs no CPU, as with a real DMA. `--device=cuda` uses a real GPU through the CUDA
  driver API (`libcuda.so.1`, loaded at run time, so building needs no CUDA toolkit). That path was
  checked only against a fake driver, which validates the call sequence and rejects asynchronous
  copies from unpinned memory.
- **Verification:** `--verify` reads ranges back from the device and compares them with the
  object's known content.
- **Connections, matched on both engines:** JSON over HTTP/1.1 with about 48 connections each (the
  Go SDK needs `--json-http1`; by default it uses one HTTP/2 connection), and Bidi with 48 gRPC
  connections each (`--grpc-conns=48`).
- **Runs:** 45 s measured after a 10 s warm-up, in 3 rounds. Go and Rust ran back to back for each
  combination, and the order of the combinations rotated between rounds.
- **Metric:** CPU seconds of the benchmark process per GiB read.
- **Environment:** as in [RESULTS.md](RESULTS.md#environment): a `c4-standard-192` VM in
  `us-central1`, reading a regional bucket in `us-west1` over CloudPath.

## Results

CPU seconds per GiB read, median of 3 rounds, with the change from heap:

| Engine, transport | Gb/s | Heap | Direct | Heap + copy |
|---|--:|--:|--:|--:|
| Rust, JSON | 17.7–19.6 | 2.178 | 2.175 (**−0.1%**) | 2.394 (**+9.9%**) |
| Go, JSON | 17.2–19.0 | 2.422 | 2.502 (+3.3%) | 2.648 (+9.3%) |
| Rust, Bidi | 9.2–9.3 | 2.468 | 2.547 (**+3.2%**) | 2.799 (**+13.4%**) |
| Go, Bidi | 9.3–9.5 | 3.398 | 3.549 (+4.4%) | 3.718 (+9.4%) |

Gb/s is the range of the three modes' medians. The third round was slower for most combinations,
on both engines alike, which is why the table uses medians.

**Hand-off costs**, on one thread with no network (`--workload=copycost`):

| Hand-off | GB/s | CPU s per GiB |
|---|--:|--:|
| `memcpy` into the caller's buffer: what the core does, into heap or pinned memory | 14.9 | 0.072 |
| Unix domain socket hop, both sides: a sidecar's hand-off | 9.2 | 0.23 |
| The same, with 4 MiB socket buffers | 8.2 | 0.26 |

**GC check**, single runs at 14:53–15:01 UTC (CPU s per GiB, with the Go GC cycles in the 45 s
window):

| Engine, transport | Heap | Direct | Direct, `GOGC=off GOMEMLIMIT=2GiB` |
|---|--:|--:|--:|
| Go, JSON | 2.359 (0 cycles) | 2.489 (67) | 2.314 (0) |
| Go, Bidi | 3.424 (0) | 3.680 (4) | 3.566 (0) |
| Rust, JSON | 2.091 (0) | 2.168 (10) | not run |

**Correctness:** 12,394 ranges (194 GiB) were read back from the device and compared, across both
engines, both transports, and the direct and heap + copy paths: 0 mismatches, 0 errors. The count
includes an earlier pass at 12:54 UTC with one connection each.

## What the results mean

1. **The direct path works on both engines, through the unchanged API.** On the Rust core it costs
   about the same CPU as the heap baseline: −0.1% on JSON, and +3.2% on Bidi, for a reason not yet
   identified.
2. **The Go SDK's own transports pay a GC cost for direct buffers.** With the read buffers off the
   Go heap, the heap is small, so the GC runs much more often: on JSON, 67 cycles in 45 s, against
   none with heap buffers. `GOGC=off` with `GOMEMLIMIT=2GiB` removed the gap on JSON, and part of it
   on Bidi, where the runs were noisy. The Rust core keeps its data buffers in Rust memory; its GC
   ran 10 cycles. Go programs that read into off-heap or GPU memory should tune the GC, with a
   higher `GOGC` or with `GOGC=off` plus a `GOMEMLIMIT`. `GOMEMLIMIT` alone doesn't make the GC run
   less often.
3. **The extra copy of heap + copy costs CPU, not throughput.** It took 3.5–3.9 ms per 16 MiB range
   (median), against roughly 330–700 ms of network time per range. It adds 0.22–0.33 CPU s per GiB,
   3–5× an uncontended `memcpy`. At 200 Gbps (about 23 GiB/s) that would be roughly 5–8 cores.
4. **FFI against a sidecar.** FFI hands the data over with one `memcpy`, 0.07 CPU s per GiB. A Unix
   socket hop costs 0.23. A sidecar would need a shared-memory data path to match FFI.
5. **What a direct path can't remove:** the Rust SDK's own receive buffers, and the core's one copy
   out of them. Removing that copy would need the Rust SDK to decrypt into the caller's memory. Its
   API hands out `Bytes`, so that would be an upstream change.

## Not measured yet

- **A real GPU:** host-to-device bandwidth, and how well the DMA overlaps the next read.
- **Next step:** the same commands with `--device=cuda` on a VM with one GPU, for example a
  `g2-standard-16` with one L4.

## Reproducing

Build the Rust core first with [`build.sh`](../build.sh), then, from `storage/`:

```sh
go build -tags gcsrust -o gpucopy ./internal/rustcore/cmd/gpucopy
```

One run of the measured pass:

```sh
./gpucopy --engine=rust --transport=bidi --grpc-conns=48 --mode=pinned --device=sim \
  --bucket=<bucket> --workers=48 --warmup=10s --duration=45s
```

- For JSON, replace `--grpc-conns=48` with `--json-http1`.
- `--verify=N` checks every Nth range of each worker. The object must hold `rapidbench`'s payload,
  as created for [RESULTS.md](RESULTS.md).
- `--workload=copycost` measures the hand-off costs.

Raw data, binary checksums and run logs: [`results/2026-11-05-gpucopy/`](results/2026-11-05-gpucopy/).
