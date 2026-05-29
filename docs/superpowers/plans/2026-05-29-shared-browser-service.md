# Shared Browser Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Phase status:** **Phase 1 is execution-ready** (complete TDD steps, grounded in code already read). **Phases 2 and 3 are a roadmap, NOT step-level executable** — each has a "Must resolve first" list of real design questions that have to be answered before it can be turned into no-placeholder steps. Do not dispatch Phase 2/3 tasks to an implementer until those are closed.

**Goal:** Lift the browser capability out of every per-chat sandbox VM into one shared browser-tier VM, behind a drop-in `RemoteSharedBrowserBackend` selected by config, so N chats share a small pool of Chrome processes instead of one each.

**Architecture:** The per-chat `ixd` daemon keeps the existing `BrowserBackend` trait but, when `IX_BROWSER_MODE=remote=<url>`, swaps `PinchtabBackend` for a new `RemoteSharedBrowserBackend` that proxies every browser call to a host-side **Browser Gateway** over HTTP (passt outbound). The gateway routes by chat_id to a pinchtab instance in a shared browser-tier Firecracker VM (booted by the existing Go SDK VMM), enforces per-chat egress, and heartbeats the VM. State (pinchtab profile dirs) lives on a host-side disk so cookies survive VM restart.

**Tech Stack:** Rust (daemon: `ix-browser`, `ix-core`, `ix-server`; `axum` + `reqwest` + `async-trait` + `thiserror`), Go (`go-sdk`: gateway + Firecracker VMM + vsock), Docker multi-stage (`daemon/cmd/Dockerfile`), Firecracker + passt.

**Decisions locked for this plan** (from the 2026-05-28 design spec + review):
- Gateway language: **Go** (reuses `go-sdk` VMM, `vsockTransport`, pool, and health code; egress matcher is a ~35-line port from Rust).
- Browser-tier VM lifecycle: **owned by the existing Go `IXManager`/VMM**, not the gateway. The gateway only routes/egresses/heartbeats.
- Placement key: **pinchtab profile name** keyed on chat_id (NOT pinchtab `agentId`; see spec corrections).

---

## File Structure

| File | Responsibility | Phase |
|---|---|---|
| `daemon/crates/ix-core/src/config.rs` | Add `BrowserMode` enum + `chat_id` to `DaemonConfig`, parse `IX_BROWSER_MODE`/`IX_CHAT_ID` | 1 |
| `daemon/crates/ix-core/src/error.rs` | Add `Error::Forbidden` → HTTP 403 (egress-denied from gateway) | 1 |
| `daemon/crates/ix-browser/src/remote.rs` | **New.** `RemoteSharedBrowserBackend` — HTTP client to the gateway, carries chat_id + egress headers | 1 |
| `daemon/crates/ix-browser/src/lib.rs` | Export `RemoteSharedBrowserBackend` | 1 |
| `daemon/crates/ix-browser/Cargo.toml` | Add `wiremock` dev-dependency for the mock-gateway tests | 1 |
| `daemon/crates/ix-server/src/main.rs` | Select backend from `config.browser_mode`; fix shutdown for both backends | 1 |
| `go-sdk/gateway.go` | **New.** Browser Gateway: listener, chat_id→pinchtab-instance routing, egress, heartbeat | 2 (roadmap) |
| `go-sdk/gateway_egress.go` | **New.** Go port of `ix-egress` `domain_matches`/`is_allowed` | 2 (roadmap) |
| `go-sdk/manager.go` | Set `IX_BROWSER_MODE`/`IX_CHAT_ID` per chat in `buildEnvSlice`; boot the browser-tier VM | 3 (roadmap) |
| `daemon/cmd/Dockerfile` | **New stage** `browser-vm` from `browser`: pinchtab + a guest vsock→pinchtab bridge as init, no `ixd` | 3 (roadmap) |

---

# Phase 1 — Daemon-side `RemoteSharedBrowserBackend` (EXECUTION-READY)

Produces working, fully unit-tested software on its own: a daemon that, when configured `remote=<url>`, proxies all browser calls to a gateway URL with the right headers and error mapping. Verified against a mock gateway HTTP server; no VM or host changes required.

### Task 1.1: Add `BrowserMode` + `chat_id` to `DaemonConfig`

**Files:**
- Modify: `daemon/crates/ix-core/src/config.rs`
- Test: `daemon/crates/ix-core/src/config.rs` (inline `#[cfg(test)] mod tests`)

- [ ] **Step 1: Write the failing tests**

Add these tests inside the existing `mod tests` block in `config.rs`, and add the three new keys to the `ALL_KEYS` array so `with_env` clears them:

