//! Streaming object writes.
//!
//! Request: proto3-JSON in the shape of `google.storage.v2.StartResumableWriteRequest`:
//! `{"writeObjectSpec": {...}, "commonObjectRequestParams": {...}, "objectChecksums": {...}}`
//! (the resource `Object` carries every attribute; preconditions and predefined ACL are on the
//! spec). Options: retry, `chunkSize`, `userProject`.
//!
//! * **No phantom commits.** The upload source only reports end-of-data when it receives an
//!   explicit `Finish` message (sent by `gcs_writer_close`). If the channel is dropped without
//!   it (abort, handle freed, host gone) the source returns an *error*, so the SDK abandons the
//!   upload instead of finalizing a truncated object. `gcs_writer_abort` also aborts the task.
//! * **No leaks on cancellation.** Abort tears the upload task down; pending writes complete.
//! * **Single-shot vs resumable decided here.** Data is collected (as `Bytes`, no
//!   concatenation) until `chunkSize` bytes arrive or the writer is closed; then the SDK is
//!   invoked with an exact size (single-shot) or unknown size (resumable).
//! * **Full fidelity.** All object attributes, preconditions, CSEK, user-supplied MD5/CRC32C,
//!   retry policy and backoff are forwarded.
//!
//! **Appendable uploads** (zonal buckets, `BidiWriteObject`): the request is then in the shape of
//! the first `google.storage.v2.BidiWriteObjectRequest`, either `writeObjectSpec` with
//! `appendable: true` (a new object, created on the first write, flush or close) or
//! `appendObjectSpec` (take over an unfinalized object; reopened right away so that
//! `gcs_writer_open_state` can report where to continue). `gcs_writer_flush` persists what was
//! written so far; `gcs_writer_close` finalizes the object or, without the `finalizeOnClose`
//! option, leaves it unfinalized. Served by the SDK's `open_appendable_object` /
//! `reopen_appendable_object`, which also handle redirects, reconnects and the running CRC32C.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use google_cloud_storage::model;
use google_cloud_storage::model_ext::KeyAes256;
use google_cloud_storage::streaming_source::{SizeHint, StreamingSource};
use serde::Deserialize;
use tokio::sync::{mpsc, oneshot, watch};
use tokio::task::AbortHandle;

use crate::call::{bytes as ffi_bytes, to_json};
use crate::options::{CallOptions, apply_data_opts};
use crate::{Completion, GcsClient, GcsDoneCb, GcsStatus, GoBuf, Status, bucket_name, guard_rc, guard_void, lock, policy, rt};

/// Writes up to this size are copied synchronously inside the FFI call when the queue has room.
const SYNC_COPY_MAX: usize = 1 << 20;
/// Queue depth between the host and the upload task (messages; hosts coalesce small writes).
const QUEUE_DEPTH: usize = 8;
/// Default resumable chunk size when the caller does not set one.
const DEFAULT_CHUNK: usize = 16 << 20;
/// Smallest resumable chunk the service accepts; used for `ChunkSize == 0` streams.
const MIN_CHUNK: usize = 256 << 10;

#[derive(Deserialize, Default)]
#[serde(default)]
struct StartWrite {
    #[serde(rename = "writeObjectSpec", alias = "write_object_spec")]
    write_object_spec: Option<model::WriteObjectSpec>,
    #[serde(rename = "commonObjectRequestParams", alias = "common_object_request_params")]
    common_object_request_params: Option<model::CommonObjectRequestParams>,
    #[serde(rename = "objectChecksums", alias = "object_checksums")]
    object_checksums: Option<model::ObjectChecksums>,
    /// Appendable uploads: take over this unfinalized object instead of creating one.
    #[serde(rename = "appendObjectSpec", alias = "append_object_spec")]
    append_object_spec: Option<AppendSpec>,
}

