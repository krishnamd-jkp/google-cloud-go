//! Streaming object reads.
//!
//! Design (vs. the POC, which issued one Tokio task + one Rust->Go callback per `Read` call):
//! a background task prefetches response chunks into a bounded queue; `gcs_reader_read` first
//! drains that queue *synchronously* (one plain cgo call, one memcpy), and only registers an
//! async waiter when the queue is empty. Small `io.Copy`-style reads therefore cost one cheap
//! FFI call instead of a task spawn plus a cross-thread callback, while time-to-first-byte is
//! unchanged (a read returns as soon as any data is available).

use std::collections::VecDeque;
use std::ffi::{CString, c_char};
use std::panic::AssertUnwindSafe;
use std::sync::{Arc, Mutex};

use bytes::{Buf, Bytes};
use google_cloud_storage::model_ext::{KeyAes256, ReadRange};
use google_cloud_storage::read_object::ReadObjectResponse;
use tokio::sync::Notify;
use tokio::task::AbortHandle;

use crate::{
    Completion, GcsClient, GcsDoneCb, GcsStatus, GoBuf, Status, bucket_name, guard_rc, guard_void, lock, req_str,
    rt, spawn_op,
};

pub const GCS_COND_GENERATION_MATCH: u32 = 1;
pub const GCS_COND_GENERATION_NOT_MATCH: u32 = 2;
pub const GCS_COND_METAGENERATION_MATCH: u32 = 4;
pub const GCS_COND_METAGENERATION_NOT_MATCH: u32 = 8;

const DEFAULT_PREFETCH: usize = 8 << 20;
const WAKE_BYTES: usize = 256 << 10;

#[repr(C)]
pub struct GcsReadParams {
    pub bucket: *const c_char,
    pub object: *const c_char,
    /// < 0 = latest generation.
    pub generation: i64,
    /// `ObjectHandle.NewRangeReader(offset, length)` semantics; length must not be 0.
    pub offset: i64,
    pub length: i64,
    pub cond_flags: u32,
    pub if_generation_match: i64,
    pub if_generation_not_match: i64,
    pub if_metageneration_match: i64,
    pub if_metageneration_not_match: i64,
    /// Customer-supplied encryption key (32 bytes) or NULL.
    pub encryption_key: *const u8,
    pub encryption_key_len: usize,
    /// 1 = return stored (e.g. gzip) bytes; 0 = let the service decompress (Go's default).
    pub read_compressed: i32,
    pub disable_crc_check: i32,
    /// Bytes buffered ahead of the reader; 0 = default (8 MiB). Not visible to GOMEMLIMIT.
    pub prefetch_bytes: usize,
    pub retry: crate::GcsRetry,
}

pub struct GcsReader {
    shared: Arc<RShared>,
    task: AbortHandle,
    attrs_json: CString,
}

struct RShared {
    st: Mutex<RState>,
    space: Notify,
    limit: usize,
    /// A pending read is woken only once this many bytes are queued (or at EOF/error), except
    /// for the first delivery (time-to-first-byte). Network-bound small reads then cost one
    /// async completion per `wake_bytes` instead of one per network chunk.
    wake_bytes: usize,
}

struct RState {
    queue: VecDeque<Bytes>,
    queued: usize,
    /// Some(Ok) = EOF, Some(Err) = sticky failure (including cancellation).
    end: Option<Result<(), Status>>,
    waiter: Option<(GoBuf, Completion)>,
    closed: bool,
    delivered: bool,
}

fn drain_into(st: &mut RState, buf: GoBuf) -> usize {
    // SAFETY: `buf` is pinned Go memory owned by the in-progress read (see GoBuf).
    let dst = unsafe { std::slice::from_raw_parts_mut(buf.ptr, buf.len) };
    let mut n = 0;
    while n < dst.len() {
        let Some(front) = st.queue.front_mut() else { break };
        let take = front.len().min(dst.len() - n);
        dst[n..n + take].copy_from_slice(&front[..take]);
        front.advance(take);
        n += take;
        if front.is_empty() {
            st.queue.pop_front();
        }
    }
    st.queued -= n;
    n
}

/// Must be called with the lock held. Returns the completion to fire after unlocking.
fn take_ready_waiter(st: &mut RState, wake_bytes: usize) -> Option<(Completion, Status, u64)> {
    if st.waiter.is_none() {
        return None;
    }
    if st.end.is_none() && st.delivered && st.queued < wake_bytes {
        let want = st.waiter.as_ref().map(|(b, _)| b.len).unwrap_or(0);
        if st.queued < want {
            return None; // keep batching: neither the threshold nor the caller's buffer is full
        }
    }
    let (buf, done) = st.waiter.take()?;
    let n = drain_into(st, buf);
    if n > 0 {
        st.delivered = true;
        return Some((done, Status::ok(), n as u64));
    }
    match &st.end {
        Some(Ok(())) => Some((done, Status::ok(), 0)),
        Some(Err(s)) => Some((done, s.clone(), 0)),
        None => {
            st.waiter = Some((buf, done));
            None
        }
    }
}