```rust
// In ALL_KEYS, append:
//     "IX_BROWSER_MODE",
//     "IX_CHAT_ID",

#[test]
fn browser_mode_defaults_to_local() {
    let cfg = with_env(&[], DaemonConfig::from_env);
    assert_eq!(cfg.browser_mode, BrowserMode::Local);
}

#[test]
fn browser_mode_local_explicit() {
    let cfg = with_env(&[("IX_BROWSER_MODE", "local")], DaemonConfig::from_env);
    assert_eq!(cfg.browser_mode, BrowserMode::Local);
}

#[test]
fn browser_mode_remote_parses_url() {
    let cfg = with_env(
        &[("IX_BROWSER_MODE", "remote=http://169.254.0.1:9100")],
        DaemonConfig::from_env,
    );
    assert_eq!(
        cfg.browser_mode,
        BrowserMode::Remote {
            gateway_url: "http://169.254.0.1:9100".to_string()
        }
    );
}

#[test]
fn chat_id_parsed_from_env() {
    let cfg = with_env(&[("IX_CHAT_ID", "chat-abc123")], DaemonConfig::from_env);
    assert_eq!(cfg.chat_id.as_deref(), Some("chat-abc123"));
}

#[test]
fn chat_id_none_when_unset() {
    let cfg = with_env(&[], DaemonConfig::from_env);
    assert!(cfg.chat_id.is_none());
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd daemon && cargo test -p ix-core browser_mode chat_id 2>&1 | head -30`
Expected: FAIL — `cannot find type BrowserMode` / `no field browser_mode on DaemonConfig`.

- [ ] **Step 3: Implement the config changes**

In `config.rs`, add the enum above `DaemonConfig`, add two fields to the struct, and parse them in `from_env`:

```rust
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BrowserMode {
    /// In-VM pinchtab (today's behaviour). Default.
    Local,
    /// Proxy browser calls to a shared Browser Gateway at this URL.
    Remote { gateway_url: String },
}
```

Add to `DaemonConfig`:

```rust
    pub browser_mode: BrowserMode,
    pub chat_id: Option<String>,
```

In `from_env`, before the `Self { ... }` literal:

```rust
        let browser_mode = match std::env::var("IX_BROWSER_MODE") {
            Ok(v) if v.starts_with("remote=") => BrowserMode::Remote {
                gateway_url: v["remote=".len()..].to_string(),
            },
            _ => BrowserMode::Local,
        };

        let chat_id = std::env::var("IX_CHAT_ID").ok().filter(|s| !s.is_empty());
```

And add `browser_mode,` and `chat_id,` to the returned `Self { ... }`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd daemon && cargo test -p ix-core 2>&1 | tail -20`
Expected: PASS — all config tests green, including the 5 new ones.

- [ ] **Step 5: Stage**

```bash
git add daemon/crates/ix-core/src/config.rs
```
(Do not commit — leave the working tree dirty per project convention.)

---

### Task 1.2: Add `Error::Forbidden` → HTTP 403

The gateway returns `403` on a per-chat egress violation. The daemon needs a variant that maps to 403 so the error surfaces faithfully (the existing enum has no 403).

**Files:**
- Modify: `daemon/crates/ix-core/src/error.rs`
- Test: `daemon/crates/ix-core/src/error.rs` (inline tests)

- [ ] **Step 1: Write the failing test**

Add to `mod tests` in `error.rs`:

```rust
#[test]
fn forbidden_maps_to_403() {
    assert_eq!(
        status_of(Error::Forbidden("egress denied: evil.com".into())),
        StatusCode::FORBIDDEN
    );
}