/// The fields of `google.storage.v2.AppendObjectSpec` a takeover uses (the SDK's public model
/// has no such type). proto3 JSON carries int64 values as strings.
#[derive(Deserialize, Default)]
#[serde(default, rename_all = "camelCase")]
struct AppendSpec {
    bucket: String,
    object: String,
    #[serde(deserialize_with = "de_i64")]
    generation: i64,
    #[serde(deserialize_with = "de_opt_i64")]
    if_metageneration_match: Option<i64>,
    #[serde(deserialize_with = "de_opt_i64")]
    if_metageneration_not_match: Option<i64>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum JsonI64 {
    Num(i64),
    Str(String),
}

impl JsonI64 {
    fn get<E: serde::de::Error>(self) -> Result<i64, E> {
        match self {
            JsonI64::Num(n) => Ok(n),
            JsonI64::Str(s) => s.parse().map_err(E::custom),
        }
    }
}

fn de_i64<'de, D: serde::Deserializer<'de>>(d: D) -> Result<i64, D::Error> {
    JsonI64::deserialize(d)?.get()
}

fn de_opt_i64<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<i64>, D::Error> {
    Option::<JsonI64>::deserialize(d)?.map(JsonI64::get).transpose()
}

enum WMsg {
    Data(Bytes),
    /// Appendable uploads: persist everything queued before this; reply = persisted size.
    Flush(oneshot::Sender<Result<i64, Status>>),
    Finish,
}

type UploadResult = Result<String, Status>;

/// An appendable upload's stream, once open.
#[derive(Clone, Copy)]
struct OpenInfo {
    generation: i64,
    persisted_size: i64,
}

type OpenState = Option<Result<OpenInfo, Status>>;

struct WInner {
    tx: Mutex<Option<mpsc::Sender<WMsg>>>,
    upload: Mutex<Option<AbortHandle>>,
    result: watch::Receiver<Option<UploadResult>>,
    accepted: AtomicU64,
    /// Appendable uploads only: set once the stream is open (or failed to open).
    opened: Option<watch::Receiver<OpenState>>,
}

pub struct GcsWriter {
    inner: Arc<WInner>,
}

impl WInner {
    /// The upload's terminal status (waits for it). Used to report the *cause* when a write
    /// fails because the upload task already ended.
    async fn terminal_status(&self) -> Status {
        let mut rx = self.result.clone();
        match rx.wait_for(|v| v.is_some()).await {
            Ok(v) => match v.as_ref() {
                Some(Err(s)) => s.clone(),
                _ => Status::err("upload already finished"),
            },
            Err(_) => Status::cancelled("upload aborted"),
        }
    }
}

/// Upload payload: buffered head first, then live data; EOF only on an explicit Finish.
struct ChanSource {
    head: VecDeque<Bytes>,
    rx: Option<mpsc::Receiver<WMsg>>,
    exact: Option<u64>,
    finished: bool,
}

impl StreamingSource for ChanSource {
    type Error = std::io::Error;

    async fn next(&mut self) -> Option<Result<Bytes, Self::Error>> {
        if let Some(b) = self.head.pop_front() {
            return Some(Ok(b));
        }
        if self.finished {
            return None;
        }
        let rx = self.rx.as_mut()?;
        loop {
            match rx.recv().await {
                Some(WMsg::Data(b)) => return Some(Ok(b)),
                Some(WMsg::Finish) => {
                    self.finished = true;
                    return None;
                }
                Some(WMsg::Flush(reply)) => reject_flush(reply),
                // Sender dropped without Finish: never let the SDK finalize a partial object.
                None => return Some(Err(std::io::Error::other("upload abandoned before Close; object not finalized"))),
            }
        }
    }

