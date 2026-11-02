//! Per-call options: the *caller's* policy knobs (retry policy and limits, backoff, user
//! project, per-call headers, read/write tuning), encoded as JSON so the ABI does not change
//! when a knob is added. Everything that *interprets* them — retry loops, backoff, per-RPC
//! idempotency, user-project routing — lives in this crate, once, for every language.
//!
//! ```json
//! {
//!   "retry": {"policy": "idempotent|always|never", "maxAttempts": 3, "maxRetryDurationMs": 60000,
//!             "backoff": {"initialMs": 100, "maxMs": 30000, "multiplier": 2.0}, "predicate": 0},
//!   "userProject": "billing-project",
//!   "attemptTimeoutMs": 0,
//!   "headers": {"x-goog-custom": "v"},
//!   "decompress": true, "verifyCrc32c": true, "prefetchBytes": 8388608, "metadataOnly": false,
//!   "chunkSize": 16777216
//! }
//! ```

use std::collections::BTreeMap;
use std::time::Duration;

use google_cloud_gax::backoff_policy::BackoffPolicyArg;
use google_cloud_gax::error::Error as GaxError;
use google_cloud_gax::exponential_backoff::ExponentialBackoffBuilder;
use google_cloud_gax::options::RequestOptions;
use google_cloud_gax::retry_policy::{NeverRetry, RetryPolicy, RetryPolicyArg, RetryPolicyExt};
use google_cloud_gax::retry_result::RetryResult;
use google_cloud_gax::retry_state::RetryState;
use google_cloud_storage::retry_policy::RetryableErrors;
use serde::Deserialize;

use crate::{GcsClient, GcsStatus, Status};

/// Which operations may be retried (mirrors the retry policies every SDK exposes).
#[derive(Deserialize, Default, Clone, Copy, PartialEq, Eq, Debug)]
#[serde(rename_all = "lowercase")]
pub(crate) enum RetryMode {
    /// Retry transient errors of idempotent operations. *Which* operations are idempotent is
    /// decided by the core (see `policy.rs`), from the request itself.
    #[default]
    Idempotent,
    /// Retry transient errors of every operation.
    Always,
    /// Never retry.
    Never,
}

#[derive(Deserialize, Default, Clone, Debug)]
#[serde(rename_all = "camelCase", default)]
pub(crate) struct Backoff {
    pub initial_ms: u64,
    pub max_ms: u64,
    pub multiplier: f64,
}

#[derive(Deserialize, Default, Clone, Debug)]
#[serde(rename_all = "camelCase", default)]
pub(crate) struct RetryOpts {
    pub policy: RetryMode,
    /// 0 = no attempt limit.
    pub max_attempts: u32,
    /// 0 = core default.
    pub max_retry_duration_ms: u64,
    pub backoff: Option<Backoff>,
    /// Opaque caller handle of a custom "should this error be retried?" predicate. The core
    /// evaluates it through the client's retry callback (`GcsClientConfig.retry_cb`); 0 = none.
    pub predicate: u64,
}

#[derive(Deserialize, Default, Clone, Debug)]
#[serde(rename_all = "camelCase", default)]
pub(crate) struct CallOptions {
    pub retry: RetryOpts,
    /// Requester-pays / billing project for this call (overrides the client's quota project).
    pub user_project: Option<String>,
    pub attempt_timeout_ms: u64,
    /// Extra request headers / gRPC metadata for this call.
    pub headers: BTreeMap<String, String>,
    // ---- reads ----
    /// Return decompressed content for objects stored with `Content-Encoding: gzip`.
    pub decompress: bool,
    /// Validate the full-object CRC32C (default true).
    pub verify_crc32c: Option<bool>,
    /// Bytes buffered ahead of the reader (0 = core default).
    pub prefetch_bytes: usize,
    /// Zero-length read: return the object metadata and an immediate EOF.
    pub metadata_only: bool,
    // ---- writes ----
    /// Uploads larger than this use resumable uploads with chunks of this size (0 = default).
    pub chunk_size: usize,
}

/// Called by the core to evaluate a caller-supplied retry predicate (`RetryOpts.predicate`).
/// `status` strings are owned by the callee (free with `gcs_string_free`). Returns non-zero to
/// retry. Runs on a core thread; it must be fast and must not block.
pub type GcsRetryCb = unsafe extern "C" fn(predicate: usize, status: GcsStatus, attempt: u32) -> i32;

impl CallOptions {
    pub(crate) fn parse(p: *const u8, len: usize) -> Result<Self, Status> {
        if p.is_null() || len == 0 {
            return Ok(Self::default());
        }
        let b = unsafe { std::slice::from_raw_parts(p, len) };
        serde_json::from_slice(b).map_err(|e| Status::invalid(format!("options: {e}")))
    }

    /// Effective idempotency: the core's per-RPC rule, widened by the caller's policy.
    pub(crate) fn idempotent(&self, by_rule: bool) -> bool {
        self.retry.policy == RetryMode::Always || by_rule
    }