#[test]
fn error_message_preserved_in_forbidden() {
    let err = Error::Forbidden("egress denied: evil.com".into());
    assert!(err.to_string().contains("evil.com"));
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd daemon && cargo test -p ix-core forbidden 2>&1 | head -20`
Expected: FAIL — `no variant named Forbidden found for enum Error`.

- [ ] **Step 3: Implement the variant**

In `error.rs`, add to the `Error` enum (e.g. after `Unavailable`):

```rust
    #[error("forbidden: {0}")]
    Forbidden(String),
```

And add the match arm in `into_response`:

```rust
            Error::Forbidden(_) => StatusCode::FORBIDDEN,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd daemon && cargo test -p ix-core 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add daemon/crates/ix-core/src/error.rs
```

---

### Task 1.3: Add `wiremock` dev-dependency

**Files:**
- Modify: `daemon/crates/ix-browser/Cargo.toml`

- [ ] **Step 1: Add the dev-dependency**

In `daemon/crates/ix-browser/Cargo.toml`, add (or extend) the `[dev-dependencies]` section:

```toml
[dev-dependencies]
wiremock = "0.6"
tokio = { version = "1", features = ["macros", "rt-multi-thread"] }
```
(If `[dev-dependencies]` already exists, only add the `wiremock` line and merge `tokio` features rather than duplicating the key.)

- [ ] **Step 2: Verify it resolves**

Run: `cd daemon && cargo fetch -p ix-browser 2>&1 | tail -5`
Expected: no error; wiremock 0.6.x fetched.

- [ ] **Step 3: Stage**

```bash
git add daemon/crates/ix-browser/Cargo.toml daemon/Cargo.lock
```

---

### Task 1.4: `RemoteSharedBrowserBackend` — navigate (struct + headers + error mapping)

This task creates the file, the struct, the shared HTTP helpers, and the first method (`navigate`). It mirrors `PinchtabBackend`'s `post_json`/`get_json`/`get_bytes`/`map_*_error` shape (`daemon/crates/ix-browser/src/pinchtab.rs:161-278`) but points at the gateway and injects `X-IX-Chat-Id` / `X-IX-Egress-Policy` / `Authorization`.

**Files:**
- Create: `daemon/crates/ix-browser/src/remote.rs`
- Modify: `daemon/crates/ix-browser/src/lib.rs`
- Test: `daemon/crates/ix-browser/src/remote.rs` (inline tests)

- [ ] **Step 1: Write the failing test**

Create `remote.rs` with only the test module at first:

```rust
#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::{EgressPolicy, PolicyMode};
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn policy() -> EgressPolicy {
        EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["example.com".to_string()],
        }
    }

    #[tokio::test]
    async fn navigate_sends_headers_and_parses_result() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            .and(header("X-IX-Chat-Id", "chat-1"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "url": "https://example.com",
                "title": "Example"
            })))
            .mount(&server)
            .await;

        let backend = RemoteSharedBrowserBackend::new(
            server.uri(),
            "chat-1".to_string(),
            &policy(),
            None,
        );
        let result = backend.navigate("https://example.com").await.unwrap();
        assert_eq!(result.url, "https://example.com");
        assert_eq!(result.title, "Example");
    }

    #[tokio::test]
    async fn navigate_sends_egress_policy_header_as_json() {
        let server = MockServer::start().await;
        // The matcher asserts the serialized policy is present as a header.
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            .and(header(
                "X-IX-Egress-Policy",
                r#"{"enabled":true,"mode":"allowlist","rules":["example.com"]}"#,
            ))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "url": "https://example.com", "title": "Example"
            })))
            .mount(&server)
            .await;

        let backend = RemoteSharedBrowserBackend::new(
            server.uri(), "chat-1".to_string(), &policy(), None,
        );
        assert!(backend.navigate("https://example.com").await.is_ok());
    }

    #[tokio::test]
    async fn available_is_true_for_remote() {
        let backend = RemoteSharedBrowserBackend::new(
            "http://127.0.0.1:1".to_string(), "c".to_string(), &policy(), None,
        );
        assert!(backend.available());
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd daemon && cargo test -p ix-browser navigate_sends 2>&1 | head -20`
Expected: FAIL — `cannot find type RemoteSharedBrowserBackend`.

- [ ] **Step 3: Implement the struct, helpers, error mapping, and `navigate`**

Prepend to `remote.rs` (above the test module):

```rust
use std::time::Duration;

use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    EgressPolicy, NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::{Error, Result};

use crate::backend::BrowserBackend;
use crate::pinchtab::{build_snapshot_path, build_text_path};

/// Browser backend that proxies every call to a shared Browser Gateway over
/// HTTP. Selected when `IX_BROWSER_MODE=remote=<url>`.
pub struct RemoteSharedBrowserBackend {
    client: reqwest::Client,
    gateway_url: String,
    chat_id: String,
    /// Pre-serialised egress policy, sent on every request so the gateway can
    /// enforce it before forwarding to pinchtab.
    egress_policy_header: String,
    auth_token: Option<String>,
}

impl RemoteSharedBrowserBackend {
    pub fn new(
        gateway_url: String,
        chat_id: String,
        egress: &EgressPolicy,
        auth_token: Option<String>,
    ) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .expect("failed to build reqwest client");
        let egress_policy_header =
            serde_json::to_string(egress).unwrap_or_else(|_| "{}".to_string());
        Self {
            client,
            gateway_url: gateway_url.trim_end_matches('/').to_string(),
            chat_id,
            egress_policy_header,
            auth_token,
        }
    }

    fn apply_headers(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        let mut rb = rb
            .header("X-IX-Chat-Id", &self.chat_id)
            .header("X-IX-Egress-Policy", &self.egress_policy_header);
        if let Some(ref tok) = self.auth_token {
            rb = rb.header("Authorization", format!("Bearer {tok}"));
        }
        rb
    }

    async fn post_json<B, T>(&self, path: &str, body: &B) -> Result<T>
    where
        B: serde::Serialize,
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.post(&url).json(body))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse gateway response: {e}")))
    }

    async fn get_json<T>(&self, path: &str) -> Result<T>
    where
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.get(&url))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse gateway response: {e}")))
    }

    async fn get_bytes(&self, path: &str) -> Result<Vec<u8>> {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.get(&url))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .bytes()
            .await
            .map(|b| b.to_vec())
            .map_err(|e| Error::Internal(format!("failed to read gateway bytes: {e}")))
    }
}