    async fn size_hint(&self) -> Result<SizeHint, Self::Error> {
        Ok(match self.exact {
            Some(n) => SizeHint::with_exact(n),
            None => SizeHint::new(),
        })
    }
}

struct Plan {
    client: GcsClient,
    spec: model::WriteObjectSpec,
    key: Option<KeyAes256>,
    checksums: Option<model::ObjectChecksums>,
    chunk_size: usize,
    no_retry: bool,
    opts: CallOptions,
}

async fn upload_task(plan: Plan, mut rx: mpsc::Receiver<WMsg>, result_tx: watch::Sender<Option<UploadResult>>) {
    let r = run_upload(plan, &mut rx).await;
    // Ignore send errors: nobody may be waiting any more.
    let _ = result_tx.send(Some(r));
}

async fn run_upload(plan: Plan, rx: &mut mpsc::Receiver<WMsg>) -> UploadResult {
    // Phase 1: content of at most `chunk_size` bytes is sent single-shot with an exact size
    // (the same rule as storage.Writer); collect until that is decided.
    let mut head = VecDeque::new();
    let mut head_len: usize = 0;
    let mut finished = false;
    while head_len <= plan.chunk_size {
        match rx.recv().await {
            Some(WMsg::Data(b)) => {
                head_len += b.len();
                head.push_back(b);
            }
            Some(WMsg::Finish) => {
                finished = true;
                break;
            }
            Some(WMsg::Flush(reply)) => reject_flush(reply),
            None => return Err(Status::cancelled("upload abandoned before Close; object not finalized")),
        }
    }
    // Hand the receiver to the source for the streaming (resumable) phase.
    let (live_rx, exact) = if finished {
        (None, Some(head_len as u64))
    } else {
        let (tx2, rx2) = mpsc::channel(1);
        drop(tx2);
        (Some(std::mem::replace(rx, rx2)), None)
    };
    let src = ChanSource { head, rx: live_rx, exact, finished };

    let Plan { client, spec, key, checksums, chunk_size, no_retry, opts } = plan;
    let idempotent = opts.idempotent(policy::write_object(&spec));
    let o = spec.resource.unwrap_or_default();
    let mut req = client.storage.write_object(bucket_name(&o.bucket), o.name, src);
    if !o.content_type.is_empty() {
        req = req.set_content_type(o.content_type);
    }
    if !o.content_encoding.is_empty() {
        req = req.set_content_encoding(o.content_encoding);
    }
    if !o.content_language.is_empty() {
        req = req.set_content_language(o.content_language);
    }
    if !o.content_disposition.is_empty() {
        req = req.set_content_disposition(o.content_disposition);
    }
    if !o.cache_control.is_empty() {
        req = req.set_cache_control(o.cache_control);
    }
    if !o.storage_class.is_empty() {
        req = req.set_storage_class(o.storage_class);
    }
    if !o.kms_key.is_empty() {
        req = req.set_kms_key(o.kms_key);
    }
    if !o.metadata.is_empty() {
        req = req.set_metadata(o.metadata);
    }
    if let Some(t) = o.custom_time {
        req = req.set_custom_time(t);
    }
    if o.temporary_hold {
        req = req.set_temporary_hold(true);
    }
    if let Some(h) = o.event_based_hold {
        req = req.set_event_based_hold(h);
    }
    if !o.acl.is_empty() {
        req = req.set_acl(o.acl);
    }
    if let Some(r) = o.retention {
        req = req.set_retention(r);
    }
    if let Some(cx) = o.contexts {
        req = req.set_contexts(cx);
    }
    if !spec.predefined_acl.is_empty() {
        req = req.set_predefined_acl(spec.predefined_acl);
    }
    if let Some(v) = spec.if_generation_match {
        req = req.set_if_generation_match(v);
    }
    if let Some(v) = spec.if_generation_not_match {
        req = req.set_if_generation_not_match(v);
    }
    if let Some(v) = spec.if_metageneration_match {
        req = req.set_if_metageneration_match(v);
    }
    if let Some(v) = spec.if_metageneration_not_match {
        req = req.set_if_metageneration_not_match(v);
    }
    if let Some(k) = key {
        req = req.set_key(k);
    }
    if let Some(c) = checksums {
        if let Some(crc) = c.crc32c {
            req = req.with_known_crc32c(crc);
        }
        if !c.md5_hash.is_empty() {
            req = req.with_known_md5_hash(c.md5_hash.to_vec());
        }
    }
    // threshold = chunk_size + 1: an exact size <= chunk_size is uploaded single-shot.
    req = req
        .with_resumable_upload_threshold(chunk_size.saturating_add(1))
        .with_resumable_upload_buffer_size(chunk_size)
        .with_idempotency(idempotent);
    let mut req = apply_data_opts!(req, &opts, &client);
    if no_retry {
        // ChunkSize == 0: a single, unbuffered stream that is never retried.
        req = req.with_retry_policy(google_cloud_gax::retry_policy::NeverRetry);
    }
    let obj = req.send_buffered().await.map_err(|e| Status::from_sdk(&e))?;
    to_json(&obj)
}

/// Replies to a flush on an upload that cannot flush (the host checks first, so this is
/// defensive).
fn reject_flush(reply: oneshot::Sender<Result<i64, Status>>) {
    let _ = reply.send(Err(Status::invalid("Flush is only supported for appendable uploads")));
}

/// What an appendable upload writes to.
enum AppendTarget {
    /// A new appendable object (`writeObjectSpec.appendable`).
    New(model::WriteObjectSpec),
    /// An existing unfinalized appendable object (`appendObjectSpec`).
    Takeover(AppendSpec),
}

struct AppendPlan {
    client: GcsClient,
    target: AppendTarget,
    finalize_on_close: bool,
    opts: CallOptions,
}

async fn append_task(
    plan: AppendPlan,
    mut rx: mpsc::Receiver<WMsg>,
    result_tx: watch::Sender<Option<UploadResult>>,
    opened_tx: watch::Sender<OpenState>,
) {
    let r = run_append(plan, &mut rx, &opened_tx).await;
    let unopened = opened_tx.borrow().is_none();
    if unopened {
        // Failed before the stream opened: wake whoever waits for the open (a takeover).
        let st = match &r {
            Err(s) => s.clone(),
            Ok(_) => Status::err("upload ended before its stream opened"),
        };
        let _ = opened_tx.send(Some(Err(st)));
    }
    // Ignore send errors: nobody may be waiting any more.
    let _ = result_tx.send(Some(r));
}

async fn run_append(plan: AppendPlan, rx: &mut mpsc::Receiver<WMsg>, opened: &watch::Sender<OpenState>) -> UploadResult {
    let AppendPlan { client, target, finalize_on_close, opts } = plan;
    // A message that arrived before the stream was open (a new object opens on the first one).
    let mut pending = None;
    // Describes the object for an unfinalized close: the SDK then reports only its generation
    // and persisted size (finalizing returns the full resource).
    let (mut writer, mut unfinalized) = match target {
        AppendTarget::Takeover(a) => {
            let mut b = client.storage.reopen_appendable_object(bucket_name(&a.bucket), a.object.clone(), a.generation);
            if let Some(v) = a.if_metageneration_match {
                b = b.set_if_metageneration_match(v);
            }
            if let Some(v) = a.if_metageneration_not_match {
                b = b.set_if_metageneration_not_match(v);
            }
            let b = apply_data_opts!(b, &opts, &client);
            let w = b.send().await.map_err(|e| Status::from_sdk(&e))?;
            let mut o = model::Object::default();
            o.bucket = bucket_name(&a.bucket);
            o.name = a.object;
            (w, o)
        }
        AppendTarget::New(spec) => {
            let o = spec.resource.unwrap_or_default();
            let mut b = client.storage.open_appendable_object(bucket_name(&o.bucket), o.name.clone());
            if !o.content_type.is_empty() {
                b = b.set_content_type(o.content_type.clone());
            }
            if !o.content_encoding.is_empty() {
                b = b.set_content_encoding(o.content_encoding.clone());
            }
            if !o.content_language.is_empty() {
                b = b.set_content_language(o.content_language.clone());
            }
            if !o.content_disposition.is_empty() {
                b = b.set_content_disposition(o.content_disposition.clone());
            }
            if !o.cache_control.is_empty() {
                b = b.set_cache_control(o.cache_control.clone());
            }
            if !o.kms_key.is_empty() {
                b = b.set_kms_key(o.kms_key.clone());
            }
            if !o.metadata.is_empty() {
                b = b.set_metadata(o.metadata.clone());
            }
            if let Some(t) = o.custom_time.clone() {
                b = b.set_custom_time(t);
            }
            if let Some(cx) = o.contexts.clone() {
                b = b.set_contexts(cx);
            }
            if let Some(v) = spec.if_generation_match {
                b = b.set_if_generation_match(v);
            }
            if let Some(v) = spec.if_generation_not_match {
                b = b.set_if_generation_not_match(v);
            }
            if let Some(v) = spec.if_metageneration_match {
                b = b.set_if_metageneration_match(v);
            }
            if let Some(v) = spec.if_metageneration_not_match {
                b = b.set_if_metageneration_not_match(v);
            }
            let b = apply_data_opts!(b, &opts, &client);
            // Created on the first message: the first data rides on the opening request.
            let w = match rx.recv().await {
                Some(WMsg::Data(first)) => b.send_and_append(first).await,
                Some(m) => {
                    pending = Some(m);
                    b.send().await
                }
                None => return Err(Status::cancelled("upload abandoned before Close; object not created")),
            }
            .map_err(|e| Status::from_sdk(&e))?;
            let mut u = o;
            u.bucket = bucket_name(&u.bucket);
            u.checksums = None; // caller-supplied values, not the object's
            (w, u)
        }
    };
    let _ = opened.send(Some(Ok(OpenInfo { generation: writer.generation(), persisted_size: writer.persisted_size() })));
    loop {
        let msg = match pending.take() {
            Some(m) => Some(m),
            None => rx.recv().await,
        };
        match msg {
            Some(WMsg::Data(b)) => writer.append(b).await.map_err(|e| Status::from_sdk(&e))?,
            Some(WMsg::Flush(reply)) => {
                let r = writer.flush().await.map_err(|e| Status::from_sdk(&e));
                let _ = reply.send(r.clone());
                r?;
            }
            Some(WMsg::Finish) => {
                if finalize_on_close {
                    let obj = writer.finalize().await.map_err(|e| Status::from_sdk(&e))?;
                    return to_json(&obj);
                }
                unfinalized.generation = writer.generation();
                unfinalized.size = writer.close().await.map_err(|e| Status::from_sdk(&e))?;
                return to_json(&unfinalized);
            }
            // Dropped without Close (abort): nothing is finalized; flushed bytes stay in the
            // unfinalized object, as with Go's own gRPC writer.
            None => return Err(Status::cancelled("upload aborted before Close; object not finalized")),
        }
    }
}

/// Opens an appendable upload (see the module docs).
fn open_append(client: &GcsClient, sw: StartWrite, opts: CallOptions) -> Result<*mut GcsWriter, Status> {
    if sw.common_object_request_params.as_ref().is_some_and(|p| !p.encryption_key_bytes.is_empty()) {
        return Err(Status::unimplemented(
            "appendable uploads cannot use customer-supplied encryption keys (not supported by the Rust SDK)",
        ));
    }
    let target = match (sw.append_object_spec, sw.write_object_spec) {
        (Some(a), _) => {
            if a.bucket.is_empty() || a.object.is_empty() || a.generation <= 0 {
                return Err(Status::invalid("appendObjectSpec.bucket, object and generation are required"));
            }
            AppendTarget::Takeover(a)
        }
        (None, Some(spec)) => {
            let o = spec.resource.as_ref().ok_or_else(|| Status::invalid("writeObjectSpec.resource is required"))?;
            if o.bucket.is_empty() || o.name.is_empty() {
                return Err(Status::invalid("resource.bucket and resource.name are required"));
            }
            // Attributes the SDK's appendable uploads cannot set: refuse instead of dropping them.
            let unsupported = [
                (!o.acl.is_empty(), "ACLs"),
                (!spec.predefined_acl.is_empty(), "a predefined ACL"),
                (o.temporary_hold || o.event_based_hold == Some(true), "object holds"),
                (o.retention.is_some(), "object retention"),
                (!o.storage_class.is_empty() && !o.storage_class.eq_ignore_ascii_case("RAPID"), "a storage class other than RAPID"),
            ];
            if let Some((_, what)) = unsupported.iter().find(|(set, _)| *set) {
                return Err(Status::unimplemented(format!("appendable uploads cannot set {what} (not supported by the Rust SDK)")));
            }
            AppendTarget::New(spec)
        }
        (None, None) => return Err(Status::invalid("writeObjectSpec or appendObjectSpec is required")),
    };
    let plan = AppendPlan { client: client.clone(), target, finalize_on_close: opts.finalize_on_close, opts };
    let (tx, rx) = mpsc::channel::<WMsg>(QUEUE_DEPTH);
    let (result_tx, result_rx) = watch::channel(None);
    let (opened_tx, opened_rx) = watch::channel(None);
    let task = rt().spawn(append_task(plan, rx, result_tx, opened_tx));
    let inner = Arc::new(WInner {
        tx: Mutex::new(Some(tx)),
        upload: Mutex::new(Some(task.abort_handle())),
        result: result_rx,
        accepted: AtomicU64::new(0),
        opened: Some(opened_rx),
    });
    Ok(Box::into_raw(Box::new(GcsWriter { inner })))
}

fn open_impl(client: &GcsClient, req: &[u8], mut opts: CallOptions) -> Result<*mut GcsWriter, Status> {
    opts.bind(client);
    let sw: StartWrite = serde_json::from_slice(req).map_err(|e| Status::invalid(format!("StartResumableWriteRequest: {e}")))?;
    let appendable =
        sw.append_object_spec.is_some() || sw.write_object_spec.as_ref().is_some_and(|s| s.appendable == Some(true));
    if appendable {
        return open_append(client, sw, opts);
    }
    let spec = sw.write_object_spec.ok_or_else(|| Status::invalid("writeObjectSpec is required"))?;
    let res = spec.resource.as_ref().ok_or_else(|| Status::invalid("writeObjectSpec.resource is required"))?;
    if res.bucket.is_empty() || res.name.is_empty() {
        return Err(Status::invalid("resource.bucket and resource.name are required"));
    }
    let key = match &sw.common_object_request_params {
        Some(p) if !p.encryption_key_bytes.is_empty() => {
            Some(KeyAes256::new(&p.encryption_key_bytes).map_err(|e| Status::invalid(format!("encryption key: {e}")))?)
        }
        _ => None,
    };
    // Absent: default chunk. 0: no chunking (minimum resumable buffer, no retries).
    let (chunk_size, no_retry) = match opts.chunk_size {
        None => (DEFAULT_CHUNK, false),
        Some(0) => (MIN_CHUNK, true),
        Some(n) => (n, false),
    };
    let plan = Plan { client: client.clone(), spec, key, checksums: sw.object_checksums, chunk_size, no_retry, opts };
    let (tx, rx) = mpsc::channel::<WMsg>(QUEUE_DEPTH);
    let (result_tx, result_rx) = watch::channel(None);
    let task = rt().spawn(upload_task(plan, rx, result_tx));
    let inner = Arc::new(WInner {
        tx: Mutex::new(Some(tx)),
        upload: Mutex::new(Some(task.abort_handle())),
        result: result_rx,
        accepted: AtomicU64::new(0),
        opened: None,
    });
    Ok(Box::into_raw(Box::new(GcsWriter { inner })))
}

/// Opens a writer. Nothing is sent until data arrives or the writer is closed (except for
/// appendable takeovers, which reopen the object right away).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_write_open(
    c: *const GcsClient,
    req: *const u8,
    req_len: usize,
    opts: *const u8,
    opts_len: usize,
    out: *mut *mut GcsWriter,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if c.is_null() || out.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let w = open_impl(unsafe { &*c }, unsafe { ffi_bytes(req, req_len) }, CallOptions::parse(opts, opts_len)?)?;
        unsafe { *out = w };
        Ok(0)
    })
}

