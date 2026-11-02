//! gcs_core v2 — a C ABI over the `google-cloud-storage` crate, used by the Go SDK's
//! opt-in Rust *data path* (object reads and writes only; the Go SDK keeps the control plane).
//!
//! This is a rewrite of the original POC bridge. The contract below is what makes it safe to
//! embed in a long-running Go process; each rule fixes a concrete defect found in the POC.
//!
//! * **No unwinding into C.** Every exported function runs under `catch_unwind`, including the
//!   synchronous prologue of async entry points. Background tasks are wrapped too, so a panic
//!   always becomes an error completion instead of a lost callback (= a goroutine hung forever).
//! * **No use-after-free.** Readers and writers are `Arc`-backed. The FFI owns one reference;
//!   each in-flight operation owns its own clone, so `close/free` racing with an operation can
//!   never free memory that a task is still using.
//! * **Exactly-once completions.** Every async operation reports exactly one callback
//!   `GcsDoneCb(user, status, n)`. `user` is an opaque `uintptr_t` (a `cgo.Handle` on the Go
//!   side); it is never a pointer, so Go never fabricates an `unsafe.Pointer` from an integer.
//! * **Pinned Go memory is released at the callback.** Read destinations / write sources
//!   handed to an async operation are only touched until that operation's callback fires.
//! * **Uploads finalize only on an explicit close.** Abort, cancellation, dropping the handle or
//!   the writer task dying never finalize a partially written object.
//! * **Errors keep their structure.** HTTP status, `google.rpc.Code`, service message and
//!   response headers cross the boundary so Go can rebuild `*googleapi.Error`.

use std::collections::HashMap;
use std::ffi::{CStr, CString, c_char, c_void};
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Mutex, MutexGuard, OnceLock};

use google_cloud_storage::client::Storage;
use tokio::runtime::Runtime;
use tokio::task::AbortHandle;

mod creds;
mod reader;
mod retry;
mod writer;

pub use reader::*;
pub use retry::{GCS_RETRY_ALWAYS, GCS_RETRY_DEFAULT, GCS_RETRY_NEVER, GcsRetry};
pub use writer::*;

// ---------------------------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------------------------

pub const GCS_OK: i32 = 0;
pub const GCS_ERR: i32 = 1;
pub const GCS_PANIC: i32 = 2;
pub const GCS_INVALID: i32 = 3;
pub const GCS_CANCELLED: i32 = 4;

/// Status crossing the boundary. Strings are owned by Rust; free with `gcs_string_free`.
#[repr(C)]
pub struct GcsStatus {
    pub code: i32,
    /// HTTP status of the failed request, or 0.
    pub http_code: i32,
    /// `google.rpc.Code` of the failure, or -1 when unknown.
    pub rpc_code: i32,
    /// Human readable message (the service message when available). NULL on OK.
    pub message: *mut c_char,
    /// Response headers as "Name: value\n" lines, or NULL.
    pub headers: *mut c_char,
}

#[derive(Clone, Debug)]
pub(crate) struct Status {
    pub code: i32,
    pub http_code: i32,
    pub rpc_code: i32,
    pub message: String,
    pub headers: String,
}

impl Status {
    pub(crate) fn ok() -> Self {
        Self::new(GCS_OK, String::new())
    }
    pub(crate) fn new(code: i32, message: impl Into<String>) -> Self {
        Status { code, http_code: 0, rpc_code: -1, message: message.into(), headers: String::new() }
    }
    pub(crate) fn err(message: impl Into<String>) -> Self {
        Self::new(GCS_ERR, message)
    }
    pub(crate) fn invalid(message: impl Into<String>) -> Self {
        Self::new(GCS_INVALID, message)
    }
    pub(crate) fn cancelled(message: impl Into<String>) -> Self {
        Self::new(GCS_CANCELLED, message)
    }
    pub(crate) fn from_panic(p: Box<dyn std::any::Any + Send>) -> Self {
        let msg = p
            .downcast_ref::<&str>()
            .map(|s| s.to_string())
            .or_else(|| p.downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "unknown panic".into());
        Self::new(GCS_PANIC, format!("gcs_core: rust panic: {msg}"))
    }
    /// Keeps the structure the Go side needs to rebuild `*googleapi.Error`.
    pub(crate) fn from_sdk(e: &google_cloud_storage::Error) -> Self {
        let service_msg = e.status().map(|s| s.message.clone()).filter(|m| !m.is_empty());
        let mut s = Self::err(service_msg.unwrap_or_else(|| e.to_string()));
        s.http_code = e.http_status_code().map(i32::from).unwrap_or(0);
        s.rpc_code = e.status().map(|st| st.code as i32).unwrap_or(-1);
        if let Some(h) = e.http_headers() {
            for (k, v) in h {
                if let Ok(v) = v.to_str() {
                    s.headers.push_str(k.as_str());
                    s.headers.push_str(": ");
                    s.headers.push_str(v);
                    s.headers.push('\n');
                }
            }
        }
        s
    }
    pub(crate) fn into_ffi(self) -> GcsStatus {
        let message = if self.code == GCS_OK { std::ptr::null_mut() } else { to_c(self.message) };
        let headers = if self.headers.is_empty() { std::ptr::null_mut() } else { to_c(self.headers) };
        GcsStatus { code: self.code, http_code: self.http_code, rpc_code: self.rpc_code, message, headers }
    }
}