/// Map the gateway's HTTP status to the same `ix_core::Error` variants the
/// route layer already handles, so error mapping is identical to in-VM pinchtab.
async fn map_gateway_error(resp: reqwest::Response) -> Result<reqwest::Response> {
    let status = resp.status();
    if status.is_success() {
        return Ok(resp);
    }
    let body = resp
        .text()
        .await
        .unwrap_or_else(|_| "unknown error".to_string());
    Err(match status.as_u16() {
        400 => Error::BadRequest(format!("gateway: {body}")),
        403 => Error::Forbidden(format!("gateway: {body}")),
        404 => Error::NotFound(format!("gateway: {body}")),
        503 => Error::Unavailable(format!("gateway: {body}")),
        _ => Error::Internal(format!("gateway HTTP {status}: {body}")),
    })
}

#[async_trait]
impl BrowserBackend for RemoteSharedBrowserBackend {
    async fn navigate(&self, url: &str) -> Result<NavigateResult> {
        self.post_json("/v1/browser/navigate", &serde_json::json!({ "url": url }))
            .await
    }

    async fn screenshot(&self) -> Result<Vec<u8>> {
        self.get_bytes("/v1/browser/screenshot?raw=true").await
    }

    async fn action(&self, action: BrowserAction) -> Result<BrowserResult> {
        self.post_json("/v1/browser/action", &action).await
    }

    async fn snapshot(&self, opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        let path = format!("/v1/browser{}", build_snapshot_path(&opts));
        self.get_json(&path).await
    }

    async fn text(&self, opts: TextOpts) -> Result<BrowserTextResult> {
        let path = format!("/v1/browser{}", build_text_path(&opts));
        self.get_json(&path).await
    }

    async fn pdf(&self) -> Result<Vec<u8>> {
        self.get_bytes("/v1/browser/pdf").await
    }

    async fn eval(&self, expr: &str) -> Result<String> {
        #[derive(serde::Deserialize)]
        struct EvalResponse {
            result: String,
        }
        let resp: EvalResponse = self
            .post_json("/v1/browser/evaluate", &serde_json::json!({ "expression": expr }))
            .await?;
        Ok(resp.result)
    }

    async fn find(&self, query: &str) -> Result<BrowserFindResult> {
        self.post_json("/v1/browser/find", &serde_json::json!({ "query": query }))
            .await
    }

    fn available(&self) -> bool {
        true
    }
}
```

Then make the two path builders reusable: in `daemon/crates/ix-browser/src/pinchtab.rs`, change `pub(crate) fn build_snapshot_path` and `pub(crate) fn build_text_path` to `pub fn` (both at lines ~353 and ~376).

- [ ] **Step 4: Export from lib.rs**

In `daemon/crates/ix-browser/src/lib.rs`, add:

```rust
pub mod remote;
pub use remote::RemoteSharedBrowserBackend;
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd daemon && cargo test -p ix-browser 2>&1 | tail -25`
Expected: PASS — `navigate_sends_headers_and_parses_result`, `navigate_sends_egress_policy_header_as_json`, `available_is_true_for_remote`, plus all existing ix-browser tests.

- [ ] **Step 6: Stage**

```bash
git add daemon/crates/ix-browser/src/remote.rs daemon/crates/ix-browser/src/lib.rs daemon/crates/ix-browser/src/pinchtab.rs
```

---

### Task 1.5: Cover the remaining methods + error mapping

**Files:**
- Test: `daemon/crates/ix-browser/src/remote.rs` (extend `mod tests`)

- [ ] **Step 1: Write failing tests for bytes, json, and error mapping**

Add to `mod tests`:

```rust
#[tokio::test]
async fn screenshot_returns_bytes() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/browser/screenshot"))
        .respond_with(ResponseTemplate::new(200).set_body_bytes(vec![0x89, 0x50, 0x4E, 0x47]))
        .mount(&server)
        .await;
    let backend =
        RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
    let bytes = backend.screenshot().await.unwrap();
    assert_eq!(bytes, vec![0x89, 0x50, 0x4E, 0x47]);
}

#[tokio::test]
async fn snapshot_builds_query_and_parses() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/browser/snapshot"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
            "url": "https://example.com", "title": "Example", "nodes": []
        })))
        .mount(&server)
        .await;
    let backend =
        RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
    let snap = backend.snapshot(SnapshotOpts::default()).await.unwrap();
    assert_eq!(snap.url, "https://example.com");
}