/// Queues `data` synchronously when possible, otherwise on a runtime thread (completes `done`).
fn write_impl(w: &GcsWriter, data: *const u8, len: usize, done: Completion) -> Result<i32, Status> {
    if len == 0 {
        return Ok(0);
    }
    let inner = w.inner.clone();
    let tx = match lock(&inner.tx).as_ref() {
        Some(tx) => tx.clone(),
        None => return Err(Status::invalid("storage: Writer is closed")),
    };
    if len <= SYNC_COPY_MAX {
        if let Ok(permit) = tx.try_reserve() {
            let b = Bytes::copy_from_slice(unsafe { std::slice::from_raw_parts(data, len) });
            permit.send(WMsg::Data(b));
            inner.accepted.fetch_add(len as u64, Ordering::Relaxed);
            return Ok(0);
        }
    }
    // Slow path: copy on a runtime thread (keeps long memcpys out of the FFI call, which in Go
    // otherwise makes the scheduler hand off Ps and spawn threads) and wait for room.
    let buf = GoBuf { ptr: data as *mut u8, len };
    rt().spawn(async move {
        // Copy first: the borrowed host buffer is released when the completion fires below.
        let b = Bytes::copy_from_slice(unsafe { buf.as_slice() });
        match tx.send(WMsg::Data(b)).await {
            Ok(()) => {
                inner.accepted.fetch_add(len as u64, Ordering::Relaxed);
                done.complete(Status::ok(), len as u64);
            }
            Err(_) => done.complete(inner.terminal_status().await, 0),
        }
    });
    Ok(1)
}

