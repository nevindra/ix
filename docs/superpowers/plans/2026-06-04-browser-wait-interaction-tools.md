# Browser Wait Tool + Interaction Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **GIT POLICY (overrides any default):** Do **NOT** run `git commit` anywhere
> in this plan. Stage files with `git add` at the end of each task and leave
> the working tree dirty. The user reviews and commits batched changes
> themselves. This applies to subagents too.

**Goal:** Add an agent-facing `browser_wait` tool (six kinds: selector, text, url, load, time, function) end-to-end across oasis → ix go-sdk → ix daemon → pinchtab's native `POST /wait`, and fix two verified bugs: local-mode `allowEvaluate` missing (breaks `browser_eval` today) and the `key` vs `press` action-kind mismatch.

**Architecture:** The daemon translates ix's `BrowserWaitOpts` into pinchtab's `waitRequest` body (`ix-browser/src/wait.rs`, pure functions) and proxies one HTTP call: local backend → `POST /wait`, remote backend → `POST {gateway}/v1/browser/wait` (the gateway gains a `"wait"` whitelist entry and relays to `POST /tabs/{id}/wait`). Timeout is NOT an error — pinchtab returns 200 `{waited:false,...}` which maps to `{satisfied:false,...}`. The oasis `Sandbox` interface grows one method (compile-enforced ripple into go-sdk, athena, and all test fakes); the `browser_wait` tool auto-registers via `sandbox.Tools()`.

**Tech Stack:** Rust (axum, reqwest, wiremock, tokio) · Go 1.x (httptest) · repos: `/home/nezhifi/Development/sandbox/ix` (daemon + go-sdk), `/home/nezhifi/Development/oasis`, `/home/nezhifi/Development/athena-new`.

**Spec:** `docs/superpowers/specs/2026-06-03-browser-wait-interaction-tools-design.md` (revised 2026-06-04 — read the "Revision note" first).

**Cross-repo ordering:** Phases must run in order. Phase 2 (oasis) intentionally breaks `go build` in go-sdk and athena until Phases 3–4 add their methods — each repo's own tests pass at the end of its own phase. All repos `replace`-point at local paths, so no go.mod changes are needed.

**Reference for pinchtab wire format** (source of truth: `/home/nezhifi/Development/sandbox/pinchtab/internal/handlers/wait.go`):
- Request: `{selector?, state?, text?, url?, load?, fn?, ms?, timeout?}` — exactly one mode field; `timeout` ms (default 10000, clamp 100..30000).
- Response: always 200 on a well-formed wait: `{"waited":bool, "elapsed":int64, "match"?:string, "error"?:string}`.
- `fn` mode requires `security.allowEvaluate:true` (else 403 `evaluate_disabled`).

---

## Phase 1 — ix daemon (Rust)

All commands run from `/home/nezhifi/Development/sandbox/ix/daemon`.

### Task 1: Wait types in ix-core

**Files:**
- Modify: `crates/ix-core/src/types.rs` (insert after `NavigateResult`, ~line 228, before the `// MCP` section)

- [ ] **Step 1: Add the two types**

```rust
// Browser wait — see ix-browser/src/wait.rs for the pinchtab translation.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserWaitOpts {
    /// "selector" | "text" | "url" | "load" | "time" | "function"
    pub kind: String,
    /// selector / text / URL glob / load state / JS expression; unused for time.
    pub value: Option<String>,
    /// Max wait in ms; None uses default (10000), capped at 30000.
    pub timeout_ms: Option<u64>,
    /// selector kind only: "visible" (default) or "hidden".
    pub state: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserWaitResult {
    pub satisfied: bool,
    pub kind: String,
    pub elapsed_ms: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}
```

- [ ] **Step 2: Verify it compiles and existing tests pass**

Run: `cargo test -p ix-core`
Expected: PASS (no behavior change; serde derives compile).

- [ ] **Step 3: Stage**

```bash
git add crates/ix-core/src/types.rs
```

### Task 2: Translation module `ix-browser/src/wait.rs` (TDD)

**Files:**
- Create: `crates/ix-browser/src/wait.rs`
- Modify: `crates/ix-browser/src/lib.rs`

- [ ] **Step 1: Create the module skeleton with tests (red)**

Create `crates/ix-browser/src/wait.rs` with ONLY the tests below plus empty stubs that `todo!()` (or write the tests first and let it fail to compile — either way, verify red before green):

```rust
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
    todo!()
}

/// Translate `BrowserWaitOpts` into pinchtab's `POST /wait` JSON body.
/// Exactly one mode field is set, derived from `kind`. Validation lives here
/// so the local and remote backends enforce identical rules.
pub fn build_wait_body(opts: &BrowserWaitOpts) -> Result<serde_json::Value> {
    todo!()
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
    todo!()
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
```

- [ ] **Step 2: Register the module and run tests to verify they fail**

Add to `crates/ix-browser/src/lib.rs` (after `pub mod remote;`):

```rust
pub mod wait;
```

Run: `cargo test -p ix-browser wait`
Expected: FAIL — panics on `todo!()` (or compile error if you wrote tests-only).

- [ ] **Step 3: Implement the three functions (green)**

Replace the `todo!()` bodies:

```rust
pub fn effective_timeout_ms(opts: &BrowserWaitOpts) -> u64 {
    opts.timeout_ms
        .unwrap_or(WAIT_DEFAULT_TIMEOUT_MS)
        .min(WAIT_MAX_TIMEOUT_MS)
}

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

pub fn to_wait_result(kind: &str, resp: PinchtabWaitResponse) -> BrowserWaitResult {
    BrowserWaitResult {
        satisfied: resp.waited,
        kind: kind.to_string(),
        elapsed_ms: resp.elapsed,
        detail: resp.error,
    }
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cargo test -p ix-browser wait`
Expected: PASS — all 17 tests.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-browser/src/wait.rs crates/ix-browser/src/lib.rs
```

### Task 3: `BrowserBackend::wait` + Pinchtab/Noop/Mock implementations

**Files:**
- Modify: `crates/ix-browser/src/backend.rs`
- Modify: `crates/ix-browser/src/pinchtab.rs`
- Modify: `crates/ix-browser/src/noop.rs`

- [ ] **Step 1: Write the failing mock/noop tests**

In `crates/ix-browser/src/backend.rs` `tests` module (bottom of file), add:

```rust
    #[tokio::test]
    async fn mock_wait_returns_configured_value() {
        use ix_core::types::{BrowserWaitOpts, BrowserWaitResult};
        let mut mock = MockBrowser::new();
        mock.wait_response = Some(BrowserWaitResult {
            satisfied: true,
            kind: "selector".to_string(),
            elapsed_ms: 840,
            detail: None,
        });
        let result = mock
            .wait(BrowserWaitOpts {
                kind: "selector".to_string(),
                value: Some("#login".to_string()),
                timeout_ms: None,
                state: None,
            })
            .await
            .unwrap();
        assert!(result.satisfied);
        assert_eq!(result.elapsed_ms, 840);
    }

    #[tokio::test]
    async fn mock_wait_unconfigured_returns_error() {
        use ix_core::types::BrowserWaitOpts;
        let mock = MockBrowser::new();
        assert!(mock
            .wait(BrowserWaitOpts {
                kind: "load".to_string(),
                value: None,
                timeout_ms: None,
                state: None,
            })
            .await
            .is_err());
    }
```

In `crates/ix-browser/src/noop.rs` test `noop_is_unavailable`, add before the closing brace (and add `BrowserWaitOpts` to the noop.rs `ix_core::types` import list):

```rust
        assert!(matches!(
            b.wait(BrowserWaitOpts {
                kind: "selector".into(),
                value: Some("#x".into()),
                timeout_ms: None,
                state: None,
            })
            .await,
            Err(Error::Unavailable(_))
        ));
```

- [ ] **Step 2: Run to verify failure**

Run: `cargo test -p ix-browser`
Expected: COMPILE ERROR — no `wait` method on the trait, no `wait_response` field.

- [ ] **Step 3: Implement trait method + all three backends**

`crates/ix-browser/src/backend.rs`:
1. Extend BOTH `ix_core::types` import lists (top of file AND inside `mod mock`) with `BrowserWaitOpts, BrowserWaitResult`.
2. Add to the trait after `find`:

```rust
    /// Block until a page condition is met or the deadline elapses. A timeout
    /// is NOT an error: the result has `satisfied: false` plus a detail.
    async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult>;
```

3. Add `pub wait_response: Option<BrowserWaitResult>,` to `MockBrowser`, add `wait_response: None,` to its `Default` impl, and add to `impl BrowserBackend for MockBrowser`:

```rust
        async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
            self.wait_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: wait not configured".into()))
        }
```

`crates/ix-browser/src/noop.rs` — add to the impl block:

```rust
    async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        unavailable()
    }
```

(Extend the noop.rs import with `BrowserWaitOpts, BrowserWaitResult`.)

`crates/ix-browser/src/pinchtab.rs` — extend the `ix_core::types` import with `BrowserWaitOpts, BrowserWaitResult` and add to `impl BrowserBackend for PinchtabBackend` (after `find`, before `available`):

```rust
    async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        let body = crate::wait::build_wait_body(&opts)?;
        // The shared client has a global 30 s timeout that would fire exactly
        // at a 30 s wait deadline — give this request its own larger budget.
        let request_timeout = Duration::from_millis(
            crate::wait::effective_timeout_ms(&opts) + crate::wait::WAIT_HTTP_MARGIN_MS,
        );
        let url = format!("{}/wait", self.base_url);
        let resp = self
            .client
            .post(&url)
            .timeout(request_timeout)
            .header("Authorization", self.auth_header())
            .json(&body)
            .send()
            .await
            .map_err(|e| Error::Internal(format!("pinchtab request failed: {e}")))?;
        let resp: crate::wait::PinchtabWaitResponse = map_pinchtab_error(resp)
            .await?
            .json()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse pinchtab response: {e}")))?;
        Ok(crate::wait::to_wait_result(&opts.kind, resp))
    }
```

Also in `pinchtab.rs`, extend `map_pinchtab_error` (line ~272) with a 403 arm so
`kind=function` against an evaluate-disabled pinchtab maps to `Forbidden`
(mirrors `map_gateway_error` in remote.rs, which already does this):

```rust
    Err(match status.as_u16() {
        400 => Error::BadRequest(format!("pinchtab: {body}")),
        403 => Error::Forbidden(format!("pinchtab: {body}")),
        404 => Error::NotFound(format!("pinchtab: {body}")),
        503 => Error::Unavailable(format!("pinchtab: {body}")),
        _ => Error::Internal(format!("pinchtab HTTP {status}: {body}")),
    })
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cargo test -p ix-browser`
Expected: COMPILE ERROR remains in `remote.rs` (missing `wait`) — that is Task 5. To verify Task 3 in isolation, add the temporary stub to `crates/ix-browser/src/remote.rs` `impl BrowserBackend` (it will be replaced in Task 5):

```rust
    async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        Err(Error::Internal("not implemented yet (Task 5)".into()))
    }
```

(plus `BrowserWaitOpts, BrowserWaitResult` in remote.rs imports), then re-run:

Run: `cargo test -p ix-browser`
Expected: PASS — including the three new tests.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-browser/src/backend.rs crates/ix-browser/src/noop.rs crates/ix-browser/src/pinchtab.rs crates/ix-browser/src/remote.rs
```