#[tokio::test]
async fn gateway_403_maps_to_forbidden() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/browser/navigate"))
        .respond_with(ResponseTemplate::new(403).set_body_string("egress denied: evil.com"))
        .mount(&server)
        .await;
    let backend =
        RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
    let err = backend.navigate("https://evil.com").await.unwrap_err();
    assert!(matches!(err, ix_core::Error::Forbidden(_)));
}

#[tokio::test]
async fn gateway_503_maps_to_unavailable() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/browser/navigate"))
        .respond_with(ResponseTemplate::new(503).set_body_string("browser-vm down"))
        .mount(&server)
        .await;
    let backend =
        RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
    let err = backend.navigate("https://example.com").await.unwrap_err();
    assert!(matches!(err, ix_core::Error::Unavailable(_)));
}
```

- [ ] **Step 2: Run tests**

Run: `cd daemon && cargo test -p ix-browser 2>&1 | tail -25`
Expected: PASS — the implementation from Task 1.4 already satisfies these (no new impl code needed; this task locks the behaviour in tests). If any fail, fix `remote.rs`, not the test.

- [ ] **Step 3: Stage**

```bash
git add daemon/crates/ix-browser/src/remote.rs
```

---

### Task 1.6: Wire backend selection into `ix-server`

**Files:**
- Modify: `daemon/crates/ix-server/src/main.rs:71-98` and `:184`

- [ ] **Step 1: Replace the hard-coded backend construction**

In `main.rs`, replace the block at lines 72-73:

```rust
    let browser = Arc::new(ix_browser::PinchtabBackend::new().await);
    let browser_trait: Arc<dyn ix_browser::BrowserBackend> = browser.clone();
```

with branch-on-config (note: `config.egress`, `config.browser_mode`, `config.chat_id` are read before `config` is moved into `AppState`):

```rust
    // Hold an optional PinchtabBackend so we can call its async shutdown later;
    // the remote backend needs no shutdown.
    let pinchtab: Option<Arc<ix_browser::PinchtabBackend>>;
    let browser_trait: Arc<dyn ix_browser::BrowserBackend> = match &config.browser_mode {
        ix_core::config::BrowserMode::Remote { gateway_url } => {
            info!(%gateway_url, "using remote shared browser backend");
            let chat_id = config.chat_id.clone().unwrap_or_default();
            let token = std::env::var("IX_BROWSER_GATEWAY_TOKEN").ok();
            pinchtab = None;
            Arc::new(ix_browser::RemoteSharedBrowserBackend::new(
                gateway_url.clone(),
                chat_id,
                &config.egress,
                token,
            ))
        }
        ix_core::config::BrowserMode::Local => {
            let backend = Arc::new(ix_browser::PinchtabBackend::new().await);
            let trait_obj: Arc<dyn ix_browser::BrowserBackend> = backend.clone();
            pinchtab = Some(backend);
            trait_obj
        }
    };
```

- [ ] **Step 2: Fix the shutdown call**

At line 184, replace `browser.shutdown().await;` with:

```rust
    if let Some(pinchtab) = pinchtab {
        pinchtab.shutdown().await;
    }
```

- [ ] **Step 3: Build the whole daemon**

Run: `cd daemon && cargo build -p ix-server 2>&1 | tail -20`
Expected: compiles clean (no unused-variable or move errors).

- [ ] **Step 4: Run the full daemon test suite**

Run: `cd daemon && cargo test --all 2>&1 | tail -25`
Expected: PASS — all crates green.

- [ ] **Step 5: Smoke-test selection manually (optional but recommended)**

Run:
```bash
cd daemon && IX_BROWSER_MODE=remote=http://127.0.0.1:9100 IX_CHAT_ID=smoke \
  IX_ADDR=127.0.0.1:8081 cargo run -p ix-server 2>&1 | head -5
```
Expected: log line `using remote shared browser backend gateway_url=http://127.0.0.1:9100` and `ixd listening`. Ctrl-C to stop.

- [ ] **Step 6: Stage**

```bash
git add daemon/crates/ix-server/src/main.rs
```

### Phase 1 Done-When
- `cargo test --all` green in `daemon/`.
- Setting `IX_BROWSER_MODE=remote=<url>` makes the daemon proxy all 8 browser methods to `<url>/v1/browser/*` with `X-IX-Chat-Id` + `X-IX-Egress-Policy` headers, and maps gateway 403/503 to `Forbidden`/`Unavailable`.
- Default (`local`/unset) behaviour is byte-for-byte unchanged.

---

# Phase 2 — Browser Gateway (Go) — ROADMAP, NOT YET EXECUTABLE

**Objective:** A host-side Go component, reachable from per-chat VMs at `169.254.0.1:9100` (passt), that receives `/v1/browser/*` calls with `X-IX-Chat-Id`, enforces egress, routes the chat to a pinchtab instance in the browser-tier VM, and heartbeats that VM.