/// Queues `data`. Returns 0 when the bytes were copied synchronously (the caller may reuse the
/// buffer immediately), 1 when pending (`data` must stay pinned until the callback, which
/// reports `n` = bytes accepted), -1 on failure.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_write(
    w: *const GcsWriter,
    data: *const u8,
    len: usize,
    cb: GcsDoneCb,
    user: usize,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if w.is_null() || (data.is_null() && len > 0) {
            return Err(Status::invalid("null argument"));
        }
        write_impl(unsafe { &*w }, data, len, Completion::new(cb, user))
    })
}

/// Bytes accepted so far (for progress reporting).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_accepted(w: *const GcsWriter) -> u64 {
    if w.is_null() {
        return 0;
    }
    unsafe { &*w }.inner.accepted.load(Ordering::Relaxed)
}

fn close_impl(w: &GcsWriter, done: Completion) -> Result<i32, Status> {
    let inner = w.inner.clone();
    let Some(tx) = lock(&inner.tx).take() else {
        return Err(Status::invalid("storage: Writer is closed"));
    };
    rt().spawn(async move {
        // If this fails the upload already ended; its result explains why.
        let _ = tx.send(WMsg::Finish).await;
        drop(tx);
        let mut rx = inner.result.clone();
        let (st, n) = match rx.wait_for(|v| v.is_some()).await {
            Ok(v) => match v.as_ref() {
                Some(Ok(js)) => match std::ffi::CString::new(js.as_str()) {
                    Ok(cs) => (Status::ok(), cs.into_raw() as u64),
                    Err(e) => (Status::err(e.to_string()), 0),
                },
                Some(Err(s)) => (s.clone(), 0),
                None => (Status::err("upload ended without a result"), 0),
            },
            Err(_) => (Status::cancelled("upload aborted"), 0),
        };
        done.complete(st, n);
    });
    Ok(1)
}

