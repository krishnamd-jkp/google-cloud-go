# Rust data path for `cloud.google.com/go/storage` (proof of concept)

**Experimental. Not a supported API.** This directory holds the FFI bindings and the Rust engine
behind `experimental.WithRustDataPath()`. When the option is on, object reads and writes go
Go → cgo → Rust (`google-cloud-storage` crate). Everything else stays on the pure-Go client.
The public `storage` API is unchanged.

```text
storage.Client ── tc ──► rustStorageClient (storage/rust_client.go)
                          ├─ NewRangeReader / OpenWriter ──► internal/rustcore (cgo) ──► gcs_core (Rust, static lib)
                          └─ everything else, and unsupported options ──► httpStorageClient (pure Go)
```

## Layout

| Path | What |
|---|---|
| `gcs_core/` | Rust engine (C ABI). See `src/lib.rs` (contract, client, runtime), `reader.rs`, `writer.rs`, `creds.rs`, `retry.rs` |
| `gcs_core.h` | Hand-written C header for the ABI |
| `rustcore.go`, `callbacks.go` | cgo bindings (`Client`, `Reader`, `Writer`, error mapping, exactly-once completions) |
| `link_linux_amd64.go` | Links `lib/linux_amd64/libgcs_core.a` (one file per supported GOOS/GOARCH) |
| `build.sh` | Builds the engine and installs it into `lib/<goos>_<goarch>/` (git-ignored) |
| `cmd/rustbench/` | Go-vs-Rust benchmark on the same workload |
| `../../rust_client.go` | `rustStorageClient`: routing, fallbacks, error mapping, credentials bridge |
| `../../rust_client_stub.go` | No-op when the engine is not compiled in |
| `../../rust_datapath_test.go` | Regression tests (real GCS or testbench) |

Build tags: the engine is compiled in only with `-tags gcsrust`, cgo enabled, on linux/amd64.
With any other combination (`CGO_ENABLED=0`, cross-compiles, no tag) the option is a no-op
and the pure-Go transport is used, so the module still builds everywhere.

## Build and run

Requires Go 1.24+ and Rust (rustup, stable). The default `ring` crypto provider needs no cmake.

```bash
cd storage

# 1. Build the Rust engine (~2–3 min cold, ~1 min incremental) -> internal/rustcore/lib/linux_amd64/
./internal/rustcore/build.sh

# 2. Build: pure Go (unchanged), no-cgo (unchanged), and with the engine
go build ./... && CGO_ENABLED=0 go build ./... && go build -tags gcsrust ./...
go vet -tags gcsrust . ./internal/rustcore

# 3. Regression tests against real GCS (objects under rustdp/<run-id>/, deleted afterwards)
RUST_DP_TEST_BUCKET=<your-bucket> go test -tags gcsrust -short -run TestRustDataPath -v .
#    ... also under the race detector:
RUST_DP_TEST_BUCKET=<your-bucket> go test -race -tags gcsrust -short -run TestRustDataPath -v .

# 4. FFI micro-benchmarks (sync call vs async completion)
go test -tags gcsrust -run XXX -bench . ./internal/rustcore

# 5. Go vs Rust on the same workload (separate processes)
go build -tags gcsrust -o /tmp/rustbench ./internal/rustcore/cmd/rustbench
/tmp/rustbench -backend go   -bucket <your-bucket> -size 16777216 -iters 3
/tmp/rustbench -backend rust -bucket <your-bucket> -size 16777216 -iters 3
/tmp/rustbench -backend rust -bucket <your-bucket> -size 16777216 -iters 3 -style 32k
```

Using it from code:

```go
client, err := storage.NewClient(ctx, experimental.WithRustDataPath()) // build with -tags gcsrust
```

### Emulator (storage-testbench)

Reads and the read retry-conformance suite run on the testbench. **Uploads from the Rust SDK
do not work there**: multipart uploads get a 500 because the media part has no Content-Type,
and resumable PUT bodies hang. This is a Rust SDK/testbench incompatibility, not a binding bug.

```bash
docker run --rm -d --net=host gcr.io/cloud-devrel-public-resources/storage-testbench:latest
curl "http://localhost:9000/start_grpc?port=8888"
export STORAGE_EMULATOR_HOST=http://localhost:9000 STORAGE_EMULATOR_HOST_GRPC=localhost:8888
go test -tags gcsrust -short -run 'Test(OpenReader|NewRangeReaderRemain)Emulated' -v .
go test -tags gcsrust -short -run 'TestRetryConformance/(rust|http)-.*storage.objects.get' -v .
```

## What the engine handles, and what falls back to Go

* **Engine:**
  * reads: full, range and tail reads, generation, all 4 preconditions, CSEK, `ReadCompressed`/decompressive transcoding, CRC32C validation and resume;
  * writes: single-shot and resumable, all editable attributes, user MD5/CRC32C, KMS, ACLs, all 4 preconditions, CSEK, retry policy, max attempts, max duration, backoff;
  * context cancellation everywhere (a cancelled upload is never finalized);
  * any Go credential, through a token callback.
* **Falls back to Go:**
  * zero-length reads;
  * `WithErrorFunc`;
  * `userProject`;
  * `ChunkSize == 0`;
  * `ChunkRetryDeadline` / `ChunkTransferTimeout`;
  * appendable writes;
  * `Retention` / `Contexts`;
  * every non-object operation (list, metadata, ACLs, IAM, HMAC, notifications, …);
  * clients built without Go credentials (e.g. a custom `http.Client`).
* **Not available on the engine path:**
  * `ReaderObjectAttrs.CacheControl` / `LastModified`;
  * per-call headers (`callctx.SetHeaders`);
  * `DisableAutoChecksum`;
  * mTLS/S2A;
  * `x-goog-api-client` attribution (reports Rust).

## Design notes (why the bindings look like this)

* **Panics.** Every exported Rust function is panic-guarded, and background tasks are wrapped, so a panic becomes an error, never an abort or a lost callback.
* **Handle lifetime.** Handles are `Arc`-backed; Go guards the raw pointer with a mutex, so `Close` racing `Read` is safe.
* **Completions.** Async completions fire exactly once. User data is a `uintptr_t` `cgo.Handle`, never an `unsafe.Pointer`, which keeps `go vet` and checkptr (`-race`) clean.
* **Hot path.** Reads drain a bounded prefetch queue synchronously (about 32 ns per call) and only wait asynchronously (tens of µs) when the network is behind. Writes copy into a bounded queue in messages of at most 1 MiB, so Rust-side buffering is bounded in bytes.
* **No partial commits.** Uploads finalize only on an explicit Close. Abort, cancellation, or dropping the handle never finalizes a partial object.