**New/modified files:**
- Create `go-sdk/gateway.go` — HTTP listener, router, heartbeat loop, per-chat→instance map.
- Create `go-sdk/gateway_egress.go` — Go port of `daemon/crates/ix-egress/src/policy.rs:26-60` (`is_allowed`, `domain_matches`). ~35 lines + table tests.

**Reuse points (already in the repo):**
- `vsockTransport(vsockUDS)` — `go-sdk/vmm_vsock.go:73` — to reach the browser-tier VM's guest port over Firecracker vsock.
- Heartbeat/health-loop pattern — `go-sdk/health.go:15` (`monitor`) — copy the 10s-tick structure for the `Healthy→Degraded→Unhealthy` state machine.
- `EgressPolicy` Go type already referenced via `ManagerConfig.DefaultEgress` (`go-sdk/manager.go:38`) — reuse its `{Enabled, Mode, Rules}` shape for the parsed `X-IX-Egress-Policy` header.

**Tasks (to be expanded into TDD steps once "Must resolve first" is closed):**
1. Egress matcher port + table tests (mirror the Rust `policy.rs` test cases exactly).
2. Header parsing: `X-IX-Chat-Id` (required → 400 if missing), `X-IX-Egress-Policy` (JSON → `EgressPolicy`).
3. Egress enforcement on navigate-like calls: deny → `403` before forwarding.
4. Chat→instance lifecycle manager: on first call for a chat, `POST /instances/start` + `/instances/{id}/tabs/open`, cache `chat_id → {instanceId, tabId}`; reuse on subsequent calls. (Unit-test against a mock pinchtab `httptest.Server`.)
5. Translate `/v1/browser/<op>` → tab-scoped pinchtab route (`/tabs/{tabId}/<op>`); forward over `vsockTransport`; stream byte responses (screenshot/pdf) through unchanged.
6. Heartbeat state machine; return `503` for calls bound to an `Unhealthy` VM.
7. `DELETE /chats/{chat_id}` for eager teardown of a chat's pinchtab instance.

**✅ RESOLVED (2026-05-29) — pinchtab integration design.** Investigation of the pinchtab source settled the linchpin:
- Run the browser-tier VM's pinchtab in **`pinchtab server`** mode (`cmd/pinchtab/cmd_server.go` → `internal/server/server.go`): one HTTP server (default `:9867`) that manages N isolated Chrome instances, each with its own profile dir and its own child bridge port (range `9868–9968`).
- **Topology constraint:** over Firecracker vsock only ONE guest port is reachable per CONNECT, so the gateway routes everything through the pinchtab **server** port (not the per-instance ports). The browser-VM's guest vsock bridge maps AF_VSOCK:1024 → TCP `127.0.0.1:9867`.
- **Per-chat lifecycle the gateway drives (all on `:9867`):**
  - create: `POST /instances/start` `{"mode":"headless","profileId":"chat-<id>"}` → `201 {"id","port","profileName",...}`. Using a stable `profileId` per chat reuses the on-disk profile dir → cookies survive restart.
  - open a tab: `POST /instances/{id}/tabs/open` → `{tabId}` (confirmed in `docs/reference/tabs.md`).
  - keep a map `chat_id → {instanceId, tabId}`.
  - per-op routing (tab-scoped, server-resolved): `POST /tabs/{tabId}/navigate`, `GET /tabs/{tabId}/snapshot`, `GET /tabs/{tabId}/text`, `POST /tabs/{tabId}/action`, `POST /tabs/{tabId}/find`, and `/tabs/{tabId}/screenshot|pdf|evaluate` (verify exact names against `docs/reference/tabs.md` while implementing).
  - destroy: `POST /instances/{instanceId}/stop` (profile dir persists on disk).
- Auth: send `Authorization: Bearer <PINCHTAB_TOKEN>` when the server is configured with a token.

**⚠ Still open (smaller):**
- **Per-chat Chrome reaper.** Pinchtab has no built-in per-profile idle reaper (only session idle/lifetime + per-tab eviction). The gateway must implement the LRU instance cap (`memory_mb/250` heuristic) and stop idle chats' instances via the orchestrator. Decide where this lives.
- **Auth (spec open Q3).** Day-1 default: rely on passt reachability, support an optional `IX_BROWSER_GATEWAY_TOKEN` (already plumbed in Phase 1 Task 1.6). Confirm whether to require it.
- **Gateway packaging:** separate Go binary vs a component started inside the existing `go-sdk` host process. Affects file layout and how it gets the browser-VM's vsock path.

---

# Phase 3 — Browser-tier VM image + boot wiring — ROADMAP, NOT YET EXECUTABLE