    /// The retry policy for this call, or `None` to keep the core default.
    pub(crate) fn retry_policy(&self, client: &GcsClient) -> Result<Option<RetryPolicyArg>, Status> {
        let r = &self.retry;
        if r.policy == RetryMode::Never {
            return Ok(Some(NeverRetry.into()));
        }
        if r.predicate != 0 {
            let cb = client.retry_cb.ok_or_else(|| Status::invalid("retry predicate given but the client has no retry_cb"))?;
            return Ok(Some(limited(CallerPredicate { cb, predicate: r.predicate as usize }, r)));
        }
        if r.max_attempts > 0 || r.max_retry_duration_ms > 0 {
            return Ok(Some(limited(RetryableErrors, r)));
        }
        Ok(None)
    }

    pub(crate) fn backoff_policy(&self) -> Result<Option<BackoffPolicyArg>, Status> {
        let Some(b) = &self.retry.backoff else { return Ok(None) };
        if b.initial_ms == 0 {
            return Ok(None);
        }
        let init = Duration::from_millis(b.initial_ms);
        // 0 = default maximum (30s), as in the language SDKs' backoff types.
        let max = Duration::from_millis(if b.max_ms == 0 { 30_000 } else { b.max_ms }.max(b.initial_ms));
        let mult = if b.multiplier > 1.0 { b.multiplier } else { 2.0 };
        let p = ExponentialBackoffBuilder::new()
            .with_initial_delay(init)
            .with_maximum_delay(max)
            .with_scaling(mult)
            .build()
            .map_err(|e| Status::invalid(format!("backoff: {e}")))?;
        Ok(Some(p.into()))
    }

    pub(crate) fn quota_project(&self, client: &GcsClient) -> Option<String> {
        self.user_project.clone().filter(|s| !s.is_empty()).or_else(|| client.quota_project.clone())
    }

    pub(crate) fn header_pairs(&self) -> Result<Vec<(http::HeaderName, http::HeaderValue)>, Status> {
        self.headers
            .iter()
            .map(|(k, v)| {
                let k = http::HeaderName::from_bytes(k.as_bytes()).map_err(|e| Status::invalid(format!("header {k:?}: {e}")))?;
                let v = http::HeaderValue::from_str(v).map_err(|e| Status::invalid(format!("header value: {e}")))?;
                Ok((k, v))
            })
            .collect()
    }

    /// Request options for the generated (gRPC) control-plane request builders.
    pub(crate) fn request_options(&self, client: &GcsClient, idempotent_by_rule: bool) -> Result<RequestOptions, Status> {
        let mut o = RequestOptions::default();
        o.set_idempotency(self.idempotent(idempotent_by_rule));
        if let Some(p) = self.retry_policy(client)? {
            o.set_retry_policy(p);
        }
        if let Some(b) = self.backoff_policy()? {
            o.set_backoff_policy(b);
        }
        if let Some(q) = self.quota_project(client) {
            o.set_quota_project(q);
        }
        if let Some(ua) = &client.user_agent {
            o.set_user_agent(ua.clone());
        }
        if self.attempt_timeout_ms > 0 {
            o.set_attempt_timeout(Duration::from_millis(self.attempt_timeout_ms));
        }
        Ok(o)
    }
}

fn limited<P: RetryPolicy + 'static>(p: P, r: &RetryOpts) -> RetryPolicyArg {
    let d = Duration::from_millis(r.max_retry_duration_ms);
    match (r.max_attempts > 0, r.max_retry_duration_ms > 0) {
        (true, true) => p.with_attempt_limit(r.max_attempts).with_time_limit(d).into(),
        (true, false) => p.with_attempt_limit(r.max_attempts).into(),
        (false, true) => p.with_time_limit(d).into(),
        (false, false) => p.into(),
    }
}

/// A caller-supplied retry predicate (e.g. Go's `storage.WithErrorFunc`). Idempotency and the
/// caller's policy still gate retries; the predicate only classifies errors.
#[derive(Clone, Copy)]
struct CallerPredicate {
    cb: GcsRetryCb,
    predicate: usize,
}

impl std::fmt::Debug for CallerPredicate {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CallerPredicate").finish_non_exhaustive()
    }
}

impl RetryPolicy for CallerPredicate {
    fn on_error(&self, state: &RetryState, error: GaxError) -> RetryResult {
        if !state.idempotent {
            return RetryResult::Permanent(error);
        }
        let st = Status::from_sdk(&error).into_ffi();
        let retry = unsafe { (self.cb)(self.predicate, st, state.attempt_count) } != 0;
        if retry { RetryResult::Continue(error) } else { RetryResult::Permanent(error) }
    }
}

/// Applies retry/backoff/user-project/user-agent options to the data-path request builders
/// (`read_object`, `write_object`), which expose them as inherent methods.
macro_rules! apply_data_opts {
    ($req:expr, $opts:expr, $client:expr) => {{
        let o: &$crate::options::CallOptions = $opts;
        let client: &$crate::GcsClient = $client;
        let mut req = $req;
        if let Some(p) = o.retry_policy(client)? {
            req = req.with_retry_policy(p);
        }
        if let Some(b) = o.backoff_policy()? {
            req = req.with_backoff_policy(b);
        }
        if let Some(q) = o.quota_project(client) {
            req = req.with_quota_project(q);
        }
        if let Some(ua) = &client.user_agent {
            req = req.with_user_agent(ua.clone());
        }
        req
    }};
}
pub(crate) use apply_data_opts;