### Task 4: Fix local-mode `allowEvaluate` (latent `browser_eval` 403 bug)

**Files:**
- Modify: `crates/ix-browser/src/pinchtab.rs:19` (the `PINCHTAB_CONFIG` constant) and its `tests::config_json_is_valid`

- [ ] **Step 1: Extend the config test (red)**

In `tests::config_json_is_valid` (pinchtab.rs, ~line 402), add after the `idpi` assertion:

```rust
        assert_eq!(
            v["security"]["allowEvaluate"],
            serde_json::Value::Bool(true),
            "security.allowEvaluate must be true — pinchtab gates /evaluate and \
             wait kind=function behind it (default false), which 403s browser_eval \
             in local mode. browser-vm-init.sh already sets it for the remote tier."
        );
```

- [ ] **Step 2: Run to verify failure**

Run: `cargo test -p ix-browser config_json_is_valid`
Expected: FAIL — `allowEvaluate` is null.

- [ ] **Step 3: Update the constant**

Replace line 19 of `crates/ix-browser/src/pinchtab.rs` with:

```rust
const PINCHTAB_CONFIG: &str = r#"{"server":{"port":"9867","bind":"127.0.0.1","token":"ix-internal"},"instanceDefaults":{"mode":"headless"},"security":{"idpi":{"enabled":false},"allowEvaluate":true}}"#;
```

- [ ] **Step 4: Run to verify pass**

Run: `cargo test -p ix-browser config_json_is_valid`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-browser/src/pinchtab.rs
```

### Task 5: `RemoteSharedBrowserBackend::wait` (TDD, wiremock)

**Files:**
- Modify: `crates/ix-browser/src/remote.rs`

- [ ] **Step 1: Write the failing tests**

Add to the `tests` module of `crates/ix-browser/src/remote.rs` (the `policy()` helper already exists; add `BrowserWaitOpts` to the test-module-visible imports — the top-level import was extended in Task 3):

```rust
    #[tokio::test]
    async fn wait_forwards_translated_body_and_maps_response() {
        use ix_core::types::BrowserWaitOpts;
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/wait"))
            // The gateway relays bodies verbatim to pinchtab, so the remote
            // backend must send the PINCHTAB wire format, not the ix one.
            .and(wiremock::matchers::body_json(serde_json::json!({
                "timeout": 5000,
                "selector": "#login",
                "state": "visible"
            })))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "waited": true, "elapsed": 840, "match": "#login"
            })))
            .mount(&server)
            .await;

        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let res = backend
            .wait(BrowserWaitOpts {
                kind: "selector".to_string(),
                value: Some("#login".to_string()),
                timeout_ms: Some(5_000),
                state: None,
            })
            .await
            .unwrap();
        assert!(res.satisfied);
        assert_eq!(res.kind, "selector");
        assert_eq!(res.elapsed_ms, 840);
        assert_eq!(res.detail, None);
    }

    #[tokio::test]
    async fn wait_timeout_response_is_not_an_error() {
        use ix_core::types::BrowserWaitOpts;
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/wait"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "waited": false, "elapsed": 10000,
                "error": "timeout after 10000ms waiting for selector"
            })))
            .mount(&server)
            .await;

        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let res = backend
            .wait(BrowserWaitOpts {
                kind: "selector".to_string(),
                value: Some("#never".to_string()),
                timeout_ms: None,
                state: None,
            })
            .await
            .unwrap();
        assert!(!res.satisfied);
        assert_eq!(
            res.detail.as_deref(),
            Some("timeout after 10000ms waiting for selector")
        );
    }

    #[tokio::test]
    async fn wait_unknown_kind_is_bad_request_without_calling_gateway() {
        use ix_core::types::BrowserWaitOpts;
        // Port 1 is never listening — proves validation short-circuits locally.
        let backend = RemoteSharedBrowserBackend::new(
            "http://127.0.0.1:1".to_string(),
            "c".into(),
            &policy(),
            None,
        );
        let err = backend
            .wait(BrowserWaitOpts {
                kind: "bogus".to_string(),
                value: None,
                timeout_ms: None,
                state: None,
            })
            .await
            .unwrap_err();
        assert!(matches!(err, ix_core::Error::BadRequest(_)));
    }
```

- [ ] **Step 2: Run to verify failure**

Run: `cargo test -p ix-browser remote`
Expected: FAIL — the Task 3 stub returns `Error::Internal`.

- [ ] **Step 3: Replace the stub with the real implementation**

In `impl BrowserBackend for RemoteSharedBrowserBackend`, replace the Task 3 stub:

```rust
    async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        let body = crate::wait::build_wait_body(&opts)?;
        // The shared client has a global 30 s timeout that would fire exactly
        // at a 30 s wait deadline — give this request its own larger budget.
        let request_timeout = Duration::from_millis(
            crate::wait::effective_timeout_ms(&opts) + crate::wait::WAIT_HTTP_MARGIN_MS,
        );
        let url = format!("{}/v1/browser/wait", self.gateway_url);
        let resp = self
            .apply_headers(self.client.post(&url).timeout(request_timeout).json(&body))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        let resp: crate::wait::PinchtabWaitResponse = map_gateway_error(resp)
            .await?
            .json()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse gateway response: {e}")))?;
        Ok(crate::wait::to_wait_result(&opts.kind, resp))
    }
```

- [ ] **Step 4: Run to verify pass**

Run: `cargo test -p ix-browser`
Expected: PASS — entire crate green.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-browser/src/remote.rs
```

### Task 6: Route `POST /v1/browser/wait` (TDD)

**Files:**
- Modify: `crates/ix-server/src/routes/browser.rs`
- Modify: `crates/ix-server/src/router.rs` (after line 60, the `find` route)
- Modify: `crates/ix-server/tests/integration.rs`