**Objective:** A Firecracker VM image running pinchtab + Chrome (no `ixd`), booted and supervised by the existing Go VMM, with per-chat daemons configured to use the gateway.

**New/modified files:**
- New stage `browser-vm` in `daemon/cmd/Dockerfile` from the existing `browser` stage (`daemon/cmd/Dockerfile:36`) — keeps Chrome + pinchtab (`:60,:65`), drops the `ixd`/`CMD ["ixd"]` default, sets init to bring up pinchtab + a guest vsock bridge.
- Modify `go-sdk/manager.go:426` (`buildEnvSlice`) — when remote browsing is on, append `IX_BROWSER_MODE=remote=<gateway>` and `IX_CHAT_ID=<sandboxID>` to each per-chat VM's env.
- Modify `go-sdk/manager.go` (`ManagerConfig` + boot path) — boot the browser-tier VM via `firecrackerBackend.startVMCold` with the browser-vm rootfs and a larger `memMB`.

**Reuse points:**
- `firecrackerBackend.startVMCold` — `go-sdk/vmm.go:117` — boots a VM; env→kernel-args via `buildKernelBootArgs` (`vmm.go:64`).
- `SnapshotManager.Restore` polls `/health` with no READY handshake (`go-sdk/snapshot.go`) — the browser-VM boot should poll pinchtab `/health` the same way, since pinchtab won't send the daemon's `READY\n` (`go-sdk/vmm_vsock.go:20`).

**Tasks (to expand later):**
1. Add `browser-vm` Docker stage; build a rootfs from it.
2. Boot path for the browser-tier VM (larger mem, browser-vm rootfs, health-poll instead of READY).
3. `buildEnvSlice` change to point per-chat VMs at the gateway.
4. Serial integration test: gateway + real browser-VM + one per-chat VM → end-to-end navigate/screenshot/snapshot (pattern: `cargo test -p ix-server -- --test-threads=1`, plus a Go integration test under `-tags=integration`).
5. Resilience test: kill browser-VM mid-test → gateway flips to 503 → recovers; cookies survive restart.

**⚠ Must resolve first (real Firecracker/pinchtab constraints — blocks step-level planning):**
- **Guest vsock→pinchtab bridge.** Firecracker host→guest delivery requires a process in the guest listening on **AF_VSOCK port 1024**; today `ixd` provides that (`daemon/crates/ix-server/src/main.rs:10-52`, the `AxumVsockListener`). Pinchtab only listens on **TCP 127.0.0.1:9867**. With `ixd` removed, nothing bridges vsock→pinchtab. Decide: add `socat VSOCK-LISTEN:1024,fork TCP:127.0.0.1:9867` to the image, OR keep a minimal `ixd` "browser-proxy" mode that forwards vsock→pinchtab, OR teach pinchtab to bind vsock. (Recommendation: socat is the smallest change.)
- **State directory is NOT a host bind-mount.** Firecracker cannot bind-mount a host directory; `/var/lib/ix/browser-state` must be either a **second ext4 block device** attached via the Firecracker `/drives` API (mirror the rootfs drive setup in `vmm.go:174-184`) or **virtiofs**. The spec's "host-mounted directory" is not directly achievable — pick the drive-image approach for day-1 and decide how it's created/sized per browser-VM.
- **Browser-VM init.** Define PID 1 for the browser-vm rootfs (e.g. a small init script that starts the vsock bridge + pinchtab in server mode, reading `IX_BROWSER_STATE_DIR`). This depends on the bridge decision above.
- **Sizing + snapshot (spec open Q4/Q5).** Decide the browser-VM memory arg and whether it gets a golden snapshot for fast cold start.

---

## Browser-tier VM — build & verify

### Build the stage

```bash
docker build --target browser-vm -f daemon/cmd/Dockerfile daemon/
```

This produces a Docker image containing Chrome, Node.js, pinchtab, socat, and
`/usr/local/bin/browser-vm-init`. The image is NOT run directly in production —
it is the source for an ext4 rootfs image via the project's rootfs build step
(e.g. converting the image filesystem to an ext4 block device for Firecracker).

### Rootfs production

Extract the image filesystem and write it into an ext4 image using the
project's existing rootfs build step. The exact tooling name is not yet
codified; the generic approach is:

```bash
# Example — adapt to whatever rootfs tooling the project uses:
CID=$(docker create <image-id>)
docker export "$CID" | ... # pipe into mke2fs / genext2fs / etc.
docker rm "$CID"
```

### Firecracker boot notes

- **Kernel init:** pass `init=/usr/local/bin/browser-vm-init` on the kernel
  command line (Firecracker `boot_args`). The script mounts `/proc`, `/sys`,
  `/dev`, writes the pinchtab config, starts pinchtab in the background, waits
  for it to become healthy, then `exec socat` as the long-lived PID-1 process.