fn to_c(s: String) -> *mut c_char {
    CString::new(s.replace('\0', " ")).map(CString::into_raw).unwrap_or(std::ptr::null_mut())
}

pub(crate) unsafe fn set_status(out: *mut GcsStatus, s: Status) {
    if !out.is_null() {
        unsafe { out.write(s.into_ffi()) };
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_string_free(s: *mut c_char) {
    if !s.is_null() {
        drop(unsafe { CString::from_raw(s) });
    }
}

// ---------------------------------------------------------------------------------------------
// Panic guards
// ---------------------------------------------------------------------------------------------

/// For entry points that return a `GcsStatus` by value.
pub(crate) fn guard_status<F: FnOnce() -> Result<(), Status>>(f: F) -> GcsStatus {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(Ok(())) => Status::ok().into_ffi(),
        Ok(Err(s)) => s.into_ffi(),
        Err(p) => Status::from_panic(p).into_ffi(),
    }
}

/// For entry points that return an `i32` code and report failures through `out_status`.
/// Convention: `0` = completed synchronously, `1` = pending (callback will fire exactly once),
/// `-1` = failed synchronously (`*out_status` set, no callback).
pub(crate) fn guard_rc<F: FnOnce() -> Result<i32, Status>>(out_status: *mut GcsStatus, f: F) -> i32 {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(Ok(rc)) => rc,
        Ok(Err(s)) => {
            unsafe { set_status(out_status, s) };
            -1
        }
        Err(p) => {
            unsafe { set_status(out_status, Status::from_panic(p)) };
            -1
        }
    }
}

/// For `void` entry points (free/close/abort): swallow panics, never unwind into C.
pub(crate) fn guard_void<F: FnOnce()>(f: F) {
    let _ = catch_unwind(AssertUnwindSafe(f));
}

/// Mutex lock that ignores poisoning (a panicking task must not wedge every later call).
pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|p| p.into_inner())
}

// ---------------------------------------------------------------------------------------------
// Completions
// ---------------------------------------------------------------------------------------------

/// Completion callback. `user` is an opaque value supplied by the caller (a cgo.Handle).
/// `status.message`/`status.headers` are owned by the callee (free with `gcs_string_free`).
pub type GcsDoneCb = unsafe extern "C" fn(user: usize, status: GcsStatus, n: u64);

#[derive(Clone, Copy)]
pub(crate) struct Completion {
    cb: GcsDoneCb,
    user: usize,
}

impl Completion {
    pub(crate) fn new(cb: GcsDoneCb, user: usize) -> Self {
        Completion { cb, user }
    }
    /// Consumes the completion: the type system makes "exactly once" hard to get wrong.
    pub(crate) fn complete(self, st: Status, n: u64) {
        unsafe { (self.cb)(self.user, st.into_ffi(), n) }
    }
}

/// A region of (pinned) Go memory lent to an async operation until its callback fires.
#[derive(Clone, Copy)]
pub(crate) struct GoBuf {
    pub ptr: *mut u8,
    pub len: usize,
}
// SAFETY: the Go side pins the memory (runtime.Pinner) and does not touch it until the
// completion callback for the operation that owns it has fired.
unsafe impl Send for GoBuf {}

impl GoBuf {
    /// SAFETY: only valid until the owning operation's completion fires (see above).
    pub(crate) unsafe fn as_slice(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len) }
    }
}

// ---------------------------------------------------------------------------------------------
// Runtime
// ---------------------------------------------------------------------------------------------

static RT: OnceLock<Runtime> = OnceLock::new();
static RT_WORKERS: AtomicUsize = AtomicUsize::new(0);

/// Installs the process-wide rustls crypto provider selected at build time.
fn install_crypto_provider() {
    #[cfg(feature = "crypto-ring")]
    {
        // Ignore the error: a provider may already be installed (e.g. by another Rust library).
        let _ = rustls::crypto::ring::default_provider().install_default();
    }
}

