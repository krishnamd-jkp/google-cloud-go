//! Streaming object writes.
//!
//! Fixes relative to the POC:
//! * **No phantom commits.** The upload source only reports end-of-data when it receives an
//!   explicit `Finish` message (sent by `gcs_writer_close`). If the channel is dropped without
//!   it (abort, handle freed, Go side gone) the source returns an *error*, so the SDK abandons
//!   the upload instead of finalizing a truncated object. `gcs_writer_abort` additionally
//!   aborts the upload task.
//! * **No leaks on cancellation.** Abort tears the upload task down; pending writes complete.
//! * **Single-shot vs resumable decided in Rust, without an extra Go-side copy.** Data is
//!   collected (as `Bytes`, no concatenation) until `chunk_size` bytes arrive or the writer is
//!   closed; then the SDK is invoked with an exact size (single-shot) or unknown size
//!   (resumable) — the same decision `storage.Writer` makes.
//! * **Full fidelity.** All object attributes, all four preconditions, CSEK, user-supplied
//!   MD5/CRC32C, retry policy and backoff are forwarded instead of being silently dropped.
//! * **Structured errors.** The upload result keeps HTTP status / rpc code / headers (the POC
//!   stringified them, which is why a 412 reached Go as "code=1 http=0").

use std::collections::VecDeque;
use std::ffi::c_char;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use google_cloud_storage::model::ObjectAccessControl;
use google_cloud_storage::model_ext::KeyAes256;
use google_cloud_storage::streaming_source::{SizeHint, StreamingSource};
use tokio::sync::{mpsc, watch};
use tokio::task::AbortHandle;

use crate::{
    Completion, GCS_COND_GENERATION_MATCH, GCS_COND_GENERATION_NOT_MATCH, GCS_COND_METAGENERATION_MATCH,
    GCS_COND_METAGENERATION_NOT_MATCH, GCS_RETRY_ALWAYS, GcsClient, GcsDoneCb, GcsRetry, GcsStatus, GoBuf, Status,
    bucket_name, guard_rc, guard_void, lock, opt_str, req_str, rt,
};

/// Writes up to this size are copied synchronously inside the cgo call when the queue has room.
const SYNC_COPY_MAX: usize = 1 << 20;
/// Queue depth between Go and the upload task (messages; Go coalesces small writes).
const QUEUE_DEPTH: usize = 8;

#[repr(C)]
pub struct GcsWriteParams {
    pub bucket: *const c_char,
    pub object: *const c_char,
    /// JSON object with optional fields: contentType, contentEncoding, contentLanguage,
    /// contentDisposition, cacheControl, storageClass, kmsKeyName, predefinedAcl, metadata{},
    /// customTimeUnixNanos, temporaryHold, eventBasedHold, md5 (base64), crc32c (number,
    /// sent only when sendCrc32c is true), acl [{entity, role}].
    pub attrs_json: *const c_char,
    pub cond_flags: u32,
    pub if_generation_match: i64,
    pub if_generation_not_match: i64,
    pub if_metageneration_match: i64,
    pub if_metageneration_not_match: i64,
    pub encryption_key: *const u8,
    pub encryption_key_len: usize,
    /// Resumable chunk size; must be > 0 (Go's ChunkSize == 0 is served by the Go path).
    pub chunk_size: usize,
    pub retry: GcsRetry,
}

enum WMsg {
    Data(Bytes),
    Finish,
}

type UploadResult = Result<String, Status>;