- **Memory:** pinchtab SERVER mode runs multiple headless Chrome instances.
  Use at least **4096 MiB** (`mem_size_mib: 4096`); increase if the pool runs
  more than ~4 concurrent instances.
- **vCPUs:** 2–4 recommended (Chrome is multi-threaded).
- **Browser-state drive:** Chrome profile dirs and cookies must survive a VM
  restart. Attach a second ext4 block device via the Firecracker `/drives` API
  (mirror the rootfs drive setup in `go-sdk/vmm.go:174-184`). Mount it at
  `/var/lib/ix/browser-state` inside the VM, or set `IX_BROWSER_STATE_DIR` on
  the kernel boot args to point at a different mountpoint. The init script
  creates the directory if it does not exist, but data will not survive restart
  unless the directory is on a persistent drive.
- **vsock:** the vsock device must be configured in the Firecracker VM config
  (the existing `go-sdk` VMM already does this for regular VMs via `passt` +
  vsock). No READY handshake is emitted by the browser-VM (pinchtab does not
  send one); poll `/health` via the vsock transport instead, the same way
  `SnapshotManager.Restore` does in `go-sdk/snapshot.go`.
- **Environment variables passed via boot args:**
  - `PINCHTAB_TOKEN=<token>` — shared secret between the Gateway and pinchtab;
    passed as a kernel boot arg (env var) by `buildKernelBootArgs` in `vmm.go`.
  - `IX_BROWSER_STATE_DIR` — override the state dir if the drive is mounted
    at a non-default path.

### Smoke check (from the Firecracker host)

Connect to the browser-VM's vsock UDS using the existing vsock transport
(the same `CONNECT 1024 / OK <port>` handshake that `go-sdk/vmm_vsock.go`
implements), then send raw HTTP:

```
# Establish vsock tunnel to guest port 1024 (socat bridges it to pinchtab)
# Using socat on the host as a stand-in for the Go vsock transport:
socat - UNIX-CONNECT:<vm-vsock-uds>

# Once connected, send the CONNECT handshake then HTTP:
CONNECT 1024
# expect: OK 1024

GET /health HTTP/1.0
Host: localhost

```

Expected response: `200 OK` with JSON `{"status":"ok","mode":"dashboard",...}`.

To verify pinchtab can start a Chrome instance:

```
POST /instances/start HTTP/1.0
Host: localhost
Content-Type: application/json
Content-Length: 24

{"mode":"headless"}
```

Expected: `201 Created` with JSON containing `{"id":"...","port":...}`.

### Configuration summary (config keys used, all verified against pinchtab source)

| JSON key | Source file:struct | Value written by init |
|---|---|---|
| `server.port` | `config_types.go:ServerConfig` | `"9867"` |
| `server.bind` | `config_types.go:ServerConfig` | `"127.0.0.1"` |
| `server.token` | `config_types.go:ServerConfig` | `$PINCHTAB_TOKEN` (empty = auto-generated) |
| `profiles.baseDir` | `config_types.go:ProfilesConfig` | `$IX_BROWSER_STATE_DIR` (default `/var/lib/ix/browser-state`) |
| `instanceDefaults.mode` | `config_types.go:InstanceDefaultsConfig` | `"headless"` |
| `security.allowEvaluate` | `config_types.go:SecurityConfig` | `true` |
| `multiInstance.instancePortStart` | `config_types.go:MultiInstanceConfig` | `9868` |
| `multiInstance.instancePortEnd` | `config_types.go:MultiInstanceConfig` | `9968` |

Config file path is passed via `PINCHTAB_CONFIG` env var
(`config_load.go:131`). Token env var `PINCHTAB_TOKEN` takes precedence over
`server.token` in the file (`config_load.go:59`, `applyFileConfig` skips token
when `PINCHTAB_TOKEN` is set).

---

## Self-Review notes (planner)

- **Spec coverage:** `RemoteSharedBrowserBackend`, `IX_BROWSER_MODE`, egress-header propagation, gateway routing/egress/heartbeat, browser-VM image, state persistence, and the pool-upgrade seam all map to a phase. The pool upgrade (Option 3) is intentionally deferred — its only daemon-visible surface (the gateway URL) is already in place after Phase 1.
- **Error variants:** the spec named `BrowserUnavailable`/`EgressDenied`; the real enum has `Unavailable` and (now) `Forbidden`. Phase 1 uses the real ones — confirmed against `error.rs`.
- **Placement primitive:** Phases 2/3 use **profile/instance**, not `agentId`, consistent with the corrected spec and the pinchtab source.
- **Type consistency:** `RemoteSharedBrowserBackend::new(gateway_url, chat_id, &EgressPolicy, Option<token>)` is used identically in every test and in the Task 1.6 wiring.