- [ ] **Step 1: Write the failing router tests**

In `crates/ix-server/tests/integration.rs`:

1. Extend the `ix_core::types` import (line ~12) with `BrowserWaitOpts, BrowserWaitResult`.
2. Add `wait` to the existing all-unavailable `MockBrowser` impl (after `find`, ~line 53):

```rust
    async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
```

3. Below the `MockBrowser` impl, add a happy-path mock:

```rust
/// Browser mock that records the last action kind and succeeds on action/wait.
/// Used for happy-path browser route tests (MockBrowser above is all-503).
#[derive(Default)]
struct RecordingBrowser {
    last_action: std::sync::Mutex<Option<String>>,
}

#[async_trait]
impl BrowserBackend for RecordingBrowser {
    fn available(&self) -> bool {
        true
    }
    async fn navigate(&self, _url: &str) -> Result<NavigateResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn screenshot(&self) -> Result<Vec<u8>> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn action(&self, action: BrowserAction) -> Result<BrowserResult> {
        *self.last_action.lock().unwrap() = Some(action.action_type.clone());
        Ok(BrowserResult { success: true, message: None })
    }
    async fn snapshot(&self, _opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn text(&self, _opts: TextOpts) -> Result<BrowserTextResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn pdf(&self) -> Result<Vec<u8>> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn eval(&self, _expr: &str) -> Result<String> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn find(&self, _query: &str) -> Result<BrowserFindResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        Ok(BrowserWaitResult {
            satisfied: true,
            kind: opts.kind,
            elapsed_ms: 42,
            detail: None,
        })
    }
}
```

4. Generalize the state helper — replace the body of `make_state` (line ~59) with a delegating pair:

```rust
/// Build a test AppState with the given workspace directory and browser backend.
fn make_state_with_browser(
    workspace: &str,
    browser: Arc<dyn BrowserBackend>,
) -> Arc<AppState> {
    let config = ix_core::config::DaemonConfig {
        addr: "127.0.0.1:0".to_string(),
        workspace: workspace.to_string(),
        egress: ix_core::types::EgressPolicy::default(),
        socket: None,
        vsock_port: None,
        vsock_ready_port: None,
        browser_mode: ix_core::config::BrowserMode::Local,
        chat_id: None,
    };
    Arc::new(AppState {
        config,
        browser,
        kernels: Arc::new(ix_code::KernelManager::new()),
        egress: None,
        start_time: std::time::Instant::now(),
    })
}

/// Build a test AppState with the given workspace directory.
fn make_state(workspace: &str) -> Arc<AppState> {
    make_state_with_browser(workspace, Arc::new(MockBrowser))
}
```

5. Add the route tests (note the `r##"…"##` guards — the JSON contains `"#`, which terminates a single-`#` raw string):

```rust
// ─── Browser wait route ────────────────────────────────────────────────────────

#[tokio::test]
async fn browser_wait_unavailable_returns_503() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/browser/wait")
                .header("content-type", "application/json")
                .body(Body::from(r##"{"kind":"selector","value":"#x"}"##))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
}

#[tokio::test]
async fn browser_wait_returns_structured_result() {
    let state = make_state_with_browser("/tmp", Arc::new(RecordingBrowser::default()));
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/browser/wait")
                .header("content-type", "application/json")
                .body(Body::from(
                    r##"{"kind":"selector","value":"#login","timeout_ms":5000}"##,
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert_eq!(json["satisfied"], true);
    assert_eq!(json["kind"], "selector");
    assert_eq!(json["elapsed_ms"], 42);
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cargo test -p ix-server -- --test-threads=1`
Expected: FAIL — 404 on `/v1/browser/wait` (route not registered yet); `wait` method compile error resolves once Step 1's mock additions are in.

- [ ] **Step 3: Implement handler + route**

`crates/ix-server/src/routes/browser.rs` — extend the `ix_core::types` import (line 11) with `BrowserWaitOpts` and append:

```rust
pub async fn wait(
    State(state): State<Arc<AppState>>,
    Json(req): Json<BrowserWaitOpts>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let result = state.browser.wait(req).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}
```

`crates/ix-server/src/router.rs` — after the `find` route (line 60):

```rust
        .route("/v1/browser/wait", post(routes::browser::wait))
```

- [ ] **Step 4: Run to verify pass**

