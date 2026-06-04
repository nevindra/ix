//! Translation between the ix `browser_wait` API and pinchtab's native
//! `POST /wait` endpoint (internal/handlers/wait.go in the pinchtab repo).
//!
//! Pure functions only — extracted so they can be unit-tested without a live
//! HTTP server, mirroring `build_snapshot_path` in pinchtab.rs.

use ix_core::types::{BrowserWaitOpts, BrowserWaitResult};
use ix_core::{Error, Result};

/// Default wait deadline (mirrors pinchtab's defaultTimeout).
pub const WAIT_DEFAULT_TIMEOUT_MS: u64 = 10_000;
/// Maximum wait deadline (mirrors pinchtab's maxWaitTimeout).
pub const WAIT_MAX_TIMEOUT_MS: u64 = 30_000;
/// Extra HTTP-request budget on top of the wait deadline so the per-request
/// reqwest timeout never fires before pinchtab's own deadline does.
pub const WAIT_HTTP_MARGIN_MS: u64 = 5_000;

/// Effective wait deadline in ms: default 10 s, capped at 30 s.
pub fn effective_timeout_ms(opts: &BrowserWaitOpts) -> u64 {
    opts.timeout_ms
        .unwrap_or(WAIT_DEFAULT_TIMEOUT_MS)
        .min(WAIT_MAX_TIMEOUT_MS)
}

/// Translate `BrowserWaitOpts` into pinchtab's `POST /wait` JSON body.
/// Exactly one mode field is set, derived from `kind`. Validation lives here
/// so the local and remote backends enforce identical rules.
pub fn build_wait_body(opts: &BrowserWaitOpts) -> Result<serde_json::Value> {
    let timeout = effective_timeout_ms(opts);
    let value = opts.value.as_deref().unwrap_or("");
    let mut body = serde_json::json!({ "timeout": timeout });

    match opts.kind.as_str() {
        "selector" => {
            if value.is_empty() {
                return Err(Error::BadRequest(
                    "wait kind=selector requires value (a selector)".into(),
                ));
            }
            let state = opts.state.as_deref().unwrap_or("visible");
            if state != "visible" && state != "hidden" {
                return Err(Error::BadRequest(format!(
                    "wait state must be visible or hidden, got {state}"
                )));
            }
            body["selector"] = value.into();
            body["state"] = state.into();
        }
        "text" => {
            if value.is_empty() {
                return Err(Error::BadRequest(
                    "wait kind=text requires value (text to appear)".into(),
                ));
            }
            body["text"] = value.into();
        }
        "url" => {
            if value.is_empty() {
                return Err(Error::BadRequest(
                    "wait kind=url requires value (a URL glob)".into(),
                ));
            }
            body["url"] = value.into();
        }
        "load" => {
            // pinchtab validates the load state itself (400 on unknown values),
            // so just default and pass through.
            let load = if value.is_empty() { "ready-state" } else { value };
            body["load"] = load.into();
        }
        "function" => {
            if value.is_empty() {
                return Err(Error::BadRequest(
                    "wait kind=function requires value (a JS expression)".into(),
                ));
            }
            body["fn"] = value.into();
        }
        "time" => {
            // The deadline IS the delay for a fixed wait.
            body["ms"] = timeout.into();
        }
        other => {
            return Err(Error::BadRequest(format!(
                "unknown wait kind: {other} (expected selector, text, url, load, time, or function)"
            )));
        }
    }
    Ok(body)
}

/// Pinchtab's `POST /wait` response body (waitResponse in wait.go).
#[derive(Debug, serde::Deserialize)]
pub struct PinchtabWaitResponse {
    pub waited: bool,
    pub elapsed: u64,
    #[serde(default)]
    pub error: Option<String>,
}

