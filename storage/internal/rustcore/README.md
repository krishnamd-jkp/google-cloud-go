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

## The ABI (`gcs_core.h`)

Generic, so it does not change when the storage API grows:

| Entry point | Payload |
|---|---|
| `gcs_call(method, req, opts)` | every unary v2 RPC (17 today); proto3-JSON request -> proto3-JSON response |
| `gcs_read_open` / `gcs_reader_*` | proto3-JSON `ReadObjectRequest`; bytes out |
| `gcs_write_open` / `gcs_writer_*` | `StartResumableWriteRequest`-shaped JSON; bytes in, `Object` out |
| `opts` (JSON) | caller policy only: retry policy/limits/backoff/predicate, userProject, headers, read/write tuning |

FieldMask fields travel as JSON arrays of raw paths (proto3 JSON cannot carry `*` or
`metadata.<key>`).

## Layout

| Path | What |
|---|---|
| `gcs_core/src/lib.rs` | safety contract, status (gRPC + HTTP codes), runtime, client |
| `gcs_core/src/call.rs` | `gcs_call`: dispatch of every unary v2 RPC |
| `gcs_core/src/policy.rs` | per-RPC idempotency rules (decided from the request) |
| `gcs_core/src/options.rs` | options JSON: retry/backoff/predicate, user project, headers |
| `gcs_core/src/reader.rs`, `writer.rs`, `creds.rs` | streaming reads, uploads, credentials bridge |
| `gcs_core.h` | hand-written C header for the ABI |
| `rustcore.go`, `callbacks.go` | generic cgo binding (`Call`, `Reader`, `Writer`, completions, token and retry callbacks) |
| `link_linux_amd64.go` | links `lib/linux_amd64/libgcs_core.a` (one file per supported GOOS/GOARCH) |
| `build.sh` | builds the core and installs it into `lib/<goos>_<goarch>/` (git-ignored) |
| `../../storage_rpc.go` | `storageRPC`: the seam between the Go API mapping and the transport |
| `../../rust_transport.go` | `rustRPC`: one line per RPC, plus generic encode/options/error plumbing |
| `../../rust_client.go` | `rustStorageClient`: construction, reads, writes |
| `../../rust_core_test.go` | tests against real GCS (or the testbench) |

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

# 3. Tests against real GCS (objects under rustcore/<run-id>/, deleted afterwards)
RUST_DP_TEST_BUCKET=<your-bucket> go test -short -tags gcsrust -run TestRustCore -v .
RUST_DP_TEST_BUCKET=<your-bucket> go test -short -race -tags gcsrust -run TestRustCore -v .
```

Using it from code:

```go
client, err := storage.NewClient(ctx, experimental.WithRustCore()) // build with -tags gcsrust
```

### Emulator (storage-testbench)

The control plane goes to the testbench's gRPC port (`STORAGE_EMULATOR_HOST_GRPC`), the data
path to its JSON port. **Uploads from the Rust SDK do not work on the testbench** (multipart
uploads get a 500 because the media part has no Content-Type; resumable PUT bodies hang), so
write tests need real GCS.

## Coverage

* **Served by the core:** bucket create/get/update/delete/list/lock-retention, object
  get/update/delete/list/compose/rewrite (copy)/move/restore, IAM get/set/test, ACL helpers
  (via bucket/object update, as in the gRPC client), reads (range, tail, metadata-only,
  preconditions, CSEK, decompression, CRC32C), writes (single-shot and resumable, all
  attributes incl. retention/contexts, preconditions, CSEK, user MD5/CRC32C), retry policy,
  `WithMaxAttempts`, `WithMaxRetryDuration`, `WithBackoff`, `WithErrorFunc` (evaluated by the
  core through a callback), `userProject`, per-call headers on metadata RPCs, any Go
  credential (token callback).
* **Not supported (same as Go's gRPC client):** HMAC keys, Pub/Sub notifications,
  `GetServiceAccount` (JSON-only APIs, absent from the v2 protos).
* **Not wired up yet:** `MultiRangeDownloader` (the Rust SDK has `open_object`), appendable
  uploads (the Rust SDK has `open_appendable_object`), `Writer.Flush`.
* **Rust SDK gaps found:** per-call headers on reads/writes, `CacheControl`/`LastModified` in
  read metadata, no built-in per-RPC idempotency in the control client (implemented here).

## Design notes

* **Panics.** Every exported Rust function is panic-guarded, and background tasks are wrapped, so a panic becomes an error, never an abort or a lost callback.
* **Handle lifetime.** Handles are `Arc`-backed; Go guards the raw pointer with a mutex, so `Close` racing `Read` is safe.
* **Completions.** Async completions fire exactly once. User data is a `uintptr_t` `cgo.Handle`, never an `unsafe.Pointer`, which keeps `go vet` and checkptr (`-race`) clean.
* **Hot path.** Reads drain a bounded prefetch queue synchronously and only wait asynchronously when the network is behind. Writes copy into a bounded queue in messages of at most 1 MiB, so Rust-side buffering is bounded in bytes.
* **No partial commits.** Uploads finalize only on an explicit Close. Abort, cancellation, or dropping the handle never finalizes a partial object.
* **Errors.** The core fills in both the gRPC code and the HTTP status, so Go errors satisfy both `status.FromError` and `errors.As(*googleapi.Error)` regardless of the transport the core used.