/// Finalizes the upload. Always pending (returns 1) unless the writer was already closed or
/// aborted (-1). The callback reports `n` = pointer to a Rust-owned C string with the created
/// object as proto3 JSON of google.storage.v2.Object (free with gcs_string_free).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_close(w: *const GcsWriter, cb: GcsDoneCb, user: usize, out_status: *mut GcsStatus) -> i32 {
    guard_rc(out_status, || {
        if w.is_null() {
            return Err(Status::invalid("null argument"));
        }
        close_impl(unsafe { &*w }, Completion::new(cb, user))
    })
}

fn flush_impl(w: &GcsWriter, done: Completion) -> Result<i32, Status> {
    let inner = w.inner.clone();
    if inner.opened.is_none() {
        return Err(Status::invalid("Flush is only supported for appendable uploads"));
    }
    let tx = match lock(&inner.tx).as_ref() {
        Some(tx) => tx.clone(),
        None => return Err(Status::invalid("storage: Writer is closed")),
    };
    rt().spawn(async move {
        let (reply_tx, reply_rx) = oneshot::channel();
        if tx.send(WMsg::Flush(reply_tx)).await.is_err() {
            return done.complete(inner.terminal_status().await, 0);
        }
        drop(tx);
        match reply_rx.await {
            Ok(Ok(n)) => done.complete(Status::ok(), n.max(0) as u64),
            Ok(Err(s)) => done.complete(s, 0),
            Err(_) => done.complete(inner.terminal_status().await, 0),
        }
    });
    Ok(1)
}