struct WInner {
    tx: Mutex<Option<mpsc::Sender<WMsg>>>,
    upload: Mutex<Option<AbortHandle>>,
    result: watch::Receiver<Option<UploadResult>>,
    accepted: AtomicU64,
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
        match rx.recv().await {
            Some(WMsg::Data(b)) => Some(Ok(b)),
            Some(WMsg::Finish) => {
                self.finished = true;
                None
            }
            // Sender dropped without Finish: never let the SDK finalize a partial object.
            None => Some(Err(std::io::Error::other("upload abandoned before Close; object not finalized"))),
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
    storage: google_cloud_storage::client::Storage,
    bucket: String,
    object: String,
    attrs: serde_json::Value,
    p_cond_flags: u32,
    conds: [i64; 4],
    key: Option<KeyAes256>,
    chunk_size: usize,
    retry: GcsRetry,
    user_agent: Option<String>,
    quota_project: Option<String>,
}

async fn upload_task(plan: Plan, mut rx: mpsc::Receiver<WMsg>, result_tx: watch::Sender<Option<UploadResult>>) {
    let r = run_upload(plan, &mut rx).await;
    // Ignore send errors: nobody may be waiting any more.
    let _ = result_tx.send(Some(r));
}

async fn run_upload(plan: Plan, rx: &mut mpsc::Receiver<WMsg>) -> UploadResult {
    // Phase 1: decide single-shot vs resumable like storage.Writer (content that ends within
    // the first chunk is sent with an exact size).
    let mut head = VecDeque::new();
    let mut head_len: usize = 0;
    let mut finished = false;
    while head_len < plan.chunk_size {
        match rx.recv().await {
            Some(WMsg::Data(b)) => {
                head_len += b.len();
                head.push_back(b);
            }
            Some(WMsg::Finish) => {
                finished = true;
                break;
            }
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

    let mut req = plan.storage.write_object(bucket_name(&plan.bucket), plan.object.clone(), src);
    let a = &plan.attrs;
    let s = |k: &str| a.get(k).and_then(|v| v.as_str()).filter(|v| !v.is_empty()).map(str::to_string);
    if let Some(v) = s("contentType") {
        req = req.set_content_type(v);
    }
    if let Some(v) = s("contentEncoding") {
        req = req.set_content_encoding(v);
    }
    if let Some(v) = s("contentLanguage") {
        req = req.set_content_language(v);
    }
    if let Some(v) = s("contentDisposition") {
        req = req.set_content_disposition(v);
    }
    if let Some(v) = s("cacheControl") {
        req = req.set_cache_control(v);
    }
    if let Some(v) = s("storageClass") {
        req = req.set_storage_class(v);
    }
    if let Some(v) = s("kmsKeyName") {
        req = req.set_kms_key(v);
    }
    if let Some(v) = s("predefinedAcl") {
        req = req.set_predefined_acl(v);
    }
    if let Some(m) = a.get("metadata").and_then(|m| m.as_object()) {
        let kv: Vec<(String, String)> =
            m.iter().map(|(k, v)| (k.clone(), v.as_str().unwrap_or_default().to_string())).collect();
        if !kv.is_empty() {
            req = req.set_metadata(kv);
        }
    }
    if let Some(ns) = a.get("customTimeUnixNanos").and_then(|v| v.as_i64()) {
        req = req.set_custom_time(google_cloud_wkt::Timestamp::clamp(ns.div_euclid(1_000_000_000), ns.rem_euclid(1_000_000_000) as i32));
    }
    if a.get("temporaryHold").and_then(|v| v.as_bool()) == Some(true) {
        req = req.set_temporary_hold(true);
    }
    if a.get("eventBasedHold").and_then(|v| v.as_bool()) == Some(true) {
        req = req.set_event_based_hold(true);
    }
    if let Some(acl) = a.get("acl").and_then(|v| v.as_array()) {
        let rules: Vec<ObjectAccessControl> = acl
            .iter()
            .map(|r| {
                ObjectAccessControl::new()
                    .set_entity(r.get("entity").and_then(|v| v.as_str()).unwrap_or_default())
                    .set_role(r.get("role").and_then(|v| v.as_str()).unwrap_or_default())
            })
            .collect();
        if !rules.is_empty() {
            req = req.set_acl(rules);
        }
    }
    if let Some(md5) = s("md5") {
        use base64_lite::decode;
        let raw = decode(&md5).ok_or_else(|| Status::invalid("md5 is not valid base64"))?;
        req = req.with_known_md5_hash(raw);
    }
    if let Some(c) = a.get("crc32c").and_then(|v| v.as_u64()) {
        req = req.with_known_crc32c(c as u32);
    }
    let f = plan.p_cond_flags;
    if f & GCS_COND_GENERATION_MATCH != 0 {
        req = req.set_if_generation_match(plan.conds[0]);
    }
    if f & GCS_COND_GENERATION_NOT_MATCH != 0 {
        req = req.set_if_generation_not_match(plan.conds[1]);
    }
    if f & GCS_COND_METAGENERATION_MATCH != 0 {
        req = req.set_if_metageneration_match(plan.conds[2]);
    }
    if f & GCS_COND_METAGENERATION_NOT_MATCH != 0 {
        req = req.set_if_metageneration_not_match(plan.conds[3]);
    }
    if let Some(k) = plan.key {
        req = req.set_key(k);
    }
    req = req.with_resumable_upload_threshold(plan.chunk_size).with_resumable_upload_buffer_size(plan.chunk_size);

    // Retry: mirror storage.RetryPolicy (+ WithMaxAttempts / WithMaxRetryDuration / WithBackoff).
    // RetryAlways means "treat as idempotent"; the policy then decides which errors retry.
    if plan.retry.mode == GCS_RETRY_ALWAYS {
        req = req.with_idempotency(true);
    }
    let mut req = crate::retry::apply_retry!(req, &plan.retry)?;
    if let Some(ua) = plan.user_agent {
        req = req.with_user_agent(ua);
    }
    if let Some(qp) = plan.quota_project {
        req = req.with_quota_project(qp);
    }
    let obj = req.send_buffered().await.map_err(|e| Status::from_sdk(&e))?;
    serde_json::to_string(&obj).map_err(|e| Status::err(format!("encoding object: {e}")))
}

/// Minimal base64 (standard alphabet) decoder to avoid another dependency.
mod base64_lite {
    pub fn decode(s: &str) -> Option<Vec<u8>> {
        let mut out = Vec::with_capacity(s.len() * 3 / 4);
        let mut acc: u32 = 0;
        let mut bits = 0;
        for c in s.bytes() {
            let v = match c {
                b'A'..=b'Z' => c - b'A',
                b'a'..=b'z' => c - b'a' + 26,
                b'0'..=b'9' => c - b'0' + 52,
                b'+' => 62,
                b'/' => 63,
                b'=' => break,
                b'\r' | b'\n' => continue,
                _ => return None,
            } as u32;
            acc = (acc << 6) | v;
            bits += 6;
            if bits >= 8 {
                bits -= 8;
                out.push((acc >> bits) as u8);
            }
        }
        Some(out)
    }
}

/// Opens a writer. Nothing is sent until data arrives or the writer is closed.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_open(
    c: *const GcsClient,
    p: *const GcsWriteParams,
    out: *mut *mut GcsWriter,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if c.is_null() || p.is_null() || out.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let client = unsafe { &*c };
        let p = unsafe { &*p };
        if p.chunk_size == 0 {
            return Err(Status::invalid("chunk_size must be > 0"));
        }
        let attrs = match unsafe { opt_str(p.attrs_json) }? {
            Some(js) => serde_json::from_str(&js).map_err(|e| Status::invalid(format!("attrs_json: {e}")))?,
            None => serde_json::Value::Null,
        };
        let key = if !p.encryption_key.is_null() && p.encryption_key_len > 0 {
            let k = unsafe { std::slice::from_raw_parts(p.encryption_key, p.encryption_key_len) };
            Some(KeyAes256::new(k).map_err(|e| Status::invalid(format!("encryption key: {e}")))?)
        } else {
            None
        };
        let plan = Plan {
            storage: client.storage.clone(),
            bucket: unsafe { req_str(p.bucket, "bucket") }?,
            object: unsafe { req_str(p.object, "object") }?,
            attrs,
            p_cond_flags: p.cond_flags,
            conds: [p.if_generation_match, p.if_generation_not_match, p.if_metageneration_match, p.if_metageneration_not_match],
            key,
            chunk_size: p.chunk_size,
            retry: p.retry,
            user_agent: client.user_agent.clone(),
            quota_project: client.quota_project.clone(),
        };
        let (tx, rx) = mpsc::channel::<WMsg>(QUEUE_DEPTH);
        let (result_tx, result_rx) = watch::channel(None);
        let task = rt().spawn(upload_task(plan, rx, result_tx));
        let inner = Arc::new(WInner {
            tx: Mutex::new(Some(tx)),
            upload: Mutex::new(Some(task.abort_handle())),
            result: result_rx,
            accepted: AtomicU64::new(0),
        });
        unsafe { *out = Box::into_raw(Box::new(GcsWriter { inner })) };
        Ok(0)
    })
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
        if len == 0 {
            return Ok(0);
        }
        let inner = unsafe { &*w }.inner.clone();
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
        // Slow path: copy on a runtime thread (keeps long memcpys out of the cgo call, which
        // otherwise makes the Go scheduler hand off Ps and spawn threads) and wait for room.
        let buf = GoBuf { ptr: data as *mut u8, len };
        let done = Completion::new(cb, user);
        rt().spawn(async move {
            // Copy first: the pinned Go buffer is released when the callback fires below.
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
    })
}

/// Bytes accepted so far (for Writer.ProgressFunc approximations).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_accepted(w: *const GcsWriter) -> u64 {
    if w.is_null() {
        return 0;
    }
    unsafe { &*w }.inner.accepted.load(Ordering::Relaxed)
}

/// Finalizes the upload. Always pending (returns 1) unless the writer was already closed or
/// aborted (-1). The callback reports `n` = pointer to a Rust-owned C string with the created
/// object as JSON (proto3 JSON of google.storage.v2.Object; free with gcs_string_free).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_writer_close(
    w: *const GcsWriter,
    cb: GcsDoneCb,
    user: usize,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if w.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let inner = unsafe { &*w }.inner.clone();
        let Some(tx) = lock(&inner.tx).take() else {
            return Err(Status::invalid("storage: Writer is closed"));
        };
        let done = Completion::new(cb, user);
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
    })
}

/// Aborts the upload without finalizing it (context cancellation / CloseWithError).
/// Idempotent. Pending writes and a pending close complete with an error.
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