async fn prefetch(shared: Arc<RShared>, mut resp: ReadObjectResponse) {
    let r = futures::FutureExt::catch_unwind(AssertUnwindSafe(prefetch_loop(&shared, &mut resp))).await;
    if let Err(p) = r {
        let done = {
            let mut st = lock(&shared.st);
            if st.end.is_none() {
                st.end = Some(Err(Status::from_panic(p)));
            }
            take_ready_waiter(&mut st, shared.wake_bytes)
        };
        if let Some((c, s, n)) = done {
            c.complete(s, n);
        }
    }
}

async fn prefetch_loop(shared: &RShared, resp: &mut ReadObjectResponse) {
    loop {
        // Backpressure: bounded read-ahead, but never stall a waiting reader.
        loop {
            {
                let st = lock(&shared.st);
                if st.closed || st.end.is_some() {
                    return;
                }
                if st.queued < shared.limit || st.waiter.is_some() {
                    break;
                }
            }
            shared.space.notified().await;
        }
        let item = resp.next().await;
        let done = {
            let mut st = lock(&shared.st);
            if st.closed {
                return;
            }
            match item {
                Some(Ok(chunk)) => {
                    if !chunk.is_empty() {
                        st.queued += chunk.len();
                        st.queue.push_back(chunk);
                    }
                }
                Some(Err(e)) => {
                    if st.end.is_none() {
                        st.end = Some(Err(Status::from_sdk(&e)));
                    }
                }
                None => {
                    if st.end.is_none() {
                        st.end = Some(Ok(()));
                    }
                }
            }
            take_ready_waiter(&mut st, shared.wake_bytes)
        };
        if let Some((c, s, n)) = done {
            c.complete(s, n);
        }
    }
}

/// Starts a read. Pending (returns 1): the callback fires once with `n` = the `GcsReader*`
/// (convert in C) or an error. `*out_op` receives an id for `gcs_op_cancel`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_reader_open(
    c: *const GcsClient,
    p: *const GcsReadParams,
    cb: GcsDoneCb,
    user: usize,
    out_op: *mut u64,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if c.is_null() || p.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let client = unsafe { &*c };
        let p = unsafe { &*p };
        let bucket = bucket_name(&unsafe { req_str(p.bucket, "bucket") }?);
        let object = unsafe { req_str(p.object, "object") }?;
        let (offset, length) = (p.offset, p.length);
        if length == 0 {
            return Err(Status::invalid("zero-length reads are not handled by the Rust data path"));
        }
        if offset < 0 && length >= 0 {
            return Err(Status::invalid(format!("storage: invalid offset {offset} < 0 requires negative length")));
        }
        let mut req = client.storage.read_object(bucket, object);
        req = req.set_read_range(if length > 0 {
            ReadRange::segment(offset as u64, length as u64)
        } else if offset < 0 {
            ReadRange::tail(offset.unsigned_abs())
        } else if offset > 0 {
            ReadRange::offset(offset as u64)
        } else {
            ReadRange::all()
        });
        if p.generation >= 0 {
            req = req.set_generation(p.generation);
        }
        let f = p.cond_flags;
        if f & GCS_COND_GENERATION_MATCH != 0 {
            req = req.set_if_generation_match(p.if_generation_match);
        }
        if f & GCS_COND_GENERATION_NOT_MATCH != 0 {
            req = req.set_if_generation_not_match(p.if_generation_not_match);
        }
        if f & GCS_COND_METAGENERATION_MATCH != 0 {
            req = req.set_if_metageneration_match(p.if_metageneration_match);
        }
        if f & GCS_COND_METAGENERATION_NOT_MATCH != 0 {
            req = req.set_if_metageneration_not_match(p.if_metageneration_not_match);
        }
        if !p.encryption_key.is_null() && p.encryption_key_len > 0 {
            let key = unsafe { std::slice::from_raw_parts(p.encryption_key, p.encryption_key_len) };
            req = req.set_key(KeyAes256::new(key).map_err(|e| Status::invalid(format!("encryption key: {e}")))?);
        }
        let read_compressed = p.read_compressed != 0;
        // The Rust SDK defaults to *not* decompressing (it returns stored gzip bytes); Go's
        // default is to return decompressed content. Mirror Go.
        req = req.with_automatic_decompression(!read_compressed);
        if p.disable_crc_check != 0 {
            req = req.compute_crc32c(false);
        }
        if let Some(ua) = &client.user_agent {
            req = req.with_user_agent(ua.clone());
        }
        if let Some(qp) = &client.quota_project {
            req = req.with_quota_project(qp.clone());
        }
        let req = crate::retry::apply_retry!(req, &p.retry)?;
        let limit = if p.prefetch_bytes > 0 { p.prefetch_bytes } else { DEFAULT_PREFETCH };
        let id = spawn_op(
            async move {
                let resp = req.send().await.map_err(|e| Status::from_sdk(&e))?;
                let hl = resp.object();
                let size = hl.size;
                let (start, mut remain) = if length < 0 && offset < 0 {
                    let n = offset.unsigned_abs().min(size.max(0) as u64) as i64;
                    (size - n, n)
                } else if length < 0 {
                    (offset, (size - offset).max(0))
                } else {
                    (offset, length.min((size - offset).max(0)))
                };
                let decompressed = !read_compressed && hl.content_encoding == "gzip";
                if decompressed {
                    remain = -1;
                }
                let attrs = serde_json::json!({
                    "size": size,
                    "generation": hl.generation,
                    "metageneration": hl.metageneration,
                    "contentType": hl.content_type,
                    "contentEncoding": hl.content_encoding,
                    "contentLanguage": hl.content_language,
                    "contentDisposition": hl.content_disposition,
                    "storageClass": hl.storage_class,
                    "etag": hl.etag,
                    "crc32c": hl.checksums.as_ref().and_then(|c| c.crc32c),
                    "metadata": hl.metadata,
                    "startOffset": start,
                    "remain": remain,
                    "decompressed": decompressed,
                });
                let attrs_json = CString::new(attrs.to_string()).map_err(|e| Status::err(e.to_string()))?;
                let shared = Arc::new(RShared {
                    st: Mutex::new(RState {
                        queue: VecDeque::new(),
                        queued: 0,
                        end: None,
                        waiter: None,
                        closed: false,
                        delivered: false,
                    }),
                    space: Notify::new(),
                    limit,
                    wake_bytes: WAKE_BYTES.min(limit),
                });
                let task = rt().spawn(prefetch(shared.clone(), resp));
                let reader = GcsReader { shared, task: task.abort_handle(), attrs_json };
                Ok(Box::into_raw(Box::new(reader)) as u64)
            },
            Completion::new(cb, user),
        );
        if !out_op.is_null() {
            unsafe { *out_op = id };
        }
        Ok(1)
    })
}