/// Appendable uploads: persists everything written so far (opening the stream first if
/// needed). Always pending (returns 1) or -1; the callback reports `n` = the persisted size.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_flush(w: *const GcsWriter, cb: GcsDoneCb, user: usize, out_status: *mut GcsStatus) -> i32 {
    guard_rc(out_status, || {
        if w.is_null() {
            return Err(Status::invalid("null argument"));
        }
        flush_impl(unsafe { &*w }, Completion::new(cb, user))
    })
}

fn open_state_impl(w: &GcsWriter, done: Completion) -> Result<i32, Status> {
    let Some(mut rx) = w.inner.opened.clone() else {
        return Err(Status::invalid("not an appendable upload"));
    };
    rt().spawn(async move {
        let r = match rx.wait_for(|v| v.is_some()).await {
            Ok(v) => (*v).clone().unwrap_or_else(|| Err(Status::err("no open state"))),
            Err(_) => Err(Status::cancelled("upload aborted")),
        };
        match r {
            Ok(info) => done.complete(Status::ok(), info.persisted_size.max(0) as u64),
            Err(s) => done.complete(s, 0),
        }
    });
    Ok(1)
}

/// Appendable uploads: completes once the stream is open, with `n` = the persisted size, i.e.
/// the offset a takeover continues from (or the error that prevented the open). Always
/// pending (returns 1) or -1.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_open_state(
    w: *const GcsWriter,
    cb: GcsDoneCb,
    user: usize,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if w.is_null() {
            return Err(Status::invalid("null argument"));
        }
        open_state_impl(unsafe { &*w }, Completion::new(cb, user))
    })
}

/// Appendable uploads: the object's generation once the stream is open; 0 before (or for
/// other uploads).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_generation(w: *const GcsWriter) -> i64 {
    if w.is_null() {
        return 0;
    }
    let Some(rx) = unsafe { &*w }.inner.opened.as_ref() else {
        return 0;
    };
    match &*rx.borrow() {
        Some(Ok(info)) => info.generation,
        _ => 0,
    }
}

/// Aborts the upload without finalizing it (cancellation / CloseWithError). Idempotent.
/// Pending writes and a pending close complete with an error.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_abort(w: *const GcsWriter) {
    guard_void(|| {
        if w.is_null() {
            return;
        }
        let inner = unsafe { &*w }.inner.clone();
        lock(&inner.tx).take();
        if let Some(h) = lock(&inner.upload).take() {
            h.abort();
        }
    });
}

/// Aborts (if still running) and releases the handle.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_free(w: *mut GcsWriter) {
    guard_void(|| {
        if w.is_null() {
            return;
        }
        unsafe { gcs_writer_abort(w) };
        drop(unsafe { Box::from_raw(w) });
    });
}