pub(crate) fn rt() -> &'static Runtime {
    RT.get_or_init(|| {
        install_crypto_provider();
        let mut b = tokio::runtime::Builder::new_multi_thread();
        b.enable_all().thread_name("gcs-core");
        let n = RT_WORKERS.load(Ordering::SeqCst);
        if n > 0 {
            b.worker_threads(n);
        }
        b.build().expect("failed to start the tokio runtime")
    })
}

/// Sets the number of Tokio worker threads. Only effective before the first client is created;
/// returns 1 when applied and 0 when the runtime already exists. 0 = Tokio default (#CPUs).
#[unsafe(no_mangle)]
pub extern "C" fn gcs_runtime_configure(worker_threads: u32) -> i32 {
    let mut applied = 0;
    guard_void(|| {
        RT_WORKERS.store(worker_threads as usize, Ordering::SeqCst);
        applied = if RT.get().is_none() { 1 } else { 0 };
    });
    applied
}

// ---------------------------------------------------------------------------------------------
// Cancellable one-shot operations (open reader). Long-lived handles have their own cancel.
// ---------------------------------------------------------------------------------------------

static NEXT_OP: AtomicU64 = AtomicU64::new(1);
static OPS: OnceLock<Mutex<HashMap<u64, AbortHandle>>> = OnceLock::new();

fn ops() -> &'static Mutex<HashMap<u64, AbortHandle>> {
    OPS.get_or_init(|| Mutex::new(HashMap::new()))
}

/// Runs `fut` on the runtime and completes `done` exactly once, also when the task is aborted
/// through `gcs_op_cancel` or panics. Returns the op id (never 0).
pub(crate) fn spawn_op<F>(fut: F, done: Completion) -> u64
where
    F: std::future::Future<Output = Result<u64, Status>> + Send + 'static,
{
    let id = NEXT_OP.fetch_add(1, Ordering::Relaxed);
    let jh = rt().spawn(futures::FutureExt::catch_unwind(AssertUnwindSafe(fut)));
    lock(ops()).insert(id, jh.abort_handle());
    rt().spawn(async move {
        let r = jh.await;
        lock(ops()).remove(&id);
        match r {
            Ok(Ok(Ok(n))) => done.complete(Status::ok(), n),
            Ok(Ok(Err(s))) => done.complete(s, 0),
            Ok(Err(p)) => done.complete(Status::from_panic(p), 0),
            Err(je) if je.is_cancelled() => done.complete(Status::cancelled("operation cancelled"), 0),
            Err(je) => done.complete(Status::new(GCS_PANIC, format!("gcs_core: task failed: {je}")), 0),
        }
    });
    id
}

/// Cancels an operation started by `spawn_op`. The completion still fires (code CANCELLED, or
/// the real result if the operation already finished).
#[unsafe(no_mangle)]
pub extern "C" fn gcs_op_cancel(op_id: u64) {
    guard_void(|| {
        if let Some(h) = lock(ops()).get(&op_id) {
            h.abort();
        }
    });
}

// ---------------------------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------------------------

pub const GCS_AUTH_ADC: i32 = 0;
pub const GCS_AUTH_ANONYMOUS: i32 = 1;
pub const GCS_AUTH_JSON: i32 = 2;
pub const GCS_AUTH_TOKEN_CALLBACK: i32 = 3;

/// Token callback used for `GCS_AUTH_TOKEN_CALLBACK`: lets the Rust engine use *any* Go
/// credential (option.WithCredentials*, WithTokenSource, impersonation, WIF, ...).
/// Returns 0 on success with `*out_token` (malloc'ed C string, freed by Rust with `free`) and
/// `*out_expiry_unix` (0 = unknown); non-zero on failure with `*out_err` (malloc'ed or NULL).
pub type GcsTokenCb =
    unsafe extern "C" fn(user: usize, out_token: *mut *mut c_char, out_expiry_unix: *mut i64, out_err: *mut *mut c_char) -> i32;

#[repr(C)]
pub struct GcsClientConfig {
    /// e.g. "https://storage.googleapis.com" or "http://localhost:9000"; NULL = default.
    pub endpoint: *const c_char,
    pub universe_domain: *const c_char,
    pub credentials_json: *const c_char,
    pub quota_project: *const c_char,
    /// Prepended to the engine's user agent (e.g. "gcloud-golang/1.70 rust-core").
    pub user_agent: *const c_char,
    /// Extra headers for every request, as "Name: value\n" lines.
    pub custom_headers: *const c_char,
    pub auth_mode: i32,
    pub token_cb: Option<GcsTokenCb>,
    pub token_user: usize,
}

pub struct GcsClient {
    pub(crate) storage: Storage,
    pub(crate) user_agent: Option<String>,
    pub(crate) quota_project: Option<String>,
}