/// JSON object attributes captured at open. Borrowed; valid until `gcs_reader_close`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_reader_attrs_json(r: *const GcsReader) -> *const c_char {
    if r.is_null() {
        return std::ptr::null();
    }
    unsafe { &*r }.attrs_json.as_ptr()
}

/// io.Reader semantics. Returns 0 with `*out_n` bytes copied (`*out_n == 0` => EOF) when data
/// (or EOF) is available now; 1 when pending (the callback fires once with `n` bytes, 0 = EOF,
/// and `buf` must stay pinned until then); -1 on failure (`*out_status`). One read at a time.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_reader_read(
    r: *const GcsReader,
    buf: *mut u8,
    cap: usize,
    cb: GcsDoneCb,
    user: usize,
    out_n: *mut usize,
    out_status: *mut GcsStatus,
) -> i32 {
    guard_rc(out_status, || {
        if r.is_null() || buf.is_null() || cap == 0 || out_n.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let r = unsafe { &*r };
        let mut st = lock(&r.shared.st);
        if st.closed {
            return Err(Status::invalid("storage: reader is closed"));
        }
        if st.waiter.is_some() {
            return Err(Status::invalid("concurrent Read calls on one reader"));
        }
        let goodbuf = GoBuf { ptr: buf, len: cap };
        let n = drain_into(&mut st, goodbuf);
        if n > 0 {
            st.delivered = true;
            drop(st);
            r.shared.space.notify_one();
            unsafe { *out_n = n };
            return Ok(0);
        }
        match &st.end {
            Some(Ok(())) => {
                unsafe { *out_n = 0 };
                return Ok(0);
            }
            Some(Err(s)) => return Err(s.clone()),
            None => {}
        }
        st.waiter = Some((goodbuf, Completion::new(cb, user)));
        drop(st);
        r.shared.space.notify_one();
        Ok(1)
    })
}

/// Fails the reader (sticky): a pending read completes with CANCELLED, later reads fail.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_reader_cancel(r: *const GcsReader) {
    guard_void(|| {
        if r.is_null() {
            return;
        }
        let r = unsafe { &*r };
        let waiter = {
            let mut st = lock(&r.shared.st);
            st.end = Some(Err(Status::cancelled("context canceled")));
            st.queue.clear();
            st.queued = 0;
            st.waiter.take()
        };
        r.task.abort();
        r.shared.space.notify_one();
        if let Some((_, done)) = waiter {
            done.complete(Status::cancelled("context canceled"), 0);
        }
    });
}

/// Closes the reader. Safe to call while a read is pending on another goroutine: the pending
/// read completes with CANCELLED and no memory is freed while still in use.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_reader_close(r: *mut GcsReader) {
    guard_void(|| {
        if r.is_null() {
            return;
        }
        let r = unsafe { Box::from_raw(r) };
        let waiter = {
            let mut st = lock(&r.shared.st);
            st.closed = true;
            st.queue.clear();
            st.queued = 0;
            st.waiter.take()
        };
        r.task.abort();
        r.shared.space.notify_one();
        if let Some((_, done)) = waiter {
            done.complete(Status::cancelled("storage: reader closed during Read"), 0);
        }
    });
}