Run: `cargo test -p ix-server -- --test-threads=1`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-server/src/routes/browser.rs crates/ix-server/src/router.rs crates/ix-server/tests/integration.rs
```

### Task 7: Translate action kind `key` → `press` (verified pinchtab mismatch)

Pinchtab's action registry (`/home/nezhifi/Development/sandbox/pinchtab/internal/bridge/action_registry.go`) registers `press` but NOT `key`; oasis's `browser` tool emits `key`. Without this, `{action:"key"}` fails with `unknown action: key`.

**Files:**
- Modify: `crates/ix-server/src/routes/browser.rs:45-52` (the `action` handler)
- Modify: `crates/ix-server/tests/integration.rs`

- [ ] **Step 1: Write the failing test**

Add to `crates/ix-server/tests/integration.rs`:

```rust
#[tokio::test]
async fn browser_action_kind_key_is_translated_to_press() {
    let rec = Arc::new(RecordingBrowser::default());
    let state = make_state_with_browser("/tmp", rec.clone());
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/browser/action")
                .header("content-type", "application/json")
                .body(Body::from(r#"{"kind":"key","key":"Enter"}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(rec.last_action.lock().unwrap().as_deref(), Some("press"));
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cargo test -p ix-server browser_action_kind_key -- --test-threads=1`
Expected: FAIL — recorded kind is `"key"`, want `"press"`.

- [ ] **Step 3: Implement the translation**

In `crates/ix-server/src/routes/browser.rs`, change the `action` handler to:

```rust
pub async fn action(
    State(state): State<Arc<AppState>>,
    Json(mut req): Json<BrowserAction>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    // oasis's `browser` tool emits kind="key" for key presses, but pinchtab's
    // action registry only registers "press" (action_registry.go) and fails
    // with "unknown action: key". Translate here so both the local and remote
    // backends always send a kind pinchtab accepts.
    if req.action_type == "key" {
        req.action_type = "press".to_string();
    }
    let result = state.browser.action(req).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}
```

- [ ] **Step 4: Run the full daemon suite**

Run: `cargo test --all -- --test-threads=1` (from `daemon/`; ix-server tests must stay serial)
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add crates/ix-server/src/routes/browser.rs crates/ix-server/tests/integration.rs
```

---

## Phase 2 — oasis sandbox package

All commands run from `/home/nezhifi/Development/oasis`. **This phase breaks `go build` in go-sdk and athena until Phases 3–4 — expected.**

### Task 8: Types + `Sandbox` interface + wrappers/fakes

**Files:**
- Modify: `sandbox/sandbox.go` (interface ~line 61, types after `BrowserFindResult` ~line 187)
- Modify: `sandbox/lazy.go` (after `BrowserFind`, line ~149)
- Modify: `sandbox/tools_test.go` (mockSandbox struct ~line 21, methods ~line 160)
- Modify: `sandbox/lazy_test.go` (mockSandbox methods ~line 52)

- [ ] **Step 1: Add types and interface method to `sandbox/sandbox.go`**

After the `BrowserFind` interface method (line ~61), add:

```go
	// BrowserWait blocks until a page condition is met or the timeout elapses.
	// A timeout is NOT an error: the result has Satisfied=false and a Detail
	// explaining what was being waited on.
	BrowserWait(ctx context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error)
```

After `BrowserFindResult` (line ~187), add:

```go
// BrowserWaitOpts configures a BrowserWait request.
type BrowserWaitOpts struct {
	Kind      string // "selector", "text", "url", "load", "time", "function"
	Value     string // selector / text / URL glob / load state / JS expression; unused for time
	TimeoutMs int    // max wait in ms; 0 uses default (10000), capped at 30000
	State     string // selector only: "visible" (default) or "hidden"
}

// BrowserWaitResult is the output of BrowserWait.
type BrowserWaitResult struct {
	Satisfied bool   // condition met before the deadline
	Kind      string // echoed kind
	ElapsedMs int    // milliseconds spent waiting
	Detail    string // why not satisfied (timeout message) when Satisfied=false
}
```

- [ ] **Step 2: Run build to see every implementor the compiler demands**

Run: `go build ./...`
Expected: FAIL — at minimum `sandbox/lazy.go` does not implement `Sandbox`. Note every type the compiler lists; each gets the same passthrough/stub treatment below.

- [ ] **Step 3: Add the lazy passthrough and fake methods**

`sandbox/lazy.go` (after `BrowserFind`, line ~149):

```go
func (l *lazySandbox) BrowserWait(ctx context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error) {
	sb, err := l.get(ctx)
	if err != nil {
		return BrowserWaitResult{}, err
	}
	return sb.BrowserWait(ctx, opts)
}
```

`sandbox/tools_test.go` — add the field to `mockSandbox` (after `browserPDFFn`, line ~36):

```go
	browserWaitFn  func(ctx context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error)
```

and the method (near `BrowserEval`, line ~160):

```go
func (m *mockSandbox) BrowserWait(ctx context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error) {
	if m.browserWaitFn != nil {
		return m.browserWaitFn(ctx, opts)
	}
	return BrowserWaitResult{}, nil
}
```

`sandbox/lazy_test.go` (after `BrowserFind`, line ~52):

```go
func (m *mockSandbox) BrowserWait(context.Context, sandbox.BrowserWaitOpts) (sandbox.BrowserWaitResult, error) {
	return sandbox.BrowserWaitResult{}, nil
}
```

If Step 2 listed any OTHER implementors (e.g. a recording sandbox in tests, an example), give each the same one-method stub returning the zero value, following that file's existing method style.

- [ ] **Step 4: Build + test the repo**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add sandbox/sandbox.go sandbox/lazy.go sandbox/tools_test.go sandbox/lazy_test.go
# plus any other files Step 3 touched
```

### Task 9: The `browser_wait` tool (TDD)

**Files:**
- Modify: `sandbox/tools.go` (args struct after `browserFindArgs` ~line 193; tool func after `browserFindTool` ~line 723; registration in `Tools()` ~line 247; `WithoutBrowser` doc ~line 61)
- Modify: `sandbox/tools_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `sandbox/tools_test.go`:

```go
func TestBrowserWaitToolDispatch(t *testing.T) {
	var captured BrowserWaitOpts
	sb := &mockSandbox{
		browserWaitFn: func(_ context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error) {
			captured = opts
			return BrowserWaitResult{Satisfied: true, Kind: opts.Kind, ElapsedMs: 840}, nil
		},
	}

	for _, tool := range Tools(sb) {
		if tool.Definition().Name != "browser_wait" {
			continue
		}
		args := json.RawMessage(`{"kind":"selector","value":"#login","timeout_ms":5000,"state":"visible"}`)
		result, err := tool.ExecuteRaw(context.Background(), args)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if captured.Kind != "selector" || captured.Value != "#login" ||
			captured.TimeoutMs != 5000 || captured.State != "visible" {
			t.Errorf("opts not forwarded: %+v", captured)
		}
		if got := decodeContent(t, result); got != "condition met (selector) after 840ms" {
			t.Errorf("content = %q", got)
		}
		return
	}
	t.Fatal("browser_wait tool not registered")
}

func TestBrowserWaitToolRendersTimeout(t *testing.T) {
	sb := &mockSandbox{
		browserWaitFn: func(_ context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error) {
			return BrowserWaitResult{
				Satisfied: false,
				Kind:      opts.Kind,
				ElapsedMs: 10000,
				Detail:    "timeout after 10000ms waiting for selector",
			}, nil
		},
	}

	for _, tool := range Tools(sb) {
		if tool.Definition().Name != "browser_wait" {
			continue
		}
		result, err := tool.ExecuteRaw(context.Background(), json.RawMessage(`{"kind":"selector","value":"#x"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := decodeContent(t, result)
		if !strings.Contains(got, "NOT met") || !strings.Contains(got, "snapshot") {
			t.Errorf("content = %q, want NOT met + snapshot hint", got)
		}
		return
	}
	t.Fatal("browser_wait tool not registered")
}
```

Also update `TestTools_WithoutBrowserOmitsBrowserTools` (line ~1069): add `"browser_wait": true,` to the `browserNames` map.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sandbox/ -run 'TestBrowserWait|TestTools_WithoutBrowser' -count=1`
Expected: FAIL — `browser_wait tool not registered`.

- [ ] **Step 3: Implement the tool**

`sandbox/tools.go` — args struct after `browserFindArgs` (line ~193):

```go
type browserWaitArgs struct {
	Kind      string `json:"kind" describe:"What to wait for: selector, text, url, load, time, function"`
	Value     string `json:"value,omitempty" describe:"Depends on kind — CSS selector (selector), text to appear (text), URL glob (url), load state: ready-state|content-loaded|network-idle (load), JS expression (function). Unused for time."`
	TimeoutMs int    `json:"timeout_ms,omitempty" describe:"Max wait in milliseconds (default 10000, max 30000). For kind=time this is the delay itself."`
	State     string `json:"state,omitempty" describe:"selector kind only: visible (default) or hidden"`
}
```

Tool func after `browserFindTool` (line ~723):

```go
func browserWaitTool(sb Sandbox) toolImpl {
	return newTool("browser_wait",
		"Wait for a page condition after navigate/click instead of polling with screenshots. Returns satisfied=false on timeout (never errors) — if not satisfied, take a snapshot to inspect the actual page state.",
		string(core.DeriveSchema[browserWaitArgs]()),
		func(ctx context.Context, args json.RawMessage) (oasis.ToolResult, error) {
			var p browserWaitArgs
			if err := json.Unmarshal(args, &p); err != nil {
				return oasis.ToolResult{Error: "invalid args: " + err.Error()}, nil
			}
			if p.Kind == "" {
				return oasis.ToolResult{Error: "kind is required (selector, text, url, load, time, function)"}, nil
			}
			res, err := sb.BrowserWait(ctx, BrowserWaitOpts{
				Kind:      p.Kind,
				Value:     p.Value,
				TimeoutMs: p.TimeoutMs,
				State:     p.State,
			})
			if err != nil {
				return oasis.ToolResult{Error: err.Error()}, nil
			}
			if res.Satisfied {
				return oasis.TextResult(fmt.Sprintf("condition met (%s) after %dms", res.Kind, res.ElapsedMs)), nil
			}
			msg := fmt.Sprintf("condition NOT met (%s) after %dms", res.Kind, res.ElapsedMs)
			if res.Detail != "" {
				msg += ": " + res.Detail
			}
			return oasis.TextResult(msg + ". Take a snapshot to inspect the current page state."), nil
		})
}
```

Register it in `Tools()` — in the `if !cfg.noBrowser` block (line ~247), after `browserFindTool(sb),`:

```go
			browserWaitTool(sb),
```

Update the `WithoutBrowser` doc comment (line ~61) to include the new tool:

```go
// WithoutBrowser omits the browser tool set (browser, screenshot, snapshot,
// page_text, export_pdf, browser_eval, browser_find, browser_wait) from the
// returned tools.
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add sandbox/tools.go sandbox/tools_test.go
```

---

## Phase 3 — ix go-sdk

All commands run from `/home/nezhifi/Development/sandbox/ix/go-sdk`.

### Task 10: Gateway `wait` op (TDD)

**Files:**
- Modify: `gateway.go:166` (the `browserOps` map)
- Modify: `gateway_test.go` (mockPinchtab handler ~line 186, new test after `TestGateway_ActionNavigationChangedBecomesSuccess` ~line 380)

- [ ] **Step 1: Write the failing test**

Add a tab-scoped wait handler to `mockPinchtab.handler()` in `gateway_test.go` (next to the `POST /tabs/{id}/action` handler, ~line 186):

```go
	mux.HandleFunc("POST /tabs/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, string(body))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"waited": true, "elapsed": 840, "match": "#login"})
	})
```

And the test:

```go
// --- wait forwards to the tab-scoped pinchtab route ---

func TestGateway_WaitForwardsToTabScopedRoute(t *testing.T) {
	g, mock, cleanup := newTestGateway(t)
	defer cleanup()
	h := g.Handler()

	rec := doReq(t, h, http.MethodPost, "/v1/browser/wait", "chat1", nil, `{"selector":"#login","state":"visible","timeout":5000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	seq := mock.callSeq()
	if len(seq) == 0 || seq[len(seq)-1] != "POST /tabs/"+mock.tabID+"/wait" {
		t.Fatalf("wait should reach pinchtab tab-scoped wait; seq=%v", seq)
	}
	var out struct {
		Waited  bool  `json:"waited"`
		Elapsed int64 `json:"elapsed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	if !out.Waited || out.Elapsed != 840 {
		t.Fatalf("relayed response wrong: %+v (body=%s)", out, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestGateway_WaitForwards ./... -count=1`
Expected: FAIL — gateway returns 404 (no `/v1/browser/wait` route).

- [ ] **Step 3: Add the op**

In `gateway.go`, add to the `browserOps` map (line ~166, after `"find"`):

```go
	"wait":       {method: http.MethodPost},
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -run TestGateway ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add gateway.go gateway_test.go
```

### Task 11: `IXSandbox.BrowserWait` (TDD)

**Files:**
- Modify: `sandbox.go` (after `BrowserFind`, ~line 610)
- Modify: `sandbox_test.go` (`ixMux` before `return mux` ~line 228; new test near `TestIXSandboxShell` ~line 248)

- [ ] **Step 1: Write the failing test**

Add the handler inside `ixMux` (before `return mux`, line ~228):

```go
	mux.HandleFunc("POST /v1/browser/wait", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["kind"] != "selector" || req["value"] != "#login" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"satisfied": true, "kind": "selector", "elapsed_ms": 840,
		})
	})
```

And the test:

```go
func TestIXSandboxBrowserWait(t *testing.T) {
	s, _ := newTestSandbox(t)

	res, err := s.BrowserWait(context.Background(), sandbox.BrowserWaitOpts{
		Kind:      "selector",
		Value:     "#login",
		TimeoutMs: 5000,
	})
	if err != nil {
		t.Fatalf("BrowserWait() returned error: %v", err)
	}
	if !res.Satisfied || res.Kind != "selector" || res.ElapsedMs != 840 {
		t.Errorf("unexpected result: %+v", res)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go build ./...`
Expected: FAIL — `*IXSandbox` no longer implements `sandbox.Sandbox` (missing `BrowserWait` after the Phase 2 interface change), and the new test calls an undefined method.

- [ ] **Step 3: Implement the method**

Add to `sandbox.go` after `BrowserFind` (line ~610):

```go
// BrowserWait blocks until a page condition is met or the timeout elapses.
// A timeout is not an error: the result reports Satisfied=false with Detail.
func (s *IXSandbox) BrowserWait(ctx context.Context, opts sandbox.BrowserWaitOpts) (sandbox.BrowserWaitResult, error) {
	if err := s.checkClosed(); err != nil {
		return sandbox.BrowserWaitResult{}, err
	}
	body := map[string]any{"kind": opts.Kind}
	if opts.Value != "" {
		body["value"] = opts.Value
	}
	if opts.TimeoutMs > 0 {
		body["timeout_ms"] = opts.TimeoutMs
	}
	if opts.State != "" {
		body["state"] = opts.State
	}
	var resp struct {
		Satisfied bool   `json:"satisfied"`
		Kind      string `json:"kind"`
		ElapsedMs int    `json:"elapsed_ms"`
		Detail    string `json:"detail"`
	}
	if err := s.client.post(ctx, "/v1/browser/wait", body, &resp); err != nil {
		return sandbox.BrowserWaitResult{}, fmt.Errorf("browser wait: %w", err)
	}
	return sandbox.BrowserWaitResult{
		Satisfied: resp.Satisfied,
		Kind:      resp.Kind,
		ElapsedMs: resp.ElapsedMs,
		Detail:    resp.Detail,
	}, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS (unit tests; integration-tagged tests are excluded by default).

- [ ] **Step 5: Stage**

```bash
git add sandbox.go sandbox_test.go
```

---

## Phase 4 — athena-new

All commands run from `/home/nezhifi/Development/athena-new`.

### Task 12: `lazySandbox.BrowserWait` passthrough

**Files:**
- Modify: `internal/adapter/lazy_sandbox.go` (after `BrowserFind`, line ~225)

- [ ] **Step 1: Verify the compile break exists (red)**

Run: `go build ./...`
Expected: FAIL — `*lazySandbox` does not implement `sandbox.Sandbox` (missing `BrowserWait`). If the compiler lists other implementors (test fakes), note them for Step 2.

- [ ] **Step 2: Add the passthrough**

After `BrowserFind` (line ~225), matching the no-retry style of `BrowserAction`/`BrowserEval`:

```go
func (l *lazySandbox) BrowserWait(ctx context.Context, opts sandbox.BrowserWaitOpts) (sandbox.BrowserWaitResult, error) {
	sb, err := l.get(ctx)
	if err != nil {
		return sandbox.BrowserWaitResult{}, err
	}
	return sb.BrowserWait(ctx, opts)
}
```

Add the same one-method stub to any test fake the compiler reported.

- [ ] **Step 3: Build + test**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS. The `browser_wait` tool now reaches agents automatically via `sandbox.Tools()` (`internal/adapter/oasis.go:634`) — no wiring changes.

- [ ] **Step 4: Stage**

```bash
git add internal/adapter/lazy_sandbox.go
# plus any test fakes touched
```

---

## Phase 5 — Gated end-to-end verification

From `/home/nezhifi/Development/sandbox/ix/daemon`. Requires `pinchtab` and Chrome in PATH (e.g. inside the browser Docker image) — tests are `#[ignore]` so CI without a browser stays green.

### Task 13: E2E interaction sweep + wait smoke

**Files:**
- Create: `crates/ix-browser/tests/browser_e2e.rs`

- [ ] **Step 1: Write the e2e tests**

```rust
//! End-to-end tests against a REAL pinchtab + Chrome.
//!
//! Ignored by default — they require `pinchtab` and Chrome in PATH (e.g.
//! inside the browser Docker image). pinchtab binds a fixed port (9867), so
//! these MUST run serially:
//!
//!   cargo test -p ix-browser --test browser_e2e -- --ignored --test-threads=1

use ix_browser::{BrowserBackend, PinchtabBackend};
use ix_core::types::{BrowserAction, BrowserWaitOpts};

const TEST_PAGE: &str = "data:text/html,<html><body>\
<button id='btn'>Click me</button>\
<input id='inp' type='text'/>\
<select id='sel'><option value='a'>A</option><option value='b'>B</option></select>\
<div id='hover-target'>hover</div>\
<p id='para'>hello e2e</p>\
</body></html>";

fn action(kind: &str, ref_: Option<&str>) -> BrowserAction {
    BrowserAction {
        action_type: kind.to_string(),
        element_ref: ref_.map(|s| s.to_string()),
        x: None,
        y: None,
        text: None,
        key: None,
        direction: None,
        value: None,
    }
}

/// Sweep every interaction kind the oasis `browser` tool can emit. `key` is
/// covered too: the daemon route translates it to `press` (routes/browser.rs),
/// so this exercises the post-translation set directly against pinchtab.
#[tokio::test]
#[ignore = "requires pinchtab + Chrome in PATH"]
async fn interaction_sweep_all_oasis_kinds() {
    let backend = PinchtabBackend::new().await;
    assert!(backend.available(), "pinchtab did not start — is it in PATH?");
    backend.navigate(TEST_PAGE).await.expect("navigate");

    let cases: Vec<BrowserAction> = vec![
        action("click", Some("#btn")),
        BrowserAction { text: Some("hi".into()), ..action("type", Some("#inp")) },
        BrowserAction { text: Some("hello@x.test".into()), ..action("fill", Some("#inp")) },
        BrowserAction { direction: Some("down".into()), ..action("scroll", None) },
        BrowserAction { key: Some("Enter".into()), ..action("press", None) },
        action("hover", Some("#hover-target")),
        BrowserAction { value: Some("b".into()), ..action("select", Some("#sel")) },
        action("focus", Some("#inp")),
    ];
    for case in cases {
        let kind = case.action_type.clone();
        let res = backend
            .action(case)
            .await
            .unwrap_or_else(|e| panic!("action {kind} errored: {e}"));
        assert!(res.success, "action {kind} reported failure: {:?}", res.message);
    }
    backend.shutdown().await;
}

#[tokio::test]
#[ignore = "requires pinchtab + Chrome in PATH"]
async fn wait_selector_text_load_and_timeout_smoke() {
    let backend = PinchtabBackend::new().await;
    assert!(backend.available(), "pinchtab did not start — is it in PATH?");
    backend.navigate(TEST_PAGE).await.expect("navigate");

    let wait = |kind: &str, value: Option<&str>| BrowserWaitOpts {
        kind: kind.to_string(),
        value: value.map(|s| s.to_string()),
        timeout_ms: Some(5_000),
        state: None,
    };

    let r = backend.wait(wait("selector", Some("#para"))).await.expect("wait selector");
    assert!(r.satisfied, "selector wait failed: {:?}", r.detail);

    let r = backend.wait(wait("text", Some("hello e2e"))).await.expect("wait text");
    assert!(r.satisfied, "text wait failed: {:?}", r.detail);

    let r = backend.wait(wait("load", None)).await.expect("wait load");
    assert!(r.satisfied, "load wait failed: {:?}", r.detail);

    // Timeout path: a selector that never appears → satisfied=false, NOT Err.
    let r = backend
        .wait(BrowserWaitOpts {
            kind: "selector".into(),
            value: Some("#never-exists".into()),
            timeout_ms: Some(1_000),
            state: None,
        })
        .await
        .expect("timeout wait must not error");
    assert!(!r.satisfied, "nonexistent selector must time out unsatisfied");
    assert!(r.detail.is_some(), "timeout must carry a detail message");
    assert!(r.elapsed_ms >= 1_000, "elapsed must reflect the wait");

    backend.shutdown().await;
}
```

- [ ] **Step 2: Verify it compiles (and is skipped) without a browser**

Run: `cargo test -p ix-browser --test browser_e2e`
Expected: `ok. 0 passed; 0 failed; 2 ignored`.

- [ ] **Step 3: Run for real where pinchtab + Chrome exist**

Run: `cargo test -p ix-browser --test browser_e2e -- --ignored --test-threads=1`
Expected: PASS (2 tests). If running on the host without Chrome, run inside the browser Docker image (`daemon/cmd/Dockerfile`, `browser` stage) instead. If any action kind fails here, that is a REAL finding — investigate the pinchtab handler for that kind before changing the test.

- [ ] **Step 4: Stage**

```bash
git add crates/ix-browser/tests/browser_e2e.rs
```

---

## Final verification (all repos)

- [ ] `cd /home/nezhifi/Development/sandbox/ix/daemon && cargo test --all -- --test-threads=1` → PASS
- [ ] `cd /home/nezhifi/Development/oasis && go test ./... -count=1` → PASS
- [ ] `cd /home/nezhifi/Development/sandbox/ix/go-sdk && go build ./... && go test ./... -count=1` → PASS
- [ ] `cd /home/nezhifi/Development/athena-new && go build ./... && go test ./... -count=1` → PASS
- [ ] `git status` in each repo shows only staged, expected files; **no commits made**.

## Known risks / notes for the executor

- **Pinchtab binary version**: `/wait` exists in the local pinchtab source (`internal/routes/routes.go:79`). The Docker image pulls `pinchtab/pinchtab:latest`; an older binary without `/wait` would surface as `Error::NotFound("pinchtab: …")` in the e2e run only — unit/route tests are unaffected.
- **`kind=function` upstream gating**: works locally after Task 4; via the gateway it depends on the browser-VM's pinchtab config, which `daemon/cmd/browser-vm-init.sh:96` already sets (`allowEvaluate: true`). No action needed.
- **oasis examples/other impls**: Task 8 Step 2 is compiler-driven on purpose — `go build ./...` enumerates every `Sandbox` implementor that needs the new method.