pub(crate) unsafe fn opt_str(p: *const c_char) -> Result<Option<String>, Status> {
    if p.is_null() {
        return Ok(None);
    }
    let s = unsafe { CStr::from_ptr(p) }.to_str().map_err(|e| Status::invalid(format!("invalid utf-8: {e}")))?;
    Ok(if s.is_empty() { None } else { Some(s.to_string()) })
}

pub(crate) unsafe fn req_str(p: *const c_char, what: &str) -> Result<String, Status> {
    unsafe { opt_str(p) }?.ok_or_else(|| Status::invalid(format!("{what} is required")))
}

pub(crate) fn bucket_name(b: &str) -> String {
    if b.starts_with("projects/") { b.to_string() } else { format!("projects/_/buckets/{b}") }
}

fn parse_headers(s: &str) -> Result<Vec<(http::HeaderName, http::HeaderValue)>, Status> {
    let mut out = Vec::new();
    for line in s.lines().filter(|l| !l.trim().is_empty()) {
        let (k, v) = line.split_once(':').ok_or_else(|| Status::invalid(format!("bad header line {line:?}")))?;
        let k = http::HeaderName::from_bytes(k.trim().as_bytes()).map_err(|e| Status::invalid(e.to_string()))?;
        let v = http::HeaderValue::from_str(v.trim()).map_err(|e| Status::invalid(e.to_string()))?;
        out.push((k, v));
    }
    Ok(out)
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_client_new(cfg: *const GcsClientConfig, out: *mut *mut GcsClient) -> GcsStatus {
    guard_status(|| {
        if cfg.is_null() || out.is_null() {
            return Err(Status::invalid("null argument"));
        }
        let cfg = unsafe { &*cfg };
        let endpoint = unsafe { opt_str(cfg.endpoint) }?;
        let universe = unsafe { opt_str(cfg.universe_domain) }?;
        let quota_project = unsafe { opt_str(cfg.quota_project) }?;
        let user_agent = unsafe { opt_str(cfg.user_agent) }?;
        let headers = match unsafe { opt_str(cfg.custom_headers) }? {
            Some(h) => parse_headers(&h)?,
            None => Vec::new(),
        };
        let credentials = match cfg.auth_mode {
            GCS_AUTH_ADC => None,
            GCS_AUTH_ANONYMOUS => Some(google_cloud_auth::credentials::anonymous::Builder::new().build()),
            GCS_AUTH_JSON => {
                let js = unsafe { req_str(cfg.credentials_json, "credentials_json") }?;
                Some(creds::from_json(&js)?)
            }
            GCS_AUTH_TOKEN_CALLBACK => {
                let cb = cfg.token_cb.ok_or_else(|| Status::invalid("token_cb is required"))?;
                Some(creds::go_token_credentials(cb, cfg.token_user))
            }
            m => return Err(Status::invalid(format!("unknown auth_mode {m}"))),
        };
        let storage = rt()
            .block_on(async move {
                let mut b = Storage::builder();
                if let Some(e) = endpoint {
                    b = b.with_endpoint(e);
                }
                if let Some(u) = universe {
                    b = b.with_universe_domain(u);
                }
                if let Some(c) = credentials {
                    b = b.with_credentials(c);
                }
                for (k, v) in headers {
                    b = b.with_custom_header(k, v);
                }
                b.build().await
            })
            .map_err(|e| Status::err(format!("building storage client: {e}")))?;
        let client = GcsClient { storage, user_agent, quota_project };
        unsafe { *out = Box::into_raw(Box::new(client)) };
        Ok(())
    })
}

/// Frees the client. In-flight readers/writers keep working: they hold their own clones of the
/// underlying (reference-counted) transport.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn gcs_client_free(c: *mut GcsClient) {
    guard_void(|| {
        if !c.is_null() {
            drop(unsafe { Box::from_raw(c) });
        }
    });
}

/// Frees memory allocated by the Go side with C.malloc (C.CString) and handed to Rust.
pub(crate) unsafe fn c_free(p: *mut c_char) {
    unsafe extern "C" {
        fn free(p: *mut c_void);
    }
    if !p.is_null() {
        unsafe { free(p.cast()) };
    }
}

/// Cheap call used to measure raw Go->Rust crossing cost.
#[unsafe(no_mangle)]
pub extern "C" fn gcs_noop(x: u64) -> u64 {
    x.wrapping_add(1)
}

/// Completes immediately from a runtime thread: measures the cost of one asynchronous
/// completion (task spawn + Rust->Go callback on a foreign thread + goroutine wake-up).
#[unsafe(no_mangle)]
pub extern "C" fn gcs_async_noop(cb: GcsDoneCb, user: usize) -> i32 {
    let mut rc = -1;
    guard_void(|| {
        let done = Completion::new(cb, user);
        rt().spawn(async move { done.complete(Status::ok(), 1) });
        rc = 1;
    });
    rc
}
