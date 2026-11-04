# Go SDK on the shared Rust core (proof of concept)

**Experimental. Not a supported API.** With `experimental.WithRustCore()`, *every* operation
of `cloud.google.com/go/storage` is served by one Rust core (`gcs_core`, built on the
`google-cloud-storage` crate). The public `storage` API is unchanged.

```text
storage.Client ── tc ──► rustStorageClient (storage/rust_client.go)
                          ├─ metadata RPCs: grpcStorageClient (Go API <-> v2 protos, unchanged)
                          │      └─ raw = rustRPC (storage/rust_transport.go) ─┐
                          └─ object reads / writes ────────────────────────────┤
                                                                               ▼
                               internal/rustcore (generic cgo binding: bytes in, bytes out)
                                                                               ▼
                               gcs_core (Rust): transport, auth, retries, idempotency,
                               uploads, checksums, read resumption, error classification
```

What stays in Go is what is inherently per-language: the public API, the mapping between Go
types and `google.storage.v2` protos (the existing gRPC-transport code, reused as-is) and
Go-specific API semantics (e.g. `Writer` Content-Type sniffing). No Go retry loop runs: `run()`
makes one call and hands the caller's retry configuration to the core.

## Transports

The constructor picks the core's transport for the data path, as it picks Go's own transport:

