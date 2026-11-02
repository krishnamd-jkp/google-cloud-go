//! Mapping of storage.RetryPolicy / WithMaxAttempts / WithMaxRetryDuration / WithBackoff onto
//! the engine's retry and backoff policies. Shared by reads and writes.

use std::time::Duration;

pub const GCS_RETRY_DEFAULT: i32 = 0;
pub const GCS_RETRY_ALWAYS: i32 = 1;
pub const GCS_RETRY_NEVER: i32 = 2;

/// Retry knobs embedded in the read/write parameter structs.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct GcsRetry {
    pub mode: i32,
    /// 0 = no attempt limit.
    pub max_attempts: i32,
    /// 0 = no time limit.
    pub max_retry_duration_ms: i64,
    /// 0 = SDK default backoff.
    pub backoff_initial_ms: i64,
    pub backoff_max_ms: i64,
    pub backoff_multiplier: f64,
}

impl GcsRetry {
    pub(crate) fn max_retry(&self) -> Option<Duration> {
        (self.max_retry_duration_ms > 0).then(|| Duration::from_millis(self.max_retry_duration_ms as u64))
    }

    pub(crate) fn backoff(&self) -> Option<(Duration, Duration, f64)> {
        (self.backoff_initial_ms > 0).then(|| {
            let init = Duration::from_millis(self.backoff_initial_ms as u64);
            let max = Duration::from_millis(self.backoff_max_ms.max(self.backoff_initial_ms) as u64);
            let mult = if self.backoff_multiplier > 1.0 { self.backoff_multiplier } else { 2.0 };
            (init, max, mult)
        })
    }
}

/// Applies a `GcsRetry` to any request builder that has `with_retry_policy` and
/// `with_backoff_policy`. Evaluates to `Result<builder, Status>`.
/// (`with_idempotency` for RetryAlways is write-only and handled by the caller.)
macro_rules! apply_retry {
    ($req:expr, $r:expr) => {{
        use google_cloud_gax::retry_policy::{NeverRetry, RetryPolicyExt};
        use google_cloud_storage::retry_policy::RetryableErrors;
        let r: &$crate::retry::GcsRetry = $r;
        let mut req = $req;
        if r.mode == $crate::retry::GCS_RETRY_NEVER {
            req = req.with_retry_policy(NeverRetry);
        } else {
            match (r.max_attempts > 0, r.max_retry()) {
                (true, Some(d)) => {
                    req = req.with_retry_policy(
                        RetryableErrors.with_attempt_limit(r.max_attempts as u32).with_time_limit(d),
                    )
                }
                (true, None) => {
                    req = req.with_retry_policy(RetryableErrors.with_attempt_limit(r.max_attempts as u32))
                }
                (false, Some(d)) => req = req.with_retry_policy(RetryableErrors.with_time_limit(d)),
                (false, None) => {}
            }
        }
        match r.backoff() {
            None => Ok(req),
            Some((init, max, mult)) => google_cloud_gax::exponential_backoff::ExponentialBackoffBuilder::new()
                .with_initial_delay(init)
                .with_maximum_delay(max)
                .with_scaling(mult)
                .build()
                .map(|b| req.with_backoff_policy(b))
                .map_err(|e| $crate::Status::invalid(format!("backoff: {e}"))),
        }
    }};
}
pub(crate) use apply_retry;