/// Map pinchtab's wait response into the ix result shape.
pub fn to_wait_result(kind: &str, resp: PinchtabWaitResponse) -> BrowserWaitResult {
    BrowserWaitResult {
        satisfied: resp.waited,
        kind: kind.to_string(),
        elapsed_ms: resp.elapsed,
        detail: resp.error,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn opts(kind: &str, value: Option<&str>) -> BrowserWaitOpts {
        BrowserWaitOpts {
            kind: kind.to_string(),
            value: value.map(|s| s.to_string()),
            timeout_ms: None,
            state: None,
        }
    }

    // ── effective_timeout_ms ─────────────────────────────────────────────────

    #[test]
    fn timeout_defaults_to_10s() {
        assert_eq!(effective_timeout_ms(&opts("load", None)), 10_000);
    }

    #[test]
    fn timeout_passes_through_in_range() {
        let mut o = opts("load", None);
        o.timeout_ms = Some(5_000);
        assert_eq!(effective_timeout_ms(&o), 5_000);
    }

    #[test]
    fn timeout_clamps_to_30s() {
        let mut o = opts("load", None);
        o.timeout_ms = Some(120_000);
        assert_eq!(effective_timeout_ms(&o), 30_000);
    }

    // ── build_wait_body ──────────────────────────────────────────────────────

    #[test]
    fn selector_defaults_state_visible() {
        let body = build_wait_body(&opts("selector", Some("#login"))).unwrap();
        assert_eq!(body["selector"], "#login");
        assert_eq!(body["state"], "visible");
        assert_eq!(body["timeout"], 10_000);
    }

    #[test]
    fn selector_state_hidden_passes_through() {
        let mut o = opts("selector", Some("#spinner"));
        o.state = Some("hidden".to_string());
        let body = build_wait_body(&o).unwrap();
        assert_eq!(body["state"], "hidden");
    }

    #[test]
    fn selector_invalid_state_is_bad_request() {
        let mut o = opts("selector", Some("#x"));
        o.state = Some("attached".to_string());
        assert!(matches!(build_wait_body(&o), Err(Error::BadRequest(_))));
    }

    #[test]
    fn selector_missing_value_is_bad_request() {
        assert!(matches!(
            build_wait_body(&opts("selector", None)),
            Err(Error::BadRequest(_))
        ));
    }

    #[test]
    fn text_sets_text_field() {
        let body = build_wait_body(&opts("text", Some("Welcome back"))).unwrap();
        assert_eq!(body["text"], "Welcome back");
        assert!(body.get("selector").is_none());
    }

    #[test]
    fn text_missing_value_is_bad_request() {
        assert!(matches!(
            build_wait_body(&opts("text", None)),
            Err(Error::BadRequest(_))
        ));
    }

    #[test]
    fn url_sets_url_field() {
        let body = build_wait_body(&opts("url", Some("*/dashboard*"))).unwrap();
        assert_eq!(body["url"], "*/dashboard*");
    }

    #[test]
    fn url_missing_value_is_bad_request() {
        assert!(matches!(
            build_wait_body(&opts("url", None)),
            Err(Error::BadRequest(_))
        ));
    }

    #[test]
    fn load_defaults_to_ready_state() {
        let body = build_wait_body(&opts("load", None)).unwrap();
        assert_eq!(body["load"], "ready-state");
    }

    #[test]
    fn load_value_passes_through() {
        let body = build_wait_body(&opts("load", Some("network-idle"))).unwrap();
        assert_eq!(body["load"], "network-idle");
    }

    #[test]
    fn function_sets_fn_field() {
        let body =
            build_wait_body(&opts("function", Some("document.title === 'Done'"))).unwrap();
        assert_eq!(body["fn"], "document.title === 'Done'");
    }

    #[test]
    fn function_missing_value_is_bad_request() {
        assert!(matches!(
            build_wait_body(&opts("function", None)),
            Err(Error::BadRequest(_))
        ));
    }

    #[test]
    fn time_uses_timeout_as_ms() {
        let mut o = opts("time", None);
        o.timeout_ms = Some(2_000);
        let body = build_wait_body(&o).unwrap();
        assert_eq!(body["ms"], 2_000);
    }

    #[test]
    fn unknown_kind_is_bad_request() {
        assert!(matches!(
            build_wait_body(&opts("bogus", None)),
            Err(Error::BadRequest(_))
        ));
    }

    // ── to_wait_result ───────────────────────────────────────────────────────

    #[test]
    fn waited_maps_to_satisfied() {
        let r = to_wait_result(
            "selector",
            PinchtabWaitResponse { waited: true, elapsed: 840, error: None },
        );
        assert!(r.satisfied);
        assert_eq!(r.kind, "selector");
        assert_eq!(r.elapsed_ms, 840);
        assert_eq!(r.detail, None);
    }

    #[test]
    fn timeout_maps_to_unsatisfied_with_detail() {
        let r = to_wait_result(
            "text",
            PinchtabWaitResponse {
                waited: false,
                elapsed: 10_000,
                error: Some("timeout after 10000ms waiting for text".to_string()),
            },
        );
        assert!(!r.satisfied);
        assert_eq!(
            r.detail.as_deref(),
            Some("timeout after 10000ms waiting for text")
        );
    }
}