| | `NewClient(ctx, experimental.WithRustCore())` | `NewGRPCClient(ctx, experimental.WithRustCore())` |
|---|---|---|
| reads | JSON API (`Storage::read_object`, HTTP/1.1) | gRPC `BidiReadObject` (`Storage::open_object`), CloudPath |
| listing | JSON API `storage.objects.list`, implemented by the core (`json_api.rs`) | gRPC v2 `ListObjects` (`StorageControl`) |
| uploads | JSON API (single request up to `ChunkSize`, resumable above) | JSON API: the Rust SDK has no gRPC upload for standard buckets |
| other metadata RPCs | gRPC (`StorageControl`, the SDK's only control plane) | gRPC |

The Rust SDK has no DirectPath; gRPC always uses CloudPath. Benchmarks of both transports
against the Go SDK: [`bench/`](bench/README.md).

## The ABI (`gcs_core.h`)

Generic, so it does not change when the storage API grows:

| Entry point | Payload |
|---|---|
| `gcs_call(method, req, opts)` | every unary v2 RPC (17 today): proto3-JSON request -> proto3-JSON response; plus JSON API methods the SDK lacks (`storage.objects.list`): JSON API query -> JSON API resource |
| `gcs_read_open` / `gcs_reader_*` | proto3-JSON `ReadObjectRequest`; bytes out |
| `gcs_write_open` / `gcs_writer_*` | `StartResumableWriteRequest`-shaped JSON; bytes in, `Object` out |
| `opts` (JSON) | caller policy only: retry policy/limits/backoff/predicate, userProject, headers, read/write tuning (`grpc` selects BidiReadObject reads) |

FieldMask fields travel as JSON arrays of raw paths (proto3 JSON cannot carry `*` or
`metadata.<key>`).

## Layout

| Path | What |
|---|---|
| `gcs_core/src/lib.rs` | safety contract, status (gRPC + HTTP codes), runtime, client |
| `gcs_core/src/call.rs` | `gcs_call`: dispatch of every unary v2 RPC |
| `gcs_core/src/json_api.rs` | JSON API methods the SDK lacks (`storage.objects.list`), on the SDK's retry loop and error types |
| `gcs_core/src/policy.rs` | per-RPC idempotency rules (decided from the request) |
| `gcs_core/src/options.rs` | options JSON: retry/backoff/predicate, user project, headers |
| `gcs_core/src/reader.rs`, `writer.rs`, `creds.rs` | streaming reads (JSON or gRPC), uploads, credentials bridge |
| `gcs_core.h` | hand-written C header for the ABI |
| `rustcore.go`, `callbacks.go` | generic cgo binding (`Call`, `Reader`, `Writer`, completions, token and retry callbacks) |
| `link_linux_amd64.go` | links `lib/linux_amd64/libgcs_core.a` (one file per supported GOOS/GOARCH) |
| `build.sh` | builds the core and installs it into `lib/<goos>_<goarch>/` (git-ignored) |
| `../../storage_rpc.go` | `storageRPC`: the seam between the Go API mapping and the transport |
| `../../rust_transport.go` | `rustRPC`: one line per RPC, plus generic encode/options/error plumbing |
| `../../rust_client.go` | `rustStorageClient`: construction, transport selection, reads, writes, JSON listing |
| `../../rust_core_test.go` | tests against real GCS (or the testbench), incl. JSON/gRPC parity with the Go transports |
| `../../rust_core_unit_test.go` | hermetic tests (no network) |
| `cmd/rustbench`, `cmd/rustbench-report`, `bench/` | benchmark harness, report generator, VM scripts |

Build tags: the core is compiled in only with `-tags gcsrust`, cgo enabled, on linux/amd64.
Without them the module builds and behaves exactly as before; `WithRustCore()` then makes
`NewClient` return an error.

## Build and test

Requires Go 1.24+ and Rust (rustup, stable). The default `ring` crypto provider needs no cmake.

```bash
cd storage

# 1. Build the Rust core (~3 min cold) -> internal/rustcore/lib/linux_amd64/libgcs_core.a
./internal/rustcore/build.sh

# 2. Build: pure Go (unchanged), no-cgo (unchanged), and with the core
go build ./... && CGO_ENABLED=0 go build ./... && go build -tags gcsrust ./...
go vet -tags gcsrust . ./internal/rustcore/...

# 3. Hermetic tests, then tests against real GCS (objects under rustcore/<run-id>/, deleted afterwards)
go test -short -tags gcsrust -run TestRustUnit -v .
RUST_DP_TEST_BUCKET=<your-bucket> go test -short -tags gcsrust -run TestRustCore -v .
RUST_DP_TEST_BUCKET=<your-bucket> go test -short -race -tags gcsrust -run 'TestRustCore|TestRustUnit' -v .
```

Using it from code:

```go
client, err := storage.NewClient(ctx, experimental.WithRustCore())     // JSON; build with -tags gcsrust
client, err := storage.NewGRPCClient(ctx, experimental.WithRustCore()) // gRPC reads and listing
```

### Emulator (storage-testbench)

The control plane goes to the testbench's gRPC port (`STORAGE_EMULATOR_HOST_GRPC`), the data
path to its JSON port. **Uploads from the Rust SDK do not work on the testbench** (multipart
uploads get a 500 because the media part has no Content-Type; resumable PUT bodies hang), so
write tests need real GCS.

## Coverage

* **Served by the core:** bucket create/get/update/delete/list/lock-retention, object
  get/update/delete/list (JSON or gRPC)/compose/rewrite (copy)/move/restore, IAM get/set/test,
  ACL helpers (via bucket/object update, as in the gRPC client), reads over JSON or gRPC (range,
  tail, metadata-only,
  preconditions, CSEK, decompression of whole-object reads, CRC32C, resumption under the
  caller's retry settings), writes (single-shot up to `ChunkSize`, resumable above it,
  `ChunkSize == 0` as an unchunked stream without retries, all attributes incl. predefined
  ACL and contexts, preconditions, CSEK, user MD5/CRC32C), retry policy, `WithMaxAttempts`,
  `WithMaxRetryDuration`, `WithBackoff`, `WithErrorFunc` (evaluated by the core through a
  callback), `userProject`, per-call headers on metadata RPCs, `option.WithEndpoint` (both
  planes), `option.WithoutAuthentication`, any Go credential (token callback),
  `option.WithGRPCConnectionPool` (gRPC connections of the data path; default 1 as in Go's gRPC
  client, instead of the Rust SDK's one per CPU).
* **Rejected explicitly:** object retention on upload (`Unimplemented`, as in Go's gRPC
  client) until the core's write path is verified with it.
* **Not supported (same as Go's gRPC client):** HMAC keys, Pub/Sub notifications,
  `GetServiceAccount` (JSON-only APIs, absent from the v2 protos).
* **Not wired up yet:** `MultiRangeDownloader` (the Rust SDK has `open_object`), appendable
  uploads (the Rust SDK has `open_appendable_object`), `Writer.Flush`.
* **Rust SDK gaps (need upstream changes):** gRPC uploads to standard buckets (only appendable
  objects in zonal buckets, behind an unstable feature), JSON listing (the control plane is
  gRPC-only; the core implements `storage.objects.list` itself), per-call headers on
  reads/writes, `CacheControl` / `LastModified` in JSON read metadata (gRPC reads carry the full
  object), `DisableAutoChecksum`, per-attempt upload timeouts
  (`ChunkRetryDeadline`/`ChunkTransferTimeout`), upload progress (`ProgressFunc` reports
  bytes accepted), HTTP/2 for the JSON data path, DirectPath, `google.rpc.Status` details,
  `x-goog-api-client` attribution, OpenTelemetry integration.

## Callback lifetimes

Values the core calls back into (the token source, `WithErrorFunc` predicates) are passed as
registry IDs, not `cgo.Handle`s. The core releases each ID through `release_cb` after its last
use (token source: when the last clone of the client's credentials is dropped; predicate: when
the operation's retry policies are dropped). A callback for an already-released ID fails
cleanly ("do not retry" / token error), and every exported Go callback recovers panics.

## Design notes

* **Panics.** Every exported Rust function is panic-guarded, and background tasks are wrapped, so a panic becomes an error, never an abort or a lost callback.
* **Handle lifetime.** Handles are `Arc`-backed; Go guards the raw pointer with a mutex, so `Close` racing `Read` is safe.
* **Completions.** Async completions fire exactly once. User data is a `uintptr_t` `cgo.Handle`, never an `unsafe.Pointer`, which keeps `go vet` and checkptr (`-race`) clean.
* **Hot path.** Reads drain a bounded prefetch queue synchronously and only wait asynchronously when the network is behind. Writes copy into a bounded queue in messages of at most 1 MiB, so Rust-side buffering is bounded in bytes.
* **No partial commits.** Uploads finalize only on an explicit Close. Abort, cancellation, or dropping the handle never finalizes a partial object.
* **Errors.** The core fills in both the gRPC code and the HTTP status, so Go errors satisfy both `status.FromError` and `errors.As(*googleapi.Error)` regardless of the transport the core used.
* **SIGPIPE.** Rust binaries ignore SIGPIPE in `main`; a library linked into a Go program does not get that, and TLS/HTTP writes (`writev`) to a connection the peer reset raise SIGPIPE on the writing thread, which the Go runtime turns into process exit for non-Go threads. The core blocks SIGPIPE on all of its threads (and builds clients on them), so such writes fail with EPIPE like any I/O error; the host's signal disposition is untouched (`TestRustUnitSIGPIPEOnCoreThreads`). Every language binding needs this: CPython and the JVM ignore SIGPIPE themselves, Go does not.
